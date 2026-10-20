package tools

import (
	"context"
	"testing"

	"agentflow/internal/config"
)

func TestRequireCapabilitiesHidesFetchWithoutNetHTTP(t *testing.T) {
	r := NewRegistry()
	r.Register(ToolSpec{
		Name:               "builtin:fetch",
		RequiresCapability: "net.http",
		Invoke:             func(context.Context, map[string]any) (any, error) { return nil, nil },
	})
	set := r.Expose(nil, config.ToolsPolicy{}, false)
	if got := len(set.RequireCapabilities(map[string]bool{"tools": true}).Tools); got != 0 {
		t.Fatalf("fetch visible without net.http: %d tools", got)
	}
	if got := len(set.RequireCapabilities(map[string]bool{"tools": true, "net.http": true}).Tools); got != 1 {
		t.Fatalf("fetch hidden with net.http: %d tools", got)
	}
}
