// Package backup creates and restores integrity-checked copies of the SQLite
// database. Backups are plain copies taken after a WAL checkpoint, with a
// recorded sha256 so integrity can be validated on restore.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"eventpos/internal/core"
	"eventpos/internal/db"
)

// Service performs backup/restore operations.
type Service struct {
	db  *db.DB
	dir string
}

// New creates a backup service writing into dir (created if missing).
func New(d *db.DB, dir string) (*Service, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Service{db: d, dir: dir}, nil
}

// Record describes a stored backup.
type Record struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"created_at"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
}

// Create checkpoints the WAL and copies the db file. kind is manual|auto.
func (s *Service) Create(ctx context.Context, kind, eventID string) (Record, error) {
	if err := s.db.Checkpoint(ctx); err != nil {
		return Record{}, fmt.Errorf("checkpoint: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	name := fmt.Sprintf("aeos-%s-%s.db", stamp, kind)
	dst := filepath.Join(s.dir, name)

	size, sum, err := copyAndHash(s.db.Path, dst)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		ID: core.NewID(), Path: dst, CreatedAt: core.NowMillis(),
		SizeBytes: size, SHA256: sum, Kind: kind, Status: "ok",
	}
	_, err = s.db.Writer.ExecContext(ctx,
		`INSERT INTO backups(id, path, created_at, size_bytes, sha256, kind, status, event_id)
		 VALUES (?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Path, rec.CreatedAt, rec.SizeBytes, rec.SHA256, rec.Kind, rec.Status, nullStr(eventID))
	return rec, err
}

// List returns recorded backups, newest first.
func (s *Service) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.Reader.QueryContext(ctx,
		`SELECT id, path, created_at, size_bytes, sha256, kind, status FROM backups ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.Path, &r.CreatedAt, &r.SizeBytes, &r.SHA256, &r.Kind, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Prune keeps the most recent keep AUTO backups, deleting older auto files and
// their records. Manual backups are never pruned. Best-effort: a missing file
// is ignored. Called after periodic backups so they don't accumulate forever.
func (s *Service) Prune(ctx context.Context, keep int) error {
	if keep < 1 {
		keep = 1
	}
	rows, err := s.db.Reader.QueryContext(ctx,
		`SELECT id, path FROM backups WHERE kind='auto' ORDER BY created_at DESC`)
	if err != nil {
		return err
	}
	type row struct{ id, path string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all[min(keep, len(all)):] {
		_ = os.Remove(r.path)
		if _, err := s.db.Writer.ExecContext(ctx, `DELETE FROM backups WHERE id=?`, r.id); err != nil {
			return err
		}
	}
	return nil
}

// Verify recomputes the sha256 of a backup file and compares to the record.
func (s *Service) Verify(ctx context.Context, id string) (bool, error) {
	var path, want string
	err := s.db.Reader.QueryRowContext(ctx, `SELECT path, sha256 FROM backups WHERE id = ?`, id).Scan(&path, &want)
	if err != nil {
		return false, core.ErrNotFound
	}
	_, got, err := hashFile(path)
	if err != nil {
		return false, err
	}
	return got == want, nil
}

func copyAndHash(src, dst string) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, "", err
	}
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
