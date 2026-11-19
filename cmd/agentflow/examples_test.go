package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/looptest"
)

// TestExampleScenariosValidate runs `agentflow validate` on every scenario
// under examples/: the config loads, every agent and spawn-profile loop
// compiles, and the gateway route checks out. Secrets are placeholders; the
// examples read them from the environment.
func TestExampleScenariosValidate(t *testing.T) {
	for k, v := range map[string]string{
		"OPENAI_API_KEY":         "sk-test",
		"QQ_APPID":               "1024",
		"QQ_APPSECRET":           "qq-test-secret",
		"TELEGRAM_BOT_TOKEN":     "123:test",
		"GITHUB_WEBHOOK_SECRET":  "gh-test",
		"COMPLETIONS_API_KEY":    "key-test",
		"CREDENTIALS_MASTER_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
	} {
		t.Setenv(k, v)
	}
	configs, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "agentflow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) < 4 {
		t.Fatalf("found %d scenario configs, want at least 4", len(configs))
	}
	for _, cfg := range configs {
		name := filepath.Base(filepath.Dir(cfg))
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := cmdValidate([]string{"-config", cfg}, &stdout, &stderr); code != 0 {
				t.Fatalf("validate exited %d:\n%s%s", code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestExampleLoopTests runs every loop test shipped with the examples: the
// building-block ones in examples/loop-tests and each scenario's tests/.
func TestExampleLoopTests(t *testing.T) {
	var files []string
	for _, pattern := range []string{
		filepath.Join("..", "..", "examples", "loop-tests", "*.yaml"),
		filepath.Join("..", "..", "examples", "*", "tests", "*.yaml"),
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
	}
	if len(files) == 0 {
		t.Fatal("no example loop tests found")
	}
	for _, path := range files {
		name, _ := filepath.Rel(filepath.Join("..", "..", "examples"), path)
		t.Run(filepath.ToSlash(strings.TrimSuffix(name, ".yaml")), func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sc, err := looptest.LoadFile(path, b)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := looptest.Run(sc, &out); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
		})
	}
}
