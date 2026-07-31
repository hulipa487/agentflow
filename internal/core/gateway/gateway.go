// Package gateway is the channel registry: session replies are routed back
// to the channel the inbound message arrived on.
package gateway

import (
	"fmt"
	"log/slog"
	"sync"
)

// Driver is a live channel's delivery half (webhook, telegram, ...).
type Driver interface {
	Name() string
	// Deliver sends text to a channel-specific target (chat id, pending
	// webhook request id, ...).
	Deliver(replyTo string, text string) error
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
func (r *Registry) Send(channel, replyTo, text string) error {
	r.mu.RLock()
	d, ok := r.drivers[channel]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown channel %q", channel)
	}
	return d.Deliver(replyTo, text)
}
