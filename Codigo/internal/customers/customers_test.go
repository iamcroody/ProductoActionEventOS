package customers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// openTestEnv opens a real on-disk SQLite database in a temp directory,
// wires up the single writer, and returns a fully constructed Module.
// Cleanup is registered via t.Cleanup so callers need not close anything.
func openTestEnv(t *testing.T) (*Module, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	w := writer.New(d.Writer)
	t.Cleanup(func() { w.Close() })

	a := &app.App{DB: d, Writer: w}
	m := New(server.Deps{App: a})
	return m, d
}

// seedActiveEvent inserts a minimal current event in 'draft' status directly via
// the writer connection and returns its id. Configuration (attendee create /
// import / topup) is only permitted while the current event is in draft; this is
// the lifecycle lock-down. Tests that need an active (started) event seed one
// explicitly via seedStartedEvent.
func seedActiveEvent(t *testing.T, d *db.DB) string {
	t.Helper()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Test Event", "draft", 50000, "cascade", core.NowMillis(),
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID
}

// seedStartedEvent inserts a minimal current event already in 'active' status —
// i.e. the event has been started and configuration is locked. Returns its id.
func seedStartedEvent(t *testing.T, d *db.DB) string {
	t.Helper()
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Started Event", "active", 50000, "cascade", core.NowMillis(),
	); err != nil {
		t.Fatalf("seed started event: %v", err)
	}
	return eventID
}

// seedAttendeeRow inserts an attendee row directly (bypassing the locked create
// handler), useful for testing operations on an already-started event.
func seedAttendeeRow(t *testing.T, d *db.DB, eventID, displayName, qrToken string) string {
	t.Helper()
	id := core.NewID()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO attendees(id, event_id, qr_token, display_name,
		  balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,0,0,'active',?)`,
		id, eventID, qrToken, displayName, core.NowMillis(),
	); err != nil {
		t.Fatalf("seed attendee row: %v", err)
	}
	return id
}

// postJSON is a test helper that issues a handler call with a JSON body.
func postJSON(handler func(http.ResponseWriter, *http.Request), path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

// postJSONWithID is like postJSON but sets the named path value (e.g. "id").
func postJSONWithID(handler func(http.ResponseWriter, *http.Request), path, idKey, idVal, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue(idKey, idVal)
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

// TestAttendeeCreateAndTopup proves the core balance invariant:
//
//  1. Creating an attendee with initial_topup seeds the balance and writes a
//     ledger row so the attendee is never visible with zero balance followed
//     by a credit.
//  2. A subsequent topup adds to the cached balance (with version guard).
//  3. The final balance equals the sum of both topups.
//  4. The balance_ledger has exactly two rows whose balance_after values are
//     strictly monotonically increasing.
func TestAttendeeCreateAndTopup(t *testing.T) {
	m, d := openTestEnv(t)
	ctx := context.Background()
	_ = seedActiveEvent(t, d)

	const initialTopup = int64(10000)
	const secondTopup = int64(5000)
	const wantFinal = initialTopup + secondTopup

	// --- step 1: create attendee with initial topup ---
	w1 := postJSON(m.create, "/api/attendees",
		fmt.Sprintf(`{"display_name":"Alice","initial_topup":%d}`, initialTopup))
	if w1.Code != http.StatusOK {
		t.Fatalf("create: status %d body: %s", w1.Code, w1.Body.String())
	}
	var created Attendee
	if err := json.NewDecoder(w1.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create: got empty ID")
	}
	if created.QRToken == "" {
		t.Fatal("create: got empty qr_token")
	}
	if created.Balance != initialTopup {
		t.Fatalf("create: balance got %d, want %d", created.Balance, initialTopup)
	}
	if created.BalanceVersion != 1 {
		t.Fatalf("create: balance_version got %d, want 1", created.BalanceVersion)
	}

	// --- step 2: second topup ---
	w2 := postJSONWithID(m.topup, "/api/attendees/"+created.ID+"/topup",
		"id", created.ID,
		fmt.Sprintf(`{"amount":%d}`, secondTopup))
	if w2.Code != http.StatusOK {
		t.Fatalf("topup: status %d body: %s", w2.Code, w2.Body.String())
	}

	// --- step 3: verify cached balance in attendees table ---
	var balance int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT balance FROM attendees WHERE id=?`, created.ID,
	).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != wantFinal {
		t.Fatalf("balance: got %d, want %d", balance, wantFinal)
	}

	// --- step 4: verify ledger has 2 rows with monotonic balance_after ---
	rows, err := d.Reader.QueryContext(ctx,
		`SELECT amount, balance_after FROM balance_ledger
		 WHERE attendee_id=?
		 ORDER BY created_at ASC, id ASC`,
		created.ID,
	)
	if err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	defer rows.Close()

	type entry struct{ amount, balanceAfter int64 }
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.amount, &e.balanceAfter); err != nil {
			t.Fatalf("scan ledger row: %v", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ledger rows iteration: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger row count: got %d, want 2", len(entries))
	}

	// First row: the initial topup (amount == initialTopup, balance_after == initialTopup).
	if entries[0].amount != initialTopup {
		t.Fatalf("ledger[0].amount: got %d, want %d", entries[0].amount, initialTopup)
	}
	if entries[0].balanceAfter != initialTopup {
		t.Fatalf("ledger[0].balance_after: got %d, want %d", entries[0].balanceAfter, initialTopup)
	}

	// Second row: the subsequent topup.
	if entries[1].amount != secondTopup {
		t.Fatalf("ledger[1].amount: got %d, want %d", entries[1].amount, secondTopup)
	}
	if entries[1].balanceAfter != wantFinal {
		t.Fatalf("ledger[1].balance_after: got %d, want %d", entries[1].balanceAfter, wantFinal)
	}

	// Strict monotonic invariant: each balance_after must exceed the previous.
	if entries[0].balanceAfter >= entries[1].balanceAfter {
		t.Fatalf("balance_after not monotonically increasing: %d >= %d",
			entries[0].balanceAfter, entries[1].balanceAfter)
	}
}

// TestAttendeeNoActiveEvent verifies that creating an attendee when no event
// is active returns a 400 Bad Request with the expected error message.
func TestAttendeeNoActiveEvent(t *testing.T) {
	m, _ := openTestEnv(t) // no event seeded
	w := postJSON(m.create, "/api/attendees", `{"display_name":"Bob"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAttendeeValidation verifies that a missing display_name is rejected.
func TestAttendeeValidation(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedActiveEvent(t, d)
	w := postJSON(m.create, "/api/attendees", `{"display_name":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestQRTokenUnique verifies that two attendees created in the same event
// get distinct QR tokens.
func TestQRTokenUnique(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedActiveEvent(t, d)

	w1 := postJSON(m.create, "/api/attendees", `{"display_name":"Alice"}`)
	if w1.Code != http.StatusOK {
		t.Fatalf("create alice: %d %s", w1.Code, w1.Body.String())
	}
	w2 := postJSON(m.create, "/api/attendees", `{"display_name":"Bob"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("create bob: %d %s", w2.Code, w2.Body.String())
	}

	var a1, a2 Attendee
	_ = json.NewDecoder(w1.Body).Decode(&a1)
	_ = json.NewDecoder(w2.Body).Decode(&a2)
	if a1.QRToken == a2.QRToken {
		t.Fatalf("qr_token collision: both got %q", a1.QRToken)
	}
}

// TestBulkImport verifies that the import endpoint creates all attendees in
// one shot and returns the correct count.
func TestBulkImport(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedActiveEvent(t, d)

	body := `{"attendees":[
		{"display_name":"Alice","initial_topup":10000},
		{"display_name":"Bob"},
		{"display_name":"Carol","initial_topup":5000}
	]}`
	w := postJSON(m.bulkImport, "/api/attendees/import", body)
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Count     int        `json:"count"`
		Attendees []Attendee `json:"attendees"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if resp.Count != 3 {
		t.Fatalf("count: got %d, want 3", resp.Count)
	}
	if len(resp.Attendees) != 3 {
		t.Fatalf("attendees len: got %d, want 3", len(resp.Attendees))
	}
	// Alice and Carol should have non-zero balances.
	balances := map[string]int64{}
	for _, a := range resp.Attendees {
		balances[a.DisplayName] = a.Balance
	}
	if balances["Alice"] != 10000 {
		t.Fatalf("alice balance: got %d, want 10000", balances["Alice"])
	}
	if balances["Bob"] != 0 {
		t.Fatalf("bob balance: got %d, want 0", balances["Bob"])
	}
	if balances["Carol"] != 5000 {
		t.Fatalf("carol balance: got %d, want 5000", balances["Carol"])
	}
}
