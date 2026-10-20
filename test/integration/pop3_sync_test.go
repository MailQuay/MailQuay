//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
	mailsync "github.com/mailquay/mailquay/internal/sync"
)

type pop3Message struct {
	uidl string
	raw  string
}

type pop3Server struct {
	messages []pop3Message
	mu       sync.Mutex
	deleted  map[int]bool
}

func (s *pop3Server) serve(t *testing.T, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(t, conn)
	}
}

func (s *pop3Server) handle(t *testing.T, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	write := func(line string) { fmt.Fprint(writer, line+"\r\n"); _ = writer.Flush() }
	write("+OK fake pop3 ready")
	s.mu.Lock()
	deleted := make(map[int]bool)
	s.mu.Unlock()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			write("-ERR empty")
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "USER", "PASS":
			write("+OK")
		case "STAT":
			total := 0
			for _, msg := range s.messages {
				total += len(msg.raw)
			}
			write(fmt.Sprintf("+OK %d %d", len(s.messages), total))
		case "UIDL":
			write("+OK")
			for i, msg := range s.messages {
				write(fmt.Sprintf("%d %s", i+1, msg.uidl))
			}
			write(".")
		case "LIST":
			write("+OK")
			for i, msg := range s.messages {
				write(fmt.Sprintf("%d %d", i+1, len(msg.raw)))
			}
			write(".")
		case "TOP":
			if len(fields) < 2 {
				write("-ERR bad top")
				continue
			}
			var n int
			fmt.Sscanf(fields[1], "%d", &n)
			if n < 1 || n > len(s.messages) {
				write("-ERR no message")
				continue
			}
			header := strings.SplitN(s.messages[n-1].raw, "\r\n\r\n", 2)[0]
			write("+OK")
			fmt.Fprint(writer, header+"\r\n\r\n.\r\n")
			_ = writer.Flush()
		case "RETR":
			var n int
			fmt.Sscanf(fields[1], "%d", &n)
			if n < 1 || n > len(s.messages) {
				write("-ERR no message")
				continue
			}
			write("+OK")
			body := strings.ReplaceAll(s.messages[n-1].raw, "\r\n.", "\r\n..")
			fmt.Fprint(writer, body)
			if !strings.HasSuffix(body, "\r\n") {
				fmt.Fprint(writer, "\r\n")
			}
			fmt.Fprint(writer, ".\r\n")
			_ = writer.Flush()
		case "DELE":
			var n int
			fmt.Sscanf(fields[1], "%d", &n)
			deleted[n] = true
			write("+OK")
		case "QUIT":
			write("+OK bye")
			return
		default:
			write("-ERR unsupported")
		}
	}
}

func TestPOP3InitialSync(t *testing.T) {
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

	fake := &pop3Server{messages: []pop3Message{
		{uidl: "uidl-one", raw: "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: POP first\r\nMessage-ID: <pop1@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\npop3 body one\r\n"},
		{uidl: "uidl-two", raw: "From: carol@example.com\r\nTo: bob@example.com\r\nSubject: POP second\r\nMessage-ID: <pop2@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\npop3 body two\r\n"},
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go fake.serve(t, ln)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	cipher, err := crypto.NewFromBase64(encoded)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SecretKey: encoded, DataDir: t.TempDir()}
	logger := platform.NewLogger("error", "text")
	blobs, err := storage.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	settingsSvc := settings.NewService(st, platform.Resources{CPUs: 2, MemoryBytes: 4 << 30}, cipher, settings.Bootstrap{SyncInterval: time.Minute, POP3Interval: time.Minute, BodyPrefetchMax: 2 << 20, MaxAttachment: 25 << 20})
	accounts := account.NewService(st, cipher, logger)
	mailboxes := mailbox.NewService(st)
	messages := message.NewService(st, hub, blobs, logger)
	syncer := mailsync.NewService(st, cfg, logger, hub, blobs, accounts, mailboxes, messages, settingsSvc)
	messages.SetContent(syncer)

	acct, err := accounts.Create(ctx, account.Input{
		Name: "POP", Email: "pop@example.com", Username: "pop", AuthType: "password",
		Password: "secret", ReceiveProtocol: "pop3", POP3Addr: ln.Addr().String(), POP3Security: "none",
		SyncEnabled: true, InitialSyncRange: "all", DownloadScope: "full_no_attachments",
		SentCopyMode: "none", POP3LeaveOnServer: true,
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
	rows, err := st.ListMailboxMessages(ctx, sqlcgen.ListMailboxMessagesParams{MailboxID: inbox.ID, PageLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("POP3 synced %d messages", len(rows))
	}
	for _, row := range rows {
		if row.BodyState != "cached" {
			t.Errorf("message %s body state = %s", row.MessageIDHeader, row.BodyState)
		}
	}
	if err := syncer.SyncNow(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	count, _ := st.CountMessagesInMailbox(ctx, inbox.ID)
	if count != 2 {
		t.Fatalf("after second POP3 sync count = %d", count)
	}
}
