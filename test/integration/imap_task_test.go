//go:build integration

package integration

import (
	"bytes"
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
	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/netpace"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	mailsync "github.com/mailquay/mailquay/internal/sync"
)

// TestIMAPMailboxFailureFinalizesTask makes the first FETCH fail after the
// sync task has started. The task must end as failed instead of staying
// running, and a second attempt must reuse the same failed row.
func TestIMAPMailboxFailureFinalizesTask(t *testing.T) {
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
	defer server.Close()
	backend := ln.Addr().String()

	seed, err := imapclient.Dial(ctx, imapclient.Config{Address: backend, Security: "none", Username: "alice", Password: "secret", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: Alice <alice@example.com>\r\nTo: bob@example.com\r\nSubject: Hello\r\nMessage-ID: <one@example.com>\r\nDate: Mon, 20 Oct 2025 10:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n")
	if _, err := seed.Append(ctx, "INBOX", []string{"\\Seen"}, time.Now(), raw); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

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
	logger := platform.NewLogger("error", "text")
	hub := events.NewHub()
	settingsSvc := settings.NewService(st, platform.Resources{CPUs: 2, MemoryBytes: 4 << 30}, cipher, settings.Bootstrap{SyncInterval: time.Minute, POP3Interval: time.Minute, BodyPrefetchMax: 2 << 20, MaxAttachment: 25 << 20})
	accounts := account.NewService(st, cipher, logger)
	mailboxes := mailbox.NewService(st)
	messages := message.NewService(st, hub, blobs, logger)
	syncer := mailsync.NewService(st, cfg, logger, hub, blobs, accounts, mailboxes, messages, settingsSvc, nil)
	messages.SetContent(syncer)

	acct, err := accounts.Create(ctx, account.Input{
		Name: "Alice", Email: "alice@example.com", Username: "alice", AuthType: "password",
		Password: "secret", ReceiveProtocol: "imap", IMAPAddr: fetchClosingProxy(t, backend), IMAPSecurity: "none",
		SyncEnabled: true, InitialSyncRange: "all", DownloadScope: "full_no_attachments",
		SentCopyMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := syncer.SyncNow(ctx, acct.ID); err != nil {
		t.Fatalf("account sync should survive a mailbox failure: %v", err)
	}
	assertFailedSyncTasks(t, ctx, st, acct.ID, 1)

	if err := syncer.SyncNow(ctx, acct.ID); err != nil {
		t.Fatalf("second account sync: %v", err)
	}
	assertFailedSyncTasks(t, ctx, st, acct.ID, 1)
}

func assertFailedSyncTasks(t *testing.T, ctx context.Context, st *store.Store, accountID uuid.UUID, want int) {
	t.Helper()
	rows, err := st.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, row := range rows {
		if row.AccountID == nil || *row.AccountID != accountID {
			continue
		}
		switch row.Status {
		case "running", "pending":
			t.Fatalf("task %s (%s) still %s after a mailbox failure", row.ID, row.Type, row.Status)
		case "failed":
			failed++
			if !strings.Contains(row.Error, "fetch envelopes") {
				t.Fatalf("failed task error = %q, want fetch failure", row.Error)
			}
		}
	}
	if failed != want {
		t.Fatalf("failed tasks = %d, want %d", failed, want)
	}
}

// fetchClosingProxy forwards IMAP traffic to upstream and drops the connection
// on the first FETCH command, simulating a provider that aborts mid-sync.
func fetchClosingProxy(t *testing.T, upstream string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go proxyConnection(client, upstream)
		}
	}()
	return ln.Addr().String()
}

func proxyConnection(client net.Conn, upstream string) {
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = server.Close()
		})
	}
	go func() {
		defer closeBoth()
		buf := make([]byte, 32*1024)
		var tail []byte
		for {
			n, err := client.Read(buf)
			if n > 0 {
				window := append(tail, bytes.ToUpper(buf[:n])...)
				if bytes.Contains(window, []byte("FETCH")) {
					return
				}
				if len(window) > 8 {
					tail = window[len(window)-8:]
				} else {
					tail = window
				}
				if _, werr := server.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	_, _ = io.Copy(client, server)
	closeBoth()
}

// TestIMAPDialPacing checks that background syncs to the same host are spaced
// by the pacer instead of dialing back to back.
func TestIMAPDialPacing(t *testing.T) {
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
	defer server.Close()

	var mu sync.Mutex
	var dials []time.Time
	proxyAddr := timingProxy(t, ln.Addr().String(), func(at time.Time) {
		mu.Lock()
		dials = append(dials, at)
		mu.Unlock()
	})

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
	hub := events.NewHub()
	settingsSvc := settings.NewService(st, platform.Resources{CPUs: 2, MemoryBytes: 4 << 30}, cipher, settings.Bootstrap{SyncInterval: time.Minute, POP3Interval: time.Minute, BodyPrefetchMax: 2 << 20, MaxAttachment: 25 << 20})
	accounts := account.NewService(st, cipher, platform.NewLogger("error", "text"))
	mailboxes := mailbox.NewService(st)
	messages := message.NewService(st, hub, blobs, platform.NewLogger("error", "text"))
	pacer := netpace.New(time.Second)
	syncer := mailsync.NewService(st, cfg, platform.NewLogger("error", "text"), hub, blobs, accounts, mailboxes, messages, settingsSvc, pacer)
	messages.SetContent(syncer)

	first, err := accounts.Create(ctx, account.Input{
		Name: "Alice", Email: "alice@example.com", Username: "alice", AuthType: "password",
		Password: "secret", ReceiveProtocol: "imap", IMAPAddr: proxyAddr, IMAPSecurity: "none",
		SyncEnabled: true, InitialSyncRange: "all", DownloadScope: "full_no_attachments",
		SentCopyMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounts.Create(ctx, account.Input{
		Name: "Alice Two", Email: "alice2@example.com", Username: "alice", AuthType: "password",
		Password: "secret", ReceiveProtocol: "imap", IMAPAddr: proxyAddr, IMAPSecurity: "none",
		SyncEnabled: true, InitialSyncRange: "all", DownloadScope: "full_no_attachments",
		SentCopyMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := syncer.SyncNow(ctx, first.ID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := syncer.SyncNow(ctx, second.ID); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	mu.Lock()
	count := len(dials)
	times := append([]time.Time(nil), dials...)
	mu.Unlock()
	if count < 2 {
		t.Fatalf("recorded %d dials, want at least 2", count)
	}
	if gap := times[1].Sub(times[0]); gap < 800*time.Millisecond {
		t.Fatalf("dials to the same host were %v apart, want at least 800ms", gap)
	}
}

func timingProxy(t *testing.T, upstream string, record func(time.Time)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			record(time.Now())
			go func() {
				server, err := net.Dial("tcp", upstream)
				if err != nil {
					_ = client.Close()
					return
				}
				go func() {
					_, _ = io.Copy(server, client)
					_ = server.Close()
				}()
				_, _ = io.Copy(client, server)
				_ = client.Close()
			}()
		}
	}()
	return ln.Addr().String()
}
