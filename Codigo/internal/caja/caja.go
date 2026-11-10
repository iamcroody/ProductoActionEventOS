// Package caja manages cash register sessions (apertura/cierre) for Action EventOS.
// A session represents one operator's cash drawer lifecycle within an active event:
// it is opened with an initial float, accumulates cash payments as orders are placed,
// and is closed with a physical count that yields an expected/counted/variance
// reconciliation.
//
// This package follows the canonical module pattern from package events: a Module
// holding shared deps, mutations serialised through the single writer with an audit
// row, and reads served from the reader DB.
package caja

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Session is a cash register session record.
type Session struct {
	ID           string `json:"id"`
	EventID      string `json:"event_id"`
	OperatorID   string `json:"operator_id"`
	OpeningFloat int64  `json:"opening_float"`
	OpenedAt     int64  `json:"opened_at"`
	ClosedAt     *int64 `json:"closed_at"`
	ExpectedCash *int64 `json:"expected_cash"`
	CountedCash  *int64 `json:"counted_cash"`
	Variance     *int64 `json:"variance"`
	Status       string `json:"status"`
}

// SessionWithTotals extends Session with a dynamically computed cash total for
// use in the sessions list. CashTotal is the sum of all cash payments for
// non-voided orders belonging to the session.
type SessionWithTotals struct {
	Session
	CashTotal int64 `json:"cash_total"`
}

// Module mounts the caja routes.
type Module struct{ d server.Deps }

// New constructs the caja module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires routes. Both admin and caja roles are allowed on every route;
// the sessions listing restricts the result set by role inside the handler.
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	ac := d.Auth.Middleware("admin", "caja")
	mux.Handle("POST /api/caja/open", ac(http.HandlerFunc(m.open)))
	mux.Handle("GET /api/caja/current", ac(http.HandlerFunc(m.current)))
	mux.Handle("POST /api/caja/close", ac(http.HandlerFunc(m.close)))
	mux.Handle("GET /api/caja/sessions", ac(http.HandlerFunc(m.sessions)))
}

// activeEventID returns the ID of the currently-active event or core.ErrNoActiveEvent.
func activeEventID(ctx context.Context, d server.Deps) (string, error) {
	var id string
	err := d.App.DB.Reader.QueryRowContext(ctx,
		`SELECT id FROM events WHERE status = 'active' LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNoActiveEvent
	}
	return id, err
}

// isUniqueConflict reports whether err is a SQLite UNIQUE constraint violation.
// Used to translate the one-open-session-per-operator partial index into
// core.ErrConflict before the error crosses into HTTP territory.
func isUniqueConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// open handles POST /api/caja/open.
// Opens a new cash session for the current operator in the active event.
// Rejects with core.ErrConflict when the operator already has an open session
// (enforced by the partial unique index idx_cash_one_open).
func (m *Module) open(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		OpeningFloat int64 `json:"opening_float"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	eventID, err := activeEventID(r.Context(), m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	id := core.NewID()
	now := core.NowMillis()

	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(
			`INSERT INTO cash_sessions(id, event_id, operator_id, opening_float, opened_at, status)
			 VALUES (?, ?, ?, ?, ?, 'open')`,
			id, eventID, u.ID, in.OpeningFloat, now)
		if err != nil {
			if isUniqueConflict(err) {
				return nil, core.ErrConflict
			}
			return nil, err
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID:    eventID,
			UserID:     u.ID,
			IP:         httpx.ClientIP(r),
			Action:     "caja_open",
			Entity:     "cash_session",
			EntityID:   id,
			DetailJSON: fmt.Sprintf(`{"opening_float":%d}`, in.OpeningFloat),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, Session{
		ID:           id,
		EventID:      eventID,
		OperatorID:   u.ID,
		OpeningFloat: in.OpeningFloat,
		OpenedAt:     now,
		Status:       "open",
	})
}

// current handles GET /api/caja/current.
// Returns the current operator's open session or core.ErrNotFound if none.
func (m *Module) current(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	s, err := loadOpenSession(r.Context(), m.d, u.ID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

// loadOpenSession fetches the operator's open cash session from the reader DB.
// Returns core.ErrNotFound when the operator has no open session.
func loadOpenSession(ctx context.Context, d server.Deps, operatorID string) (Session, error) {
	var s Session
	row := d.App.DB.Reader.QueryRowContext(ctx,
		`SELECT id, event_id, operator_id, opening_float, opened_at,
		        closed_at, expected_cash, counted_cash, variance, status
		 FROM cash_sessions
		 WHERE operator_id = ? AND status = 'open'
		 LIMIT 1`,
		operatorID)
	err := row.Scan(
		&s.ID, &s.EventID, &s.OperatorID, &s.OpeningFloat, &s.OpenedAt,
		&s.ClosedAt, &s.ExpectedCash, &s.CountedCash, &s.Variance, &s.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, core.ErrNotFound
	}
	return s, err
}

// close handles POST /api/caja/close.
// Closes the current operator's open session. expected_cash is computed inside
// the writer transaction as opening_float + SUM of cash payments for non-voided
// orders linked to this session. variance = counted_cash - expected_cash.
// Audits action='caja_close' with expected/counted/variance in detail_json.
func (m *Module) close(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		CountedCash int64 `json:"counted_cash"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}

	// Resolve the open session before entering the writer so the reader does not
	// block the write queue.
	s, err := loadOpenSession(r.Context(), m.d, u.ID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	now := core.NowMillis()
	var closed Session

	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		// Sum all cash payments for non-voided orders belonging to this session.
		var cashSales int64
		if err := tx.QueryRow(
			`SELECT COALESCE(SUM(p.amount), 0)
			 FROM payments p
			 JOIN orders o ON o.id = p.order_id
			 WHERE p.method = 'cash' AND o.session_id = ? AND o.status <> 'voided'`,
			s.ID).Scan(&cashSales); err != nil {
			return nil, err
		}

		expectedCash := s.OpeningFloat + cashSales
		variance := in.CountedCash - expectedCash
		countedCash := in.CountedCash // local copy so pointer remains valid after the tx

		res, err := tx.Exec(
			`UPDATE cash_sessions
			 SET status = 'closed', closed_at = ?, expected_cash = ?, counted_cash = ?, variance = ?
			 WHERE id = ? AND status = 'open'`,
			now, expectedCash, countedCash, variance, s.ID)
		if err != nil {
			return nil, err
		}
		// If someone else closed the session between our read and this write, bail out.
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrConflict
		}

		closed = Session{
			ID:           s.ID,
			EventID:      s.EventID,
			OperatorID:   s.OperatorID,
			OpeningFloat: s.OpeningFloat,
			OpenedAt:     s.OpenedAt,
			ClosedAt:     &now,
			ExpectedCash: &expectedCash,
			CountedCash:  &countedCash,
			Variance:     &variance,
			Status:       "closed",
		}

		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID:  s.EventID,
			UserID:   u.ID,
			IP:       httpx.ClientIP(r),
			Action:   "caja_close",
			Entity:   "cash_session",
			EntityID: s.ID,
			DetailJSON: fmt.Sprintf(`{"expected":%d,"counted":%d,"variance":%d}`,
				expectedCash, countedCash, variance),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, closed)
}

// sessions handles GET /api/caja/sessions.
// Lists sessions for the active event ordered newest first. Admin sees all
// sessions; caja sees only their own. Each entry includes a cash_total
// dynamically computed from payments so the caller never needs a second request.
func (m *Module) sessions(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())

	eventID, err := activeEventID(r.Context(), m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	// Correlated subquery computes the cash total for each session inline.
	const base = `
		SELECT cs.id, cs.event_id, cs.operator_id, cs.opening_float, cs.opened_at,
		       cs.closed_at, cs.expected_cash, cs.counted_cash, cs.variance, cs.status,
		       COALESCE((
		           SELECT SUM(p.amount) FROM payments p JOIN orders o ON o.id = p.order_id
		           WHERE p.method = 'cash' AND o.session_id = cs.id AND o.status <> 'voided'
		       ), 0) AS cash_total
		FROM cash_sessions cs
		WHERE cs.event_id = ?`

	var rows *sql.Rows
	if u.Role == "admin" {
		rows, err = m.d.App.DB.Reader.QueryContext(r.Context(),
			base+` ORDER BY cs.opened_at DESC`, eventID)
	} else {
		rows, err = m.d.App.DB.Reader.QueryContext(r.Context(),
			base+` AND cs.operator_id = ? ORDER BY cs.opened_at DESC`,
			eventID, u.ID)
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()

	out := []SessionWithTotals{}
	for rows.Next() {
		var st SessionWithTotals
		if err := rows.Scan(
			&st.ID, &st.EventID, &st.OperatorID, &st.OpeningFloat, &st.OpenedAt,
			&st.ClosedAt, &st.ExpectedCash, &st.CountedCash, &st.Variance, &st.Status,
			&st.CashTotal,
		); err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, st)
	}
	httpx.JSON(w, http.StatusOK, out)
}
