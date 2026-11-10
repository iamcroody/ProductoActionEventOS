package reports

import (
	"context"
	"path/filepath"
	"testing"

	"eventpos/internal/core"
	"eventpos/internal/db"
)

// openTestDB spins up a real on-disk SQLite (WAL) database in a temp directory
// with the full production schema applied. The database is closed automatically
// when the test ends.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestDashboardVoidedExcluded inserts two paid orders and one voided order,
// then asserts that:
//   - total_sold counts only the two paid orders (18 000 pesos).
//   - order_count is 2, not 3.
//   - top_products reflects only items from the paid orders; Beer (qty 2)
//     ranks above Wine (qty 1); the voided Beer unit is excluded.
//   - low_stock surfaces Wine (stock_qty = 3, ≤ 5) but not Beer (stock_qty = 10).
func TestDashboardVoidedExcluded(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := core.NowMillis()

	const (
		eventID   = "evt-001"
		userID    = "usr-001"
		prod1ID   = "prod-001" // Beer, price 5000, stock 10
		prod2ID   = "prod-002" // Wine, price 8000, stock 3
		ord1ID    = "ord-001"  // paid, 2 × Beer = 10 000
		ord2ID    = "ord-002"  // paid, 1 × Wine = 8 000
		ordVoidID = "ord-void" // voided, 1 × Beer = 5 000 — must not appear in metrics
	)

	// --- seed event, user, products ---

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Test Event", "active", 50000, "cascade", now); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, role, pin_hash, created_at)
		 VALUES (?,?,?,?,?)`,
		userID, "testop", "admin", "argon2id$salt$key", now); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		prod1ID, eventID, "Beer", 5000, 1, 10, now); err != nil {
		t.Fatalf("insert product Beer: %v", err)
	}

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, track_stock, stock_qty, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		prod2ID, eventID, "Wine", 8000, 1, 3, now); err != nil {
		t.Fatalf("insert product Wine: %v", err)
	}

	// --- seed orders ---

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
		                    paid_from_balance, paid_cash, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		ord1ID, eventID, userID, "paid", 10000, 10000, 0, 10000, now); err != nil {
		t.Fatalf("insert order 1: %v", err)
	}

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
		                    paid_from_balance, paid_cash, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		ord2ID, eventID, userID, "paid", 8000, 8000, 8000, 0, now); err != nil {
		t.Fatalf("insert order 2: %v", err)
	}

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO orders(id, event_id, operator_id, status, subtotal, total,
		                    paid_from_balance, paid_cash, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		ordVoidID, eventID, userID, "voided", 5000, 5000, 0, 5000, now); err != nil {
		t.Fatalf("insert voided order: %v", err)
	}

	// --- seed order items ---

	// 2 × Beer on ord1
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO order_items(id, order_id, product_id, name, qty, unit_price, line_total)
		 VALUES (?,?,?,?,?,?,?)`,
		"item-001", ord1ID, prod1ID, "Beer", 2, 5000, 10000); err != nil {
		t.Fatalf("insert item 1: %v", err)
	}

	// 1 × Wine on ord2
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO order_items(id, order_id, product_id, name, qty, unit_price, line_total)
		 VALUES (?,?,?,?,?,?,?)`,
		"item-002", ord2ID, prod2ID, "Wine", 1, 8000, 8000); err != nil {
		t.Fatalf("insert item 2: %v", err)
	}

	// 1 × Beer on the voided order — must not contribute to top_products
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO order_items(id, order_id, product_id, name, qty, unit_price, line_total)
		 VALUES (?,?,?,?,?,?,?)`,
		"item-void", ordVoidID, prod1ID, "Beer", 1, 5000, 5000); err != nil {
		t.Fatalf("insert voided item: %v", err)
	}

	// --- exercise ---

	dash, err := queryDashboard(ctx, d.Reader, eventID)
	if err != nil {
		t.Fatalf("queryDashboard: %v", err)
	}

	// total_sold must exclude the voided order (5 000 pesos).
	if want := int64(18000); dash.TotalSold != want {
		t.Errorf("total_sold: got %d, want %d", dash.TotalSold, want)
	}

	// order_count must be 2 (not 3).
	if want := int64(2); dash.OrderCount != want {
		t.Errorf("order_count: got %d, want %d", dash.OrderCount, want)
	}

	// top_products: two distinct products, Beer first (qty 2) then Wine (qty 1).
	if got := len(dash.TopProducts); got != 2 {
		t.Fatalf("top_products len: got %d, want 2", got)
	}
	if dash.TopProducts[0].ProductID != prod1ID {
		t.Errorf("top[0].product_id: got %q, want %q", dash.TopProducts[0].ProductID, prod1ID)
	}
	if dash.TopProducts[0].QtySold != 2 {
		t.Errorf("top[0].qty_sold: got %d, want 2", dash.TopProducts[0].QtySold)
	}
	if dash.TopProducts[1].ProductID != prod2ID {
		t.Errorf("top[1].product_id: got %q, want %q", dash.TopProducts[1].ProductID, prod2ID)
	}
	if dash.TopProducts[1].QtySold != 1 {
		t.Errorf("top[1].qty_sold: got %d, want 1", dash.TopProducts[1].QtySold)
	}

	// low_stock: Wine (stock 3, ≤ 5) must appear; Beer (stock 10) must not.
	if got := len(dash.LowStock); got != 1 {
		t.Fatalf("low_stock len: got %d, want 1", got)
	}
	if dash.LowStock[0].ProductID != prod2ID {
		t.Errorf("low_stock[0].product_id: got %q, want %q", dash.LowStock[0].ProductID, prod2ID)
	}
	if dash.LowStock[0].StockQty != 3 {
		t.Errorf("low_stock[0].stock_qty: got %d, want 3", dash.LowStock[0].StockQty)
	}
}
