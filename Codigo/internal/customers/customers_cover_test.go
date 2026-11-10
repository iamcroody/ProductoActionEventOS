package customers

// customers_cover_test.go — HTTP-level tests that drive all routes through the
// real ServeMux with an authenticated session, covering the Register function
// and all handler error branches not reached by customers_test.go /
// customers_extra_test.go.
//
// Helpers openTestEnv, seedActiveEvent, postJSON, and postJSONWithID are
// declared in customers_test.go and are reused here (same package).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/config"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/realtime"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// coverHashToken replicates the unexported auth.hashToken so we can seed
// sessions directly without going through the real login endpoint.
func coverHashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// coverEnv holds the full HTTP+auth infrastructure for route-level tests.
type coverEnv struct {
	database *db.DB
	mux      *http.ServeMux
	adminTok string // raw session token for the seeded admin user
	cajaTok  string // raw session token for the seeded caja-role user
}

// newCoverEnv opens a fresh temp DB, seeds admin + caja users each with a
// one-hour session, registers the customers module routes, and returns a
// coverEnv ready to fire requests.
func newCoverEnv(t *testing.T) *coverEnv {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	w := writer.New(d.Writer)
	t.Cleanup(func() { w.Close() })

	hub := realtime.NewHub(16)

	backupDir := filepath.Join(tmpDir, "backups")
	backupSvc, err := backup.New(d, backupDir)
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}

	a := &app.App{
		DB:      d,
		Writer:  w,
		Hub:     hub,
		DataDir: tmpDir,
	}

	authSvc := auth.New(d)
	cfgStore := config.New(d)

	deps := server.Deps{
		App:    a,
		Auth:   authSvc,
		Config: cfgStore,
		Backup: backupSvc,
	}

	expAt := time.Now().Add(time.Hour).UTC().UnixMilli()
	ctx := context.Background()

	// Seed admin user + session.
	adminID := core.NewID()
	adminPinHash, err := auth.HashSecret("adminpin")
	if err != nil {
		t.Fatalf("HashSecret admin: %v", err)
	}
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,?,?)`,
		adminID, "coveradmin", "admin", adminPinHash, 1, core.NowMillis()); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}
	const rawAdmin = "cover-admin-raw-token-deadbeef1234"
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at)
		 VALUES (?,?,?,?,?)`,
		coverHashToken(rawAdmin), adminID, "127.0.0.1", core.NowMillis(), expAt); err != nil {
		t.Fatalf("seed admin session: %v", err)
	}

	// Seed caja-role user + session.
	cajaID := core.NewID()
	cajaPinHash, err := auth.HashSecret("cajapin")
	if err != nil {
		t.Fatalf("HashSecret caja: %v", err)
	}
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,?,?)`,
		cajaID, "covercaja", "caja", cajaPinHash, 1, core.NowMillis()); err != nil {
		t.Fatalf("seed caja user: %v", err)
	}
	const rawCaja = "cover-caja-raw-token-deadbeef5678"
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at)
		 VALUES (?,?,?,?,?)`,
		coverHashToken(rawCaja), cajaID, "127.0.0.1", core.NowMillis(), expAt); err != nil {
		t.Fatalf("seed caja session: %v", err)
	}

	mux := http.NewServeMux()
	m := New(deps)
	m.Register(mux, deps)

	return &coverEnv{
		database: d,
		mux:      mux,
		adminTok: rawAdmin,
		cajaTok:  rawCaja,
	}
}

// request fires an HTTP request through the mux with an optional session cookie
// and returns the response recorder.
func (e *coverEnv) request(t *testing.T, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "aeos_session", Value: token})
	}
	rw := httptest.NewRecorder()
	e.mux.ServeHTTP(rw, req)
	return rw
}

// requestRaw fires a request with a pre-serialized body string (for malformed JSON tests).
func (e *coverEnv) requestRaw(t *testing.T, token, method, path, rawBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(rawBody))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "aeos_session", Value: token})
	}
	rw := httptest.NewRecorder()
	e.mux.ServeHTTP(rw, req)
	return rw
}

func (e *coverEnv) admin(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	return e.request(t, e.adminTok, method, path, body)
}

func (e *coverEnv) caja(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	return e.request(t, e.cajaTok, method, path, body)
}

func (e *coverEnv) anon(t *testing.T, method, path string) *httptest.ResponseRecorder {
	return e.request(t, "", method, path, nil)
}

// createAttendeeViaHTTP posts to /api/attendees and returns the decoded Attendee.
func (e *coverEnv) createAttendeeViaHTTP(t *testing.T, name string) Attendee {
	t.Helper()
	rw := e.admin(t, "POST", "/api/attendees", map[string]any{"display_name": name})
	if rw.Code != http.StatusOK {
		t.Fatalf("createAttendee %q: got %d body: %s", name, rw.Code, rw.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode attendee: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// Auth / Register coverage
// ---------------------------------------------------------------------------

// TestRegister_Unauthenticated verifies that all routes reject requests with no
// cookie (no aeos_session) with HTTP 401.
func TestRegister_Unauthenticated(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database) // so auth is the only blocker

	routes := []struct{ method, path string }{
		{"POST", "/api/attendees"},
		{"POST", "/api/attendees/import"},
		{"GET", "/api/attendees"},
		{"GET", "/api/attendees/fakeid"},
		{"GET", "/api/qr/faketoken"},
		{"GET", "/api/attendees/fakeid/ledger"},
		{"POST", "/api/attendees/fakeid/topup"},
		{"POST", "/api/attendees/fakeid/block"},
		{"POST", "/api/attendees/fakeid/unblock"},
	}
	for _, r := range routes {
		rw := e.anon(t, r.method, r.path)
		if rw.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", r.method, r.path, rw.Code)
		}
	}
}

// TestRegister_ForbiddenRole verifies that a caja user cannot access admin-only
// routes (block and unblock require "admin").
func TestRegister_ForbiddenRole(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	a := e.createAttendeeViaHTTP(t, "BlockTarget")

	rw := e.caja(t, "POST", "/api/attendees/"+a.ID+"/block", nil)
	if rw.Code != http.StatusForbidden {
		t.Errorf("block with caja: want 403, got %d body: %s", rw.Code, rw.Body.String())
	}

	rw = e.caja(t, "POST", "/api/attendees/"+a.ID+"/unblock", nil)
	if rw.Code != http.StatusForbidden {
		t.Errorf("unblock with caja: want 403, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /api/attendees — create
// ---------------------------------------------------------------------------

// TestHTTPCreate_Success exercises POST /api/attendees with a valid name (no topup).
func TestHTTPCreate_Success(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees", map[string]any{"display_name": "HTTPAlice"})
	if rw.Code != http.StatusOK {
		t.Fatalf("create: got %d body: %s", rw.Code, rw.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.ID == "" {
		t.Error("create: empty ID")
	}
	if a.QRToken == "" {
		t.Error("create: empty QR token")
	}
	if a.Balance != 0 {
		t.Errorf("create no-topup: balance = %d, want 0", a.Balance)
	}
}

// TestHTTPCreate_WithInitialTopup exercises POST /api/attendees with initial_topup
// via the caja role (admin also passes middleware, but let's use caja to cover the
// middleware "admin passes caja guard" branch).
func TestHTTPCreate_WithInitialTopup(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.caja(t, "POST", "/api/attendees", map[string]any{
		"display_name":  "HTTPBob",
		"initial_topup": 7500,
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("create with topup: got %d body: %s", rw.Code, rw.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Balance != 7500 {
		t.Errorf("create with topup: balance = %d, want 7500", a.Balance)
	}
	if a.BalanceVersion != 1 {
		t.Errorf("create with topup: balance_version = %d, want 1", a.BalanceVersion)
	}
}

// TestHTTPCreate_BadJSON verifies that a malformed JSON body → 400.
func TestHTTPCreate_BadJSON(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.requestRaw(t, e.adminTok, "POST", "/api/attendees", `not-json`)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPCreate_EmptyDisplayName verifies that an empty display_name → 400.
func TestHTTPCreate_EmptyDisplayName(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees", map[string]any{"display_name": ""})
	if rw.Code != http.StatusBadRequest {
		t.Errorf("empty name: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPCreate_NoActiveEvent verifies that creating when no event is active → 400.
func TestHTTPCreate_NoActiveEvent(t *testing.T) {
	e := newCoverEnv(t) // no event seeded
	rw := e.admin(t, "POST", "/api/attendees", map[string]any{"display_name": "Ghost"})
	if rw.Code != http.StatusBadRequest {
		t.Errorf("no active event: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /api/attendees/import — bulk import
// ---------------------------------------------------------------------------

// TestHTTPBulkImport_Success exercises a valid bulk import through the mux.
func TestHTTPBulkImport_Success(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	body := map[string]any{
		"attendees": []map[string]any{
			{"display_name": "BulkAlice", "initial_topup": 3000},
			{"display_name": "BulkBob"},
		},
	}
	rw := e.admin(t, "POST", "/api/attendees/import", body)
	if rw.Code != http.StatusOK {
		t.Fatalf("bulk import: got %d body: %s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Count     int        `json:"count"`
		Attendees []Attendee `json:"attendees"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 2 {
		t.Errorf("bulk count: got %d, want 2", resp.Count)
	}
}

// TestHTTPBulkImport_EmptyList verifies that an empty attendees array → 400.
func TestHTTPBulkImport_EmptyList(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees/import", map[string]any{"attendees": []any{}})
	if rw.Code != http.StatusBadRequest {
		t.Errorf("empty list: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPBulkImport_InvalidRow verifies that a row with empty display_name → 400.
func TestHTTPBulkImport_InvalidRow(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	body := map[string]any{
		"attendees": []map[string]any{
			{"display_name": "ValidGuy"},
			{"display_name": ""}, // invalid — no name
		},
	}
	rw := e.admin(t, "POST", "/api/attendees/import", body)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("invalid row: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPBulkImport_BadJSON verifies that malformed JSON → 400.
func TestHTTPBulkImport_BadJSON(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.requestRaw(t, e.adminTok, "POST", "/api/attendees/import", `{bad json`)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPBulkImport_NoActiveEvent verifies that import without an active event → 400.
func TestHTTPBulkImport_NoActiveEvent(t *testing.T) {
	e := newCoverEnv(t) // no event seeded

	body := map[string]any{
		"attendees": []map[string]any{
			{"display_name": "NoEvent"},
		},
	}
	rw := e.admin(t, "POST", "/api/attendees/import", body)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("no active event: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GET /api/attendees — search
// ---------------------------------------------------------------------------

// TestHTTPSearch_EmptyQuery verifies that an empty query returns the full list
// for the active event.
func TestHTTPSearch_EmptyQuery(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	e.createAttendeeViaHTTP(t, "SearchOne")
	e.createAttendeeViaHTTP(t, "SearchTwo")

	rw := e.admin(t, "GET", "/api/attendees?q=", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("search empty: got %d body: %s", rw.Code, rw.Body.String())
	}
	var results []Attendee
	if err := json.NewDecoder(rw.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) < 2 {
		t.Errorf("search empty: got %d results, want at least 2", len(results))
	}
}

// TestHTTPSearch_WithQuery exercises the LIKE search branch.
func TestHTTPSearch_WithQuery(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	e.createAttendeeViaHTTP(t, "Queryable")
	e.createAttendeeViaHTTP(t, "Irrelevant")

	rw := e.admin(t, "GET", "/api/attendees?q=Queryable", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("search: got %d body: %s", rw.Code, rw.Body.String())
	}
	var results []Attendee
	if err := json.NewDecoder(rw.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("search with query: got %d results, want 1", len(results))
	}
}

// TestHTTPSearch_NoActiveEvent verifies search returns 400 when no event is active.
func TestHTTPSearch_NoActiveEvent(t *testing.T) {
	e := newCoverEnv(t) // no event seeded

	rw := e.admin(t, "GET", "/api/attendees?q=anything", nil)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("search no event: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GET /api/attendees/{id} — get
// ---------------------------------------------------------------------------

// TestHTTPGet_Found exercises GET /api/attendees/{id} for an existing attendee.
func TestHTTPGet_Found(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "GetTarget")

	rw := e.admin(t, "GET", "/api/attendees/"+a.ID, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("get found: got %d body: %s", rw.Code, rw.Body.String())
	}
	var got Attendee
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("get: id mismatch got %s want %s", got.ID, a.ID)
	}
}

// TestHTTPGet_NotFound verifies GET /api/attendees/{id} returns 404 for an unknown id.
func TestHTTPGet_NotFound(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "GET", "/api/attendees/nonexistent-id-xyz", nil)
	if rw.Code != http.StatusNotFound {
		t.Errorf("get not found: want 404, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GET /api/qr/{token} — byQR
// ---------------------------------------------------------------------------

// TestHTTPByQR_Found resolves an attendee from its QR token.
func TestHTTPByQR_Found(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "QRTarget")

	rw := e.admin(t, "GET", "/api/qr/"+a.QRToken, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("byQR found: got %d body: %s", rw.Code, rw.Body.String())
	}
	var got Attendee
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("byQR: id mismatch got %s want %s", got.ID, a.ID)
	}
}

// TestHTTPByQR_NotFound verifies 404 for an unknown QR token.
func TestHTTPByQR_NotFound(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "GET", "/api/qr/unknown-qr-token-xyz", nil)
	if rw.Code != http.StatusNotFound {
		t.Errorf("byQR not found: want 404, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GET /api/attendees/{id}/ledger — ledger
// ---------------------------------------------------------------------------

// TestHTTPLedger_Empty verifies that the ledger returns an empty array for a
// fresh attendee with no balance transactions.
func TestHTTPLedger_Empty(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "LedgerEmpty")

	rw := e.admin(t, "GET", "/api/attendees/"+a.ID+"/ledger", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("ledger empty: got %d body: %s", rw.Code, rw.Body.String())
	}
	var entries []LedgerEntry
	if err := json.NewDecoder(rw.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ledger empty: got %d entries, want 0", len(entries))
	}
}

// TestHTTPLedger_WithEntries verifies the ledger returns rows after a topup.
func TestHTTPLedger_WithEntries(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	// Create with initial topup so there is a ledger row.
	rw := e.admin(t, "POST", "/api/attendees", map[string]any{
		"display_name":  "LedgerFull",
		"initial_topup": 5000,
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("create: got %d body: %s", rw.Code, rw.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode attendee: %v", err)
	}

	rw = e.admin(t, "GET", "/api/attendees/"+a.ID+"/ledger", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("ledger with entries: got %d body: %s", rw.Code, rw.Body.String())
	}
	var entries []LedgerEntry
	if err := json.NewDecoder(rw.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ledger with entries: got %d entries, want 1", len(entries))
	}
	if entries[0].Amount != 5000 {
		t.Errorf("ledger entry amount: got %d, want 5000", entries[0].Amount)
	}
}

// TestHTTPLedger_UnknownAttendee verifies that the ledger for an unknown id
// returns an empty array (the query returns zero rows, no 404).
func TestHTTPLedger_UnknownAttendee(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "GET", "/api/attendees/nosuchid/ledger", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("ledger unknown id: got %d body: %s", rw.Code, rw.Body.String())
	}
	var entries []LedgerEntry
	if err := json.NewDecoder(rw.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ledger unknown: got %d entries, want 0", len(entries))
	}
}

// ---------------------------------------------------------------------------
// POST /api/attendees/{id}/topup — topup
// ---------------------------------------------------------------------------

// TestHTTPTopup_Success exercises the happy-path topup through the mux.
func TestHTTPTopup_Success(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "TopupTarget")

	rw := e.caja(t, "POST", "/api/attendees/"+a.ID+"/topup", map[string]any{"amount": 2000})
	if rw.Code != http.StatusOK {
		t.Fatalf("topup: got %d body: %s", rw.Code, rw.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("topup: ok = %v, want true", resp["ok"])
	}
	// balance should be 2000 (started from 0).
	if bal, ok := resp["balance"].(float64); !ok || int64(bal) != 2000 {
		t.Errorf("topup balance: got %v, want 2000", resp["balance"])
	}
}

// TestHTTPTopup_NonPositiveAmount verifies that amount <= 0 returns 400.
func TestHTTPTopup_NonPositiveAmount(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "TopupZero")

	rw := e.admin(t, "POST", "/api/attendees/"+a.ID+"/topup", map[string]any{"amount": 0})
	if rw.Code != http.StatusBadRequest {
		t.Errorf("topup zero: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}

	rw = e.admin(t, "POST", "/api/attendees/"+a.ID+"/topup", map[string]any{"amount": -500})
	if rw.Code != http.StatusBadRequest {
		t.Errorf("topup negative: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPTopup_BadJSON verifies that a malformed body → 400.
func TestHTTPTopup_BadJSON(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "TopupBadJSON")

	rw := e.requestRaw(t, e.adminTok, "POST", "/api/attendees/"+a.ID+"/topup", `{not json`)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("topup bad json: want 400, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPTopup_NotFound verifies that topping up an unknown attendee → 404.
func TestHTTPTopup_NotFound(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees/nosuchid/topup", map[string]any{"amount": 1000})
	if rw.Code != http.StatusNotFound {
		t.Errorf("topup not found: want 404, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /api/attendees/{id}/block and /unblock — setStatus
// ---------------------------------------------------------------------------

// TestHTTPBlock_Success exercises the block route end-to-end.
func TestHTTPBlock_Success(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "BlockMe")

	rw := e.admin(t, "POST", "/api/attendees/"+a.ID+"/block", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("block: got %d body: %s", rw.Code, rw.Body.String())
	}
	var resp map[string]bool
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["ok"] {
		t.Error("block: ok = false, want true")
	}

	// Verify status in DB.
	var status string
	if err := e.database.Reader.QueryRowContext(context.Background(),
		`SELECT status FROM attendees WHERE id=?`, a.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "blocked" {
		t.Errorf("block: status = %q, want 'blocked'", status)
	}
}

// TestHTTPBlock_NotFound verifies 404 when blocking an unknown attendee.
func TestHTTPBlock_NotFound(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees/nosuchid/block", nil)
	if rw.Code != http.StatusNotFound {
		t.Errorf("block not found: want 404, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPUnblock_Success exercises the unblock route end-to-end.
func TestHTTPUnblock_Success(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)
	a := e.createAttendeeViaHTTP(t, "UnblockMe")

	// Block first.
	if rw := e.admin(t, "POST", "/api/attendees/"+a.ID+"/block", nil); rw.Code != http.StatusOK {
		t.Fatalf("pre-block: got %d body: %s", rw.Code, rw.Body.String())
	}

	// Then unblock.
	rw := e.admin(t, "POST", "/api/attendees/"+a.ID+"/unblock", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("unblock: got %d body: %s", rw.Code, rw.Body.String())
	}

	var status string
	if err := e.database.Reader.QueryRowContext(context.Background(),
		`SELECT status FROM attendees WHERE id=?`, a.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "active" {
		t.Errorf("unblock: status = %q, want 'active'", status)
	}
}

// TestHTTPUnblock_NotFound verifies 404 when unblocking an unknown attendee.
func TestHTTPUnblock_NotFound(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees/nosuchid/unblock", nil)
	if rw.Code != http.StatusNotFound {
		t.Errorf("unblock not found: want 404, got %d body: %s", rw.Code, rw.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Additional coverage: caja can create + topup but not block
// ---------------------------------------------------------------------------

// TestHTTPCaja_CreateAndTopup verifies that the caja role can reach both the
// create and topup routes (admin passes the caja middleware guard too).
func TestHTTPCaja_CreateAndTopup(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	// caja creates an attendee
	rw := e.caja(t, "POST", "/api/attendees", map[string]any{"display_name": "CajaUser"})
	if rw.Code != http.StatusOK {
		t.Fatalf("caja create: got %d body: %s", rw.Code, rw.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// caja tops up
	rw = e.caja(t, "POST", "/api/attendees/"+a.ID+"/topup", map[string]any{"amount": 100})
	if rw.Code != http.StatusOK {
		t.Fatalf("caja topup: got %d body: %s", rw.Code, rw.Body.String())
	}
}

// TestHTTPLedger_AfterTopup verifies the ledger grows after a subsequent topup.
func TestHTTPLedger_AfterTopup(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	// Create with topup then do another topup.
	rw := e.admin(t, "POST", "/api/attendees", map[string]any{
		"display_name":  "LedgerGrow",
		"initial_topup": 1000,
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("create: got %d", rw.Code)
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rw = e.admin(t, "POST", "/api/attendees/"+a.ID+"/topup", map[string]any{"amount": 500})
	if rw.Code != http.StatusOK {
		t.Fatalf("topup: got %d", rw.Code)
	}

	rw = e.admin(t, "GET", "/api/attendees/"+a.ID+"/ledger", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("ledger: got %d", rw.Code)
	}
	var entries []LedgerEntry
	if err := json.NewDecoder(rw.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("ledger after topup: got %d entries, want 2", len(entries))
	}
}

// ---------------------------------------------------------------------------
// Branch coverage: scanAttendee phone field + ledger nullable ref IDs
// ---------------------------------------------------------------------------

// TestHTTPCreate_WithPhone creates an attendee that has the optional phone field
// set, then retrieves it through GET /api/attendees/{id}. This covers the
// phone.Valid branch inside scanAttendee (source line 124-125).
func TestHTTPCreate_WithPhone(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	rw := e.admin(t, "POST", "/api/attendees", map[string]any{
		"display_name": "PhoneUser",
		"phone":        "+5491112345678",
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("create with phone: got %d body: %s", rw.Code, rw.Body.String())
	}
	var a Attendee
	if err := json.NewDecoder(rw.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// GET the attendee through the route so scanAttendee runs with a non-null phone.
	rw = e.admin(t, "GET", "/api/attendees/"+a.ID, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("get with phone: got %d body: %s", rw.Code, rw.Body.String())
	}
	var got Attendee
	if err := json.NewDecoder(rw.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Phone == nil || *got.Phone != "+5491112345678" {
		t.Errorf("get: phone = %v, want '+5491112345678'", got.Phone)
	}
}

// TestHTTPLedger_WithRefIDs inserts a balance_ledger row that has both
// ref_order_id and ref_payment_id set (non-NULL), then fetches the ledger.
// This covers the refOrderID.Valid and refPaymentID.Valid branches in the
// ledger handler (source lines 460-461 and 463-464).
func TestHTTPLedger_WithRefIDs(t *testing.T) {
	e := newCoverEnv(t)
	seedActiveEvent(t, e.database)

	// Create an attendee through the normal API.
	a := e.createAttendeeViaHTTP(t, "RefIDTarget")

	// Insert a ledger row directly with non-null ref_order_id and ref_payment_id.
	// The handler reads these as sql.NullString; providing real values exercises
	// the Valid branches.
	if _, err := e.database.Writer.ExecContext(context.Background(),
		`INSERT INTO balance_ledger(id, attendee_id, type, amount, balance_after,
		  ref_order_id, ref_payment_id, user_id, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		core.NewID(), a.ID, "debit", -500, 500,
		"order-ref-abc", "pay-ref-xyz", "testuser", core.NowMillis(),
	); err != nil {
		t.Fatalf("insert ledger row with ref IDs: %v", err)
	}

	rw := e.admin(t, "GET", "/api/attendees/"+a.ID+"/ledger", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("ledger with ref IDs: got %d body: %s", rw.Code, rw.Body.String())
	}
	var entries []LedgerEntry
	if err := json.NewDecoder(rw.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("ledger with ref IDs: expected at least 1 entry, got 0")
	}
	// Find the entry we inserted.
	var found bool
	for _, en := range entries {
		if en.RefOrderID != nil && *en.RefOrderID == "order-ref-abc" {
			found = true
			if en.RefPaymentID == nil || *en.RefPaymentID != "pay-ref-xyz" {
				t.Errorf("ref_payment_id: got %v, want 'pay-ref-xyz'", en.RefPaymentID)
			}
		}
	}
	if !found {
		t.Error("ledger: entry with ref_order_id 'order-ref-abc' not found")
	}
}

// TestHTTPGet_DBError drops the attendees table before issuing GET
// /api/attendees/{id}. Auth succeeds (sessions/users table intact), but the
// handler's QueryRowContext returns a non-ErrNoRows error, exercising the
// unexpected-error branch (source lines 406-408) and returning HTTP 500.
func TestHTTPGet_DBError(t *testing.T) {
	e := newCoverEnv(t)
	// Do NOT seed an active event or create attendees — the table gets dropped.

	// Drop the attendees table so the handler's SELECT fails.
	if _, err := e.database.Writer.ExecContext(context.Background(),
		`DROP TABLE IF EXISTS attendees`); err != nil {
		t.Fatalf("drop attendees: %v", err)
	}

	rw := e.admin(t, "GET", "/api/attendees/anyfakeid", nil)
	if rw.Code != http.StatusInternalServerError {
		t.Errorf("get with dropped table: want 500, got %d body: %s", rw.Code, rw.Body.String())
	}
}
