// Package auth handles operator identity for Action EventOS: argon2id PIN/
// password hashing, server-side sessions (only a hash of the token is stored),
// and role-based middleware. Authorization is always enforced on the server;
// the UI only hides what a role cannot do.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/httpx"
)

const (
	cookieName   = "aeos_session"
	sessionTTL   = 12 * time.Hour
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// User is the authenticated operator carried in the request context.
type User struct {
	ID       string
	Username string
	FullName string
	Role     string
}

type ctxKey struct{}

// FromContext returns the authenticated user, if any.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// HashSecret hashes a PIN or password with argon2id, returning an encoded
// string that embeds the salt.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifySecret checks a secret against an encoded hash in constant time.
func VerifySecret(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	// Guard against a malformed/corrupted hash with empty salt or key:
	// argon2.IDKey panics (nil deref) when given a zero-length key/salt.
	// A zero-length component is by definition not a valid hash.
	if len(salt) == 0 || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Service issues and validates sessions.
type Service struct {
	db *db.DB
}

func New(d *db.DB) *Service { return &Service{db: d} }

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Login verifies username+secret (PIN or password) and returns a raw session
// token to set as a cookie. secretKind is "pin" or "password".
func (s *Service) Login(ctx context.Context, username, secret, ip string) (string, User, error) {
	var u User
	var pinHash string
	var passHash sql.NullString
	var active int
	row := s.db.Reader.QueryRowContext(ctx,
		`SELECT id, username, COALESCE(full_name,''), role, pin_hash, password_hash, active
		 FROM users WHERE username = ?`, username)
	if err := row.Scan(&u.ID, &u.Username, &u.FullName, &u.Role, &pinHash, &passHash, &active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", User{}, core.ErrForbidden
		}
		return "", User{}, err
	}
	if active == 0 {
		return "", User{}, core.ErrForbidden
	}
	ok := VerifySecret(secret, pinHash)
	if !ok && passHash.Valid {
		ok = VerifySecret(secret, passHash.String)
	}
	if !ok {
		return "", User{}, core.ErrForbidden
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", User{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := core.NowMillis()
	exp := time.Now().Add(sessionTTL).UTC().UnixMilli()
	if _, err := s.db.Writer.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, ip, created_at, expires_at) VALUES (?,?,?,?,?)`,
		hashToken(token), u.ID, ip, now, exp); err != nil {
		return "", User{}, err
	}
	return token, u, nil
}

// Logout revokes the session behind the request cookie.
func (s *Service) Logout(ctx context.Context, raw string) {
	if raw == "" {
		return
	}
	_, _ = s.db.Writer.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		core.NowMillis(), hashToken(raw))
}

func (s *Service) lookup(ctx context.Context, raw string) (User, bool) {
	if raw == "" {
		return User{}, false
	}
	var u User
	row := s.db.Reader.QueryRowContext(ctx,
		`SELECT u.id, u.username, COALESCE(u.full_name,''), u.role
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.id = ? AND s.revoked_at IS NULL AND s.expires_at > ? AND u.active = 1`,
		hashToken(raw), core.NowMillis())
	if err := row.Scan(&u.ID, &u.Username, &u.FullName, &u.Role); err != nil {
		return User{}, false
	}
	return u, true
}

// SetCookie writes the session cookie.
func SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearCookie removes the session cookie.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
}

// TokenFromRequest extracts the raw session token from the cookie.
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// Middleware authenticates the request and, if roles are given, authorizes the
// user's role. An empty roles slice means "any authenticated user".
func (s *Service) Middleware(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := s.lookup(r.Context(), TokenFromRequest(r))
			if !ok {
				httpx.JSON(w, http.StatusUnauthorized, map[string]string{"error": "no autenticado"})
				return
			}
			if len(allowed) > 0 && !allowed[u.Role] && u.Role != "admin" {
				httpx.Error(w, core.ErrForbidden)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, u)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
