package events_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"eventpos/internal/events"
	"eventpos/internal/realtime"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// hashToken replicates the unexported auth.hashToken so we can seed sessions
// directly into the DB without going through the real login endpoint.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// testEnv holds the full infrastructure needed for an events handler test.
type testEnv struct {
	database *db.DB
	deps     server.Deps
	mux      *http.ServeMux
	rawToken string // the raw session token to set as a cookie
}

// newTestEnv opens a fresh temp-db, seeds an admin user + session, registers
// the events module routes, and returns a testEnv ready to drive requests.
func newTestEnv(t *testing.T) *testEnv {
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

	// Seed an admin user directly (no argon2id needed – we only need the hash).
	userID := core.NewID()
	pinHash, err := auth.HashSecret("testpin")
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,?,?)`,
		userID, "testadmin", "admin", pinHash, 1, core.NowMillis()); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Seed a session for the admin that expires in 1 hour.
	const rawToken = "events-test-raw-token-deadbeef1234"
	expAt := time.Now().Add(time.Hour).UTC().UnixMilli()
	if _, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at)
		 VALUES (?,?,?,?,?)`,
		hashToken(rawToken), userID, "127.0.0.1", core.NowMillis(), expAt); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	mux := http.NewServeMux()
	m := events.New(deps)
	m.Register(mux, deps)

	return &testEnv{database: d, deps: deps, mux: mux, rawToken: rawToken}
}

// do fires an authenticated HTTP request through the mux and returns the recorder.
// body may be any JSON-serialisable value (or nil for bodyless requests).
func (e *testEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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
	req.AddCookie(&http.Cookie{Name: "aeos_session", Value: e.rawToken})

	rw := httptest.NewRecorder()
	e.mux.ServeHTTP(rw, req)
	return rw
}

// decodeJSON decodes the response body into v, failing the test on error.
func decodeJSON(t *testing.T, rw *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rw.Body).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

// createEvent posts to /api/events and returns the created Event.
func (e *testEnv) createEvent(t *testing.T, name string) events.Event {
	t.Helper()
	rw := e.do(t, "POST", "/api/events", map[string]any{"name": name})
	if rw.Code != http.StatusOK {
		t.Fatalf("createEvent %q: got %d, body: %s", name, rw.Code, rw.Body.String())
	}
	var ev events.Event
	decodeJSON(t, rw, &ev)
	return ev
}

// activateEvent posts to /api/events/{id}/activate.
func (e *testEnv) activateEvent(t *testing.T, id string) {
	t.Helper()
	rw := e.do(t, "POST", "/api/events/"+id+"/activate", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("activateEvent %q: got %d, body: %s", id, rw.Code, rw.Body.String())
	}
}

// closeEvent posts to /api/events/{id}/close.
func (e *testEnv) closeEvent(t *testing.T, id string) {
	t.Helper()
	rw := e.do(t, "POST", "/api/events/"+id+"/close", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("closeEvent %q: got %d, body: %s", id, rw.Code, rw.Body.String())
	}
}

// dbStatus reads the status column of an event directly from the DB.
func (e *testEnv) dbStatus(t *testing.T, eventID string) string {
	t.Helper()
	var status string
	if err := e.database.Reader.QueryRowContext(context.Background(),
		`SELECT status FROM events WHERE id = ?`, eventID).Scan(&status); err != nil {
		t.Fatalf("dbStatus %q: %v", eventID, err)
	}
	return status
}

// auditCount returns the number of audit_log rows for a given action + entity_id.
func (e *testEnv) auditCount(t *testing.T, action, entityID string) int {
	t.Helper()
	var n int
	if err := e.database.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE action = ? AND entity_id = ?`,
		action, entityID).Scan(&n); err != nil {
		t.Fatalf("auditCount %q/%q: %v", action, entityID, err)
	}
	return n
}

// activeCount returns the number of events currently in 'active' status.
func (e *testEnv) activeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.database.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE status = 'active'`).Scan(&n); err != nil {
		t.Fatalf("activeCount: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Test cases
// ---------------------------------------------------------------------------

// TestCreateEvent_AppearsInListAsDraft creates an event and verifies it appears
// in GET /api/events with status 'draft', default_topup 50000, and split_policy
// 'cascade' (the system defaults when those fields are not supplied).
func TestCreateEvent_AppearsInListAsDraft(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", map[string]any{"name": "Music Fest 2025"})
	if rw.Code != http.StatusOK {
		t.Fatalf("create: got %d, body: %s", rw.Code, rw.Body.String())
	}

	var created events.Event
	decodeJSON(t, rw, &created)

	if created.ID == "" {
		t.Error("created.ID must not be empty")
	}
	if created.Name != "Music Fest 2025" {
		t.Errorf("name = %q, want 'Music Fest 2025'", created.Name)
	}
	if created.Status != "draft" {
		t.Errorf("status = %q, want 'draft'", created.Status)
	}
	if created.DefaultTopup != 50000 {
		t.Errorf("default_topup = %d, want 50000", created.DefaultTopup)
	}
	if created.SplitPolicy != "cascade" {
		t.Errorf("split_policy = %q, want 'cascade'", created.SplitPolicy)
	}

	// Must also appear in the list endpoint.
	listRw := e.do(t, "GET", "/api/events", nil)
	if listRw.Code != http.StatusOK {
		t.Fatalf("list: got %d", listRw.Code)
	}
	var list []events.Event
	decodeJSON(t, listRw, &list)

	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if list[0].ID != created.ID {
		t.Errorf("list[0].id = %q, want %q", list[0].ID, created.ID)
	}
	if list[0].Status != "draft" {
		t.Errorf("list[0].status = %q, want 'draft'", list[0].Status)
	}
	if list[0].DefaultTopup != 50000 {
		t.Errorf("list[0].default_topup = %d, want 50000", list[0].DefaultTopup)
	}
	if list[0].SplitPolicy != "cascade" {
		t.Errorf("list[0].split_policy = %q, want 'cascade'", list[0].SplitPolicy)
	}
}

// TestCreateEvent_DefaultTopupZeroBecomesDefault verifies that a caller-supplied
// default_topup of 0 (≤ 0) is replaced by the system default of 50 000.
func TestCreateEvent_DefaultTopupZeroBecomesDefault(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", map[string]any{
		"name":          "Zero Topup Event",
		"default_topup": 0,
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rw.Code, rw.Body.String())
	}
	var ev events.Event
	decodeJSON(t, rw, &ev)
	if ev.DefaultTopup != 50000 {
		t.Errorf("default_topup = %d, want 50000", ev.DefaultTopup)
	}
}

// TestCreateEvent_DefaultTopupNegativeBecomesDefault verifies that a negative
// default_topup is replaced by the system default.
func TestCreateEvent_DefaultTopupNegativeBecomesDefault(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", map[string]any{
		"name":          "Negative Topup Event",
		"default_topup": -1000,
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rw.Code, rw.Body.String())
	}
	var ev events.Event
	decodeJSON(t, rw, &ev)
	if ev.DefaultTopup != 50000 {
		t.Errorf("default_topup = %d, want 50000", ev.DefaultTopup)
	}
}

// TestCreateEvent_SplitPolicyCascadeDefault verifies that an unrecognised
// split_policy is silently replaced with 'cascade'.
func TestCreateEvent_SplitPolicyCascadeDefault(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", map[string]any{
		"name":         "Bad Policy Event",
		"split_policy": "unknown_policy",
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rw.Code, rw.Body.String())
	}
	var ev events.Event
	decodeJSON(t, rw, &ev)
	if ev.SplitPolicy != "cascade" {
		t.Errorf("split_policy = %q, want 'cascade'", ev.SplitPolicy)
	}
}

// TestCreateEvent_SplitPolicyStrictEqualPreserved verifies that 'strict_equal'
// is kept as-is.
func TestCreateEvent_SplitPolicyStrictEqualPreserved(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", map[string]any{
		"name":         "Strict Equal Event",
		"split_policy": "strict_equal",
	})
	if rw.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rw.Code, rw.Body.String())
	}
	var ev events.Event
	decodeJSON(t, rw, &ev)
	if ev.SplitPolicy != "strict_equal" {
		t.Errorf("split_policy = %q, want 'strict_equal'", ev.SplitPolicy)
	}
}

// TestActivateEvent verifies that activating a draft event sets its status to
// 'active', that GET /api/events/active returns it, and that
// events.ActiveEvent returns it.
func TestActivateEvent(t *testing.T) {
	e := newTestEnv(t)

	ev := e.createEvent(t, "Activation Test")
	if ev.Status != "draft" {
		t.Fatalf("initial status = %q, want 'draft'", ev.Status)
	}

	e.activateEvent(t, ev.ID)

	// DB-level check.
	if got := e.dbStatus(t, ev.ID); got != "active" {
		t.Errorf("db status = %q, want 'active'", got)
	}

	// GET /api/events/active must return this event.
	activeRw := e.do(t, "GET", "/api/events/active", nil)
	if activeRw.Code != http.StatusOK {
		t.Fatalf("GET /api/events/active: got %d, body: %s", activeRw.Code, activeRw.Body.String())
	}
	var active events.Event
	decodeJSON(t, activeRw, &active)
	if active.ID != ev.ID {
		t.Errorf("active.id = %q, want %q", active.ID, ev.ID)
	}
	if active.Status != "active" {
		t.Errorf("active.status = %q, want 'active'", active.Status)
	}

	// events.ActiveEvent exported helper must also return it.
	got, err := events.ActiveEvent(context.Background(), e.deps)
	if err != nil {
		t.Fatalf("events.ActiveEvent: %v", err)
	}
	if got.ID != ev.ID {
		t.Errorf("ActiveEvent().ID = %q, want %q", got.ID, ev.ID)
	}
}

// TestOneActiveInvariant verifies the one-active constraint enforced by the
// partial unique index. When a second event is activated the first must become
// 'closed', and exactly one row may have status='active' in the DB.
func TestOneActiveInvariant(t *testing.T) {
	e := newTestEnv(t)

	evA := e.createEvent(t, "Event A")
	evB := e.createEvent(t, "Event B")

	// Activate A.
	e.activateEvent(t, evA.ID)
	if got := e.dbStatus(t, evA.ID); got != "active" {
		t.Fatalf("A.status = %q after first activate, want 'active'", got)
	}

	// Activate B — must close A automatically.
	e.activateEvent(t, evB.ID)

	statusA := e.dbStatus(t, evA.ID)
	statusB := e.dbStatus(t, evB.ID)

	if statusA != "closed" {
		t.Errorf("A.status = %q, want 'closed' after B activated", statusA)
	}
	if statusB != "active" {
		t.Errorf("B.status = %q, want 'active'", statusB)
	}

	// The partial unique index allows at most 1 active row; verify via count.
	if n := e.activeCount(t); n != 1 {
		t.Errorf("active count = %d, want 1", n)
	}

	// events.ActiveEvent must resolve to B.
	active, err := events.ActiveEvent(context.Background(), e.deps)
	if err != nil {
		t.Fatalf("events.ActiveEvent: %v", err)
	}
	if active.ID != evB.ID {
		t.Errorf("ActiveEvent().ID = %q, want B (%q)", active.ID, evB.ID)
	}
}

// TestOneActiveInvariant_ThirdEvent extends the invariant test across three
// sequential activations to confirm the last-in-wins rule is robust.
func TestOneActiveInvariant_ThirdEvent(t *testing.T) {
	e := newTestEnv(t)

	evA := e.createEvent(t, "Event A")
	evB := e.createEvent(t, "Event B")
	evC := e.createEvent(t, "Event C")

	e.activateEvent(t, evA.ID)
	e.activateEvent(t, evB.ID)
	e.activateEvent(t, evC.ID)

	if n := e.activeCount(t); n != 1 {
		t.Errorf("active count = %d, want 1 after three activations", n)
	}

	active, err := events.ActiveEvent(context.Background(), e.deps)
	if err != nil {
		t.Fatalf("events.ActiveEvent: %v", err)
	}
	if active.ID != evC.ID {
		t.Errorf("ActiveEvent().ID = %q, want C (%q)", active.ID, evC.ID)
	}

	if got := e.dbStatus(t, evA.ID); got != "closed" {
		t.Errorf("A.status = %q, want 'closed'", got)
	}
	if got := e.dbStatus(t, evB.ID); got != "closed" {
		t.Errorf("B.status = %q, want 'closed'", got)
	}
}

// TestCloseEvent verifies that closing an active event transitions it to
// 'closed', and that ActiveEvent subsequently returns ErrNoActiveEvent
// (both via the exported helper and via GET /api/events/active → 400).
func TestCloseEvent(t *testing.T) {
	e := newTestEnv(t)

	ev := e.createEvent(t, "Close Test Event")
	e.activateEvent(t, ev.ID)

	if got := e.dbStatus(t, ev.ID); got != "active" {
		t.Fatalf("status before close = %q, want 'active'", got)
	}

	e.closeEvent(t, ev.ID)

	if got := e.dbStatus(t, ev.ID); got != "closed" {
		t.Errorf("status after close = %q, want 'closed'", got)
	}

	// events.ActiveEvent must return ErrNoActiveEvent.
	_, err := events.ActiveEvent(context.Background(), e.deps)
	if !errors.Is(err, core.ErrNoActiveEvent) {
		t.Errorf("ActiveEvent err = %v, want core.ErrNoActiveEvent", err)
	}

	// The HTTP endpoint must map ErrNoActiveEvent → 400 Bad Request.
	activeRw := e.do(t, "GET", "/api/events/active", nil)
	if activeRw.Code != http.StatusBadRequest {
		t.Errorf("GET /api/events/active after close: got %d, want 400", activeRw.Code)
	}
}

// TestAuditLog_Create verifies that creating an event writes exactly one
// 'event_create' row to audit_log with the event's ID as entity_id.
func TestAuditLog_Create(t *testing.T) {
	e := newTestEnv(t)

	ev := e.createEvent(t, "Audit Create Event")

	if n := e.auditCount(t, "event_create", ev.ID); n != 1 {
		t.Errorf("event_create audit rows = %d, want 1", n)
	}
}

// TestAuditLog_Activate verifies that activating an event writes an
// 'event_activate' audit row.
func TestAuditLog_Activate(t *testing.T) {
	e := newTestEnv(t)

	ev := e.createEvent(t, "Audit Activate Event")
	e.activateEvent(t, ev.ID)

	if n := e.auditCount(t, "event_activate", ev.ID); n != 1 {
		t.Errorf("event_activate audit rows = %d, want 1", n)
	}
}

// TestAuditLog_Close verifies that closing an event writes an 'event_close'
// audit row.
func TestAuditLog_Close(t *testing.T) {
	e := newTestEnv(t)

	ev := e.createEvent(t, "Audit Close Event")
	e.activateEvent(t, ev.ID)
	e.closeEvent(t, ev.ID)

	if n := e.auditCount(t, "event_close", ev.ID); n != 1 {
		t.Errorf("event_close audit rows = %d, want 1", n)
	}
}

// TestAuditLog_FullLifecycle verifies that the create → activate → close
// lifecycle produces exactly one audit row for each of the three actions on
// the same entity.
func TestAuditLog_FullLifecycle(t *testing.T) {
	e := newTestEnv(t)

	ev := e.createEvent(t, "Full Lifecycle Audit")
	e.activateEvent(t, ev.ID)
	e.closeEvent(t, ev.ID)

	for _, action := range []string{"event_create", "event_activate", "event_close"} {
		if n := e.auditCount(t, action, ev.ID); n != 1 {
			t.Errorf("audit action %q: count = %d, want 1", action, n)
		}
	}
}

// TestValidation_EmptyName verifies that POST /api/events with an empty name
// is rejected with HTTP 400 and that the response body carries the
// ErrValidation message. No event must be persisted.
func TestValidation_EmptyName(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", map[string]any{"name": ""})
	if rw.Code != http.StatusBadRequest {
		t.Errorf("empty name: got %d, want 400", rw.Code)
	}

	var resp map[string]string
	decodeJSON(t, rw, &resp)
	if errMsg := resp["error"]; errMsg != core.ErrValidation.Error() {
		t.Errorf("error = %q, want %q", errMsg, core.ErrValidation.Error())
	}

	// No event must have been created.
	listRw := e.do(t, "GET", "/api/events", nil)
	if listRw.Code != http.StatusOK {
		t.Fatalf("list after failed create: got %d", listRw.Code)
	}
	var list []events.Event
	decodeJSON(t, listRw, &list)
	if len(list) != 0 {
		t.Errorf("expected 0 events after rejected create, got %d", len(list))
	}
}

// TestValidation_NoBody verifies that a POST /api/events with no body (which
// produces an EOF decode error) is also rejected with 400.
func TestValidation_NoBody(t *testing.T) {
	e := newTestEnv(t)

	rw := e.do(t, "POST", "/api/events", nil)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("no body: got %d, want 400", rw.Code)
	}
}

// TestActiveEvent_NoneInitially verifies that ActiveEvent returns
// ErrNoActiveEvent on a fresh database (no active event yet).
func TestActiveEvent_NoneInitially(t *testing.T) {
	e := newTestEnv(t)

	_, err := events.ActiveEvent(context.Background(), e.deps)
	if !errors.Is(err, core.ErrNoActiveEvent) {
		t.Errorf("expected ErrNoActiveEvent on empty db, got: %v", err)
	}
}

// TestListEvents_MultipleEvents verifies that the list returns all events in
// creation order (newest first per ORDER BY created_at DESC).
func TestListEvents_MultipleEvents(t *testing.T) {
	e := newTestEnv(t)

	evA := e.createEvent(t, "First Event")
	evB := e.createEvent(t, "Second Event")

	listRw := e.do(t, "GET", "/api/events", nil)
	if listRw.Code != http.StatusOK {
		t.Fatalf("list: got %d", listRw.Code)
	}
	var list []events.Event
	decodeJSON(t, listRw, &list)

	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	// Newest first — evB should be list[0].
	if list[0].ID != evB.ID {
		t.Errorf("list[0].id = %q, want evB (%q)", list[0].ID, evB.ID)
	}
	if list[1].ID != evA.ID {
		t.Errorf("list[1].id = %q, want evA (%q)", list[1].ID, evA.ID)
	}
}

// TestUnauthenticated verifies that requests without a valid session are
// rejected with 401 Unauthorized.
func TestUnauthenticated(t *testing.T) {
	e := newTestEnv(t)

	// Fire a list request with no cookie at all.
	req := httptest.NewRequest("GET", "/api/events", nil)
	rw := httptest.NewRecorder()
	e.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated list: got %d, want 401", rw.Code)
	}
}
