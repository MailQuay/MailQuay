// Package events provides the in-process event hub that feeds SSE clients.
package events

import (
	"context"
	"sync"
	"time"
)

const (
	// ringSize bounds replay memory to the most recent events.
	ringSize = 1000
	// replayWindow bounds how old a replayed event may be.
	replayWindow = 5 * time.Minute
	// subscriberBuffer is the per-connection queue. Slow consumers drop
	// their oldest event instead of blocking publishers.
	subscriberBuffer = 64
)

// Event is one published change. IDs are strictly increasing and start at 1.
type Event struct {
	ID   uint64
	Type string
	Data any
}

type ringEntry struct {
	event Event
	at    time.Time
}

type subscriber struct {
	ch     chan Event
	closed bool // guarded by Hub.mu
}

// Hub fans events out to subscribers and keeps a bounded replay buffer.
type Hub struct {
	mu   sync.Mutex
	seq  uint64
	ring []ringEntry
	head int
	subs map[*subscriber]struct{}
	now  func() time.Time
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{
		ring: make([]ringEntry, 0, ringSize),
		subs: make(map[*subscriber]struct{}),
		now:  time.Now,
	}
}

// Publish assigns the next ID and delivers the event to every subscriber
// without blocking. A subscriber whose buffer is full loses its oldest event.
func (h *Hub) Publish(typ string, data any) Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq++
	event := Event{ID: h.seq, Type: typ, Data: data}
	h.appendLocked(event)

	for sub := range h.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// Drop the oldest queued event, then enqueue the new one.
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- event:
			default:
			}
		}
	}
	return event
}

func (h *Hub) appendLocked(event Event) {
	entry := ringEntry{event: event, at: h.now()}
	if len(h.ring) < ringSize {
		h.ring = append(h.ring, entry)
		return
	}
	h.ring[h.head] = entry
	h.head = (h.head + 1) % ringSize
}

// Subscribe registers a listener. The returned channel is closed when
// unsubscribe is called or ctx is done, whichever happens first. Callers must
// call unsubscribe when they stop reading so the forwarding goroutine exits.
// The unsubscribe function is idempotent.
func (h *Hub) Subscribe(ctx context.Context) (<-chan Event, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	subscribeCtx, cancel := context.WithCancel(ctx)

	sub := &subscriber{ch: make(chan Event, subscriberBuffer)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()

	var unsubscribe func()
	unsubscribe = func() {
		cancel()
		h.remove(sub)
	}
	go func() {
		<-subscribeCtx.Done()
		unsubscribe()
	}()

	return sub.ch, unsubscribe
}

func (h *Hub) remove(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub.closed {
		return
	}
	sub.closed = true
	delete(h.subs, sub)
	close(sub.ch)
}

// Since returns retained events with IDs greater than id, oldest first. The
// buffer keeps at most the last 1000 events published within 5 minutes.
func (h *Hub) Since(id uint64) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	cutoff := h.now().Add(-replayWindow)
	replayed := make([]Event, 0, len(h.ring))
	for offset := range len(h.ring) {
		index := offset
		if len(h.ring) == ringSize {
			index = (h.head + offset) % ringSize
		}
		entry := h.ring[index]
		if entry.event.ID <= id || entry.at.Before(cutoff) {
			continue
		}
		replayed = append(replayed, entry.event)
	}
	return replayed
}

// LatestID returns the ID of the most recently published event.
func (h *Hub) LatestID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}
