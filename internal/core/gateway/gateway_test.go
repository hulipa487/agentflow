package gateway

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The package had no tests, and it is the single point every reply passes
// through: a delivery that goes to the wrong channel, or a failure that is
// swallowed instead of reported, is a user who never gets an answer.

type fakeDriver struct {
	name string
	err  error

	calls   int
	gotTo   string
	gotText string
	gotAtts []media.Part
}

func (f *fakeDriver) Name() string { return f.name }

func (f *fakeDriver) Deliver(replyTo, text string, attachments []media.Part) error {
	f.calls++
	f.gotTo, f.gotText, f.gotAtts = replyTo, text, attachments
	return f.err
}

func counter(t *testing.T, name string) int64 {
	t.Helper()
	c, ok := metrics.Global().Get(name)
	if !ok {
		t.Fatalf("counter %q is not registered", name)
	}
	return c.Value()
}

func TestSendDeliversToTheRegisteredChannel(t *testing.T) {
	d := &fakeDriver{name: "telegram"}
	r := NewRegistry(discardLog())
	r.Register(d)

	before := counter(t, "agentflow_egress_total")
	atts := []media.Part{{Type: "image", MIME: "image/png", Handle: "media:ab"}}
	if err := r.Send("telegram", "chat-9", "hello", atts); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if d.calls != 1 || d.gotTo != "chat-9" || d.gotText != "hello" {
		t.Fatalf("driver saw (%d, %q, %q); want one delivery to chat-9 with the text", d.calls, d.gotTo, d.gotText)
	}
	if len(d.gotAtts) != 1 || d.gotAtts[0].Handle != "media:ab" {
		t.Fatalf("attachments did not reach the channel: %#v", d.gotAtts)
	}
	if got := counter(t, "agentflow_egress_total") - before; got != 1 {
		t.Fatalf("egress_total moved by %d; want 1", got)
	}
}

// TestSendToAnUnknownChannelIsAnError: a reply addressed to a channel that is
// not registered must fail where the session can hear it. A silent drop is a
// user waiting for an answer that was never sent.
func TestSendToAnUnknownChannelIsAnError(t *testing.T) {
	r := NewRegistry(discardLog())

	egressBefore := counter(t, "agentflow_egress_total")
	failedBefore := counter(t, "agentflow_egress_failed")
	chanBefore := counter(t, "agentflow_channel_errors")

	err := r.Send("nope", "chat-9", "hello", nil)
	if err == nil {
		t.Fatal("Send to an unknown channel must be an error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %q does not name the channel", err)
	}

	if got := counter(t, "agentflow_egress_total") - egressBefore; got != 1 {
		t.Errorf("egress_total moved by %d; an attempt was made, so it counts", got)
	}
	if got := counter(t, "agentflow_egress_failed") - failedBefore; got != 1 {
		t.Errorf("egress_failed moved by %d; want 1", got)
	}
	if got := counter(t, "agentflow_channel_errors") - chanBefore; got != 1 {
		t.Errorf("channel_errors moved by %d; want 1", got)
	}
}

// TestDeliverFailureIsReportedAndCounted — the channel's own error travels back
// to the caller, and the two failure counters move together.
func TestDeliverFailureIsReportedAndCounted(t *testing.T) {
	boom := errors.New("telegram 502")
	d := &fakeDriver{name: "telegram", err: boom}
	r := NewRegistry(discardLog())
	r.Register(d)

	failedBefore := counter(t, "agentflow_egress_failed")
	chanBefore := counter(t, "agentflow_channel_errors")

	err := r.Send("telegram", "chat-9", "hello", nil)
	if !errors.Is(err, boom) {
		t.Fatalf("Send returned %v; want the channel's own error", err)
	}
	if got := counter(t, "agentflow_egress_failed") - failedBefore; got != 1 {
		t.Errorf("egress_failed moved by %d; want 1", got)
	}
	if got := counter(t, "agentflow_channel_errors") - chanBefore; got != 1 {
		t.Errorf("channel_errors moved by %d; want 1", got)
	}
}

// TestRegisterReplacesByName: two channels sharing a name would otherwise
// shadow each other silently, and which one wins would depend on registration
// order.
func TestRegisterReplacesByName(t *testing.T) {
	first := &fakeDriver{name: "telegram"}
	second := &fakeDriver{name: "telegram"}
	r := NewRegistry(discardLog())
	r.Register(first)
	r.Register(second)

	if err := r.Send("telegram", "chat-9", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if second.calls != 1 {
		t.Fatalf("the later registration did not win: second.calls = %d", second.calls)
	}
	if first.calls != 0 {
		t.Fatalf("the replaced driver still received deliveries: %d", first.calls)
	}
}
