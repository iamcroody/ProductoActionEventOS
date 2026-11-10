package backup_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"eventpos/internal/backup"
	"eventpos/internal/db"
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

// TestBackup_CreateAndList verifies that Create writes a backup file, records a
// non-empty SHA-256, and that List returns the record afterwards.
func TestBackup_CreateAndList(t *testing.T) {
	d := openTestDB(t)
	svc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}

	ctx := context.Background()
	rec, err := svc.Create(ctx, "manual", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// File must exist on disk.
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	// SHA-256 must have been recorded.
	if rec.SHA256 == "" {
		t.Fatal("Create returned empty SHA256")
	}
	if rec.ID == "" {
		t.Fatal("Create returned empty ID")
	}

	// List must return the newly created record.
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 backup record, got %d", len(list))
	}
	if list[0].ID != rec.ID {
		t.Errorf("List[0].ID %q != created ID %q", list[0].ID, rec.ID)
	}
	if list[0].SHA256 != rec.SHA256 {
		t.Errorf("List[0].SHA256 mismatch")
	}
}

// TestBackup_VerifyIntegrity confirms that Verify returns true for an untouched
// backup file and false after a single byte is appended (proving sha256
// integrity checking).
func TestBackup_VerifyIntegrity(t *testing.T) {
	d := openTestDB(t)
	svc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}

	ctx := context.Background()
	rec, err := svc.Create(ctx, "manual", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Untouched backup must verify.
	ok, err := svc.Verify(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Verify (intact): %v", err)
	}
	if !ok {
		t.Fatal("expected intact backup to verify as true")
	}

	// Corrupt the file by appending one byte.
	f, err := os.OpenFile(rec.Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open backup for corruption: %v", err)
	}
	_, writeErr := f.Write([]byte{0xFF})
	f.Close()
	if writeErr != nil {
		t.Fatalf("corrupt backup: %v", writeErr)
	}

	// Corrupted backup must not verify.
	ok, err = svc.Verify(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Verify (corrupted): %v", err)
	}
	if ok {
		t.Fatal("expected corrupted backup to verify as false")
	}
}

// TestBackup_RoundTrip confirms the backup file is a valid SQLite database.
// It opens the backed-up file directly and reads the schema_meta row that
// db.Open seeds at startup.
func TestBackup_RoundTrip(t *testing.T) {
	d := openTestDB(t)
	svc, err := backup.New(d, t.TempDir())
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}

	ctx := context.Background()
	rec, err := svc.Create(ctx, "auto", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Open the backup as a plain SQLite file (driver registered by the db import).
	bdb, err := sql.Open("sqlite", rec.Path)
	if err != nil {
		t.Fatalf("sql.Open backup: %v", err)
	}
	defer bdb.Close()

	var version int
	if err := bdb.QueryRowContext(ctx,
		`SELECT version FROM schema_meta WHERE id = 1`).Scan(&version); err != nil {
		t.Fatalf("read schema_meta from backup: %v", err)
	}
	if version < 1 {
		t.Fatalf("expected schema version >= 1, got %d", version)
	}
}
