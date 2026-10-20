package main

import (
	"strings"
	"testing"
)

func TestRedactConfig(t *testing.T) {
	in := []byte("api_key: sk-secret\nname: visible\ntoken: abc\n")
	out := string(redactConfig(in))
	if strings.Contains(out, "sk-secret") || strings.Contains(out, "abc") {
		t.Fatalf("secret survived redaction: %s", out)
	}
	if !strings.Contains(out, "visible") {
		t.Fatalf("non-secret was redacted: %s", out)
	}
}
