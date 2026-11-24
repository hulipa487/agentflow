package script

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

func TestCompileCacheAndRuntimeTimeout(t *testing.T) {
	src := `export function spin(): void { while (true) {} }`
	p1, err := Compile("spin.ts", src, true)
	if err != nil {
		t.Fatalf("Compile 1: %v", err)
	}
	p2, err := Compile("spin.ts", src, true)
	if err != nil {
		t.Fatalf("Compile 2: %v", err)
	}
	if p1 != p2 {
		t.Fatal("expected Compile cache hit to return identical *goja.Program pointer")
	}

	rt := NewRuntime(RuntimeOptions{MaxCallStack: 64})
	if _, err := rt.RunString(`var module = { exports: {} }; var exports = module.exports;`); err != nil {
		t.Fatal(err)
	}
	if _, err := RunWithTimeout(context.Background(), rt, time.Second, func() (goja.Value, error) {
		return rt.RunProgram(p1)
	}); err != nil {
		t.Fatalf("load program: %v", err)
	}

	_, err = RunWithTimeout(context.Background(), rt, 30*time.Millisecond, func() (goja.Value, error) {
		return rt.RunString(`exports.spin()`)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded on infinite loop, got %v", err)
	}
}

func TestStackDepthBound(t *testing.T) {
	rt := NewRuntime(RuntimeOptions{MaxCallStack: 16})
	_, err := RunWithTimeout(context.Background(), rt, time.Second, func() (goja.Value, error) {
		return rt.RunString(`function r(n) { return r(n + 1); } r(0);`)
	})
	if err == nil {
		t.Fatal("expected stack overflow error, got nil")
	}
	var stackErr *goja.StackOverflowError
	if !errors.As(err, &stackErr) && !strings.Contains(strings.ToLower(err.Error()), "stack") {
		t.Fatalf("expected stack overflow error, got %T: %v", err, err)
	}
}
