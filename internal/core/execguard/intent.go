package execguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const intentRowPrefix = "exec|"

// ErrUncertainOutcome indicates a prior write attempt was recorded in-flight or
// left interrupted/uncertain across a crash or timeout. Execution fails closed.
var ErrUncertainOutcome = errors.New("previous write attempt has an uncertain outcome; reconcile before retrying")

// ExecutionStatus records durable intent state around side-effecting calls.
type ExecutionStatus string

const (
	ExecExecuting ExecutionStatus = "executing"
	ExecSucceeded ExecutionStatus = "succeeded"
	ExecFailed    ExecutionStatus = "failed"
	ExecUncertain ExecutionStatus = "uncertain"
)

// ExecutionRecord tracks one idempotent write attempt.
type ExecutionRecord struct {
	Key             string          `json:"key"`
	CanonicalAction string          `json:"action"`
	ArgsHash        string          `json:"args_hash"`
	Destination     string          `json:"destination,omitempty"`
	Status          ExecutionStatus `json:"status"`
	ResultJSON      string          `json:"result_json,omitempty"`
	Error           string          `json:"error,omitempty"`
	StartedAt       int64           `json:"started_at"`
	UpdatedAt       int64           `json:"updated_at"`
}

func intentRowKey(scope, idemKey string) string {
	if strings.TrimSpace(scope) == "" {
		scope = "global"
	}
	return intentRowPrefix + scope + "|" + idemKey
}

// BeginExecution records durable write intent before calling an external
// driver. replayed is true when a previous identical invocation already
// succeeded (and rec.ResultJSON carries the cached JSON result). In-flight or
// uncertain prior attempts fail closed with ErrUncertainOutcome.
func (s *Store) BeginExecution(ctx context.Context, scope, idemKey, action, argsHash, dest string) (ExecutionRecord, bool, error) {
	if s == nil || strings.TrimSpace(idemKey) == "" {
		return ExecutionRecord{}, false, nil
	}
	key := intentRowKey(scope, idemKey)
	now := time.Now()
	exp := now.Add(s.intentTTL())
	rec := ExecutionRecord{
		Key:             idemKey,
		CanonicalAction: action,
		ArgsHash:        argsHash,
		Destination:     dest,
		Status:          ExecExecuting,
		StartedAt:       now.Unix(),
		UpdatedAt:       now.Unix(),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return ExecutionRecord{}, false, err
	}
	inserted, err := s.putIfAbsent(ctx, key, string(raw), exp)
	if err != nil {
		return ExecutionRecord{}, false, err
	}
	if inserted {
		return rec, false, nil
	}

	existingRaw, ok, err := s.getRow(ctx, key)
	if err != nil {
		return ExecutionRecord{}, false, err
	}
	if !ok {
		return ExecutionRecord{}, false, ErrUncertainOutcome
	}
	var existing ExecutionRecord
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return ExecutionRecord{}, false, err
	}
	if existing.CanonicalAction != action || existing.ArgsHash != argsHash {
		return existing, false, fmt.Errorf("idempotency key %q reused for different action or arguments", idemKey)
	}
	switch existing.Status {
	case ExecSucceeded:
		return existing, true, nil
	case ExecFailed:
		if existing.Error == "" {
			return existing, false, errors.New("previous execution failed")
		}
		return existing, false, errors.New(existing.Error)
	default:
		return existing, false, ErrUncertainOutcome
	}
}

// CompleteExecution transitions an in-flight execution intent to a terminal
// status (succeeded, failed, or uncertain) and mirrors terminal status onto
// the originating approval record when approvalID is provided.
func (s *Store) CompleteExecution(ctx context.Context, scope, idemKey, resultJSON string, execErr error, uncertain bool, approvalID string) error {
	if s == nil || strings.TrimSpace(idemKey) == "" {
		return nil
	}
	key := intentRowKey(scope, idemKey)
	existingRaw, ok, err := s.getRow(ctx, key)
	if err != nil {
		return err
	}
	var rec ExecutionRecord
	if ok {
		_ = json.Unmarshal([]byte(existingRaw), &rec)
	} else {
		rec = ExecutionRecord{Key: idemKey, StartedAt: time.Now().Unix()}
	}

	var approvalStatus ApprovalStatus
	switch {
	case uncertain:
		rec.Status = ExecUncertain
		approvalStatus = ApprovalUncertain
		if execErr != nil {
			rec.Error = execErr.Error()
		} else {
			rec.Error = ErrUncertainOutcome.Error()
		}
	case execErr != nil:
		rec.Status = ExecFailed
		approvalStatus = ApprovalFailed
		rec.Error = execErr.Error()
	default:
		rec.Status = ExecSucceeded
		approvalStatus = ApprovalSucceeded
		rec.ResultJSON = resultJSON
		rec.Error = ""
	}
	rec.UpdatedAt = time.Now().Unix()

	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := s.putRow(ctx, key, string(raw), time.Now().Add(s.intentTTL())); err != nil {
		return err
	}
	if approvalID != "" {
		_ = s.markApprovalOutcome(ctx, approvalID, approvalStatus)
	}
	return nil
}
