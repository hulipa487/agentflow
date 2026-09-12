package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"agentflow/internal/core/media"
)

// The message journal is the core-owned audit trail: every inbound message
// (at the router, before routing) and every outbound reply (at the session
// egress, with its delivery result) is persisted here. It lives in the
// runtime store — core operational state — precisely so that no loop can
// skip, edit, or forge it; that property is what makes it an audit trail
// rather than a log a user script maintains.
//
// Media is never stored here: attachments are journaled as their small part
// descriptors (type, mime, handle), which join to the blob store by handle.

// JournalEntry is one journaled message. Direction is "in" (router ingress)
// or "out" (session egress); Status records what happened to it.
type JournalEntry struct {
	ID          string         // message id (stamped at ingress if absent)
	Ts          int64          // unix seconds
	Direction   string         // in | out
	Status      string         // in: routed | dropped_queue; out: delivered | failed | blocked_safety
	Channel     string         //
	Chat        string         // channel chat id, when known
	Sender      string         // msg.from (in) or recipient (out)
	Agent       string         //
	SessionID   string         // out only
	Type        string         // message type (user|timer|agent|...)
	Text        string         //
	Attachments []media.Part   // descriptors only (handles, never bytes)
	Provenance  map[string]any // in only
	Err         string         // delivery error detail (out, failed)
}

// RecordMessage appends one entry to the journal.
func (s *Store) RecordMessage(ctx context.Context, e JournalEntry) error {
	atts, err := json.Marshal(e.Attachments)
	if err != nil {
		return err
	}
	prov, err := json.Marshal(e.Provenance)
	if err != nil {
		return err
	}
	ts := e.Ts
	if ts == 0 {
		ts = time.Now().Unix()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO message_journal
			(id, ts, direction, status, channel, chat, sender, agent, session_id,
			 type, text, attachments_json, provenance_json, err)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, ts, e.Direction, e.Status, e.Channel, e.Chat, e.Sender, e.Agent,
		e.SessionID, e.Type, e.Text, string(atts), string(prov), e.Err)
	return err
}

// JournalFilter narrows ListMessages. Zero fields match everything.
type JournalFilter struct {
	Channel   string
	SessionID string
	Sender    string
	Since     int64 // unix seconds; 0 = no lower bound
	Limit     int   // default 100, capped at 1000
}

// ListMessages returns journal rows newest-first matching the filter.
func (s *Store) ListMessages(ctx context.Context, f JournalFilter) ([]JournalEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q := `SELECT id, ts, direction, status, channel, chat, sender, agent, session_id,
	             type, text, attachments_json, provenance_json, err
	      FROM message_journal WHERE 1=1`
	args := []any{}
	if f.Channel != "" {
		q += ` AND channel = ?`
		args = append(args, f.Channel)
	}
	if f.SessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, f.SessionID)
	}
	if f.Sender != "" {
		q += ` AND sender = ?`
		args = append(args, f.Sender)
	}
	if f.Since > 0 {
		q += ` AND ts >= ?`
		args = append(args, f.Since)
	}
	q += ` ORDER BY ts DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var atts, prov string
		if err := rows.Scan(&e.ID, &e.Ts, &e.Direction, &e.Status, &e.Channel,
			&e.Chat, &e.Sender, &e.Agent, &e.SessionID, &e.Type, &e.Text,
			&atts, &prov, &e.Err); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(atts), &e.Attachments)
		_ = json.Unmarshal([]byte(prov), &e.Provenance)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneMessages deletes journal rows older than cutoff, returning the count.
func (s *Store) PruneMessages(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM message_journal WHERE ts < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// journalRowCount is a test/ops helper: total journaled rows.
func (s *Store) journalRowCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_journal`).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
