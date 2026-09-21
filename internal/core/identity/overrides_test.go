package identity

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Overrides round-trip, and clearing one puts the person back on whatever the
// agent is configured with.
func TestProfileOverridesRoundTrip(t *testing.T) {
	r := newTestRegistry(t)
	p, err := r.CreateProfile("Oscar", "o@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A fresh profile inherits everything.
	if m, a, err := r.Overrides(p.UserID); err != nil || m != "" || a != "" {
		t.Fatalf("a new profile should have no overrides: %q %q err=%v", m, a, err)
	}
	model, add := "premium", "Speak Cantonese unless asked otherwise."
	if err := r.SetOverrides(p.UserID, &model, &add); err != nil {
		t.Fatalf("set: %v", err)
	}
	if m, a, err := r.Overrides(p.UserID); err != nil || m != model || a != add {
		t.Fatalf("overrides = %q %q err=%v, want %q %q", m, a, err, model, add)
	}
	// They ride on the profile the console reads, too.
	got, ok, err := r.Get(p.UserID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Model != model || got.InstructionsAppend != add {
		t.Fatalf("profile did not carry the overrides: %+v", got)
	}
	// A nil argument leaves a field alone; an empty one clears it.
	only := "standard"
	if err := r.SetOverrides(p.UserID, &only, nil); err != nil {
		t.Fatalf("set model only: %v", err)
	}
	if m, a, _ := r.Overrides(p.UserID); m != only || a != add {
		t.Fatalf("a nil argument must not clear the other field: %q %q", m, a)
	}
	empty := ""
	if err := r.SetOverrides(p.UserID, &empty, &empty); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if m, a, _ := r.Overrides(p.UserID); m != "" || a != "" {
		t.Fatalf("clearing should leave nothing: %q %q", m, a)
	}
	// And an unknown profile is an error, not a silent no-op.
	if err := r.SetOverrides("u_nope", &model, nil); err == nil {
		t.Fatal("setting overrides on an unknown profile must fail")
	}
}

// An existing database from before the overrides existed gets the columns on
// the next boot: CREATE TABLE IF NOT EXISTS alone would leave it without them.
func TestOverridesMigrateAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// The profiles table as it shipped before this feature.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE profiles (
		user_id        TEXT PRIMARY KEY,
		display_name   TEXT NOT NULL DEFAULT '',
		email          TEXT NOT NULL DEFAULT '',
		tokens_per_day INTEGER NOT NULL DEFAULT 0,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO profiles (user_id, display_name, created_at, updated_at)
		VALUES ('u_old', 'Old Timer', 1, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r := openAt(t, path)
	// The pre-existing row survives, with empty overrides.
	model, add, err := r.Overrides("u_old")
	if err != nil || model != "" || add != "" {
		t.Fatalf("migrated row: %q %q err=%v", model, add, err)
	}
	// And the new columns are writable.
	want := "premium"
	if err := r.SetOverrides("u_old", &want, nil); err != nil {
		t.Fatalf("set after migration: %v", err)
	}
	if m, _, _ := r.Overrides("u_old"); m != want {
		t.Fatalf("override did not stick after migration: %q", m)
	}
}

// The session layer asks for overrides on every loop turn, so the answer is
// cached; a change reaches the asking instance within the window.
func TestSettingsSourceCachesAndExpires(t *testing.T) {
	r := newTestRegistry(t)
	p, _ := r.CreateProfile("Oscar", "")
	model := "premium"
	if err := r.SetOverrides(p.UserID, &model, nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	src := NewSettingsSource(r, 40*time.Millisecond)
	ctx := context.Background()

	if m, _, err := src.Settings(ctx, p.UserID); err != nil || m != model {
		t.Fatalf("first read: %q err=%v", m, err)
	}
	// A change is not seen until the entry expires — that is the trade for not
	// querying the store on every turn.
	other := "standard"
	if err := r.SetOverrides(p.UserID, &other, nil); err != nil {
		t.Fatalf("set again: %v", err)
	}
	if m, _, _ := src.Settings(ctx, p.UserID); m != model {
		t.Fatalf("the cache should still answer with the old value, got %q", m)
	}
	time.Sleep(60 * time.Millisecond)
	if m, _, err := src.Settings(ctx, p.UserID); err != nil || m != other {
		t.Fatalf("after the window the new value should be read: %q err=%v", m, err)
	}
	// An unregistered user has no overrides and is not an error.
	if m, a, err := src.Settings(ctx, "u_nobody"); err != nil || m != "" || a != "" {
		t.Fatalf("unknown user: %q %q err=%v", m, a, err)
	}
	// An empty user id (engine work with no person behind it) short-circuits.
	if m, a, err := src.Settings(ctx, ""); err != nil || m != "" || a != "" {
		t.Fatalf("no user: %q %q err=%v", m, a, err)
	}
}

// A profile read must never fail a person's turn: an unreadable store leaves
// the session on what was last known rather than erroring.
func TestSettingsSourceFallsBackToTheLastKnownValue(t *testing.T) {
	r := newTestRegistry(t)
	p, _ := r.CreateProfile("Oscar", "")
	model := "premium"
	if err := r.SetOverrides(p.UserID, &model, nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	src := NewSettingsSource(r, time.Millisecond)
	ctx := context.Background()
	if m, _, err := src.Settings(ctx, p.UserID); err != nil || m != model {
		t.Fatalf("first read: %q err=%v", m, err)
	}
	time.Sleep(5 * time.Millisecond)
	// The store goes away, which is what an unreachable database looks like.
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if m, _, err := src.Settings(ctx, p.UserID); err != nil || m != model {
		t.Fatalf("a failed read should answer with the last known value: %q err=%v", m, err)
	}
}
