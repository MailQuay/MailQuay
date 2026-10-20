package sync

import (
	"context"
	"sync"
	"time"
)

// limiter is a resizable concurrency gate. Settings changes take effect for
// new acquisitions without restarting workers.
type limiter struct {
	mu     sync.Mutex
	limit  int
	active int
}

func newLimiter(limit int) *limiter {
	if limit < 1 {
		limit = 1
	}
	return &limiter{limit: limit}
}

func (l *limiter) setLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	l.mu.Lock()
	l.limit = limit
	l.mu.Unlock()
}

func (l *limiter) acquire(ctx context.Context) bool {
	for {
		l.mu.Lock()
		if l.active < l.limit {
			l.active++
			l.mu.Unlock()
			return true
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.mu.Unlock()
}

func (l *limiter) currentLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}
