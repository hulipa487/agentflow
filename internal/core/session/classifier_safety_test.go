package session

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
)

type blockBadClassifier struct{}

func (blockBadClassifier) Classify(_ context.Context, req safety.ClassifyRequest) (safety.Verdict, error) {
	if strings.Contains(req.Text, "bad") {
		return safety.Verdict{Label: "violence", Score: 0.99, Model: "fake"}, nil
	}
	return safety.Verdict{Label: "safe", Score: 0.99, Model: "fake"}, nil
}

func classifierDispatcher() *safety.Dispatcher {
	return safety.New(&safety.Profile{
		Name: "test",
		Filters: []safety.Filter{
			safety.NewClassifierFilter(safety.ClassifierOptions{
				Name:       "moderation",
				Classifier: blockBadClassifier{},
				Ingress:    true,
				Egress:     true,
				Threshold:  0.7,
				Categories: map[string]string{"safe": "allow", "violence": "block"},
				Timeout:    time.Second,
				OnError:    "block",
				Refusal:    "blocked by policy",
				Fallback:   "fallback reply",
			}),
		},
	})
}

func classifierActor(t *testing.T, gw Gateway) *Actor {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New("main|test",
		Identity{SessionID: "main|test", Agent: "main"},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, classifierDispatcher(), map[string]OpHandler{}, pool.New(1), log)
}

// TestIngressBlockSkipsTurn: a classifier block must not hand the loop an
// empty message. The inbox op keeps waiting; the next allowed message is the
// one the loop receives.
func TestIngressBlockSkipsTurn(t *testing.T) {
	gw := &fakeGW{}
	a := classifierActor(t, gw)
	ctx := context.Background()
	got := make(chan Message, 1)
	go func() {
		var current Message
		_, ok, _ := a.dispatchInline(ctx, Op{Type: "inbox"}, &current)
		if ok {
			got <- current
		}
	}()

	a.Mailbox <- Message{ID: "blocked", Type: "user", Text: "this is bad", Channel: "chan", ReplyTo: "r"}
	waitFor(t, "refusal", time.Second, func() bool {
		for _, rec := range gw.snapshot() {
			if rec.text == "blocked by policy" {
				return true
			}
		}
		return false
	})

	a.Mailbox <- Message{ID: "allowed", Type: "user", Text: "this is fine", Channel: "chan", ReplyTo: "r"}
	select {
	case m := <-got:
		if m.ID != "allowed" || m.Text != "this is fine" {
			t.Fatalf("loop received %+v; want the allowed message", m)
		}
	case <-time.After(time.Second):
		t.Fatal("inbox did not return the allowed message")
	}
}

func TestEgressBlockSendsFallback(t *testing.T) {
	gw := &fakeGW{}
	a := classifierActor(t, gw)
	_, ok := a.egress(context.Background(), "chan", "r", "this is bad", nil, "m1")
	if ok {
		t.Fatal("egress returned success for a blocked reply")
	}
	sends := gw.snapshot()
	if len(sends) != 1 || sends[0].text != "fallback reply" {
		t.Fatalf("sends = %+v; want one fallback", sends)
	}
}
