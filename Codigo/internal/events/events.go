// Package events manages the event lifecycle (draft -> active -> closed).
// Almost everything in Action EventOS is scoped to an event, and exactly one
// event may be active at a time (enforced by a partial unique index).
//
// This package is the canonical pattern for a domain module: a Service holding
// the shared deps, a Module implementing server.Module, mutations routed
// through the single writer with an audit row, and reads served from readerDB.
package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Event is an event record.
type Event struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	StartsAt     *int64 `json:"starts_at"`
	EndsAt       *int64 `json:"ends_at"`
	Status       string `json:"status"`
	DefaultTopup int64  `json:"default_topup"`
	SplitPolicy  string `json:"split_policy"`
	CreatedAt    int64  `json:"created_at"`
}

// Module mounts the events routes.
type Module struct{ d server.Deps }

// New constructs the events module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires routes. Reads need any authenticated user; writes need admin.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	any := d.Auth.Middleware()
	admin := d.Auth.Middleware("admin")
	mux.Handle("GET /api/events", any(http.HandlerFunc(m.list)))
	mux.Handle("GET /api/events/active", any(http.HandlerFunc(m.active)))
	mux.Handle("GET /api/events/current", any(http.HandlerFunc(m.current)))
	mux.Handle("POST /api/events", admin(http.HandlerFunc(m.create)))
	mux.Handle("POST /api/events/{id}/activate", admin(http.HandlerFunc(m.activate)))
	mux.Handle("POST /api/events/{id}/close", admin(http.HandlerFunc(m.close)))
	mux.Handle("PUT /api/events/{id}", admin(http.HandlerFunc(m.update)))
}

func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, name, starts_at, ends_at, status, default_topup, split_policy, created_at
		 FROM events ORDER BY created_at DESC, id DESC`)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Name, &e.StartsAt, &e.EndsAt, &e.Status,
			&e.DefaultTopup, &e.SplitPolicy, &e.CreatedAt); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, e)
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (m *Module) active(w http.ResponseWriter, r *http.Request) {
	e, err := ActiveEvent(r.Context(), m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

// ActiveEvent returns the currently active event or core.ErrNoActiveEvent.
// Exported so other modules can resolve the active event.
func ActiveEvent(ctx context.Context, d server.Deps) (Event, error) {
	var e Event
	row := d.App.DB.Reader.QueryRowContext(ctx,
		`SELECT id, name, starts_at, ends_at, status, default_topup, split_policy, created_at
		 FROM events WHERE status = 'active' LIMIT 1`)
	err := row.Scan(&e.ID, &e.Name, &e.StartsAt, &e.EndsAt, &e.Status,
		&e.DefaultTopup, &e.SplitPolicy, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, core.ErrNoActiveEvent
	}
	return e, err
}

// RowQueryer is the minimal read surface needed to resolve the current event.
// Both *sql.DB (the reader pool) and *sql.Tx satisfy it.
type RowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CurrentEventID resolves the single "current" event — the one being configured
// or run, i.e. status IN ('draft','active'). It returns the event id and its
// status, or core.ErrNoActiveEvent when none exists. An 'active' event is
// preferred over a 'draft' one (and the earliest otherwise) so the result is
// deterministic. Catalog/customers use the status to enforce the lock-down:
// configuration is only allowed while the current event is 'draft'.
func CurrentEventID(ctx context.Context, reader RowQueryer) (id string, status string, err error) {
	err = reader.QueryRowContext(ctx,
		`SELECT id, status FROM events WHERE status IN ('draft','active')
		 ORDER BY (status='active') DESC, created_at ASC LIMIT 1`).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", core.ErrNoActiveEvent
	}
	return id, status, err
}

// CurrentEvent returns the full current event (draft or active) for the UI, or
// core.ErrNoActiveEvent when none exists.
func CurrentEvent(ctx context.Context, d server.Deps) (Event, error) {
	var e Event
	row := d.App.DB.Reader.QueryRowContext(ctx,
		`SELECT id, name, starts_at, ends_at, status, default_topup, split_policy, created_at
		 FROM events WHERE status IN ('draft','active')
		 ORDER BY (status='active') DESC, created_at ASC LIMIT 1`)
	err := row.Scan(&e.ID, &e.Name, &e.StartsAt, &e.EndsAt, &e.Status,
		&e.DefaultTopup, &e.SplitPolicy, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, core.ErrNoActiveEvent
	}
	return e, err
}

func (m *Module) current(w http.ResponseWriter, r *http.Request) {
	e, err := CurrentEvent(r.Context(), m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		Name         string `json:"name"`
		StartsAt     *int64 `json:"starts_at"`
		EndsAt       *int64 `json:"ends_at"`
		DefaultTopup int64  `json:"default_topup"`
		SplitPolicy  string `json:"split_policy"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.Name == "" {
		httpx.Error(w, core.ErrValidation)
		return
	}
	if in.DefaultTopup <= 0 {
		in.DefaultTopup = 50000
	}
	if in.SplitPolicy != "strict_equal" {
		in.SplitPolicy = "cascade"
	}
	id := core.NewID()
	now := core.NowMillis()
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		if _, err := tx.Exec(
			`INSERT INTO events(id, name, starts_at, ends_at, status, default_topup, split_policy, created_at)
			 VALUES (?,?,?,?, 'draft', ?,?,?)`,
			id, in.Name, in.StartsAt, in.EndsAt, in.DefaultTopup, in.SplitPolicy, now); err != nil {
			return nil, err
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: id, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "event_create", Entity: "event", EntityID: id,
			DetailJSON: fmt.Sprintf(`{"name":%q}`, in.Name),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, Event{ID: id, Name: in.Name, Status: "draft",
		DefaultTopup: in.DefaultTopup, SplitPolicy: in.SplitPolicy, CreatedAt: now})
}

func (m *Module) update(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	var in struct {
		Name         string `json:"name"`
		DefaultTopup int64  `json:"default_topup"`
		SplitPolicy  string `json:"split_policy"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(`UPDATE events SET name=?, default_topup=?, split_policy=? WHERE id=?`,
			in.Name, in.DefaultTopup, in.SplitPolicy, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: id, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "event_update", Entity: "event", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Module) activate(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		// Close any currently-active event to honor the one-active invariant.
		if _, err := tx.Exec(`UPDATE events SET status='closed' WHERE status='active' AND id<>?`, id); err != nil {
			return nil, err
		}
		res, err := tx.Exec(`UPDATE events SET status='active' WHERE id=? AND status IN ('draft','active')`, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: id, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "event_activate", Entity: "event", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Module) close(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		res, err := tx.Exec(`UPDATE events SET status='closed' WHERE id=?`, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID: id, UserID: u.ID, IP: httpx.ClientIP(r),
			Action: "event_close", Entity: "event", EntityID: id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	// Safety backup at event close.
	if _, err := m.d.Backup.Create(r.Context(), "auto", id); err != nil {
		// non-fatal
		_ = err
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}
