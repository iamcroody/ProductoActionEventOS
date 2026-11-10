package reports

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/server"
)

// newReportsMod creates a minimal Module wired to d.
// Reports are read-only so no writer is needed in the module itself.
func newReportsMod(t *testing.T, d *db.DB) *Module {
	t.Helper()
	return New(server.Deps{App: &app.App{DB: d}})
}

// parseCSVData parses a full CSV body into a slice of rows (including header).
func parseCSVData(t *testing.T, body string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(body))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	return rows
}

// seedEventAndUser inserts a minimal event + operator user and returns their IDs.
func seedEventAndUser(t *testing.T, d *db.DB, eventID, userID, status string) {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Test Event", status, 50000, "cascade", now); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, role, pin_hash, created_at)
		 VALUES (?,?,?,?,?)`,
		userID, "op-"+userID, "caja", "argon2id$salt$key", now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

// insertOrder inserts a minimal order at the given unix-ms timestamp with the
// given status and total, returning its ID.
func insertOrder(t *testing.T, d *db.DB, eventID, userID, orderID, status string, total, createdAt int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
		                    paid_from_balance, paid_cash, created_at)
		 VALUES (?,?,?,?,?,?,0,?,?)`,
		orderID, eventID, userID, status, total, total, total, createdAt); err != nil {
		t.Fatalf("insert order %s: %v", orderID, err)
	}
}

// ---------------------------------------------------------------------------
// sales_by_hour grouping
// ---------------------------------------------------------------------------

// TestSalesByHourBuckets seeds two orders placed exactly 2 hours apart and
// asserts they land in two distinct hour buckets with correct totals.
func TestSalesByHourBuckets(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	const (
		eventID = "evt-hour-001"
		userID  = "usr-hour-001"
		ord1ID  = "ord-hour-001"
		ord2ID  = "ord-hour-002"
	)

	seedEventAndUser(t, d, eventID, userID, "active")

	// Two timestamps exactly 2 hours apart (always in different local-time hours).
	t1 := int64(3_600_000)  // 01:00 UTC Jan 1 1970
	t2 := int64(10_800_000) // 03:00 UTC Jan 1 1970

	insertOrder(t, d, eventID, userID, ord1ID, "paid", 5000, t1)
	insertOrder(t, d, eventID, userID, ord2ID, "paid", 8000, t2)

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	if len(dash.SalesByHour) != 2 {
		t.Fatalf("sales_by_hour: got %d buckets, want 2; buckets=%v", len(dash.SalesByHour), dash.SalesByHour)
	}

	// Each bucket must have exactly 1 order.
	for i, b := range dash.SalesByHour {
		if b.Count != 1 {
			t.Errorf("bucket[%d] count: got %d, want 1", i, b.Count)
		}
	}

	// Combined total must equal the sum of both orders.
	var totalSum int64
	for _, b := range dash.SalesByHour {
		totalSum += b.Total
	}
	if totalSum != 13000 {
		t.Errorf("bucket total sum: got %d, want 13000", totalSum)
	}

	// Buckets must be ordered by hour (ascending).
	if dash.SalesByHour[0].Hour >= dash.SalesByHour[1].Hour {
		t.Errorf("buckets not ordered: %q >= %q", dash.SalesByHour[0].Hour, dash.SalesByHour[1].Hour)
	}
}

// TestSalesByHourVoidedExcluded asserts that a voided order does not appear
// in any hour bucket.
func TestSalesByHourVoidedExcluded(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	const (
		eventID = "evt-hour-v01"
		userID  = "usr-hour-v01"
	)
	seedEventAndUser(t, d, eventID, userID, "active")

	ts := int64(3_600_000)
	insertOrder(t, d, eventID, userID, "ord-hv-paid", "paid", 5000, ts)
	insertOrder(t, d, eventID, userID, "ord-hv-void", "voided", 9000, ts)

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	// Only the paid order should appear (1 bucket, total=5000, count=1).
	if len(dash.SalesByHour) != 1 {
		t.Fatalf("hour buckets: got %d, want 1", len(dash.SalesByHour))
	}
	if dash.SalesByHour[0].Total != 5000 {
		t.Errorf("bucket total: got %d, want 5000", dash.SalesByHour[0].Total)
	}
	if dash.SalesByHour[0].Count != 1 {
		t.Errorf("bucket count: got %d, want 1", dash.SalesByHour[0].Count)
	}
}

// ---------------------------------------------------------------------------
// sales_by_operator aggregation
// ---------------------------------------------------------------------------

// TestSalesByOperatorAggregation seeds two operators each with different order
// totals and verifies the operator buckets aggregate correctly.
func TestSalesByOperatorAggregation(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()

	const eventID = "evt-op-001"

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Op Event", "active", 50000, "cascade", now); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Two operators.
	for _, u := range []struct{ id, name string }{
		{"usr-op-a", "alice"},
		{"usr-op-b", "bob"},
	} {
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO users(id, username, role, pin_hash, created_at)
			 VALUES (?,?,?,?,?)`,
			u.id, u.name, "caja", "argon2id$s$k", now); err != nil {
			t.Fatalf("insert user %s: %v", u.id, err)
		}
	}

	// Alice: two orders → total 15000
	insertOrder(t, d, eventID, "usr-op-a", "ord-a1", "paid", 10000, now)
	insertOrder(t, d, eventID, "usr-op-a", "ord-a2", "paid", 5000, now)
	// Bob: one order → total 8000
	insertOrder(t, d, eventID, "usr-op-b", "ord-b1", "paid", 8000, now)
	// Voided order for Alice — must NOT count.
	insertOrder(t, d, eventID, "usr-op-a", "ord-a-void", "voided", 99000, now)

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	if len(dash.SalesByOperator) != 2 {
		t.Fatalf("operator buckets: got %d, want 2", len(dash.SalesByOperator))
	}

	// Results are ordered by total DESC, so Alice (15000) should be first.
	got := map[string]int64{}
	for _, b := range dash.SalesByOperator {
		got[b.OperatorID] = b.Total
	}
	if got["usr-op-a"] != 15000 {
		t.Errorf("alice total: got %d, want 15000", got["usr-op-a"])
	}
	if got["usr-op-b"] != 8000 {
		t.Errorf("bob total: got %d, want 8000", got["usr-op-b"])
	}
}

// ---------------------------------------------------------------------------
// Voided orders excluded from all metrics
// ---------------------------------------------------------------------------

// TestVoidedExcludedFromAllAggregations seeds paid and voided orders and
// asserts voided orders are excluded from total_sold, top_products,
// sales_by_hour, and sales_by_operator simultaneously.
func TestVoidedExcludedFromAllAggregations(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()

	const (
		eventID = "evt-void-all"
		userID  = "usr-void-all"
		prodID  = "prd-void-all"
	)

	seedEventAndUser(t, d, eventID, userID, "active")

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		prodID, eventID, "Widget", 1000, 0, 0, now); err != nil {
		t.Fatalf("insert product: %v", err)
	}

	// One paid order.
	insertOrder(t, d, eventID, userID, "ord-va-paid", "paid", 3000, now)
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO order_items(id, order_id, product_id, name, qty, unit_price, line_total)
		 VALUES (?,?,?,?,?,?,?)`,
		"item-va-paid", "ord-va-paid", prodID, "Widget", 3, 1000, 3000); err != nil {
		t.Fatalf("insert paid item: %v", err)
	}

	// One voided order (same hour, same operator).
	insertOrder(t, d, eventID, userID, "ord-va-void", "voided", 9999, now)
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO order_items(id, order_id, product_id, name, qty, unit_price, line_total)
		 VALUES (?,?,?,?,?,?,?)`,
		"item-va-void", "ord-va-void", prodID, "Widget", 99, 1000, 9999); err != nil {
		t.Fatalf("insert voided item: %v", err)
	}

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	if dash.TotalSold != 3000 {
		t.Errorf("total_sold: got %d, want 3000", dash.TotalSold)
	}
	if dash.OrderCount != 1 {
		t.Errorf("order_count: got %d, want 1", dash.OrderCount)
	}

	// top_products: only 3 units (from paid order)
	if len(dash.TopProducts) != 1 {
		t.Fatalf("top_products len: got %d, want 1", len(dash.TopProducts))
	}
	if dash.TopProducts[0].QtySold != 3 {
		t.Errorf("top[0].qty_sold: got %d, want 3", dash.TopProducts[0].QtySold)
	}

	// sales_by_hour: only the paid order counts
	var hourTotal int64
	for _, b := range dash.SalesByHour {
		hourTotal += b.Total
	}
	if hourTotal != 3000 {
		t.Errorf("hour bucket total sum: got %d, want 3000", hourTotal)
	}

	// sales_by_operator: only the paid order counts
	if len(dash.SalesByOperator) != 1 {
		t.Fatalf("operator buckets: got %d, want 1", len(dash.SalesByOperator))
	}
	if dash.SalesByOperator[0].Total != 3000 {
		t.Errorf("operator total: got %d, want 3000", dash.SalesByOperator[0].Total)
	}
}

// ---------------------------------------------------------------------------
// export.csv
// ---------------------------------------------------------------------------

// seedFullExportData sets up an event with a product, attendee, ledger entry,
// and a paid order so that all export reports have at least one data row.
func seedFullExportData(t *testing.T, d *db.DB, eventID string) {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()

	seedEventAndUser(t, d, eventID, "usr-exp-"+eventID, "closed")

	prodID := "prd-exp-" + eventID
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		prodID, eventID, "Export Product", 2000, 0, 0, now); err != nil {
		t.Fatalf("insert export product: %v", err)
	}

	attendeeID := "att-exp-" + eventID
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		attendeeID, eventID, "qr-"+core.NewID(), "Export Attendee", 100, 0, now); err != nil {
		t.Fatalf("insert attendee: %v", err)
	}

	// Ledger entry for the attendee (no ref_payment_id needed; chain_hash nullable).
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO balance_ledger(id, attendee_id, type, amount, balance_after, user_id, chain_hash, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		core.NewID(), attendeeID, "topup", 100, 100, "usr-exp-"+eventID, "testhash", now); err != nil {
		t.Fatalf("insert ledger: %v", err)
	}
}

func TestExportCSVProducts(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-csv-prod"
	seedFullExportData(t, d, eventID)

	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet,
		"/api/reports/export.csv?report=products&event_id="+eventID, nil)
	rr := httptest.NewRecorder()
	m.exportCSV(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("products export: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	rows := parseCSVData(t, rr.Body.String())
	if len(rows) < 2 {
		t.Fatalf("products export: expected header + ≥1 data row, got %d rows", len(rows))
	}
	if rows[0][0] != "id" {
		t.Errorf("products header[0]: got %q, want 'id'", rows[0][0])
	}
	if rows[0][1] != "name" {
		t.Errorf("products header[1]: got %q, want 'name'", rows[0][1])
	}
}

func TestExportCSVAttendees(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-csv-att"
	seedFullExportData(t, d, eventID)

	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet,
		"/api/reports/export.csv?report=attendees&event_id="+eventID, nil)
	rr := httptest.NewRecorder()
	m.exportCSV(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("attendees export: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	rows := parseCSVData(t, rr.Body.String())
	if len(rows) < 2 {
		t.Fatalf("attendees export: expected header + ≥1 data row, got %d rows", len(rows))
	}
	if rows[0][0] != "id" {
		t.Errorf("attendees header[0]: got %q, want 'id'", rows[0][0])
	}
}

func TestExportCSVLedger(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-csv-ldg"
	seedFullExportData(t, d, eventID)

	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet,
		"/api/reports/export.csv?report=ledger&event_id="+eventID, nil)
	rr := httptest.NewRecorder()
	m.exportCSV(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("ledger export: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	rows := parseCSVData(t, rr.Body.String())
	if len(rows) < 2 {
		t.Fatalf("ledger export: expected header + ≥1 data row, got %d rows", len(rows))
	}
	if rows[0][0] != "id" {
		t.Errorf("ledger header[0]: got %q, want 'id'", rows[0][0])
	}
}

func TestExportCSVUnknownReport(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-csv-unk"
	seedFullExportData(t, d, eventID)

	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet,
		"/api/reports/export.csv?report=bogus&event_id="+eventID, nil)
	rr := httptest.NewRecorder()
	m.exportCSV(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown report: got %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ?event_id= scoping to a specific (closed) event
// ---------------------------------------------------------------------------

// TestEventIDFilterScopesToEvent verifies that ?event_id= queries a specific
// closed event and does not bleed results from a different event.
func TestEventIDFilterScopesToEvent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()

	// Event A (closed) with 1 order totalling 7000.
	const (
		eventA = "evt-scope-a"
		eventB = "evt-scope-b"
		userA  = "usr-scope-a"
		userB  = "usr-scope-b"
	)

	for _, ev := range []struct{ id, uid string }{{eventA, userA}, {eventB, userB}} {
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
			 VALUES (?,?,?,?,?,?)`,
			ev.id, "Event "+ev.id, "closed", 50000, "cascade", now); err != nil {
			t.Fatalf("insert event %s: %v", ev.id, err)
		}
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO users(id, username, role, pin_hash, created_at)
			 VALUES (?,?,?,?,?)`,
			ev.uid, "op-"+ev.uid, "caja", "argon2id$s$k", now); err != nil {
			t.Fatalf("insert user %s: %v", ev.uid, err)
		}
	}

	insertOrder(t, d, eventA, userA, "ord-scope-a1", "paid", 7000, now)
	insertOrder(t, d, eventB, userB, "ord-scope-b1", "paid", 99000, now)

	// Query dashboard scoped to eventA.
	dash, err := queryDashboard(ctx, d.Reader, eventA)
	if err != nil {
		t.Fatalf("queryDashboard(eventA): %v", err)
	}
	if dash.EventID != eventA {
		t.Errorf("event_id: got %q, want %q", dash.EventID, eventA)
	}
	if dash.TotalSold != 7000 {
		t.Errorf("total_sold: got %d, want 7000 (eventB's 99000 must not bleed through)", dash.TotalSold)
	}
	if dash.OrderCount != 1 {
		t.Errorf("order_count: got %d, want 1", dash.OrderCount)
	}

	// Same via HTTP using ?event_id=.
	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet,
		"/api/reports/dashboard?event_id="+eventA, nil)
	rr := httptest.NewRecorder()
	m.dashboard(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard HTTP: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}
