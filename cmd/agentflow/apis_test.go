package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/ext"
	"agentflow/internal/core/hosthttp"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/tools"
)

func TestRegisterConfiguredAPIsDirAndInline(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pkgDir := filepath.Join(dir, "apis", "mathpkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "manifest.yaml"), []byte(`
name: mathpkg
runtime: script
entry: ./index.ts
actions:
  add:
    description: "Add two numbers"
    expose_as_tool: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "index.ts"), []byte(`
export function add(args: { a: number; b: number }): number {
  return args.a + args.b;
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	inlineFile := filepath.Join(dir, "greeter.ts")
	if err := os.WriteFile(inlineFile, []byte(`
export const greet = (args: { name: string }): string => "hi " + args.name;
`), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := ext.NewRegistry(ext.HostDeps{
		HTTP: hosthttp.New(log, nil, netguard.Policy{}),
		Log:  log,
	})
	defer reg.Close(context.Background())

	apis := map[string]config.APIConfig{
		"mathpkg": {Dir: "./apis/mathpkg"},
		"greeter": {
			Runtime: "script",
			Entry:   "./greeter.ts",
			Actions: map[string]config.APIActionConfig{
				"greet": {Description: "Greet user", ExposeAsTool: true},
			},
		},
	}
	if err := registerConfiguredAPIs(reg, apis, dir); err != nil {
		t.Fatalf("registerConfiguredAPIs: %v", err)
	}

	toolReg := tools.NewRegistry()
	reg.RegisterTools(toolReg)

	res, err := reg.Call(context.Background(), "mathpkg", "add", map[string]any{"a": 2, "b": 3})
	if err != nil || res != float64(5) {
		t.Fatalf("mathpkg.add = (%v, %v); want 5", res, err)
	}
	res2, err := reg.Call(context.Background(), "greeter", "greet", map[string]any{"name": "AgentFlow"})
	if err != nil || res2 != "hi AgentFlow" {
		t.Fatalf("greeter.greet = (%v, %v)", res2, err)
	}

	exposed := toolReg.Expose(nil, config.ToolsPolicy{Default: "all"}, false).RequireCapabilities(map[string]bool{"apis": true})
	if _, ok := exposed.ByName["api:mathpkg.add"]; !ok {
		t.Fatalf("missing api:mathpkg.add in tool registry")
	}
	if _, ok := exposed.ByName["api:greeter.greet"]; !ok {
		t.Fatalf("missing api:greeter.greet in tool registry")
	}
}
