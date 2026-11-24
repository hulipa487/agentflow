package execguard

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	rs, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	return NewStore(rs)
}

func baseRequest() ActionRequest {
	return ActionRequest{
		CanonicalID:        "builtin:ssh.exec",
		Revision:           ActionRevision("builtin:ssh.exec", "write", "net.ssh", "true", "box:22"),
		Kind:               ActionWrite,
		Destination:        "box:22",
		Description:        "Run command over SSH",
		RequiredCapability: "net.ssh",
		NeedsConfirm:       true,
		Args: map[string]any{
			"command": "uname -a",
			"token":   "super-secret-token",
		},
		Caller: Caller{
			Agent:             "ops",
			SessionKey:        "ops|tg:1",
			WorkspaceScope:    "user:u1",
			PersonalUUID:      "u1",
			Channel:           "telegram",
			ReplyTo:           "chat:1",
			ApproverPrincipal: "user:u1",
			GrantedCaps:       map[string]bool{"net.ssh": true},
		},
	}
}

func TestEvaluatePolicyPrecedence(t *testing.T) {
	req := baseRequest()
	if d, _ := Evaluate(req); d != DecisionRequireApproval {
		t.Fatalf("got %q, want require_approval", d)
	}
	req.NeedsConfirm = false
	if d, _ := Evaluate(req); d != DecisionAllow {
		t.Fatalf("got %q, want allow", d)
	}
	req.Caller.HostedMode = true
	req.Kind = ActionUnclassified
	if d, reason := Evaluate(req); d != DecisionDeny || !strings.Contains(reason, "unclassified") {
		t.Fatalf("hosted unclassified: got %q (%s), want deny", d, reason)
	}
	req.Kind = ActionWrite
	req.Caller.GrantedCaps = map[string]bool{}
	if d, reason := Evaluate(req); d != DecisionDeny || !strings.Contains(reason, "net.ssh") {
		t.Fatalf("missing cap: got %q (%s), want deny", d, reason)
	}
	req.PolicyForbidden = true
	if d, reason := Evaluate(req); d != DecisionDeny || !strings.Contains(reason, "forbidden") {
		t.Fatalf("forbidden: got %q (%s), want deny", d, reason)
	}
}

func TestRedactArgsMasksSecretsAndTruncates(t *testing.T) {
	long := strings.Repeat("x", 300)
	red := RedactArgs(map[string]any{
		"api_key": "sk-123",
		"nested": map[string]any{
			"password": "pw",
			"safe":     long,
		},
	})
	if red["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key not redacted: %v", red["api_key"])
	}
	nested := red["nested"].(map[string]any)
	if nested["password"] != "[REDACTED]" {
		t.Fatalf("nested password not redacted: %v", nested["password"])
	}
	if !strings.HasSuffix(nested["safe"].(string), "…") || len([]rune(nested["safe"].(string))) > maxRedactRunes+1 {
		t.Fatalf("long string not bounded: len=%d", len([]rune(nested["safe"].(string))))
	}
}

func TestApprovalLifecycleSingleUseAndBindings(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	req := baseRequest()

	rec, err := st.CreateApproval(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RedactedArgs["token"] != "[REDACTED]" {
		t.Fatalf("approval preview leaked secret: %v", rec.RedactedArgs)
	}

	// Claim before approval fails.
	if _, err := st.ClaimForExecution(ctx, rec.ID, req); !errors.Is(err, ErrApprovalNotApproved) {
		t.Fatalf("claim pending: got %v, want ErrApprovalNotApproved", err)
	}

	// Wrong approver fails.
	if _, err := st.Decide(ctx, rec.ID, true, "user:attacker", "telegram"); !errors.Is(err, ErrUnauthorizedApprover) {
		t.Fatalf("unauthorized approver: got %v", err)
	}

	// Authorized approver succeeds; second decision fails.
	decided, err := st.Decide(ctx, rec.ID, true, "user:u1", "telegram")
	if err != nil || decided.Status != ApprovalApproved {
		t.Fatalf("approve: rec=%+v err=%v", decided, err)
	}
	if _, err := st.Decide(ctx, rec.ID, false, "user:u1", "telegram"); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("second decide: got %v, want ErrApprovalNotPending", err)
	}

	// Binding mismatches reject claim without consuming approval.
	badArgs := baseRequest()
	badArgs.Args["command"] = "rm -rf /"
	if _, err := st.ClaimForExecution(ctx, rec.ID, badArgs); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("args mismatch: got %v", err)
	}
	badRev := baseRequest()
	badRev.Revision = "changed"
	if _, err := st.ClaimForExecution(ctx, rec.ID, badRev); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("revision mismatch: got %v", err)
	}
	revokedCap := baseRequest()
	revokedCap.Caller.GrantedCaps = map[string]bool{}
	if _, err := st.ClaimForExecution(ctx, rec.ID, revokedCap); err == nil || !strings.Contains(err.Error(), "net.ssh") {
		t.Fatalf("revoked cap: got %v", err)
	}

	// Valid claim succeeds exactly once; replay is rejected.
	claimed, err := st.ClaimForExecution(ctx, rec.ID, req)
	if err != nil || claimed.Status != ApprovalClaimed {
		t.Fatalf("valid claim: rec=%+v err=%v", claimed, err)
	}
	if _, err := st.ClaimForExecution(ctx, rec.ID, req); !errors.Is(err, ErrApprovalAlreadyUsed) {
		t.Fatalf("replay claim: got %v, want ErrApprovalAlreadyUsed", err)
	}
}

func TestApprovalExpiryAndStudioDecide(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Expired record cannot be decided or claimed.
	st.ApprovalTTL = -time.Second
	expRec, err := st.CreateApproval(ctx, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decide(ctx, expRec.ID, true, "studio:operator", "studio"); !errors.Is(err, ErrApprovalExpired) && !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("expired decide: got %v", err)
	}

	// Valid Studio operator decision wakes WaitDecision.
	st.ApprovalTTL = time.Minute
	rec, err := st.CreateApproval(ctx, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.ListPending(ctx, rec.SessionKey)
	if err != nil || len(pending) != 1 || pending[0].ID != rec.ID {
		t.Fatalf("ListPending = %+v, err=%v", pending, err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = st.Decide(context.Background(), rec.ID, true, "studio:operator", "studio")
	}()
	waited, ok, err := st.WaitDecision(ctx, rec.ID, 2*time.Second)
	if err != nil || !ok || waited.Status != ApprovalApproved || waited.DecidedBy != "studio:operator" {
		t.Fatalf("WaitDecision = ok:%v rec:%+v err:%v", ok, waited, err)
	}
}

func TestExecutionIntentIdempotencyAndFailClosed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	_, hash := CanonicalJSON(map[string]any{"x": 1})

	// First begin succeeds as fresh; concurrent retry while in-flight fails closed.
	if _, replayed, err := st.BeginExecution(ctx, "user:u1", "k1", "api:pay.charge", hash, "api.stripe.com"); err != nil || replayed {
		t.Fatalf("begin 1: replayed=%v err=%v", replayed, err)
	}
	if _, replayed, err := st.BeginExecution(ctx, "user:u1", "k1", "api:pay.charge", hash, "api.stripe.com"); !errors.Is(err, ErrUncertainOutcome) || replayed {
		t.Fatalf("in-flight retry must fail closed: replayed=%v err=%v", replayed, err)
	}

	// Marking uncertain stays fail-closed on subsequent retry.
	if err := st.CompleteExecution(ctx, "user:u1", "k1", "", context.DeadlineExceeded, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.BeginExecution(ctx, "user:u1", "k1", "api:pay.charge", hash, "api.stripe.com"); !errors.Is(err, ErrUncertainOutcome) {
		t.Fatalf("uncertain retry must fail closed: %v", err)
	}

	// Succeeding replays cached JSON and rejects reusing key for different args.
	if _, replayed, err := st.BeginExecution(ctx, "user:u1", "k2", "api:pay.charge", hash, "api.stripe.com"); err != nil || replayed {
		t.Fatalf("begin k2: replayed=%v err=%v", replayed, err)
	}
	if err := st.CompleteExecution(ctx, "user:u1", "k2", `{"ok":true,"charge_id":"ch_1"}`, nil, false, ""); err != nil {
		t.Fatal(err)
	}
	rec, replayed, err := st.BeginExecution(ctx, "user:u1", "k2", "api:pay.charge", hash, "api.stripe.com")
	if err != nil || !replayed || rec.ResultJSON != `{"ok":true,"charge_id":"ch_1"}` {
		t.Fatalf("replay k2: replayed=%v rec=%+v err=%v", replayed, rec, err)
	}
	if _, _, err := st.BeginExecution(ctx, "user:u1", "k2", "api:pay.charge", "different-hash", "api.stripe.com"); err == nil {
		t.Fatal("reusing idempotency key with different args must fail")
	}
}
