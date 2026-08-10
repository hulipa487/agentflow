package budget

import (
	"testing"
	"time"
)

func TestReserveCommitRelease(t *testing.T) {
	p := NewPool(1000)
	l, err := p.Reserve(500)
	if err != nil {
		t.Fatal(err)
	}
	if p.Remaining() != 500 {
		t.Fatalf("remaining=%d, want 500", p.Remaining())
	}
	if err := p.Commit(l, 400); err != nil {
		t.Fatal(err)
	}
	if p.Used() != 400 {
		t.Fatalf("used=%d, want 400", p.Used())
	}
	if p.Remaining() != 600 {
		t.Fatalf("remaining=%d, want 600", p.Remaining())
	}
}

// windowedPool returns a windowed pool whose clock the test controls.
func windowedPool(limit int64, window time.Duration) (*Pool, *time.Time) {
	p := NewPool(limit)
	p.SetWindow(window)
	now := time.Now()
	p.now = func() time.Time { return *(&now) }
	return p, &now
}

func TestWindowFreesBudgetAsCommitsAge(t *testing.T) {
	p, now := windowedPool(1000, time.Hour)
	l, _ := p.Reserve(600)
	if err := p.Commit(l, 600); err != nil {
		t.Fatal(err)
	}
	if p.Remaining() != 400 {
		t.Fatalf("remaining=%d, want 400", p.Remaining())
	}
	// Advance past the window: the old commit ages out and budget frees up.
	*now = now.Add(2 * time.Hour)
	if p.Remaining() != 1000 {
		t.Fatalf("remaining after window=%d, want 1000", p.Remaining())
	}
	if p.Used() != 0 {
		t.Fatalf("used after window=%d, want 0", p.Used())
	}
}

func TestWindowReserveRespectsTrailingSum(t *testing.T) {
	p, now := windowedPool(1000, time.Hour)
	l, _ := p.Reserve(700)
	p.Commit(l, 700)
	// Within the window, only 300 remain.
	if _, err := p.Reserve(400); err == nil {
		t.Fatal("reserve beyond windowed budget should fail")
	}
	// After the window, the full budget is available again.
	*now = now.Add(2 * time.Hour)
	if _, err := p.Reserve(1000); err != nil {
		t.Fatalf("reserve after window should succeed: %v", err)
	}
}

func TestWindowResetDailyIsNoop(t *testing.T) {
	p, _ := windowedPool(1000, time.Hour)
	l, _ := p.Reserve(500)
	p.Commit(l, 500)
	p.ResetDaily()
	if p.Used() != 500 {
		t.Fatalf("ResetDaily must not clear windowed usage; used=%d", p.Used())
	}
}

func TestWindowCarveInheritsWindow(t *testing.T) {
	p, _ := windowedPool(1000, time.Hour)
	child, err := p.Carve(400)
	if err != nil {
		t.Fatal(err)
	}
	if child.window != time.Hour {
		t.Fatalf("carved child should inherit window; got %v", child.window)
	}
}

func TestReserveExhausted(t *testing.T) {
	p := NewPool(100)
	if _, err := p.Reserve(50); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Reserve(60); err != ErrExhausted {
		t.Fatalf("expected ErrExhausted, got %v", err)
	}
}

func TestChildCarveAndExhaustion(t *testing.T) {
	parent := NewPool(1000)
	child, err := parent.Carve(300)
	if err != nil {
		t.Fatal(err)
	}
	if child.Remaining() != 300 {
		t.Fatalf("child remaining=%d, want 300", child.Remaining())
	}
	if parent.Remaining() != 700 {
		t.Fatalf("parent remaining=%d, want 700", parent.Remaining())
	}
	// Child cannot exceed its own limit.
	if _, err := child.Reserve(400); err != ErrExhausted {
		t.Fatalf("expected child exhaustion, got %v", err)
	}
	// Reserve within child limit succeeds and is tracked in parent too.
	l, err := child.Reserve(200)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Commit(l, 200); err != nil {
		t.Fatal(err)
	}
	if parent.Used() != 200 {
		t.Fatalf("parent used=%d, want 200", parent.Used())
	}
	if child.Used() != 200 {
		t.Fatalf("child used=%d, want 200", child.Used())
	}
}

func TestChildCannotExceedParent(t *testing.T) {
	parent := NewPool(100)
	if _, err := parent.Carve(200); err == nil {
		t.Fatal("expected carve to fail when parent has insufficient budget")
	}
}

func TestReleaseReturnsBudget(t *testing.T) {
	p := NewPool(100)
	l, _ := p.Reserve(60)
	if p.Remaining() != 40 {
		t.Fatalf("remaining=%d, want 40", p.Remaining())
	}
	p.Release(l)
	if p.Remaining() != 100 {
		t.Fatalf("remaining=%d, want 100 after release", p.Remaining())
	}
}

func TestResetDaily(t *testing.T) {
	p := NewPool(100)
	l, _ := p.Reserve(50)
	_ = p.Commit(l, 50)
	if p.Used() != 50 {
		t.Fatalf("used=%d, want 50", p.Used())
	}
	p.ResetDaily()
	if p.Used() != 0 {
		t.Fatalf("used=%d, want 0 after reset", p.Used())
	}
}

func TestStartDailyReset(t *testing.T) {
	p := NewPool(100)
	l, _ := p.Reserve(50)
	_ = p.Commit(l, 50)
	stop := p.StartDailyReset()
	defer stop()
	// We can't wait for midnight in a unit test; just verify the goroutine
	// starts and stop works without panicking.
	time.Sleep(10 * time.Millisecond)
	stop()
}
