package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
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
	UserUUID    string         // profile behind the message ("" when the sender is unregistered)
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
			(id, ts, direction, status, channel, chat, sender, user_uuid, agent, session_id,
			 type, text, attachments_json, provenance_json, err)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, ts, e.Direction, e.Status, e.Channel, e.Chat, e.Sender, e.UserUUID, e.Agent,
		e.SessionID, e.Type, e.Text, string(atts), string(prov), e.Err)
	return err
}

// JournalFilter selects journal rows for an operator view. Zero fields are
// unset, so an empty filter means "the newest rows".
type JournalFilter struct {
	UserUUID  string
	SessionID string
	Direction string // "in" | "out" | ""
	Since     int64  // unix seconds, inclusive
	Until     int64  // unix seconds, exclusive (0 = now)
	Limit     int    // default 100, capped at 1000
}

// ListMessages returns journal rows matching the filter, newest first. It is
// the read side of the audit trail: the per-user view the console and an
// operator query build on.
func (s *Store) ListMessages(ctx context.Context, f JournalFilter) ([]JournalEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	where := []string{"1 = 1"}
	args := []any{}
	if f.UserUUID != "" {
		where = append(where, "user_uuid = ?")
		args = append(args, f.UserUUID)
	}
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.Direction != "" {
		where = append(where, "direction = ?")
		args = append(args, f.Direction)
	}
	if f.Since > 0 {
		where = append(where, "ts >= ?")
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, "ts < ?")
		args = append(args, f.Until)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, direction, status, channel, chat, sender, user_uuid, agent,
		       session_id, type, text, attachments_json, provenance_json, err
		FROM message_journal
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY ts DESC, id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		// Every column but the first four is nullable (rows predating a field,
		// or written by hand), so they scan through NullString rather than
		// failing the whole query on one NULL.
		var channel, chat, sender, userUUID, agent, sessionID, typ, text, atts, prov, errStr sql.NullString
		if err := rows.Scan(&e.ID, &e.Ts, &e.Direction, &e.Status, &channel, &chat, &sender,
			&userUUID, &agent, &sessionID, &typ, &text, &atts, &prov, &errStr); err != nil {
			return nil, err
		}
		e.Channel, e.Chat, e.Sender, e.UserUUID = channel.String, chat.String, sender.String, userUUID.String
		e.Agent, e.SessionID, e.Type, e.Text, e.Err = agent.String, sessionID.String, typ.String, text.String, errStr.String
		if atts.String != "" {
			_ = json.Unmarshal([]byte(atts.String), &e.Attachments)
		}
		if prov.String != "" {
			_ = json.Unmarshal([]byte(prov.String), &e.Provenance)
		}
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
