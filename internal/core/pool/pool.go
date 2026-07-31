// Package pool is a small fixed-size worker pool. Blocking ops (llm.chat,
// channel sends) run here so the session actor goroutine stays responsive to
// reload and shutdown signals while an op is in flight.
package pool

// Pool dispatches jobs to a fixed set of workers.
type Pool struct {
	jobs chan func()
}

// New starts n workers.
func New(n int) *Pool {
	if n < 1 {
		n = 1
	}
	p := &Pool{jobs: make(chan func(), 256)}
	for i := 0; i < n; i++ {
		go func() {
			for f := range p.jobs {
				f()
			}
		}()
	}
	return p
}

// Submit queues f. It blocks only if the backlog is full, which is the
// intended backpressure point.
func (p *Pool) Submit(f func()) { p.jobs <- f }

// Call runs f on a worker and returns a channel that receives f's result.
// Callers typically select on the result channel against reload/shutdown
// signals; an abandoned result is discarded (buffered, never blocks).
func Call[T any](p *Pool, f func() T) <-chan T {
	ch := make(chan T, 1)
	p.Submit(func() { ch <- f() })
	return ch
}
