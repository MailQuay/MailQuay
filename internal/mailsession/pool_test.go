package mailsession

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/imapclient"
)

func TestPoolReusesConnection(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	dial := dialer(t, addr, &dials)
	p := NewPool(time.Minute)
	defer p.Close()
	id := uuid.New()

	for range 2 {
		if err := p.Do(context.Background(), id, false, dial, listMailboxes); err != nil {
			t.Fatal(err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

func TestPoolSerializesCommands(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	p := NewPool(time.Minute)
	defer p.Close()
	id := uuid.New()

	var active, peak atomic.Int32
	fn := func(ctx context.Context, c *imapclient.Client) error {
		now := active.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return nil
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Do(context.Background(), id, false, dialer(t, addr, &dials), fn); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak concurrent operations = %d, want 1", peak.Load())
	}
}

func TestInteractiveJumpsQueue(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	p := NewPool(time.Minute)
	defer p.Close()
	id := uuid.New()

	var mu sync.Mutex
	var order []string
	record := func(name string) func(context.Context, *imapclient.Client) error {
		return func(context.Context, *imapclient.Client) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = p.Do(context.Background(), id, false, dialer(t, addr, &dials), func(context.Context, *imapclient.Client) error {
			close(started)
			<-release
			mu.Lock()
			order = append(order, "background-running")
			mu.Unlock()
			return nil
		})
	}()
	<-started

	normalDone := make(chan struct{})
	go func() {
		defer close(normalDone)
		_ = p.Do(context.Background(), id, false, dialer(t, addr, &dials), record("normal"))
	}()
	time.Sleep(10 * time.Millisecond)
	urgentDone := make(chan struct{})
	go func() {
		defer close(urgentDone)
		_ = p.Do(context.Background(), id, true, dialer(t, addr, &dials), record("urgent"))
	}()
	time.Sleep(10 * time.Millisecond)
	close(release)
	<-urgentDone
	<-normalDone

	mu.Lock()
	defer mu.Unlock()
	want := []string{"background-running", "urgent", "normal"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestInteractiveFallsBackWhenBusy(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	p := NewPool(time.Minute)
	defer p.Close()
	id := uuid.New()

	started := make(chan struct{})
	release := make(chan struct{})
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		_ = p.Do(context.Background(), id, false, dialer(t, addr, &dials), func(context.Context, *imapclient.Client) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	start := time.Now()
	if err := p.Do(context.Background(), id, true, dialer(t, addr, &dials), listMailboxes); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= 800*time.Millisecond {
		t.Fatalf("interactive call took %v; it waited for the busy session", elapsed)
	}
	close(release)
	<-blockerDone
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials = %d, want 2 (session + transient)", got)
	}
}

func TestPoolRedialsAfterError(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	p := NewPool(time.Minute)
	defer p.Close()
	id := uuid.New()
	boom := errors.New("boom")
	fail := func(context.Context, *imapclient.Client) error { return boom }

	if err := p.Do(context.Background(), id, false, dialer(t, addr, &dials), fail); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if err := p.Do(context.Background(), id, false, dialer(t, addr, &dials), listMailboxes); err != nil {
		t.Fatal(err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials = %d, want 2", got)
	}
}

func TestReapClosesIdleSessions(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	p := NewPool(3 * time.Minute)
	defer p.Close()
	id := uuid.New()
	if err := p.Do(context.Background(), id, false, dialer(t, addr, &dials), listMailboxes); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	s := p.sessions[id]
	p.mu.Unlock()
	if s == nil || s.client == nil {
		t.Fatal("session has no connection")
	}
	conn := s.client
	p.reap(time.Now().Add(4 * time.Minute))
	if !conn.Closed() {
		t.Fatal("idle connection was not closed")
	}
}

func TestTryDoReturnsBusy(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	p := NewPool(time.Minute)
	defer p.Close()
	id := uuid.New()

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Do(context.Background(), id, false, dialer(t, addr, &dials), func(context.Context, *imapclient.Client) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	err := p.TryDo(context.Background(), id, 50*time.Millisecond, dialer(t, addr, &dials), listMailboxes)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	close(release)
	<-done
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

func TestNilPoolUsesTransientConnection(t *testing.T) {
	addr := startServer(t)
	var dials atomic.Int32
	var p *Pool
	for range 2 {
		if err := p.Do(context.Background(), uuid.New(), false, dialer(t, addr, &dials), listMailboxes); err != nil {
			t.Fatal(err)
		}
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials = %d, want 2", got)
	}
}

func listMailboxes(ctx context.Context, c *imapclient.Client) error {
	_, err := c.List(ctx)
	return err
}

func dialer(t *testing.T, addr string, dials *atomic.Int32) DialFunc {
	t.Helper()
	return func(ctx context.Context, interactive bool) (*imapclient.Client, error) {
		dials.Add(1)
		return imapclient.Dial(ctx, imapclient.Config{
			Address: addr, Security: imapclient.SecurityNone,
			Username: "alice", Password: "secret", Timeout: 5 * time.Second,
		})
	}
}

func startServer(t *testing.T) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = ln.Close()
	})
	return ln.Addr().String()
}
