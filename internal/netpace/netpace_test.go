package netpace

import (
	"context"
	"testing"
	"time"
)

func TestPacerSpacesSameHost(t *testing.T) {
	p := New(40 * time.Millisecond)
	ctx := context.Background()
	start := time.Now()
	for i := range 3 {
		if err := p.Wait(ctx, "imap.example.com:993"); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("paced waits returned after %v, want at least 70ms", elapsed)
	}
}

func TestPacerSeparatesHosts(t *testing.T) {
	p := New(200 * time.Millisecond)
	ctx := context.Background()
	if err := p.Wait(ctx, "a.example.com:993"); err != nil {
		t.Fatalf("host a wait: %v", err)
	}
	start := time.Now()
	if err := p.Wait(ctx, "b.example.com:993"); err != nil {
		t.Fatalf("host b wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("unrelated host waited %v", elapsed)
	}
	if err := p.Wait(ctx, "a.example.com:993"); err != nil {
		t.Fatalf("host a second wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("same host reused its slot after %v", elapsed)
	}
}

func TestPacerContextCanceled(t *testing.T) {
	p := New(time.Hour)
	ctx := context.Background()
	if err := p.Wait(ctx, "imap.example.com:993"); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(waitCtx, "imap.example.com:993"); err == nil {
		t.Fatal("wait returned nil for canceled context")
	}
}

func TestNilPacerDoesNotBlock(t *testing.T) {
	var p *Pacer
	if err := p.Wait(context.Background(), "imap.example.com:993"); err != nil {
		t.Fatalf("nil pacer wait: %v", err)
	}
	if interval := p.Interval(); interval != 0 {
		t.Fatalf("nil pacer interval = %v", interval)
	}
}

func TestHostKeyIgnoresPort(t *testing.T) {
	if a, b := hostKey("IMAP.Example.com:993"), hostKey("imap.example.com:143"); a != b {
		t.Fatalf("host keys differ: %q vs %q", a, b)
	}
}

func TestInteractiveMarker(t *testing.T) {
	if IsInteractive(context.Background()) {
		t.Fatal("background context reported as interactive")
	}
	if !IsInteractive(WithInteractive(context.Background())) {
		t.Fatal("marked context not reported as interactive")
	}
}
