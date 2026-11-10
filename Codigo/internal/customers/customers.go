// Package customers manages event attendees (Asistentes) and their prepaid
// balance (Saldo). Each attendee carries a unique QR token for till scanning
// and a cached balance backed by an immutable balance_ledger.
//
// This package follows the canonical module pattern: all mutations route
// through app.Writer.Do, every mutation writes an audit row inside the same
// transaction, and balance changes use app.AppendLedger with a
// balance_version optimistic-lock guard to prevent lost updates.
package customers

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	qrcode "github.com/skip2/go-qrcode"

	"eventpos/internal/app"
	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/events"
	"eventpos/internal/httpx"
	"eventpos/internal/server"
)

// Attendee is a paying customer record scoped to one event.
type Attendee struct {
	ID             string  `json:"id"`
	EventID        string  `json:"event_id"`
	QRToken        string  `json:"qr_token"`
	DisplayName    string  `json:"display_name"`
	DocID          *string `json:"doc_id"`
	Email          *string `json:"email"`
	Phone          *string `json:"phone"`
	NoRedeem       bool    `json:"no_redeem"` // sin dinero redimible: no recibe saldo, paga en efectivo
	Balance        int64   `json:"balance"`
	BalanceVersion int64   `json:"balance_version"`
	Status         string  `json:"status"`
	CreatedAt      int64   `json:"created_at"`
}

// LedgerEntry is one immutable row from balance_ledger.
type LedgerEntry struct {
	ID           string  `json:"id"`
	AttendeeID   string  `json:"attendee_id"`
	Type         string  `json:"type"`
	Amount       int64   `json:"amount"`
	BalanceAfter int64   `json:"balance_after"`
	RefOrderID   *string `json:"ref_order_id"`
	RefPaymentID *string `json:"ref_payment_id"`
	UserID       string  `json:"user_id"`
	CreatedAt    int64   `json:"created_at"`
}

// Module mounts the attendee and balance routes.
type Module struct{ d server.Deps }

// New constructs the customers module.
func New(d server.Deps) *Module { return &Module{d: d} }

// Register wires all attendee routes onto mux.
// Reads require any authenticated user. Create and top-up require "caja"
// (admin always passes — see auth.Middleware). Block/unblock require "admin".
func (m *Module) Register(mux *http.ServeMux, d server.Deps) {
	any := d.Auth.Middleware()
	admin := d.Auth.Middleware("admin")
	// "caja" is sufficient: auth.Middleware lets "admin" through automatically.
	caja := d.Auth.Middleware("caja")

	mux.Handle("POST /api/attendees", caja(http.HandlerFunc(m.create)))
	mux.Handle("POST /api/attendees/import", caja(http.HandlerFunc(m.bulkImport)))
	mux.Handle("GET /api/attendees", any(http.HandlerFunc(m.search)))
	// QR lookup lives under its own parent path (/api/qr/...) rather than under
	// /api/attendees/by-qr/{token}: the latter would be ambiguous with
	// /api/attendees/{id}/ledger in Go 1.22+ ServeMux (both match
	// /api/attendees/by-qr/ledger), which panics at registration.
	mux.Handle("GET /api/qr/{token}", any(http.HandlerFunc(m.byQR)))
	mux.Handle("GET /api/attendees/{id}/qr.png", any(http.HandlerFunc(m.qrPNG)))
	mux.Handle("GET /api/attendees/{id}/ledger", any(http.HandlerFunc(m.ledger)))
	mux.Handle("GET /api/attendees/{id}", any(http.HandlerFunc(m.get)))
	mux.Handle("POST /api/attendees/{id}/topup", caja(http.HandlerFunc(m.topup)))
	mux.Handle("POST /api/attendees/{id}/block", admin(http.HandlerFunc(m.block)))
	mux.Handle("POST /api/attendees/{id}/unblock", admin(http.HandlerFunc(m.unblock)))
}

// --- helpers ---

// generateQRToken returns a 22-character URL-safe base64 token backed by 16
// random bytes (128 bits of entropy). Collision probability is negligible.
func generateQRToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// currentEvent returns the id and status of the current event (status draft or
// active), or core.ErrNoActiveEvent if none exists. Attendee configuration runs
// against this event; reads work in both draft and active, while create/import/
// topup/edit are rejected once the event is 'active' (see lockedIfActive).
func currentEvent(r *http.Request, d server.Deps) (id string, status string, err error) {
	return events.CurrentEventID(r.Context(), d.App.DB.Reader)
}

// lockedIfActive writes a 409 Conflict carrying core.ErrEventLocked and returns
// true when the current event is 'active'. We bypass httpx.Error on purpose:
// httpx is shared and cannot be edited, and its default would map an unknown
// error to 500. Writing 409 here keeps the lock response explicit and uniform.
func lockedIfActive(w http.ResponseWriter, status string) bool {
	if status == "active" {
		httpx.JSON(w, http.StatusConflict, map[string]string{"error": core.ErrEventLocked.Error()})
		return true
	}
	return false
}

// int64ptr returns a pointer to v. Used to fill the optional *int64 fields
// in AuditEntry (BalanceBefore, BalanceAfter).
func int64ptr(v int64) *int64 { return &v }

// scanAttendee fills an Attendee from a scan function (row.Scan or rows.Scan).
// Nullable string columns (doc_id, phone) are mapped to *string.
func scanAttendee(scan func(...any) error) (Attendee, error) {
	var a Attendee
	var docID, email, phone sql.NullString
	var noRedeem int
	err := scan(
		&a.ID, &a.EventID, &a.QRToken, &a.DisplayName,
		&docID, &email, &phone, &noRedeem,
		&a.Balance, &a.BalanceVersion, &a.Status, &a.CreatedAt,
	)
	if docID.Valid {
		a.DocID = &docID.String
	}
	if email.Valid {
		a.Email = &email.String
	}
	if phone.Valid {
		a.Phone = &phone.String
	}
	a.NoRedeem = noRedeem == 1
	return a, err
}

// insertAttendeeInTx inserts one attendee row (balance=0, version=0) and,
// when initialTopup > 0, atomically upgrades the balance, appends a ledger
// row, and writes a topup audit entry — all inside the caller's transaction.
// It also writes an attendee_create audit entry unconditionally.
// Returns the final balance and balance_version so callers can build the
// response without a second read.
func insertAttendeeInTx(
	tx *sql.Tx,
	id, eventID, token, displayName string,
	docID, email, phone *string,
	noRedeem bool,
	initialTopup, now int64,
	userID string,
) (balance int64, version int64, err error) {

	// A no_redeem attendee never holds redeemable balance: force any requested
	// initial top-up to zero (they pay cash only).
	noRedeemInt := 0
	if noRedeem {
		noRedeemInt = 1
		initialTopup = 0
	}

	if _, err = tx.Exec(
		`INSERT INTO attendees
		 (id, event_id, qr_token, display_name, doc_id, email, phone, no_redeem,
		  balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?,0,0,'active',?)`,
		id, eventID, token, displayName, docID, email, phone, noRedeemInt, now,
	); err != nil {
		return 0, 0, err
	}

	if initialTopup > 0 {
		balance = initialTopup
		version = 1
		if _, err = tx.Exec(
			`UPDATE attendees SET balance=?, balance_version=1
			 WHERE id=? AND balance_version=0`,
			balance, id,
		); err != nil {
			return 0, 0, err
		}
		if _, err = app.AppendLedger(tx, id, "topup",
			initialTopup, balance, "", "", userID); err != nil {
			return 0, 0, err
		}
		topupDetail, _ := json.Marshal(map[string]any{"amount": initialTopup})
		if err = app.WriteAudit(tx, app.AuditEntry{
			EventID:       eventID,
			UserID:        userID,
			AttendeeID:    id,
			Action:        "attendee_topup",
			Entity:        "attendee",
			EntityID:      id,
			BalanceBefore: int64ptr(0),
			BalanceAfter:  int64ptr(balance),
			DetailJSON:    string(topupDetail),
		}); err != nil {
			return 0, 0, err
		}
	}

	createDetail, _ := json.Marshal(map[string]any{"display_name": displayName})
	if err = app.WriteAudit(tx, app.AuditEntry{
		EventID:    eventID,
		UserID:     userID,
		AttendeeID: id,
		Action:     "attendee_create",
		Entity:     "attendee",
		EntityID:   id,
		DetailJSON: string(createDetail),
	}); err != nil {
		return 0, 0, err
	}

	return balance, version, nil
}

// --- route handlers ---

// create handles POST /api/attendees.
// Body: {display_name, doc_id?, phone?, initial_topup?}
// If initial_topup > 0, the topup is performed in the same writer transaction
// so the attendee is never visible with a zero balance followed by a credit.
func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		DisplayName  string  `json:"display_name"`
		DocID        *string `json:"doc_id"`
		Email        *string `json:"email"`
		Phone        *string `json:"phone"`
		NoRedeem     bool    `json:"no_redeem"`
		InitialTopup int64   `json:"initial_topup"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.DisplayName == "" || in.InitialTopup < 0 || in.InitialTopup > core.MaxMoney {
		httpx.Error(w, core.ErrValidation)
		return
	}
	// Registering attendees is normal operation DURING the event (people keep
	// arriving), so it is NOT blocked by the event lock — only menu/inventory
	// configuration is frozen once the event is active.
	eventID, _, err := currentEvent(r, m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	token, err := generateQRToken()
	if err != nil {
		httpx.Error(w, err)
		return
	}
	id := core.NewID()
	now := core.NowMillis()

	var finalBalance, finalVersion int64
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		var txErr error
		finalBalance, finalVersion, txErr = insertAttendeeInTx(
			tx, id, eventID, token, in.DisplayName, in.DocID, in.Email, in.Phone,
			in.NoRedeem, in.InitialTopup, now, u.ID,
		)
		return nil, txErr
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, Attendee{
		ID:             id,
		EventID:        eventID,
		QRToken:        token,
		DisplayName:    in.DisplayName,
		DocID:          in.DocID,
		Email:          in.Email,
		Phone:          in.Phone,
		NoRedeem:       in.NoRedeem,
		Balance:        finalBalance,
		BalanceVersion: finalVersion,
		Status:         "active",
		CreatedAt:      now,
	})
}

// bulkImport handles POST /api/attendees/import.
// Body: {attendees: [{display_name, doc_id?, phone?, initial_topup?}, ...]}
// All rows are inserted in one writer transaction, replacing the slow
// spreadsheet import workflow. Returns {count, attendees}.
func (m *Module) bulkImport(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	var in struct {
		Attendees []struct {
			DisplayName  string  `json:"display_name"`
			DocID        *string `json:"doc_id"`
			Email        *string `json:"email"`
			Phone        *string `json:"phone"`
			NoRedeem     bool    `json:"no_redeem"`
			InitialTopup int64   `json:"initial_topup"`
		} `json:"attendees"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if len(in.Attendees) == 0 {
		httpx.Error(w, core.ErrValidation)
		return
	}

	// Validate names and pre-generate IDs + QR tokens outside the transaction
	// so that crypto/rand I/O does not extend the write lock window.
	type prepared struct {
		id, token           string
		displayName         string
		docID, email, phone *string
		noRedeem            bool
		initialTopup        int64
	}
	items := make([]prepared, len(in.Attendees))
	for i, a := range in.Attendees {
		if a.DisplayName == "" || a.InitialTopup < 0 || a.InitialTopup > core.MaxMoney {
			httpx.Error(w, core.ErrValidation)
			return
		}
		token, err := generateQRToken()
		if err != nil {
			httpx.Error(w, err)
			return
		}
		items[i] = prepared{
			id:           core.NewID(),
			token:        token,
			displayName:  a.DisplayName,
			docID:        a.DocID,
			email:        a.Email,
			phone:        a.Phone,
			noRedeem:     a.NoRedeem,
			initialTopup: a.InitialTopup,
		}
	}

	eventID, _, err := currentEvent(r, m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	now := core.NowMillis()

	created := make([]Attendee, len(items))
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		for i, it := range items {
			bal, ver, err := insertAttendeeInTx(
				tx, it.id, eventID, it.token, it.displayName,
				it.docID, it.email, it.phone, it.noRedeem, it.initialTopup, now, u.ID,
			)
			if err != nil {
				return nil, err
			}
			created[i] = Attendee{
				ID:             it.id,
				EventID:        eventID,
				QRToken:        it.token,
				DisplayName:    it.displayName,
				DocID:          it.docID,
				Email:          it.email,
				Phone:          it.phone,
				NoRedeem:       it.noRedeem,
				Balance:        bal,
				BalanceVersion: ver,
				Status:         "active",
				CreatedAt:      now,
			}
		}
		return nil, nil
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"count":     len(created),
		"attendees": created,
	})
}

// search handles GET /api/attendees?q=
// Hot path at the till: case-insensitive LIKE on display_name or doc_id
// within the active event, ordered by name, capped at 25 rows.
func (m *Module) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	eventID, _, err := currentEvent(r, m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	like := "%" + q + "%"
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, event_id, qr_token, display_name, doc_id, email, phone, no_redeem,
		        balance, balance_version, status, created_at
		 FROM attendees
		 WHERE event_id = ?
		   AND (lower(display_name) LIKE lower(?) OR doc_id LIKE ?)
		 ORDER BY display_name
		 LIMIT 25`,
		eventID, like, like,
	)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []Attendee{}
	for rows.Next() {
		a, err := scanAttendee(rows.Scan)
		if err != nil {
			httpx.Error(w, err)
			return
		}
		out = append(out, a)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// get handles GET /api/attendees/{id}.
func (m *Module) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row := m.d.App.DB.Reader.QueryRowContext(r.Context(),
		`SELECT id, event_id, qr_token, display_name, doc_id, email, phone, no_redeem,
		        balance, balance_version, status, created_at
		 FROM attendees WHERE id=?`, id)
	a, err := scanAttendee(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, core.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

// byQR handles GET /api/attendees/by-qr/{token}.
// Resolves an attendee from a scanned QR token. The unique index on qr_token
// makes this an O(1) lookup. Returns 404 if the token is unknown.
func (m *Module) byQR(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	row := m.d.App.DB.Reader.QueryRowContext(r.Context(),
		`SELECT id, event_id, qr_token, display_name, doc_id, email, phone, no_redeem,
		        balance, balance_version, status, created_at
		 FROM attendees WHERE qr_token=?`, token)
	a, err := scanAttendee(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, core.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

// qrPNG handles GET /api/attendees/{id}/qr.png — a printable QR that opens the
// customer web (isolated port) pre-filled with this attendee's token, so they
// can self-order in one scan. Encodes ClientBaseURL + "/?qr=<token>".
func (m *Module) qrPNG(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var token string
	err := m.d.App.DB.Reader.QueryRowContext(r.Context(),
		`SELECT qr_token FROM attendees WHERE id=?`, id).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, core.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, err)
		return
	}
	url := m.d.App.ClientBaseURL + "/?qr=" + token
	png, err := qrcode.Encode(url, qrcode.Medium, 512)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// ledger handles GET /api/attendees/{id}/ledger.
// Returns balance history newest-first (topups, debits, adjustments, etc.).
func (m *Module) ledger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rows, err := m.d.App.DB.Reader.QueryContext(r.Context(),
		`SELECT id, attendee_id, type, amount, balance_after,
		        ref_order_id, ref_payment_id, user_id, created_at
		 FROM balance_ledger
		 WHERE attendee_id=?
		 ORDER BY created_at DESC, id DESC`, id)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	defer rows.Close()
	out := []LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		var refOrderID, refPaymentID sql.NullString
		if err := rows.Scan(
			&e.ID, &e.AttendeeID, &e.Type, &e.Amount, &e.BalanceAfter,
			&refOrderID, &refPaymentID, &e.UserID, &e.CreatedAt,
		); err != nil {
			httpx.Error(w, err)
			return
		}
		if refOrderID.Valid {
			e.RefOrderID = &refOrderID.String
		}
		if refPaymentID.Valid {
			e.RefPaymentID = &refPaymentID.String
		}
		out = append(out, e)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// topup handles POST /api/attendees/{id}/topup.
// Body: {amount}
// Reads balance+version inside the writer transaction (serialized by the
// single writer), applies the credit, and uses a version guard so a stale
// concurrent reader cannot cause a lost update.
func (m *Module) topup(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	var in struct {
		Amount int64 `json:"amount"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, err)
		return
	}
	if in.Amount <= 0 || in.Amount > core.MaxMoney {
		httpx.Error(w, core.ErrValidation)
		return
	}

	// Recargas are normal operation during the event (people recharge at the
	// till as they arrive), so they are not blocked by the event lock. The
	// no_redeem rule still applies (handled below).
	_, _, err := currentEvent(r, m.d)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	var newBalance int64
	_, err = m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		// Re-read inside the writer tx to obtain the authoritative balance.
		var balance, version int64
		var eventID string
		var noRedeem int
		if err := tx.QueryRow(
			`SELECT balance, balance_version, event_id, no_redeem FROM attendees WHERE id=?`, id,
		).Scan(&balance, &version, &eventID, &noRedeem); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, core.ErrNotFound
			}
			return nil, err
		}
		// A no_redeem attendee never holds redeemable balance: reject top-ups.
		if noRedeem == 1 {
			return nil, core.ErrValidation
		}

		newBalance = balance + in.Amount
		res, err := tx.Exec(
			`UPDATE attendees
			 SET balance=?, balance_version=balance_version+1
			 WHERE id=? AND balance_version=?`,
			newBalance, id, version,
		)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Version mismatch: a concurrent topup already committed.
			return nil, core.ErrConflict
		}

		if _, err := app.AppendLedger(tx, id, "topup",
			in.Amount, newBalance, "", "", u.ID); err != nil {
			return nil, err
		}

		detail, _ := json.Marshal(map[string]any{"amount": in.Amount})
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID:       eventID,
			UserID:        u.ID,
			AttendeeID:    id,
			Action:        "attendee_topup",
			Entity:        "attendee",
			EntityID:      id,
			BalanceBefore: int64ptr(balance),
			BalanceAfter:  int64ptr(newBalance),
			DetailJSON:    string(detail),
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "balance": newBalance})
}

// setStatus is the shared implementation for block and unblock.
func (m *Module) setStatus(w http.ResponseWriter, r *http.Request, status, action string) {
	u, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	_, err := m.d.App.Writer.Do(r.Context(), func(tx *sql.Tx) (any, error) {
		// Fetch event_id for the audit row before we mutate.
		var eventID string
		if err := tx.QueryRow(
			`SELECT event_id FROM attendees WHERE id=?`, id,
		).Scan(&eventID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, core.ErrNotFound
			}
			return nil, err
		}
		res, err := tx.Exec(`UPDATE attendees SET status=? WHERE id=?`, status, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, core.ErrNotFound
		}
		return nil, app.WriteAudit(tx, app.AuditEntry{
			EventID:    eventID,
			UserID:     u.ID,
			AttendeeID: id,
			Action:     action,
			Entity:     "attendee",
			EntityID:   id,
		})
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// block handles POST /api/attendees/{id}/block.
func (m *Module) block(w http.ResponseWriter, r *http.Request) {
	m.setStatus(w, r, "blocked", "attendee_block")
}

// unblock handles POST /api/attendees/{id}/unblock.
func (m *Module) unblock(w http.ResponseWriter, r *http.Request) {
	m.setStatus(w, r, "active", "attendee_unblock")
}
