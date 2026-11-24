package execguard

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/runtime"
)

const (
	approvalRowPrefix = "approval|"
	defaultApproveTTL = 10 * time.Minute
	defaultIntentTTL  = 24 * time.Hour
)

var (
	ErrApprovalNotFound     = errors.New("approval record not found")
	ErrApprovalExpired      = errors.New("approval has expired")
	ErrApprovalNotPending   = errors.New("approval has already been decided")
	ErrApprovalNotApproved  = errors.New("approval is not in approved state")
	ErrApprovalAlreadyUsed  = errors.New("approval has already been claimed")
	ErrApprovalMismatch     = errors.New("approval does not match action, arguments, destination, workspace, or session")
	ErrUnauthorizedApprover = errors.New("sender is not authorized to decide this approval")
)

// CASRows extends runtime.Rows with atomic insert-if-absent and
// compare-and-swap primitives backed by the runtime database.
type CASRows interface {
	runtime.Rows
	PutRowIfAbsent(ctx context.Context, key, value string, expiresAt time.Time) (bool, error)
	CompareAndSwapRow(ctx context.Context, key, oldValue, newValue string, expiresAt time.Time) (bool, error)
}

// ApprovalStatus tracks the lifecycle of one runtime-owned approval request.
type ApprovalStatus string

const (
	ApprovalPending   ApprovalStatus = "pending"
	ApprovalApproved  ApprovalStatus = "approved"
	ApprovalDenied    ApprovalStatus = "denied"
	ApprovalClaimed   ApprovalStatus = "claimed"
	ApprovalExpired   ApprovalStatus = "expired"
	ApprovalSucceeded ApprovalStatus = "succeeded"
	ApprovalFailed    ApprovalStatus = "failed"
	ApprovalUncertain ApprovalStatus = "uncertain"
)

// ApprovalRecord is the durable, immutable authorization binding created when a
// high-impact action requires explicit user/operator confirmation.
type ApprovalRecord struct {
	ID               string         `json:"id"`
	Status           ApprovalStatus `json:"status"`
	SessionKey       string         `json:"session_key"`
	Agent            string         `json:"agent"`
	WorkspaceScope   string         `json:"workspace_scope,omitempty"`
	EligibleApprover string         `json:"eligible_approver,omitempty"`
	Channel          string         `json:"channel,omitempty"`
	ReplyTo          string         `json:"reply_to,omitempty"`
	CanonicalAction  string         `json:"action"`
	ActionRevision   string         `json:"action_revision"`
	ArgsHash         string         `json:"args_hash"`
	RedactedArgs     map[string]any `json:"args_preview,omitempty"`
	Destination      string         `json:"destination,omitempty"`
	Description      string         `json:"description,omitempty"`
	CreatedAt        int64          `json:"created_at"`
	ExpiresAt        int64          `json:"expires_at"`
	DecidedAt        int64          `json:"decided_at,omitempty"`
	DecidedBy        string         `json:"decided_by,omitempty"`
}

// Store persists runtime-owned approvals and execution idempotency records.
type Store struct {
	rows        runtime.Rows
	cas         CASRows
	ApprovalTTL time.Duration
	IntentTTL   time.Duration

	mu      sync.Mutex
	mem     map[string]memRow
	waiters map[string][]chan ApprovalRecord
}

type memRow struct {
	value     string
	expiresAt time.Time
}

// NewStore builds an execution-guard Store backed by rows. When rows is nil
// (or does not implement CASRows), thread-safe CAS semantics still work in
// process via an internal mutex or in-memory table.
func NewStore(rows runtime.Rows) *Store {
	s := &Store{
		rows:        rows,
		ApprovalTTL: defaultApproveTTL,
		IntentTTL:   defaultIntentTTL,
		mem:         map[string]memRow{},
		waiters:     map[string][]chan ApprovalRecord{},
	}
	if cas, ok := rows.(CASRows); ok {
		s.cas = cas
	}
	return s
}

func (s *Store) approvalTTL() time.Duration {
	if s == nil || s.ApprovalTTL == 0 {
		return defaultApproveTTL
	}
	return s.ApprovalTTL
}

func (s *Store) intentTTL() time.Duration {
	if s == nil || s.IntentTTL <= 0 {
		return defaultIntentTTL
	}
	return s.IntentTTL
}

// CreateApproval mints and persists a new pending approval record bound to req.
func (s *Store) CreateApproval(ctx context.Context, req ActionRequest) (ApprovalRecord, error) {
	if s == nil {
		return ApprovalRecord{}, errors.New("execguard store is nil")
	}
	argsHash := req.ArgsHash
	if argsHash == "" {
		_, argsHash = CanonicalJSON(req.Args)
	}
	now := time.Now()
	ttl := s.approvalTTL()
	exp := now.Add(ttl)
	rec := ApprovalRecord{
		ID:               uuid.New().String(),
		Status:           ApprovalPending,
		SessionKey:       req.Caller.SessionKey,
		Agent:            req.Caller.Agent,
		WorkspaceScope:   req.Caller.WorkspaceScope,
		EligibleApprover: req.Caller.ApproverPrincipal,
		Channel:          req.Caller.Channel,
		ReplyTo:          req.Caller.ReplyTo,
		CanonicalAction:  req.CanonicalID,
		ActionRevision:   req.Revision,
		ArgsHash:         argsHash,
		RedactedArgs:     RedactArgs(req.Args),
		Destination:      req.Destination,
		Description:      req.Description,
		CreatedAt:        now.Unix(),
		ExpiresAt:        exp.Unix(),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return ApprovalRecord{}, err
	}
	// PutRow expects zero time for never-expiring; positive ttl ensures cleanup.
	inserted, err := s.putIfAbsent(ctx, approvalRowPrefix+rec.ID, string(raw), exp)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !inserted {
		return ApprovalRecord{}, errors.New("approval id collision")
	}
	return rec, nil
}

// GetApproval returns the current approval record and false if missing or expired.
func (s *Store) GetApproval(ctx context.Context, id string) (ApprovalRecord, bool, error) {
	if s == nil || strings.TrimSpace(id) == "" {
		return ApprovalRecord{}, false, nil
	}
	raw, ok, err := s.getRow(ctx, approvalRowPrefix+id)
	if err != nil || !ok {
		return ApprovalRecord{}, false, err
	}
	var rec ApprovalRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return ApprovalRecord{}, false, err
	}
	return rec, true, nil
}

// ListPending returns non-expired pending approvals, optionally filtered by sessionKey.
func (s *Store) ListPending(ctx context.Context, sessionKey string) ([]ApprovalRecord, error) {
	if s == nil {
		return nil, nil
	}
	raws, err := s.listRows(ctx, approvalRowPrefix)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	out := make([]ApprovalRecord, 0, len(raws))
	for _, raw := range raws {
		var rec ApprovalRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		if rec.Status != ApprovalPending || now > rec.ExpiresAt {
			continue
		}
		if sessionKey != "" && rec.SessionKey != sessionKey {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// Decide atomically transitions a pending approval to approved or denied after
// verifying expiry and approver authorization.
func (s *Store) Decide(ctx context.Context, id string, approve bool, actorPrincipal, channel string) (ApprovalRecord, error) {
	if s == nil {
		return ApprovalRecord{}, errors.New("execguard store is nil")
	}
	key := approvalRowPrefix + strings.TrimSpace(id)
	raw, ok, err := s.getRow(ctx, key)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !ok {
		return ApprovalRecord{}, ErrApprovalNotFound
	}
	var rec ApprovalRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return ApprovalRecord{}, err
	}
	if time.Now().Unix() > rec.ExpiresAt {
		return rec, ErrApprovalExpired
	}
	if rec.Status != ApprovalPending {
		return rec, ErrApprovalNotPending
	}
	if !authorizedApprover(rec, actorPrincipal, channel) {
		return ApprovalRecord{}, ErrUnauthorizedApprover
	}

	next := rec
	if approve {
		next.Status = ApprovalApproved
	} else {
		next.Status = ApprovalDenied
	}
	next.DecidedAt = time.Now().Unix()
	next.DecidedBy = actorPrincipal

	nextRaw, err := json.Marshal(next)
	if err != nil {
		return ApprovalRecord{}, err
	}
	swapped, err := s.compareAndSwap(ctx, key, raw, string(nextRaw), time.Unix(rec.ExpiresAt, 0))
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !swapped {
		return ApprovalRecord{}, ErrApprovalNotPending
	}
	s.notifyWaiters(next)
	return next, nil
}

func authorizedApprover(rec ApprovalRecord, actorPrincipal, channel string) bool {
	actorPrincipal = strings.TrimSpace(actorPrincipal)
	channel = strings.TrimSpace(channel)
	if actorPrincipal == "" {
		return false
	}
	if channel == "studio" && actorPrincipal == "studio:operator" {
		return true
	}
	if rec.EligibleApprover == "" {
		return false
	}
	if actorPrincipal != rec.EligibleApprover {
		return false
	}
	if rec.Channel != "" && channel != "" && channel != rec.Channel {
		return false
	}
	return true
}

// ClaimForExecution verifies that id is approved, unexpired, matches req's
// binding (session, workspace, action, revision, args, destination), and still
// passes policy/capability evaluation, then atomically claims it for one run.
func (s *Store) ClaimForExecution(ctx context.Context, id string, req ActionRequest) (ApprovalRecord, error) {
	if s == nil {
		return ApprovalRecord{}, errors.New("execguard store is nil")
	}
	argsHash := req.ArgsHash
	if argsHash == "" {
		_, argsHash = CanonicalJSON(req.Args)
	}
	key := approvalRowPrefix + strings.TrimSpace(id)
	raw, ok, err := s.getRow(ctx, key)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !ok {
		return ApprovalRecord{}, ErrApprovalNotFound
	}
	var rec ApprovalRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return ApprovalRecord{}, err
	}
	if time.Now().Unix() > rec.ExpiresAt {
		return rec, ErrApprovalExpired
	}
	switch rec.Status {
	case ApprovalApproved:
	case ApprovalClaimed, ApprovalSucceeded, ApprovalFailed, ApprovalUncertain:
		return rec, ErrApprovalAlreadyUsed
	default:
		return rec, ErrApprovalNotApproved
	}
	if rec.SessionKey != req.Caller.SessionKey ||
		rec.WorkspaceScope != req.Caller.WorkspaceScope ||
		rec.CanonicalAction != req.CanonicalID ||
		rec.ActionRevision != req.Revision ||
		rec.ArgsHash != argsHash ||
		rec.Destination != req.Destination {
		return rec, ErrApprovalMismatch
	}
	if decision, reason := Evaluate(req); decision == DecisionDeny {
		return rec, errors.New(reason)
	}

	next := rec
	next.Status = ApprovalClaimed
	nextRaw, err := json.Marshal(next)
	if err != nil {
		return ApprovalRecord{}, err
	}
	exp := time.Now().Add(s.intentTTL())
	if curExp := time.Unix(rec.ExpiresAt, 0); curExp.After(exp) {
		exp = curExp
	}
	swapped, err := s.compareAndSwap(ctx, key, raw, string(nextRaw), exp)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !swapped {
		return ApprovalRecord{}, ErrApprovalAlreadyUsed
	}
	return next, nil
}

// WaitDecision blocks until approval id leaves the pending state, timeout
// elapses, or ctx is cancelled.
func (s *Store) WaitDecision(ctx context.Context, id string, timeout time.Duration) (ApprovalRecord, bool, error) {
	if s == nil {
		return ApprovalRecord{}, false, errors.New("execguard store is nil")
	}
	rec, ok, err := s.GetApproval(ctx, id)
	if err != nil {
		return ApprovalRecord{}, false, err
	}
	if !ok {
		return ApprovalRecord{}, false, ErrApprovalNotFound
	}
	if rec.Status != ApprovalPending {
		return rec, true, nil
	}

	ch := s.addWaiter(id)
	defer s.removeWaiter(id, ch)

	// Re-check immediately after registering to avoid missing a concurrent Decide.
	rec, ok, err = s.GetApproval(ctx, id)
	if err != nil {
		return ApprovalRecord{}, false, err
	}
	if !ok {
		return ApprovalRecord{}, false, ErrApprovalNotFound
	}
	if rec.Status != ApprovalPending {
		return rec, true, nil
	}

	if timeout <= 0 {
		timeout = time.Until(time.Unix(rec.ExpiresAt, 0))
		if timeout <= 0 {
			return rec, false, nil
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return ApprovalRecord{}, false, ctx.Err()
		case <-timer.C:
			return rec, false, nil
		case updated := <-ch:
			if updated.Status != ApprovalPending {
				return updated, true, nil
			}
		case <-poll.C:
			cur, ok, err := s.GetApproval(ctx, id)
			if err != nil {
				return ApprovalRecord{}, false, err
			}
			if !ok || time.Now().Unix() > cur.ExpiresAt {
				return cur, false, nil
			}
			if cur.Status != ApprovalPending {
				return cur, true, nil
			}
		}
	}
}

func (s *Store) markApprovalOutcome(ctx context.Context, id string, status ApprovalStatus) error {
	if s == nil || id == "" {
		return nil
	}
	key := approvalRowPrefix + id
	raw, ok, err := s.getRow(ctx, key)
	if err != nil || !ok {
		return err
	}
	var rec ApprovalRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return err
	}
	if rec.Status != ApprovalClaimed {
		return nil
	}
	rec.Status = status
	nextRaw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.compareAndSwap(ctx, key, raw, string(nextRaw), time.Now().Add(s.intentTTL()))
	return err
}

func (s *Store) addWaiter(id string) chan ApprovalRecord {
	ch := make(chan ApprovalRecord, 1)
	s.mu.Lock()
	s.waiters[id] = append(s.waiters[id], ch)
	s.mu.Unlock()
	return ch
}

func (s *Store) removeWaiter(id string, target chan ApprovalRecord) {
	s.mu.Lock()
	list := s.waiters[id]
	kept := list[:0]
	for _, ch := range list {
		if ch != target {
			kept = append(kept, ch)
		}
	}
	if len(kept) == 0 {
		delete(s.waiters, id)
	} else {
		s.waiters[id] = kept
	}
	s.mu.Unlock()
}

func (s *Store) notifyWaiters(rec ApprovalRecord) {
	s.mu.Lock()
	list := append([]chan ApprovalRecord(nil), s.waiters[rec.ID]...)
	s.mu.Unlock()
	for _, ch := range list {
		select {
		case ch <- rec:
		default:
		}
	}
}

func (s *Store) putIfAbsent(ctx context.Context, key, value string, expiresAt time.Time) (bool, error) {
	if s.cas != nil {
		return s.cas.PutRowIfAbsent(ctx, key, value, expiresAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows != nil {
		_, ok, err := s.rows.GetRow(ctx, key)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
		if err := s.rows.PutRow(ctx, key, value, expiresAt); err != nil {
			return false, err
		}
		return true, nil
	}
	if cur, ok := s.mem[key]; ok && (cur.expiresAt.IsZero() || cur.expiresAt.After(time.Now())) {
		return false, nil
	}
	s.mem[key] = memRow{value: value, expiresAt: expiresAt}
	return true, nil
}

func (s *Store) compareAndSwap(ctx context.Context, key, oldValue, newValue string, expiresAt time.Time) (bool, error) {
	if s.cas != nil {
		return s.cas.CompareAndSwapRow(ctx, key, oldValue, newValue, expiresAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows != nil {
		row, ok, err := s.rows.GetRow(ctx, key)
		if err != nil || !ok || row.Value != oldValue {
			return false, err
		}
		if err := s.rows.PutRow(ctx, key, newValue, expiresAt); err != nil {
			return false, err
		}
		return true, nil
	}
	cur, ok := s.mem[key]
	if !ok || (!cur.expiresAt.IsZero() && !cur.expiresAt.After(time.Now())) || cur.value != oldValue {
		return false, nil
	}
	s.mem[key] = memRow{value: newValue, expiresAt: expiresAt}
	return true, nil
}

func (s *Store) putRow(ctx context.Context, key, value string, expiresAt time.Time) error {
	if s.rows != nil {
		return s.rows.PutRow(ctx, key, value, expiresAt)
	}
	s.mu.Lock()
	s.mem[key] = memRow{value: value, expiresAt: expiresAt}
	s.mu.Unlock()
	return nil
}

func (s *Store) getRow(ctx context.Context, key string) (string, bool, error) {
	if s.rows != nil {
		row, ok, err := s.rows.GetRow(ctx, key)
		return row.Value, ok, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.mem[key]
	if !ok {
		return "", false, nil
	}
	if !cur.expiresAt.IsZero() && !cur.expiresAt.After(time.Now()) {
		delete(s.mem, key)
		return "", false, nil
	}
	return cur.value, true, nil
}

func (s *Store) listRows(ctx context.Context, prefix string) ([]string, error) {
	if s.rows != nil {
		rows, err := s.rows.ListRows(ctx, prefix)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Value)
		}
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := []string{}
	for k, cur := range s.mem {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if !cur.expiresAt.IsZero() && !cur.expiresAt.After(now) {
			delete(s.mem, k)
			continue
		}
		out = append(out, cur.value)
	}
	return out, nil
}

type ctxKey int

const (
	approvedIDKey ctxKey = iota + 1
	idempotencyKeyCtxKey
	preAuthorizedKey
)

// WithApprovedID carries an actor-verified runtime approval ID in Go context
// only (never serialized into VM-reachable Op payloads).
func WithApprovedID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, approvedIDKey, id)
}

// ApprovedIDFromCtx extracts the actor-verified approval ID from ctx.
func ApprovedIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(approvedIDKey).(string)
	return v
}

// WithIdempotencyKey attaches an execution idempotency key in Go context.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyCtxKey, key)
}

// IdempotencyKeyFromCtx extracts the execution idempotency key from ctx.
func IdempotencyKeyFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(idempotencyKeyCtxKey).(string)
	return v
}

// WithPreAuthorized marks a canonical action as already authorized for this Go
// call chain (so tools.run -> ext.Registry wrapper does not double-claim).
func WithPreAuthorized(ctx context.Context, canonicalID string) context.Context {
	if canonicalID == "" {
		return ctx
	}
	return context.WithValue(ctx, preAuthorizedKey, canonicalID)
}

// IsPreAuthorized reports whether canonicalID was already authorized upstream
// in ctx.
func IsPreAuthorized(ctx context.Context, canonicalID string) bool {
	v, _ := ctx.Value(preAuthorizedKey).(string)
	return v != "" && v == canonicalID
}
