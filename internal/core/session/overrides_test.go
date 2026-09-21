package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// stubSettings stands in for the identity-backed lookup.
type stubSettings struct {
	model        string
	instructions string
	err          error
}

func (s stubSettings) Settings(context.Context, string) (string, string, error) {
	if s.err != nil {
		return "", "", s.err
	}
	return s.model, s.instructions, nil
}

func actorForInfo() *Actor {
	b := &StringBox{}
	b.Store("You are a helpful bot.")
	return &Actor{
		Info:     &Info{Name: "bot", Model: "base-model", HistoryBudget: 100, Instructions: b},
		Identity: Identity{SessionID: "bot|chat-1"},
	}
}

func infoOf(t *testing.T, a *Actor, ctx context.Context) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(a.infoJSON(ctx)), &got); err != nil {
		t.Fatalf("agent.info is not JSON: %v", err)
	}
	return got
}

// agent.info carries the person's overrides, which is what makes a per-user
// model and prompt engine-enforced for every loop that reads its own model and
// system prompt from here — the documented idiom, and no Lua changes.
func TestAgentInfoCarriesPerUserOverrides(t *testing.T) {
	a := actorForInfo()
	bg := context.Background()

	// Default: the agent's own model and instructions, nothing layered.
	got := infoOf(t, a, bg)
	if got["model"] != "base-model" || got["instructions"] != "You are a helpful bot." {
		t.Fatalf("unoverridden info = %v", got)
	}
	if _, present := got["agent_model"]; present {
		t.Fatalf("agent_model should only appear alongside an override: %v", got)
	}

	a.SetProfileSettings(stubSettings{model: "premium", instructions: "Speak Cantonese."})
	ctx := WithUserUUID(bg, "u_1")
	got = infoOf(t, a, ctx)
	if got["model"] != "premium" {
		t.Fatalf("model = %v, want the profile's", got["model"])
	}
	if got["agent_model"] != "base-model" {
		t.Fatalf("agent_model = %v, want what the agent would have used", got["agent_model"])
	}
	if got["instructions"] != "You are a helpful bot.\n\nSpeak Cantonese." {
		t.Fatalf("instructions = %q, want the agent's with the person's layer appended", got["instructions"])
	}
	if got["instructions_append"] != "Speak Cantonese." {
		t.Fatalf("instructions_append = %v", got["instructions_append"])
	}

	// Engine work with nobody behind it: the overrides do not apply.
	got = infoOf(t, a, bg)
	if got["model"] != "base-model" || got["instructions"] != "You are a helpful bot." {
		t.Fatalf("a turn with no user must not pick up overrides: %v", got)
	}

	// A lookup that fails leaves the agent's own values in place: a profile
	// read must never fail a person's turn.
	a.SetProfileSettings(stubSettings{err: errors.New("identity store unreachable")})
	got = infoOf(t, a, WithUserUUID(bg, "u_1"))
	if got["model"] != "base-model" || got["instructions"] != "You are a helpful bot." {
		t.Fatalf("a failed lookup must fall back to the agent's values: %v", got)
	}
}

// An agent with no instructions of its own gets the person's layer alone,
// rather than a leading blank paragraph.
func TestLayeredInstructionsWithNoBase(t *testing.T) {
	if got := layerInstructions("", "Only this."); got != "Only this." {
		t.Fatalf("layerInstructions(\"\", x) = %q", got)
	}
	if got := layerInstructions("Base.", ""); got != "Base." {
		t.Fatalf("layerInstructions(x, \"\") = %q", got)
	}
}
