package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// A flag that was removed names its replacement instead of failing with Go's
// generic "flag provided but not defined".
func TestRemovedFlagsNameTheirReplacement(t *testing.T) {
	cases := []struct {
		name string
		run  func(args []string, stdout, stderr io.Writer) int
		args []string
		want string
	}{
		{"run -configdir", runArgs, []string{"-configdir", "d"}, "-configdir was removed; use -config DIR"},
		{"run -workers", runArgs, []string{"-workers", "4"}, "set runtime.scheduler.workers"},
		{"run -log-json", runArgs, []string{"-log-json"}, "use -log-format json"},
		{"run -no-webui", runArgs, []string{"-no-webui"}, "use -no-console"},
		{"validate -configdir", cmdValidate, []string{"-configdir=d"}, "use -config DIR"},
		{"init -dir", cmdInit, []string{"-dir", "x"}, "agentflow init DIR"},
		{"init -configdir", cmdInit, []string{"-configdir"}, "use -layout dir"},
		{"user export -id", cmdUser, []string{"export", "-id", "u_1"}, "agentflow user export USER"},
		{"user keygen -out", cmdUser, []string{"keygen", "-out", "k"}, "agentflow user keygen PREFIX"},
		{"migrate blobs -from", cmdMigrate, []string{"blobs", "-from", "fs:d"}, "agentflow migrate blobs DIR"},
		{"cred list -format", cmdCred, []string{"list", "-format", "json"}, "use -json"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := c.run(c.args, &out, &errOut); code != 2 {
			t.Errorf("%s: exit = %d, want 2", c.name, code)
		}
		if !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%s: stderr = %q, want it to mention %q", c.name, errOut.String(), c.want)
		}
	}
}

// runArgs is the engine's flag parsing as a command, for the table above.
func runArgs(args []string, _, stderr io.Writer) int {
	_, code, _ := parseRunFlags(args, stderr)
	return code
}

func TestHoistKeepsBooleansAndPositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("user", "", "")
	fs.Bool("json", false, "")
	got := hoist(fs, []string{"NAME", "-json", "OTHER", "-user", "u_1", "-x=1", "--", "-literal"})
	want := "-json -user u_1 -x=1 -- NAME OTHER -literal"
	if strings.Join(got, " ") != want {
		t.Fatalf("hoist = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestHelpFlagIsSuccess(t *testing.T) {
	for _, args := range [][]string{
		{"validate", "-h"}, {"doctor", "--help"}, {"cred", "set", "-h"}, {"user", "-h"},
		{"migrate", "store", "-h"}, {"init", "-help"}, {"loop", "check", "-h"},
	} {
		var out, errOut bytes.Buffer
		handled, code := runSubcommand(args, &out, &errOut)
		if !handled || code != 0 {
			t.Errorf("%v: handled=%v code=%d", args, handled, code)
		}
		if !strings.Contains(errOut.String(), "usage: agentflow") {
			t.Errorf("%v: no usage printed: %q", args, errOut.String())
		}
	}
	// `agentflow run -h` is the engine path's own help.
	var errOut bytes.Buffer
	if _, code, ok := parseRunFlags([]string{"-h"}, &errOut); ok || code != 0 {
		t.Fatalf("run -h: ok=%v code=%d", ok, code)
	}
}

func TestPositionalArgumentsAreChecked(t *testing.T) {
	cases := [][]string{
		{"validate", "stray"},
		{"loop", "check"},
		{"loop", "check", "a.lua", "b.lua"},
		{"user", "export"},
		{"user", "import"},
		{"migrate", "blobs"},
		{"init", "a", "b"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		if _, code := runSubcommand(args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit = %d, want 2 (%s)", args, code, errOut.String())
		}
	}
	var errOut bytes.Buffer
	if _, code, _ := parseRunFlags([]string{"stray"}, &errOut); code != 2 {
		t.Fatalf("run with a stray argument: exit = %d", code)
	}
}

// Flags that do nothing for a cred command are refused, not ignored.
func TestCredRejectsFlagsThatDoNotApply(t *testing.T) {
	for _, args := range [][]string{
		{"get", "name", "-kind", "oauth"},
		{"list", "-header", "X-Key"},
		{"delete", "name", "-json"},
		{"set", "name", "-json"},
	} {
		var out, errOut bytes.Buffer
		if code := credMain(args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "flag provided but not defined") {
			t.Errorf("%v: stderr = %q", args, errOut.String())
		}
	}
}

func TestCredListJSON(t *testing.T) {
	cfg := credTestEnv(t)
	var out, errOut bytes.Buffer
	if code := credMain([]string{"set", "svc", "-config", cfg}, strings.NewReader("v\n"), &out, &errOut); code != 0 {
		t.Fatalf("set: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := credMain([]string{"list", "-config", cfg, "-json"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("list -json: %d %s", code, errOut.String())
	}
	var refs []map[string]any
	if err := json.Unmarshal(out.Bytes(), &refs); err != nil || len(refs) != 1 {
		t.Fatalf("list -json = %q (%v)", out.String(), err)
	}
	if strings.Contains(out.String(), `"v"`) {
		t.Fatal("list printed a value")
	}
}

func writeInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if code := cmdInit([]string{dir}, &out, &errOut); code != 0 {
		t.Fatalf("init: %d %s", code, errOut.String())
	}
	return dir
}

func TestConfigShowAndDoctorJSON(t *testing.T) {
	dir := writeInit(t)
	var out, errOut bytes.Buffer
	if code := cmdConfigShow([]string{"-config", dir, "-json"}, &out, &errOut); code != 0 {
		t.Fatalf("config show -json: %d %s", code, errOut.String())
	}
	var sum configSummary
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil {
		t.Fatalf("config show -json is not JSON: %v\n%s", err, out.String())
	}
	if sum.Agents != 1 || len(sum.Loops) != 1 || sum.Loops[0].Name != "echo" || sum.Kind != "file" {
		t.Fatalf("summary = %+v", sum)
	}

	out.Reset()
	if code := cmdDoctor([]string{"-config", dir, "-json"}, &out, &errOut); code != 0 {
		t.Fatalf("doctor -json: %d %s\n%s", code, errOut.String(), out.String())
	}
	var results []doctorResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) == 0 {
		t.Fatalf("doctor -json = %q (%v)", out.String(), err)
	}
	if results[0].Name != "configuration" || results[0].Status != "PASS" {
		t.Fatalf("first result = %+v", results[0])
	}
}

func TestInitIntoADirectoryTheEngineCanFind(t *testing.T) {
	dir := writeInit(t)
	src, err := config.ResolveSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	if src.IsDir || src.Path != filepath.Join(dir, "agentflow.yaml") {
		t.Fatalf("-config %s resolved to %+v", dir, src)
	}
}

func TestResolveAdminPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentflow.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: \"1\"\ngateway:\n  listen: \":9191\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // nothing to discover
	t.Setenv("AGENTFLOW_URL", "")
	t.Setenv("ADMIN_TOKEN", "env-token")

	url, token, err := resolveAdmin("", "", "")
	if err != nil || url != defaultAdminURL || token != "env-token" {
		t.Fatalf("defaults = %q %q %v", url, token, err)
	}
	url, _, err = resolveAdmin("", "", cfgPath)
	if err != nil || url != "http://127.0.0.1:9191" {
		t.Fatalf("from config = %q %v", url, err)
	}
	t.Setenv("AGENTFLOW_URL", "http://env:1/")
	url, _, _ = resolveAdmin("", "", cfgPath)
	if url != "http://env:1" {
		t.Fatalf("env should beat the config: %q", url)
	}
	url, token, _ = resolveAdmin("http://flag:2", "flag-token", cfgPath)
	if url != "http://flag:2" || token != "flag-token" {
		t.Fatalf("flags should win: %q %q", url, token)
	}
	t.Setenv("AGENTFLOW_URL", "")
	if _, _, err := resolveAdmin("", "", filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("a named config that does not exist was ignored")
	}
}

func TestAdminURLFromConfig(t *testing.T) {
	no := false
	cases := map[string]string{
		"":               "http://127.0.0.1:8080",
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"0.0.0.0:8000":   "http://127.0.0.1:8000",
		"[::]:8000":      "http://[::1]:8000",
		"admin.lan:9000": "http://admin.lan:9000",
	}
	for listen, want := range cases {
		cfg := &config.Config{}
		cfg.Gateway.Listen = listen
		got, err := adminURLFromConfig(cfg)
		if err != nil || got != want {
			t.Errorf("listen %q = %q, %v; want %q", listen, got, err, want)
		}
	}
	cfg := &config.Config{}
	cfg.Runtime.Admin.TCP = &no
	if _, err := adminURLFromConfig(cfg); err == nil {
		t.Fatal("a socket-only admin API produced a URL")
	}
}
