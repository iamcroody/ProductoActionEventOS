package reports

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eventpos/internal/core"
	"eventpos/internal/db"
)

// seedClosing builds an event with two operators, a set of paid orders with
// known created_at/paid_at deltas, products (one out of stock), and attendees
// (registered with leftover balance). It returns the eventID.
//
// Timing layout (deltas in seconds):
//   - fast operator "flash": 3 paid orders, deltas 10s, 20s, 30s -> avg 20s
//   - slow operator "turtle": 2 paid orders, deltas 100s, 200s -> avg 150s
//     (only 2 orders => does NOT qualify as fastest)
//   - "solo" operator: 1 paid order delta 5s (does not qualify, <3)
//
// Global avg over the 6 paid orders = (10+20+30+100+200+5)/6 = 365/6 ≈ 60.833s.
func seedClosing(t *testing.T, d *db.DB, eventID string) {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Writer.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	exec(`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
	      VALUES (?,?,?,?,?,?)`, eventID, "Cierre Fest", "active", 50000, "cascade", now)

	for _, u := range []struct{ id, name string }{
		{"u-flash", "flash"}, {"u-turtle", "turtle"}, {"u-solo", "solo"},
	} {
		exec(`INSERT INTO users(id, username, role, pin_hash, created_at)
		      VALUES (?,?,?,?,?)`, u.id, u.name, "caja", "h", now)
	}

	// Products: one tracked & out of stock, one tracked low-stock, one in stock.
	exec(`INSERT INTO products(id, event_id, name, price, cost, track_stock, stock_qty, created_at)
	      VALUES (?,?,?,?,?,?,?,?)`, "p-agotado", eventID, "Cerveza", 8000, 3000, 1, 0, now)
	exec(`INSERT INTO products(id, event_id, name, price, cost, track_stock, stock_qty, created_at)
	      VALUES (?,?,?,?,?,?,?,?)`, "p-low", eventID, "Vino", 12000, 5000, 1, 3, now)
	exec(`INSERT INTO products(id, event_id, name, price, cost, track_stock, stock_qty, created_at)
	      VALUES (?,?,?,?,?,?,?,?)`, "p-ok", eventID, "Agua", 2000, 500, 1, 50, now)

	base := int64(1_700_000_000_000) // fixed ms epoch
	const s = int64(1000)
	// orderID, operator, deltaSeconds, total
	orders := []struct {
		id    string
		op    string
		delta int64
		total int64
	}{
		{"o-f1", "u-flash", 10, 8000},
		{"o-f2", "u-flash", 20, 8000},
		{"o-f3", "u-flash", 30, 8000},
		{"o-t1", "u-turtle", 100, 12000},
		{"o-t2", "u-turtle", 200, 12000},
		{"o-s1", "u-solo", 5, 2000},
	}
	for i, o := range orders {
		created := base + int64(i)*s
		paid := created + o.delta*s
		exec(`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
		      paid_from_balance, paid_cash, created_at, paid_at)
		      VALUES (?,?,?,?,?,?,?,?,?,?)`,
			o.id, eventID, o.op, "paid", o.total, o.total, o.total, 0, created, paid)
	}
	// A voided order with a paid_at must be excluded from timing.
	exec(`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
	      paid_from_balance, paid_cash, created_at, paid_at)
	      VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"o-void", eventID, "u-flash", "voided", 9999, 9999, 0, 9999, base, base+9999*s)

	// Attendees: 3 registered; one active with balance, others with balance 0.
	exec(`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
	      VALUES (?,?,?,?,?,?,?)`, "a-1", eventID, "qr1", "Ana", 15000, 0, now)
	exec(`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
	      VALUES (?,?,?,?,?,?,?)`, "a-2", eventID, "qr2", "Beto", 0, 0, now)
	exec(`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, created_at)
	      VALUES (?,?,?,?,?,?,?)`, "a-3", eventID, "qr3", "Caro", 5000, 0, now)
}

func TestClosingMetrics(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const eventID = "evt-closing"
	seedClosing(t, d, eventID)

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	t.Run("avg_seconds_per_order", func(t *testing.T) {
		// (10+20+30+100+200+5)/6 = 60.8333...
		want := 365.0 / 6.0
		if diff := dash.AvgSecondsPerOrder - want; diff > 0.01 || diff < -0.01 {
			t.Errorf("avg_seconds_per_order: got %.4f, want %.4f", dash.AvgSecondsPerOrder, want)
		}
	})

	t.Run("fastest_operator", func(t *testing.T) {
		if dash.FastestOperator == nil {
			t.Fatal("fastest_operator: got nil, want flash")
		}
		if dash.FastestOperator.Username != "flash" {
			t.Errorf("fastest username: got %q, want flash", dash.FastestOperator.Username)
		}
		if dash.FastestOperator.OrderCount != 3 {
			t.Errorf("fastest order_count: got %d, want 3", dash.FastestOperator.OrderCount)
		}
		if diff := dash.FastestOperator.AvgSeconds - 20.0; diff > 0.01 || diff < -0.01 {
			t.Errorf("fastest avg_seconds: got %.4f, want 20", dash.FastestOperator.AvgSeconds)
		}
	})

	t.Run("registered_and_pending", func(t *testing.T) {
		if dash.RegisteredAttendees != 3 {
			t.Errorf("registered: got %d, want 3", dash.RegisteredAttendees)
		}
		if dash.PendingBalance != 20000 {
			t.Errorf("pending_balance: got %d, want 20000", dash.PendingBalance)
		}
	})

	t.Run("avg_consumption_per_client", func(t *testing.T) {
		// revenue = 8000*3 + 12000*2 + 2000 = 50000; /3 = 16666.67
		want := 50000.0 / 3.0
		if diff := dash.AvgConsumptionPerClient - want; diff > 0.01 || diff < -0.01 {
			t.Errorf("avg_consumption_per_client: got %.4f, want %.4f", dash.AvgConsumptionPerClient, want)
		}
	})

	t.Run("out_of_stock", func(t *testing.T) {
		if len(dash.OutOfStock) != 1 {
			t.Fatalf("out_of_stock len: got %d, want 1", len(dash.OutOfStock))
		}
		if dash.OutOfStock[0].Name != "Cerveza" {
			t.Errorf("out_of_stock[0]: got %q, want Cerveza", dash.OutOfStock[0].Name)
		}
	})

	t.Run("operator_avg_seconds", func(t *testing.T) {
		byUser := map[string]float64{}
		for _, o := range dash.SalesByOperator {
			byUser[o.Username] = o.AvgSeconds
		}
		if diff := byUser["flash"] - 20.0; diff > 0.01 || diff < -0.01 {
			t.Errorf("flash avg_seconds: got %.4f, want 20", byUser["flash"])
		}
		if diff := byUser["turtle"] - 150.0; diff > 0.01 || diff < -0.01 {
			t.Errorf("turtle avg_seconds: got %.4f, want 150", byUser["turtle"])
		}
	})
}

// TestFastestRequiresMinOrders verifies the 3-order qualification: with only
// one operator that has 2 paid orders, no fastest operator is reported.
func TestFastestRequiresMinOrders(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()
	const eventID = "evt-min"
	seedEventAndUser(t, d, eventID, "u-min", "active")

	base := int64(1_700_000_000_000)
	for i, delta := range []int64{10, 20} {
		created := base + int64(i)*1000
		if _, err := d.Writer.ExecContext(ctx,
			`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
			 paid_from_balance, paid_cash, created_at, paid_at)
			 VALUES (?,?,?,?,?,?,0,0,?,?)`,
			"om-"+string(rune('a'+i)), eventID, "u-min", "paid", 1000, 1000,
			created, created+delta*1000); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	_ = now

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}
	if dash.FastestOperator != nil {
		t.Errorf("fastest_operator: got %+v, want nil (only 2 orders)", dash.FastestOperator)
	}
}

// TestClosingResponseShape checks the consolidated builder maps every field.
func TestClosingResponseShape(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const eventID = "evt-shape"
	seedClosing(t, d, eventID)

	rep, err := queryClosing(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryClosing: %v", err)
	}
	if rep.Event.ID != eventID || rep.Event.Name != "Cierre Fest" || rep.Event.Status != "active" {
		t.Errorf("event header: got %+v", rep.Event)
	}
	if rep.Totals.Revenue != 50000 {
		t.Errorf("revenue: got %d, want 50000", rep.Totals.Revenue)
	}
	if rep.Totals.OrderCount != 6 {
		t.Errorf("order_count: got %d, want 6", rep.Totals.OrderCount)
	}
	if rep.Attendees.Registered != 3 {
		t.Errorf("registered: got %d, want 3", rep.Attendees.Registered)
	}
	if rep.Attendees.PendingBalance != 20000 {
		t.Errorf("pending_balance: got %d, want 20000", rep.Attendees.PendingBalance)
	}
	if rep.Timing.FastestOperator == nil || rep.Timing.FastestOperator.Username != "flash" {
		t.Errorf("fastest_operator: got %+v", rep.Timing.FastestOperator)
	}
	if len(rep.Operators) != 3 {
		t.Errorf("operators: got %d, want 3", len(rep.Operators))
	}
	if len(rep.Inventory.OutOfStock) != 1 {
		t.Errorf("out_of_stock: got %d, want 1", len(rep.Inventory.OutOfStock))
	}
	if len(rep.Inventory.LowStock) == 0 {
		t.Errorf("low_stock: got 0, want >=1")
	}
}

// TestClosingXLSXSmoke asserts the workbook endpoint returns a non-empty
// spreadsheet body with a 200.
func TestClosingXLSXSmoke(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-xlsx"
	seedClosing(t, d, eventID)

	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet, "/api/reports/closing.xlsx?event_id="+eventID, nil)
	rr := httptest.NewRecorder()
	m.closingXLSX(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("closing.xlsx: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "spreadsheet") {
		t.Errorf("content-type: got %q, want spreadsheet", ct)
	}
	body := rr.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("closing.xlsx: empty body")
	}
	// xlsx is a zip archive: must start with the PK signature.
	if !bytes.HasPrefix(body, []byte{0x50, 0x4B}) {
		t.Errorf("closing.xlsx: body does not look like an xlsx (zip) file")
	}
}

// TestClosingCSVSmoke asserts the consolidated CSV endpoint returns a BOM-led,
// sectioned CSV with a 200.
func TestClosingCSVSmoke(t *testing.T) {
	d := openTestDB(t)
	const eventID = "evt-csv2"
	seedClosing(t, d, eventID)

	m := newReportsMod(t, d)
	req := httptest.NewRequest(http.MethodGet, "/api/reports/closing.csv?event_id="+eventID, nil)
	rr := httptest.NewRecorder()
	m.closingCSV(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("closing.csv: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "csv") {
		t.Errorf("content-type: got %q, want csv", ct)
	}
	body := rr.Body.Bytes()
	if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Errorf("closing.csv: missing UTF-8 BOM")
	}
	// Body after BOM must be parseable CSV and include the section headers.
	r := csv.NewReader(strings.NewReader(string(body[3:])))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("parse closing.csv: %v", err)
	}
	joined := rr.Body.String()
	for _, want := range []string{"CIERRE DE EVENTO", "INDICADORES", "POR VENDEDOR", "TOP PRODUCTOS", "PRODUCTOS AGOTADOS"} {
		if !strings.Contains(joined, want) {
			t.Errorf("closing.csv missing section %q", want)
		}
	}
	if len(records) < 10 {
		t.Errorf("closing.csv: too few rows (%d)", len(records))
	}
}

// TestClosingResolverMostRecent verifies the active-or-most-recent fallback.
func TestClosingResolverMostRecent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()

	// Two closed events; the most-recently created should win.
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`, "ev-old", "Old", "closed", 50000, "cascade", now-1000); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`, "ev-new", "New", "closed", 50000, "cascade", now); err != nil {
		t.Fatalf("insert new: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/reports/closing", nil)
	id, err := resolveClosingEventID(ctx, req, d.Reader)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "ev-new" {
		t.Errorf("most-recent fallback: got %q, want ev-new", id)
	}

	// An active event must outrank a newer closed one.
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`, "ev-active", "Active", "active", 50000, "cascade", now-5000); err != nil {
		t.Fatalf("insert active: %v", err)
	}
	id, err = resolveClosingEventID(ctx, req, d.Reader)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if id != "ev-active" {
		t.Errorf("active priority: got %q, want ev-active", id)
	}
}
