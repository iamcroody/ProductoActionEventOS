package app_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"eventpos/internal/app"
	"eventpos/internal/core"
	"eventpos/internal/db"
	"eventpos/internal/writer"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newWriter(t *testing.T, d *db.DB) *writer.Writer {
	t.Helper()
	w := writer.New(d.Writer)
	t.Cleanup(func() { w.Close() })
	return w
}

// TestWriteAudit_HashChainIntact writes several audit entries via WriteAudit
// and asserts that VerifyAuditChain returns "" (intact).
func TestWriteAudit_HashChainIntact(t *testing.T) {
	d := openTestDB(t)
	w := newWriter(t, d)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		n := i // capture loop variable
		_, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
			return nil, app.WriteAudit(tx, app.AuditEntry{
				Action:     "test.login",
				UserID:     "user-1",
				DetailJSON: fmt.Sprintf(`{"step":%d}`, n),
			})
		})
		if err != nil {
			t.Fatalf("WriteAudit %d: %v", i, err)
		}
	}

	broken, err := app.VerifyAuditChain(ctx, d)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if broken != "" {
		t.Fatalf("expected intact chain, got broken id %q", broken)
	}
}

// TestVerifyAuditChain_TamperDetection proves both halves of tamper detection:
//  1. An honest chain passes verification.
//  2. A chain that contains a row with a deliberately wrong chain_hash is
//     detected, and VerifyAuditChain returns that row's id.
//
// NOTE: The audit_log table has an immutability trigger that blocks UPDATE.
// To exercise the broken-chain case we open a second database and INSERT a
// row with a fabricated (wrong) chain_hash directly via raw SQL.
func TestVerifyAuditChain_TamperDetection(t *testing.T) {
	ctx := context.Background()

	// Part 1 — intact chain.
	d1 := openTestDB(t)
	w1 := newWriter(t, d1)
	for i := 0; i < 3; i++ {
		_, err := w1.Do(ctx, func(tx *sql.Tx) (any, error) {
			return nil, app.WriteAudit(tx, app.AuditEntry{Action: "audit.test"})
		})
		if err != nil {
			t.Fatalf("write entry %d: %v", i, err)
		}
	}
	broken, err := app.VerifyAuditChain(ctx, d1)
	if err != nil {
		t.Fatalf("VerifyAuditChain (intact): %v", err)
	}
	if broken != "" {
		t.Fatalf("intact chain reported broken at %q", broken)
	}

	// Part 2 — fresh db; insert a single audit row with a wrong chain_hash.
	// The triggers only block UPDATE and DELETE, so direct INSERT is allowed.
	d2 := openTestDB(t)
	fakeID := core.NewID()
	_, err = d2.Writer.ExecContext(ctx,
		`INSERT INTO audit_log(id, at, action, chain_hash) VALUES (?,?,?,?)`,
		fakeID, core.NowMillis(), "audit.tamper",
		// All-zeros is an astronomically unlikely correct hash.
		"0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("insert tampered row: %v", err)
	}

	broken, err = app.VerifyAuditChain(ctx, d2)
	if err != nil {
		t.Fatalf("VerifyAuditChain (tampered): %v", err)
	}
	if broken != fakeID {
		t.Fatalf("expected broken id %q, got %q", fakeID, broken)
	}
}

// TestAppendLedger covers:
//  1. topup then debit produce correct balance_after snapshots.
//  2. The second ledger row's prev_ledger_id points to the first (per-attendee
//     hash chain is maintained).
//  3. A duplicate ref_payment_id is rejected with an error (structural
//     double-discount prevention via UNIQUE constraint).
func TestAppendLedger(t *testing.T) {
	d := openTestDB(t)
	w := newWriter(t, d)
	ctx := context.Background()

	// Prerequisites: one event and one attendee (foreign-key chain).
	eventID := core.NewID()
	attendeeID := core.NewID()
	_, err := d.Writer.ExecContext(ctx,
		`INSERT INTO events(id, name, status, default_topup, split_policy, created_at)
		 VALUES (?,?,?,?,?,?)`,
		eventID, "Test Event", "draft", 50000, "cascade", core.NowMillis())
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	_, err = d.Writer.ExecContext(ctx,
		`INSERT INTO attendees(id, event_id, qr_token, display_name, balance, balance_version, status, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		attendeeID, eventID, core.NewID(), "Alice", 0, 0, "active", core.NowMillis())
	if err != nil {
		t.Fatalf("insert attendee: %v", err)
	}

	// Topup: +50000 → balance_after = 50000.
	raw, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		return app.AppendLedger(tx, attendeeID, "topup", 50000, 50000, "", "", "system")
	})
	if err != nil {
		t.Fatalf("topup: %v", err)
	}
	topupID := raw.(string)

	// Debit: -20000 → balance_after = 30000; unique ref_payment_id = "pay-001".
	raw, err = w.Do(ctx, func(tx *sql.Tx) (any, error) {
		return app.AppendLedger(tx, attendeeID, "debit", -20000, 30000, "", "pay-001", "system")
	})
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	debitID := raw.(string)

	// Read back both rows and verify snapshots + chain linkage.
	type row struct {
		id, typ   string
		amount    int64
		balAfter  int64
		prevID    sql.NullString
		chainHash string
	}
	qrows, err := d.Reader.QueryContext(ctx,
		`SELECT id, type, amount, balance_after, prev_ledger_id, chain_hash
		 FROM balance_ledger WHERE attendee_id = ? ORDER BY created_at ASC, id ASC`, attendeeID)
	if err != nil {
		t.Fatalf("query ledger: %v", err)
	}
	defer qrows.Close()
	var ledger []row
	for qrows.Next() {
		var r row
		if err := qrows.Scan(&r.id, &r.typ, &r.amount, &r.balAfter, &r.prevID, &r.chainHash); err != nil {
			t.Fatal(err)
		}
		ledger = append(ledger, r)
	}
	if err := qrows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(ledger) != 2 {
		t.Fatalf("want 2 ledger rows, got %d", len(ledger))
	}

	// Topup row checks.
	if ledger[0].id != topupID {
		t.Errorf("topup id mismatch: %s != %s", ledger[0].id, topupID)
	}
	if ledger[0].typ != "topup" || ledger[0].amount != 50000 || ledger[0].balAfter != 50000 {
		t.Errorf("topup row wrong: type=%s amount=%d balAfter=%d",
			ledger[0].typ, ledger[0].amount, ledger[0].balAfter)
	}

	// Debit row checks.
	if ledger[1].id != debitID {
		t.Errorf("debit id mismatch: %s != %s", ledger[1].id, debitID)
	}
	if ledger[1].typ != "debit" || ledger[1].amount != -20000 || ledger[1].balAfter != 30000 {
		t.Errorf("debit row wrong: type=%s amount=%d balAfter=%d",
			ledger[1].typ, ledger[1].amount, ledger[1].balAfter)
	}

	// Per-attendee hash chain: debit's prev_ledger_id must point at topup.
	if !ledger[1].prevID.Valid || ledger[1].prevID.String != topupID {
		t.Errorf("debit prev_ledger_id %q, want %q", ledger[1].prevID.String, topupID)
	}
	if ledger[0].chainHash == "" || ledger[1].chainHash == "" {
		t.Error("chain_hash must not be empty")
	}
	if ledger[0].chainHash == ledger[1].chainHash {
		t.Error("consecutive chain_hash values must differ")
	}

	// Duplicate ref_payment_id "pay-001" must be rejected (UNIQUE constraint =
	// structural double-discount prevention).
	_, err = w.Do(ctx, func(tx *sql.Tx) (any, error) {
		return app.AppendLedger(tx, attendeeID, "debit", -5000, 25000, "", "pay-001", "system")
	})
	if err == nil {
		t.Fatal("expected UNIQUE violation for duplicate ref_payment_id, got nil")
	}
}
