package sales

// Adversarial audit tests (independent reviewer). These attempt to BREAK the
// engine's stated guarantees: no double-discount, no negative balance/stock,
// atomic sales, correct split cascade, correct void reversal, race-safe
// idempotency. They reuse the temp-db harness from sales_test.go.
//
// Run: cd /Users/mnzz/action && PATH=/opt/homebrew/bin:$PATH go test -race -run Audit ./internal/sales/

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/core"
)

// Type aliases to the exact anonymous struct types used by createReq, so we can
// build slice literals concisely. Structurally identical => assignable.
type itemT = struct {
	ProductID string `json:"product_id"`
	Qty       int64  `json:"qty"`
}
type payerT = struct {
	AttendeeID string `json:"attendee_id"`
	QRToken    string `json:"qr_token"`
}

func payerQR(qr string) payerT { return payerT{QRToken: qr} }

func (h *harness) setPolicy(t *testing.T, policy string) {
	t.Helper()
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return tx.Exec(`UPDATE events SET split_policy=? WHERE id='ev1'`, policy)
	}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
}

// sumBalanceDebits returns SUM(-amount) of an attendee's 'debit' ledger rows.
func (h *harness) sumBalanceDebits(t *testing.T, att string) int64 {
	t.Helper()
	var s int64
	if err := h.db.Reader.QueryRow(
		`SELECT COALESCE(SUM(-amount),0) FROM balance_ledger WHERE attendee_id=? AND type='debit'`, att).
		Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// orderPaymentSums returns total balance-method and cash-method payment amounts.
func (h *harness) orderPaymentSums(t *testing.T, orderID string) (bal, cash int64) {
	t.Helper()
	if err := h.db.Reader.QueryRow(
		`SELECT COALESCE(SUM(CASE WHEN method='balance' THEN amount ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN method='cash'    THEN amount ELSE 0 END),0)
		 FROM payments WHERE order_id=?`, orderID).Scan(&bal, &cash); err != nil {
		t.Fatal(err)
	}
	return
}

// ===========================================================================
// MONEY: shares sum EXACTLY to total; no peso lost or created. Many awkward
// totals (non-divisible by payer count) and mixed balances.
// ===========================================================================

func TestAuditSharesSumToTotalNoLostPesos(t *testing.T) {
	cases := []struct {
		name     string
		price    int64 // untracked product, qty=1 so total==price
		balances []int64
	}{
		{"3payers_rem1_allRich", 10000, []int64{1_000_000, 1_000_000, 1_000_000}},
		{"3payers_rem2_allRich", 10001, []int64{1_000_000, 1_000_000, 1_000_000}},
		{"7payers_rem_mixed", 99991, []int64{1_000_000, 0, 5000, 1_000_000, 1, 2, 1_000_000}},
		{"2payers_rem1", 7, []int64{1_000_000, 1_000_000}},
		{"5payers_prime", 100003, []int64{30000, 30000, 30000, 30000, 30000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedProduct(t, "p1", "Svc", c.price, "none", 0, 0) // untracked
			payers := make([]payerT, len(c.balances))
			startBal := make([]int64, len(c.balances))
			for i, b := range c.balances {
				id := "a" + string(rune('A'+i))
				h.seedAttendee(t, id, "qr-"+id, b)
				payers[i] = payerID(id)
				startBal[i] = b
			}
			res, err := h.sell(createReq{
				CashConfirmed: true, // allow any cash residual
				Items:         []itemT{item("p1", 1)},
				Payers:        payers,
			})
			if err != nil {
				t.Fatalf("sell: %v", err)
			}
			oid := res.summary.ID

			// 1) order total == price.
			if res.summary.Total != c.price {
				t.Fatalf("total: got %d want %d", res.summary.Total, c.price)
			}
			// 2) paid_from_balance + paid_cash == total (no peso lost/created).
			if res.summary.PaidFromBalance+res.summary.PaidCash != c.price {
				t.Fatalf("paid_from_balance(%d)+paid_cash(%d) != total(%d)",
					res.summary.PaidFromBalance, res.summary.PaidCash, c.price)
			}
			// 3) order_splits.share_amount sum == total (exact, no rounding loss).
			var shareSum, splitBal, splitCash int64
			if err := h.db.Reader.QueryRow(
				`SELECT COALESCE(SUM(share_amount),0), COALESCE(SUM(paid_from_balance),0), COALESCE(SUM(cash_collected),0)
				 FROM order_splits WHERE order_id=?`, oid).Scan(&shareSum, &splitBal, &splitCash); err != nil {
				t.Fatal(err)
			}
			if len(payers) > 1 && shareSum != c.price {
				t.Fatalf("split shares sum: got %d want %d", shareSum, c.price)
			}
			if len(payers) > 1 {
				if splitBal != res.summary.PaidFromBalance || splitCash != res.summary.PaidCash {
					t.Fatalf("split bal/cash (%d/%d) != order (%d/%d)",
						splitBal, splitCash, res.summary.PaidFromBalance, res.summary.PaidCash)
				}
			}
			// 4) Payments reconcile to the order.
			pbal, pcash := h.orderPaymentSums(t, oid)
			if pbal != res.summary.PaidFromBalance {
				t.Fatalf("balance payments %d != paid_from_balance %d", pbal, res.summary.PaidFromBalance)
			}
			if pcash != res.summary.PaidCash {
				t.Fatalf("cash payments %d != paid_cash %d", pcash, res.summary.PaidCash)
			}
			// 5) Each attendee's balance decreased by EXACTLY their ledger debit,
			//    and never went negative; total balance drained == paid_from_balance.
			var totalDrained int64
			for i := range payers {
				id := payers[i].AttendeeID
				end := h.balance(t, id)
				if end < 0 {
					t.Fatalf("%s balance negative: %d", id, end)
				}
				debited := startBal[i] - end
				if debited < 0 {
					t.Fatalf("%s balance INCREASED (money created): %d -> %d", id, startBal[i], end)
				}
				if got := h.sumBalanceDebits(t, id); got != debited {
					t.Fatalf("%s ledger debit %d != balance delta %d", id, got, debited)
				}
				totalDrained += debited
			}
			if totalDrained != res.summary.PaidFromBalance {
				t.Fatalf("sum of balance drains %d != paid_from_balance %d", totalDrained, res.summary.PaidFromBalance)
			}
		})
	}
}

// ===========================================================================
// SPLIT CASCADE: lender must never be overdrawn; debtor covered up to lender's
// spare balance; residual is cash and requires confirmation.
// ===========================================================================

func TestAuditCascadeNeverOverdrawsLender(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Bottle", 30000, "none", 0, 0)
	h.seedAttendee(t, "a1", "qr-a1", 25000) // lender: spare 10000 over its 15000 share
	h.seedAttendee(t, "a2", "qr-a2", 1000)  // debtor: short 14000

	res, err := h.sell(createReq{
		CashConfirmed: true,
		Items:         []itemT{item("p1", 1)},
		Payers:        []payerT{payerID("a1"), payerID("a2")},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if h.balance(t, "a1") != 0 || h.balance(t, "a2") != 0 {
		t.Fatalf("balances: a1=%d a2=%d want 0/0", h.balance(t, "a1"), h.balance(t, "a2"))
	}
	// a1 debit must be exactly its full balance 25000 (15000 own + 10000 lent),
	// never more (would imply negative balance / overdraw).
	if got := h.sumBalanceDebits(t, "a1"); got != 25000 {
		t.Fatalf("a1 debit: got %d want 25000 (overdraw if >)", got)
	}
	if got := h.sumBalanceDebits(t, "a2"); got != 1000 {
		t.Fatalf("a2 debit: got %d want 1000", got)
	}
	if res.summary.PaidFromBalance != 26000 || res.summary.PaidCash != 4000 {
		t.Fatalf("cascade: from_balance=%d cash=%d want 26000/4000", res.summary.PaidFromBalance, res.summary.PaidCash)
	}
}

// Cash residual under cascade still requires cash_confirmed (all-or-nothing).
func TestAuditCascadeCashResidualRequiresConfirm(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Bottle", 30000, "none", 0, 0)
	h.seedAttendee(t, "a1", "qr-a1", 5000)
	h.seedAttendee(t, "a2", "qr-a2", 5000)
	// total 30000, both balances 5000 => 20000 cash residual, no confirmation.
	_, err := h.sell(createReq{
		Items:  []itemT{item("p1", 1)},
		Payers: []payerT{payerID("a1"), payerID("a2")},
	})
	if err != core.ErrCashNotConfirmed {
		t.Fatalf("want ErrCashNotConfirmed, got %v", err)
	}
	// Nothing moved.
	if h.balance(t, "a1") != 5000 || h.balance(t, "a2") != 5000 {
		t.Fatalf("balances must be untouched on reject: a1=%d a2=%d", h.balance(t, "a1"), h.balance(t, "a2"))
	}
	var n int64
	if err := h.db.Reader.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no order must be created on reject, got %d", n)
	}
}

// strict_equal policy: a payer's spare balance must NOT cover another's
// shortfall (contrast with cascade). Each pays own share; rest is cash.
func TestAuditStrictEqualNoLending(t *testing.T) {
	h := newHarness(t)
	h.setPolicy(t, "strict_equal")
	h.seedProduct(t, "p1", "Bottle", 30000, "none", 0, 0)
	h.seedAttendee(t, "a1", "qr-a1", 25000) // surplus 10000 must be RETAINED
	h.seedAttendee(t, "a2", "qr-a2", 1000)

	res, err := h.sell(createReq{
		CashConfirmed: true,
		Items:         []itemT{item("p1", 1)},
		Payers:        []payerT{payerID("a1"), payerID("a2")},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	// a1 pays only its 15000 share, keeps 10000. a2 pays 1000, 14000 cash.
	if h.balance(t, "a1") != 10000 {
		t.Fatalf("strict_equal lent surplus! a1 balance got %d want 10000", h.balance(t, "a1"))
	}
	if h.balance(t, "a2") != 0 {
		t.Fatalf("a2 balance got %d want 0", h.balance(t, "a2"))
	}
	if res.summary.PaidFromBalance != 16000 || res.summary.PaidCash != 14000 {
		t.Fatalf("strict_equal alloc: from_balance=%d cash=%d want 16000/14000",
			res.summary.PaidFromBalance, res.summary.PaidCash)
	}
}

// ===========================================================================
// NEGATIVE GUARD / AGGREGATION: a product referenced by TWO line items must be
// decremented ONCE with a correct combined version guard (no false conflict, no
// double-decrement). Combined qty over stock => InsufficientStock, not Conflict.
// ===========================================================================

func TestAuditDuplicateLineItemAggregation(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 10)
	h.seedAttendee(t, "a1", "qr-a1", 1_000_000)

	res, err := h.sell(createReq{
		Items:  []itemT{item("p1", 3), item("p1", 4)}, // same product twice
		Payers: []payerT{payerID("a1")},
	})
	if err != nil {
		t.Fatalf("sell: %v (must NOT be a false version-guard conflict)", err)
	}
	if got := h.stock(t, "p1"); got != 3 {
		t.Fatalf("stock: got %d want 3 (10-7, single combined decrement)", got)
	}
	// Exactly ONE 'sale' stock movement for the combined qty.
	var moves, totalDelta int64
	if err := h.db.Reader.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(delta),0) FROM stock_movements WHERE ref_order_id=? AND reason='sale'`,
		res.summary.ID).Scan(&moves, &totalDelta); err != nil {
		t.Fatal(err)
	}
	if moves != 1 || totalDelta != -7 {
		t.Fatalf("stock movements: count=%d delta=%d want 1/-7", moves, totalDelta)
	}
	if res.summary.Total != 35000 {
		t.Fatalf("total: got %d want 35000", res.summary.Total)
	}
	if got := h.balance(t, "a1"); got != 1_000_000-35000 {
		t.Fatalf("balance: got %d want %d", got, 1_000_000-35000)
	}
}

func TestAuditDuplicateLineItemOverStock(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 5)
	h.seedAttendee(t, "a1", "qr-a1", 1_000_000)
	_, err := h.sell(createReq{
		Items:  []itemT{item("p1", 3), item("p1", 3)}, // combined 6 > 5
		Payers: []payerT{payerID("a1")},
	})
	if err != core.ErrInsufficientStock {
		t.Fatalf("combined over stock: want ErrInsufficientStock, got %v", err)
	}
	if got := h.stock(t, "p1"); got != 5 {
		t.Fatalf("stock must be untouched: got %d want 5", got)
	}
}

// ===========================================================================
// DUPLICATE PAYER: an attendee may appear at most once, even mixing id & qr.
// ===========================================================================

func TestAuditDuplicatePayerRejected(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 0, 0)
	h.seedAttendee(t, "a1", "qr-a1", 1_000_000)

	// Same attendee twice by id.
	if _, err := h.sell(createReq{
		Items:  []itemT{item("p1", 1)},
		Payers: []payerT{payerID("a1"), payerID("a1")},
	}); err != core.ErrValidation {
		t.Fatalf("dup by id: want ErrValidation, got %v", err)
	}
	// Same attendee, once by id once by qr_token.
	if _, err := h.sell(createReq{
		Items:  []itemT{item("p1", 1)},
		Payers: []payerT{payerID("a1"), payerQR("qr-a1")},
	}); err != core.ErrValidation {
		t.Fatalf("dup by id+qr: want ErrValidation, got %v", err)
	}
	if got := h.balance(t, "a1"); got != 1_000_000 {
		t.Fatalf("balance must be untouched: got %d", got)
	}
}

// ===========================================================================
// IDEMPOTENCY under concurrency: N concurrent identical request_id => exactly
// ONE order, charged ONCE, stock decremented ONCE. Tries to slip a duplicate
// through the request_id check.
// ===========================================================================

func TestAuditConcurrentIdempotency(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 1000)
	h.seedAttendee(t, "a1", "qr-a1", 1_000_000)

	const n = 64
	req := createReq{
		RequestID: "dup-key-1",
		Items:     []itemT{item("p1", 2)},
		Payers:    []payerT{payerID("a1")},
	}
	var wg sync.WaitGroup
	ids := make([]string, n)
	var firstCount int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.sell(req)
			if err != nil {
				t.Errorf("sell: %v", err)
				return
			}
			ids[i] = res.summary.ID
			if !res.idempotent {
				atomic.AddInt64(&firstCount, 1)
			}
		}(i)
	}
	wg.Wait()

	// Exactly one non-idempotent (real) creation.
	if firstCount != 1 {
		t.Fatalf("non-idempotent creations: got %d want 1 (double-charge race!)", firstCount)
	}
	// All returned the same order id.
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("order id mismatch: %s vs %s", ids[i], ids[0])
		}
	}
	// One order, one charge, one decrement.
	var orderCount int64
	if err := h.db.Reader.QueryRow(`SELECT COUNT(*) FROM orders WHERE request_id='dup-key-1'`).Scan(&orderCount); err != nil {
		t.Fatal(err)
	}
	if orderCount != 1 {
		t.Fatalf("orders for request_id: got %d want 1", orderCount)
	}
	if got := h.balance(t, "a1"); got != 1_000_000-10000 {
		t.Fatalf("balance: got %d want %d (charged exactly once)", got, 1_000_000-10000)
	}
	if got := h.stock(t, "p1"); got != 998 {
		t.Fatalf("stock: got %d want 998 (decremented exactly once)", got)
	}
	var payCount int64
	if err := h.db.Reader.QueryRow(`SELECT COUNT(*) FROM payments WHERE order_id=?`, ids[0]).Scan(&payCount); err != nil {
		t.Fatal(err)
	}
	if payCount != 1 {
		t.Fatalf("payments: got %d want 1", payCount)
	}
}

// ===========================================================================
// VOID of a SPLIT order: each payer's balance refunded EXACTLY; stock restored;
// double-void rejected; money conserved end-to-end.
// ===========================================================================

func TestAuditVoidSplitRefundsEachPayer(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Bottle", 30000, "none", 1, 5)
	h.seedAttendee(t, "a1", "qr-a1", 25000)
	h.seedAttendee(t, "a2", "qr-a2", 20000)

	res, err := h.sell(createReq{
		Items:  []itemT{item("p1", 1)},
		Payers: []payerT{payerID("a1"), payerID("a2")},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	// shares 15000/15000, both afford => from_balance 30000, cash 0.
	if res.summary.PaidFromBalance != 30000 || res.summary.PaidCash != 0 {
		t.Fatalf("alloc: from_balance=%d cash=%d want 30000/0", res.summary.PaidFromBalance, res.summary.PaidCash)
	}
	if h.balance(t, "a1") != 10000 || h.balance(t, "a2") != 5000 {
		t.Fatalf("post-sale: a1=%d a2=%d want 10000/5000", h.balance(t, "a1"), h.balance(t, "a2"))
	}

	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return processVoid(context.Background(), tx, h.user, "127.0.0.1", res.summary.ID)
	}); err != nil {
		t.Fatalf("void: %v", err)
	}
	// Both refunded to original.
	if h.balance(t, "a1") != 25000 || h.balance(t, "a2") != 20000 {
		t.Fatalf("post-void: a1=%d a2=%d want 25000/20000", h.balance(t, "a1"), h.balance(t, "a2"))
	}
	if h.stock(t, "p1") != 5 {
		t.Fatalf("stock restored: got %d want 5", h.stock(t, "p1"))
	}
	// Net ledger per attendee (debit + refund) must be zero.
	for _, id := range []string{"a1", "a2"} {
		var net int64
		if err := h.db.Reader.QueryRow(
			`SELECT COALESCE(SUM(amount),0) FROM balance_ledger WHERE attendee_id=?`, id).Scan(&net); err != nil {
			t.Fatal(err)
		}
		if net != 0 {
			t.Fatalf("%s net ledger after void: got %d want 0", id, net)
		}
	}
	// Double void rejected, no extra refund.
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return processVoid(context.Background(), tx, h.user, "127.0.0.1", res.summary.ID)
	}); err != core.ErrValidation {
		t.Fatalf("double void: want ErrValidation, got %v", err)
	}
	if h.balance(t, "a1") != 25000 || h.balance(t, "a2") != 20000 {
		t.Fatalf("double-void changed balances: a1=%d a2=%d", h.balance(t, "a1"), h.balance(t, "a2"))
	}
}

// Overage cash_due exactness across several totals: no off-by-one.
func TestAuditOverageCashDueExact(t *testing.T) {
	for _, tc := range []struct{ price, bal, wantCash, wantBal int64 }{
		{30000, 20000, 10000, 20000},
		{30001, 20000, 10001, 20000},
		{1, 0, 1, 0},
		{99999, 50000, 49999, 50000},
	} {
		h := newHarness(t)
		h.seedProduct(t, "p1", "Combo", tc.price, "none", 0, 0)
		h.seedAttendee(t, "a1", "qr-a1", tc.bal)
		res, err := h.sell(createReq{
			CashConfirmed: true,
			Items:         []itemT{item("p1", 1)},
			Payers:        []payerT{payerID("a1")},
		})
		if err != nil {
			t.Fatalf("price=%d: %v", tc.price, err)
		}
		if res.summary.PaidCash != tc.wantCash || res.summary.PaidFromBalance != tc.wantBal {
			t.Fatalf("price=%d: cash=%d from_balance=%d want %d/%d",
				tc.price, res.summary.PaidCash, res.summary.PaidFromBalance, tc.wantCash, tc.wantBal)
		}
		if res.summary.PaidFromBalance+res.summary.PaidCash != tc.price {
			t.Fatalf("price=%d: split does not sum to total", tc.price)
		}
	}
}

// ===========================================================================
// AUDIT/LEDGER hash chain stays intact after a batch of sales + a void.
// ===========================================================================

func TestAuditChainIntactAfterActivity(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 1, 100)
	h.seedAttendee(t, "a1", "qr-a1", 1_000_000)
	h.seedAttendee(t, "a2", "qr-a2", 1_000_000)

	var lastOrder string
	for i := 0; i < 10; i++ {
		res, err := h.sell(createReq{
			Items:  []itemT{item("p1", 1)},
			Payers: []payerT{payerID("a1"), payerID("a2")},
		})
		if err != nil {
			t.Fatalf("sell %d: %v", i, err)
		}
		lastOrder = res.summary.ID
	}
	if _, err := h.w.Do(context.Background(), func(tx *sql.Tx) (any, error) {
		return processVoid(context.Background(), tx, h.user, "127.0.0.1", lastOrder)
	}); err != nil {
		t.Fatalf("void: %v", err)
	}
	if bad, err := app.VerifyAuditChain(context.Background(), h.db); err != nil {
		t.Fatalf("verify: %v", err)
	} else if bad != "" {
		t.Fatalf("audit chain broken at id=%s", bad)
	}
}

// ===========================================================================
// DEFENSE-IN-DEPTH PROBE: empty payers list reaching allocate() divides by
// zero. The HTTP handler guards len(payers)==0, so this is not reachable via
// the API, but allocate() itself has no guard. Verified directly (NOT through
// the writer goroutine, which would crash the whole process on panic).
// ===========================================================================

func TestAuditAllocateEmptyPayersPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Logf("FINDING (LOW/defense-in-depth): allocate() panics with empty payers: %v", r)
		} else {
			t.Logf("allocate() tolerated empty payers (no panic)")
		}
	}()
	_ = allocate([]payerAlloc{}, 1000, "cascade")
}

// Sell with no open caja session is silently allowed (session_id NULL) even
// though core.ErrNoOpenCashSession exists. This test documents the behavior:
// it asserts the engine permits the sale (so reviewers see the gap explicitly).
func TestAuditSaleWithoutCajaSession(t *testing.T) {
	h := newHarness(t)
	h.seedProduct(t, "p1", "Beer", 5000, "none", 0, 0)
	h.seedAttendee(t, "a1", "qr-a1", 100000)
	res, err := h.sell(createReq{
		Items:  []itemT{item("p1", 1)},
		Payers: []payerT{payerID("a1")},
	})
	if err != nil {
		t.Fatalf("sell without caja session errored: %v", err)
	}
	var sess sql.NullString
	if err := h.db.Reader.QueryRow(`SELECT session_id FROM orders WHERE id=?`, res.summary.ID).Scan(&sess); err != nil {
		t.Fatal(err)
	}
	if sess.Valid {
		t.Fatalf("expected NULL session_id, got %q", sess.String)
	}
	t.Logf("FINDING (info): sale committed with NULL caja session; ErrNoOpenCashSession is never enforced")
}
