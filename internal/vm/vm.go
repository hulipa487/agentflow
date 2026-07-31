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
	Yielded                // loop made an op request (see message)
	Failed                 // Lua error (see message)
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
func (s *State) Start(fn, code string) (Status, string) {
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
