// Package chaosaudit is an adversarial / chaos regression suite for Action
// EventOS. It boots the fully-wired server in-process (the exact module set
// cmd/eventpos builds, INCLUDING clientapi) on a fresh temp SQLite DB and then
// abuses every mutating endpoint with hostile inputs and concurrency, trying to
// provoke crashes, 5xx, negative balances/stock, double-spend, oversell, broken
// integrity, over-delivery, or wrong money math.
//
// The harness mirrors internal/apitest + internal/loadtest. Run with -race.
//
// Invariant guarantees asserted after every storm:
//   - no HTTP 5xx / transport failure anywhere
//   - every attendees.balance >= 0  and  SUM(balance_ledger.amount) == balance
//   - every products.stock_qty >= 0  and  SUM(stock_movements.delta) == stock_qty
//   - order_items.delivered_qty <= qty (never over-delivered)
//   - SUM(payments) == SUM(order totals) for non-voided orders (conservation)
//   - GET /api/integrity -> ok:true with an intact audit chain
package chaosaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/clientapi"
	"eventpos/internal/config"
	"eventpos/internal/core"
	"eventpos/internal/customers"
	"eventpos/internal/db"
	"eventpos/internal/events"
	"eventpos/internal/kitchen"
	"eventpos/internal/oversight"
	"eventpos/internal/realtime"
	"eventpos/internal/reports"
	"eventpos/internal/sales"
	"eventpos/internal/server"
	"eventpos/internal/users"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client
	d      *db.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "chaos.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	w := writer.New(d.Writer)
	t.Cleanup(w.Close)

	hub := realtime.NewHub(64)
	cfg := config.New(d)
	if err := cfg.EnsureDefaults(ctx); err != nil {
		t.Fatalf("ensure defaults: %v", err)
	}
	backupSvc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	authSvc := auth.New(d)

	application := &app.App{DB: d, Writer: w, Hub: hub, DataDir: t.TempDir()}

	if _, _, err := bootstrap.SeedAdmin(ctx, d); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	deps := server.Deps{App: application, Auth: authSvc, Config: cfg, Backup: backupSvc}
	mods := []server.Module{
		users.New(deps), events.New(deps), catalog.New(deps), customers.New(deps),
		caja.New(deps), sales.New(deps), kitchen.New(deps), reports.New(deps),
		oversight.New(deps), clientapi.New(deps),
	}

	srv := server.New(deps, fstest.MapFS{}, mods...)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	return &harness{ts: ts, client: &http.Client{Jar: jar}, d: d}
}

func (h *harness) reqWith(client *http.Client, method, path string, body any) (int, []byte) {
	var r io.Reader
	switch b := body.(type) {
	case nil:
		// no body
	case string: // raw (possibly malformed) body
		r = strings.NewReader(b)
	default:
		j, err := json.Marshal(body)
		if err != nil {
			panic(fmt.Sprintf("marshal %s %s: %v", method, path, err))
		}
		r = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		panic(fmt.Sprintf("new request %s %s: %v", method, path, err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte(err.Error()) // status 0 = transport failure
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (h *harness) req(method, path string, body any) (int, []byte) {
	return h.reqWith(h.client, method, path, body)
}

func (h *harness) login(t *testing.T, username, secret string) {
	t.Helper()
	code, data := h.req("POST", "/api/auth/login",
		map[string]any{"username": username, "secret": secret})
	if code != http.StatusOK {
		t.Fatalf("login %s: status %d body %s", username, code, data)
	}
}

func (h *harness) mustJSON(t *testing.T, method, path string, body, out any) {
	t.Helper()
	code, data := h.req(method, path, body)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("%s %s: status %d body %s", method, path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s decode: %v (body %s)", method, path, err, data)
		}
	}
}

// ---------------------------------------------------------------------------
// Wire shapes + builders
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}
type attendeeResp struct {
	ID      string `json:"id"`
	QRToken string `json:"qr_token"`
	Balance int64  `json:"balance"`
}
type productResp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StockQty int64  `json:"stock_qty"`
	Price    int64  `json:"price"`
}
type orderResp struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Total           int64  `json:"total"`
	PaidFromBalance int64  `json:"paid_from_balance"`
	PaidCash        int64  `json:"paid_cash"`
	Payments        []struct {
		Method string `json:"method"`
		Amount int64  `json:"amount"`
	} `json:"payments"`
}

func (h *harness) draftEvent(t *testing.T) string {
	t.Helper()
	var ev idResp
	h.mustJSON(t, "POST", "/api/events",
		map[string]any{"name": "Chaos", "default_topup": 0, "split_policy": "cascade"}, &ev)
	return ev.ID
}
func (h *harness) activate(t *testing.T, id string) {
	t.Helper()
	h.mustJSON(t, "POST", "/api/events/"+id+"/activate", map[string]any{}, nil)
}
func (h *harness) category(t *testing.T, name string) string {
	t.Helper()
	var c idResp
	h.mustJSON(t, "POST", "/api/categories", map[string]any{"name": name, "sort_order": 0}, &c)
	return c.ID
}
func (h *harness) product(t *testing.T, cat, name string, price, stock int64) productResp {
	t.Helper()
	var p productResp
	h.mustJSON(t, "POST", "/api/products", map[string]any{
		"category_id": cat, "name": name, "price": price, "cost": 0,
		"prep_station": "bar", "track_stock": 1, "stock_qty": stock,
	}, &p)
	return p
}
func (h *harness) attendee(t *testing.T, name string, topup int64) attendeeResp {
	t.Helper()
	var a attendeeResp
	h.mustJSON(t, "POST", "/api/attendees",
		map[string]any{"display_name": name, "doc_id": "d-" + name,
			"email": name + "@x.com", "phone": "p-" + name, "initial_topup": topup}, &a)
	return a
}

// ---------------------------------------------------------------------------
// Status recorder (flags any 5xx / transport failure)
// ---------------------------------------------------------------------------

type recorder struct {
	mu       sync.Mutex
	statuses map[int]int
	bad      []string
	total    int64
}

func newRecorder() *recorder { return &recorder{statuses: map[int]int{}} }
func (rec *recorder) record(label string, code int) {
	atomic.AddInt64(&rec.total, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.statuses[code]++
	if code >= 500 || code == 0 {
		if len(rec.bad) < 80 {
			rec.bad = append(rec.bad, fmt.Sprintf("%s -> %d", label, code))
		}
	}
}
func (rec *recorder) check(t *testing.T) {
	t.Helper()
	t.Logf("status distribution: %v (total=%d)", rec.statuses, rec.total)
	if len(rec.bad) > 0 {
		t.Errorf("SERVER ERRORS (5xx/transport): %v", rec.bad)
	}
}

// ---------------------------------------------------------------------------
// Direct-SQL invariant assertions
// ---------------------------------------------------------------------------

func (h *harness) invariants(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// negative balance
	rows, _ := h.d.Reader.QueryContext(ctx,
		`SELECT id, balance FROM attendees WHERE balance < 0`)
	for rows.Next() {
		var id string
		var b int64
		_ = rows.Scan(&id, &b)
		t.Errorf("NEGATIVE BALANCE: attendee %s = %d", id, b)
	}
	rows.Close()

	// negative stock
	rows, _ = h.d.Reader.QueryContext(ctx,
		`SELECT id, stock_qty FROM products WHERE track_stock=1 AND stock_qty < 0`)
	for rows.Next() {
		var id string
		var q int64
		_ = rows.Scan(&id, &q)
		t.Errorf("NEGATIVE STOCK: product %s = %d", id, q)
	}
	rows.Close()

	// ledger == balance
	rows, _ = h.d.Reader.QueryContext(ctx, `
		SELECT a.id, a.balance, COALESCE(SUM(l.amount),0)
		FROM attendees a LEFT JOIN balance_ledger l ON l.attendee_id=a.id
		GROUP BY a.id HAVING a.balance != COALESCE(SUM(l.amount),0)`)
	for rows.Next() {
		var id string
		var c, s int64
		_ = rows.Scan(&id, &c, &s)
		t.Errorf("LEDGER MISMATCH: attendee %s cached=%d ledger=%d", id, c, s)
	}
	rows.Close()

	// movements == stock
	rows, _ = h.d.Reader.QueryContext(ctx, `
		SELECT p.id, p.stock_qty, COALESCE(SUM(sm.delta),0)
		FROM products p LEFT JOIN stock_movements sm ON sm.product_id=p.id
		WHERE p.track_stock=1 GROUP BY p.id
		HAVING p.stock_qty != COALESCE(SUM(sm.delta),0)`)
	for rows.Next() {
		var id string
		var c, s int64
		_ = rows.Scan(&id, &c, &s)
		t.Errorf("STOCK MOVEMENT MISMATCH: product %s cached=%d movements=%d", id, c, s)
	}
	rows.Close()

	// over-delivery
	rows, _ = h.d.Reader.QueryContext(ctx,
		`SELECT id, qty, delivered_qty FROM order_items WHERE delivered_qty > qty`)
	for rows.Next() {
		var id string
		var q, dq int64
		_ = rows.Scan(&id, &q, &dq)
		t.Errorf("OVER-DELIVERED: item %s delivered=%d qty=%d", id, dq, q)
	}
	rows.Close()
}

// assertConservation: SUM(payments) == SUM(order totals) for non-voided orders,
// and balance + cash legs cover the order value exactly.
func (h *harness) assertConservation(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var orderTotal, payTotal, balPay, cashPay int64
	_ = h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total),0) FROM orders WHERE status<>'voided'`).Scan(&orderTotal)
	_ = h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(p.amount),0) FROM payments p JOIN orders o ON o.id=p.order_id
		 WHERE o.status<>'voided'`).Scan(&payTotal)
	_ = h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(p.amount),0) FROM payments p JOIN orders o ON o.id=p.order_id
		 WHERE o.status<>'voided' AND p.method='balance'`).Scan(&balPay)
	_ = h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(p.amount),0) FROM payments p JOIN orders o ON o.id=p.order_id
		 WHERE o.status<>'voided' AND p.method='cash'`).Scan(&cashPay)
	if payTotal != orderTotal {
		t.Errorf("CONSERVATION: payments=%d != order totals=%d (diff %d)",
			payTotal, orderTotal, payTotal-orderTotal)
	}
	if balPay+cashPay != orderTotal {
		t.Errorf("CONSERVATION: balance(%d)+cash(%d)=%d != order value=%d",
			balPay, cashPay, balPay+cashPay, orderTotal)
	}
	t.Logf("conservation: orderTotal=%d payments=%d (balance=%d cash=%d)",
		orderTotal, payTotal, balPay, cashPay)
}

func (h *harness) assertIntegrityAPI(t *testing.T) {
	t.Helper()
	code, data := h.req("GET", "/api/integrity", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/integrity: status %d body %s", code, data)
	}
	var res struct {
		OK    bool `json:"ok"`
		Audit struct {
			Intact   bool   `json:"intact"`
			BrokenAt string `json:"broken_at"`
		} `json:"audit"`
		BalanceMismatches []json.RawMessage `json:"balance_mismatches"`
		StockMismatches   []json.RawMessage `json:"stock_mismatches"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("decode integrity: %v body %s", err, data)
	}
	if !res.OK {
		t.Errorf("integrity ok=false bal_mm=%d stock_mm=%d audit_intact=%v broken_at=%q",
			len(res.BalanceMismatches), len(res.StockMismatches), res.Audit.Intact, res.Audit.BrokenAt)
	}
}

// ===========================================================================
// 1. Malformed / hostile inputs to mutating endpoints.
// ===========================================================================

func TestMalformedHostileInputs(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.category(t, "Bebidas")
	prod := h.product(t, cat, "Cerveza", 1000, 100)
	att := h.attendee(t, "Victim", 100000)

	rec := newRecorder()
	hit := func(label, method, path string, body any, wantMax int) {
		code, data := h.req(method, path, body)
		rec.record(label, code)
		if code >= 500 || code == 0 {
			t.Errorf("%s: got %d (body %s) — expected graceful 4xx, not 5xx", label, code, data)
		}
		_ = wantMax
	}

	big := strings.Repeat("A", 1<<20) // 1 MB name
	hostileNames := map[string]string{
		"giant-1mb":  big,
		"emoji":      "🍺🔥💀 Cerveza",
		"rtl":        "‮DROP TABLE‬",
		"nullbyte":   "Cerve\x00za",
		"sqlish":     "'; DROP TABLE products;--",
		"likewild":   "%_%",
		"whitespace": "   ",
		"quote-json": `He said "hi" \ backslash`,
	}

	// --- catalog (draft-time) hostile names ---
	for label, name := range hostileNames {
		hit("category:"+label, "POST", "/api/categories",
			map[string]any{"name": name, "sort_order": 0}, 400)
		hit("product:"+label, "POST", "/api/products",
			map[string]any{"category_id": cat, "name": name, "price": 1000,
				"prep_station": "bar", "track_stock": 0, "stock_qty": 0}, 400)
	}

	// --- malformed bodies / wrong types / unknown fields ---
	hit("cat:malformed-json", "POST", "/api/categories", "{not json", 400)
	hit("cat:empty-name", "POST", "/api/categories", map[string]any{"name": "", "sort_order": 0}, 400)
	hit("prod:neg-price", "POST", "/api/products",
		map[string]any{"category_id": cat, "name": "x", "price": -5,
			"prep_station": "bar", "track_stock": 0, "stock_qty": 0}, 400)
	hit("prod:neg-cost", "POST", "/api/products",
		map[string]any{"category_id": cat, "name": "x", "price": 5, "cost": -1,
			"prep_station": "bar", "track_stock": 0, "stock_qty": 0}, 400)
	hit("prod:wrong-type-price", "POST", "/api/products",
		map[string]any{"category_id": cat, "name": "x", "price": "lots",
			"prep_station": "bar", "track_stock": 0, "stock_qty": 0}, 400)
	hit("prod:unknown-field", "POST", "/api/products",
		map[string]any{"category_id": cat, "name": "x", "price": 5,
			"prep_station": "bar", "track_stock": 0, "stock_qty": 0, "evil": true}, 400)
	hit("prod:bad-station", "POST", "/api/products",
		map[string]any{"category_id": cat, "name": "x", "price": 5,
			"prep_station": "nuclear", "track_stock": 0, "stock_qty": 0}, 400)

	h.activate(t, ev)

	// --- order endpoint hostile inputs (event active) ---
	order := func(label string, body any) { hit(label, "POST", "/api/orders", body, 400) }
	order("ord:malformed", "{")
	order("ord:empty", map[string]any{})
	order("ord:no-items", map[string]any{"request_id": "x1",
		"payers": []map[string]any{{"attendee_id": att.ID}}})
	order("ord:no-payers", map[string]any{"request_id": "x2",
		"items": []map[string]any{{"product_id": prod.ID, "qty": 1}}})
	order("ord:qty0", map[string]any{"request_id": "x3",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": 0}},
		"payers": []map[string]any{{"attendee_id": att.ID}}})
	order("ord:qty-neg", map[string]any{"request_id": "x4",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": -10}},
		"payers": []map[string]any{{"attendee_id": att.ID}}})
	order("ord:qty-huge", map[string]any{"request_id": "x5",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": 100001}},
		"payers": []map[string]any{{"attendee_id": att.ID}}})
	order("ord:qty-maxint", map[string]any{"request_id": "x6",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": int64(math.MaxInt64)}},
		"payers": []map[string]any{{"attendee_id": att.ID}}})
	order("ord:unknown-product", map[string]any{"request_id": "x7",
		"items":  []map[string]any{{"product_id": "nope", "qty": 1}},
		"payers": []map[string]any{{"attendee_id": att.ID}}})
	order("ord:unknown-attendee", map[string]any{"request_id": "x8",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": 1}},
		"payers": []map[string]any{{"attendee_id": "nope"}}})
	order("ord:dup-payer", map[string]any{"request_id": "x9",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": 1}},
		"payers": []map[string]any{{"attendee_id": att.ID}, {"attendee_id": att.ID}}})
	order("ord:unknown-field", map[string]any{"request_id": "x10",
		"items":  []map[string]any{{"product_id": prod.ID, "qty": 1}},
		"payers": []map[string]any{{"attendee_id": att.ID}}, "evil": 1})

	// --- topup hostile inputs ---
	hit("topup:zero", "POST", "/api/attendees/"+att.ID+"/topup", map[string]any{"amount": 0}, 400)
	hit("topup:neg", "POST", "/api/attendees/"+att.ID+"/topup", map[string]any{"amount": -100}, 400)
	hit("topup:wrong-type", "POST", "/api/attendees/"+att.ID+"/topup", map[string]any{"amount": "lots"}, 400)
	hit("topup:unknown-att", "POST", "/api/attendees/nope/topup", map[string]any{"amount": 100}, 404)

	// --- search with SQL-ish / wildcard strings ---
	for label, q := range map[string]string{
		"q-drop": "'; DROP TABLE attendees;--", "q-pct": "%", "q-underscore": "_",
		"q-emoji": "🍺", "q-null": "a\x00b",
	} {
		code, _ := h.req("GET", "/api/attendees?q="+url.QueryEscape(q), nil)
		rec.record("search:"+label, code)
		if code >= 500 {
			t.Errorf("search %s: status %d (expected 2xx/4xx)", label, code)
		}
	}

	rec.check(t)
	h.invariants(t)
	h.assertConservation(t)
	h.assertIntegrityAPI(t)
}

// ===========================================================================
// 2. Money/stock integrity under concurrent abuse on the SAME attendee + SAME
//    product (sales + topups + deliveries + duplicate request_ids), -race.
// ===========================================================================

func TestStormSameAttendeeSameProduct(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	ev := h.draftEvent(t)
	cat := h.category(t, "Bebidas")
	const stock = 80
	const price = 1000
	prod := h.product(t, cat, "Agua", price, stock)
	att := h.attendee(t, "Storm", 12000) // small balance -> exercises overage->cash floor
	h.activate(t, ev)
	h.mustJSON(t, "POST", "/api/caja/open", map[string]any{"opening_float": 0}, nil)

	rec := newRecorder()
	var wg sync.WaitGroup
	gate := make(chan struct{})

	const (
		saleG    = 200 // try to buy 1 unit each (more than stock -> oversell pressure)
		topupG   = 60
		deliverG = 30
		dupG     = 20
	)
	dupReqID := "dup-storm-shared"

	// sales (overage allowed via cash_confirmed; never should go negative)
	for i := 0; i < saleG; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-gate
			code, _ := h.req("POST", "/api/orders", map[string]any{
				"request_id":     fmt.Sprintf("sale-%d", n),
				"items":          []map[string]any{{"product_id": prod.ID, "qty": 1}},
				"payers":         []map[string]any{{"attendee_id": att.ID}},
				"cash_confirmed": true,
			})
			rec.record("sale", code)
		}(i)
	}
	// topups
	for i := 0; i < topupG; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-gate
			code, _ := h.req("POST", "/api/attendees/"+att.ID+"/topup",
				map[string]any{"amount": int64(500)})
			rec.record("topup", code)
		}(i)
	}
	// deliveries (partial, racing the sales that create the items)
	for i := 0; i < deliverG; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			for r := 0; r < 6; r++ {
				code, data := h.req("GET", "/api/pendientes", nil)
				rec.record("board", code)
				if code != http.StatusOK {
					continue
				}
				var board []struct {
					Items []struct {
						ID string `json:"id"`
					} `json:"items"`
				}
				if json.Unmarshal(data, &board) != nil || len(board) == 0 {
					continue
				}
				for _, o := range board {
					for _, it := range o.Items {
						code, _ := h.req("POST", "/api/pendientes/items/"+it.ID+"/entregar",
							map[string]any{"qty": 1})
						rec.record("entregar", code)
					}
				}
			}
		}()
	}
	// duplicate request_id fired concurrently -> must charge exactly once
	for i := 0; i < dupG; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			code, _ := h.req("POST", "/api/orders", map[string]any{
				"request_id":     dupReqID,
				"items":          []map[string]any{{"product_id": prod.ID, "qty": 1}},
				"payers":         []map[string]any{{"attendee_id": att.ID}},
				"cash_confirmed": true,
			})
			rec.record("dup", code)
		}()
	}

	close(gate)
	wg.Wait()

	rec.check(t)
	h.invariants(t)
	h.assertConservation(t)
	h.assertIntegrityAPI(t)

	// stock can never be oversold: ends >= 0, and movements reconcile (checked above)
	var stockQty int64
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT stock_qty FROM products WHERE id=?`, prod.ID).Scan(&stockQty)
	if stockQty < 0 {
		t.Errorf("oversold: stock=%d", stockQty)
	}

	// duplicate request_id charged exactly once
	var dupCount int
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM orders WHERE request_id=?`, dupReqID).Scan(&dupCount)
	if dupCount > 1 {
		t.Errorf("DOUBLE-SPEND: request_id %q produced %d orders, want <=1", dupReqID, dupCount)
	}
	t.Logf("final stock=%d, dup orders=%d", stockQty, dupCount)
}

// ===========================================================================
// 3. Lifecycle abuse.
// ===========================================================================

func TestLifecycleAbuse(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	// sell with no active event -> 400
	code, _ := h.req("POST", "/api/orders", map[string]any{
		"request_id": "no-ev", "items": []map[string]any{{"product_id": "x", "qty": 1}},
		"payers": []map[string]any{{"attendee_id": "y"}}})
	if code != http.StatusBadRequest {
		t.Errorf("sell with no active event: got %d want 400", code)
	}

	ev := h.draftEvent(t)
	cat := h.category(t, "Bebidas")
	prod := h.product(t, cat, "Cerveza", 1000, 50)
	h.attendee(t, "Lc", 50000)
	h.activate(t, ev)

	// mutate menu while active -> 409 (must NOT corrupt)
	for label, call := range map[string]func() (int, []byte){
		"create-category": func() (int, []byte) {
			return h.req("POST", "/api/categories", map[string]any{"name": "Late", "sort_order": 0})
		},
		"create-product": func() (int, []byte) {
			return h.req("POST", "/api/products", map[string]any{"category_id": cat,
				"name": "Late", "price": 1, "prep_station": "bar", "track_stock": 0, "stock_qty": 0})
		},
		"update-product": func() (int, []byte) {
			return h.req("PUT", "/api/products/"+prod.ID, map[string]any{"category_id": cat,
				"name": "X", "price": 1, "cost": 0, "prep_station": "bar",
				"track_stock": 1, "sort_order": 0, "active": 1})
		},
		"delete-product": func() (int, []byte) {
			return h.req("DELETE", "/api/products/"+prod.ID, nil)
		},
		"adjust-stock": func() (int, []byte) {
			return h.req("POST", "/api/inventory/adjust",
				map[string]any{"product_id": prod.ID, "delta": 10, "reason": "restock"})
		},
	} {
		code, data := call()
		if code != http.StatusConflict {
			t.Errorf("%s while active: got %d want 409 (body %s)", label, code, data)
		}
	}

	// double-activate (idempotent, must not break the one-active invariant)
	code, data := h.req("POST", "/api/events/"+ev+"/activate", map[string]any{})
	if code >= 500 {
		t.Errorf("double-activate: got %d (body %s)", code, data)
	}
	var activeCount int
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE status='active'`).Scan(&activeCount)
	if activeCount != 1 {
		t.Errorf("one-active invariant broken: %d active events", activeCount)
	}

	// close, then operate after closed -> selling/dashboard 400, no 5xx
	h.mustJSON(t, "POST", "/api/events/"+ev+"/close", map[string]any{}, nil)
	code, _ = h.req("POST", "/api/orders", map[string]any{
		"request_id": "after-close", "items": []map[string]any{{"product_id": prod.ID, "qty": 1}},
		"payers": []map[string]any{{"attendee_id": "y"}}})
	if code != http.StatusBadRequest {
		t.Errorf("sell after close: got %d want 400", code)
	}
	code, _ = h.req("GET", "/api/reports/dashboard", nil)
	if code != http.StatusBadRequest {
		t.Errorf("dashboard after close (no active): got %d want 400", code)
	}

	h.invariants(t)
}

// ===========================================================================
// 4. Tips distribution edge cases (pool=0, huge pool, <3 meseros, no aseo).
// ===========================================================================

func TestTipsEdgeCases(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	ev := h.draftEvent(t)
	h.activate(t, ev)

	// helper to fetch tips
	tips := func(pool string) (int64, int64, int) {
		code, data := h.req("GET", "/api/reports/tips?pool="+pool, nil)
		var r struct {
			Pool        int64 `json:"pool"`
			Distributed int64 `json:"distributed"`
			Meseros     []struct {
				Share int64 `json:"share"`
			} `json:"meseros"`
			Aseo []json.RawMessage `json:"aseo"`
		}
		_ = json.Unmarshal(data, &r)
		return r.Pool, r.Distributed, code
	}

	// no meseros, no aseo: pool=0 and huge pool -> distributed 0 (no recipients), no 5xx
	for _, p := range []string{"0", "1000000", "9223372036854775807", "-50", "notanumber"} {
		_, dist, code := tips(p)
		if code >= 500 {
			t.Errorf("tips pool=%s: status %d", p, code)
		}
		if dist != 0 {
			t.Errorf("tips pool=%s with no recipients: distributed=%d want 0", p, dist)
		}
	}

	// add 2 meseros (< 3) and aseo staff via config; ensure exact conservation.
	for _, name := range []string{"mesero1", "mesero2"} {
		h.mustJSON(t, "POST", "/api/users", map[string]any{
			"username": name, "full_name": name, "role": "mesero", "pin": "4321"}, nil)
	}
	// aseo staff config (names) — they always get the bonus.
	h.mustJSON(t, "PUT", "/api/config",
		map[string]string{"aseo_staff": `["Ana","Boris"]`}, nil)

	for _, p := range []string{"0", "1", "7", "1000000", "9223372036854775807"} {
		pool, dist, code := tips(p)
		if code >= 500 {
			t.Errorf("tips pool=%s: status %d", p, code)
		}
		// largest-remainder must distribute the pool EXACTLY when recipients exist.
		if pool > 0 && dist != pool {
			t.Errorf("tips pool=%s: distributed=%d != pool=%d (money created/lost)", p, dist, pool)
		}
		if dist < 0 {
			t.Errorf("tips pool=%s: negative distribution %d", p, dist)
		}
	}
	h.invariants(t)
}

// ===========================================================================
// 5. Split-payment allocation: two payers, cash confirmation, lending.
// ===========================================================================

func TestSplitAllocation(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	ev := h.draftEvent(t)
	cat := h.category(t, "Bebidas")
	prod := h.product(t, cat, "Combo", 10000, 100)
	rich := h.attendee(t, "Rich", 100000)
	poor := h.attendee(t, "Poor", 1000)
	h.activate(t, ev)
	h.mustJSON(t, "POST", "/api/caja/open", map[string]any{"opening_float": 0}, nil)

	// total 20000, split 2 => 10000 each. poor has 1000, rich covers shortfall
	// under cascade. No cash should be due.
	var ord orderResp
	h.mustJSON(t, "POST", "/api/orders", map[string]any{
		"request_id": "split-1",
		"items":      []map[string]any{{"product_id": prod.ID, "qty": 2}},
		"payers":     []map[string]any{{"attendee_id": rich.ID}, {"attendee_id": poor.ID}},
	}, &ord)
	if ord.Total != 20000 {
		t.Fatalf("split total: got %d want 20000", ord.Total)
	}
	if ord.PaidCash != 0 {
		t.Errorf("cascade split should need no cash, got cash=%d", ord.PaidCash)
	}
	if ord.PaidFromBalance != 20000 {
		t.Errorf("paid_from_balance=%d want 20000", ord.PaidFromBalance)
	}
	// poor fully drained, rich covered the rest
	var rb, pb int64
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id=?`, rich.ID).Scan(&rb)
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id=?`, poor.ID).Scan(&pb)
	if rb < 0 || pb < 0 {
		t.Errorf("negative balance after split: rich=%d poor=%d", rb, pb)
	}
	if rb+pb != 100000+1000-20000 {
		t.Errorf("split money math: rich+poor=%d want %d", rb+pb, 100000+1000-20000)
	}

	// Now exceed combined balance WITHOUT cash_confirmed -> must be rejected 400.
	code, _ := h.req("POST", "/api/orders", map[string]any{
		"request_id": "split-over",
		"items":      []map[string]any{{"product_id": prod.ID, "qty": 100}}, // 1,000,000
		"payers":     []map[string]any{{"attendee_id": rich.ID}, {"attendee_id": poor.ID}},
	})
	if code != http.StatusBadRequest {
		t.Errorf("over-balance split without cash_confirmed: got %d want 400", code)
	}

	h.invariants(t)
	h.assertConservation(t)
	h.assertIntegrityAPI(t)
}

// ===========================================================================
// 6. Partial delivery clamp + double delivery rejection.
// ===========================================================================

func TestDeliveryClamp(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	ev := h.draftEvent(t)
	cat := h.category(t, "Bebidas")
	prod := h.product(t, cat, "Agua", 1000, 100)
	att := h.attendee(t, "Del", 100000)
	h.activate(t, ev)

	var ord orderResp
	h.mustJSON(t, "POST", "/api/orders", map[string]any{
		"request_id": "deliv-1",
		"items":      []map[string]any{{"product_id": prod.ID, "qty": 3}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	}, &ord)

	// find the item id
	var itemID string
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT id FROM order_items WHERE order_id=?`, ord.ID).Scan(&itemID)

	// deliver way more than remaining -> must clamp to qty, not over-deliver
	code, data := h.req("POST", "/api/pendientes/items/"+itemID+"/entregar",
		map[string]any{"qty": 9999})
	if code != http.StatusOK {
		t.Fatalf("over-qty deliver: got %d body %s", code, data)
	}
	var dq, q int64
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT delivered_qty, qty FROM order_items WHERE id=?`, itemID).Scan(&dq, &q)
	if dq > q {
		t.Errorf("OVER-DELIVERED: delivered=%d qty=%d", dq, q)
	}
	if dq != 3 {
		t.Errorf("clamp: delivered=%d want 3", dq)
	}
	// delivering again -> already fully delivered -> 409
	code, _ = h.req("POST", "/api/pendientes/items/"+itemID+"/entregar", map[string]any{"qty": 1})
	if code != http.StatusConflict {
		t.Errorf("double deliver: got %d want 409", code)
	}
	h.invariants(t)
}

// ===========================================================================
// 7. Integer-overflow money math (price near math.MaxInt64).
//    A sale's line_total = unit_price * qty can overflow int64 silently.
//    Expected SAFE behavior: reject (4xx) OR charge correctly. Current code
//    persists a NEGATIVE-total order and does NOT charge the attendee -> the
//    attendee receives deliverable goods for free and payment conservation
//    breaks. This assertion FAILS, documenting the bug (see REPORT).
// ===========================================================================

func TestPriceOverflowMoneyMath(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	ev := h.draftEvent(t)
	cat := h.category(t, "Bebidas")

	// FIXED: an absurd price (> core.MaxMoney) is now rejected at CREATION, so it
	// can never reach a sale and overflow unit_price*qty into a negative total.
	for _, p := range []int64{math.MaxInt64, core.MaxMoney + 1} {
		code, data := h.req("POST", "/api/products", map[string]any{
			"category_id": cat, "name": "Diamond", "price": p, "cost": 0, "track_stock": 0,
		})
		if code != http.StatusBadRequest {
			t.Fatalf("price %d should be rejected at create: got %d (body %s), want 400", p, code, data)
		}
	}

	// A valid large price (== core.MaxMoney) is accepted and a sale charges it
	// EXACTLY with no overflow (MaxMoney*qty stays well within int64).
	prod := h.product(t, cat, "Pricey", core.MaxMoney, 100)
	att := h.attendee(t, "Whale", core.MaxMoney)
	h.activate(t, ev)
	h.mustJSON(t, "POST", "/api/caja/open", map[string]any{"opening_float": 0}, nil)

	code, data := h.req("POST", "/api/orders", map[string]any{
		"request_id": "big-1",
		"items":      []map[string]any{{"product_id": prod.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("valid large sale: got %d (body %s), want 2xx", code, data)
	}
	var ord orderResp
	_ = json.Unmarshal(data, &ord)
	if ord.Total != core.MaxMoney {
		t.Errorf("total=%d, want %d (charged exactly, no overflow)", ord.Total, core.MaxMoney)
	}
	h.assertConservation(t)
	h.invariants(t)
}

// ===========================================================================
// 8. Topup near int64 overflow must never corrupt or go negative.
// ===========================================================================

func TestTopupOverflow(t *testing.T) {
	h := newHarness(t)
	h.login(t, bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
	ev := h.draftEvent(t)
	att := h.attendee(t, "MaxMoney", 0)
	h.activate(t, ev)

	// First top-up to MaxInt64 (valid, fits).
	code, _ := h.req("POST", "/api/attendees/"+att.ID+"/topup",
		map[string]any{"amount": int64(math.MaxInt64)})
	t.Logf("topup MaxInt64 -> %d", code)

	// Second huge top-up overflows balance+amount. Must be rejected gracefully
	// (4xx), NOT a 500, and must never corrupt the balance.
	code, data := h.req("POST", "/api/attendees/"+att.ID+"/topup",
		map[string]any{"amount": int64(math.MaxInt64)})
	t.Logf("overflow topup -> status %d body %s", code, data)
	if code >= 500 || code == 0 {
		t.Errorf("ROBUSTNESS: overflow top-up returned %d (should be a graceful 4xx, not 5xx)", code)
	}

	var bal int64
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id=?`, att.ID).Scan(&bal)
	if bal < 0 {
		t.Errorf("NEGATIVE BALANCE via overflow: %d", bal)
	}
	h.invariants(t)
	h.assertIntegrityAPI(t)
}
