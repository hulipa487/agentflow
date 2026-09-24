// Package gateway is the channel registry: session replies are routed back
// to the channel the inbound message arrived on.
package gateway

import (
	"fmt"
	"log/slog"
	"sync"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
)

// Driver is a live channel's delivery half (webhook, telegram, ...).
type Driver interface {
	Name() string
	// Deliver sends text (plus optional media attachments) to a
	// channel-specific target (chat id, pending webhook request id, ...).
	// A channel that cannot deliver media returns an error, never a silent
	// drop.
	Deliver(replyTo string, text string, attachments []media.Part) error
}

// Registry maps channel names to drivers.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
	log     *slog.Logger
}

func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{
		drivers: map[string]Driver{},
		log:     log.With("module", "gateway"),
	}
}

func (r *Registry) Register(d Driver) {
	r.mu.Lock()
	r.drivers[d.Name()] = d
	r.mu.Unlock()
	r.log.Info("channel registered", "channel", d.Name())
}

// Send delivers text to the message's origin channel. A reply to an unknown
// channel is an error the session should hear about (op failure), not a
// silent drop.
//
// It is also where egress is counted: every reply the engine delivers goes
// through here, so one pair of increments covers the whole egress path, and a
// failure is counted whether it is an unknown channel or a delivery error.
func (r *Registry) Send(channel, replyTo, text string, attachments []media.Part) error {
	metrics.Inc("agentflow_egress_total")
	r.mu.RLock()
	d, ok := r.drivers[channel]
	r.mu.RUnlock()
	if !ok {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return fmt.Errorf("unknown channel %q", channel)
	}
	if err := d.Deliver(replyTo, text, attachments); err != nil {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return err
	}
	return nil
}
