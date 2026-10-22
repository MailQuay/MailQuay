//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/mailsession"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	mailsync "github.com/mailquay/mailquay/internal/sync"
)

// TestIMAPSyncReconnectsAfterConnectionDrop drops the first SELECT in the
// middle of a sync pass. The pass must redial and finish instead of marking
// every remaining mailbox failed with errors from the dead connection.
func TestIMAPSyncReconnectsAfterConnectionDrop(t *testing.T) {
	dsn := os.Getenv("MAILQUAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MAILQUAY_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resetSchema(t, ctx, dsn)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mem := imapmemserver.New()
	user := imapmemserver.NewUser("alice", "secret")
	for _, name := range []string{"INBOX", "Archive"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	go func() { _ = server.Serve(upstream) }()
	defer server.Close()
	proxy := newDropProxy(t, upstream.Addr().String())

	dataDir := t.TempDir()
	blobs, err := storage.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	cipher, err := crypto.NewFromBase64(encoded)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SecretKey: encoded, DataDir: dataDir}
	logger := platform.NewLogger("debug", "text")
	hub := events.NewHub()
	settingsSvc := settings.NewService(st, platform.Resources{CPUs: 2, MemoryBytes: 4 << 30}, cipher, settings.Bootstrap{SyncInterval: time.Minute, POP3Interval: time.Minute, BodyPrefetchMax: 2 << 20, MaxAttachment: 25 << 20})
	accounts := account.NewService(st, cipher, logger)
	mailboxes := mailbox.NewService(st)
	messages := message.NewService(st, hub, blobs, logger)
	pool := mailsession.NewPool(time.Minute)
	defer pool.Close()
	syncer := mailsync.NewService(st, cfg, logger, hub, blobs, accounts, mailboxes, messages, settingsSvc, nil, pool)
	messages.SetContent(syncer)

	acct, err := accounts.Create(ctx, account.Input{
		Name: "Alice", Email: "alice@example.com", Username: "alice", AuthType: "password",
		Password: "secret", ReceiveProtocol: "imap", IMAPAddr: proxy.Addr(), IMAPSecurity: "none",
		SyncEnabled: true, InitialSyncRange: "all", DownloadScope: "full_no_attachments",
		SentCopyMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncNow(ctx, acct.ID); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows, err := st.ListMailboxes(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("mailboxes = %d, want 2", len(rows))
	}
	for _, mb := range rows {
		if mb.LastError != "" {
			t.Fatalf("mailbox %s kept error %q", mb.Path, mb.LastError)
		}
		if mb.SyncState != "idle" {
			t.Fatalf("mailbox %s state = %q, want idle", mb.Path, mb.SyncState)
		}
	}
	if got := proxy.Accepted(); got != 2 {
		t.Fatalf("proxy accepted %d connections, want 2 (initial + retry)", got)
	}
	state, err := st.GetAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncState != "connected" || state.LastError != "" {
		t.Fatalf("account state = %q error %q, want connected", state.SyncState, state.LastError)
	}
}

// dropProxy forwards IMAP traffic and closes both sides instead of forwarding
// the first SELECT command, simulating a provider that drops the connection
// mid-pass.
type dropProxy struct {
	upstream string
	ln       net.Listener
	wg       sync.WaitGroup

	mu       sync.Mutex
	accepted int
	dropped  bool
}

func newDropProxy(t *testing.T, upstream string) *dropProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &dropProxy{upstream: upstream, ln: ln}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *dropProxy) Addr() string { return p.ln.Addr().String() }

func (p *dropProxy) Accepted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

func (p *dropProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.accepted++
		p.mu.Unlock()
		p.wg.Add(1)
		go p.forward(conn)
	}
}

func (p *dropProxy) forward(client net.Conn) {
	defer p.wg.Done()
	upstream, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	defer client.Close()
	defer upstream.Close()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_, _ = io.Copy(client, upstream)
	}()
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if isSelectCommand(line) && p.claimDrop() {
				return
			}
			if _, werr := io.WriteString(upstream, line); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// claimDrop reports whether the caller may drop this connection; only the
// first SELECT seen by the proxy is dropped.
func (p *dropProxy) claimDrop() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dropped {
		return false
	}
	p.dropped = true
	return true
}

func isSelectCommand(line string) bool {
	fields := strings.Fields(line)
	return len(fields) >= 2 && strings.EqualFold(fields[1], "SELECT")
}
