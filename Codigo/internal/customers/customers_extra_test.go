package customers

// customers_extra_test.go — deeper edge-case and concurrency tests.
//
// Helpers openTestEnv, seedActiveEvent, postJSON, and postJSONWithID are
// declared in customers_test.go and reused here because both files belong to
// the same package (package customers).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"eventpos/internal/core"
)

// TestTopupConcurrencyBalanceInvariant launches many goroutines that all topup
// the same attendee concurrently. Because every topup serialises through the
// single writer, all should succeed with no lost updates. The final balance
// must equal the sum of all topup amounts, and the ledger must contain one row
// per topup with a monotonically increasing balance_after sequence.
func TestTopupConcurrencyBalanceInvariant(t *testing.T) {
	m, d := openTestEnv(t)
	ctx := context.Background()
	_ = seedActiveEvent(t, d)

	// Create the target attendee with zero initial balance.
	rr := postJSON(m.create, "/api/attendees", `{"display_name":"ConcurrentUser"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create attendee: %d %s", rr.Code, rr.Body.String())
	}
	var target Attendee
	if err := json.NewDecoder(rr.Body).Decode(&target); err != nil {
		t.Fatalf("decode attendee: %v", err)
	}

	const topupAmount = int64(500)
	const goroutines = 20
	const wantFinal = topupAmount * goroutines

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"amount":%d}`, topupAmount)
			rr := postJSONWithID(
				m.topup,
				"/api/attendees/"+target.ID+"/topup",
				"id", target.ID,
				body,
			)
			if rr.Code != http.StatusOK {
				t.Errorf("topup failed: %d %s", rr.Code, rr.Body.String())
			}
		}()
	}
	wg.Wait()

	// Final balance must equal sum of all topups — no lost updates.
	var finalBalance int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT balance FROM attendees WHERE id=?`, target.ID).Scan(&finalBalance); err != nil {
		t.Fatalf("read final balance: %v", err)
	}
	if finalBalance != wantFinal {
		t.Fatalf("lost update detected: balance got %d, want %d", finalBalance, wantFinal)
	}

	// Ledger must have exactly goroutines rows, one per successful topup.
	rows, err := d.Reader.QueryContext(ctx,
		`SELECT balance_after FROM balance_ledger
		 WHERE attendee_id=?
		 ORDER BY created_at ASC, id ASC`,
		target.ID)
	if err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	defer rows.Close()

	var ledgerBalances []int64
	for rows.Next() {
		var ba int64
		if err := rows.Scan(&ba); err != nil {
			t.Fatalf("scan ledger row: %v", err)
		}
		ledgerBalances = append(ledgerBalances, ba)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ledger rows: %v", err)
	}

	if len(ledgerBalances) != goroutines {
		t.Fatalf("ledger row count: got %d, want %d", len(ledgerBalances), goroutines)
	}

	// balance_after must be strictly monotonically increasing (no lost updates).
	for i := 1; i < len(ledgerBalances); i++ {
		if ledgerBalances[i] <= ledgerBalances[i-1] {
			t.Fatalf("balance_after not monotonically increasing at index %d: %d <= %d",
				i, ledgerBalances[i], ledgerBalances[i-1])
		}
	}
	// Last balance_after must equal the final cached balance.
	if ledgerBalances[goroutines-1] != wantFinal {
		t.Fatalf("ledger final balance_after: got %d, want %d",
			ledgerBalances[goroutines-1], wantFinal)
	}
}

// TestBulkImportUniqueQRTokensAndTopups covers:
//   - Bulk import of N attendees produces N unique qr_token values (DB-level
//     and in the response).
//   - Each attendee's balance matches its initial_topup.
//   - Attendees without an initial_topup have balance 0.
func TestBulkImportUniqueQRTokensAndTopups(t *testing.T) {
	m, d := openTestEnv(t)
	ctx := context.Background()
	_ = seedActiveEvent(t, d)

	// Build a batch of 15 attendees, alternating between with/without topup.
	const n = 15
	type importEntry struct {
		DisplayName  string `json:"display_name"`
		InitialTopup int64  `json:"initial_topup,omitempty"`
	}
	entries := make([]importEntry, n)
	wantBalances := make([]int64, n)
	for i := 0; i < n; i++ {
		entries[i] = importEntry{DisplayName: fmt.Sprintf("BulkUser%d", i)}
		if i%2 == 0 {
			entries[i].InitialTopup = int64((i + 1) * 1000)
			wantBalances[i] = entries[i].InitialTopup
		}
	}

	body, err := json.Marshal(map[string]any{"attendees": entries})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rr := postJSON(m.bulkImport, "/api/attendees/import", string(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk import: %d %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Count     int        `json:"count"`
		Attendees []Attendee `json:"attendees"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if resp.Count != n {
		t.Fatalf("count: got %d, want %d", resp.Count, n)
	}
	if len(resp.Attendees) != n {
		t.Fatalf("attendees len: got %d, want %d", len(resp.Attendees), n)
	}

	// Assert all QR tokens in the response are unique.
	responseTokens := make(map[string]bool)
	for i, a := range resp.Attendees {
		if a.QRToken == "" {
			t.Errorf("attendee[%d] has empty qr_token", i)
		}
		if responseTokens[a.QRToken] {
			t.Errorf("duplicate qr_token in response: %q", a.QRToken)
		}
		responseTokens[a.QRToken] = true
	}

	// Assert all QR tokens stored in the DB are unique (the UNIQUE index should
	// enforce this, but we verify the count matches).
	var dbTokenCount int
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT qr_token) FROM attendees`).Scan(&dbTokenCount); err != nil {
		t.Fatalf("count distinct tokens: %v", err)
	}
	if dbTokenCount != n {
		t.Fatalf("db qr_token distinct count: got %d, want %d", dbTokenCount, n)
	}

	// Assert balances match initial_topup (order is preserved by bulkImport).
	for i, a := range resp.Attendees {
		if a.Balance != wantBalances[i] {
			t.Errorf("attendee[%d] (%s) balance: got %d, want %d",
				i, a.DisplayName, a.Balance, wantBalances[i])
		}
	}
}

// TestSearchByDocIDAndEventScope verifies two properties of GET /api/attendees:
//  1. The query matches on doc_id as well as display_name.
//  2. Results are scoped to the active event — an attendee in a non-active
//     event with the same display_name does not appear.
func TestSearchByDocIDAndEventScope(t *testing.T) {
	m, d := openTestEnv(t)
	ctx := context.Background()
	_ = seedActiveEvent(t, d)

	// Create an attendee with a doc_id in the active event.
	w1 := postJSON(m.create, "/api/attendees",
		`{"display_name":"SearchAlice","doc_id":"DOCID-UNIQUE-99"}`)
	if w1.Code != http.StatusOK {
		t.Fatalf("create alice: %d %s", w1.Code, w1.Body.String())
	}
	var alice Attendee
	if err := json.NewDecoder(w1.Body).Decode(&alice); err != nil {
		t.Fatalf("decode alice: %v", err)
	}

	// Also create a second attendee in the same active event.
	w2 := postJSON(m.create, "/api/attendees", `{"display_name":"OtherUser"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("create other: %d %s", w2.Code, w2.Body.String())
	}

	// Insert a third attendee directly into a different (closed) event. They
	// share the same display_name as Alice — they must NOT appear in searches
	// scoped to the current event. A 'closed' event is used (not another draft)
	// so the current event remains unambiguous under the lock-down model.
	otherEventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		otherEventID, "Closed Event", "closed", 50000, "cascade", core.NowMillis()); err != nil {
		t.Fatalf("seed closed event: %v", err)
	}
	otherAttendeeID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO attendees(id, event_id, qr_token, display_name,
		  balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,0,0,'active',?)`,
		otherAttendeeID, otherEventID, "OTHER-QR-UNIQUE-TOKEN",
		"SearchAlice", core.NowMillis()); err != nil {
		t.Fatalf("seed other-event attendee: %v", err)
	}

	// --- Search by display_name: should return only the active-event Alice ---
	req1 := httptest.NewRequest(http.MethodGet, "/api/attendees?q=SearchAlice", nil)
	rr1 := httptest.NewRecorder()
	m.search(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("search by name: %d %s", rr1.Code, rr1.Body.String())
	}
	var results1 []Attendee
	if err := json.NewDecoder(rr1.Body).Decode(&results1); err != nil {
		t.Fatalf("decode search result: %v", err)
	}
	if len(results1) != 1 {
		t.Fatalf("search by name: expected 1 result, got %d", len(results1))
	}
	if results1[0].ID != alice.ID {
		t.Fatalf("search by name: expected alice ID=%s, got %s", alice.ID, results1[0].ID)
	}

	// --- Search by doc_id ---
	req2 := httptest.NewRequest(http.MethodGet, "/api/attendees?q=DOCID-UNIQUE-99", nil)
	rr2 := httptest.NewRecorder()
	m.search(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("search by doc_id: %d %s", rr2.Code, rr2.Body.String())
	}
	var results2 []Attendee
	if err := json.NewDecoder(rr2.Body).Decode(&results2); err != nil {
		t.Fatalf("decode search result: %v", err)
	}
	if len(results2) != 1 {
		t.Fatalf("search by doc_id: expected 1 result, got %d", len(results2))
	}
	if results2[0].ID != alice.ID {
		t.Fatalf("search by doc_id: expected alice, got ID=%s", results2[0].ID)
	}

	// --- Search for a term that matches nothing ---
	req3 := httptest.NewRequest(http.MethodGet, "/api/attendees?q=ZZZNOMATCH", nil)
	rr3 := httptest.NewRecorder()
	m.search(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("search (no match): %d %s", rr3.Code, rr3.Body.String())
	}
	var results3 []Attendee
	if err := json.NewDecoder(rr3.Body).Decode(&results3); err != nil {
		t.Fatalf("decode search result (no match): %v", err)
	}
	if len(results3) != 0 {
		t.Fatalf("search (no match): expected 0 results, got %d", len(results3))
	}
}

// TestQRTokenDBUniqueConstraint verifies that the DB-level UNIQUE constraint on
// qr_token prevents two attendees from sharing a token, even when an attendee
// is inserted directly bypassing the API's token-generation path.
func TestQRTokenDBUniqueConstraint(t *testing.T) {
	_, d := openTestEnv(t)
	ctx := context.Background()
	eventID := seedActiveEvent(t, d)

	now := core.NowMillis()
	id1 := core.NewID()
	id2 := core.NewID()
	const dupToken = "DUPLICATE-QR-TOKEN-FOR-TEST"

	// First insert must succeed.
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO attendees(id, event_id, qr_token, display_name,
		  balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,0,0,'active',?)`,
		id1, eventID, dupToken, "User1", now); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	// Second insert with the same qr_token must fail with a UNIQUE error.
	_, err := d.Writer.ExecContext(ctx,
		`INSERT INTO attendees(id, event_id, qr_token, display_name,
		  balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,0,0,'active',?)`,
		id2, eventID, dupToken, "User2", now)
	if err == nil {
		t.Fatal("expected UNIQUE constraint error for duplicate qr_token, got nil")
	}
}
