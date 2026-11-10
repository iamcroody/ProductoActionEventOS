// Package core holds tiny shared primitives used across EventPOS modules:
// time-ordered IDs, millisecond timestamps, and common errors.
package core

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// MaxMoney is the upper bound (in COP) accepted for any single money input
// (price, cost, top-up, tip pool). It is far above any real event figure (1
// billón COP) yet small enough that price·qty (qty capped at 100000) and sums
// across a cart can never overflow int64. Inputs above this are rejected with
// ErrValidation, preventing integer-overflow money bugs.
const MaxMoney int64 = 1_000_000_000_000

// Common domain errors. Handlers map these to HTTP status codes.
var (
	ErrNotFound          = errors.New("no encontrado")
	ErrInsufficientStock = errors.New("stock insuficiente")
	ErrInsufficientFunds = errors.New("saldo insuficiente")
	ErrCashNotConfirmed  = errors.New("excedente en efectivo no confirmado")
	ErrConflict          = errors.New("conflicto de concurrencia")
	ErrValidation        = errors.New("datos inválidos")
	ErrNoActiveEvent     = errors.New("no hay evento activo")
	ErrEventLocked       = errors.New("evento iniciado: no se puede modificar")
	ErrNoOpenCashSession = errors.New("no hay caja abierta")
	ErrForbidden         = errors.New("no autorizado")
	ErrDuplicate         = errors.New("registro duplicado")
)

// NewID returns a time-ordered UUIDv7 string. Time ordering gives good index
// locality for the high-volume append-only tables (ledger, audit, movements).
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 only errors if the system clock/entropy fails; fall back to V4.
		return uuid.NewString()
	}
	return id.String()
}

// NowMillis returns the current UTC time as unix milliseconds.
func NowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}
