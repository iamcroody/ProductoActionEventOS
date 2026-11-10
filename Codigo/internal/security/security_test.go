// Package security is an exhaustive authorization / data-isolation / injection
// suite for Action EventOS. It boots the fully-wired server in-process via
// httptest with the exact nine-module set from cmd/eventpos (the same harness
// the apitest integration suite uses) and drives everything over real HTTP.
//
// It focuses on the security perimeter rather than business happy-paths:
//
//   - A full endpoint x role authorization matrix (admin, caja, mesero, cocina,
//     bar, auditor + unauthenticated) asserting the expected 401 / 403 / passes.
//   - Explicit mutation-denial: non-privileged roles cannot mutate catalog,
//     inventory, users, events, config or backups.
//   - Data scoping: a second event's attendees and orders must not leak through
//     attendee search or reports.
//   - Session security: forged, edited, revoked and expired session cookies are
//     all rejected with 401.
//   - Injection safety: attendee / product names containing quotes, semicolons,
//     "DROP TABLE", emoji and 10k-character strings are stored verbatim
//     (parameterized) and break nothing.
package security

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/backup"
	"eventpos/internal/bootstrap"
	"eventpos/internal/caja"
	"eventpos/internal/catalog"
	"eventpos/internal/config"
	"eventpos/internal/customers"
	"eventpos/internal/db"
	"eventpos/internal/events"
	"eventpos/internal/kitchen"
	"eventpos/internal/oversight"
	"eventpos/internal/realtime"
	"eventpos/internal/reports"
	"eventpos/internal/sales"
	"eventpos/internal/server"
	"eventpos/internal/users"
	"eventpos/internal/writer"
)

// ---------------------------------------------------------------------------
// Harness: a fully-wired in-process server with one cookie-jar client per role
// plus an unauthenticated client. The DB handle is retained so session-security
// tests can directly manipulate the sessions table.
// ---------------------------------------------------------------------------

const anyAuth = "ANY" // sentinel: route allows any authenticated user.

// allRoles is every operator role the system recognises.
var allRoles = []string{"admin", "caja", "mesero", "cocina", "bar", "auditor"}

type harness struct {
	ts      *httptest.Server
	d       *db.DB
	clients map[string]*http.Client // role -> logged-in client; "anon" -> no cookie
	tokens  map[string]string       // role -> raw session token
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	d, err := db.Open(filepath.Join(t.TempDir(), "sec.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	w := writer.New(d.Writer)
	t.Cleanup(w.Close)

	hub := realtime.NewHub(64)
	cfg := config.New(d)
	if err := cfg.EnsureDefaults(ctx); err != nil {
		t.Fatalf("ensure defaults: %v", err)
	}
	backupSvc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	authSvc := auth.New(d)

	application := &app.App{DB: d, Writer: w, Hub: hub, DataDir: t.TempDir()}

	if _, _, err := bootstrap.SeedAdmin(ctx, d); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	deps := server.Deps{App: application, Auth: authSvc, Config: cfg, Backup: backupSvc}
	mods := []server.Module{
		users.New(deps), events.New(deps), catalog.New(deps), customers.New(deps),
		caja.New(deps), sales.New(deps), kitchen.New(deps), reports.New(deps),
		oversight.New(deps),
	}

	srv := server.New(deps, fstest.MapFS{}, mods...)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{
		ts:      ts,
		d:       d,
		clients: map[string]*http.Client{},
		tokens:  map[string]string{},
	}

	// admin (the bootstrap-seeded operator).
	h.clients["admin"] = newClient(t)
	h.tokens["admin"] = h.login(t, "admin", bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)

	// One operator per remaining role, created by the admin.
	for _, role := range allRoles {
		if role == "admin" {
			continue
		}
		username := role + "_op"
		code, body := h.do(t, "admin", "POST", "/api/users", map[string]any{
			"username": username, "full_name": "Op " + role, "role": role, "pin": "1111",
		})
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("create %s user: status %d, body %s", role, code, body)
		}
		h.clients[role] = newClient(t)
		h.tokens[role] = h.login(t, role, username, "1111")
	}

	// Unauthenticated client (no cookie jar).
	h.clients["anon"] = &http.Client{}

	return h
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// login authenticates the named role's client and returns the raw session token
// (so it can be hashed and located in the sessions table).
func (h *harness) login(t *testing.T, role, username, secret string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"username": username, "secret": secret})
	req, err := http.NewRequest("POST", h.ts.URL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.clients[role].Do(req)
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: status %d, body %s", username, resp.StatusCode, data)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "aeos_session" {
			return c.Value
		}
	}
	t.Fatalf("login %s: no session cookie returned", username)
	return ""
}

// do issues a request via the named role's client and returns status + body.
func (h *harness) do(t *testing.T, role, method, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.clients[role].Do(req)
	if err != nil {
		t.Fatalf("do %s %s (%s): %v", method, path, role, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// mustOK fails unless the response is 200/201, decoding into out when non-nil.
func (h *harness) mustOK(t *testing.T, role, method, path string, body, out any) {
	t.Helper()
	code, data := h.do(t, role, method, path, body)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("%s %s (%s): status %d, body %s", method, path, role, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s decode: %v (body %s)", method, path, err, data)
		}
	}
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Wire shapes (only fields assertions read).
// ---------------------------------------------------------------------------

type idResp struct {
	ID string `json:"id"`
}

type attendee struct {
	ID          string `json:"id"`
	QRToken     string `json:"qr_token"`
	DisplayName string `json:"display_name"`
	Balance     int64  `json:"balance"`
}

type product struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type orderResp struct {
	ID    string `json:"id"`
	Total int64  `json:"total"`
}

// ---------------------------------------------------------------------------
// The endpoint catalog. allowed==nil/[ANY] means "any authenticated user".
// Paths with {fake} ids are fine: the auth middleware runs first, so an allowed
// role passes the gate (then gets 400/404), which is exactly what we assert.
// ---------------------------------------------------------------------------

type endpoint struct {
	method  string
	path    string
	allowed []string // empty/ANY => any authenticated user
}

func endpoints() []endpoint {
	return []endpoint{
		// users (admin only)
		{"GET", "/api/users", []string{"admin"}},
		{"POST", "/api/users", []string{"admin"}},
		{"PUT", "/api/users/fakeid", []string{"admin"}},
		{"POST", "/api/users/fakeid/pin", []string{"admin"}},
		{"DELETE", "/api/users/fakeid", []string{"admin"}},

		// events
		{"GET", "/api/events", []string{anyAuth}},
		{"GET", "/api/events/active", []string{anyAuth}},
		{"POST", "/api/events", []string{"admin"}},
		{"POST", "/api/events/fakeid/activate", []string{"admin"}},
		{"POST", "/api/events/fakeid/close", []string{"admin"}},
		{"PUT", "/api/events/fakeid", []string{"admin"}},

		// catalog
		{"GET", "/api/categories", []string{anyAuth}},
		{"POST", "/api/categories", []string{"admin"}},
		{"PUT", "/api/categories/fakeid", []string{"admin"}},
		{"GET", "/api/products", []string{anyAuth}},
		{"POST", "/api/products", []string{"admin"}},
		{"PUT", "/api/products/fakeid", []string{"admin"}},
		{"DELETE", "/api/products/fakeid", []string{"admin"}},
		{"POST", "/api/inventory/adjust", []string{"admin", "caja"}},
		{"GET", "/api/inventory/movements", []string{anyAuth}},
		{"GET", "/api/inventory/low-stock", []string{anyAuth}},

		// customers
		{"POST", "/api/attendees", []string{"admin", "caja"}},
		{"POST", "/api/attendees/import", []string{"admin", "caja"}},
		{"GET", "/api/attendees", []string{anyAuth}},
		{"GET", "/api/qr/faketoken", []string{anyAuth}},
		{"GET", "/api/attendees/fakeid/ledger", []string{anyAuth}},
		{"GET", "/api/attendees/fakeid", []string{anyAuth}},
		{"POST", "/api/attendees/fakeid/topup", []string{"admin", "caja"}},
		{"POST", "/api/attendees/fakeid/block", []string{"admin"}},
		{"POST", "/api/attendees/fakeid/unblock", []string{"admin"}},

		// caja
		{"POST", "/api/caja/open", []string{"admin", "caja"}},
		{"GET", "/api/caja/current", []string{"admin", "caja"}},
		{"POST", "/api/caja/close", []string{"admin", "caja"}},
		{"GET", "/api/caja/sessions", []string{"admin", "caja"}},

		// sales
		{"POST", "/api/orders", []string{"admin", "caja", "mesero"}},
		{"GET", "/api/orders", []string{"admin", "caja", "auditor"}},
		{"GET", "/api/orders/fakeid", []string{anyAuth}},
		{"POST", "/api/orders/fakeid/void", []string{"admin"}},

		// pendientes (entregas)
		{"GET", "/api/pendientes", []string{anyAuth}},
		{"POST", "/api/pendientes/items/fakeid/entregar", []string{"admin", "caja", "mesero", "cocina", "bar"}},
		{"POST", "/api/pendientes/items/fakeid/entregar-todo", []string{"admin", "caja", "mesero", "cocina", "bar"}},
		{"GET", "/api/pendientes/history", []string{anyAuth}},

		// reports
		{"GET", "/api/reports/dashboard", []string{"admin", "caja", "auditor"}},
		{"GET", "/api/reports/tips", []string{"admin", "caja", "auditor"}},
		{"GET", "/api/reports/sales", []string{"admin", "caja", "auditor"}},
		{"GET", "/api/reports/export.csv", []string{"admin", "caja", "auditor"}},
		{"GET", "/api/reports/export.json", []string{"admin", "caja", "auditor"}},

		// oversight
		{"GET", "/api/audit", []string{"admin", "auditor"}},
		{"GET", "/api/audit/verify", []string{"admin", "auditor"}},
		{"GET", "/api/audit/export.csv", []string{"admin", "auditor"}},
		{"GET", "/api/integrity", []string{"admin", "auditor"}},

		// server core
		{"GET", "/api/auth/me", []string{anyAuth}},
		{"GET", "/api/config", []string{anyAuth}},
		{"PUT", "/api/config", []string{"admin"}},
		{"GET", "/api/backups", []string{"admin"}},
		{"POST", "/api/backups", []string{"admin"}},
		{"POST", "/api/backups/verify", []string{"admin"}},
	}
}

func roleAllowed(role string, allowed []string) bool {
	if role == "admin" {
		return true // middleware always lets admin through.
	}
	for _, a := range allowed {
		if a == anyAuth || a == role {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1. Full endpoint x role authorization matrix.
// ---------------------------------------------------------------------------

func TestAuthorizationMatrix(t *testing.T) {
	h := newHarness(t)

	for _, ep := range endpoints() {
		ep := ep
		// Unauthenticated: every middleware-guarded route returns 401.
		t.Run("anon "+ep.method+" "+ep.path, func(t *testing.T) {
			code, body := h.do(t, "anon", ep.method, ep.path, nil)
			if code != http.StatusUnauthorized {
				t.Fatalf("anon %s %s: status %d, want 401 (body %s)",
					ep.method, ep.path, code, body)
			}
		})

		for _, role := range allRoles {
			role := role
			t.Run(role+" "+ep.method+" "+ep.path, func(t *testing.T) {
				code, body := h.do(t, role, ep.method, ep.path, nil)
				if roleAllowed(role, ep.allowed) {
					// Passed the authz gate: never 401/403. (Business
					// validation may still yield 400/404/409/200.)
					if code == http.StatusUnauthorized || code == http.StatusForbidden {
						t.Fatalf("%s %s (%s): status %d, want the authz gate to pass (body %s)",
							ep.method, ep.path, role, code, body)
					}
				} else {
					// Disallowed authenticated role: must be 403.
					if code != http.StatusForbidden {
						t.Fatalf("%s %s (%s): status %d, want 403 (body %s)",
							ep.method, ep.path, role, code, body)
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 2. /api/health is public; everything else demands a session.
// ---------------------------------------------------------------------------

func TestPublicHealthEndpoint(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(t, "anon", "GET", "/api/health", nil)
	if code != http.StatusOK {
		t.Fatalf("anon health: status %d, want 200 (body %s)", code, body)
	}
}

// ---------------------------------------------------------------------------
// 3. Mutation denial: meseros/cocina/bar/auditor cannot mutate privileged
//    resources. Any non-403 here is a HIGH-severity privilege escalation.
// ---------------------------------------------------------------------------

func TestNonPrivilegedCannotMutate(t *testing.T) {
	h := newHarness(t)

	type mut struct{ method, path, what string }
	mutations := []mut{
		{"POST", "/api/products", "catalog: create product"},
		{"PUT", "/api/products/fakeid", "catalog: update product"},
		{"DELETE", "/api/products/fakeid", "catalog: delete product"},
		{"POST", "/api/categories", "catalog: create category"},
		{"PUT", "/api/categories/fakeid", "catalog: update category"},
		{"POST", "/api/inventory/adjust", "inventory: adjust stock"},
		{"POST", "/api/users", "users: create"},
		{"PUT", "/api/users/fakeid", "users: update"},
		{"DELETE", "/api/users/fakeid", "users: delete"},
		{"POST", "/api/users/fakeid/pin", "users: reset pin"},
		{"POST", "/api/events", "events: create"},
		{"POST", "/api/events/fakeid/activate", "events: activate"},
		{"POST", "/api/events/fakeid/close", "events: close"},
		{"PUT", "/api/events/fakeid", "events: update"},
		{"PUT", "/api/config", "config: set"},
		{"GET", "/api/backups", "backups: list"},
		{"POST", "/api/backups", "backups: create"},
		{"POST", "/api/backups/verify", "backups: verify"},
		{"POST", "/api/attendees/fakeid/block", "customers: block"},
		{"POST", "/api/attendees/fakeid/unblock", "customers: unblock"},
		{"POST", "/api/orders/fakeid/void", "sales: void"},
	}

	for _, role := range []string{"mesero", "cocina", "bar", "auditor"} {
		for _, m := range mutations {
			role, m := role, m
			t.Run(role+" "+m.path, func(t *testing.T) {
				// Use a non-empty body so we genuinely reach (and are rejected
				// by) the authz layer rather than a body-decode short-circuit.
				code, body := h.do(t, role, m.method, m.path, map[string]any{"x": 1})
				if code != http.StatusForbidden {
					t.Fatalf("PRIVILEGE ESCALATION: %s allowed to %s (%s %s): status %d, want 403 (body %s)",
						role, m.what, m.method, m.path, code, body)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Positive authorization: allowed roles actually get through to 200 on
//    representative read endpoints (gate passes AND the handler succeeds).
// ---------------------------------------------------------------------------

func TestAllowedRolesReachData(t *testing.T) {
	h := newHarness(t)
	// Establish an active event so the dashboard etc. have something to read.
	h.activeEvent(t)

	// Any authenticated role can read events, config and their own identity.
	for _, role := range allRoles {
		h.mustOK(t, role, "GET", "/api/events", nil, nil)
		h.mustOK(t, role, "GET", "/api/config", nil, nil)
		h.mustOK(t, role, "GET", "/api/auth/me", nil, nil)
		h.mustOK(t, role, "GET", "/api/products", nil, nil)
	}

	// Reports are readable by admin, caja, auditor.
	for _, role := range []string{"admin", "caja", "auditor"} {
		h.mustOK(t, role, "GET", "/api/reports/dashboard", nil, nil)
	}

	// Oversight is readable by admin, auditor.
	for _, role := range []string{"admin", "auditor"} {
		h.mustOK(t, role, "GET", "/api/integrity", nil, nil)
	}

	// Pendientes board is readable by any authenticated operator.
	for _, role := range []string{"admin", "cocina", "bar", "mesero", "caja", "auditor"} {
		h.mustOK(t, role, "GET", "/api/pendientes", nil, nil)
	}
}

// ---------------------------------------------------------------------------
// 5. Data scoping: a second event's attendees and orders must not leak through
//    attendee search or reports.
// ---------------------------------------------------------------------------

func TestEventDataIsolation(t *testing.T) {
	h := newHarness(t)

	// The lock-down lifecycle allows only ONE event in the draft-or-active state
	// at a time, so the two events are run strictly sequentially: configure A
	// (draft) → activate → sell → close, then configure B (draft) → activate →
	// sell. Reports remain scoped by ?event_id= so the isolation assertions hold
	// against both the (now-closed) event A and the active event B.

	// --- Event A: configure while draft, activate, sell, then close. ---
	evA := h.draftEvent(t)
	catA := h.createCategory(t, "BebidasA")
	prodA := h.createProduct(t, catA, "CervezaA", 8000)
	attA := h.createAttendee(t, "ZebraAlphaScopedA", 50000)
	h.activate(t, evA)
	h.mustOK(t, "admin", "POST", "/api/orders", map[string]any{
		"request_id": "scope-A-1",
		"items":      []map[string]any{{"product_id": prodA.ID, "qty": 1}},
		"payers":     []map[string]any{{"attendee_id": attA.ID}},
	}, nil)
	// Close A so a second event can be created and configured.
	h.closeEvent(t, evA)

	// --- Event B: configure while draft, activate, sell. ---
	evB := h.draftEvent(t)
	catB := h.createCategory(t, "BebidasB")
	prodB := h.createProduct(t, catB, "CervezaB", 5000)
	attB := h.createAttendee(t, "ZebraBetaScopedB", 50000)
	h.activate(t, evB)
	h.mustOK(t, "admin", "POST", "/api/orders", map[string]any{
		"request_id": "scope-B-1",
		"items":      []map[string]any{{"product_id": prodB.ID, "qty": 2}},
		"payers":     []map[string]any{{"attendee_id": attB.ID}},
	}, nil)

	// Attendee search is scoped to the active event (B): A's attendee must NOT
	// appear, B's must.
	var foundA []attendee
	h.mustOK(t, "admin", "GET", "/api/attendees?q=ZebraAlphaScoped", nil, &foundA)
	if len(foundA) != 0 {
		t.Fatalf("DATA LEAK: event A attendee surfaced via search while event B active: %+v", foundA)
	}
	var foundB []attendee
	h.mustOK(t, "admin", "GET", "/api/attendees?q=ZebraBetaScoped", nil, &foundB)
	if len(foundB) != 1 {
		t.Fatalf("event B attendee not found in own-event search: got %d", len(foundB))
	}

	// Auditor (a read-only role) must see the same scoping, not a wider view.
	var auditorView []attendee
	h.mustOK(t, "auditor", "GET", "/api/attendees?q=ZebraAlphaScoped", nil, &auditorView)
	if len(auditorView) != 0 {
		t.Fatalf("DATA LEAK: auditor saw event A attendee via search during event B: %+v", auditorView)
	}

	// Reports are scoped per event_id and must not cross-contaminate.
	dashTotal := func(eventID string) int64 {
		var dash struct {
			TotalSold int64 `json:"total_sold"`
		}
		h.mustOK(t, "admin", "GET", "/api/reports/dashboard?event_id="+eventID, nil, &dash)
		return dash.TotalSold
	}
	if got := dashTotal(evA); got != 8000 {
		t.Fatalf("event A dashboard total_sold: got %d, want 8000 (B's 10000 must not leak)", got)
	}
	if got := dashTotal(evB); got != 10000 {
		t.Fatalf("event B dashboard total_sold: got %d, want 10000 (A's 8000 must not leak)", got)
	}
}

// ---------------------------------------------------------------------------
// 6. Session security: forged, edited, revoked and expired cookies -> 401.
// ---------------------------------------------------------------------------

func TestSessionSecurity(t *testing.T) {
	h := newHarness(t)

	// Helper: hit /api/auth/me with an explicit raw cookie value on a fresh,
	// jar-less client, returning the status.
	probe := func(cookieVal string) int {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/auth/me", nil)
		if cookieVal != "" {
			req.AddCookie(&http.Cookie{Name: "aeos_session", Value: cookieVal})
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Sanity: a valid token is accepted.
	if code := probe(h.tokens["admin"]); code != http.StatusOK {
		t.Fatalf("valid admin token rejected: status %d", code)
	}

	t.Run("forged", func(t *testing.T) {
		if code := probe("totally-made-up-token-value"); code != http.StatusUnauthorized {
			t.Fatalf("forged cookie: status %d, want 401", code)
		}
	})

	t.Run("edited", func(t *testing.T) {
		tok := []byte(h.tokens["admin"])
		// Flip the last character to a different valid base64url rune.
		last := tok[len(tok)-1]
		if last == 'A' {
			tok[len(tok)-1] = 'B'
		} else {
			tok[len(tok)-1] = 'A'
		}
		if code := probe(string(tok)); code != http.StatusUnauthorized {
			t.Fatalf("edited cookie: status %d, want 401", code)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		// A throwaway caja session we will log out (revoke) explicitly.
		c := newClient(t)
		h.clients["throwaway"] = c
		tok := h.login(t, "throwaway", "caja_op", "1111")
		if code := probe(tok); code != http.StatusOK {
			t.Fatalf("fresh throwaway token rejected: status %d", code)
		}
		// Logout revokes the server-side session.
		h.mustOK(t, "throwaway", "POST", "/api/auth/logout", nil, nil)
		if code := probe(tok); code != http.StatusUnauthorized {
			t.Fatalf("revoked cookie: status %d, want 401", code)
		}
	})

	t.Run("expired", func(t *testing.T) {
		c := newClient(t)
		h.clients["expiring"] = c
		tok := h.login(t, "expiring", "mesero_op", "1111")
		if code := probe(tok); code != http.StatusOK {
			t.Fatalf("fresh expiring token rejected: status %d", code)
		}
		// Force the session to have already expired.
		if _, err := h.d.Writer.ExecContext(context.Background(),
			`UPDATE sessions SET expires_at = 1 WHERE id = ?`, hashToken(tok)); err != nil {
			t.Fatalf("expire session: %v", err)
		}
		if code := probe(tok); code != http.StatusUnauthorized {
			t.Fatalf("expired cookie: status %d, want 401", code)
		}
	})
}

// ---------------------------------------------------------------------------
// 7. Injection safety: nasty names are stored verbatim (parameterized queries)
//    and never corrupt the database.
// ---------------------------------------------------------------------------

func TestInjectionSafety(t *testing.T) {
	h := newHarness(t)
	// Injection payloads enter via attendee/product creation, which is
	// configuration: keep the event in draft so those endpoints are reachable.
	h.draftEvent(t)
	cat := h.createCategory(t, "Inyecciones")

	payloads := map[string]string{
		"sqli_drop":     "Robert'); DROP TABLE attendees;--",
		"sqli_delete":   "x'; DELETE FROM users; --",
		"sqli_union":    "' UNION SELECT pin_hash FROM users --",
		"quotes_mixed":  `O'Brien said "hello"; weird` + "`backtick`",
		"emoji":         "Fiesta 🍺🎉😀🥳 Cerveza",
		"unicode":       "Iñtërnâtiônàližætiøn ñ ü",
		"html":          "<script>alert('xss')</script>",
		"semicolons":    ";;;;; SELECT * FROM sessions ;;;;;",
		"long_10k":      strings.Repeat("A", 10000),
		"newlines_tabs": "line1\nline2\tcol\r\nend",
		"backslashes":   `C:slash path no-escape`,
		"json_breakers": `{"injected":true},"x":[`,
	}

	for label, name := range payloads {
		label, name := label, name
		t.Run("attendee/"+label, func(t *testing.T) {
			var created attendee
			h.mustOK(t, "admin", "POST", "/api/attendees",
				map[string]any{"display_name": name, "initial_topup": 1000}, &created)
			if created.DisplayName != name {
				t.Fatalf("attendee name mangled on create: got %q, want %q", created.DisplayName, name)
			}
			// Round-trip through a fresh GET to confirm verbatim persistence.
			var fetched attendee
			h.mustOK(t, "admin", "GET", "/api/attendees/"+created.ID, nil, &fetched)
			if fetched.DisplayName != name {
				t.Fatalf("attendee name mangled on read: got %q, want %q", fetched.DisplayName, name)
			}
		})

		t.Run("product/"+label, func(t *testing.T) {
			var created product
			h.mustOK(t, "admin", "POST", "/api/products", map[string]any{
				"category_id": cat, "name": name, "price": 1000, "cost": 500,
				"prep_station": "bar", "track_stock": 0, "stock_qty": 0,
			}, &created)
			if created.Name != name {
				t.Fatalf("product name mangled on create: got %q, want %q", created.Name, name)
			}
		})
	}

	// The database survived every payload: a canary attendee still works and the
	// audit chain / integrity check still pass (no tables dropped, chain intact).
	canary := h.createAttendee(t, "CanaryStillAlive", 2000)
	if canary.Balance != 2000 {
		t.Fatalf("canary attendee balance: got %d, want 2000", canary.Balance)
	}
	var found []attendee
	h.mustOK(t, "admin", "GET", "/api/attendees?q=CanaryStillAlive", nil, &found)
	if len(found) != 1 {
		t.Fatalf("canary not searchable after injection attempts: got %d", len(found))
	}
	var integ struct {
		OK bool `json:"ok"`
	}
	h.mustOK(t, "admin", "GET", "/api/integrity", nil, &integ)
	if !integ.OK {
		t.Fatalf("integrity not ok after injection attempts: %+v", integ)
	}

	// The users table is intact and admin login still works (no DROP / DELETE
	// took effect): a brand-new client can authenticate as admin.
	fresh := newClient(t)
	h.clients["postinj"] = fresh
	_ = h.login(t, "postinj", bootstrap.DefaultAdminUser, bootstrap.DefaultAdminSecret)
}

// ---------------------------------------------------------------------------
// Builder helpers (admin-driven). Mirrors the apitest harness conventions.
// ---------------------------------------------------------------------------

// draftEvent creates an event (which starts in 'draft' status) and returns its
// id. Under the lock-down lifecycle, configuration (catalog, attendees, top-ups)
// is only allowed while draft; call activate once setup is complete.
func (h *harness) draftEvent(t *testing.T) string {
	t.Helper()
	var ev idResp
	h.mustOK(t, "admin", "POST", "/api/events", map[string]any{
		"name": "Fiesta", "default_topup": 50000, "split_policy": "cascade",
	}, &ev)
	return ev.ID
}

// activate promotes a draft event to 'active' so selling/reports are allowed.
func (h *harness) activate(t *testing.T, id string) {
	t.Helper()
	h.mustOK(t, "admin", "POST", "/api/events/"+id+"/activate", map[string]any{}, nil)
}

// closeEvent closes an event, freeing the single-current-event slot so another
// event can be created and configured.
func (h *harness) closeEvent(t *testing.T, id string) {
	t.Helper()
	h.mustOK(t, "admin", "POST", "/api/events/"+id+"/close", map[string]any{}, nil)
}

// activeEvent creates a draft event and immediately activates it, returning its
// id. Use only when no configuration is needed after activation.
func (h *harness) activeEvent(t *testing.T) string {
	t.Helper()
	id := h.draftEvent(t)
	h.activate(t, id)
	return id
}

func (h *harness) createCategory(t *testing.T, name string) string {
	t.Helper()
	var c idResp
	h.mustOK(t, "admin", "POST", "/api/categories", map[string]any{"name": name, "sort_order": 0}, &c)
	return c.ID
}

func (h *harness) createProduct(t *testing.T, categoryID, name string, price int64) product {
	t.Helper()
	var p product
	h.mustOK(t, "admin", "POST", "/api/products", map[string]any{
		"category_id": categoryID, "name": name, "price": price, "cost": 0,
		"prep_station": "bar", "track_stock": 1, "stock_qty": 100,
	}, &p)
	return p
}

func (h *harness) createAttendee(t *testing.T, name string, topup int64) attendee {
	t.Helper()
	var a attendee
	h.mustOK(t, "admin", "POST", "/api/attendees",
		map[string]any{"display_name": name, "initial_topup": topup}, &a)
	return a
}

var _ = orderResp{} // reserved wire shape; keep import surface stable.
