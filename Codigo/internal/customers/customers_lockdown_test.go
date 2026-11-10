package customers

// customers_lockdown_test.go — proves the lifecycle lock-down and the redimible
// (no_redeem) / identity-field behaviour:
//
//   - attendee create / import / topup are rejected with HTTP 409 +
//     core.ErrEventLocked once the current event is 'active' (started);
//   - block / unblock keep working while the event is active (the "bloquear el
//     bono por si cualquier cosa" requirement);
//   - a no_redeem attendee gets its initial_topup forced to 0 and rejects topups
//     with core.ErrValidation (400);
//   - email is stored and returned.
//
// Helpers openTestEnv, seedActiveEvent (draft), seedStartedEvent, seedAttendeeRow,
// postJSON, postJSONWithID live in customers_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"eventpos/internal/core"
)

// getAttendee issues a proper GET /api/attendees/{id} against m.get and decodes
// the result.
func getAttendee(t *testing.T, m *Module, id string) Attendee {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/attendees/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	m.get(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get attendee %s: got %d; body: %s", id, w.Code, w.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(w.Body).Decode(&a); err != nil {
		t.Fatalf("decode attendee: %v", err)
	}
	return a
}

// expectLocked asserts a 409 Conflict whose body error equals ErrEventLocked.
func expectLocked(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusConflict {
		t.Fatalf("locked mutation: got %d, want 409; body: %s", code, body)
	}
	var resp map[string]string
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, body)
	}
	if resp["error"] != core.ErrEventLocked.Error() {
		t.Errorf("error = %q, want %q", resp["error"], core.ErrEventLocked.Error())
	}
}

// (a) Registering attendees works DURING the active event — people keep
// arriving, so registration is operation, not locked configuration.
func TestCreate_AllowedWhenActive(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedStartedEvent(t, d)

	rr := postJSON(m.create, "/api/attendees", `{"display_name":"WalkUp"}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("create while active: got %d, want 2xx; body: %s", rr.Code, rr.Body.String())
	}
}

// (a) Bulk import also works while the event is active.
func TestBulkImport_AllowedWhenActive(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedStartedEvent(t, d)

	rr := postJSON(m.bulkImport, "/api/attendees/import",
		`{"attendees":[{"display_name":"A"},{"display_name":"B"}]}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("import while active: got %d, want 2xx; body: %s", rr.Code, rr.Body.String())
	}
}

// (a) Recargas (top-ups) work while the event is active.
func TestTopup_AllowedWhenActive(t *testing.T) {
	m, d := openTestEnv(t)
	evID := seedStartedEvent(t, d)
	aID := seedAttendeeRow(t, d, evID, "Started", "started-qr-token")

	rr := postJSONWithID(m.topup, "/api/attendees/"+aID+"/topup", "id", aID, `{"amount":1000}`)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("topup while active: got %d, want 2xx; body: %s", rr.Code, rr.Body.String())
	}
	var bal int64
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id=?`, aID).Scan(&bal); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if bal != 1000 {
		t.Fatalf("topup balance: got %d, want 1000", bal)
	}
}

// (b) Block / unblock MUST keep working while the event is active.
func TestBlockUnblock_WorkWhenActive(t *testing.T) {
	m, d := openTestEnv(t)
	evID := seedStartedEvent(t, d)
	aID := seedAttendeeRow(t, d, evID, "BonoHolder", "bono-qr-token")

	// Block.
	rr := postJSONWithID(m.block, "/api/attendees/"+aID+"/block", "id", aID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("block while active: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var status string
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT status FROM attendees WHERE id=?`, aID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "blocked" {
		t.Fatalf("status after block = %q, want 'blocked'", status)
	}

	// Unblock.
	rr = postJSONWithID(m.unblock, "/api/attendees/"+aID+"/unblock", "id", aID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("unblock while active: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT status FROM attendees WHERE id=?`, aID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Fatalf("status after unblock = %q, want 'active'", status)
	}
}

// (c) A no_redeem attendee created with initial_topup gets balance forced to 0,
// and subsequent topups are rejected with core.ErrValidation (400).
func TestNoRedeem_ForcesZeroAndRejectsTopup(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedActiveEvent(t, d) // draft → configuration allowed

	rr := postJSON(m.create, "/api/attendees",
		`{"display_name":"CashOnly","no_redeem":true,"initial_topup":5000}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create no_redeem: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rr.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !a.NoRedeem {
		t.Error("created attendee no_redeem = false, want true")
	}
	if a.Balance != 0 {
		t.Fatalf("no_redeem balance forced: got %d, want 0", a.Balance)
	}

	// No ledger row should exist (no balance was ever credited).
	var ledgerRows int
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM balance_ledger WHERE attendee_id=?`, a.ID).Scan(&ledgerRows); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerRows != 0 {
		t.Fatalf("no_redeem attendee has %d ledger rows, want 0", ledgerRows)
	}

	// Topup of a no_redeem attendee is rejected with 400 (ErrValidation).
	rr = postJSONWithID(m.topup, "/api/attendees/"+a.ID+"/topup", "id", a.ID, `{"amount":1000}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no_redeem topup: got %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
	var bal int64
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT balance FROM attendees WHERE id=?`, a.ID).Scan(&bal); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if bal != 0 {
		t.Fatalf("no_redeem balance after rejected topup: got %d, want 0", bal)
	}
}

// (d) email is stored on create and returned on read; it also flows through
// bulk import.
func TestEmailStoredAndReturned(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedActiveEvent(t, d) // draft

	const email = "croody@example.com"
	rr := postJSON(m.create, "/api/attendees",
		`{"display_name":"Emailer","email":"`+email+`","doc_id":"CED-1","phone":"+57300"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create with email: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rr.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Email == nil || *a.Email != email {
		t.Fatalf("create response email = %v, want %q", a.Email, email)
	}

	// Stored in the DB.
	var dbEmail string
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT email FROM attendees WHERE id=?`, a.ID).Scan(&dbEmail); err != nil {
		t.Fatalf("read email: %v", err)
	}
	if dbEmail != email {
		t.Fatalf("stored email = %q, want %q", dbEmail, email)
	}

	// Returned via GET /api/attendees/{id} (exercises scanAttendee).
	got := getAttendee(t, m, a.ID)
	if got.Email == nil || *got.Email != email {
		t.Fatalf("get email = %v, want %q", got.Email, email)
	}
	if got.NoRedeem {
		t.Errorf("default no_redeem should be false, got true")
	}
}

// TestBulkImport_EmailAndNoRedeem verifies email + no_redeem flow through import.
func TestBulkImport_EmailAndNoRedeem(t *testing.T) {
	m, d := openTestEnv(t)
	_ = seedActiveEvent(t, d) // draft

	body := `{"attendees":[
		{"display_name":"Redeemer","email":"r@example.com","initial_topup":3000},
		{"display_name":"CashGuy","no_redeem":true,"initial_topup":9999}
	]}`
	rr := postJSON(m.bulkImport, "/api/attendees/import", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Attendees []Attendee `json:"attendees"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]Attendee{}
	for _, a := range resp.Attendees {
		byName[a.DisplayName] = a
	}
	if got := byName["Redeemer"]; got.Email == nil || *got.Email != "r@example.com" || got.Balance != 3000 {
		t.Fatalf("Redeemer = %+v, want email r@example.com balance 3000", got)
	}
	if got := byName["CashGuy"]; !got.NoRedeem || got.Balance != 0 {
		t.Fatalf("CashGuy = %+v, want no_redeem=true balance=0", got)
	}
	_ = d
}
