package writer

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"eventpos/internal/db"
)

// openTestDB spins up a real on-disk SQLite (WAL) in a temp dir.
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

// TestWriterSerializesIncrements proves that many goroutines hammering a
// read-modify-write through the writer never lose an update — the classic
// race that would corrupt a balance. Without serialization this would drop
// increments; with the single-writer it must total exactly N.
func TestWriterSerializesIncrements(t *testing.T) {
	d := openTestDB(t)
	w := New(d.Writer)
	defer w.Close()

	ctx := context.Background()
	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		_, err := tx.Exec(`CREATE TABLE counter(id INTEGER PRIMARY KEY, n INTEGER)`)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(`INSERT INTO counter(id, n) VALUES (1, 0)`)
		return nil, err
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	const goroutines, perG = 20, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				_, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
					var n int
					if err := tx.QueryRow(`SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
						return nil, err
					}
					_, err := tx.Exec(`UPDATE counter SET n = ? WHERE id = 1`, n+1)
					return nil, err
				})
				if err != nil {
					t.Errorf("do: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	var got int
	if err := d.Reader.QueryRow(`SELECT n FROM counter WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := goroutines * perG; got != want {
		t.Fatalf("lost updates: got %d, want %d", got, want)
	}
}

// TestWriterRollback verifies a failing command rolls back fully.
func TestWriterRollback(t *testing.T) {
	d := openTestDB(t)
	w := New(d.Writer)
	defer w.Close()
	ctx := context.Background()

	if _, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		return tx.Exec(`CREATE TABLE t(x INTEGER)`)
	}); err != nil {
		t.Fatal(err)
	}
	// Insert a row then fail — the insert must be rolled back.
	_, err := w.Do(ctx, func(tx *sql.Tx) (any, error) {
		if _, err := tx.Exec(`INSERT INTO t(x) VALUES (1)`); err != nil {
			return nil, err
		}
		return nil, sql.ErrConnDone // force rollback
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var count int
	if err := d.Reader.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rollback failed: %d rows", count)
	}
}
