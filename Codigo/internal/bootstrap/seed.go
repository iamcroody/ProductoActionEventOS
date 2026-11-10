// Package bootstrap seeds first-run data so the operator can log in immediately.
package bootstrap

import (
	"context"

	"eventpos/internal/auth"
	"eventpos/internal/core"
	"eventpos/internal/db"
)

// Default credentials for the central admin console. Memorable and on-brand:
// usuario "action", contraseña "croody". Created automatically so there is
// never a lock-out; the admin can change it or add operators from the console.
const (
	DefaultAdminUser   = "action"
	DefaultAdminSecret = "croody"
)

// SeedAdmin ensures the default "action" admin operator exists. It is
// idempotent: it creates the account only if it is missing, so existing data is
// preserved and you can never be locked out. Returns the created username/secret,
// or empty strings if the account already existed.
func SeedAdmin(ctx context.Context, d *db.DB) (username, secret string, err error) {
	var count int
	if err = d.Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE username = ?`, DefaultAdminUser).Scan(&count); err != nil {
		return "", "", err
	}
	if count > 0 {
		return "", "", nil
	}
	// The same hash backs both pin_hash and password_hash so login with the
	// secret works whether the UI sends it as a PIN or a password.
	hash, err := auth.HashSecret(DefaultAdminSecret)
	if err != nil {
		return "", "", err
	}
	_, err = d.Writer.ExecContext(ctx,
		`INSERT INTO users(id, username, full_name, role, pin_hash, password_hash, active, created_at)
		 VALUES (?,?,?,?,?,?,1,?)`,
		core.NewID(), DefaultAdminUser, "Administrador", "admin", hash, hash, core.NowMillis())
	if err != nil {
		return "", "", err
	}
	return DefaultAdminUser, DefaultAdminSecret, nil
}
