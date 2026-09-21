// Package inbox is the shared queue of messages waiting for a session.
//
// A session is a conversation: (agent, key). In a single-instance deployment a
// message reaches it by being handed to the actor's mailbox. In a fleet the
// message can arrive at any instance while the session runs on exactly one, and
// this is the piece in between: every message for a session is written here
// first, and the instance that holds the session's lease claims it and delivers
// it. The writer does not need to know where the session is, or whether it
// exists yet.
//
// Delivery is at-least-once. A claim that is not acked within the visibility
// window becomes claimable again — which is how a holder that dies mid-delivery
// is recovered — so a message can be delivered twice in that one case. The
// claim-then-ack window is a single in-process call wide, and an acked row is
// never claimable again.
package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"agentflow/internal/core/session"
	"agentflow/internal/core/storedb"
)

// schema is the inbox table, written once for both backends. Timestamps are
// unix nanoseconds, the convention the store's other deadlines use.
//
// The primary key is (session_key, msg_id), which makes a retried write of the
// same message a no-op rather than a second copy. msg_id is the message's own
// id qualified by the instance that accepted it — see Queue.Post: message ids
// are minted per instance (a channel driver's counter, for instance), so the id
// alone is not unique across a fleet, and two instances' first message of the
// day must not be mistaken for one another.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS session_inbox (
		session_key TEXT NOT NULL,
		msg_id      TEXT NOT NULL,
		ts          BIGINT NOT NULL,
		body        TEXT NOT NULL,
		claimed_by  TEXT NOT NULL DEFAULT '',
		claimed_at  BIGINT NOT NULL DEFAULT 0,
		done        INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (session_key, msg_id)
	)`,
	`CREATE INDEX IF NOT EXISTS session_inbox_ready ON session_inbox (session_key, done, ts)`,
}

// Defaults for the two windows. Visibility is how long a claim is respected
// before another instance may take the message over: long enough that a slow
// delivery is not stolen, short enough that a crash is not a stall. Retention
// is how long an acked row is kept — it exists only so that a duplicate write
// of an already-delivered message is still recognized as one.
const (
	DefaultVisibility = 60 * time.Second
	DefaultRetention  = time.Hour
)

// Item is one claimed message.
type Item struct {
	SessionKey string
	MsgID      string
	Message    session.Message
	Ts         int64
}

// Queue is the shared inbox on one store. Its origin identifies the instance
// writing to it, and qualifies every message id it stores.
type Queue struct {
	st         *storedb.DB
	origin     string
	log        *slog.Logger
	visibility atomic.Int64 // nanoseconds
	retention  atomic.Int64
}

// Open creates the queue over the store at target and creates its schema.
// origin identifies this instance (lease.OwnerID builds one); it must be
// non-empty in a fleet, because it is what keeps two instances' identically
// numbered messages apart.
func Open(target, origin string, log *slog.Logger) (*Queue, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st, err := storedb.Open(target)
	if err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.ExecDDL(ctx, schema...); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("inbox: %w", err)
	}
	q := &Queue{st: st, origin: origin, log: log.With("module", "inbox")}
	q.SetVisibility(DefaultVisibility)
	q.SetRetention(DefaultRetention)
	return q, nil
}

// rowID is the stored key for a message: this instance's origin qualified by
// the message's own id. The separator is "|", which appears in neither an owner
// id (host, pid, hex) nor a driver-minted message id, so two different pairs
// cannot collide on one key.
func (q *Queue) rowID(msgID string) string {
	if q.origin == "" {
		return msgID
	}
	return q.origin + "|" + msgID
}

// SetVisibility sets how long a claim is respected before the message may be
// taken over. A deployment tunes it to its slowest delivery.
func (q *Queue) SetVisibility(d time.Duration) { q.visibility.Store(int64(d)) }

// SetRetention sets how long acked rows are kept.
func (q *Queue) SetRetention(d time.Duration) { q.retention.Store(int64(d)) }

// Close releases this queue's hold on the connection pool.
func (q *Queue) Close() error { return q.st.Close() }

// Post enqueues a message for a session. Posting the same (session, id) twice
// from one instance is one row, not two: a retried inbound cannot become a
// duplicate reply. Two *instances* posting the same id, though, are two
// messages — their ids are minted independently — and both are kept.
func (q *Queue) Post(ctx context.Context, sessionKey string, msg session.Message) error {
	if sessionKey == "" {
		return fmt.Errorf("inbox: empty session key")
	}
	if msg.ID == "" {
		return fmt.Errorf("inbox: message for %q has no id; a message without one cannot be deduplicated", sessionKey)
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("inbox: encode message %s: %w", msg.ID, err)
	}
	// ts is the enqueue time, which is what orders a session's backlog. The
	// message carries its own timestamp in the body.
	if _, err := q.st.Exec(ctx, `
		INSERT INTO session_inbox (session_key, msg_id, ts, body)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (session_key, msg_id) DO NOTHING`,
		sessionKey, q.rowID(msg.ID), time.Now().UnixNano(), string(body)); err != nil {
		return fmt.Errorf("inbox: post %s: %w", msg.ID, err)
	}
	return nil
}

// Claim takes up to limit messages for one session and returns everything this
// owner now holds for it — including anything an earlier round delivered
// without acking, which is what makes a crashed delivery recoverable rather
// than lost.
//
// Whether a row is available is decided by the update's own WHERE clause, so
// two instances claiming at the same instant cannot both be handed the same
// message.
func (q *Queue) Claim(ctx context.Context, owner, sessionKey string, limit int) ([]Item, error) {
	if owner == "" {
		return nil, fmt.Errorf("inbox: empty owner")
	}
	if limit <= 0 {
		limit = 100
	}
	now := time.Now().UnixNano()
	stale := now - q.visibility.Load()
	if _, err := q.st.Exec(ctx, `
		UPDATE session_inbox SET claimed_by = ?, claimed_at = ?
		WHERE session_key = ? AND done = 0
		  AND (claimed_by = '' OR claimed_at < ?)
		  AND msg_id IN (
			SELECT msg_id FROM session_inbox
			WHERE session_key = ? AND done = 0 AND (claimed_by = '' OR claimed_at < ?)
			ORDER BY ts LIMIT ?
		  )`,
		owner, now, sessionKey, stale, sessionKey, stale, limit); err != nil {
		return nil, fmt.Errorf("inbox: claim %s: %w", sessionKey, err)
	}

	rows, err := q.st.Query(ctx, `
		SELECT msg_id, ts, body FROM session_inbox
		WHERE session_key = ? AND claimed_by = ? AND done = 0
		ORDER BY ts`, sessionKey, owner)
	if err != nil {
		return nil, fmt.Errorf("inbox: read claims for %s: %w", sessionKey, err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var body string
		if err := rows.Scan(&it.MsgID, &it.Ts, &body); err != nil {
			return nil, fmt.Errorf("inbox: scan claim: %w", err)
		}
		if err := json.Unmarshal([]byte(body), &it.Message); err != nil {
			// A row this engine cannot read is not deliverable; drop it rather
			// than blocking the session behind it forever.
			q.log.Error("inbox: dropping an unreadable message", "session", sessionKey, "id", it.MsgID, "err", err)
			if _, err := q.st.Exec(ctx, `DELETE FROM session_inbox WHERE session_key = ? AND msg_id = ?`,
				sessionKey, it.MsgID); err != nil {
				q.log.Warn("inbox: drop failed", "id", it.MsgID, "err", err)
			}
			continue
		}
		it.SessionKey = sessionKey
		out = append(out, it)
	}
	return out, rows.Err()
}

// Ack marks messages delivered. Only the owner that claimed them can ack them,
// so a late ack cannot close a message another instance is now responsible for.
func (q *Queue) Ack(ctx context.Context, owner string, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	for _, it := range items {
		if _, err := q.st.Exec(ctx,
			`UPDATE session_inbox SET done = 1 WHERE session_key = ? AND msg_id = ? AND claimed_by = ?`,
			it.SessionKey, it.MsgID, owner); err != nil {
			return fmt.Errorf("inbox: ack %s: %w", it.MsgID, err)
		}
	}
	return nil
}

// Depth reports how many messages are waiting for a session. It is the
// operator's view of a backlog, and how a test sees that a message has landed.
func (q *Queue) Depth(ctx context.Context, sessionKey string) (int, error) {
	var n int
	err := q.st.QueryRow(ctx,
		`SELECT COUNT(*) FROM session_inbox WHERE session_key = ? AND done = 0`, sessionKey).Scan(&n)
	return n, err
}

// Len reports how many messages are waiting across the sessions named. A fleet
// uses it to see whether the inbox is draining.
func (q *Queue) Len(ctx context.Context, sessionKeys []string) (int, error) {
	if len(sessionKeys) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(sessionKeys))
	for _, k := range sessionKeys {
		args = append(args, k)
	}
	var n int
	err := q.st.QueryRow(ctx,
		`SELECT COUNT(*) FROM session_inbox WHERE done = 0 AND session_key IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(sessionKeys)), ",")+`)`, args...).Scan(&n)
	return n, err
}

// Sweep deletes acked rows past retention. It is safe to run on any instance:
// the rows it removes are already delivered.
func (q *Queue) Sweep(ctx context.Context) (int, error) {
	cutoff := time.Now().UnixNano() - q.retention.Load()
	res, err := q.st.Exec(ctx,
		`DELETE FROM session_inbox WHERE done = 1 AND claimed_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("inbox: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
