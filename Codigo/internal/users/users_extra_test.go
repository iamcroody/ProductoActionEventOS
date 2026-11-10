package users

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"eventpos/internal/auth"
)

// ---------------------------------------------------------------------------
// PIN validation boundary tests
// ---------------------------------------------------------------------------

// TestPINLengthBoundaries exercises every edge around the 4–6 digit rule:
//   - 3 digits → 400
//   - 4 digits → 200 (minimum valid)
//   - 5 digits → 200
//   - 6 digits → 200 (maximum valid)
//   - 7 digits → 400
//   - non-digits → 400
func TestPINLengthBoundaries(t *testing.T) {
	cases := []struct {
		pin      string
		wantCode int
	}{
		{"123", http.StatusBadRequest},
		{"1234", http.StatusOK},
		{"12345", http.StatusOK},
		{"123456", http.StatusOK},
		{"1234567", http.StatusBadRequest},
		{"abcd", http.StatusBadRequest},
		{"12ab", http.StatusBadRequest},
		{"", http.StatusBadRequest},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("pin="+tc.pin, func(t *testing.T) {
			d := openTestDB(t)
			deps, w := testDeps(d)
			defer w.Close()
			m := New(deps)

			req := jsonReq(http.MethodPost, "/api/users", map[string]any{
				"username": "pintest-" + tc.pin + "-u",
				"role":     "mesero",
				"pin":      tc.pin,
			})
			rr := httptest.NewRecorder()
			m.create(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("pin %q: got %d, want %d; body: %s",
					tc.pin, rr.Code, tc.wantCode, rr.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PIN reset
// ---------------------------------------------------------------------------

// TestPINResetRehashes verifies that after a PIN reset:
//   - the new PIN verifies against the stored hash, and
//   - the old PIN no longer verifies.
func TestPINResetRehashes(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()
	m := New(deps)

	// Create a user with PIN "1111".
	createReq := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "pinreset-user",
		"role":     "mesero",
		"pin":      "1111",
	})
	rr := httptest.NewRecorder()
	m.create(rr, createReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: got %d; body: %s", rr.Code, rr.Body.String())
	}
	var u User
	if err := json.Unmarshal(rr.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Read the original pin_hash.
	var oldHash string
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT pin_hash FROM users WHERE id=?`, u.ID).Scan(&oldHash); err != nil {
		t.Fatalf("read old hash: %v", err)
	}
	if !auth.VerifySecret("1111", oldHash) {
		t.Fatal("sanity: old PIN must verify before reset")
	}

	// Reset PIN to "9999".
	resetReq := jsonReq(http.MethodPost, "/api/users/"+u.ID+"/pin", map[string]any{
		"pin": "9999",
	})
	resetReq.SetPathValue("id", u.ID)
	rr2 := httptest.NewRecorder()
	m.resetPin(rr2, resetReq)
	if rr2.Code != http.StatusOK {
		t.Fatalf("resetPin: got %d; body: %s", rr2.Code, rr2.Body.String())
	}

	// Read the new pin_hash.
	var newHash string
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT pin_hash FROM users WHERE id=?`, u.ID).Scan(&newHash); err != nil {
		t.Fatalf("read new hash: %v", err)
	}

	if newHash == oldHash {
		t.Fatal("pin_hash was not updated after reset")
	}
	if !auth.VerifySecret("9999", newHash) {
		t.Fatal("new PIN must verify against the new hash")
	}
	if auth.VerifySecret("1111", newHash) {
		t.Fatal("old PIN must NOT verify against the new hash")
	}
}

// ---------------------------------------------------------------------------
// Soft-delete: last admin protection and non-last admin deletion
// ---------------------------------------------------------------------------

// TestSoftDeleteNonLastAdmin verifies that a non-last admin CAN be soft-deleted.
func TestSoftDeleteNonLastAdmin(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()
	m := New(deps)

	// Create two admins.
	var adminIDs [2]string
	for i, name := range []string{"admin-a", "admin-b"} {
		req := jsonReq(http.MethodPost, "/api/users", map[string]any{
			"username": name,
			"role":     "admin",
			"pin":      "1234",
		})
		rr := httptest.NewRecorder()
		m.create(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("create %s: got %d; %s", name, rr.Code, rr.Body.String())
		}
		var u User
		if err := json.Unmarshal(rr.Body.Bytes(), &u); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		adminIDs[i] = u.ID
	}

	// Soft-delete admin-a (not the last admin) — must succeed (200).
	delReq := httptest.NewRequest(http.MethodDelete, "/api/users/"+adminIDs[0], nil)
	delReq.SetPathValue("id", adminIDs[0])
	rr := httptest.NewRecorder()
	m.softDelete(rr, delReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete non-last admin: got %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// Verify active=0 in the database.
	var active int
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT active FROM users WHERE id=?`, adminIDs[0]).Scan(&active); err != nil {
		t.Fatalf("read active: %v", err)
	}
	if active != 0 {
		t.Fatalf("deleted admin: active=%d, want 0", active)
	}

	// admin-b must still be active.
	var activeB int
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT active FROM users WHERE id=?`, adminIDs[1]).Scan(&activeB); err != nil {
		t.Fatalf("read activeB: %v", err)
	}
	if activeB != 1 {
		t.Fatalf("remaining admin: active=%d, want 1", activeB)
	}

	// Now trying to delete admin-b (now the last active admin) must return 400.
	delReq2 := httptest.NewRequest(http.MethodDelete, "/api/users/"+adminIDs[1], nil)
	delReq2.SetPathValue("id", adminIDs[1])
	rr2 := httptest.NewRecorder()
	m.softDelete(rr2, delReq2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("last admin delete: got %d, want 400", rr2.Code)
	}
}

// ---------------------------------------------------------------------------
// No credential hashes in any JSON response
// ---------------------------------------------------------------------------

// TestNoHashInListResponse verifies that the list endpoint never returns
// pin_hash or password_hash even when the DB contains them.
func TestNoHashInListResponse(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()
	m := New(deps)

	// Create a user with both PIN and password.
	req := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "nohash-user",
		"role":     "caja",
		"pin":      "4321",
		"password": "supersecret",
	})
	rr := httptest.NewRecorder()
	m.create(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: got %d; %s", rr.Code, rr.Body.String())
	}

	// create response must not contain hashes.
	var createRaw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &createRaw); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	for _, field := range []string{"pin_hash", "password_hash"} {
		if _, ok := createRaw[field]; ok {
			t.Errorf("create response contains %q", field)
		}
	}

	// list response must not contain hashes.
	listReq := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	rr2 := httptest.NewRecorder()
	m.list(rr2, listReq)
	if rr2.Code != http.StatusOK {
		t.Fatalf("list: got %d", rr2.Code)
	}
	var listRaw []map[string]any
	if err := json.Unmarshal(rr2.Body.Bytes(), &listRaw); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listRaw) == 0 {
		t.Fatal("list: expected at least one user")
	}
	for i, u := range listRaw {
		for _, field := range []string{"pin_hash", "password_hash"} {
			if _, ok := u[field]; ok {
				t.Errorf("list[%d] contains %q", i, field)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Update role validation
// ---------------------------------------------------------------------------

// TestUpdateInvalidRole asserts that updating a user to an unrecognised role
// is rejected with 400 Bad Request.
func TestUpdateInvalidRole(t *testing.T) {
	d := openTestDB(t)
	deps, w := testDeps(d)
	defer w.Close()
	m := New(deps)

	// Create a user to update.
	req := jsonReq(http.MethodPost, "/api/users", map[string]any{
		"username": "role-update-user",
		"role":     "mesero",
		"pin":      "5678",
	})
	rr := httptest.NewRecorder()
	m.create(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: got %d; %s", rr.Code, rr.Body.String())
	}
	var u User
	if err := json.Unmarshal(rr.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	for _, badRole := range []string{"superadmin", "root", "guest", "", "ADMIN"} {
		badRole := badRole
		t.Run("role="+badRole, func(t *testing.T) {
			upReq := jsonReq(http.MethodPut, "/api/users/"+u.ID, map[string]any{
				"full_name": "Test",
				"role":      badRole,
				"active":    1,
			})
			upReq.SetPathValue("id", u.ID)
			rr2 := httptest.NewRecorder()
			m.update(rr2, upReq)
			if rr2.Code != http.StatusBadRequest {
				t.Fatalf("role %q: got %d, want 400; body: %s", badRole, rr2.Code, rr2.Body.String())
			}
		})
	}

	// Valid roles must be accepted.
	for _, goodRole := range []string{"admin", "caja", "mesero", "cocina", "bar", "auditor"} {
		goodRole := goodRole
		t.Run("role="+goodRole, func(t *testing.T) {
			upReq := jsonReq(http.MethodPut, "/api/users/"+u.ID, map[string]any{
				"full_name": "Test",
				"role":      goodRole,
				"active":    1,
			})
			upReq.SetPathValue("id", u.ID)
			rr2 := httptest.NewRecorder()
			m.update(rr2, upReq)
			if rr2.Code != http.StatusOK {
				t.Fatalf("valid role %q: got %d, want 200; body: %s", goodRole, rr2.Code, rr2.Body.String())
			}
		})
	}
}
