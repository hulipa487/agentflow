package caps

import (
	"context"
	"errors"
	"testing"

	"agentflow/internal/core/metrics"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

type fakeRecorder struct {
	got       []runtime.UsageRecord
	withEvent []bool
	fail      bool
}

func (f *fakeRecorder) RecordUsage(rec runtime.UsageRecord, withEvent bool) error {
	if f.fail {
		return errors.New("ledger unavailable")
	}
	f.got = append(f.got, rec)
	f.withEvent = append(f.withEvent, withEvent)
	return nil
}

func TestReplyUsageExtractsEveryTokenClass(t *testing.T) {
	u, ok := replyUsage(`{"text":"hi","usage":{"input":100,"output":20,"cached":64,"cache_write":8,"reasoning":5}}`)
	if !ok {
		t.Fatal("usage should be recognized")
	}
	if u.Input != 100 || u.Output != 20 || u.Cached != 64 || u.CacheWrite != 8 || u.Reasoning != 5 {
		t.Fatalf("token classes not extracted: %+v", u)
	}

	// No usage at all: the caller falls back to its estimate.
	if _, ok := replyUsage(`{"text":"hi"}`); ok {
		t.Error("a reply without usage must not report usage")
	}
	if _, ok := replyUsage(`not json`); ok {
		t.Error("a malformed reply must not report usage")
	}
	// Zero tokens is 'no usage', not 'zero usage'.
	if _, ok := replyUsage(`{"usage":{"input":0,"output":0}}`); ok {
		t.Error("an all-zero usage must not be treated as reported")
	}
}

// Attribution is the security-relevant part: a channel turn is charged to its
// user, and engine-fired work lands in the shared service bucket rather than on
// somebody's account.
func TestMeteringAttributesToTheContextUser(t *testing.T) {
	rec := &fakeRecorder{}
	mt := Metering{Agent: "bot", Ledger: rec, Events: true}
	u := usageCounts{Input: 10, Output: 2, Cached: 4}

	mt.record(context.Background(), session.Op{Model: "m"}, "chat", u, true)
	mt.record(session.WithUserUUID(context.Background(), "u_1"), session.Op{Model: "m"}, "chat", u, true)

	if len(rec.got) != 2 {
		t.Fatalf("expected two records, got %d", len(rec.got))
	}
	if rec.got[0].UserID != "" {
		t.Errorf("a userless context must land in the service bucket, got %q", rec.got[0].UserID)
	}
	if rec.got[1].UserID != "u_1" {
		t.Errorf("user attribution lost: %q", rec.got[1].UserID)
	}
	if rec.got[1].Agent != "bot" || rec.got[1].Model != "m" || rec.got[1].Kind != "chat" {
		t.Errorf("attribution fields wrong: %+v", rec.got[1])
	}
	if rec.got[1].Cached != 4 || !rec.got[1].OK {
		t.Errorf("token classes or status lost: %+v", rec.got[1])
	}
	if !rec.withEvent[1] {
		t.Error("the event flag must reach the ledger")
	}
}

func TestMeteringRecordsFailuresWithoutTokens(t *testing.T) {
	rec := &fakeRecorder{}
	mt := Metering{Agent: "bot", Ledger: rec}
	mt.record(context.Background(), session.Op{Model: "m"}, "rerank", usageCounts{}, false)

	if len(rec.got) != 1 {
		t.Fatalf("a failed call is still an invocation: %+v", rec.got)
	}
	if rec.got[0].OK {
		t.Error("failure status lost")
	}
	if rec.got[0].Kind != "rerank" {
		t.Errorf("kind lost: %q", rec.got[0].Kind)
	}
}

func TestMeteringNilLedgerIsANoOp(t *testing.T) {
	// No ledger configured: accounting is off, and nothing panics.
	Metering{Agent: "bot"}.record(context.Background(), session.Op{}, "chat", usageCounts{Input: 1}, true)
}

// A ledger write failure must not fail the LLM call it was measuring — but it
// must be visible, not silent.
func TestMeteringLedgerFailureIsCounted(t *testing.T) {
	c, ok := metrics.Global().Get("agentflow_usage_record_failed")
	if !ok {
		t.Fatal("agentflow_usage_record_failed is not registered")
	}
	before := c.Value()

	mt := Metering{Agent: "bot", Ledger: &fakeRecorder{fail: true}}
	mt.record(context.Background(), session.Op{}, "chat", usageCounts{Input: 1}, true)

	if got := c.Value(); got != before+1 {
		t.Fatalf("a failed ledger write must be counted: before=%d after=%d", before, got)
	}
}
