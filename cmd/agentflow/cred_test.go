package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credTestEnv materializes a minimal config pointing the credential store at
// a temp path and sets the master key env var it names.
func credTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agentflow.yaml")
	body := `
version: "1"
runtime:
  persistence: sqlite://` + filepath.ToSlash(filepath.Join(dir, "agentflow.db")) + `
  credentials:
    enabled: true
    path: ` + filepath.ToSlash(filepath.Join(dir, "credentials.db")) + `
    master_key_env: AF_TEST_MASTER_KEY
agents:
  bot: { loop: plugin:per_chat }
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AF_TEST_MASTER_KEY", "test-master-key")
	return cfg
}

// TestCredRoundTripStdin: set reads the value from stdin (never argv), get
// returns it, list shows names only, delete removes it.
func TestCredRoundTripStdin(t *testing.T) {
	cfg := credTestEnv(t)

	var out, errOut bytes.Buffer
	if code := credMain([]string{"-config", cfg, "set", "github_token"},
		strings.NewReader("sekret-value\n"), &out, &errOut); code != 0 {
		t.Fatalf("set failed (%d): %s %s", code, out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := credMain([]string{"-config", cfg, "get", "github_token"},
		strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("get failed (%d): %s %s", code, out.String(), errOut.String())
	}
	if got := out.String(); strings.TrimRight(got, "\r\n") != "sekret-value" {
		t.Fatalf("get = %q, want the stored value", got)
	}

	// list: names only — the value must not appear.
	out.Reset()
	errOut.Reset()
	if code := credMain([]string{"-config", cfg, "list"},
		strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("list failed (%d): %s %s", code, out.String(), errOut.String())
	}
	names := out.String()
	if !strings.Contains(names, "github_token") {
		t.Fatalf("list missing the name: %q", names)
	}
	if strings.Contains(names, "sekret-value") {
		t.Fatal("list must never show values")
	}

	// Re-set (upsert) then delete; get afterwards fails.
	out.Reset()
	errOut.Reset()
	if code := credMain([]string{"-config", cfg, "set", "github_token"},
		strings.NewReader("second-value\n"), &out, &errOut); code != 0 {
		t.Fatalf("re-set failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := credMain([]string{"-config", cfg, "get", "github_token"},
		strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "second-value") {
		t.Fatalf("upsert not visible: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := credMain([]string{"-config", cfg, "delete", "github_token"},
		strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("delete failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := credMain([]string{"-config", cfg, "get", "github_token"},
		strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("get after delete must fail")
	}
}

// TestCredMissingMasterKey: an unset master key env var is a clear error,
// before anything touches the store.
func TestCredMissingMasterKey(t *testing.T) {
	cfg := credTestEnv(t)
	t.Setenv("AF_TEST_MASTER_KEY", "")

	var out, errOut bytes.Buffer
	code := credMain([]string{"-config", cfg, "set", "x"},
		strings.NewReader("v\n"), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "AF_TEST_MASTER_KEY") {
		t.Fatalf("error must name the env var: %s", errOut.String())
	}
}

// TestCredEmptyValueRejected: an empty stdin yields an error, not a stored
// empty secret.
func TestCredEmptyValueRejected(t *testing.T) {
	cfg := credTestEnv(t)
	var out, errOut bytes.Buffer
	if code := credMain([]string{"-config", cfg, "set", "x"},
		strings.NewReader("\n"), &out, &errOut); code == 0 {
		t.Fatal("empty value must fail")
	}
	if !strings.Contains(errOut.String(), "empty") {
		t.Fatalf("error should say why: %s", errOut.String())
	}
}

// TestCredUsageErrors: unknown commands and missing names are usage errors.
func TestCredUsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"get"},
		{"list", "unexpected-name"},
	} {
		if code := credMain(args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("args %v: exit = %d, want 2", args, code)
		}
		out.Reset()
		errOut.Reset()
	}
}

// TestHoistFlags: flags after the positional command still parse.
func TestHoistFlags(t *testing.T) {
	got := hoistFlags([]string{"set", "name", "-config", "cfg.yaml", "-configdir", "d"})
	want := "-config cfg.yaml -configdir d set name"
	if strings.Join(got, " ") != want {
		t.Fatalf("hoistFlags = %v, want [%s]", got, want)
	}
	if got := hoistFlags([]string{"get", "-h"}); strings.Join(got, " ") != "get -h" {
		t.Fatalf("lone trailing flag must stay: %v", got)
	}
}

// TestCredFlagsAfterCommand: the natural CLI spelling works — the store path
// comes from -configdir even though it follows the positional name.
func TestCredFlagsAfterCommand(t *testing.T) {
	cfg := credTestEnv(t)
	var out, errOut bytes.Buffer
	if code := credMain([]string{"set", "late_flag", "-config", cfg},
		strings.NewReader("v-late\n"), &out, &errOut); code != 0 {
		t.Fatalf("set failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	if code := credMain([]string{"get", "late_flag", "-config", cfg},
		strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "v-late") {
		t.Fatalf("get failed: code=%d out=%q", code, out.String())
	}
}
