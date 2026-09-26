package session

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/pool"
)

// Define + applyDefAtRestart are the runtime agent registry's path into a
// live session: the update is staged by the caller's goroutine and applied on
// the actor's, so the field swaps need no locks. These tests pin both halves
// of that contract.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDefineStagesAndSignals(t *testing.T) {
	a := New("s|k", Identity{SessionID: "s|k"}, &Info{Name: "s"}, nil, nil, nil, nil, nil,
		map[string]OpHandler{}, pool.New(1), quietLogger())

	next := DefUpdate{
		Info:     &Info{Name: "s", Model: "m2"},
		Handlers: map[string]OpHandler{"probe": func(ctx context.Context, op Op) (string, bool) { return `"ok"`, true }},
		CanContact: map[string]bool{
			"other": true,
		},
		Capabilities: map[string]bool{"llm.chat": true},
		LoopSrc:      "-- staged",
		LoopFile:     "/tmp/loop.lua",
	}
	a.Define(next)

	// Staged, and the restart is requested.
	if a.pendingDef.Load() == nil {
		t.Fatal("Define staged nothing")
	}
	select {
	case <-a.reload:
	default:
		t.Fatal("Define did not signal a restart")
	}
}

func TestApplyAtRestartSwapsEveryField(t *testing.T) {
	oldInfo := &Info{Name: "s", Model: "m1"}
	oldHandlers := map[string]OpHandler{}
	a := New("s|k", Identity{SessionID: "s|k", CanContact: map[string]bool{"x": true}}, oldInfo,
		nil, nil, nil, nil, nil, oldHandlers, pool.New(1), quietLogger())

	next := DefUpdate{
		Info:         &Info{Name: "s", Model: "m2"},
		Handlers:     map[string]OpHandler{"probe": func(ctx context.Context, op Op) (string, bool) { return `"ok"`, true }},
		CanContact:   map[string]bool{"y": true},
		Capabilities: map[string]bool{"memory": true},
		LoopSrc:      "-- staged",
		LoopFile:     "/tmp/loop.lua",
	}
	a.pendingDef.Store(&next)

	a.applyDefAtRestart()

	if a.Info != next.Info {
		t.Error("Info was not swapped")
	}
	if _, ok := a.handlers["probe"]; !ok {
		t.Error("handlers were not swapped")
	}
	if !a.Identity.CanContact["y"] || a.Identity.CanContact["x"] {
		t.Errorf("CanContact = %v; want the replacement's", a.Identity.CanContact)
	}
	if !a.Identity.Capabilities["memory"] {
		t.Errorf("Capabilities = %v; want the replacement's", a.Identity.Capabilities)
	}
	if a.LoopSrc != "-- staged" || a.LoopFile != "/tmp/loop.lua" {
		t.Errorf("loop source not swapped: %q / %q", a.LoopSrc, a.LoopFile)
	}

	// The update is consumed: a second restart applies nothing, so a snapshot
	// is idempotent rather than replaying stale state.
	a.applyDefAtRestart()
	if a.Info != next.Info {
		t.Error("a consumed update was replayed")
	}
}
