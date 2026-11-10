package catalog

// catalog_extra_test.go — deeper edge-case and concurrency tests.
//
// Helpers openTestDB, buildDeps, seedSession, and authedReq are declared in
// catalog_test.go and reused here because both files belong to the same
// package (package catalog).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"eventpos/internal/core"
	"eventpos/internal/writer"
)

// TestInventoryAdjustConcurrency launches more goroutines than there is
// available stock. Because every adjust call is serialised through the single
// writer, exactly initialStock decrements should succeed, final stock must be
// zero (never negative), and the movement rows must match the successful ops.
func TestInventoryAdjustConcurrency(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()

	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	ctx := context.Background()
	now := core.NowMillis()

	// Seed active event.
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Concurrent Event", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	rawToken := seedSession(t, d, eventID)

	// Seed a tracked product with 20 units of stock.
	const initialStock = int64(20)
	productID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		productID, eventID, "BeerConcurrent", 5000, "bar",
		1, initialStock, 0, 0, now); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	// Launch 50 goroutines — 30 more than the available stock — each trying
	// to remove 1 unit.
	const goroutines = 50
	codes := make([]int, goroutines)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		i := i // capture loop variable by value
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"product_id":%q,"delta":-1,"reason":"waste"}`, productID)
			req := authedReq(http.MethodPost, "/api/inventory/adjust", body, rawToken)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			codes[i] = rr.Code
		}()
	}
	wg.Wait()

	// Count successes (200) and failures (400 ErrInsufficientStock).
	var successes, failures int
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusBadRequest:
			failures++
		default:
			t.Errorf("unexpected HTTP status: %d", code)
		}
	}
	if int64(successes) != initialStock {
		t.Fatalf("expected exactly %d successful decrements, got %d (failures=%d)",
			initialStock, successes, failures)
	}

	// Final stock must be 0 — never negative.
	var finalStock int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT stock_qty FROM products WHERE id=?`, productID).Scan(&finalStock); err != nil {
		t.Fatalf("read final stock: %v", err)
	}
	if finalStock != 0 {
		t.Fatalf("stock did not drain to zero: expected 0, got %d", finalStock)
	}

	// Movements are the source of truth: waste count and sum must match.
	var movCount int
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND reason='waste'`,
		productID).Scan(&movCount); err != nil {
		t.Fatalf("read movement count: %v", err)
	}
	if int64(movCount) != initialStock {
		t.Fatalf("waste movement count: expected %d, got %d", initialStock, movCount)
	}

	var movSum int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(delta),0) FROM stock_movements WHERE product_id=?`,
		productID).Scan(&movSum); err != nil {
		t.Fatalf("read movement sum: %v", err)
	}
	if movSum != -initialStock {
		t.Fatalf("movement sum: expected %d, got %d", -initialStock, movSum)
	}
}

// TestCreateProductInitialStockMovement verifies that POST /api/products with
// track_stock=1 and stock_qty>0 inserts exactly one 'initial' stock_movements
// row whose delta and balance_after equal stock_qty. When stock_qty==0 or
// track_stock==0, no movement is created.
func TestCreateProductInitialStockMovement(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()

	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	ctx := context.Background()
	now := core.NowMillis()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Stock Movement Event", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	rawToken := seedSession(t, d, eventID)

	// --- Case 1: track_stock=1, stock_qty=25 → one 'initial' movement ---
	req1 := authedReq(http.MethodPost, "/api/products",
		`{"name":"Agua","price":2000,"prep_station":"none","track_stock":1,"stock_qty":25}`,
		rawToken)
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("create product (tracked, stock=25): got %d: %s", rr1.Code, rr1.Body.String())
	}
	var p1 Product
	if err := json.NewDecoder(rr1.Body).Decode(&p1); err != nil {
		t.Fatalf("decode product1: %v", err)
	}

	var movDelta, movBalance int64
	var movReason string
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT delta, reason, balance_after FROM stock_movements WHERE product_id=?`,
		p1.ID).Scan(&movDelta, &movReason, &movBalance); err != nil {
		t.Fatalf("initial movement not found for product %s: %v", p1.ID, err)
	}
	if movDelta != 25 {
		t.Errorf("initial movement delta: got %d, want 25", movDelta)
	}
	if movReason != "initial" {
		t.Errorf("initial movement reason: got %q, want \"initial\"", movReason)
	}
	if movBalance != 25 {
		t.Errorf("initial movement balance_after: got %d, want 25", movBalance)
	}

	// --- Case 2: track_stock=1, stock_qty=0 → no movement ---
	req2 := authedReq(http.MethodPost, "/api/products",
		`{"name":"ZeroStock","price":1000,"prep_station":"none","track_stock":1,"stock_qty":0}`,
		rawToken)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("create product (tracked, stock=0): got %d: %s", rr2.Code, rr2.Body.String())
	}
	var p2 Product
	if err := json.NewDecoder(rr2.Body).Decode(&p2); err != nil {
		t.Fatalf("decode product2: %v", err)
	}
	var count2 int
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stock_movements WHERE product_id=?`, p2.ID).Scan(&count2); err != nil {
		t.Fatalf("count movements (zero stock): %v", err)
	}
	if count2 != 0 {
		t.Errorf("zero-stock product: expected 0 movements, got %d", count2)
	}

	// --- Case 3: track_stock=0, stock_qty=10 → no movement even when qty>0 ---
	req3 := authedReq(http.MethodPost, "/api/products",
		`{"name":"Untracked","price":500,"prep_station":"none","track_stock":0,"stock_qty":10}`,
		rawToken)
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("create product (untracked): got %d: %s", rr3.Code, rr3.Body.String())
	}
	var p3 Product
	if err := json.NewDecoder(rr3.Body).Decode(&p3); err != nil {
		t.Fatalf("decode product3: %v", err)
	}
	var count3 int
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stock_movements WHERE product_id=?`, p3.ID).Scan(&count3); err != nil {
		t.Fatalf("count movements (untracked): %v", err)
	}
	if count3 != 0 {
		t.Errorf("untracked product: expected 0 movements, got %d", count3)
	}
}

// TestLowStockThresholdBoundary verifies the <= boundary of the low-stock
// query: stock == threshold is included; stock > threshold is excluded.
func TestLowStockThresholdBoundary(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()

	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	ctx := context.Background()
	now := core.NowMillis()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Low Stock Event", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	rawToken := seedSession(t, d, eventID)

	type stockCase struct {
		name  string
		qty   int64
		inLow bool // should appear when threshold=5
	}
	cases := []stockCase{
		{"StockZero", 0, true},
		{"StockBelow", 3, true},
		{"StockAtThreshold", 5, true},     // stock == threshold: included (<=)
		{"StockAboveThreshold", 6, false}, // stock > threshold: excluded
		{"StockHighAbove", 100, false},
	}

	productIDs := make(map[string]string) // name → id
	for _, c := range cases {
		pid := core.NewID()
		productIDs[c.name] = pid
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO products(id, event_id, name, price, prep_station,
			  track_stock, stock_qty, stock_version, sort_order, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			pid, eventID, c.name, 1000, "none", 1, c.qty, 0, 0, now); err != nil {
			t.Fatalf("seed product %s: %v", c.name, err)
		}
	}

	req := authedReq(http.MethodGet, "/api/inventory/low-stock?threshold=5", "", rawToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("low-stock: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var products []Product
	if err := json.NewDecoder(rr.Body).Decode(&products); err != nil {
		t.Fatalf("decode low-stock response: %v", err)
	}

	returned := make(map[string]bool)
	for _, p := range products {
		returned[p.ID] = true
	}

	for _, c := range cases {
		pid := productIDs[c.name]
		if c.inLow && !returned[pid] {
			t.Errorf("product %q (stock=%d) should appear in low-stock list (threshold=5)",
				c.name, c.qty)
		}
		if !c.inLow && returned[pid] {
			t.Errorf("product %q (stock=%d) should NOT appear in low-stock list (threshold=5)",
				c.name, c.qty)
		}
	}
}

// TestProductValidationNegativePrice verifies that POST /api/products rejects a
// negative price with HTTP 400 (ErrValidation).
func TestProductValidationNegativePrice(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()

	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	ctx := context.Background()
	now := core.NowMillis()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Validation Event", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	rawToken := seedSession(t, d, eventID)

	req := authedReq(http.MethodPost, "/api/products",
		`{"name":"BadPriceProduct","price":-1,"prep_station":"none"}`, rawToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("negative price: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestUpdateProductDoesNotAlterStockQty verifies that PUT /api/products/{id}
// does not touch stock_qty — stock changes must go through the inventory-adjust
// endpoint. The updateProduct handler intentionally excludes stock_qty from its
// request struct and its UPDATE SQL.
func TestUpdateProductDoesNotAlterStockQty(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()

	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	ctx := context.Background()
	now := core.NowMillis()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Update Test Event", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	rawToken := seedSession(t, d, eventID)

	// Seed a product with stock_qty=15 directly.
	const initialStock = int64(15)
	productID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		productID, eventID, "UpdateMe", 3000, "none", 1, initialStock, 0, 0, now); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	// PUT to change name and price — stock_qty is intentionally absent from body.
	req := authedReq(http.MethodPut, "/api/products/"+productID,
		`{"name":"UpdatedName","price":4000,"prep_station":"none","track_stock":1,"active":1}`,
		rawToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update product: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// stock_qty must be unchanged.
	var stockQty int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT stock_qty FROM products WHERE id=?`, productID).Scan(&stockQty); err != nil {
		t.Fatalf("read stock_qty after PUT: %v", err)
	}
	if stockQty != initialStock {
		t.Fatalf("PUT altered stock_qty: expected %d, got %d", initialStock, stockQty)
	}

	// Name and price must reflect the update.
	var name string
	var price int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT name, price FROM products WHERE id=?`, productID).Scan(&name, &price); err != nil {
		t.Fatalf("read name/price after PUT: %v", err)
	}
	if name != "UpdatedName" {
		t.Errorf("name after PUT: got %q, want %q", name, "UpdatedName")
	}
	if price != 4000 {
		t.Errorf("price after PUT: got %d, want 4000", price)
	}
}
