// Package db owns the SQLite connection lifecycle for EventPOS.
//
// It opens two *sql.DB handles against the same file:
//
//   - Writer: SetMaxOpenConns(1) — every mutation funnels through a single
//     physical connection. Combined with the single-writer goroutine in
//     package writer, this serializes all writes in-process and removes any
//     possibility of interleaved balance/stock mutations.
//   - Reader: a pool — WAL mode lets readers run concurrently with the writer.
//
// Money is stored as INTEGER pesos; timestamps as INTEGER unix-millis UTC.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is bumped whenever schema.sql changes in a way that needs a
// fresh apply. The bootstrap re-runs the (idempotent) schema when the stored
// version is behind.
const schemaVersion = 1

// DB bundles the writer and reader handles plus the on-disk path.
type DB struct {
	Writer *sql.DB
	Reader *sql.DB
	Path   string
}

// pragmas applied to every new connection. synchronous=NORMAL is the
// recommended durable+fast setting under WAL.
const pragmas = `PRAGMA journal_mode=WAL;` +
	`PRAGMA busy_timeout=5000;` +
	`PRAGMA foreign_keys=ON;` +
	`PRAGMA synchronous=NORMAL;` +
	`PRAGMA wal_autocheckpoint=1000;` +
	`PRAGMA temp_store=MEMORY;`

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*DB, error) {
	base := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)", path)
	// The writer uses _txlock=immediate so every transaction takes the SQLite
	// write lock up front (fail-fast instead of mid-transaction SQLITE_BUSY).
	writerDSN := base + "&_txlock=immediate"

	writer, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	reader, err := sql.Open("sqlite", base)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	reader.SetMaxOpenConns(8)
	reader.SetMaxIdleConns(8)

	d := &DB{Writer: writer, Reader: reader, Path: path}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.applyPragmas(ctx); err != nil {
		d.Close()
		return nil, err
	}
	if err := d.bootstrap(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) applyPragmas(ctx context.Context) error {
	for _, h := range []*sql.DB{d.Writer, d.Reader} {
		if _, err := h.ExecContext(ctx, pragmas); err != nil {
			return fmt.Errorf("apply pragmas: %w", err)
		}
	}
	return nil
}

// additiveMigrations are idempotent ALTER TABLE ADD COLUMN statements applied
// to pre-existing databases (CREATE TABLE IF NOT EXISTS does not add columns to
// tables that already exist). A "duplicate column name" error is expected and
// ignored when the column is already present.
var additiveMigrations = []string{
	`ALTER TABLE products ADD COLUMN cost INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE order_items ADD COLUMN unit_cost INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE order_items ADD COLUMN delivered_qty INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE attendees ADD COLUMN email TEXT`,
	`ALTER TABLE attendees ADD COLUMN no_redeem INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE orders ADD COLUMN source TEXT NOT NULL DEFAULT 'operator'`,
}

// bootstrap applies the embedded schema if the stored version is behind.
func (d *DB) bootstrap(ctx context.Context) error {
	if _, err := d.Writer.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range additiveMigrations {
		if _, err := d.Writer.ExecContext(ctx, m); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migration %q: %w", m, err)
		}
	}
	var current int
	row := d.Writer.QueryRowContext(ctx, `SELECT version FROM schema_meta WHERE id = 1`)
	switch err := row.Scan(&current); err {
	case sql.ErrNoRows:
		_, err = d.Writer.ExecContext(ctx,
			`INSERT INTO schema_meta(id, version) VALUES (1, ?)`, schemaVersion)
		return err
	case nil:
		if current < schemaVersion {
			_, err = d.Writer.ExecContext(ctx,
				`UPDATE schema_meta SET version = ? WHERE id = 1`, schemaVersion)
			return err
		}
		return nil
	default:
		return fmt.Errorf("read schema version: %w", err)
	}
}

// Checkpoint forces a WAL checkpoint (TRUNCATE) — used before backups so the
// main db file is self-contained.
func (d *DB) Checkpoint(ctx context.Context) error {
	_, err := d.Writer.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE);`)
	return err
}

// Close closes both handles.
func (d *DB) Close() error {
	var first error
	if d.Reader != nil {
		if err := d.Reader.Close(); err != nil {
			first = err
		}
	}
	if d.Writer != nil {
		if err := d.Writer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
