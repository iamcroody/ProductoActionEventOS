// Package loadtest is a HIGH-INTENSITY end-to-end load + integrity suite for
// Action EventOS. It boots the fully-wired server in-process (the exact module
// set cmd/eventpos builds, INCLUDING the clientapi self-service module) on a
// fresh temp SQLite DB, configures a realistic event while it is 'draft',
// activates it, then hammers the WHOLE system concurrently from many goroutines:
//
//   - Caja sales      : operators POST /api/orders for random attendees.
//   - Client web orders: each attendee's "phone" runs its OWN cookie-jar client,
//     authenticates via POST /api/cliente/acceso, then POSTs /api/cliente/pedido
//     several times — exercising the customer self-service web path under load.
//   - Top-ups         : operators POST /api/attendees/{id}/topup.
//   - Deliveries      : operators GET /api/pendientes and POST .../entregar
//     (partial) against whatever is pending.
//   - Idempotency     : a pool of goroutines reuse ONE request_id concurrently;
//     the system must charge exactly once.
//
// After the storm it proves nothing broke: no HTTP 5xx anywhere; every balance
// and stock cache equals its immutable ledger/movement sum; GET /api/integrity
// reports ok:true with an intact audit chain; delivered_qty never exceeds qty;
// and money is conserved (payments == order totals; balance debits + cash ==
// order value). Run with -race.
package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/clientapi"
	"eventpos/internal/config"
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
// Harness — mirrors internal/apitest/internal/stress, plus the clientapi module
// and direct DB access for invariant assertions.
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client // operator session (cookie-jar), thread-safe
	d      *db.DB       // direct DB access for SQL invariant assertions
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "loadtest.db"))
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
		oversight.New(deps),
		clientapi.New(deps), // the customer self-service web path
	}

	srv := server.New(deps, fstest.MapFS{}, mods...)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &harness{ts: ts, client: &http.Client{Jar: jar}, d: d}
}

// reqWith issues one request through the supplied client and returns
// (status, body). Safe for concurrent use (http.Client + cookiejar are
// thread-safe). On a transport-level error it returns status 0.
func (h *harness) reqWith(client *http.Client, method, path string, body any) (int, []byte) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			panic(fmt.Sprintf("marshal %s %s: %v", method, path, err))
		}
		r = bytes.NewReader(b)
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
		panic(fmt.Sprintf("do %s %s: %v", method, path, err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(fmt.Sprintf("read body %s %s: %v", method, path, err))
	}
	return resp.StatusCode, data
}

// req is the operator-session shorthand.
func (h *harness) req(method, path string, body any) (int, []byte) {
	return h.reqWith(h.client, method, path, body)
}

func (h *harness) login(username, secret string) {
	code, data := h.req("POST", "/api/auth/login",
		map[string]any{"username": username, "secret": secret})
	if code != http.StatusOK {
		panic(fmt.Sprintf("login %s: status %d body %s", username, code, data))
	}
}

// ---------------------------------------------------------------------------
// Setup helpers (test goroutine only, pre-storm).
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type attendeeResp struct {
	ID      string `json:"id"`
	Balance int64  `json:"balance"`
	QRToken string `json:"qr_token"`
}

type productResp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	StockQty int64  `json:"stock_qty"`
	Price    int64  `json:"price"`
}

func (h *harness) mustSetup(t *testing.T, method, path string, body, out any) {
	t.Helper()
	code, data := h.req(method, path, body)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("%s %s: status %d body %s", method, path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode: %v (body %s)", method, path, err, data)
		}
	}
}

func (h *harness) createEvent(t *testing.T) string {
	t.Helper()
	var ev idResp
	h.mustSetup(t, "POST", "/api/events",
		map[string]any{"name": "LoadFest", "default_topup": 0, "split_policy": "cascade"}, &ev)
	return ev.ID
}

func (h *harness) activate(t *testing.T, id string) {
	t.Helper()
	h.mustSetup(t, "POST", "/api/events/"+id+"/activate", map[string]any{}, nil)
}

func (h *harness) createCategory(t *testing.T, name string) string {
	t.Helper()
	var c idResp
	h.mustSetup(t, "POST", "/api/categories",
		map[string]any{"name": name, "sort_order": 0}, &c)
	return c.ID
}

func (h *harness) createProduct(t *testing.T, catID, name string, price, stock int64, station string) productResp {
	t.Helper()
	var p productResp
	h.mustSetup(t, "POST", "/api/products", map[string]any{
		"category_id":  catID,
		"name":         name,
		"price":        price,
		"cost":         0,
		"prep_station": station,
		"track_stock":  1,
		"stock_qty":    stock,
	}, &p)
	p.Price = price
	return p
}

// identity is the full self-service identity for one attendee.
type identity struct {
	name, email, doc, phone string
}

func identityFor(i int) identity {
	return identity{
		name:  fmt.Sprintf("Cli %d", i),
		email: fmt.Sprintf("cli%d@x.com", i),
		doc:   fmt.Sprintf("doc%d", i),
		phone: fmt.Sprintf("phone%d", i),
	}
}

func (h *harness) createAttendee(t *testing.T, id identity, topup int64) attendeeResp {
	t.Helper()
	var a attendeeResp
	h.mustSetup(t, "POST", "/api/attendees", map[string]any{
		"display_name":  id.name,
		"email":         id.email,
		"doc_id":        id.doc,
		"phone":         id.phone,
		"initial_topup": topup,
	}, &a)
	return a
}

// ---------------------------------------------------------------------------
// Status recorder — collects the status distribution and flags any 5xx.
// ---------------------------------------------------------------------------

type recorder struct {
	mu       sync.Mutex
	statuses map[int]int
	fivexx   []string
	total    int64
}

func newRecorder() *recorder { return &recorder{statuses: map[int]int{}} }

func (rec *recorder) record(label string, code int) {
	atomic.AddInt64(&rec.total, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.statuses[code]++
	if code >= 500 || code == 0 {
		if len(rec.fivexx) < 50 {
			rec.fivexx = append(rec.fivexx, fmt.Sprintf("%s -> %d", label, code))
		}
	}
}

// ---------------------------------------------------------------------------
// Direct-SQL invariant assertions (after the storm).
// ---------------------------------------------------------------------------

func (h *harness) assertNoNegativeStock(t *testing.T) {
	t.Helper()
	rows, err := h.d.Reader.QueryContext(context.Background(),
		`SELECT id, name, stock_qty FROM products WHERE track_stock = 1 AND stock_qty < 0`)
	if err != nil {
		t.Fatalf("negative stock query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var qty int64
		_ = rows.Scan(&id, &name, &qty)
		t.Errorf("NEGATIVE STOCK: product %s (%s) stock_qty=%d", id, name, qty)
	}
}

func (h *harness) assertNoNegativeBalance(t *testing.T) {
	t.Helper()
	rows, err := h.d.Reader.QueryContext(context.Background(),
		`SELECT id, display_name, balance FROM attendees WHERE balance < 0`)
	if err != nil {
		t.Fatalf("negative balance query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var bal int64
		_ = rows.Scan(&id, &name, &bal)
		t.Errorf("NEGATIVE BALANCE: attendee %s (%s) balance=%d", id, name, bal)
	}
}

// assertLedgerMatchesBalance: SUM(balance_ledger.amount) == attendees.balance.
func (h *harness) assertLedgerMatchesBalance(t *testing.T) {
	t.Helper()
	rows, err := h.d.Reader.QueryContext(context.Background(), `
		SELECT a.id, a.display_name, a.balance, COALESCE(SUM(l.amount),0) AS ledger_sum
		FROM attendees a
		LEFT JOIN balance_ledger l ON l.attendee_id = a.id
		GROUP BY a.id
		HAVING a.balance != COALESCE(SUM(l.amount),0)`)
	if err != nil {
		t.Fatalf("ledger mismatch query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var cached, computed int64
		_ = rows.Scan(&id, &name, &cached, &computed)
		t.Errorf("LEDGER MISMATCH: attendee %s (%s) cached=%d ledger_sum=%d diff=%d",
			id, name, cached, computed, cached-computed)
	}
}

// assertStockMovementsMatchQty: SUM(stock_movements.delta) == products.stock_qty.
func (h *harness) assertStockMovementsMatchQty(t *testing.T) {
	t.Helper()
	rows, err := h.d.Reader.QueryContext(context.Background(), `
		SELECT p.id, p.name, p.stock_qty, COALESCE(SUM(sm.delta),0) AS movement_sum
		FROM products p
		LEFT JOIN stock_movements sm ON sm.product_id = p.id
		WHERE p.track_stock = 1
		GROUP BY p.id
		HAVING p.stock_qty != COALESCE(SUM(sm.delta),0)`)
	if err != nil {
		t.Fatalf("stock movement mismatch query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var cached, computed int64
		_ = rows.Scan(&id, &name, &cached, &computed)
		t.Errorf("STOCK MOVEMENT MISMATCH: product %s (%s) cached=%d movement_sum=%d diff=%d",
			id, name, cached, computed, cached-computed)
	}
}

// assertDeliveredNotExceedQty: delivered_qty never exceeds qty for any item.
func (h *harness) assertDeliveredNotExceedQty(t *testing.T) {
	t.Helper()
	rows, err := h.d.Reader.QueryContext(context.Background(),
		`SELECT id, order_id, qty, delivered_qty FROM order_items WHERE delivered_qty > qty`)
	if err != nil {
		t.Fatalf("delivered overshoot query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, oid string
		var qty, dq int64
		_ = rows.Scan(&id, &oid, &qty, &dq)
		t.Errorf("OVER-DELIVERED: item %s (order %s) delivered_qty=%d > qty=%d", id, oid, dq, qty)
	}
}

// assertConservation: for non-voided orders, SUM(payments)==SUM(order totals),
// and balance debits + cash collected == total order value.
func (h *harness) assertConservation(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var orderTotal, payTotal, balPay, cashPay int64
	if err := h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total),0) FROM orders WHERE status <> 'voided'`).Scan(&orderTotal); err != nil {
		t.Fatalf("order total query: %v", err)
	}
	if err := h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(p.amount),0) FROM payments p
		 JOIN orders o ON o.id = p.order_id WHERE o.status <> 'voided'`).Scan(&payTotal); err != nil {
		t.Fatalf("payment total query: %v", err)
	}
	if err := h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(p.amount),0) FROM payments p
		 JOIN orders o ON o.id = p.order_id WHERE o.status <> 'voided' AND p.method = 'balance'`).Scan(&balPay); err != nil {
		t.Fatalf("balance payment query: %v", err)
	}
	if err := h.d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(p.amount),0) FROM payments p
		 JOIN orders o ON o.id = p.order_id WHERE o.status <> 'voided' AND p.method = 'cash'`).Scan(&cashPay); err != nil {
		t.Fatalf("cash payment query: %v", err)
	}
	if payTotal != orderTotal {
		t.Errorf("CONSERVATION: payments=%d != order totals=%d (diff %d)", payTotal, orderTotal, payTotal-orderTotal)
	}
	if balPay+cashPay != orderTotal {
		t.Errorf("CONSERVATION: balance(%d)+cash(%d)=%d != order value=%d",
			balPay, cashPay, balPay+cashPay, orderTotal)
	}
	t.Logf("conservation OK: orderTotal=%d payments=%d (balance=%d cash=%d)",
		orderTotal, payTotal, balPay, cashPay)
}

// assertIntegrityAPI: GET /api/integrity -> ok:true and audit chain intact.
func (h *harness) assertIntegrityAPI(t *testing.T) {
	t.Helper()
	code, data := h.req("GET", "/api/integrity", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/integrity: status=%d body=%s", code, data)
	}
	var result struct {
		OK    bool `json:"ok"`
		Audit struct {
			Intact   bool   `json:"intact"`
			BrokenAt string `json:"broken_at"`
		} `json:"audit"`
		BalanceMismatches []json.RawMessage `json:"balance_mismatches"`
		StockMismatches   []json.RawMessage `json:"stock_mismatches"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode integrity response: %v (body %s)", err, data)
	}
	if !result.OK {
		t.Errorf("integrity ok=false; balance_mismatches=%d stock_mismatches=%d audit_intact=%v broken_at=%q",
			len(result.BalanceMismatches), len(result.StockMismatches),
			result.Audit.Intact, result.Audit.BrokenAt)
	}
	if !result.Audit.Intact {
		t.Errorf("audit chain broken at %q", result.Audit.BrokenAt)
	}
}

// ---------------------------------------------------------------------------
// The storm.
// ---------------------------------------------------------------------------

func TestFullSystemStorm(t *testing.T) {
	const (
		numAttendees = 40        // index 0 reserved for the idempotency proof
		initialTopup = 1_000_000 // funds many orders without overage/cash path
		numProducts  = 5
		stockEach    = 5_000

		cajaGoroutines     = 120 // operator POST /api/orders
		clientPedidos      = 3   // self-service orders per client goroutine
		topupGoroutines    = 40
		deliveryGoroutines = 25
		deliveryRounds     = 4
		idemGoroutines     = 12
		idemReqID          = "idem-shared-storm-1"
	)
	prices := []int64{1000, 1500, 2000, 2500, 3000}

	h := newHarness(t)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	// ---- configure while DRAFT ----
	evID := h.createEvent(t)
	cat := h.createCategory(t, "Bebidas")
	prods := make([]productResp, numProducts)
	for i := 0; i < numProducts; i++ {
		prods[i] = h.createProduct(t, cat, fmt.Sprintf("Prod%d", i), prices[i], stockEach, "bar")
	}
	atts := make([]attendeeResp, numAttendees)
	ids := make([]identity, numAttendees)
	for i := 0; i < numAttendees; i++ {
		ids[i] = identityFor(i)
		atts[i] = h.createAttendee(t, ids[i], initialTopup)
		if atts[i].Balance != initialTopup {
			t.Fatalf("attendee %d initial balance: got %d want %d", i, atts[i].Balance, initialTopup)
		}
	}

	// ---- activate (selling + client orders require active) ----
	h.activate(t, evID)

	rec := newRecorder()
	startGate := make(chan struct{})
	var wg sync.WaitGroup

	// randAtt returns a random attendee index in [1, numAttendees) — index 0 is
	// kept pristine for the idempotency proof.
	randAtt := func(rng *rand.Rand) int { return 1 + rng.Intn(numAttendees-1) }

	// --- Caja sales (operator cookie) ---
	for i := 0; i < cajaGoroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed) + 1))
			<-startGate

			att := atts[randAtt(rng)]
			nItems := 1 + rng.Intn(2) // 1..2 distinct products
			seen := map[string]bool{}
			items := make([]map[string]any, 0, nItems)
			for len(items) < nItems {
				p := prods[rng.Intn(numProducts)]
				if seen[p.ID] {
					continue
				}
				seen[p.ID] = true
				items = append(items, map[string]any{"product_id": p.ID, "qty": 1 + rng.Intn(2)})
			}
			code, _ := h.req("POST", "/api/orders", map[string]any{
				"request_id": fmt.Sprintf("caja-%d-%s", seed, randID()),
				"items":      items,
				"payers":     []map[string]any{{"attendee_id": att.ID}},
			})
			rec.record("caja-order", code)
		}(i)
	}

	// --- Client self-orders (WEB participation): own cookie jar per client ---
	for i := 1; i < numAttendees; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Jar: jar}
			id := ids[idx]
			<-startGate

			// Authenticate the customer's phone. Half use the QR token to locate
			// (still requires every identity field to match); half use cédula.
			body := map[string]any{
				"nombre": id.name, "correo": id.email, "cedula": id.doc, "celular": id.phone,
			}
			if idx%2 == 0 {
				body["qr_token"] = atts[idx].QRToken
			}
			code, _ := h.reqWith(client, "POST", "/api/cliente/acceso", body)
			rec.record("cliente-acceso", code)
			if code != http.StatusOK {
				return // cannot place orders without the aeos_client cookie
			}

			rng := rand.New(rand.NewSource(int64(idx) + 7_000))
			for j := 0; j < clientPedidos; j++ {
				p := prods[rng.Intn(numProducts)]
				code, _ := h.reqWith(client, "POST", "/api/cliente/pedido", map[string]any{
					"request_id": fmt.Sprintf("cli-%d-%d-%s", idx, j, randID()),
					"items":      []map[string]any{{"product_id": p.ID, "qty": 1 + rng.Intn(2)}},
				})
				rec.record("cliente-pedido", code)
			}
		}(i)
	}

	// --- Top-ups (operator cookie) ---
	for i := 0; i < topupGoroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed) + 100_000))
			<-startGate
			att := atts[randAtt(rng)]
			code, _ := h.req("POST", "/api/attendees/"+att.ID+"/topup",
				map[string]any{"amount": int64(500 + rng.Intn(5)*500)})
			rec.record("topup", code)
		}(i)
	}

	// --- Deliveries (operator cookie): partial entregar on whatever is pending ---
	for i := 0; i < deliveryGoroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed) + 500_000))
			<-startGate
			for r := 0; r < deliveryRounds; r++ {
				code, data := h.req("GET", "/api/pendientes", nil)
				rec.record("pendientes-board", code)
				if code != http.StatusOK {
					continue
				}
				var board []struct {
					Items []struct {
						ID      string `json:"id"`
						Pending int64  `json:"pending"`
					} `json:"items"`
				}
				if json.Unmarshal(data, &board) != nil || len(board) == 0 {
					continue
				}
				// Pick a random order with at least one pending item.
				o := board[rng.Intn(len(board))]
				if len(o.Items) == 0 {
					continue
				}
				it := o.Items[rng.Intn(len(o.Items))]
				code, _ = h.req("POST", "/api/pendientes/items/"+it.ID+"/entregar",
					map[string]any{"qty": 1}) // partial delivery of one unit
				rec.record("entregar", code)
			}
		}(i)
	}

	// --- Idempotency: many goroutines, ONE request_id, attendee[0], prods[0] ---
	idemBody := map[string]any{
		"request_id": idemReqID,
		"items":      []map[string]any{{"product_id": prods[0].ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": atts[0].ID}},
	}
	idemResults := make([]string, idemGoroutines)
	for i := 0; i < idemGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startGate
			code, data := h.req("POST", "/api/orders", idemBody)
			rec.record("idem-order", code)
			var resp struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(data, &resp)
			idemResults[idx] = resp.ID
		}(i)
	}

	// ---- fire ----
	start := time.Now()
	close(startGate)
	wg.Wait()
	elapsed := time.Since(start)

	total := atomic.LoadInt64(&rec.total)
	t.Logf("STORM complete in %s: %d ops (%.0f ops/s)", elapsed.Round(time.Millisecond),
		total, float64(total)/elapsed.Seconds())
	t.Logf("status distribution: %v", rec.statuses)

	// ---- proof: no 5xx / transport failures anywhere ----
	if len(rec.fivexx) > 0 {
		t.Errorf("server errors (5xx/transport) occurred: %v", rec.fivexx)
	}

	// ---- proof: idempotency charged exactly once ----
	// All non-empty responses must carry the same order id.
	var firstID string
	for _, id := range idemResults {
		if id == "" {
			continue
		}
		if firstID == "" {
			firstID = id
		} else if id != firstID {
			t.Errorf("idempotency: divergent order ids %q vs %q", firstID, id)
		}
	}
	if firstID == "" {
		t.Errorf("idempotency: no order id returned by any goroutine")
	}
	var idemCount int
	if err := h.d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM orders WHERE request_id = ?`, idemReqID).Scan(&idemCount); err != nil {
		t.Fatalf("idem count query: %v", err)
	}
	if idemCount != 1 {
		t.Errorf("idempotency: %d orders with request_id=%q, want exactly 1", idemCount, idemReqID)
	}
	// attendee[0] saw exactly one charge (it is excluded from all other traffic).
	var bal0 int64
	if err := h.d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id = ?`, atts[0].ID).Scan(&bal0); err != nil {
		t.Fatalf("attendee[0] balance query: %v", err)
	}
	if want := int64(initialTopup) - prices[0]; bal0 != want {
		t.Errorf("idempotency: attendee[0] balance=%d, want %d (charged %d times)",
			bal0, want, (int64(initialTopup)-bal0)/prices[0])
	}

	// ---- proof: integrity invariants ----
	h.assertNoNegativeStock(t)
	h.assertNoNegativeBalance(t)
	h.assertLedgerMatchesBalance(t)
	h.assertStockMovementsMatchQty(t)
	h.assertDeliveredNotExceedQty(t)
	h.assertConservation(t)
	h.assertIntegrityAPI(t)

	// Sanity: some real work actually happened (the storm wasn't a no-op).
	var orderCount, deliveredItems int
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM orders`).Scan(&orderCount)
	_ = h.d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM order_items WHERE delivered_qty > 0`).Scan(&deliveredItems)
	t.Logf("persisted: %d orders, %d order_items with deliveries", orderCount, deliveredItems)
	if orderCount < 2 {
		t.Errorf("expected many orders to persist, got %d", orderCount)
	}
}

// randID returns a short unique-enough suffix for request ids.
func randID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
}
