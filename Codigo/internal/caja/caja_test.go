package caja

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// openTestDB spins up a real on-disk SQLite (WAL) in a temp dir with the full schema.
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

// buildDeps wires the writer, app, and auth service for testing.
// Cleanup order: writer is stopped before the DB is closed (LIFO).
func buildDeps(t *testing.T, d *db.DB) server.Deps {
	t.Helper()
	w := writer.New(d.Writer)
	t.Cleanup(w.Close)
	return server.Deps{
		App:  &app.App{DB: d, Writer: w},
		Auth: auth.New(d),
	}
}

// seedEvent inserts an active event and returns its ID.
func seedEvent(t *testing.T, d *db.DB) string {
	t.Helper()
	id := core.NewID()
	_, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?, 'Test Event', 'active', 50000, 'cascade', ?)`,
		id, core.NowMillis())
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

// seedUser inserts a user with the caja role and returns its ID.
// The pin_hash is a placeholder because tests bypass the login flow entirely.
func seedUser(t *testing.T, d *db.DB) string {
	t.Helper()
	id := core.NewID()
	_, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?, 'cajero1', 'caja', 'placeholder', 1, ?)`,
		id, core.NowMillis())
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedAuthToken inserts a pre-hashed session row for userID and returns the raw
// token string to use as the aeos_session cookie value.
// auth.Service stores sha256(rawToken) as the session ID, so we replicate that
// here instead of going through the (argon2id-slow) Login path.
func seedAuthToken(t *testing.T, d *db.DB, userID string) string {
	t.Helper()
	const rawToken = "test-raw-token-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	sum := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(sum[:])
	now := core.NowMillis()
	_, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at) VALUES (?,?,?,?,?)`,
		tokenHash, userID, "127.0.0.1", now, now+86400000)
	if err != nil {
		t.Fatalf("seed auth token: %v", err)
	}
	return rawToken
}

// newReq builds an HTTP test request with the session cookie attached.
// body may be "" for GET requests.
func newReq(method, path, body, token string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: "aeos_session", Value: token})
	return r
}

// TestCajaOpenCloseVariance is the primary integration test for the caja module.
// It exercises the full open → conflict → close lifecycle and verifies that the
// variance maths are correct.
func TestCajaOpenCloseVariance(t *testing.T) {
	d := openTestDB(t)
	deps := buildDeps(t, d)
	mod := New(deps)

	mux := http.NewServeMux()
	mod.Register(mux, deps)

	_ = seedEvent(t, d)
	userID := seedUser(t, d)
	token := seedAuthToken(t, d, userID)

	// --- open: should succeed and return status=open ---
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newReq("POST", "/api/caja/open", `{"opening_float":50000}`, token))
	if rr.Code != http.StatusOK {
		t.Fatalf("open: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var opened Session
	if err := json.NewDecoder(rr.Body).Decode(&opened); err != nil {
		t.Fatalf("decode open response: %v", err)
	}
	if opened.Status != "open" {
		t.Errorf("open status: got %q, want %q", opened.Status, "open")
	}
	if opened.OpeningFloat != 50000 {
		t.Errorf("opening_float: got %d, want 50000", opened.OpeningFloat)
	}

	// --- second open for the same operator must fail with 409 Conflict ---
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, newReq("POST", "/api/caja/open", `{"opening_float":50000}`, token))
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second open: got %d, want 409: %s", rr2.Code, rr2.Body.String())
	}

	// --- current: should return the open session ---
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, newReq("GET", "/api/caja/current", "", token))
	if rr3.Code != http.StatusOK {
		t.Fatalf("current: got %d, want 200: %s", rr3.Code, rr3.Body.String())
	}
	var current Session
	if err := json.NewDecoder(rr3.Body).Decode(&current); err != nil {
		t.Fatalf("decode current response: %v", err)
	}
	if current.ID != opened.ID {
		t.Errorf("current.ID: got %q, want %q", current.ID, opened.ID)
	}

	// --- close with counted_cash=60000 (no cash payments, so expected=50000) ---
	// expected_cash = opening_float + SUM(cash payments) = 50000 + 0 = 50000
	// variance      = counted_cash - expected_cash        = 60000 - 50000 = 10000
	rr4 := httptest.NewRecorder()
	mux.ServeHTTP(rr4, newReq("POST", "/api/caja/close", `{"counted_cash":60000}`, token))
	if rr4.Code != http.StatusOK {
		t.Fatalf("close: got %d, want 200: %s", rr4.Code, rr4.Body.String())
	}
	var closed Session
	if err := json.NewDecoder(rr4.Body).Decode(&closed); err != nil {
		t.Fatalf("decode close response: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("closed status: got %q, want %q", closed.Status, "closed")
	}
	if closed.ExpectedCash == nil || *closed.ExpectedCash != 50000 {
		t.Errorf("expected_cash: got %v, want 50000", closed.ExpectedCash)
	}
	if closed.CountedCash == nil || *closed.CountedCash != 60000 {
		t.Errorf("counted_cash: got %v, want 60000", closed.CountedCash)
	}
	// variance = 60000 - 50000 = 10000 (operator counted more than expected → surplus)
	if closed.Variance == nil || *closed.Variance != 10000 {
		t.Errorf("variance: got %v, want 10000", closed.Variance)
	}
	if closed.ClosedAt == nil {
		t.Error("closed_at should be set after close")
	}
}
