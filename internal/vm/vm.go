// Package vm wraps the pure-Go Goja TypeScript/JavaScript engine behind a
// minimal Go Start/Resume/Close API.
//
// One State owns one Goja runtime and (once Start is called) one worker
// goroutine that executes the loop and yields across channels on host ops.
// A State must be driven from a single caller goroutine at a time (session
// actors and the router guarantee this).
package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"agentflow/internal/script"
)

// Status of a Start/Resume drive step.
type Status int

const (
	Finished Status = iota // loop returned (session over)
	Yielded                // loop made an op request (see message)
	Failed                 // runtime or compile error (see message)
)

var (
	errInstructionBudget = errors.New("instruction budget exceeded")
	errClosed            = errors.New("vm state closed")
)

type stepResult struct {
	status Status
	msg    string
}

type resumeMsg struct {
	respJSON string
	ok       bool
}

// State is a sandboxed Goja runtime + channel-bridged loop worker. Not safe
// for concurrent Start/Resume calls from multiple caller goroutines.
type State struct {
	rt          *goja.Runtime
	instrBudget int64
	memCap      atomic.Int64
	isSealed    bool

	watchdogMu    sync.Mutex
	watchdogTimer *time.Timer
	watchdogGen   atomic.Uint64
	budgetTripped atomic.Bool

	started   bool
	inWorker  atomic.Bool
	finished  bool
	closed    atomic.Bool
	closeOnce sync.Once
	stepCh    chan stepResult
	resumeCh  chan resumeMsg
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// budgetDuration maps a legacy instruction budget count onto a per-resume CPU
// watchdog duration so runaway pure-JS loops still interrupt promptly.
func budgetDuration(instrBudget int64) time.Duration {
	if instrBudget <= 0 {
		return 0
	}
	d := time.Duration(instrBudget) * 5 * time.Microsecond
	if d < 25*time.Millisecond {
		return 25 * time.Millisecond
	}
	if d > 5*time.Second {
		return 5 * time.Second
	}
	return d
}

// New creates a sandboxed Goja state. instrBudget configures the per-resume CPU
// watchdog limit (0 = unlimited).
func New(instrBudget int64) *State {
	rt := script.NewRuntime(script.RuntimeOptions{})
	s := &State{
		rt:          rt,
		instrBudget: instrBudget,
	}

	_ = rt.Set("__af_now", func(call goja.FunctionCall) goja.Value {
		return rt.ToValue(time.Now().Unix())
	})
	_ = rt.Set("__af_mem_cap", func(call goja.FunctionCall) goja.Value {
		return rt.ToValue(s.memCap.Load())
	})
	_ = rt.Set("exports", rt.NewObject())
	_ = rt.Set("__af_op", func(call goja.FunctionCall) goja.Value {
		return s.hostOp(call)
	})
	return s
}

func (s *State) armWatchdog() {
	d := budgetDuration(s.instrBudget)
	if d <= 0 {
		return
	}
	s.watchdogMu.Lock()
	defer s.watchdogMu.Unlock()
	if s.closed.Load() {
		return
	}
	if s.watchdogTimer != nil {
		s.watchdogTimer.Stop()
	}
	s.budgetTripped.Store(false)
	gen := s.watchdogGen.Add(1)
	s.watchdogTimer = time.AfterFunc(d, func() {
		s.watchdogMu.Lock()
		active := s.watchdogGen.Load() == gen && !s.closed.Load()
		s.watchdogMu.Unlock()
		if active {
			s.budgetTripped.Store(true)
			s.rt.Interrupt(errInstructionBudget)
		}
	})
}

func (s *State) disarmWatchdog() {
	s.watchdogMu.Lock()
	s.watchdogGen.Add(1)
	if s.watchdogTimer != nil {
		s.watchdogTimer.Stop()
		s.watchdogTimer = nil
	}
	s.watchdogMu.Unlock()
}

func (s *State) throwError(msg string) {
	if ctor, ok := goja.AssertConstructor(s.rt.Get("Error")); ok {
		if obj, err := ctor(nil, s.rt.ToValue(msg)); err == nil {
			panic(obj)
		}
	}
	panic(s.rt.ToValue(msg))
}

func (s *State) hostOp(call goja.FunctionCall) goja.Value {
	if !s.inWorker.Load() {
		s.throwError("cannot yield outside loop execution")
	}
	reqJSON := ""
	if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
		reqJSON = call.Arguments[0].String()
	}

	s.disarmWatchdog()
	select {
	case s.stepCh <- stepResult{status: Yielded, msg: reqJSON}:
	case <-s.stopCh:
		panic(errClosed)
	}

	select {
	case r := <-s.resumeCh:
		if s.closed.Load() {
			panic(errClosed)
		}
		// Clear any stale interrupt that might have raced right as we disarmed.
		s.rt.ClearInterrupt()
		s.armWatchdog()
		return s.rt.ToValue([]any{r.respJSON, r.ok})
	case <-s.stopCh:
		panic(errClosed)
	}
}

// SetMemoryCap configures a soft memory limit in bytes for large prelude string
// and array joins (0 = uncapped). Exceeding a guard throws RangeError("out of memory").
func (s *State) SetMemoryCap(capBytes int64) {
	if s == nil {
		return
	}
	s.memCap.Store(capBytes)
}

// Close destroys the state, interrupts any running loop worker, and waits for
// the worker goroutine (if started) to exit cleanly.
func (s *State) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.disarmWatchdog()
		if s.rt != nil {
			s.rt.Interrupt(errClosed)
		}
		if s.stopCh != nil {
			close(s.stopCh)
		}
		if s.started && s.doneCh != nil {
			<-s.doneCh
		}
	})
}

// Eval compiles (stripping TypeScript annotations when present) and executes
// a chunk on the runtime before Start (e.g. to load the prelude, support
// chunks, or route helpers).
func (s *State) Eval(name, code string) error {
	if s.started {
		return errors.New("cannot Eval after Start")
	}
	isTS := name != "@prelude" && name != "@json"
	prog, err := script.Compile(name, code, isTS)
	if err != nil {
		return err
	}
	useBudget := s.instrBudget > 0 && isTS
	if useBudget {
		s.armWatchdog()
		defer func() {
			s.disarmWatchdog()
			s.rt.ClearInterrupt()
		}()
	}
	if _, err := s.rt.RunProgram(prog); err != nil {
		return errors.New(s.formatJSError(err))
	}
	return nil
}

const sealGlobalsJS = `(function(g) {
  var names = [
    "af", "session", "log", "time", "os", "agent", "scheduler", "store",
    "user", "memory", "tool", "tools", "shell", "http", "runtime",
    "credential", "mail", "llm", "openai", "files", "json", "router"
  ];
  for (var i = 0; i < names.length; i++) {
    var obj = g[names[i]];
    if (obj && typeof obj === "object") {
      var keys = Object.keys(obj);
      for (var j = 0; j < keys.length; j++) {
        var child = obj[keys[j]];
        if (child && typeof child === "object") {
          Object.freeze(child);
        }
      }
      Object.freeze(obj);
    }
  }
})(globalThis);`

var sealGlobalsProg = goja.MustCompile("@seal", sealGlobalsJS, false)

// sealGlobals freezes the prelude and service namespace objects read-only
// before user code runs in Start. (`api` is already a frozen Proxy.)
func (s *State) sealGlobals() {
	if s.isSealed {
		return
	}
	s.isSealed = true
	_, _ = s.rt.RunProgram(sealGlobalsProg)
}

// Start seals prelude namespaces, compiles and executes the chunk top-level
// (which may yield host ops), and then invokes the named global/exported loop
// function. name identifies the chunk in version refusals and compile errors.
func (s *State) Start(fn, name, code string) (Status, string) {
	if err := CheckChunkVersion(name, code); err != nil {
		return Failed, err.Error()
	}
	prog, err := script.Compile(name, code, true)
	if err != nil {
		return Failed, err.Error()
	}

	s.sealGlobals()

	s.stepCh = make(chan stepResult, 1)
	s.resumeCh = make(chan resumeMsg)
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.started = true
	s.finished = false

	go s.runWorker(fn, prog)

	res := <-s.stepCh
	if res.status != Yielded {
		s.finished = true
	}
	return res.status, res.msg
}

func (s *State) runWorker(fn string, prog *goja.Program) {
	s.inWorker.Store(true)
	defer func() {
		s.inWorker.Store(false)
		s.disarmWatchdog()
		if r := recover(); r != nil {
			if rErr, ok := r.(error); ok && errors.Is(rErr, errClosed) {
				close(s.doneCh)
				return
			}
			close(s.doneCh)
			panic(r)
		}
		close(s.doneCh)
	}()

	s.armWatchdog()
	if _, err := s.rt.RunProgram(prog); err != nil {
		s.disarmWatchdog()
		if s.closed.Load() {
			return
		}
		s.sendResult(stepResult{status: Failed, msg: s.formatJSError(err)})
		return
	}

	loopFn, ok := s.lookupLoopFn(fn)
	if !ok {
		s.disarmWatchdog()
		s.sendResult(stepResult{
			status: Failed,
			msg:    fmt.Sprintf("global '%s' is not defined (loop plugin must define it)", fn),
		})
		return
	}

	if _, err := loopFn(goja.Undefined()); err != nil {
		s.disarmWatchdog()
		if s.closed.Load() {
			return
		}
		s.sendResult(stepResult{status: Failed, msg: s.formatJSError(err)})
		return
	}

	s.disarmWatchdog()
	s.sendResult(stepResult{status: Finished})
}

func (s *State) lookupLoopFn(fn string) (goja.Callable, bool) {
	if call, ok := goja.AssertFunction(s.rt.Get(fn)); ok {
		return call, true
	}
	if exp := s.rt.Get("exports"); exp != nil && !goja.IsUndefined(exp) && !goja.IsNull(exp) {
		obj := exp.ToObject(s.rt)
		if call, ok := goja.AssertFunction(obj.Get(fn)); ok {
			return call, true
		}
	}
	return nil, false
}

func (s *State) sendResult(res stepResult) {
	select {
	case s.stepCh <- res:
	case <-s.stopCh:
	}
}

// Resume answers a yielded op request by passing [respJSON, ok] back to the
// suspended __af_op call inside the worker goroutine.
func (s *State) Resume(respJSON string, ok bool) (Status, string) {
	if !s.started || s.finished || s.closed.Load() {
		return Failed, "cannot resume unstarted VM state"
	}
	select {
	case s.resumeCh <- resumeMsg{respJSON: respJSON, ok: ok}:
	case <-s.doneCh:
		return Failed, "cannot resume finished VM state"
	case <-s.stopCh:
		return Failed, "cannot resume closed VM state"
	}

	res := <-s.stepCh
	if res.status != Yielded {
		s.finished = true
	}
	return res.status, res.msg
}

func (s *State) formatJSError(err error) string {
	if err == nil {
		return ""
	}
	if s.budgetTripped.Load() || errors.Is(err, errInstructionBudget) {
		return errInstructionBudget.Error()
	}
	var ierr *goja.InterruptedError
	if errors.As(err, &ierr) {
		if errors.Is(ierr, errInstructionBudget) {
			return errInstructionBudget.Error()
		}
		return ierr.Error()
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		raw := strings.TrimSpace(ex.Error())
		if val := ex.Value(); val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
			if _, ok := val.(*goja.Object); ok && strings.HasPrefix(raw, "[object ") {
				if b, mErr := json.Marshal(val.Export()); mErr == nil {
					return fmt.Sprintf("(non-string error: object %s)", string(b))
				}
				return "(non-string error: object)"
			}
		}
		if raw != "" {
			return raw
		}
	}
	return err.Error()
}

// CompileCheck validates that code strips TypeScript cleanly and parses under
// Goja (used by hot reload, CLI validation, and contract tests before swapping loops).
func CompileCheck(name, code string) error {
	_, err := script.Compile(name, code, true)
	return err
}
