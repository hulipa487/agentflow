// Package address parses the narrow, core-owned agent address grammar.
package address

import (
	"fmt"
	"strings"
)

// Kind identifies the target class encoded in an address.
type Kind string

const (
	Agent   Kind = "agent"
	Session Kind = "session"
	User    Kind = "user"
)

// Address is a parsed agent or session destination. New is only meaningful
// for agent:<name>:new, which asks the supervisor to create an ephemeral
// session for that agent template.
type Address struct {
	Kind    Kind
	Agent   string
	Session string
	New     bool
	User    string
}

// Parse accepts only agent:<name>, agent:<name>:new, session:<id>, and
// user:<uuid>. Names and IDs may contain letters, digits, _, -, and .; a
// user UUID may contain letters, digits, _, and - (the minted "u_<base62>"
// format); accepting any other punctuation would make address splitting and
// authorization ambiguous.
func Parse(raw string) (Address, error) {
	if rest, ok := strings.CutPrefix(raw, "session:"); ok {
		if !validSession(rest) {
			return Address{}, fmt.Errorf("invalid session address %q", raw)
		}
		return Address{Kind: Session, Session: rest}, nil
	}
	if rest, ok := strings.CutPrefix(raw, "user:"); ok {
		if !validUser(rest) {
			return Address{}, fmt.Errorf("invalid user address %q", raw)
		}
		return Address{Kind: User, User: rest}, nil
	}

	parts := strings.Split(raw, ":")
	switch {
	case len(parts) == 2 && parts[0] == string(Agent):
		if !valid(parts[1]) {
			return Address{}, fmt.Errorf("invalid agent address %q", raw)
		}
		return Address{Kind: Agent, Agent: parts[1], New: false}, nil
	case len(parts) == 3 && parts[0] == string(Agent) && parts[2] == "new":
		if !valid(parts[1]) {
			return Address{}, fmt.Errorf("invalid agent address %q", raw)
		}
		return Address{Kind: Agent, Agent: parts[1], New: true}, nil
	default:
		return Address{}, fmt.Errorf("invalid address %q", raw)
	}
}

func (a Address) String() string {
	switch a.Kind {
	case Agent:
		if a.New {
			return "agent:" + a.Agent + ":new"
		}
		return "agent:" + a.Agent
	case Session:
		return "session:" + a.Session
	case User:
		return "user:" + a.User
	default:
		return ""
	}
}

func valid(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '|' {
			continue
		}
		return false
	}
	return true
}

func validSession(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == '|' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func validUser(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
