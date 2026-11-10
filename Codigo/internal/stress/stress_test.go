// Package stress is a concurrency / stress test suite for Action EventOS.
// It boots the fully-wired server in-process (identical harness to
// internal/apitest) and fires hundreds of concurrent HTTP orders + topups
// against it, then verifies every correctness invariant:
//
//   - stock_qty never negative
//   - attendee balance never negative
//   - SUM(balance_ledger.amount) == attendees.balance for every attendee
//   - SUM(stock_movements.delta) == products.stock_qty for every tracked product
//   - GET /api/integrity ok == true (audit chain intact)
//
// It also hammers the same request_id from many goroutines simultaneously and
// asserts that exactly one charge was made (idempotency under concurrency).
package stress

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
// Harness – mirrors internal/apitest exactly, plus direct DB access.
// ---------------------------------------------------------------------------

type harness struct {
	ts     *httptest.Server
	client *http.Client // shared, thread-safe; cookie jar persists login session
	d      *db.DB       // direct DB access for SQL invariant assertions
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "stress.db"))
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

// req issues one request and returns (status, body).  It is safe for
// concurrent use: http.Client and cookiejar.Jar are both thread-safe.
func (h *harness) req(method, path string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		return 0, nil, fmt.Errorf("new request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("do %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read body %s %s: %w", method, path, err)
	}
	return resp.StatusCode, data, nil
}

// mustReq is a test-goroutine-safe variant; callers pass errors back via the
// errCh channel rather than calling t.Fatal inside a goroutine.
func (h *harness) mustReq(method, path string, body any) (int, []byte) {
	code, data, err := h.req(method, path, body)
	if err != nil {
		panic(fmt.Sprintf("request %s %s failed: %v", method, path, err))
	}
	return code, data
}

// login logs in and panics if it fails (called from setup goroutine only).
func (h *harness) login(username, secret string) {
	code, data, err := h.req("POST", "/api/auth/login",
		map[string]any{"username": username, "secret": secret})
	if err != nil || code != http.StatusOK {
		panic(fmt.Sprintf("login %s: status %d err %v body %s", username, code, err, data))
	}
}

// ---------------------------------------------------------------------------
// Setup helpers (called from the test goroutine only, pre-storm).
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

func (h *harness) mustSetup(t *testing.T, method, path string, body any, out any) {
	t.Helper()
	code, data, err := h.req(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("%s %s: status %d body %s", method, path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode: %v (body %s)", method, path, err, data)
		}
	}
}

// createEvent creates an event (which starts in 'draft' status) and returns its
// id. Under the lock-down lifecycle, configuration (catalog, attendees) is only
// allowed while draft; call activate before the sale/concurrency phase.
func (h *harness) createEvent(t *testing.T) string {
	t.Helper()
	var ev idResp
	h.mustSetup(t, "POST", "/api/events",
		map[string]any{"name": "StressFest", "default_topup": 0, "split_policy": "cascade"}, &ev)
	return ev.ID
}

// activate promotes a draft event to 'active' so selling is allowed.
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

func (h *harness) createAttendee(t *testing.T, name string, balance int64) attendeeResp {
	t.Helper()
	var a attendeeResp
	h.mustSetup(t, "POST", "/api/attendees",
		map[string]any{"display_name": name, "initial_topup": balance}, &a)
	return a
}

// ---------------------------------------------------------------------------
// Direct-SQL invariant assertions (called after the storm).
// ---------------------------------------------------------------------------

// assertNoNegativeStock checks products.stock_qty >= 0 for all tracked products.
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

// assertNoNegativeBalance checks attendees.balance >= 0 for all attendees.
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

// assertLedgerMatchesBalance verifies SUM(balance_ledger.amount)==attendees.balance.
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

// assertStockMovementsMatchQty verifies SUM(stock_movements.delta)==products.stock_qty.
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

// assertIntegrityAPI calls GET /api/integrity and asserts ok==true and audit intact.
func (h *harness) assertIntegrityAPI(t *testing.T) {
	t.Helper()
	code, data, err := h.req("GET", "/api/integrity", nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/integrity: status=%d err=%v body=%s", code, err, data)
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
// TestConcurrentOrdersAndTopups – the main storm test.
// ---------------------------------------------------------------------------

// TestConcurrentOrdersAndTopups fires many concurrent orders and topups through
// the HTTP stack and verifies all safety invariants afterward.
//
// Design:
//   - 5 products, each with 30 units of stock and price 1 000 pesos (bar station)
//   - 5 attendees, each with 200 000 pesos initial balance
//   - 150 order goroutines, each buying 1 unit of a random product from a random attendee
//   - 40 topup goroutines, each crediting 500 pesos to a random attendee
//
// With only 150 stock units across 5 products and 150 goroutines, late goroutines
// will receive ErrInsufficientStock or ErrConflict (4xx) — exercising the OCC
// version guard. Balance is always plentiful so orders do not trigger the
// cash-confirmed path.
func TestConcurrentOrdersAndTopups(t *testing.T) {
	const (
		numProducts        = 5
		stockPerProduct    = 30
		pricePerProduct    = 1_000
		numAttendees       = 5
		initialBalance     = 200_000
		numOrderGoroutines = 150
		numTopupGoroutines = 40
		topupAmount        = 500
	)

	h := newHarness(t)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	evID := h.createEvent(t)
	cat := h.createCategory(t, "Bebidas")

	prods := make([]productResp, numProducts)
	for i := 0; i < numProducts; i++ {
		prods[i] = h.createProduct(t, cat,
			fmt.Sprintf("Prod%d", i), pricePerProduct, stockPerProduct, "bar")
	}

	atts := make([]attendeeResp, numAttendees)
	for i := 0; i < numAttendees; i++ {
		atts[i] = h.createAttendee(t, fmt.Sprintf("Att%d", i), initialBalance)
	}

	// Configuration complete; activate before the concurrency storm so orders
	// (which require an active event) are accepted.
	h.activate(t, evID)

	// ---- storm ----
	var (
		successOrders atomic.Int64
		failedOrders  atomic.Int64
		serverErrOrds atomic.Int64
		successTopups atomic.Int64
		failedTopups  atomic.Int64
		serverErrTops atomic.Int64
	)

	startGate := make(chan struct{})
	var wg sync.WaitGroup

	// Order goroutines.
	for i := 0; i < numOrderGoroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			<-startGate

			prod := prods[rng.Intn(numProducts)]
			att := atts[rng.Intn(numAttendees)]
			reqID := fmt.Sprintf("stress-ord-%d", seed)

			code, _ := h.mustReq("POST", "/api/orders", map[string]any{
				"request_id": reqID,
				"items":      []map[string]any{{"product_id": prod.ID, "qty": 1}},
				"payers":     []map[string]any{{"attendee_id": att.ID}},
			})
			switch {
			case code == http.StatusCreated || code == http.StatusOK:
				successOrders.Add(1)
			case code >= 400 && code < 500:
				failedOrders.Add(1)
			default:
				serverErrOrds.Add(1)
			}
		}(i)
	}

	// Topup goroutines.
	for i := 0; i < numTopupGoroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed + 100_000)))
			<-startGate

			att := atts[rng.Intn(numAttendees)]
			code, _ := h.mustReq("POST", "/api/attendees/"+att.ID+"/topup",
				map[string]any{"amount": topupAmount})
			switch {
			case code == http.StatusOK:
				successTopups.Add(1)
			case code >= 400 && code < 500:
				failedTopups.Add(1)
			default:
				serverErrTops.Add(1)
			}
		}(i)
	}

	start := time.Now()
	close(startGate) // release all goroutines simultaneously
	wg.Wait()
	elapsed := time.Since(start)

	total := successOrders.Load() + failedOrders.Load() +
		successTopups.Load() + failedTopups.Load()
	t.Logf("storm complete in %s: orders ok=%d 4xx=%d 5xx=%d | topups ok=%d 4xx=%d 5xx=%d | total ops=%d (%.1f ops/s)",
		elapsed.Round(time.Millisecond),
		successOrders.Load(), failedOrders.Load(), serverErrOrds.Load(),
		successTopups.Load(), failedTopups.Load(), serverErrTops.Load(),
		total, float64(total)/elapsed.Seconds())

	// Unexpected server errors are hard failures.
	if n := serverErrOrds.Load(); n > 0 {
		t.Errorf("%d order requests returned 5xx (unexpected server error)", n)
	}
	if n := serverErrTops.Load(); n > 0 {
		t.Errorf("%d topup requests returned 5xx (unexpected server error)", n)
	}

	// ---- invariants ----
	h.assertNoNegativeStock(t)
	h.assertNoNegativeBalance(t)
	h.assertLedgerMatchesBalance(t)
	h.assertStockMovementsMatchQty(t)
	h.assertIntegrityAPI(t)
}

// ---------------------------------------------------------------------------
// TestConcurrentIdempotency – same request_id from many goroutines.
// ---------------------------------------------------------------------------

// TestConcurrentIdempotency hammers a single request_id from many goroutines at
// once.  The serialized writer + idempotency check must ensure the attendee is
// charged exactly once and exactly one order row exists.
func TestConcurrentIdempotency(t *testing.T) {
	const (
		numGoroutines  = 60
		initialBalance = 50_000
		productPrice   = 2_000
		requestID      = "idem-stress-concurrent-1"
	)

	h := newHarness(t)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	evID := h.createEvent(t)
	cat := h.createCategory(t, "Bebidas")
	prod := h.createProduct(t, cat, "Cerveza", productPrice, 500, "bar")
	att := h.createAttendee(t, "IdemUser", initialBalance)
	h.activate(t, evID)

	body := map[string]any{
		"request_id": requestID,
		"items":      []map[string]any{{"product_id": prod.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": att.ID}},
	}

	type reqResult struct {
		status  int
		orderID string
	}

	results := make([]reqResult, numGoroutines)
	startGate := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startGate

			code, data := h.mustReq("POST", "/api/orders", body)
			var resp struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(data, &resp)
			results[idx] = reqResult{status: code, orderID: resp.ID}
		}(i)
	}

	close(startGate) // fire all goroutines at once
	wg.Wait()

	// All requests must succeed (200 idempotent replay or 201 first creation).
	for i, r := range results {
		if r.status != http.StatusOK && r.status != http.StatusCreated {
			t.Errorf("goroutine %d: unexpected status %d", i, r.status)
		}
	}

	// All responses must carry the same order ID.
	if len(results) == 0 {
		t.Fatal("no results")
	}
	firstID := results[0].orderID
	if firstID == "" {
		t.Fatal("first response has empty order ID")
	}
	for i, r := range results[1:] {
		if r.orderID != firstID {
			t.Errorf("goroutine %d returned different order ID: got %q want %q", i+1, r.orderID, firstID)
		}
	}

	// Balance charged exactly once.
	code, data, err := h.req("GET", "/api/attendees/"+att.ID, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET attendee: status=%d err=%v", code, err)
	}
	var attResp attendeeResp
	if err := json.Unmarshal(data, &attResp); err != nil {
		t.Fatalf("decode attendee: %v (body %s)", err, data)
	}
	wantBalance := int64(initialBalance - productPrice)
	if attResp.Balance != wantBalance {
		t.Errorf("balance after idempotent storm: got %d, want %d (charged %d times instead of 1)",
			attResp.Balance, wantBalance, (initialBalance-attResp.Balance)/productPrice)
	}

	// Exactly one order must exist.
	code2, data2, err2 := h.req("GET", "/api/orders", nil)
	if err2 != nil || code2 != http.StatusOK {
		t.Fatalf("GET /api/orders: status=%d err=%v", code2, err2)
	}
	var orders []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data2, &orders); err != nil {
		t.Fatalf("decode orders list: %v", err)
	}
	if len(orders) != 1 {
		t.Errorf("order count: got %d, want 1 (idempotency failed)", len(orders))
	}

	// Final invariants.
	h.assertNoNegativeBalance(t)
	h.assertLedgerMatchesBalance(t)
	h.assertIntegrityAPI(t)
}

// ---------------------------------------------------------------------------
// TestConcurrentTopupAndOrderRace – race between topup credit and order debit.
// ---------------------------------------------------------------------------

// TestConcurrentTopupAndOrderRace interleaves topups and orders for the same
// attendee, exercising the balance_version OCC guard under maximum contention.
// The invariants are the same as the main storm test.
func TestConcurrentTopupAndOrderRace(t *testing.T) {
	const (
		numPairs       = 60 // 60 topup + 60 order goroutines = 120 total
		productPrice   = 500
		topupAmount    = 600
		initialBalance = 100_000
	)

	h := newHarness(t)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	evID := h.createEvent(t)
	cat := h.createCategory(t, "Food")
	prod := h.createProduct(t, cat, "Taco", productPrice, 10_000, "cocina")
	att := h.createAttendee(t, "RaceUser", initialBalance)
	h.activate(t, evID)

	var (
		okOrders  atomic.Int64
		okTopups  atomic.Int64
		errOrders atomic.Int64
		errTopups atomic.Int64
	)

	startGate := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < numPairs; i++ {
		// order goroutine
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			<-startGate
			reqID := fmt.Sprintf("race-ord-%d", seed)
			code, _ := h.mustReq("POST", "/api/orders", map[string]any{
				"request_id": reqID,
				"items":      []map[string]any{{"product_id": prod.ID, "qty": 1}},
				"payers":     []map[string]any{{"attendee_id": att.ID}},
			})
			if code == http.StatusOK || code == http.StatusCreated {
				okOrders.Add(1)
			} else if code >= 500 {
				errOrders.Add(1)
			}
		}(i)

		// topup goroutine
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			<-startGate
			code, _ := h.mustReq("POST", "/api/attendees/"+att.ID+"/topup",
				map[string]any{"amount": topupAmount})
			if code == http.StatusOK {
				okTopups.Add(1)
			} else if code >= 500 {
				errTopups.Add(1)
			}
		}(i)
	}

	start := time.Now()
	close(startGate)
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("race test: %s | orders ok=%d | topups ok=%d | order 5xx=%d | topup 5xx=%d",
		elapsed.Round(time.Millisecond),
		okOrders.Load(), okTopups.Load(), errOrders.Load(), errTopups.Load())

	if n := errOrders.Load(); n > 0 {
		t.Errorf("%d order goroutines saw 5xx errors", n)
	}
	if n := errTopups.Load(); n > 0 {
		t.Errorf("%d topup goroutines saw 5xx errors", n)
	}

	h.assertNoNegativeBalance(t)
	h.assertLedgerMatchesBalance(t)
	h.assertNoNegativeStock(t)
	h.assertStockMovementsMatchQty(t)
	h.assertIntegrityAPI(t)
}

// ---------------------------------------------------------------------------
// TestStockBoundaryRace – many goroutines race for the very last unit.
// ---------------------------------------------------------------------------

// TestStockBoundaryRace creates one product with exactly N units and fires
// N+extra goroutines, each trying to buy 1 unit. Exactly N should succeed;
// the rest must fail gracefully (never going negative).
func TestStockBoundaryRace(t *testing.T) {
	const (
		stockUnits   = 20
		goroutines   = 50 // more than stock: some MUST fail
		productPrice = 100
		attBalance   = 1_000_000
	)

	h := newHarness(t)
	h.login(bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	evID := h.createEvent(t)
	cat := h.createCategory(t, "Limited")
	prod := h.createProduct(t, cat, "LastUnit", productPrice, stockUnits, "bar")
	att := h.createAttendee(t, "Buyer", attBalance)
	h.activate(t, evID)

	var (
		okCount   atomic.Int64
		failCount atomic.Int64
		srvErr    atomic.Int64
	)

	startGate := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			<-startGate
			reqID := fmt.Sprintf("stock-race-%d", seed)
			code, _ := h.mustReq("POST", "/api/orders", map[string]any{
				"request_id": reqID,
				"items":      []map[string]any{{"product_id": prod.ID, "qty": 1}},
				"payers":     []map[string]any{{"attendee_id": att.ID}},
			})
			switch {
			case code == http.StatusOK || code == http.StatusCreated:
				okCount.Add(1)
			case code >= 400 && code < 500:
				failCount.Add(1)
			default:
				srvErr.Add(1)
			}
		}(i)
	}

	close(startGate)
	wg.Wait()

	t.Logf("stock boundary: ok=%d fail(4xx)=%d 5xx=%d (stock was %d, goroutines %d)",
		okCount.Load(), failCount.Load(), srvErr.Load(), stockUnits, goroutines)

	if n := srvErr.Load(); n > 0 {
		t.Errorf("%d requests returned 5xx", n)
	}

	// Total ok orders must be <= stockUnits.
	if okCount.Load() > stockUnits {
		t.Errorf("more orders succeeded (%d) than available stock (%d)",
			okCount.Load(), stockUnits)
	}

	h.assertNoNegativeStock(t)
	h.assertLedgerMatchesBalance(t)
	h.assertStockMovementsMatchQty(t)
	h.assertIntegrityAPI(t)
}
