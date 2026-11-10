package caja

// caja_extra_test.go — deeper edge-case tests for the cash-register module.
//
// Helpers openTestDB, buildDeps, seedEvent, seedUser, seedAuthToken, and
// newReq are declared in caja_test.go and reused here because both files
// belong to the same package (package caja).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"eventpos/internal/core"
	"eventpos/internal/db"
)

// seedOrderExtra inserts an order row linked to a cash session and returns the
// new order ID. Only the columns required by the payments JOIN in the close
// handler are populated; everything else takes its column default.
// Named with the "Extra" suffix to avoid collision with any future helper of
// the same purpose in caja_test.go.
func seedOrderExtra(t *testing.T, dbase *db.DB,
	eventID, sessionID, operatorID, status string) string {
	t.Helper()
	id := core.NewID()
	if _, err := dbase.Writer.ExecContext(context.Background(),
		`INSERT INTO orders(id, event_id, session_id, operator_id, status, created_at)
		 VALUES (?,?,?,?,?,?)`,
		id, eventID, sessionID, operatorID, status, core.NowMillis()); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return id
}

// seedCashPaymentExtra inserts a cash payment row for an order. The payments
// table is append-only; we insert directly because no public API seeds payments
// in the caja package.
func seedCashPaymentExtra(t *testing.T, dbase *db.DB,
	orderID, userID string, amount int64) {
	t.Helper()
	id := core.NewID()
	if _, err := dbase.Writer.ExecContext(context.Background(),
		`INSERT INTO payments(id, order_id, method, amount, created_at, user_id)
		 VALUES (?,?,'cash',?,?,?)`,
		id, orderID, amount, core.NowMillis(), userID); err != nil {
		t.Fatalf("seed cash payment: %v", err)
	}
}

// TestCajaVarianceMathWithCashPayments opens a session, seeds several cash
// payments, closes with a counted amount, and asserts:
//   - expected_cash = opening_float + SUM(cash payments for non-voided orders)
//   - variance = counted_cash - expected_cash
func TestCajaVarianceMathWithCashPayments(t *testing.T) {
	d := openTestDB(t)
	deps := buildDeps(t, d)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	eventID := seedEvent(t, d)
	userID := seedUser(t, d)
	token := seedAuthToken(t, d, userID)

	const openingFloat = int64(50000)

	// Open a session.
	openRR := httptest.NewRecorder()
	mux.ServeHTTP(openRR, newReq("POST", "/api/caja/open",
		fmt.Sprintf(`{"opening_float":%d}`, openingFloat), token))
	if openRR.Code != http.StatusOK {
		t.Fatalf("open: got %d: %s", openRR.Code, openRR.Body.String())
	}
	var sess Session
	if err := json.NewDecoder(openRR.Body).Decode(&sess); err != nil {
		t.Fatalf("decode open response: %v", err)
	}

	// Seed three paid orders with cash payments linked to the session.
	cashAmounts := []int64{10000, 5000, 15000} // total = 30000
	for _, amount := range cashAmounts {
		orderID := seedOrderExtra(t, d, eventID, sess.ID, userID, "paid")
		seedCashPaymentExtra(t, d, orderID, userID, amount)
	}

	// Close with a counted amount.
	const countedCash = int64(85000)
	const wantExpected = openingFloat + 30000       // 80000
	const wantVariance = countedCash - wantExpected // 5000

	closeRR := httptest.NewRecorder()
	mux.ServeHTTP(closeRR, newReq("POST", "/api/caja/close",
		fmt.Sprintf(`{"counted_cash":%d}`, countedCash), token))
	if closeRR.Code != http.StatusOK {
		t.Fatalf("close: got %d: %s", closeRR.Code, closeRR.Body.String())
	}
	var closed Session
	if err := json.NewDecoder(closeRR.Body).Decode(&closed); err != nil {
		t.Fatalf("decode close response: %v", err)
	}

	if closed.Status != "closed" {
		t.Errorf("status: got %q, want %q", closed.Status, "closed")
	}
	if closed.ExpectedCash == nil {
		t.Fatal("expected_cash is nil")
	}
	if *closed.ExpectedCash != wantExpected {
		t.Errorf("expected_cash: got %d, want %d", *closed.ExpectedCash, wantExpected)
	}
	if closed.CountedCash == nil {
		t.Fatal("counted_cash is nil")
	}
	if *closed.CountedCash != countedCash {
		t.Errorf("counted_cash: got %d, want %d", *closed.CountedCash, countedCash)
	}
	if closed.Variance == nil {
		t.Fatal("variance is nil")
	}
	if *closed.Variance != wantVariance {
		t.Errorf("variance: got %d, want %d", *closed.Variance, wantVariance)
	}
}

// TestCajaVoidedOrdersExcludedFromExpectedCash verifies that cash payments for
// voided orders are excluded from the expected_cash calculation on close.
func TestCajaVoidedOrdersExcludedFromExpectedCash(t *testing.T) {
	d := openTestDB(t)
	deps := buildDeps(t, d)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	eventID := seedEvent(t, d)
	userID := seedUser(t, d)
	token := seedAuthToken(t, d, userID)

	const openingFloat = int64(100000)

	// Open session.
	openRR := httptest.NewRecorder()
	mux.ServeHTTP(openRR, newReq("POST", "/api/caja/open",
		fmt.Sprintf(`{"opening_float":%d}`, openingFloat), token))
	if openRR.Code != http.StatusOK {
		t.Fatalf("open: %d %s", openRR.Code, openRR.Body.String())
	}
	var sess Session
	if err := json.NewDecoder(openRR.Body).Decode(&sess); err != nil {
		t.Fatalf("decode open: %v", err)
	}

	// Paid order (cash = 50000) — included in expected_cash.
	paidOrderID := seedOrderExtra(t, d, eventID, sess.ID, userID, "paid")
	seedCashPaymentExtra(t, d, paidOrderID, userID, 50000)

	// Voided order (cash = 200000) — must be excluded from expected_cash.
	voidedOrderID := seedOrderExtra(t, d, eventID, sess.ID, userID, "voided")
	seedCashPaymentExtra(t, d, voidedOrderID, userID, 200000)

	// expected_cash = 100000 + 50000 = 150000 (voided not counted)
	const countedCash = int64(160000)
	const wantExpected = int64(150000)
	const wantVariance = countedCash - wantExpected // 10000

	closeRR := httptest.NewRecorder()
	mux.ServeHTTP(closeRR, newReq("POST", "/api/caja/close",
		fmt.Sprintf(`{"counted_cash":%d}`, countedCash), token))
	if closeRR.Code != http.StatusOK {
		t.Fatalf("close: %d %s", closeRR.Code, closeRR.Body.String())
	}
	var closed Session
	if err := json.NewDecoder(closeRR.Body).Decode(&closed); err != nil {
		t.Fatalf("decode close: %v", err)
	}

	if closed.ExpectedCash == nil || *closed.ExpectedCash != wantExpected {
		t.Errorf("expected_cash: got %v, want %d", closed.ExpectedCash, wantExpected)
	}
	if closed.Variance == nil || *closed.Variance != wantVariance {
		t.Errorf("variance: got %v, want %d", closed.Variance, wantVariance)
	}
}

// TestCajaCloseGuards verifies two error conditions:
//  1. Calling close when no session is open returns 404 (ErrNotFound).
//  2. Closing a session that was already closed also returns 404, because
//     loadOpenSession finds no open session for the operator.
func TestCajaCloseGuards(t *testing.T) {
	d := openTestDB(t)
	deps := buildDeps(t, d)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	_ = seedEvent(t, d)
	userID := seedUser(t, d)
	token := seedAuthToken(t, d, userID)

	// Guard 1: no open session → 404.
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, newReq("POST", "/api/caja/close", `{"counted_cash":0}`, token))
	if rr1.Code != http.StatusNotFound {
		t.Fatalf("close with no session: expected 404, got %d: %s",
			rr1.Code, rr1.Body.String())
	}

	// Open and then properly close a session.
	openRR := httptest.NewRecorder()
	mux.ServeHTTP(openRR, newReq("POST", "/api/caja/open", `{"opening_float":10000}`, token))
	if openRR.Code != http.StatusOK {
		t.Fatalf("open: %d %s", openRR.Code, openRR.Body.String())
	}

	closeRR := httptest.NewRecorder()
	mux.ServeHTTP(closeRR, newReq("POST", "/api/caja/close", `{"counted_cash":10000}`, token))
	if closeRR.Code != http.StatusOK {
		t.Fatalf("first close: %d %s", closeRR.Code, closeRR.Body.String())
	}

	// Guard 2: close again after already closed → 404.
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, newReq("POST", "/api/caja/close", `{"counted_cash":10000}`, token))
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("close already-closed session: expected 404, got %d: %s",
			rr2.Code, rr2.Body.String())
	}
}

// TestCajaReopenAfterCloseAndOneOpenRule verifies:
//  1. After closing a session, the same operator can open a new one (the
//     partial unique index allows it once the old session is 'closed').
//  2. The one-open-per-operator rule still holds: opening a third session
//     while the second is open returns 409 Conflict.
func TestCajaReopenAfterCloseAndOneOpenRule(t *testing.T) {
	d := openTestDB(t)
	deps := buildDeps(t, d)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	_ = seedEvent(t, d)
	userID := seedUser(t, d)
	token := seedAuthToken(t, d, userID)

	// Open session 1.
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, newReq("POST", "/api/caja/open", `{"opening_float":5000}`, token))
	if rr1.Code != http.StatusOK {
		t.Fatalf("open session 1: %d %s", rr1.Code, rr1.Body.String())
	}

	// Close session 1.
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, newReq("POST", "/api/caja/close", `{"counted_cash":5000}`, token))
	if rr2.Code != http.StatusOK {
		t.Fatalf("close session 1: %d %s", rr2.Code, rr2.Body.String())
	}

	// Open session 2 — must succeed because session 1 is closed.
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, newReq("POST", "/api/caja/open", `{"opening_float":7500}`, token))
	if rr3.Code != http.StatusOK {
		t.Fatalf("open session 2 (after close): expected 200, got %d: %s",
			rr3.Code, rr3.Body.String())
	}
	var sess2 Session
	if err := json.NewDecoder(rr3.Body).Decode(&sess2); err != nil {
		t.Fatalf("decode session 2: %v", err)
	}
	if sess2.Status != "open" {
		t.Errorf("session 2 status: got %q, want \"open\"", sess2.Status)
	}
	if sess2.OpeningFloat != 7500 {
		t.Errorf("session 2 opening_float: got %d, want 7500", sess2.OpeningFloat)
	}

	// Attempt to open session 3 while session 2 is still open → 409 Conflict.
	rr4 := httptest.NewRecorder()
	mux.ServeHTTP(rr4, newReq("POST", "/api/caja/open", `{"opening_float":1000}`, token))
	if rr4.Code != http.StatusConflict {
		t.Fatalf("open session 3 (while session 2 open): expected 409, got %d: %s",
			rr4.Code, rr4.Body.String())
	}
}
