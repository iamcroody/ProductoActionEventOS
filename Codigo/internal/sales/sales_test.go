package sales

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Test harness: real on-disk SQLite + real Writer, seeded with an active event,
// products, attendees and a user — exactly the production write path.
// ---------------------------------------------------------------------------

type harness struct {
	db   *db.DB
	w    *writer.Writer
	user auth.User
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	w := writer.New(d.Writer)
	t.Cleanup(func() {
		w.Close()
		d.Close()
	})
	h := &harness{db: d, w: w, user: auth.User{ID: "user-1", Username: "caja1", FullName: "Caja Uno", Role: "caja"}}

	now := core.NowMillis()
	_, err = w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		if _, err := tx.Exec(`INSERT INTO users(id,username,role,pin_hash,active,created_at) VALUES (?,?,?,?,1,?)`,
			h.user.ID, h.user.Username, h.user.Role, "x", now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO events(id,name,status,default_topup,split_policy,created_at)
			VALUES ('ev1','Test',  'active', 50000, 'cascade', ?)`, now); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed base: %v", err)
	}
	return h
}

// seedProduct inserts a product in the active event.
func (h *harness) seedProduct(t *testing.T, id, name string, price int64, station string, track int, stock int64) {
	t.Helper()
	_, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return tx.Exec(`INSERT INTO products(id,event_id,name,price,prep_station,track_stock,stock_qty,stock_version,active,created_at)
			VALUES (?,?,?,?,?,?,?,0,1,?)`, id, "ev1", name, price, station, track, stock, core.NowMillis())
	})
	if err != nil {
		t.Fatalf("seed product %s: %v", id, err)
	}
}

// seedAttendee inserts an attendee with a starting balance.
func (h *harness) seedAttendee(t *testing.T, id, qr string, balance int64) {
	t.Helper()
	_, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return tx.Exec(`INSERT INTO attendees(id,event_id,qr_token,display_name,balance,balance_version,status,created_at)
			VALUES (?,?,?,?,?,0,'active',?)`, id, "ev1", qr, id, balance, core.NowMillis())
	})
	if err != nil {
		t.Fatalf("seed attendee %s: %v", id, err)
	}
}

// sell runs a sale through the real writer path and returns the result/err.
func (h *harness) sell(in createReq) (*saleResult, error) {
	out, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return processSale(context.Background(), tx, h.user, "127.0.0.1", "Caja Uno", in)
	})
	if err != nil {
		return nil, err
	}
	return out.(*saleResult), nil
}

func (h *harness) balance(t *testing.T, id string) int64 {
	t.Helper()
	var b int64
	if err := h.db.Reader.QueryRow(`SELECT balance FROM attendees WHERE id = ?`, id).Scan(&b); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b
}

func (h *harness) stock(t *testing.T, id string) int64 {
	t.Helper()
	var s int64
	if err := h.db.Reader.QueryRow(`SELECT stock_qty FROM products WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return s
}

func item(pid string, qty int64) struct {
	ProductID string `json:"product_id"`
	Qty       int64  `json:"qty"`
} {
	return struct {
		ProductID string `json:"product_id"`
		Qty       int64  `json:"qty"`
	}{pid, qty}
}

func payerID(id string) struct {
	AttendeeID string `json:"attendee_id"`
	QRToken    string `json:"qr_token"`
} {
	return struct {
		AttendeeID string `json:"attendee_id"`
		QRToken    string `json:"qr_token"`
	}{AttendeeID: id}
}

// ---------------------------------------------------------------------------
// (a) Basic sale: debits balance and decrements stock exactly.
// ---------------------------------------------------------------------------

func TestBasicSale(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "bar", 1, 10)
	h.seedAttendee(t, "a1", "qr-a1", 20000)

	res, err := h.sell(createReq{
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 2)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{payerID("a1")},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if res.summary.Total != 10000 || res.summary.PaidFromBalance != 10000 || res.summary.PaidCash != 0 {
		t.Fatalf("totals wrong: %+v", res.summary)
	}
	if got := h.balance(t, "a1"); got != 10000 {
		t.Fatalf("balance: got %d want 10000", got)
	}
	if got := h.stock(t, "p1"); got != 8 {
		t.Fatalf("stock: got %d want 8", got)
	}
	if res.summary.Status != "preparing" {
		t.Fatalf("status: got %q want preparing", res.summary.Status)
	}
	// A single pendientes ticket should be queued (all items are deliverables).
	if len(res.tickets) != 1 || res.tickets[0].station != "pendientes" {
		t.Fatalf("tickets: %+v", res.tickets)
	}

	// Ledger debit must equal the balance delta exactly.
	var ledgerSum int64
	if err := h.db.Reader.QueryRow(
		`SELECT COALESCE(SUM(-amount),0) FROM balance_ledger WHERE attendee_id='a1' AND type='debit'`).
		Scan(&ledgerSum); err != nil {
		t.Fatal(err)
	}
	if ledgerSum != 10000 {
		t.Fatalf("ledger debit sum: got %d want 10000", ledgerSum)
	}
}

// ---------------------------------------------------------------------------
// (b) Overage: total>balance.
// ---------------------------------------------------------------------------

func TestOverageRequiresCashConfirmation(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Combo", 30000, "none", 1, 5)
	h.seedAttendee(t, "a1", "qr-a1", 20000)

	base := createReq{
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 1)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{payerID("a1")},
	}

	// Without confirmation -> rejected, nothing changes.
	if _, err := h.sell(base); err != core.ErrCashNotConfirmed {
		t.Fatalf("want ErrCashNotConfirmed, got %v", err)
	}
	if got := h.balance(t, "a1"); got != 20000 {
		t.Fatalf("balance must be untouched on reject: got %d", got)
	}
	if got := h.stock(t, "p1"); got != 5 {
		t.Fatalf("stock must be untouched on reject: got %d", got)
	}

	// With confirmation -> balance to 0, cash records the remainder.
	base.CashConfirmed = true
	res, err := h.sell(base)
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if res.summary.PaidFromBalance != 20000 || res.summary.PaidCash != 10000 {
		t.Fatalf("split wrong: %+v", res.summary)
	}
	if got := h.balance(t, "a1"); got != 0 {
		t.Fatalf("balance: got %d want 0", got)
	}
	var cash int64
	if err := h.db.Reader.QueryRow(
		`SELECT amount FROM payments WHERE order_id=? AND method='cash'`, res.summary.ID).Scan(&cash); err != nil {
		t.Fatalf("cash payment: %v", err)
	}
	if cash != 10000 {
		t.Fatalf("cash payment: got %d want 10000", cash)
	}
}

// ---------------------------------------------------------------------------
// (c) CONCURRENCY: the heart of the engine.
// ---------------------------------------------------------------------------

// TestConcurrentLastUnitStock: many goroutines race for a product with only a
// few units in stock. Exactly stock-count sales must succeed; stock never goes
// negative.
func TestConcurrentLastUnitStock(t *testing.T) {
	h := newHarness(t)
	const initialStock = 7
	h.seedProduct(t, "p1", "Limited", 1000, "none", 1, initialStock)
	// Give every payer a deep balance so money is never the limiting factor.
	const buyers = 50
	attendees := make([]string, buyers)
	for i := 0; i < buyers; i++ {
		id := "a" + strconv.Itoa(i)
		attendees[i] = id
		h.seedAttendee(t, id, "qr-"+id, 1_000_000)
	}

	var success, insufficient int64
	var wg sync.WaitGroup
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.sell(createReq{
				Items: []struct {
					ProductID string `json:"product_id"`
					Qty       int64  `json:"qty"`
				}{item("p1", 1)},
				Payers: []struct {
					AttendeeID string `json:"attendee_id"`
					QRToken    string `json:"qr_token"`
				}{payerID(attendees[i])},
			})
			switch err {
			case nil:
				atomic.AddInt64(&success, 1)
			case core.ErrInsufficientStock:
				atomic.AddInt64(&insufficient, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if success != initialStock {
		t.Fatalf("successes: got %d want %d", success, initialStock)
	}
	if insufficient != buyers-initialStock {
		t.Fatalf("rejections: got %d want %d", insufficient, buyers-initialStock)
	}
	if got := h.stock(t, "p1"); got != 0 {
		t.Fatalf("final stock: got %d want 0 (never negative)", got)
	}
	// Stock movements must reconcile to exactly the sales made.
	var moves int64
	if err := h.db.Reader.QueryRow(
		`SELECT COUNT(*) FROM stock_movements WHERE product_id='p1' AND reason='sale'`).Scan(&moves); err != nil {
		t.Fatal(err)
	}
	if moves != initialStock {
		t.Fatalf("sale movements: got %d want %d", moves, initialStock)
	}
}

// TestConcurrentSameAttendee: many goroutines charge the SAME attendee whose
// balance only covers a fraction of the requests. Exactly the affordable number
// succeed; balance never goes negative; SUM(ledger debits) == balance delta.
func TestConcurrentSameAttendee(t *testing.T) {
	h := newHarness(t)
	// Unlimited stock so money is the only constraint.
	h.seedProduct(t, "p1", "Drink", 1000, "none", 0, 0)
	const startBalance = 9000
	const price = 1000
	h.seedAttendee(t, "a1", "qr-a1", startBalance)

	const attempts = 60
	var success, insufficient int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.sell(createReq{
				Items: []struct {
					ProductID string `json:"product_id"`
					Qty       int64  `json:"qty"`
				}{item("p1", 1)},
				Payers: []struct {
					AttendeeID string `json:"attendee_id"`
					QRToken    string `json:"qr_token"`
				}{payerID("a1")},
			})
			switch err {
			case nil:
				atomic.AddInt64(&success, 1)
			case core.ErrCashNotConfirmed:
				// Once balance is exhausted, a sale needs cash confirmation.
				atomic.AddInt64(&insufficient, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	wantSuccess := int64(startBalance / price)
	if success != wantSuccess {
		t.Fatalf("successes: got %d want %d", success, wantSuccess)
	}
	if got := h.balance(t, "a1"); got != 0 {
		t.Fatalf("final balance: got %d want 0 (never negative)", got)
	}

	var ledgerDelta int64
	if err := h.db.Reader.QueryRow(
		`SELECT COALESCE(SUM(-amount),0) FROM balance_ledger WHERE attendee_id='a1' AND type='debit'`).
		Scan(&ledgerDelta); err != nil {
		t.Fatal(err)
	}
	if ledgerDelta != startBalance-0 {
		t.Fatalf("ledger debit sum %d != balance delta %d", ledgerDelta, startBalance)
	}
	// Each successful sale = exactly one balance payment (no double discount).
	var payCount int64
	if err := h.db.Reader.QueryRow(
		`SELECT COUNT(*) FROM payments WHERE attendee_id='a1' AND method='balance'`).Scan(&payCount); err != nil {
		t.Fatal(err)
	}
	if payCount != wantSuccess {
		t.Fatalf("balance payments: got %d want %d", payCount, wantSuccess)
	}
}

// ---------------------------------------------------------------------------
// (d) Idempotency: same request_id twice -> one charge, same order.
// ---------------------------------------------------------------------------

func TestIdempotency(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 10)
	h.seedAttendee(t, "a1", "qr-a1", 20000)

	req := createReq{
		RequestID: "client-uuid-1",
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 1)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{payerID("a1")},
	}

	first, err := h.sell(req)
	if err != nil {
		t.Fatalf("first sell: %v", err)
	}
	if first.idempotent {
		t.Fatal("first call must not be idempotent replay")
	}

	second, err := h.sell(req)
	if err != nil {
		t.Fatalf("second sell: %v", err)
	}
	if !second.idempotent {
		t.Fatal("second call must be an idempotent replay")
	}
	if second.summary.ID != first.summary.ID {
		t.Fatalf("idempotent must return same order: %s vs %s", first.summary.ID, second.summary.ID)
	}
	// Charged exactly once.
	if got := h.balance(t, "a1"); got != 15000 {
		t.Fatalf("balance must reflect one charge: got %d want 15000", got)
	}
	if got := h.stock(t, "p1"); got != 9 {
		t.Fatalf("stock must reflect one sale: got %d want 9", got)
	}
	var orderCount int64
	if err := h.db.Reader.QueryRow(`SELECT COUNT(*) FROM orders WHERE request_id=?`, "client-uuid-1").Scan(&orderCount); err != nil {
		t.Fatal(err)
	}
	if orderCount != 1 {
		t.Fatalf("orders for request_id: got %d want 1", orderCount)
	}
}

// ---------------------------------------------------------------------------
// Split allocation + void reversal.
// ---------------------------------------------------------------------------

func TestSplitCascade(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Bottle", 30000, "none", 0, 0)
	// a1 has spare balance, a2 is short -> cascade covers a2's shortfall.
	h.seedAttendee(t, "a1", "qr-a1", 25000)
	h.seedAttendee(t, "a2", "qr-a2", 1000)

	res, err := h.sell(createReq{
		CashConfirmed: true,
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 1)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{
			payerID("a1"), payerID("a2"),
		},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	// total 30000, shares 15000/15000. a1 own=15000 + lends its remaining 10000
	// to a2 (debit 25000). a2 own=1000. a2 shortfall 14000, covered 10000 by a1,
	// residual 4000 collected as cash. So from_balance=26000, cash=4000.
	if res.summary.PaidFromBalance != 26000 || res.summary.PaidCash != 4000 {
		t.Fatalf("cascade allocation wrong: from_balance=%d cash=%d (want 26000/4000)",
			res.summary.PaidFromBalance, res.summary.PaidCash)
	}
	if got := h.balance(t, "a1"); got != 0 {
		t.Fatalf("a1 balance: got %d want 0", got)
	}
	if got := h.balance(t, "a2"); got != 0 {
		t.Fatalf("a2 balance: got %d want 0", got)
	}
	// Splits sum to total.
	var shareSum int64
	if err := h.db.Reader.QueryRow(`SELECT COALESCE(SUM(share_amount),0) FROM order_splits WHERE order_id=?`,
		res.summary.ID).Scan(&shareSum); err != nil {
		t.Fatal(err)
	}
	if shareSum != 30000 {
		t.Fatalf("split shares sum: got %d want 30000", shareSum)
	}
}

func TestVoidReverses(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 10)
	h.seedAttendee(t, "a1", "qr-a1", 20000)

	res, err := h.sell(createReq{
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 3)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{payerID("a1")},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if h.balance(t, "a1") != 5000 || h.stock(t, "p1") != 7 {
		t.Fatalf("pre-void state wrong: bal=%d stock=%d", h.balance(t, "a1"), h.stock(t, "p1"))
	}

	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return processVoid(context.Background(), tx, h.user, "127.0.0.1", res.summary.ID)
	}); err != nil {
		t.Fatalf("void: %v", err)
	}

	if got := h.balance(t, "a1"); got != 20000 {
		t.Fatalf("balance restored: got %d want 20000", got)
	}
	if got := h.stock(t, "p1"); got != 10 {
		t.Fatalf("stock restored: got %d want 10", got)
	}
	var st string
	if err := h.db.Reader.QueryRow(`SELECT status FROM orders WHERE id=?`, res.summary.ID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "voided" {
		t.Fatalf("status: got %q want voided", st)
	}
	// Double void is rejected.
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return processVoid(context.Background(), tx, h.user, "127.0.0.1", res.summary.ID)
	}); err != core.ErrValidation {
		t.Fatalf("double void: want ErrValidation, got %v", err)
	}
}

// TestSaleRequiresActiveEvent proves selling only works while the event is
// 'active'. With the event still in 'draft' (configuration phase) processSale
// finds no active event and rejects the sale; once activated, the same sale
// succeeds. This is the sales side of the lifecycle lock-down.
func TestSaleRequiresActiveEvent(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 10)
	h.seedAttendee(t, "a1", "qr-a1", 20000)

	// Move the event back to draft: selling must now be refused.
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return tx.Exec(`UPDATE events SET status='draft' WHERE id='ev1'`)
	}); err != nil {
		t.Fatalf("set draft: %v", err)
	}

	sale := createReq{
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 1)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{payerID("a1")},
	}
	if _, err := h.sell(sale); err != core.ErrNoActiveEvent {
		t.Fatalf("draft sale: want ErrNoActiveEvent, got %v", err)
	}
	// Nothing was charged.
	if got := h.balance(t, "a1"); got != 20000 {
		t.Fatalf("balance must be untouched while draft: got %d", got)
	}

	// Activate the event: the same sale now succeeds.
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return tx.Exec(`UPDATE events SET status='active' WHERE id='ev1'`)
	}); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if _, err := h.sell(sale); err != nil {
		t.Fatalf("active sale: %v", err)
	}
	if got := h.balance(t, "a1"); got != 15000 {
		t.Fatalf("balance after active sale: got %d, want 15000", got)
	}
}

func TestBlockedAttendeeRejected(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 10)
	h.seedAttendee(t, "a1", "qr-a1", 20000)
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return tx.Exec(`UPDATE attendees SET status='blocked' WHERE id='a1'`)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := h.sell(createReq{
		Items: []struct {
			ProductID string `json:"product_id"`
			Qty       int64  `json:"qty"`
		}{item("p1", 1)},
		Payers: []struct {
			AttendeeID string `json:"attendee_id"`
			QRToken    string `json:"qr_token"`
		}{payerID("a1")},
	})
	if err != core.ErrValidation {
		t.Fatalf("blocked payer: want ErrValidation, got %v", err)
	}
}
