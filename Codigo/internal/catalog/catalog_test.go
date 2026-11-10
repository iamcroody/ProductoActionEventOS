package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// openTestDB spins up a real on-disk SQLite (WAL) in a temp dir with the full schema applied.
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

// buildDeps wires a minimal server.Deps from a test DB and a writer.
// The caller owns w and must call w.Close() after the test.
func buildDeps(d *db.DB, w *writer.Writer) server.Deps {
	a := &app.App{DB: d, Writer: w}
	return server.Deps{App: a, Auth: auth.New(d)}
}

// seedSession inserts an admin user and a live session, returning the raw
// cookie value that auth.Middleware will accept.
func seedSession(t *testing.T, d *db.DB, eventID string) string {
	t.Helper()
	ctx := context.Background()
	now := core.NowMillis()

	userID := core.NewID()
	// pin_hash only matters for Login; session lookup does not re-verify it.
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, role, pin_hash, active, created_at)
		 VALUES (?,?,?,?,1,?)`,
		userID, "testadmin", "admin", "argon2id$dummysalt$dummykey", now); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rawToken := "catalog-test-token-abc123"
	sum := sha256.Sum256([]byte(rawToken))
	sessionHash := hex.EncodeToString(sum[:])
	exp := now + 24*3600*1000 // +24 h in millis

	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at)
		 VALUES (?,?,?,?,?)`,
		sessionHash, userID, "127.0.0.1", now, exp); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return rawToken
}

// authedReq builds an HTTP request carrying the session cookie.
func authedReq(method, target, body, rawToken string) *http.Request {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, bodyReader)
	req.AddCookie(&http.Cookie{Name: "aeos_session", Value: rawToken})
	return req
}

// TestInventoryAdjust exercises the happy-path stock adjustment and the
// negative-stock guard via real SQLite + the full HTTP handler stack.
func TestInventoryAdjust(t *testing.T) {
	d := openTestDB(t)
	w := writer.New(d.Writer)
	defer w.Close()

	deps := buildDeps(d, w)
	mod := New(deps)
	mux := http.NewServeMux()
	mod.Register(mux, deps)

	ctx := context.Background()
	now := core.NowMillis()

	// Seed active event.
	eventID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Fiesta Test", "draft", 50000, "cascade", now); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	rawToken := seedSession(t, d, eventID)

	// Seed a tracked product with initial stock_qty=10.
	productID := core.NewID()
	if _, err := d.Writer.ExecContext(ctx,
		`INSERT INTO products(id, event_id, name, price, prep_station,
		  track_stock, stock_qty, stock_version, sort_order, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		productID, eventID, "Cerveza", 5000, "bar", 1, 10, 0, 0, now); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	// --- Case 1: valid restock +5, expect stock_qty to become 15 ---
	body1 := `{"product_id":"` + productID + `","delta":5,"reason":"restock"}`
	req1 := authedReq(http.MethodPost, "/api/inventory/adjust", body1, rawToken)
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("restock: expected 200, got %d; body: %s", rr1.Code, rr1.Body.String())
	}

	var qty int64
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT stock_qty FROM products WHERE id=?`, productID).Scan(&qty); err != nil {
		t.Fatalf("read qty after restock: %v", err)
	}
	if qty != 15 {
		t.Fatalf("restock: expected stock_qty=15, got %d", qty)
	}

	// Confirm the movement row was written.
	var movCount int
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND delta=5 AND reason='restock'`,
		productID).Scan(&movCount); err != nil {
		t.Fatalf("read movements: %v", err)
	}
	if movCount != 1 {
		t.Fatalf("restock movement: expected 1 row, got %d", movCount)
	}

	// --- Case 2: delta that would produce negative stock → must be rejected ---
	// Current qty is 15; delta -20 would yield -5.
	body2 := `{"product_id":"` + productID + `","delta":-20,"reason":"waste"}`
	req2 := authedReq(http.MethodPost, "/api/inventory/adjust", body2, rawToken)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("negative stock: expected 400, got %d; body: %s", rr2.Code, rr2.Body.String())
	}

	// Stock must be unchanged after the rejected attempt.
	if err := d.Reader.QueryRowContext(ctx,
		`SELECT stock_qty FROM products WHERE id=?`, productID).Scan(&qty); err != nil {
		t.Fatalf("read qty after rejection: %v", err)
	}
	if qty != 15 {
		t.Fatalf("rejected adjust mutated stock: expected 15, got %d", qty)
	}

	// --- Case 3: exact-drain to zero is allowed ---
	body3 := `{"product_id":"` + productID + `","delta":-15,"reason":"adjustment"}`
	req3 := authedReq(http.MethodPost, "/api/inventory/adjust", body3, rawToken)
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("drain to zero: expected 200, got %d; body: %s", rr3.Code, rr3.Body.String())
	}

	if err := d.Reader.QueryRowContext(ctx,
		`SELECT stock_qty FROM products WHERE id=?`, productID).Scan(&qty); err != nil {
		t.Fatalf("read qty after drain: %v", err)
	}
	if qty != 0 {
		t.Fatalf("drain to zero: expected stock_qty=0, got %d", qty)
	}
}
