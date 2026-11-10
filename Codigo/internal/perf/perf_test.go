// Package perf is a performance + stress suite for Action EventOS. It boots the
// fully-wired in-process server (the cmd/eventpos module set) on a fresh temp
// SQLite DB, seeds a realistic large event (1 event, a few products, 5000
// attendees via bulk import), activates it, and then measures the hot paths an
// operator actually waits on:
//
//   - attendee SEARCH latency across varied query shapes (name prefix, full
//     name, doc_id): p95 must stay well under 500ms.
//   - a single caja SALE end-to-end latency: must finish under 1s.
//   - a concurrency BURST of mixed sales + top-ups across random attendees,
//     after which every money/stock invariant must still hold (no negative
//     balance/stock, cached == ledger/movement sum, GET /api/integrity ok, no
//     5xx) and throughput is logged.
//
// Run with -race.
package perf

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
	"net/url"
	"path/filepath"
	"sort"
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
// Harness — fully-wired server + direct DB access for invariant assertions.
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client // operator session (cookie-jar), thread-safe
	d      *db.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	w := writer.New(d.Writer)
	t.Cleanup(w.Close)

	hub := realtime.NewHub(256)
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

// req issues one request through the operator client. Safe for concurrent use.
// On a transport error it returns status 0 (recorded as a failure by callers).
func (h *harness) req(method, path string, body any) (int, []byte) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		return 0, nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (h *harness) mustReq(t *testing.T, method, path string, body, out any) {
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

func (h *harness) login(t *testing.T) {
	t.Helper()
	h.mustReq(t, "POST", "/api/auth/login",
		map[string]any{"username": bootstrap.DefaultAdminUser, "secret": bootstrap.DefaultAdminSecret}, nil)
}

// ---------------------------------------------------------------------------
// Wire shapes.
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type attendeeResp struct {
	ID      string `json:"id"`
	Balance int64  `json:"balance"`
}

type productResp struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int64  `json:"price"`
}

// ---------------------------------------------------------------------------
// Seeding.
// ---------------------------------------------------------------------------

const (
	numAttendees = 5000
	numProducts  = 4
	fundedPool   = 300       // attendees pre-funded for sales (subset of the 5000)
	initialTopup = 5_000_000 // ample: avoids overage/cash path during the burst
	bigStock     = 5_000_000 // ample: avoids stock exhaustion during the burst
)

var prodPrices = []int64{1000, 1500, 2000, 2500}

// seed builds the event, catalog and 5000 attendees, then activates. It returns
// the product list and the attendee ids.
func (h *harness) seed(t *testing.T) ([]productResp, []string) {
	t.Helper()

	var ev idResp
	h.mustReq(t, "POST", "/api/events",
		map[string]any{"name": "PerfFest", "default_topup": 0, "split_policy": "cascade"}, &ev)

	var cat idResp
	h.mustReq(t, "POST", "/api/categories", map[string]any{"name": "Bebidas", "sort_order": 0}, &cat)

	prods := make([]productResp, numProducts)
	for i := 0; i < numProducts; i++ {
		var p productResp
		h.mustReq(t, "POST", "/api/products", map[string]any{
			"category_id": cat.ID, "name": fmt.Sprintf("Prod%d", i),
			"price": prodPrices[i], "cost": int64(0), "prep_station": "bar",
			"track_stock": 1, "stock_qty": int64(bigStock),
		}, &p)
		p.Price = prodPrices[i]
		prods[i] = p
	}

	// Bulk import 5000 attendees CREATE-ONLY (initial_topup=0). This is deliberate:
	// the production audit chain-head lookup (app.WriteAudit ->
	// "ORDER BY at DESC, id DESC LIMIT 1") sorts the WHOLE audit_log on every
	// append, so each row written costs O(rows-so-far) and seeding is O(n^2) in
	// the TOTAL number of audit rows — independent of batch size. Funding every
	// attendee at import would double the audit rows (a topup audit per attendee)
	// and roughly quadruple seed time. Instead we create all 5000 (one audit row
	// each) and fund only a pool afterwards. See the reported O(n^2) defect in
	// app.WriteAudit. display_name = "Asistente N", doc_id = "DOC<N>".
	ids := make([]string, 0, numAttendees)
	const batch = 1000
	importStart := time.Now()
	for start := 0; start < numAttendees; start += batch {
		end := start + batch
		if end > numAttendees {
			end = numAttendees
		}
		rows := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			rows = append(rows, map[string]any{
				"display_name":  fmt.Sprintf("Asistente %d", i),
				"doc_id":        fmt.Sprintf("DOC%d", i),
				"email":         fmt.Sprintf("a%d@x.com", i),
				"phone":         fmt.Sprintf("300%07d", i),
				"initial_topup": int64(0),
			})
		}
		var resp struct {
			Count     int            `json:"count"`
			Attendees []attendeeResp `json:"attendees"`
		}
		h.mustReq(t, "POST", "/api/attendees/import", map[string]any{"attendees": rows}, &resp)
		if resp.Count != end-start {
			t.Fatalf("bulk import [%d:%d]: count=%d, want %d", start, end, resp.Count, end-start)
		}
		for _, a := range resp.Attendees {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) != numAttendees {
		t.Fatalf("seeded %d attendees, want %d", len(ids), numAttendees)
	}

	// Fund a pool of attendees (ids[:fundedPool]) so the sale paths have balance.
	for i := 0; i < fundedPool; i++ {
		h.mustReq(t, "POST", "/api/attendees/"+ids[i]+"/topup", map[string]any{"amount": int64(initialTopup)}, nil)
	}
	t.Logf("seeded %d attendees (bulk import, %d funded) in %s",
		len(ids), fundedPool, time.Since(importStart).Round(time.Millisecond))

	// Activate so selling is allowed.
	h.mustReq(t, "POST", "/api/events/"+ev.ID+"/activate", map[string]any{}, nil)
	return prods, ids
}

// p95 returns the 95th-percentile duration from a slice (input is copied+sorted).
func p95(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), durs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)) * 0.95)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func maxDur(durs []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range durs {
		if d > m {
			m = d
		}
	}
	return m
}

// statusRec collects the status distribution of the concurrency burst and flags
// any 5xx/transport failure.
type statusRec struct {
	mu       sync.Mutex
	statuses map[int]int
	fivexx   []string
	total    int64
}

func newStatusRec() *statusRec { return &statusRec{statuses: map[int]int{}} }

func (r *statusRec) record(label string, code int) {
	atomic.AddInt64(&r.total, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[code]++
	if code >= 500 || code == 0 {
		if len(r.fivexx) < 50 {
			r.fivexx = append(r.fivexx, fmt.Sprintf("%s -> %d", label, code))
		}
	}
}

// TestPerfAt5000 seeds ONE event with 5000 attendees (bulk import) and then runs
// all three measurements against that single dataset: search latency, single
// sale latency, and a concurrency burst with full integrity verification. Seeding
// once (rather than per-test) keeps the suite tractable under -race.
func TestPerfAt5000(t *testing.T) {
	h := newHarness(t)
	h.login(t)
	prods, ids := h.seed(t)
	h.mustReq(t, "POST", "/api/caja/open", map[string]any{"opening_float": 100000}, nil)

	// -------------------------------------------------------------------
	// 1. Search latency across varied query shapes.
	// -------------------------------------------------------------------
	t.Run("search_latency", func(t *testing.T) {
		rng := rand.New(rand.NewSource(1))
		queries := make([]string, 0, 300)
		for i := 0; i < 100; i++ {
			queries = append(queries, "Asistente")                                         // broad prefix (matches all, LIMIT 25)
			queries = append(queries, fmt.Sprintf("Asistente %d", rng.Intn(numAttendees))) // full name
			queries = append(queries, fmt.Sprintf("DOC%d", rng.Intn(numAttendees)))        // doc_id
		}

		durs := make([]time.Duration, 0, len(queries))
		for _, q := range queries {
			path := "/api/attendees?q=" + url.QueryEscape(q)
			start := time.Now()
			code, data := h.req("GET", path, nil)
			durs = append(durs, time.Since(start))
			if code != http.StatusOK {
				t.Fatalf("search q=%q: status %d body %s", q, code, data)
			}
		}

		// Correctness spot-check: a full-name query resolves the exact attendee.
		var hits []attendeeResp
		h.mustReq(t, "GET", "/api/attendees?q="+url.QueryEscape("Asistente 4321"), nil, &hits)
		if len(hits) == 0 {
			t.Fatalf("full-name search returned no results")
		}

		got := p95(durs)
		t.Logf("SEARCH @%d attendees over %d queries: p95=%s max=%s", numAttendees, len(durs), got, maxDur(durs))
		if got >= 500*time.Millisecond {
			t.Fatalf("search p95=%s exceeds 500ms budget", got)
		}
	})

	// -------------------------------------------------------------------
	// 2. Single caja sale end-to-end latency.
	// -------------------------------------------------------------------
	t.Run("single_sale_latency", func(t *testing.T) {
		start := time.Now()
		var ord struct {
			ID    string `json:"id"`
			Total int64  `json:"total"`
		}
		h.mustReq(t, "POST", "/api/orders", map[string]any{
			"request_id": "perf-single-sale",
			"items":      []map[string]any{{"product_id": prods[0].ID, "qty": 1}},
			"payers":     []map[string]any{{"attendee_id": ids[0]}},
		}, &ord)
		elapsed := time.Since(start)

		t.Logf("SINGLE SALE end-to-end latency: %s (total=%d)", elapsed.Round(time.Microsecond), ord.Total)
		if elapsed >= time.Second {
			t.Fatalf("single sale latency %s exceeds 1s budget", elapsed)
		}
		if ord.Total != prods[0].Price {
			t.Fatalf("sale total: got %d, want %d", ord.Total, prods[0].Price)
		}
	})

	// -------------------------------------------------------------------
	// 3. Concurrency burst: 300 mixed sales + top-ups, then integrity.
	// -------------------------------------------------------------------
	t.Run("concurrency_burst", func(t *testing.T) {
		const burst = 300
		rec := newStatusRec()
		startGate := make(chan struct{})
		var wg sync.WaitGroup

		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(seed) + 1))
				<-startGate
				if seed%2 == 0 {
					// Sale: payer drawn from the pre-funded pool so it has balance.
					att := ids[rng.Intn(fundedPool)]
					p := prods[rng.Intn(len(prods))]
					code, _ := h.req("POST", "/api/orders", map[string]any{
						"request_id": fmt.Sprintf("burst-sale-%d", seed),
						"items":      []map[string]any{{"product_id": p.ID, "qty": int64(1 + rng.Intn(2))}},
						"payers":     []map[string]any{{"attendee_id": att}},
					})
					rec.record("sale", code)
				} else {
					// Top-up: any of the 5000 random attendees.
					att := ids[rng.Intn(len(ids))]
					code, _ := h.req("POST", "/api/attendees/"+att+"/topup",
						map[string]any{"amount": int64(1000 + rng.Intn(10)*500)})
					rec.record("topup", code)
				}
			}(i)
		}

		start := time.Now()
		close(startGate)
		wg.Wait()
		elapsed := time.Since(start)

		total := atomic.LoadInt64(&rec.total)
		t.Logf("BURST: %d concurrent ops in %s (%.0f ops/s); status distribution: %v",
			total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), rec.statuses)

		if len(rec.fivexx) > 0 {
			t.Errorf("server errors during burst: %v", rec.fivexx)
		}

		// --- Invariants after the storm (direct SQL) ---
		assertNoNegativeBalance(t, h)
		assertNoNegativeStock(t, h)
		assertLedgerMatchesBalance(t, h)
		assertStockMovementsMatchQty(t, h)

		var integ struct {
			OK    bool `json:"ok"`
			Audit struct {
				Intact   bool   `json:"intact"`
				BrokenAt string `json:"broken_at"`
			} `json:"audit"`
		}
		h.mustReq(t, "GET", "/api/integrity", nil, &integ)
		if !integ.OK || !integ.Audit.Intact {
			t.Errorf("integrity after burst: ok=%v audit_intact=%v broken_at=%q", integ.OK, integ.Audit.Intact, integ.Audit.BrokenAt)
		}

		var orderCount int
		if err := h.d.Reader.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orderCount); err != nil {
			t.Fatalf("order count: %v", err)
		}
		t.Logf("persisted %d orders total after the burst", orderCount)
		if orderCount == 0 {
			t.Errorf("expected some orders to persist from the burst")
		}
	})
}

// ---------------------------------------------------------------------------
// Direct-SQL invariant helpers.
// ---------------------------------------------------------------------------

func assertNoNegativeBalance(t *testing.T, h *harness) {
	t.Helper()
	var n int
	if err := h.d.Reader.QueryRow(`SELECT COUNT(*) FROM attendees WHERE balance < 0`).Scan(&n); err != nil {
		t.Fatalf("negative balance query: %v", err)
	}
	if n != 0 {
		t.Errorf("found %d attendees with negative balance", n)
	}
}

func assertNoNegativeStock(t *testing.T, h *harness) {
	t.Helper()
	var n int
	if err := h.d.Reader.QueryRow(`SELECT COUNT(*) FROM products WHERE track_stock=1 AND stock_qty < 0`).Scan(&n); err != nil {
		t.Fatalf("negative stock query: %v", err)
	}
	if n != 0 {
		t.Errorf("found %d products with negative stock", n)
	}
}

func assertLedgerMatchesBalance(t *testing.T, h *harness) {
	t.Helper()
	var n int
	err := h.d.Reader.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT a.id FROM attendees a
			LEFT JOIN balance_ledger l ON l.attendee_id = a.id
			GROUP BY a.id
			HAVING a.balance != COALESCE(SUM(l.amount),0))`).Scan(&n)
	if err != nil {
		t.Fatalf("ledger mismatch query: %v", err)
	}
	if n != 0 {
		t.Errorf("%d attendee balances disagree with their ledger", n)
	}
}

func assertStockMovementsMatchQty(t *testing.T, h *harness) {
	t.Helper()
	var n int
	err := h.d.Reader.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT p.id FROM products p
			LEFT JOIN stock_movements sm ON sm.product_id = p.id
			WHERE p.track_stock = 1
			GROUP BY p.id
			HAVING p.stock_qty != COALESCE(SUM(sm.delta),0))`).Scan(&n)
	if err != nil {
		t.Fatalf("stock mismatch query: %v", err)
	}
	if n != 0 {
		t.Errorf("%d product stocks disagree with their movements", n)
	}
}
