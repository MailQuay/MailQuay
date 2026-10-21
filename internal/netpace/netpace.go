// Package netpace spaces outbound connection attempts to the same remote host.
//
// Mail providers throttle and risk-control clients by connection rate, so the
// pacing budget belongs to the remote endpoint. Local concurrency settings
// decide how much work runs in parallel; this package decides how often the
// remote host is contacted at all.
package netpace

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Pacer serializes connection attempts per remote host at a fixed interval.
// A nil Pacer admits every attempt immediately.
type Pacer struct {
	interval time.Duration

	mu   sync.Mutex
	next map[string]time.Time
}

// New returns a Pacer that starts at most one connection per host every
// interval. Non-positive intervals disable pacing.
func New(interval time.Duration) *Pacer {
	return &Pacer{interval: interval, next: make(map[string]time.Time)}
}

// Interval reports the configured spacing per host.
func (p *Pacer) Interval() time.Duration {
	if p == nil {
		return 0
	}
	return p.interval
}

// Wait reserves the next connection slot for addr and blocks until the slot
// starts. It returns the context error when ctx ends before the slot.
func (p *Pacer) Wait(ctx context.Context, addr string) error {
	if p == nil || p.interval <= 0 {
		return nil
	}
	host := hostKey(addr)
	for {
		p.mu.Lock()
		now := time.Now()
		next := p.next[host]
		if !now.Before(next) {
			p.next[host] = now.Add(p.interval)
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()

		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// hostKey normalizes an address to the throttled bucket. Ports are ignored so
// IMAP and POP3 connections to the same server share one budget.
func hostKey(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return strings.ToLower(addr)
}

type interactiveKey struct{}

// WithInteractive marks ctx as user-initiated work. Such dials are not queued
// behind background prefetch, so opening a message stays responsive.
func WithInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveKey{}, true)
}

// IsInteractive reports whether ctx carries the user-initiated marker.
func IsInteractive(ctx context.Context) bool {
	marked, _ := ctx.Value(interactiveKey{}).(bool)
	return marked
}
