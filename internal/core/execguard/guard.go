// Package execguard implements the unified execution-boundary authorization
// policy, runtime-owned single-use approvals, argument redaction, and durable
// write-intent / idempotency tracking shared across tools, external APIs, and
// session actors.
package execguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Decision is the execution-boundary verdict for an action invocation.
type Decision string

const (
	DecisionAllow           Decision = "allow"
	DecisionRequireApproval Decision = "require_approval"
	DecisionDeny            Decision = "deny"
)

// ActionKind classifies an action's side-effect profile. Hosted deployments
// deny ActionUnclassified by default so unreviewed third-party or remote tools
// cannot execute implicitly.
type ActionKind string

const (
	ActionUnclassified ActionKind = ""
	ActionRead         ActionKind = "read"
	ActionWrite        ActionKind = "write"
	ActionDestructive  ActionKind = "destructive"
)

// ParseActionKind validates an explicit action classification string.
func ParseActionKind(raw string) (ActionKind, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ActionUnclassified, true
	case "read":
		return ActionRead, true
	case "write":
		return ActionWrite, true
	case "destructive":
		return ActionDestructive, true
	default:
		return ActionUnclassified, false
	}
}

// Caller captures the core-stamped identity, origin channel, capabilities, and
// deployment posture for one invocation. Never populated from script arguments.
type Caller struct {
	Agent             string
	SessionKey        string
	WorkspaceScope    string
	PersonalUUID      string
	Channel           string
	ReplyTo           string
	ApproverPrincipal string
	ProvenanceKind    string
	GrantedCaps       map[string]bool
	HostedMode        bool
}

// ActionRequest describes one canonical tool or external API invocation to
// authorize at the shared runtime boundary.
type ActionRequest struct {
	CanonicalID        string
	Revision           string
	Kind               ActionKind
	Destination        string
	Description        string
	RequiredCapability string
	NeedsConfirm       bool
	PolicyForbidden    bool
	Args               map[string]any
	ArgsHash           string
	Caller             Caller
	ApprovalID         string
	IdempotencyKey     string
}

// ActionRevision hashes action definition metadata so any config, schema, or
// policy change invalidates previously issued pending/approved records.
func ActionRevision(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Evaluate applies all authorization gates in strict order:
// policy forbid -> missing capability -> hosted-mode unclassified deny -> approval gate.
func Evaluate(req ActionRequest) (Decision, string) {
	if req.PolicyForbidden {
		return DecisionDeny, fmt.Sprintf("action %q is forbidden by policy", req.CanonicalID)
	}
	if req.RequiredCapability != "" && !req.Caller.GrantedCaps[req.RequiredCapability] {
		return DecisionDeny, fmt.Sprintf("agent %q lacks capability %q for %q", req.Caller.Agent, req.RequiredCapability, req.CanonicalID)
	}
	if req.Caller.HostedMode && req.Kind == ActionUnclassified {
		return DecisionDeny, fmt.Sprintf("hosted mode denies unclassified action %q", req.CanonicalID)
	}
	if req.NeedsConfirm {
		return DecisionRequireApproval, "approval required"
	}
	return DecisionAllow, ""
}
