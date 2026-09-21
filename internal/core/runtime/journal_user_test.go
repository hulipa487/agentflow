package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func journalStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func record(t *testing.T, s *Store, e JournalEntry) {
	t.Helper()
	if err := s.RecordMessage(context.Background(), e); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// The per-user audit view: rows are attributed to the profile behind them, and
// an unregistered sender's rows stay out of every user's view.
func TestJournalAttributesRowsToUsers(t *testing.T) {
	s := journalStore(t)
	record(t, s, JournalEntry{ID: "m1", Ts: 100, Direction: "in", Status: "routed",
		Channel: "telegram", Sender: "user:u_1", UserUUID: "u_1", Agent: "bot", Text: "hi"})
	record(t, s, JournalEntry{ID: "m1", Ts: 101, Direction: "out", Status: "delivered",
		Channel: "telegram", Sender: "chat-1", UserUUID: "u_1", Agent: "bot", SessionID: "s1", Text: "hello"})
	record(t, s, JournalEntry{ID: "m2", Ts: 102, Direction: "in", Status: "routed",
		Channel: "telegram", Sender: "user:i_anon", UserUUID: "", Agent: "bot", Text: "who?"})
	record(t, s, JournalEntry{ID: "m3", Ts: 103, Direction: "in", Status: "routed",
		Channel: "telegram", Sender: "user:u_2", UserUUID: "u_2", Agent: "bot", Text: "other"})

	ctx := context.Background()
	rows, err := s.ListMessages(ctx, JournalFilter{UserUUID: "u_1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("u_1 should have two rows, got %d (%+v)", len(rows), rows)
	}
	// Newest first, and both directions present.
	if rows[0].Direction != "out" || rows[0].Text != "hello" {
		t.Fatalf("newest first expected: %+v", rows[0])
	}
	if rows[1].Text != "hi" {
		t.Fatalf("inbound row missing: %+v", rows[1])
	}
	for _, r := range rows {
		if r.UserUUID != "u_1" {
			t.Fatalf("another user's row leaked in: %+v", r)
		}
	}

	// An unregistered sender's traffic belongs to nobody.
	if rows, _ := s.ListMessages(ctx, JournalFilter{UserUUID: "i_anon"}); len(rows) != 0 {
		t.Fatalf("identity ids are not profiles: %+v", rows)
	}
	// No filter: everything, newest first.
	all, err := s.ListMessages(ctx, JournalFilter{})
	if err != nil || len(all) != 4 {
		t.Fatalf("unfiltered list: %d err=%v", len(all), err)
	}
	if all[0].Ts != 103 {
		t.Fatalf("unfiltered list must be newest first: %+v", all[0])
	}

	// Direction and window filters compose.
	onlyIn, err := s.ListMessages(ctx, JournalFilter{Direction: "in", Since: 102})
	if err != nil || len(onlyIn) != 2 {
		t.Fatalf("direction+since filter: %d err=%v (%+v)", len(onlyIn), err, onlyIn)
	}
	if onlyIn[0].Ts != 103 || onlyIn[1].Ts != 102 {
		t.Fatalf("window filter wrong: %+v", onlyIn)
	}

	// Attachments and provenance survive the round trip.
	record(t, s, JournalEntry{ID: "m4", Ts: 104, Direction: "in", Status: "routed",
		UserUUID: "u_1", Provenance: map[string]any{"kind": "user"}, Text: "with provenance"})
	rows, _ = s.ListMessages(ctx, JournalFilter{UserUUID: "u_1", Limit: 1})
	if len(rows) != 1 || rows[0].Provenance["kind"] != "user" {
		t.Fatalf("provenance or limit lost: %+v", rows)
	}
}

// The column arrived after the journal shipped, so an existing database — the
// shape a live deployment has — must be altered in place rather than silently
// losing attribution.
func TestJournalMigratesAnExistingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// The pre-attribution schema, with a row in it.
	if _, err := db.Exec(`
		CREATE TABLE message_journal (
			id TEXT NOT NULL, ts INTEGER NOT NULL, direction TEXT NOT NULL,
			status TEXT NOT NULL, channel TEXT, chat TEXT, sender TEXT, agent TEXT,
			session_id TEXT, type TEXT, text TEXT, attachments_json TEXT,
			provenance_json TEXT, err TEXT
		);
		INSERT INTO message_journal (id, ts, direction, status, sender, text)
		VALUES ('old1', 50, 'in', 'routed', 'user:u_legacy', 'before');
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open after migration: %v", err)
	}
	defer s.Close()

	// New rows carry attribution...
	record(t, s, JournalEntry{ID: "new1", Ts: 200, Direction: "in", Status: "routed",
		UserUUID: "u_1", Text: "after"})
	rows, err := s.ListMessages(context.Background(), JournalFilter{UserUUID: "u_1"})
	if err != nil {
		t.Fatalf("list after migration: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "new1" {
		t.Fatalf("attribution lost after migration: %+v", rows)
	}
	// ...and the pre-existing row survived, unattributed.
	old, err := s.ListMessages(context.Background(), JournalFilter{Since: 1, Until: 100})
	if err != nil || len(old) != 1 || old[0].ID != "old1" || old[0].UserUUID != "" {
		t.Fatalf("legacy row should survive unattributed: %+v err=%v", old, err)
	}

	// Reopening must not fail (the ALTER runs once).
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
}
