// Package script provides a pure-Go TypeScript/JavaScript compilation and
// sandboxed execution layer built on github.com/dop251/goja.
package script

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

const (
	compileCacheMaxPrograms = 4096
	defaultMaxCallStack     = 256
)

type compiledProgram struct {
	prog *goja.Program
	err  string
}

var compilation = struct {
	sync.RWMutex
	programs map[[32]byte]compiledProgram
}{
	programs: map[[32]byte]compiledProgram{},
}

func cacheKey(name string, isTS bool, src string) [32]byte {
	h := sha256.New()
	h.Write([]byte(name))
	if isTS {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write([]byte(src))
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func storeCompiled(key [32]byte, entry compiledProgram) {
	compilation.Lock()
	if len(compilation.programs) >= compileCacheMaxPrograms {
		compilation.programs = map[[32]byte]compiledProgram{}
	}
	compilation.programs[key] = entry
	compilation.Unlock()
}

// Compile strips TypeScript (when isTS is true) and compiles src into an
// immutable, lock-free shared *goja.Program keyed by (name, isTS, SHA-256).
func Compile(name, src string, isTS bool) (*goja.Program, error) {
	key := cacheKey(name, isTS, src)
	compilation.RLock()
	entry, ok := compilation.programs[key]
	compilation.RUnlock()
	if ok {
		if entry.err != "" {
			return nil, errors.New(entry.err)
		}
		return entry.prog, nil
	}

	jsSrc := src
	if isTS {
		jsSrc = StripTS(src)
	}
	prog, err := goja.Compile(name, jsSrc, false)
	if err != nil {
		msg := formatScriptError(name, err)
		storeCompiled(key, compiledProgram{err: msg})
		return nil, errors.New(msg)
	}
	storeCompiled(key, compiledProgram{prog: prog})
	return prog, nil
}

func formatScriptError(name string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if name != "" && !strings.HasPrefix(msg, name) {
		return fmt.Sprintf("%s: %s", name, msg)
	}
	return msg
}

// RuntimeOptions bounds a Goja runtime instance.
type RuntimeOptions struct {
	MaxCallStack int
}

// NewRuntime builds a fresh, isolated Goja runtime with struct field JSON
// tag mapping enabled and a bounded call stack.
func NewRuntime(opts RuntimeOptions) *goja.Runtime {
	rt := goja.New()
	rt.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	maxStack := opts.MaxCallStack
	if maxStack <= 0 {
		maxStack = defaultMaxCallStack
	}
	rt.SetMaxCallStackSize(maxStack)
	return rt
}

// RunWithTimeout runs fn against rt, interrupting execution if ctx is
// cancelled or timeout elapses. It always cleans up any timer/goroutine before
// returning.
func RunWithTimeout(ctx context.Context, rt *goja.Runtime, timeout time.Duration, fn func() (goja.Value, error)) (goja.Value, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rt.ClearInterrupt()

	callCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}

	var (
		done chan struct{}
		wg   sync.WaitGroup
	)
	if callCtx.Done() != nil {
		done = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-callCtx.Done():
				rt.Interrupt(callCtx.Err())
			case <-done:
			}
		}()
	}

	val, err := fn()

	if done != nil {
		close(done)
		wg.Wait()
	}
	if cancel != nil {
		cancel()
	}
	rt.ClearInterrupt()

	if err != nil {
		var ierr *goja.InterruptedError
		if errors.As(err, &ierr) {
			if callCtx.Err() != nil {
				return nil, callCtx.Err()
			}
		}
		return nil, err
	}
	return val, nil
}
