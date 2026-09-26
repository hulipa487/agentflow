// Package vm wraps the Luau VM behind a minimal Go API.
//
// It is the ONLY cgo package in agentflow (see DESIGN.md). One State owns one
// Luau state and one loop thread; a State must be used from a single
// goroutine at a time (session actors guarantee this).
package vm

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo CFLAGS: -I${SRCDIR}/../../third_party/luau/VM/include
#cgo CFLAGS: -I${SRCDIR}/../../third_party/luau/Common/include
#cgo CFLAGS: -I${SRCDIR}/../../third_party/luau/Ast/include
#cgo CFLAGS: -I${SRCDIR}/../../third_party/luau/Compiler/include
#cgo CXXFLAGS: -I${SRCDIR}
#cgo CXXFLAGS: -I${SRCDIR}/../../third_party/luau/VM/include
#cgo CXXFLAGS: -I${SRCDIR}/../../third_party/luau/Common/include
#cgo CXXFLAGS: -I${SRCDIR}/../../third_party/luau/Ast/include
#cgo CXXFLAGS: -I${SRCDIR}/../../third_party/luau/Compiler/include
#cgo LDFLAGS: -L${SRCDIR}/lib -lluau
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Status of a Start/Resume drive step.
type Status int

const (
	Finished Status = iota // loop returned (session over)
	// Yielded is never named in Go — the actor/router handle the yielded op
	// directly — but its VALUE is load-bearing: the C shim returns
	// AF_YIELDED=1 / AF_ERROR=2, so removing this member renumbers Failed
	// onto the yielded status and every op request reads as a Lua error.
	Yielded // loop made an op request (see message)
	Failed  // Lua error (see message)
)

// State is a Luau state + loop thread. Not safe for concurrent use.
type State struct {
	v *C.afvm
}

// New creates a sandboxed state. instrBudget limits interrupt hits per resume
// (0 = unlimited).
func New(instrBudget int64) *State {
	return &State{v: C.afvm_new(C.long(instrBudget))}
}

// SetMemoryCap caps the state's total allocation, in bytes (0 = uncapped, the
// default). Exceeding it fails allocations, which Luau raises as an
// out-of-memory script error — the same death as exceeding the instruction
// budget, so an oversized state dies under the same crash-restart rule. It is
// the engine's bound (E18's per-tenant VM budget is this cap times the
// per-tenant session cap), not a Lua-visible setting, so it is set from the
// host after New.
func (s *State) SetMemoryCap(capBytes int64) {
	if s.v != nil {
		C.afvm_set_memcap(s.v, C.long(capBytes))
	}
}

// Close destroys the state.
func (s *State) Close() {
	if s.v != nil {
		C.afvm_close(s.v)
		s.v = nil
	}
}

// Eval compiles and executes a chunk (e.g. to define the loop global).
func (s *State) Eval(name, code string) error {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	ccode := C.CString(code)
	defer C.free(unsafe.Pointer(ccode))

	errbuf := make([]byte, 4096)
	rc := C.afvm_eval(s.v, cname, ccode, C.size_t(len(code)),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf)))
	if rc != 0 {
		return errors.New(C.GoString((*C.char)(unsafe.Pointer(&errbuf[0]))))
	}
	return nil
}

// Start seals globals, loads the plugin chunk onto the loop thread, runs it
// (top-level code may yield), then resumes the named global as the loop.
//
// name identifies the chunk in a version refusal, and is the file path where
// there is one: it is what the operator is told to go and edit.
//
// The chunk is version-gated here as well as at builtins.Resolve, because this
// is the one point a loop source is loaded into a VM. A source that never
// passed through Resolve — an AgentDef's in-memory LoopSrc, a route source
// handed straight to the router — reaches the engine no other way, so without
// the check here the embedding path could run Lua written against a different
// prelude. A source that did pass through Resolve is checked a second time,
// which costs a scan of the leading comment block and can never disagree:
// Resolve returns only what this same check accepted.
//
// A refusal is returned as Failed carrying the gate's own message, so a caller
// that already has a Failed path — the session restart, the router rebuild —
// treats it the way it treats a Lua error. That is deliberate: nothing here can
// repair the chunk, and the alternative to failing loudly is running against an
// API the chunk was not written for.
func (s *State) Start(fn, name, code string) (Status, string) {
	if err := CheckChunkVersion(name, code); err != nil {
		return Failed, err.Error()
	}
	cfn := C.CString(fn)
	defer C.free(unsafe.Pointer(cfn))
	ccode := C.CString(code)
	defer C.free(unsafe.Pointer(ccode))
	errbuf := make([]byte, 4096)
	rc := C.afvm_start(s.v, cfn, ccode, C.size_t(len(code)),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf)))
	status := Status(rc)
	if status == Failed {
		return status, C.GoString((*C.char)(unsafe.Pointer(&errbuf[0])))
	}
	return status, s.lastMsg()
}

// Resume answers a yielded op request. ok=false raises in Lua.
func (s *State) Resume(respJSON string, ok bool) (Status, string) {
	cresp := C.CString(respJSON)
	defer C.free(unsafe.Pointer(cresp))
	cok := C.int(0)
	if ok {
		cok = 1
	}
	return Status(C.afvm_resume(s.v, cresp, C.size_t(len(respJSON)), cok)), s.lastMsg()
}

func (s *State) lastMsg() string {
	n := C.afvm_lastmsg(s.v, nil, 0)
	if n == 0 {
		return ""
	}
	buf := make([]byte, n)
	C.afvm_lastmsg(s.v, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(n))
	return string(buf)
}

// CompileCheck validates that code parses (used by hot reload before swap).
func CompileCheck(name, code string) error {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	ccode := C.CString(code)
	defer C.free(unsafe.Pointer(ccode))
	errbuf := make([]byte, 4096)
	rc := C.afvm_check(cname, ccode, C.size_t(len(code)),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf)))
	if rc != 0 {
		return errors.New(C.GoString((*C.char)(unsafe.Pointer(&errbuf[0]))))
	}
	return nil
}
