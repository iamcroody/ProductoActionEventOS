// Package app holds the shared application context and the cross-cutting
// helpers (audit hash-chain, ledger append) that every Action EventOS module
// depends on. Freezing this contract lets domain modules be developed in
// isolation: each owns its own package and registers routes against *App.
package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/realtime"
	"eventpos/internal/writer"
)

// Brand identifiers. Action EventOS is the product; CROODY is the studio.
const (
	ProductName = "Action EventOS"
	Signature   = "Powered by CROODY"
)

// App is the shared runtime context handed to every module.
type App struct {
	DB      *db.DB
	Writer  *writer.Writer
	Hub     *realtime.Hub
	DataDir string
	// ClientBaseURL is the LAN URL of the SEPARATE customer web (its own port),
	// e.g. http://192.168.1.5:8090 . Used to build the QR codes attendees scan.
	// The customer port serves only the client web + /api/cliente, never the
	// management console or operator API.
	ClientBaseURL string
	// StaffURL is the PRIVATE management URL carrying the access key, e.g.
	// http://192.168.1.5:8080/?k=<key> . Staff phones scan a QR of this to get
	// past the access gate. Exposed only to authenticated admins so they can
	// onboard staff devices; never shown to clients.
	StaffURL string
}

// AuditEntry is one immutable audit record. Modules fill the relevant fields.
type AuditEntry struct {
	EventID       string
	UserID        string
	AttendeeID    string
	IP            string
	Action        string
	Entity        string
	EntityID      string
	ProductsJSON  string
	BalanceBefore *int64
	BalanceAfter  *int64
	DetailJSON    string
}

// WriteAudit appends an audit row inside an existing transaction, maintaining a
// global tamper-evident hash chain (chain_hash = sha256(prev_hash || payload)).
// Because all writes are serialized through the single writer, reading the last
// row inside the tx is race-free.
func WriteAudit(tx *sql.Tx, e AuditEntry) error {
	var prevID, prevHash string
	row := tx.QueryRow(`SELECT id, COALESCE(chain_hash,'') FROM audit_log ORDER BY at DESC, id DESC LIMIT 1`)
	_ = row.Scan(&prevID, &prevHash) // empty on first row

	id := core.NewID()
	at := core.NowMillis()
	payload := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s|%s", id, at, e.UserID, e.AttendeeID,
		e.Action, e.Entity, e.EntityID, e.DetailJSON)
	sum := sha256.Sum256([]byte(prevHash + payload))
	chainHash := hex.EncodeToString(sum[:])

	_, err := tx.Exec(`INSERT INTO audit_log
		(id, event_id, at, user_id, attendee_id, ip, action, entity, entity_id,
		 products_json, balance_before, balance_after, detail_json, prev_audit_id, chain_hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, nullStr(e.EventID), at, nullStr(e.UserID), nullStr(e.AttendeeID), nullStr(e.IP),
		e.Action, nullStr(e.Entity), nullStr(e.EntityID), nullStr(e.ProductsJSON),
		e.BalanceBefore, e.BalanceAfter, nullStr(e.DetailJSON), nullStr(prevID), chainHash)
	return err
}

// AppendLedger inserts an immutable balance_ledger row with hash chaining, and
// returns the new id. ledgerType is one of the schema's CHECK values.
// refPaymentID is UNIQUE in the ledger, giving structural double-discount
// prevention. Pass "" for a topup/adjustment with no payment.
func AppendLedger(tx *sql.Tx, attendeeID, ledgerType string, amount, balanceAfter int64,
	refOrderID, refPaymentID, userID string) (string, error) {

	var prevID, prevHash string
	row := tx.QueryRow(`SELECT id, COALESCE(chain_hash,'') FROM balance_ledger
		WHERE attendee_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, attendeeID)
	_ = row.Scan(&prevID, &prevHash)

	id := core.NewID()
	at := core.NowMillis()
	payload := fmt.Sprintf("%s|%s|%s|%d|%d|%s", id, attendeeID, ledgerType, amount, balanceAfter, refPaymentID)
	sum := sha256.Sum256([]byte(prevHash + payload))
	chainHash := hex.EncodeToString(sum[:])

	_, err := tx.Exec(`INSERT INTO balance_ledger
		(id, attendee_id, type, amount, balance_after, ref_order_id, ref_payment_id,
		 prev_ledger_id, chain_hash, user_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, attendeeID, ledgerType, amount, balanceAfter, nullStr(refOrderID), nullStr(refPaymentID),
		nullStr(prevID), chainHash, userID, at)
	return id, err
}

// VerifyAuditChain recomputes the audit hash chain and returns the first id
// where it breaks, or "" if intact.
func VerifyAuditChain(ctx context.Context, d *db.DB) (string, error) {
	rows, err := d.Reader.QueryContext(ctx,
		`SELECT id, at, user_id, attendee_id, action, entity, entity_id, detail_json, chain_hash
		 FROM audit_log ORDER BY at ASC, id ASC`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	prevHash := ""
	for rows.Next() {
		var id, action, chainHash string
		var at int64
		var userID, attendeeID, entity, entityID, detail sql.NullString
		if err := rows.Scan(&id, &at, &userID, &attendeeID, &action, &entity, &entityID, &detail, &chainHash); err != nil {
			return "", err
		}
		payload := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s|%s", id, at, userID.String, attendeeID.String,
			action, entity.String, entityID.String, detail.String)
		sum := sha256.Sum256([]byte(prevHash + payload))
		if hex.EncodeToString(sum[:]) != chainHash {
			return id, nil
		}
		prevHash = chainHash
	}
	return "", rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
