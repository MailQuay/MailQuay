//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/mailsession"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
	mailsync "github.com/mailquay/mailquay/internal/sync"
)

// TestIMAPMarkReadUpdatesMailboxCounts reads a message through the message
// service and checks the stored unread counter, which feeds the sidebar badge.
// Waiting for the next sync pass left the badge stale.
func TestIMAPMarkReadUpdatesMailboxCounts(t *testing.T) {
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
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	go func() { _ = server.Serve(ln) }()
	defer server.Close()
	addr := ln.Addr().String()

	seed, err := imapclient.Dial(ctx, imapclient.Config{Address: addr, Security: "none", Username: "alice", Password: "secret", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: Alice <alice@example.com>\r\nTo: bob@example.com\r\nSubject: Unread\r\nMessage-ID: <unread@example.com>\r\nDate: Mon, 20 Oct 2025 10:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n")
	if _, err := seed.Append(ctx, "INBOX", nil, time.Now(), raw); err != nil {
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
	logger := platform.NewLogger("debug", "text")
	hub := events.NewHub()
	settingsSvc := settings.NewService(st, platform.Resources{CPUs: 2, MemoryBytes: 4 << 30}, cipher, settings.Bootstrap{SyncInterval: time.Minute, POP3Interval: time.Minute, BodyPrefetchMax: 2 << 20, MaxAttachment: 25 << 20})
	accounts := account.NewService(st, cipher, logger)
	mailboxes := mailbox.NewService(st)
	pool := mailsession.NewPool(time.Minute)
	defer pool.Close()
	messages := message.NewService(st, hub, blobs, logger)
	syncer := mailsync.NewService(st, cfg, logger, hub, blobs, accounts, mailboxes, messages, settingsSvc, nil, pool)
	messages.SetContent(syncer)

	acct, err := accounts.Create(ctx, account.Input{
		Name: "Alice", Email: "alice@example.com", Username: "alice", AuthType: "password",
		Password: "secret", ReceiveProtocol: "imap", IMAPAddr: addr, IMAPSecurity: "none",
		SyncEnabled: true, InitialSyncRange: "all", DownloadScope: "full_no_attachments",
		SentCopyMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.SyncNow(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	inbox, err := st.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: acct.ID, Role: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	if inbox.Unread != 1 {
		t.Fatalf("unread after sync = %d, want 1", inbox.Unread)
	}
	rows, err := st.ListMailboxMessages(ctx, sqlcgen.ListMailboxMessagesParams{MailboxID: inbox.ID, PageLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("inbox messages = %d, want 1", len(rows))
	}
	read := false
	if _, err := messages.Update(ctx, rows[0].ID, &read, nil); err != nil {
		t.Fatal(err)
	}
	inbox, err = st.GetMailbox(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inbox.Unread != 0 {
		t.Fatalf("unread after read = %d, want 0", inbox.Unread)
	}
	if inbox.Total != 1 {
		t.Fatalf("total after read = %d, want 1", inbox.Total)
	}
}
