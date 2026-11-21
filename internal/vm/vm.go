// Package vm wraps the pure-Go gopher-lua VM behind a minimal Go API.
//
// One State owns one Lua main state and one sandboxed loop thread; a State
// must be used from a single goroutine at a time (session actors guarantee
// this).
package vm

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// Status of a Start/Resume drive step.
type Status int

const (
	Finished Status = iota // loop returned (session over)
	Yielded                // loop made an op request (see message)
	Failed                 // Lua error (see message)
)

var (
	closedChan           = make(chan struct{})
	errInstructionBudget = errors.New("instruction budget exceeded")
)

func init() {
	close(closedChan)
}

// budgetContext implements context.Context to enforce a deterministic
// per-resume bytecode instruction limit inside gopher-lua's
// mainLoopWithContext without spawning any goroutines or timers.
type budgetContext struct {
	max       int64
	remaining int64
	tripped   bool
}

func (b *budgetContext) arm() {
	b.remaining = b.max
	b.tripped = false
}

func (b *budgetContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (b *budgetContext) Value(any) any               { return nil }

func (b *budgetContext) Done() <-chan struct{} {
	if b.max <= 0 {
		return nil
	}
	if b.tripped {
		return closedChan
	}
	b.remaining--
	if b.remaining <= 0 {
		b.tripped = true
		return closedChan
	}
	return nil
}

func (b *budgetContext) Err() error {
	if b.tripped {
		return errInstructionBudget
	}
	return nil
}

// State is a sandboxed Lua state + loop coroutine. Not safe for concurrent use.
type State struct {
	L           *lua.LState
	T           *lua.LState
	env         *lua.LTable
	budget      *budgetContext
	instrBudget int64
	memCap      int64
	fn          string
	phase       int // 0 = running plugin chunk, 1 = running loop fn
	sealed      map[*lua.LTable]bool
	isSealed    bool
}

// New creates a sandboxed state. instrBudget limits bytecode instructions per
// resume (0 = unlimited).
func New(instrBudget int64) *State {
	L := lua.NewState(lua.Options{
		SkipOpenLibs:        true,
		IncludeGoStackTrace: false,
		MinimizeStackMemory: true,
		RegistrySize:        1024,
		RegistryMaxSize:     65535,
	})
	s := &State{
		L:           L,
		instrBudget: instrBudget,
		sealed:      map[*lua.LTable]bool{},
	}
	if instrBudget > 0 {
		s.budget = &budgetContext{max: instrBudget, remaining: instrBudget}
	}
	s.openLibs()
	return s
}

// SetMemoryCap configures a soft memory limit in bytes for this state
// (0 = uncapped, the default). Under pure-Go gopher-lua, Go's garbage
// collector manages the heap, so this cap enforces guards on large string and
// table allocations (such as string.rep and table.concat) rather than a hard
// per-byte C allocator cap. Exceeding a guard raises an "out of memory" Lua
// error.
func (s *State) SetMemoryCap(capBytes int64) {
	if s == nil {
		return
	}
	s.memCap = capBytes
}

// Close destroys the state and releases its resources.
func (s *State) Close() {
	if s.T != nil {
		s.T.Close()
		s.T = nil
	}
	if s.L != nil {
		s.L.Close()
		s.L = nil
	}
}

// Chunk compilation is cached process-wide keyed by (name, SHA-256(source)) so
// repeated session spawns reuse immutable compiled FunctionProtos while hot
// reloads compile changed sources anew and preserve exact chunk names in stack
// traces and compile error messages.
const compileCacheMaxChunks = 4096

type compiledChunk struct {
	proto *lua.FunctionProto
	err   string
}

var compilation = struct {
	sync.RWMutex
	chunks map[[32]byte]compiledChunk
}{chunks: map[[32]byte]compiledChunk{}}

func cacheKey(name, code string) [32]byte {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(code))
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func formatCompileError(name string, err error) string {
	var pe *parse.Error
	if errors.As(err, &pe) {
		if pe.Pos.Line == parse.EOF {
			return fmt.Sprintf("%s:EOF: %s", name, pe.Message)
		}
		if pe.Token != "" {
			return fmt.Sprintf("%s:%d:%d: %s near %q", name, pe.Pos.Line, pe.Pos.Column, pe.Message, pe.Token)
		}
		return fmt.Sprintf("%s:%d:%d: %s", name, pe.Pos.Line, pe.Pos.Column, pe.Message)
	}
	msg := strings.TrimSpace(err.Error())
	if strings.HasPrefix(msg, name+":") {
		return msg
	}
	return fmt.Sprintf("%s: %s", name, msg)
}

func compileChunk(name, code string) (*lua.FunctionProto, error) {
	key := cacheKey(name, code)
	compilation.RLock()
	entry, ok := compilation.chunks[key]
	compilation.RUnlock()
	if ok {
		if entry.err != "" {
			return nil, errors.New(entry.err)
		}
		return entry.proto, nil
	}

	stmts, err := parse.Parse(strings.NewReader(code), name)
	if err != nil {
		msg := formatCompileError(name, err)
		cacheStore(key, compiledChunk{err: msg})
		return nil, errors.New(msg)
	}
	proto, err := lua.Compile(stmts, name)
	if err != nil {
		msg := formatCompileError(name, err)
		cacheStore(key, compiledChunk{err: msg})
		return nil, errors.New(msg)
	}
	cacheStore(key, compiledChunk{proto: proto})
	return proto, nil
}

func cacheStore(key [32]byte, entry compiledChunk) {
	compilation.Lock()
	if len(compilation.chunks) >= compileCacheMaxChunks {
		compilation.chunks = map[[32]byte]compiledChunk{}
	}
	compilation.chunks[key] = entry
	compilation.Unlock()
}

// Eval compiles and executes a chunk on the main state (e.g. to load the
// prelude or support chunks before Start).
func (s *State) Eval(name, code string) error {
	proto, err := compileChunk(name, code)
	if err != nil {
		return err
	}
	// Apply the instruction budget on non-base Eval chunks so runaway code
	// evaluated directly via Eval still terminates, while trusted prelude
	// loading always succeeds even under tight test budgets.
	useBudget := s.budget != nil && name != "@json" && name != "@prelude"
	if useBudget {
		s.budget.arm()
		s.L.SetContext(s.budget)
		defer s.L.RemoveContext()
	}
	fn := s.L.NewFunctionFromProto(proto)
	s.L.Push(fn)
	if err := s.L.PCall(0, 0, nil); err != nil {
		return errors.New(formatLuaError(err))
	}
	return nil
}

// sealGlobals freezes the main state's globals and reachable stdlib/prelude
// tables read-only before user code runs in Start.
func (s *State) sealGlobals() {
	if s.isSealed {
		return
	}
	s.isSealed = true

	roMT := s.L.NewTable()
	roMT.RawSetString("__newindex", s.L.NewFunction(func(L *lua.LState) int {
		L.RaiseError("attempt to modify a readonly table")
		return 0
	}))
	roMT.RawSetString("__metatable", lua.LString("The metatable is locked"))

	var seal func(tbl *lua.LTable)
	seal = func(tbl *lua.LTable) {
		if tbl == nil || s.sealed[tbl] {
			return
		}
		s.sealed[tbl] = true
		tbl.ForEach(func(_, v lua.LValue) {
			if child, ok := v.(*lua.LTable); ok {
				seal(child)
			}
		})
		if tbl.Metatable == lua.LNil {
			tbl.Metatable = roMT
		}
	}
	seal(s.L.G.Global)
}

func (s *State) newLoopThread() *lua.LState {
	th, cancel := s.L.NewThread()
	if cancel != nil {
		cancel()
	}
	th.Env = s.env
	if s.budget != nil {
		th.SetContext(s.budget)
	}
	return th
}

// Start seals globals, loads the plugin chunk onto the sandboxed loop thread,
// runs it (top-level code may yield), then resumes the named global as the
// loop.
//
// name identifies the chunk in a version refusal and compile errors.
func (s *State) Start(fn, name, code string) (Status, string) {
	if err := CheckChunkVersion(name, code); err != nil {
		return Failed, err.Error()
	}
	proto, err := compileChunk(name, code)
	if err != nil {
		return Failed, err.Error()
	}

	s.sealGlobals()

	// Create a fresh sandboxed environment inheriting sealed globals read-only.
	env := s.L.NewTable()
	envMT := s.L.NewTable()
	envMT.RawSetString("__index", s.L.G.Global)
	envMT.RawSetString("__metatable", lua.LString("The metatable is locked"))
	env.Metatable = envMT
	env.RawSetString("_G", env)
	s.env = env

	s.fn = fn
	s.phase = 0
	s.T = s.newLoopThread()

	chunkFn := s.T.NewFunctionFromProto(proto)
	return s.drive(chunkFn)
}

// Resume answers a yielded op request by passing (respJSON, ok) as return
// values to the yielded __af_op call.
func (s *State) Resume(respJSON string, ok bool) (Status, string) {
	if s.T == nil {
		return Failed, "cannot resume unstarted VM state"
	}
	return s.drive(nil, lua.LString(respJSON), lua.LBool(ok))
}

// drive runs the active coroutine; when the plugin chunk finishes (from Start
// or from a later Resume), it chains into the named loop function transparently.
func (s *State) drive(fn *lua.LFunction, args ...lua.LValue) (Status, string) {
	st, msg := s.doResume(fn, args...)
	if st == Finished && s.phase == 0 {
		s.phase = 1
		loopVal := s.L.GetField(s.env, s.fn)
		loopFn, ok := loopVal.(*lua.LFunction)
		if !ok {
			return Failed, fmt.Sprintf("global '%s' is not defined (loop plugin must define it)", s.fn)
		}
		// Phase 0's thread is dead after returning; start phase 1 on a fresh
		// thread sharing the exact same sandboxed environment.
		s.T = s.newLoopThread()
		st, msg = s.doResume(loopFn)
	}
	return st, msg
}

func (s *State) doResume(fn *lua.LFunction, args ...lua.LValue) (Status, string) {
	if s.budget != nil {
		s.budget.arm()
	}
	resState, err, values := s.L.Resume(s.T, fn, args...)
	switch resState {
	case lua.ResumeYield:
		if len(values) > 0 {
			return Yielded, valueToMsg(values[0])
		}
		return Yielded, ""
	case lua.ResumeOK:
		return Finished, ""
	default:
		return Failed, formatLuaError(err)
	}
}

func valueToMsg(v lua.LValue) string {
	switch tv := v.(type) {
	case lua.LString:
		return string(tv)
	case lua.LNumber:
		return tv.String()
	case *lua.LNilType:
		return ""
	default:
		return fmt.Sprintf("(non-string error: %s)", v.Type().String())
	}
}

func formatLuaError(err error) string {
	if err == nil {
		return ""
	}
	var aerr *lua.ApiError
	if errors.As(err, &aerr) {
		if aerr.Object != nil {
			switch tv := aerr.Object.(type) {
			case lua.LString:
				return string(tv)
			case lua.LNumber:
				return tv.String()
			default:
				return fmt.Sprintf("(non-string error: %s)", aerr.Object.Type().String())
			}
		}
	}
	return err.Error()
}

// CompileCheck validates that code parses and compiles cleanly (used by hot
// reload and CLI validation before swapping loops).
func CompileCheck(name, code string) error {
	stmts, err := parse.Parse(strings.NewReader(code), name)
	if err != nil {
		return errors.New(formatCompileError(name, err))
	}
	if _, err := lua.Compile(stmts, name); err != nil {
		return errors.New(formatCompileError(name, err))
	}
	return nil
}
