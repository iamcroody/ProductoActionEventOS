// Package httpx holds small HTTP helpers shared by every module: JSON
// encoding, domain-error -> status mapping, client IP extraction.
package httpx

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"eventpos/internal/core"
)

// JSON writes v as JSON with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Error maps a domain error to an HTTP status and a JSON body.
func Error(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, core.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, core.ErrValidation), errors.Is(err, core.ErrInsufficientStock),
		errors.Is(err, core.ErrInsufficientFunds), errors.Is(err, core.ErrCashNotConfirmed),
		errors.Is(err, core.ErrNoActiveEvent), errors.Is(err, core.ErrNoOpenCashSession):
		status = http.StatusBadRequest
	case errors.Is(err, core.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrDuplicate):
		status = http.StatusConflict
	}
	JSON(w, status, map[string]string{"error": err.Error()})
}

// Decode reads a JSON request body into v.
func Decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return core.ErrValidation
	}
	return nil
}

// ClientIP returns the best-effort client IP for audit logging.
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
