// Package mailsession keeps one reusable IMAP connection per account.
//
// IMAP is a stateful protocol: a connection selects one mailbox at a time and
// commands must not interleave. MailQuay therefore serializes all work for an
// account on a single connection instead of dialing per message. Interactive
// requests keep a short acquisition budget and fall back to a transient
// connection so opening a message never waits behind a long sync.
package mailsession

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/imapclient"
)

// DialFunc opens a connection. interactive is true for transient connections
// that bypass background pacing.
type DialFunc func(ctx context.Context, interactive bool) (*imapclient.Client, error)

// interactiveWait bounds how long an interactive request waits for the shared
// connection before using a transient one.
const interactiveWait = 250 * time.Millisecond

// Pool owns one session per account.
type Pool struct {
	idleTimeout time.Duration

	mu        sync.Mutex
	sessions  map[uuid.UUID]*Session
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewPool returns a pool whose idle connections are closed after idleTimeout.
// A nil Pool runs every call on a transient connection.
func NewPool(idleTimeout time.Duration) *Pool {
	if idleTimeout <= 0 {
		idleTimeout = 3 * time.Minute
	}
	p := &Pool{
		idleTimeout: idleTimeout,
		sessions:    make(map[uuid.UUID]*Session),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go p.reaper()
	return p
}

// ErrBusy reports that a session stayed busy for the whole wait budget.
var ErrBusy = errors.New("mailsession: account connection busy")

// Do runs fn on the account's connection. Background calls queue in order;
// interactive calls jump the queue and fall back to a transient connection
// when the shared one stays busy.
func (p *Pool) Do(ctx context.Context, accountID uuid.UUID, interactive bool, dial DialFunc, fn func(context.Context, *imapclient.Client) error) error {
	if p == nil {
		return transient(ctx, dial, interactive, fn)
	}
	s := p.session(accountID)
	wait := time.Duration(0)
	if interactive {
		wait = interactiveWait
	}
	acquired, err := s.gate.acquire(ctx, interactive, wait)
	if err != nil {
		return err
	}
	if !acquired {
		if interactive {
			return transient(ctx, dial, true, fn)
		}
		return ErrBusy
	}
	return s.run(ctx, dial, fn)
}

// TryDo runs fn only if the session frees up within wait. Callers use it for
// low-priority background work that can simply retry on the next tick.
func (p *Pool) TryDo(ctx context.Context, accountID uuid.UUID, wait time.Duration, dial DialFunc, fn func(context.Context, *imapclient.Client) error) error {
	if p == nil {
		return transient(ctx, dial, false, fn)
	}
	s := p.session(accountID)
	acquired, err := s.gate.acquire(ctx, false, wait)
	if err != nil {
		return err
	}
	if !acquired {
		return ErrBusy
	}
	return s.run(ctx, dial, fn)
}

func transient(ctx context.Context, dial DialFunc, interactive bool, fn func(context.Context, *imapclient.Client) error) error {
	c, err := dial(ctx, interactive)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(ctx, c)
}

// Drop discards the account's connection, for example after account settings
// changed. The next call dials again.
func (p *Pool) Drop(accountID uuid.UUID) {
	if p == nil {
		return
	}
	p.mu.Lock()
	s := p.sessions[accountID]
	delete(p.sessions, accountID)
	p.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// Close closes every connection and stops the idle reaper.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stop)
		<-p.done
		p.mu.Lock()
		sessions := p.sessions
		p.sessions = make(map[uuid.UUID]*Session)
		p.mu.Unlock()
		for _, s := range sessions {
			s.close()
		}
	})
}

func (p *Pool) session(accountID uuid.UUID) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[accountID]
	if s == nil {
		s = &Session{}
		p.sessions[accountID] = s
	}
	return s
}

func (p *Pool) reaper() {
	defer close(p.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case now := <-ticker.C:
			p.reap(now)
		}
	}
}

// reap closes connections that have been idle longer than idleTimeout.
func (p *Pool) reap(now time.Time) {
	p.mu.Lock()
	sessions := make([]*Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.mu.Unlock()
	for _, s := range sessions {
		if !s.gate.tryAcquire() {
			continue
		}
		s.mu.Lock()
		if s.client != nil && now.Sub(s.lastUsed) >= p.idleTimeout {
			_ = s.client.Close()
			s.client = nil
		}
		s.mu.Unlock()
		s.gate.release()
	}
}

// Session is the per-account connection and its command gate.
type Session struct {
	gate gate

	mu       sync.Mutex
	client   *imapclient.Client
	lastUsed time.Time
}

func (s *Session) run(ctx context.Context, dial DialFunc, fn func(context.Context, *imapclient.Client) error) error {
	defer s.gate.release()
	c, err := s.conn(ctx, dial)
	if err != nil {
		return err
	}
	err = fn(ctx, c)
	s.finish(err)
	return err
}

func (s *Session) conn(ctx context.Context, dial DialFunc) (*imapclient.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && !s.client.Closed() {
		return s.client, nil
	}
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
	c, err := dial(ctx, false)
	if err != nil {
		return nil, err
	}
	s.client = c
	return c, nil
}

// finish records the outcome and discards the connection after an error so the
// next caller starts from a clean protocol state.
func (s *Session) finish(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	if s.client == nil {
		return
	}
	if cause != nil || s.client.Closed() {
		_ = s.client.Close()
		s.client = nil
	}
}

func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
}

// gate serializes commands with strict priority for interactive waiters.
type gate struct {
	mu      sync.Mutex
	busy    bool
	waiters []*waiter
}

type waiter struct {
	ch       chan struct{}
	urgent   bool
	granted  bool
	canceled bool
}

// acquire blocks until the gate is held. A positive timeout returns false when
// the budget expires first.
func (g *gate) acquire(ctx context.Context, urgent bool, timeout time.Duration) (bool, error) {
	g.mu.Lock()
	if !g.busy {
		g.busy = true
		g.mu.Unlock()
		return true, nil
	}
	w := &waiter{ch: make(chan struct{}), urgent: urgent}
	if urgent {
		idx := 0
		for idx < len(g.waiters) && g.waiters[idx].urgent {
			idx++
		}
		g.waiters = append(g.waiters, nil)
		copy(g.waiters[idx+1:], g.waiters[idx:])
		g.waiters[idx] = w
	} else {
		g.waiters = append(g.waiters, w)
	}
	g.mu.Unlock()

	var timer *time.Timer
	var expired <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-w.ch:
		return true, nil
	case <-ctx.Done():
		g.abandon(w)
		return false, ctx.Err()
	case <-expired:
		g.abandon(w)
		return false, nil
	}
}

// abandon removes a waiter that gave up. If the gate was granted in the same
// instant, ownership is handed to the next waiter.
func (g *gate) abandon(w *waiter) {
	g.mu.Lock()
	if w.granted {
		g.mu.Unlock()
		g.release()
		return
	}
	w.canceled = true
	g.mu.Unlock()
}

func (g *gate) tryAcquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy {
		return false
	}
	g.busy = true
	return true
}

func (g *gate) release() {
	g.mu.Lock()
	for len(g.waiters) > 0 {
		w := g.waiters[0]
		g.waiters = g.waiters[1:]
		if w.canceled {
			continue
		}
		w.granted = true
		close(w.ch)
		g.mu.Unlock()
		return
	}
	g.busy = false
	g.mu.Unlock()
}
