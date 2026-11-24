package ext

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/hosthttp"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
)

func extLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestScriptExternalAPIAndToolBridge(t *testing.T) {
	creds, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "test-master-key", extLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer creds.Close()

	ctx := session.WithPersonalIdentity(context.Background(), "u1")
	if err := creds.Put(ctx, "u1", "stripe", "api_key", "sk_test_secret_42", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := creds.Put(ctx, "u1", "other", "api_key", "other_secret", "", ""); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"charged":true,"echo":`+string(b)+`}`)
	}))
	defer srv.Close()

	parsedURL, _ := url.Parse(srv.URL)
	pkgDir := t.TempDir()
	manifestYAML := `
name: billing
version: "1.0.0"
runtime: script
entry: ./index.ts
timeout: 200ms
capabilities:
  http:
    allowed_hosts: ["` + parsedURL.Hostname() + `"]
    credentials: ["stripe"]
actions:
  charge:
    description: "Charge an amount"
    expose_as_tool: true
    parameters:
      type: object
      required: [amount]
      properties:
        amount: { type: number }
  bad_host:
    description: "Attempt unauthorized host"
  bad_cred:
    description: "Attempt unauthorized credential"
  spin:
    description: "Infinite loop"
`
	indexTS := `
import type { ExtHost } from "./af";

interface ChargeArgs {
  amount: number;
  url: string;
}

export function charge(args: ChargeArgs, host: ExtHost): unknown {
  host.log.info("billing.charge starting");
  const resp = host.http.request({
    method: "POST",
    url: args.url,
    auth_service: "stripe",
    json: { amount: args.amount },
  });
  return { status: resp.status, body: JSON.parse(resp.body) };
}

export function bad_host(args: { url: string }, host: ExtHost): unknown {
  return host.http.request({ url: args.url });
}

export function bad_cred(args: { url: string }, host: ExtHost): unknown {
  return host.http.request({ url: args.url, auth_service: "other" });
}

export function spin(): void {
  while (true) {}
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "index.ts"), []byte(indexTS), 0o644); err != nil {
		t.Fatal(err)
	}

	httpClient := hosthttp.New(extLogger(), creds, netguard.Policy{AllowPrivate: true})
	reg := NewRegistry(HostDeps{HTTP: httpClient, Log: extLogger()})
	defer reg.Close(context.Background())

	if err := reg.RegisterDir(pkgDir); err != nil {
		t.Fatalf("RegisterDir: %v", err)
	}

	// Normal invocation with TS types, credential injection, and JSON return.
	res, err := reg.Call(ctx, "billing", "charge", map[string]any{"amount": 99, "url": srv.URL + "/v1/charges"})
	if err != nil {
		t.Fatalf("Call charge: %v", err)
	}
	if gotAuth != "Bearer sk_test_secret_42" {
		t.Fatalf("expected host-injected credential, got %q", gotAuth)
	}
	resMap := res.(map[string]any)
	if resMap["status"] != float64(200) {
		t.Fatalf("unexpected charge response: %+v", resMap)
	}

	// Allowed host enforcement.
	if _, err := reg.Call(ctx, "billing", "bad_host", map[string]any{"url": "http://localhost:" + parsedURL.Port()}); err == nil || !strings.Contains(err.Error(), "not in allowed_hosts") {
		t.Fatalf("expected allowed_hosts error, got %v", err)
	}

	// Allowed credential enforcement.
	if _, err := reg.Call(ctx, "billing", "bad_cred", map[string]any{"url": srv.URL}); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected credential allowlist error, got %v", err)
	}

	// Timeout enforcement.
	if _, err := reg.Call(ctx, "billing", "spin", nil); err == nil {
		t.Fatal("expected timeout error on infinite loop")
	}

	// Parallel batch execution.
	batch := reg.CallBatch(ctx, []BatchCall{
		{API: "billing", Action: "charge", Args: map[string]any{"amount": 10, "url": srv.URL}},
		{API: "billing", Action: "bad_host", Args: map[string]any{"url": "https://evil.example"}},
		{API: "billing", Action: "charge", Args: map[string]any{"amount": 20, "url": srv.URL}},
	})
	if len(batch) != 3 || !batch[0].OK || batch[1].OK || !batch[2].OK {
		t.Fatalf("unexpected batch results: %+v", batch)
	}

	// Tool registry bridge (`api:billing.charge` gated behind `apis` capability).
	toolReg := tools.NewRegistry()
	reg.RegisterTools(toolReg)

	exposed := toolReg.Expose(nil, config.ToolsPolicy{Default: "all"}, false).RequireCapabilities(map[string]bool{"apis": true})
	tSpec, ok := exposed.ByName["api:billing.charge"]
	if !ok {
		t.Fatalf("api:billing.charge not exposed: %+v", exposed.ByName)
	}
	tRes, err := tSpec.Invoke(ctx, map[string]any{"amount": 7, "url": srv.URL})
	if err != nil {
		t.Fatalf("tool invoke failed: %v", err)
	}
	if tRes.(map[string]any)["status"] != float64(200) {
		t.Fatalf("unexpected tool invoke result: %+v", tRes)
	}

	hidden := toolReg.Expose(nil, config.ToolsPolicy{Default: "all"}, false).RequireCapabilities(map[string]bool{})
	if _, ok := hidden.ByName["api:billing.charge"]; ok {
		t.Fatal("api:billing.charge must be hidden without apis capability")
	}
}

func TestWasmExternalAPIOnWazero(t *testing.T) {
	// Compile a real Go Wasm (wasip1 reactor) guest to test end-to-end wazero hostcalls,
	// linear memory passing, credential injection, and concurrent invocations.
	srcDir := t.TempDir()
	wasmGoMod := "module wasmapi\n\ngo 1.26.0\n"
	wasmMainGo := `package main

import (
	"encoding/json"
	"unsafe"
)

//go:wasmimport af_host http_request
func hostHTTPRequest(reqPtr, reqLen uint32) uint64

//go:wasmimport af_host log
func hostLog(level uint32, msgPtr, msgLen uint32)

var pinned = map[uint32][]byte{}

//go:wasmexport alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(&buf[0])))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport dealloc
func dealloc(ptr, size uint32) {
	delete(pinned, ptr)
}

func readGuest(ptr, size uint32) []byte {
	if ptr == 0 || size == 0 {
		return nil
	}
	out := make([]byte, size)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), size))
	return out
}

func packResp(v any) uint64 {
	b, _ := json.Marshal(v)
	p := alloc(uint32(len(b)))
	copy(pinned[p], b)
	return (uint64(p) << 32) | uint64(len(b))
}

//go:wasmexport af_invoke
func afInvoke(reqPtr, reqLen uint32) uint64 {
	msg := "wasm invoke"
	msgBuf := []byte(msg)
	hostLog(1, uint32(uintptr(unsafe.Pointer(&msgBuf[0]))), uint32(len(msgBuf)))

	raw := readGuest(reqPtr, reqLen)
	var req struct {
		Action string         ` + "`json:\"action\"`" + `
		Args   map[string]any ` + "`json:\"args\"`" + `
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return packResp(map[string]any{"ok": false, "error": err.Error()})
	}

	httpReq := map[string]any{
		"method":       "GET",
		"url":          req.Args["url"],
		"auth_service": req.Args["auth_service"],
	}
	hb, _ := json.Marshal(httpReq)
	packed := hostHTTPRequest(uint32(uintptr(unsafe.Pointer(&hb[0]))), uint32(len(hb)))
	if packed == 0 {
		return packResp(map[string]any{"ok": false, "error": "host http call returned null"})
	}
	hPtr := uint32(packed >> 32)
	hLen := uint32(packed & 0xffffffff)
	hBytes := readGuest(hPtr, hLen)
	dealloc(hPtr, hLen)

	var hResp map[string]any
	_ = json.Unmarshal(hBytes, &hResp)
	if ok, _ := hResp["ok"].(bool); !ok {
		errMsg, _ := hResp["error"].(string)
		return packResp(map[string]any{"ok": false, "error": errMsg})
	}
	return packResp(map[string]any{
		"ok": true,
		"data": map[string]any{
			"action": req.Action,
			"status": hResp["status"],
			"body":   hResp["body"],
		},
	})
}

func main() {}
`
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(wasmGoMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(wasmMainGo), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgDir := t.TempDir()
	wasmPath := filepath.Join(pkgDir, "gh.wasm")
	buildCmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", wasmPath, ".")
	buildCmd.Dir = srcDir
	buildCmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("compile wasip1 guest: %v\n%s", err, string(out))
	}

	creds, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "test-master-key", extLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer creds.Close()
	ctx := session.WithPersonalIdentity(context.Background(), "u1")
	if err := creds.Put(ctx, "u1", "github", "api_key", "ghp_wasm_secret_77", "", ""); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	parsedURL, _ := url.Parse(srv.URL)

	manifestYAML := `
name: ghwasm
version: "1.0.0"
runtime: wasm
entry: ./gh.wasm
timeout: 5s
memory_mb: 32
capabilities:
  http:
    allowed_hosts: ["` + parsedURL.Hostname() + `"]
    credentials: ["github"]
actions:
  repo:
    description: "Fetch repo via wasm"
    expose_as_tool: true
`
	if err := os.WriteFile(filepath.Join(pkgDir, "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	httpClient := hosthttp.New(extLogger(), creds, netguard.Policy{AllowPrivate: true})
	reg := NewRegistry(HostDeps{HTTP: httpClient, Log: extLogger()})
	defer reg.Close(context.Background())

	if err := reg.RegisterDir(pkgDir); err != nil {
		t.Fatalf("RegisterDir wasm: %v", err)
	}

	res, err := reg.Call(ctx, "ghwasm", "repo", map[string]any{"url": srv.URL, "auth_service": "github"})
	if err != nil {
		t.Fatalf("Call wasm repo: %v", err)
	}
	m := res.(map[string]any)
	if m["body"] != "Bearer ghp_wasm_secret_77" || m["status"] != float64(200) {
		t.Fatalf("unexpected wasm response: %+v", m)
	}

	// Host allowlist enforcement through Wasm hostcall.
	if _, err := reg.Call(ctx, "ghwasm", "repo", map[string]any{"url": "http://localhost:" + parsedURL.Port(), "auth_service": "github"}); err == nil || !strings.Contains(err.Error(), "allowed_hosts") {
		t.Fatalf("expected allowed_hosts refusal from wasm hostcall, got %v", err)
	}

	// Concurrent anonymous wazero module invocations.
	calls := make([]BatchCall, 6)
	for i := range calls {
		calls[i] = BatchCall{API: "ghwasm", Action: "repo", Args: map[string]any{"url": srv.URL, "auth_service": "github"}}
	}
	batch := reg.CallBatch(ctx, calls)
	for i, br := range batch {
		if !br.OK {
			t.Fatalf("concurrent wasm call %d failed: %s", i, br.Error)
		}
		if br.Data.(map[string]any)["body"] != "Bearer ghp_wasm_secret_77" {
			t.Fatalf("concurrent wasm call %d unexpected body: %+v", i, br.Data)
		}
	}

	// Tool exposure check.
	toolReg := tools.NewRegistry()
	reg.RegisterTools(toolReg)
	exposed := toolReg.Expose(nil, config.ToolsPolicy{Default: "all"}, false).RequireCapabilities(map[string]bool{"apis": true})
	if _, ok := exposed.ByName["api:ghwasm.repo"]; !ok {
		t.Fatalf("expected api:ghwasm.repo exposed on tools.Registry")
	}
}

func TestExternalAPIHTTPBase64BinaryResponse(t *testing.T) {
	rawPayload := []byte{0xff, 0xfe, 0x00, 'P', 'K', 0x03, 0x04, 0x80, 0x81}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(rawPayload)
	}))
	defer srv.Close()

	parsedURL, _ := url.Parse(srv.URL)
	pkgDir := t.TempDir()
	manifestYAML := `
name: binapi
runtime: script
entry: ./index.ts
capabilities:
  http:
    allowed_hosts: ["` + parsedURL.Hostname() + `"]
actions:
  fetch_b64:
    description: "Fetch raw bytes as base64"
`
	indexTS := `
export function fetch_b64(args: { url: string }, host: any): unknown {
  const resp = host.http.request({
    url: args.url,
    response_encoding: "base64",
  });
  return { status: resp.status, body: resp.body, body_base64: resp.body_base64 };
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "index.ts"), []byte(indexTS), 0o644); err != nil {
		t.Fatal(err)
	}

	httpClient := hosthttp.New(extLogger(), nil, netguard.Policy{AllowPrivate: true})
	reg := NewRegistry(HostDeps{HTTP: httpClient, Log: extLogger()})
	defer reg.Close(context.Background())

	if err := reg.RegisterDir(pkgDir); err != nil {
		t.Fatalf("RegisterDir binapi: %v", err)
	}
	res, err := reg.Call(context.Background(), "binapi", "fetch_b64", map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Call fetch_b64: %v", err)
	}
	m := res.(map[string]any)
	if m["status"] != float64(200) || m["body"] != "" {
		t.Fatalf("unexpected response fields: %+v", m)
	}
	wantB64 := base64.StdEncoding.EncodeToString(rawPayload)
	if gotB64, _ := m["body_base64"].(string); gotB64 != wantB64 {
		t.Fatalf("body_base64 = %q, want %q", gotB64, wantB64)
	}
}
