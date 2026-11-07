package fleet

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T, target, owner string) *Registry {
	t.Helper()
	r, err := Open(Config{
		Target:      target,
		Owner:       owner,
		Region:      "eu-west",
		Zone:        "eu-west-1a",
		Version:     "test",
		Topology:    "single",
		ConfigEpoch: "epoch-" + owner,
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", owner, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestSingleRefusesASecondLiveInstance(t *testing.T) {
	target := filepath.Join(t.TempDir(), "agentflow.db")
	ctx := context.Background()
	a := openTest(t, target, "instance-a")
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckSingle(ctx); err != nil {
		t.Fatalf("first instance refused: %v", err)
	}

	b := openTest(t, target, "instance-b")
	err := b.CheckSingle(ctx)
	if err == nil {
		t.Fatal("second single instance was allowed")
	}
	for _, want := range []string{"instance-a", "single topology", "-force-single"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err, want)
		}
	}

	// B's own row must not make B refuse itself on a retry.
	if err := b.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckSingle(ctx); err == nil {
		t.Fatal("refresh of own row should leave A as the only peer")
	}
}

func TestCloseRemovesTheRow(t *testing.T) {
	target := filepath.Join(t.TempDir(), "agentflow.db")
	ctx := context.Background()
	a := openTest(t, target, "instance-a")
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openTest(t, target, "instance-b")
	if err := b.CheckSingle(ctx); err != nil {
		t.Fatalf("closed instance still blocked a new one: %v", err)
	}
}

func TestExpiredRowIsNotLive(t *testing.T) {
	target := filepath.Join(t.TempDir(), "agentflow.db")
	ctx := context.Background()
	a := openTest(t, target, "instance-a")
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	// Age the row past the live window without deleting it.
	old := time.Now().Add(-LiveWindow - time.Minute).Unix()
	if _, err := a.st.Exec(ctx, `UPDATE instances SET last_seen = ? WHERE owner = ?`, old, "instance-a"); err != nil {
		t.Fatal(err)
	}
	b := openTest(t, target, "instance-b")
	if err := b.CheckSingle(ctx); err != nil {
		t.Fatalf("expired peer blocked a new instance: %v", err)
	}
	list, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("List returned %d instances, want 0 after expiry", len(list))
	}
	n, err := b.Sweep(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Sweep removed %d rows, want 1", n)
	}
}

func TestHeartbeatLoop(t *testing.T) {
	target := filepath.Join(t.TempDir(), "agentflow.db")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := openTest(t, target, "instance-a")
	r.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		list, err := r.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].Owner == "instance-a" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not register the instance")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
