package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func loadAdminConfig(t *testing.T, adminBlock string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow.yaml")
	src := `version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
agents:
  echo: { loop: ./echo.lua }
runtime:
  admin:
` + adminBlock
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestAdminTransportValidation(t *testing.T) {
	if _, err := loadAdminConfig(t, "    tcp: false\n"); err == nil || !strings.Contains(err.Error(), "no runtime.admin.socket") {
		t.Fatalf("tcp-only disable: %v", err)
	}

	socket := "/run/agentflow/admin.sock"
	if runtime.GOOS == "windows" {
		socket = `\\.\pipe\agentflow-admin-test`
	}
	cfg, err := loadAdminConfig(t, "    tcp: false\n    socket: \""+strings.ReplaceAll(socket, `\`, `\\`)+"\"\n")
	if err != nil {
		t.Fatalf("socket-only admin: %v", err)
	}
	if cfg.Runtime.Admin.TCPEnabled() {
		t.Fatal("tcp should be disabled")
	}
	if cfg.Runtime.Admin.Socket != socket {
		t.Fatalf("socket = %q, want %q", cfg.Runtime.Admin.Socket, socket)
	}

	if _, err := loadAdminConfig(t, "    socket: \""+strings.ReplaceAll(socket, `\`, `\\`)+"\"\n"); err != nil {
		t.Fatalf("socket with default tcp: %v", err)
	}
}

// The admin plane has no port of its own: a config that still names one is
// told where the admin API went instead of being silently ignored.
func TestAdminListenIsRetired(t *testing.T) {
	_, err := loadAdminConfig(t, "    listen: 127.0.0.1:9090\n")
	if err == nil || !strings.Contains(err.Error(), "runtime.admin.listen was removed") || !strings.Contains(err.Error(), "gateway.listen") {
		t.Fatalf("admin.listen: %v", err)
	}
}

func TestReservedChannelPaths(t *testing.T) {
	for p, reserved := range map[string]bool{
		"/":             false,
		"/webhook/x/":   false,
		"/administer/":  false,
		"/admin/":       true,
		"/admin/hooks/": true,
		"/healthz/":     true,
		"/readyz/":      true,
		"/metrics/":     true,
		"/health/":      true,
		"/v1/users/":    true,
		"/v1/users/x/":  true,
		"/v1/other/":    false,
	} {
		if got := reservedChannelPath(p); got != reserved {
			t.Errorf("reservedChannelPath(%q) = %v, want %v", p, got, reserved)
		}
	}
}
