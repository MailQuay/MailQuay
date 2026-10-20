package events

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

func receive(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatal("subscriber channel closed unexpectedly")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func TestPublishMonotonicAndReplayLimit(t *testing.T) {
	hub := NewHub()
	const published = 1200
	for i := 1; i <= published; i++ {
		event := hub.Publish("message.new", i)
		if event.ID != uint64(i) {
			t.Fatalf("event %d has ID %d", i, event.ID)
		}
	}
	if got := hub.LatestID(); got != published {
		t.Fatalf("LatestID() = %d, want %d", got, published)
	}

	replayed := hub.Since(0)
	if len(replayed) != ringSize {
		t.Fatalf("Since(0) returned %d events, want %d", len(replayed), ringSize)
	}
	if replayed[0].ID != published-ringSize+1 {
		t.Fatalf("oldest replayed ID = %d, want %d", replayed[0].ID, published-ringSize+1)
	}
	if replayed[len(replayed)-1].ID != published {
		t.Fatalf("newest replayed ID = %d, want %d", replayed[len(replayed)-1].ID, published)
	}
	for i := 1; i < len(replayed); i++ {
		if replayed[i].ID != replayed[i-1].ID+1 {
			t.Fatalf("replayed IDs are not contiguous at %d: %d, %d", i, replayed[i-1].ID, replayed[i].ID)
		}
	}
	if replayed[0].Data != 201 {
		t.Fatalf("replayed data = %v, want 201", replayed[0].Data)
	}

	if got := hub.Since(published - 1); len(got) != 1 || got[0].ID != published {
		t.Fatalf("Since(latest-1) = %+v, want only %d", got, published)
	}
	if got := hub.Since(published); len(got) != 0 {
		t.Fatalf("Since(latest) = %+v, want empty", got)
	}
	if got := hub.Since(published + 100); len(got) != 0 {
		t.Fatalf("Since(future) = %+v, want empty", got)
	}
}

func TestReplayWindow(t *testing.T) {
	now := time.Date(2025, time.September, 1, 12, 0, 0, 0, time.UTC)
	hub := NewHub()
	hub.now = func() time.Time { return now }

	first := hub.Publish("first", nil)
	now = now.Add(4 * time.Minute)
	second := hub.Publish("second", nil)

	now = now.Add(2 * time.Minute)
	replayed := hub.Since(0)
	if len(replayed) != 1 || replayed[0].ID != second.ID {
		t.Fatalf("Since(0) = %+v, want only event %d (first is older than the window)", replayed, second.ID)
	}
	if got := hub.Since(first.ID); len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("Since(first) = %+v, want only event %d", got, second.ID)
	}

	now = now.Add(4 * time.Minute)
	if got := hub.Since(0); len(got) != 0 {
		t.Fatalf("Since(0) after the window = %+v, want empty", got)
	}
}

func TestSubscriberIsolationAndUnsubscribe(t *testing.T) {
	hub := NewHub()
	firstCh, unsubscribeFirst := hub.Subscribe(context.Background())
	secondCh, unsubscribeSecond := hub.Subscribe(context.Background())
	defer unsubscribeSecond()

	event := hub.Publish("first", 1)
	if got := receive(t, firstCh); got.ID != event.ID {
		t.Fatalf("first subscriber received %+v", got)
	}
	if got := receive(t, secondCh); got.ID != event.ID {
		t.Fatalf("second subscriber received %+v", got)
	}

	unsubscribeFirst()
	unsubscribeFirst() // idempotent

	if _, ok := <-firstCh; ok {
		t.Fatal("unsubscribed channel is still open")
	}

	second := hub.Publish("second", 2)
	if got := receive(t, secondCh); got.ID != second.ID {
		t.Fatalf("second subscriber received %+v after first unsubscribed", got)
	}
}

func TestSlowConsumerDoesNotBlockAndDropsOldest(t *testing.T) {
	hub := NewHub()
	ch, unsubscribe := hub.Subscribe(context.Background())
	defer unsubscribe()

	const published = subscriberBuffer + 100
	for i := 1; i <= published; i++ {
		hub.Publish("task.updated", i)
	}

	var received []Event
	for {
		select {
		case event := <-ch:
			received = append(received, event)
		default:
			goto drained
		}
	}
drained:
	if len(received) != subscriberBuffer {
		t.Fatalf("slow subscriber buffered %d events, want %d", len(received), subscriberBuffer)
	}
	if got, want := received[0].ID, uint64(published-subscriberBuffer+1); got != want {
		t.Fatalf("oldest buffered ID = %d, want %d", got, want)
	}
	if got, want := received[len(received)-1].ID, uint64(published); got != want {
		t.Fatalf("newest buffered ID = %d, want %d", got, want)
	}
}

func TestSubscribeContextCancel(t *testing.T) {
	hub := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	ch, unsubscribe := hub.Subscribe(ctx)

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel delivered an event instead of closing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed after context cancellation")
	}

	unsubscribe() // still idempotent after cancellation
	hub.Publish("after", nil)
}

func TestConcurrentPublish(t *testing.T) {
	hub := NewHub()
	const workers = 8
	const perWorker = 100

	var wg sync.WaitGroup
	ids := make(chan uint64, workers*perWorker)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range perWorker {
				ids <- hub.Publish("message.updated", nil).ID
			}
		}()
	}
	wg.Wait()
	close(ids)

	collected := make([]uint64, 0, workers*perWorker)
	for id := range ids {
		collected = append(collected, id)
	}
	if len(collected) != workers*perWorker {
		t.Fatalf("published %d IDs, want %d", len(collected), workers*perWorker)
	}
	sort.Slice(collected, func(i, j int) bool { return collected[i] < collected[j] })
	for i, id := range collected {
		if id != uint64(i+1) {
			t.Fatalf("ID at position %d = %d, want %d", i, id, i+1)
		}
	}
	if got := hub.LatestID(); got != workers*perWorker {
		t.Fatalf("LatestID() = %d, want %d", got, workers*perWorker)
	}
}
