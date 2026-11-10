// Package config provides typed access to the app_config key/value store.
package config

import (
	"context"
	"database/sql"
	"errors"

	"eventpos/internal/core"
	"eventpos/internal/db"
)

// Well-known keys.
const (
	KeyCurrency     = "currency"
	KeyDefaultTopup = "default_topup"
	KeyOrgName      = "org_name"
)

// Store reads/writes app_config.
type Store struct{ db *db.DB }

func New(d *db.DB) *Store { return &Store{db: d} }

// EnsureDefaults seeds first-run configuration.
func (s *Store) EnsureDefaults(ctx context.Context) error {
	defaults := map[string]string{
		KeyCurrency:     "COP",
		KeyDefaultTopup: "50000",
		KeyOrgName:      "ABC Corporation",
	}
	for k, v := range defaults {
		if _, err := s.Get(ctx, k); errors.Is(err, core.ErrNotFound) {
			if err := s.Set(ctx, k, v, "system"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Get returns the value for key.
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.Reader.QueryRowContext(ctx, `SELECT value FROM app_config WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	return v, err
}

// All returns the full config map.
func (s *Store) All(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `SELECT key, value FROM app_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Set upserts a config value.
func (s *Store) Set(ctx context.Context, key, value, by string) error {
	_, err := s.db.Writer.ExecContext(ctx,
		`INSERT INTO app_config(key, value, updated_at, updated_by) VALUES (?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		key, value, core.NowMillis(), by)
	return err
}
