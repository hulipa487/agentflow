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
