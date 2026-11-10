package config_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"eventpos/internal/config"
	"eventpos/internal/core"
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

// TestEnsureDefaults_SeedsValues verifies that calling EnsureDefaults on a
// fresh database inserts the expected defaults: currency=COP,
// default_topup=50000, and a non-empty org_name.
func TestEnsureDefaults_SeedsValues(t *testing.T) {
	d := openTestDB(t)
	s := config.New(d)
	ctx := context.Background()

	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}

	cases := []struct{ key, want string }{
		{config.KeyCurrency, "COP"},
		{config.KeyDefaultTopup, "50000"},
	}
	for _, c := range cases {
		got, err := s.Get(ctx, c.key)
		if err != nil {
			t.Fatalf("Get(%q): %v", c.key, err)
		}
		if got != c.want {
			t.Errorf("Get(%q) = %q, want %q", c.key, got, c.want)
		}
	}

	// org_name must be non-empty (exact default value is "ABC Corporation").
	orgName, err := s.Get(ctx, config.KeyOrgName)
	if err != nil {
		t.Fatalf("Get(org_name): %v", err)
	}
	if orgName == "" {
		t.Error("org_name should be non-empty after EnsureDefaults")
	}
}

// TestEnsureDefaults_DoesNotOverwrite verifies that a second call to
// EnsureDefaults does not overwrite values that were already set.
func TestEnsureDefaults_DoesNotOverwrite(t *testing.T) {
	d := openTestDB(t)
	s := config.New(d)
	ctx := context.Background()

	// Seed defaults.
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatalf("first EnsureDefaults: %v", err)
	}
	// Override the currency.
	if err := s.Set(ctx, config.KeyCurrency, "USD", "test"); err != nil {
		t.Fatalf("Set currency: %v", err)
	}

	// Run EnsureDefaults again — it must not clobber the user-set value.
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatalf("second EnsureDefaults: %v", err)
	}

	got, err := s.Get(ctx, config.KeyCurrency)
	if err != nil {
		t.Fatalf("Get currency: %v", err)
	}
	if got != "USD" {
		t.Errorf("EnsureDefaults overwrote existing value: got %q, want %q", got, "USD")
	}
}

// TestSetAndGet verifies the basic round-trip: Set followed by Get returns the
// value that was stored, including after an upsert (second Set overwrites).
func TestSetAndGet(t *testing.T) {
	d := openTestDB(t)
	s := config.New(d)
	ctx := context.Background()

	if err := s.Set(ctx, "venue", "Hall A", "test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "venue")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "Hall A" {
		t.Errorf("Get = %q, want %q", got, "Hall A")
	}

	// Upsert: Set again with a different value.
	if err := s.Set(ctx, "venue", "Hall B", "test"); err != nil {
		t.Fatalf("Set (upsert): %v", err)
	}
	got, err = s.Get(ctx, "venue")
	if err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if got != "Hall B" {
		t.Errorf("Get after upsert = %q, want %q", got, "Hall B")
	}
}

// TestAll_ReturnsFullMap verifies that All returns a map containing every key
// that was previously set, with the correct values.
func TestAll_ReturnsFullMap(t *testing.T) {
	d := openTestDB(t)
	s := config.New(d)
	ctx := context.Background()

	// Seed the standard defaults so we have known keys.
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}
	// Add a custom key as well.
	if err := s.Set(ctx, "tax_rate", "19", "test"); err != nil {
		t.Fatalf("Set tax_rate: %v", err)
	}

	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	required := map[string]string{
		config.KeyCurrency:     "COP",
		config.KeyDefaultTopup: "50000",
		"tax_rate":             "19",
	}
	for k, want := range required {
		got, ok := all[k]
		if !ok {
			t.Errorf("All missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("All[%q] = %q, want %q", k, got, want)
		}
	}
	// Must have at least 4 entries (3 defaults + 1 custom).
	if len(all) < 4 {
		t.Errorf("All returned %d entries, want >= 4", len(all))
	}
}

// TestGet_MissingKey_ReturnsErrNotFound verifies that Get of an unknown key
// returns core.ErrNotFound (and not some other error or a zero value).
func TestGet_MissingKey_ReturnsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	s := config.New(d)
	ctx := context.Background()

	_, err := s.Get(ctx, "key_that_does_not_exist")
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want core.ErrNotFound", err)
	}
}
