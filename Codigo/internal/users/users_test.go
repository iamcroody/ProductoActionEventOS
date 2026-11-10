package users

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/db"
	"eventpos/internal/server"
	"eventpos/internal/writer"
)

// openTestDB spins up a real on-disk SQLite database in a temp directory,
// applying the full production schema via db.Open.
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

// testDeps builds a minimal server.Deps wired to the supplied database.
// Hub and Backup are left nil because no users handler uses them.
func testDeps(d *db.DB) (server.Deps, *writer.Writer) {
	w := writer.New(d.Writer)
	deps := server.Deps{
		App: &app.App{
			DB:     d,
			Writer: w,
		},
		Auth: auth.New(d),
	}
	return deps, w
}

// jsonReq builds a POST/PUT request with a JSON body.
func jsonReq(method, path string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestCreateOperator creates a caja operator, verifies the response shape,
// asserts no credential hashes are returned, and confirms the stored PIN
// can be verified via auth.VerifySecret.
func TestCreateOperator(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()

	m := New(deps)

	// Create a caja operator with a 4-digit PIN.
	req := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username":  "cajero1",
		"full_name": "Cajero Uno",
		"role":      "caja",
		"pin":       "1234",
	})
	rr := httptest.NewRecorder()
	m.create(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("create: got status %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// Decode the returned User struct.
	var u User
	if err := json.Unmarshal(rr.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if u.ID == "" {
		t.Fatal("id missing in response")
	}
	if u.Username != "cajero1" {
		t.Fatalf("username: got %q, want cajero1", u.Username)
	}
	if u.Role != "caja" {
		t.Fatalf("role: got %q, want caja", u.Role)
	}
	if u.Active != 1 {
		t.Fatalf("active: got %d, want 1", u.Active)
	}

	// Assert no credential hashes appear in the raw JSON response.
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["pin_hash"]; ok {
		t.Fatal("pin_hash must not be returned in the response")
	}
	if _, ok := raw["password_hash"]; ok {
		t.Fatal("password_hash must not be returned in the response")
	}

	// Read the stored pin_hash directly from the database and verify it
	// matches the original plaintext PIN via auth.VerifySecret.
	var pinHash string
	err := d.Reader.QueryRowContext(context.Background(),
		`SELECT pin_hash FROM users WHERE id = ?`, u.ID).Scan(&pinHash)
	if err != nil {
		t.Fatalf("read pin_hash: %v", err)
	}
	if !auth.VerifySecret("1234", pinHash) {
		t.Fatal("stored pin_hash does not verify against the original PIN")
	}
}

// TestDuplicateUsername asserts that creating two operators with the same
// username returns 409 Conflict.
func TestDuplicateUsername(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()

	m := New(deps)

	first := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "cajero1",
		"role":     "caja",
		"pin":      "1234",
	})
	rr := httptest.NewRecorder()
	m.create(rr, first)
	if rr.Code != http.StatusOK {
		t.Fatalf("first create: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// Second create with the same username must fail with 409.
	second := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "cajero1",
		"role":     "mesero",
		"pin":      "5678",
	})
	rr2 := httptest.NewRecorder()
	m.create(rr2, second)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("duplicate username: got %d, want 409; body: %s", rr2.Code, rr2.Body.String())
	}
}

// TestInvalidRole asserts that supplying an unknown role returns 400.
func TestInvalidRole(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()

	m := New(deps)
	req := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "hacker",
		"role":     "superadmin",
		"pin":      "1234",
	})
	rr := httptest.NewRecorder()
	m.create(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid role: got %d, want 400", rr.Code)
	}
}

// TestInvalidPin asserts that a non-digit or out-of-range PIN returns 400.
func TestInvalidPin(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()

	m := New(deps)
	for _, pin := range []string{"123", "1234567", "abcd", ""} {
		req := jsonReq(http.MethodPost, "/api/users", map[string]any{
			"username": "op1",
			"role":     "mesero",
			"pin":      pin,
		})
		rr := httptest.NewRecorder()
		m.create(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("pin %q: got %d, want 400", pin, rr.Code)
		}
	}
}

// TestSoftDeleteLastAdmin asserts that removing the sole active admin
// is rejected with 400.
func TestSoftDeleteLastAdmin(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()

	m := New(deps)

	// Create the only admin.
	req := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "admin1",
		"role":     "admin",
		"pin":      "1234",
	})
	rr := httptest.NewRecorder()
	m.create(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create admin: %d – %s", rr.Code, rr.Body.String())
	}
	var u User
	_ = json.Unmarshal(rr.Body.Bytes(), &u)

	// Attempting to delete the last active admin must return 400.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/users/"+u.ID, nil)
	delReq.SetPathValue("id", u.ID)
	rr2 := httptest.NewRecorder()
	m.softDelete(rr2, delReq)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("last admin delete: got %d, want 400; body: %s", rr2.Code, rr2.Body.String())
	}
}
