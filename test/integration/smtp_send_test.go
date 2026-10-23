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

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/compose"
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
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

type sinkBackend struct {
	mu   sync.Mutex
	raw  [][]byte
	from []string
	to   [][]string
}

func (b *sinkBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &sinkSession{backend: b}, nil
}

type sinkSession struct {
	backend *sinkBackend
	from    string
	to      []string
}

func (s *sinkSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *sinkSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != "alice" || password != "secret" {
			return smtp.ErrAuthFailed
		}
		return nil
	}), nil
}

func (s *sinkSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *sinkSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *sinkSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.raw = append(s.backend.raw, raw)
	s.backend.from = append(s.backend.from, s.from)
	s.backend.to = append(s.backend.to, append([]string(nil), s.to...))
	s.backend.mu.Unlock()
	return nil
}

func (s *sinkSession) Reset()        {}
func (s *sinkSession) Logout() error { return nil }

func TestOutboxSendsSMTPAndMarksDraft(t *testing.T) {
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

	sink := &sinkBackend{}
	smtpServer := smtp.NewServer(sink)
	smtpServer.Domain = "localhost"
	smtpServer.AllowInsecureAuth = true
	smtpServer.ReadTimeout = 10 * time.Second
	smtpServer.WriteTimeout = 10 * time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = smtpServer.Serve(ln) }()
	defer smtpServer.Close()

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
	_ = mailbox.NewService(st)
	messages := message.NewService(st, hub, blobs, logger)
	composer := compose.NewService(st, accounts, settingsSvc, messages, blobs, hub, logger, nil, mailsession.NewPool(time.Minute))

	acct, err := accounts.Create(ctx, account.Input{
		Name: "Alice", Email: "alice@example.com", Username: "alice", AuthType: "password",
		Password: "secret", ReceiveProtocol: "imap", IMAPAddr: "127.0.0.1:1", IMAPSecurity: "none",
		SMTPAddr: ln.Addr().String(), SMTPSecurity: "none", SyncEnabled: true,
		InitialSyncRange: "all", DownloadScope: "full_no_attachments", SentCopyMode: "none",
		DefaultSendName: "Alice Sender", DefaultReplyTo: "replies@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := composer.Upload(ctx, "note.txt", "text/plain", []byte("attachment payload"))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := composer.Create(ctx, compose.DraftInput{
		AccountID: acct.ID, ReplyTo: "reply@example.com",
		To: []string{"bob@example.com"}, Cc: []string{"carol@example.com"},
		Subject: "Integration subject", BodyText: "hello from test", BodyHTML: "<p>hello from test</p>",
		AttachmentIDs: []uuid.UUID{uploaded.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := composer.Send(ctx, compose.SendInput{DraftID: draft.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := composer.SendNow(ctx, out.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetOutbox(ctx, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "sent" {
		t.Fatalf("outbox status = %q (%s)", updated.Status, updated.LastError)
	}
	sentDraft, err := st.GetDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sentDraft.Status != "sent" {
		t.Fatalf("draft status = %q", sentDraft.Status)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.raw) != 1 {
		t.Fatalf("captured %d messages", len(sink.raw))
	}
	raw := string(sink.raw[0])
	for _, want := range []string{
		"Subject: Integration subject", "hello from test", "bob@example.com", "note.txt",
		"YXR0YWNobWVudCBwYXlsb2Fk", "\"Alice Sender\" <alice@example.com>", "Reply-To: reply@example.com",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw message missing %q", want)
		}
	}
	if len(sink.to[0]) != 2 {
		t.Errorf("recipients = %v", sink.to[0])
	}
	_ = sqlcgen.Outbox{}
	_ = bytes.MinRead
	var _ = uuid.Nil
}
