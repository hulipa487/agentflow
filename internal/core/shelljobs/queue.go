package shelljobs

import (
	"context"
	"sync"
)

// keyed is a set of counting semaphores, one per key, created on demand and
// dropped when nobody holds or waits for them. It is what keeps one project to
// one job at a time and one user to a bounded number, without a table of every
// project and user that ever ran a command.
type keyed struct {
	mu   sync.Mutex
	sems map[string]*keyedSem
}

type keyedSem struct {
	slots chan struct{}
	refs  int // holders and waiters
}

func newKeyed() *keyed { return &keyed{sems: map[string]*keyedSem{}} }

// acquire takes one of capacity slots for key, waiting until ctx ends. The
// returned release must be called exactly once.
func (k *keyed) acquire(ctx context.Context, key string, capacity int) (release func(), err error) {
	k.mu.Lock()
	s := k.sems[key]
	if s == nil {
		s = &keyedSem{slots: make(chan struct{}, capacity)}
		k.sems[key] = s
	}
	s.refs++
	k.mu.Unlock()

	drop := func() {
		k.mu.Lock()
		if s.refs--; s.refs == 0 {
			delete(k.sems, key)
		}
		k.mu.Unlock()
	}
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// waiting reports how many holders and waiters a key has.
func (k *keyed) waiting(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s := k.sems[key]; s != nil {
		return s.refs
	}
	return 0
}
