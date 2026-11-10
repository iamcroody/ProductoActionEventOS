package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"eventpos/internal/core"
	"eventpos/internal/db"
)

// openTestDB spins up a real on-disk SQLite (WAL) database in a temp dir and
// registers cleanup. Mirrors the pattern used in writer_test.go.
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

// seedUser inserts a user row with a freshly hashed PIN and returns the user ID.
func seedUser(t *testing.T, d *db.DB, username, pin, role string, active int) string {
	t.Helper()
	id := core.NewID()
	h, err := HashSecret(pin)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	_, err = d.Writer.ExecContext(context.Background(),
		`INSERT INTO users(id, username, full_name, role, pin_hash, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, username, "Test User", role, h, active, core.NowMillis())
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	return id
}

// requestWithCookie builds an httptest.Request carrying the named session cookie.
func requestWithCookie(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	return r
}

// isErrForbidden reports whether err is (or wraps) core.ErrForbidden.
func isForbidden(err error) bool {
	return errors.Is(err, core.ErrForbidden)
}

// ---------------------------------------------------------------------------
// HashSecret / VerifySecret
// ---------------------------------------------------------------------------

func TestHashSecret_CorrectSecretVerifies(t *testing.T) {
	secret := "1234"
	hash, err := HashSecret(secret)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if !VerifySecret(secret, hash) {
		t.Error("VerifySecret: correct secret should verify against its own hash")
	}
}

func TestHashSecret_WrongSecretRejected(t *testing.T) {
	hash, err := HashSecret("correct")
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if VerifySecret("wrong", hash) {
		t.Error("VerifySecret: wrong secret must not verify")
	}
}

func TestHashSecret_RandomSalt_TwoHashesDiffer(t *testing.T) {
	secret := "same-secret"
	h1, err := HashSecret(secret)
	if err != nil {
		t.Fatalf("first HashSecret: %v", err)
	}
	h2, err := HashSecret(secret)
	if err != nil {
		t.Fatalf("second HashSecret: %v", err)
	}
	if h1 == h2 {
		t.Error("two hashes of the same secret must differ (random salt)")
	}
	// Both hashes must still verify correctly.
	if !VerifySecret(secret, h1) {
		t.Error("h1 should verify")
	}
	if !VerifySecret(secret, h2) {
		t.Error("h2 should verify")
	}
}

func TestVerifySecret_MalformedEncoded_ReturnsFalse(t *testing.T) {
	// Note: "argon2id$$" (empty key) is omitted here because it exposes a real
	// bug — see TestVerifySecret_BugEmptyKeyPanics below.
	cases := []struct {
		name    string
		encoded string
	}{
		{"empty string", ""},
		{"no separator", "notencoded"},
		{"one field only", "argon2id"},
		{"two fields only", "argon2id$abc"},
		{"wrong algorithm prefix", "bcrypt$abc$def"},
		{"invalid base64 in salt", "argon2id$!!!$abc"},
		{"invalid base64 in key", "argon2id$abc$!!!"},
		{"extra fields", "argon2id$abc$def$ghi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if VerifySecret("anything", tc.encoded) {
				t.Errorf("VerifySecret(%q) should return false for malformed input", tc.encoded)
			}
		})
	}
}

// TestVerifySecret_BugEmptyKeyPanics documents a real bug in VerifySecret:
// when the encoded string has the form "argon2id$$" both the salt and key
// base64 fields decode without error but produce nil/empty byte slices.
// VerifySecret then calls argon2.IDKey with keyLen=0, which panics with a
// nil-pointer dereference inside the argon2 library.
//
// Bug:    VerifySecret does not validate len(want) > 0 before calling argon2.IDKey.
// Cause:  base64.RawStdEncoding.DecodeString("") returns (nil, nil); len(nil)==0;
//
//	argon2.IDKey(..., uint32(0)) panics inside blake2bHash.
//
// Fix:    In VerifySecret, after decoding want, add:
//
//	if len(want) == 0 || len(salt) == 0 { return false }
func TestVerifySecret_BugEmptyKeyPanics(t *testing.T) {
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		VerifySecret("anything", "argon2id$$")
	}()
	if panicked {
		t.Error("BUG: VerifySecret(_, \"argon2id$$\") panics — " +
			"argon2.IDKey is called with keyLen=0 because an empty base64 field " +
			"decodes to a nil slice; fix: guard len(want)==0 || len(salt)==0 before calling IDKey")
	}
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestLogin_CorrectPIN_ReturnsTokenAndUser(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "alice", "9999", "caja", 1)

	token, user, err := svc.Login(context.Background(), "alice", "9999", "127.0.0.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Error("Login: expected non-empty token")
	}
	if user.Username != "alice" {
		t.Errorf("Login user: got %q, want %q", user.Username, "alice")
	}
	if user.Role != "caja" {
		t.Errorf("Login role: got %q, want %q", user.Role, "caja")
	}
	if user.ID == "" {
		t.Error("Login: user.ID must not be empty")
	}
}

func TestLogin_WrongPIN_ErrForbidden(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "bob", "1234", "mesero", 1)

	_, _, err := svc.Login(context.Background(), "bob", "wrongpin", "127.0.0.1")
	if err == nil {
		t.Fatal("Login: expected error for wrong PIN, got nil")
	}
	if !isForbidden(err) {
		t.Errorf("Login wrong PIN: got %v, want ErrForbidden", err)
	}
}

func TestLogin_UnknownUsername_ErrForbidden(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	_, _, err := svc.Login(context.Background(), "nobody", "1234", "127.0.0.1")
	if err == nil {
		t.Fatal("Login: expected error for unknown username, got nil")
	}
	if !isForbidden(err) {
		t.Errorf("Login unknown user: got %v, want ErrForbidden", err)
	}
}

func TestLogin_InactiveUser_ErrForbidden(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	// active = 0 means the account is disabled.
	seedUser(t, d, "charlie", "5678", "auditor", 0)

	_, _, err := svc.Login(context.Background(), "charlie", "5678", "127.0.0.1")
	if err == nil {
		t.Fatal("Login: expected error for inactive user, got nil")
	}
	if !isForbidden(err) {
		t.Errorf("Login inactive user: got %v, want ErrForbidden", err)
	}
}

// ---------------------------------------------------------------------------
// Session lifecycle
// ---------------------------------------------------------------------------

func TestSession_TokenResolvesAfterLogin(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "diana", "0000", "admin", 1)
	token, _, err := svc.Login(context.Background(), "diana", "0000", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	user, ok := svc.lookup(context.Background(), token)
	if !ok {
		t.Fatal("lookup: expected valid session immediately after Login")
	}
	if user.Username != "diana" {
		t.Errorf("lookup user: got %q, want %q", user.Username, "diana")
	}
}

func TestSession_LogoutRevokesToken(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "evan", "1111", "caja", 1)
	token, _, err := svc.Login(context.Background(), "evan", "1111", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	svc.Logout(context.Background(), token)

	_, ok := svc.lookup(context.Background(), token)
	if ok {
		t.Error("lookup: revoked session must not resolve")
	}
}

func TestSession_ExpiredSession_Rejected(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	userID := seedUser(t, d, "fiona", "2222", "bar", 1)

	// Craft a session whose expires_at is already in the past.
	fakeToken := "expiredtokenvalue12345678901234567890"
	pastExp := time.Now().Add(-time.Hour).UTC().UnixMilli()
	_, err := d.Writer.ExecContext(context.Background(),
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at) VALUES (?,?,?,?,?)`,
		hashToken(fakeToken), userID, "::1", core.NowMillis(), pastExp)
	if err != nil {
		t.Fatalf("insert expired session: %v", err)
	}

	_, ok := svc.lookup(context.Background(), fakeToken)
	if ok {
		t.Error("lookup: expired session must not resolve")
	}
}

// ---------------------------------------------------------------------------
// Only the sha256 hash of the token is stored, never the raw token.
// ---------------------------------------------------------------------------

func TestSession_OnlyHashStored_NotRawToken(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "grace", "3333", "admin", 1)
	token, _, err := svc.Login(context.Background(), "grace", "3333", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Compute the expected sha256 hash independently (not via hashToken) to
	// confirm the storage format rather than just round-tripping the helper.
	sum := sha256.Sum256([]byte(token))
	expectedID := hex.EncodeToString(sum[:])

	// The raw token must NOT be stored.
	var rawCount int
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, token).Scan(&rawCount); err != nil {
		t.Fatalf("query raw token: %v", err)
	}
	if rawCount != 0 {
		t.Error("sessions.id must not hold the raw token")
	}

	// The sha256 hex digest of the token MUST be stored.
	var hashCount int
	if err := d.Reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, expectedID).Scan(&hashCount); err != nil {
		t.Fatalf("query hashed token: %v", err)
	}
	if hashCount != 1 {
		t.Errorf("sessions.id must hold sha256(token): found %d row(s) for hash %q", hashCount, expectedID)
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// nopHandler records whether the inner handler was reached and replies 200.
func nopHandler() (http.Handler, *bool) {
	reached := new(bool)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
	return h, reached
}

func TestMiddleware_NoCookie_Unauthorized(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h, reached := nopHandler()

	svc.Middleware()(h).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: got %d, want 401", w.Code)
	}
	if *reached {
		t.Error("inner handler must not run for unauthenticated request")
	}
}

func TestMiddleware_InvalidCookie_Unauthorized(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	r := requestWithCookie("completely-bogus-token-not-in-db")
	w := httptest.NewRecorder()
	h, reached := nopHandler()

	svc.Middleware()(h).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid cookie: got %d, want 401", w.Code)
	}
	if *reached {
		t.Error("inner handler must not run for invalid token")
	}
}

func TestMiddleware_DisallowedRole_Forbidden(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	// "mesero" is not in the allowed set {"admin", "caja"}.
	seedUser(t, d, "henry", "4444", "mesero", 1)
	token, _, err := svc.Login(context.Background(), "henry", "4444", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	r := requestWithCookie(token)
	w := httptest.NewRecorder()
	h, reached := nopHandler()

	svc.Middleware("admin", "caja")(h).ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("disallowed role: got %d, want 403", w.Code)
	}
	if *reached {
		t.Error("inner handler must not run for forbidden role")
	}
}

func TestMiddleware_AdminPassesAnyRoleRestriction(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	// "admin" is not listed in Middleware("caja") but the admin override must let it through.
	seedUser(t, d, "irene", "5555", "admin", 1)
	token, _, err := svc.Login(context.Background(), "irene", "5555", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	r := requestWithCookie(token)
	w := httptest.NewRecorder()
	h, reached := nopHandler()

	svc.Middleware("caja")(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("admin override: got %d, want 200", w.Code)
	}
	if !*reached {
		t.Error("inner handler must run for admin regardless of role list")
	}
}

func TestMiddleware_AllowedRole_PassesAndPutsUserInContext(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "julia", "6666", "caja", 1)
	token, _, err := svc.Login(context.Background(), "julia", "6666", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	r := requestWithCookie(token)
	w := httptest.NewRecorder()

	var ctxUser User
	var ctxOk bool
	inner := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		ctxUser, ctxOk = FromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	})

	svc.Middleware("caja", "admin")(inner).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("allowed role: got %d, want 200", w.Code)
	}
	if !ctxOk {
		t.Fatal("FromContext: user must be present in context for allowed role")
	}
	if ctxUser.Username != "julia" {
		t.Errorf("FromContext username: got %q, want %q", ctxUser.Username, "julia")
	}
	if ctxUser.Role != "caja" {
		t.Errorf("FromContext role: got %q, want %q", ctxUser.Role, "caja")
	}
}

func TestMiddleware_EmptyRoleList_AnyAuthenticatedUserPasses(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "kevin", "7777", "bar", 1)
	token, _, err := svc.Login(context.Background(), "kevin", "7777", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	r := requestWithCookie(token)
	w := httptest.NewRecorder()
	h, reached := nopHandler()

	// No roles specified → any authenticated user is allowed.
	svc.Middleware()(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("empty role list: got %d, want 200", w.Code)
	}
	if !*reached {
		t.Error("inner handler must run for any authenticated user when role list is empty")
	}
}

func TestMiddleware_AfterLogout_Unauthorized(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "lena", "8888", "cocina", 1)
	token, _, err := svc.Login(context.Background(), "lena", "8888", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	svc.Logout(context.Background(), token)

	r := requestWithCookie(token)
	w := httptest.NewRecorder()
	h, reached := nopHandler()

	svc.Middleware()(h).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("post-logout token: got %d, want 401", w.Code)
	}
	if *reached {
		t.Error("inner handler must not run after Logout")
	}
}

// TestMiddleware_SetCookieThenRequest verifies that a token written by
// SetCookie on a recorder can be round-tripped back as a real cookie on a
// subsequent request — confirming the cookie name constant is consistent.
func TestMiddleware_SetCookieThenRequest(t *testing.T) {
	d := openTestDB(t)
	svc := New(d)

	seedUser(t, d, "marta", "9090", "auditor", 1)
	token, _, err := svc.Login(context.Background(), "marta", "9090", "::1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Write the cookie to a recorder the way the real login handler would.
	loginW := httptest.NewRecorder()
	SetCookie(loginW, token)

	// Copy the cookie from the response onto a new request.
	r := httptest.NewRequest(http.MethodGet, "/protected", nil)
	for _, c := range loginW.Result().Cookies() {
		r.AddCookie(c)
	}

	w := httptest.NewRecorder()
	h, reached := nopHandler()
	svc.Middleware()(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("SetCookie round-trip: got %d, want 200", w.Code)
	}
	if !*reached {
		t.Error("inner handler must run when cookie from SetCookie is used")
	}
}
