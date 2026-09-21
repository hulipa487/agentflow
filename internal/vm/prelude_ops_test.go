package vm

import (
	"encoding/json"
	"testing"
)

// The Lua-facing op builders are the only way a loop reaches these features, so
// a field one of them drops is a feature no loop can reach — and silently, when
// the Go handler reads an empty value and does the plain thing.
//
// shell.spawn is the case that was wrong: the engine's checkout (project + ref
// into the first volume mount) and scratch_mount are documented on shell.spawn,
// and the builder did not forward them, so a loop asking for a checkout got a
// plain container and no error.
func TestShellSpawnForwardsCheckoutAndScratch(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}

	status, msg := st.Start("loop", `
local h = shell.spawn({ image = "alpine:3.20", provider = "docker",
                        project = "proj", ref = "main",
                        volumes = { "data:/work" }, scratch_mount = "/scratch" })
return h
`)
	if status != Yielded {
		t.Fatalf("expected a yielded op request, got status %v: %s", status, msg)
	}

	var op struct {
		Type     string   `json:"type"`
		Image    string   `json:"image"`
		Project  string   `json:"project"`
		Ref      string   `json:"ref"`
		Volumes  []string `json:"volumes"`
		Scratch  string   `json:"scratch_mount"`
		Provider string   `json:"provider"`
	}
	if err := json.Unmarshal([]byte(msg), &op); err != nil {
		t.Fatalf("op payload is not JSON (%q): %v", msg, err)
	}
	if op.Type != "shell.spawn" {
		t.Fatalf("op type = %q", op.Type)
	}
	if op.Project != "proj" || op.Ref != "main" {
		t.Fatalf("the spawn carried no checkout: %s", msg)
	}
	if len(op.Volumes) != 1 || op.Volumes[0] != "data:/work" {
		t.Fatalf("the spawn carried no volumes: %s", msg)
	}
	if op.Scratch != "/scratch" {
		t.Fatalf("the spawn carried no scratch mount: %s", msg)
	}
	if op.Image != "alpine:3.20" || op.Provider != "docker" {
		t.Fatalf("the plain fields stopped being forwarded: %s", msg)
	}
}
