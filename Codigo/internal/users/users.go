// Package users manages operator accounts for Action EventOS: the people who
// run the event (admin, caja, mesero, cocina, bar, auditor). Note that
// "users" here means operators, not event attendees / customers.
//
// Every route is admin-only. Mutations are serialised through the single
// writer and each mutation appends an audit row.
package users

import (
	"database/sql"
	"net/http"
	"regexp"
	"strings"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// validRoles is the complete set of roles an operator may hold.
var validRoles = map[string]bool{
	"admin":   true,
	"caja":    true,
	"mesero":  true,
	"cocina":  true,
	"bar":     true,
	"auditor": true,
}

// pinRE accepts PINs that are exactly 4, 5, or 6 ASCII digits.
var pinRE = regexp.MustCompile(`^\d{4,6}$`)

// User is an operator record safe to return in API responses.
// pin_hash and password_hash are deliberately absent to prevent credential leaks.
type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FullName  string `json:"full_name"`
	Role      string `json:"role"`
	Active    int    `json:"active"`
	CreatedAt int64  `json:"created_at"`
}

// Module mounts the operator-management routes.
type Module struct{ d server.Deps }

// New constructs the users module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires all routes. Every route requires the admin role.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	admin := d.Auth.Middleware("admin")
	mux.Handle("GET /api/users", admin(http.HandlerFunc(m.list)))
	mux.Handle("POST /api/users", admin(http.HandlerFunc(m.create)))
	mux.Handle("PUT /api/users/{id}", admin(http.HandlerFunc(m.update)))
	mux.Handle("POST /api/users/{id}/pin", admin(http.HandlerFunc(m.resetPin)))
	mux.Handle("DELETE /api/users/{id}", admin(http.HandlerFunc(m.softDelete)))
}

// list returns all operators ordered by role then username.
// Hash fields are never selected to keep them off the wire.
func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, username, COALESCE(full_name,''), role, active, created_at
		 FROM users ORDER BY role, username`)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.FullName, &u.Role, &u.Active, &u.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, u)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// create registers a new operator. The caller must supply a valid PIN (4-6
// digits); an optional password lets the operator log in with either secret.
// A UNIQUE violation on username is mapped to core.ErrDuplicate (409).
func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		Username string `json:"username"`
		FullName string `json:"full_name"`
		Role     string `json:"role"`
		Pin      string `json:"pin"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.Username == "" || in.Role == "" || in.Pin == "" {
		httpx.Error(w, core.ErrValidation)
		return
	}
	if !validRoles[in.Role] {
		httpx.Error(w, core.ErrValidation)
		return
	}
	if !pinRE.MatchString(in.Pin) {
		httpx.Error(w, core.ErrValidation)
		return
	}

	pinHash, err := auth.HashSecret(in.Pin)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	// password is optional; only hash it when provided.
	var passHash any
	if in.Password != "" {
		h, err := auth.HashSecret(in.Password)
		if err != nil {
			httpx.Error(w, err)
			return
		}
		passHash = h
	}

	id := core.NewID()
	now := core.NowMillis()
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO users(id, username, full_name, role, pin_hash, password_hash, active, created_at)
			 VALUES (?,?,?,?,?,?,1,?)`,
			id, in.Username, nullStr(in.FullName), in.Role, pinHash, passHash, now)
		if err != nil {
			if isUnique(err) {
				return nil, core.ErrDuplicate
			}
			return nil, err
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "user_create", Entity: "user", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, User{
		ID:        id,
		Username:  in.Username,
		FullName:  in.FullName,
		Role:      in.Role,
		Active:    1,
		CreatedAt: now,
	})
}

// update replaces full_name, role, and active status for an existing operator.
func (m *Module) update(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	var in struct {
		FullName string `json:"full_name"`
		Role     string `json:"role"`
		Active   int    `json:"active"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if !validRoles[in.Role] {
		httpx.Error(w, core.ErrValidation)
		return
	}
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(
			`UPDATE users SET full_name=?, role=?, active=? WHERE id=?`,
			nullStr(in.FullName), in.Role, in.Active, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "user_update", Entity: "user", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// resetPin re-hashes and stores a new PIN for the given operator.
func (m *Module) resetPin(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	var in struct {
		Pin string `json:"pin"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if !pinRE.MatchString(in.Pin) {
		httpx.Error(w, core.ErrValidation)
		return
	}
	pinHash, err := auth.HashSecret(in.Pin)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(`UPDATE users SET pin_hash=? WHERE id=?`, pinHash, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "user_pin_reset", Entity: "user", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// softDelete marks an operator inactive (active=0). It refuses to deactivate
// the last remaining active admin to prevent a lock-out scenario.
func (m *Module) softDelete(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		// Read the target's role and current active status in one query.
		var role string
		var currentActive int
		err := tx.QueryRow(`SELECT role, active FROM users WHERE id=?`, id).
			Scan(&role, &currentActive)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, core.ErrNotFound
			}
			return nil, err
		}

		// Block the operation if this would remove the last active admin.
		if role == "admin" && currentActive == 1 {
			var count int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM users WHERE role='admin' AND active=1`,
			).Scan(&count); err != nil {
				return nil, err
			}
			if count <= 1 {
				return nil, core.ErrValidation
			}
		}

		if _, err := tx.Exec(`UPDATE users SET active=0 WHERE id=?`, id); err != nil {
			return nil, err
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "user_delete", Entity: "user", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// isUnique reports whether err is a SQLite UNIQUE constraint violation.
func isUnique(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// nullStr converts an empty string to a SQL NULL so optional TEXT columns
// store NULL instead of an empty string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
