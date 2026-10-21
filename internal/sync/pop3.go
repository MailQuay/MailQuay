package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/mime"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/pop3client"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func (s *Service) pop3Config(acct sqlcgen.Account, creds account.Credentials) pop3client.Config {
	addr := ""
	if acct.Pop3Addr != nil {
		addr = *acct.Pop3Addr
	}
	return pop3client.Config{
		Address: addr, Security: acct.Pop3Security, Username: acct.Username,
		Password: creds.Password, Timeout: 30 * time.Second,
	}
}

func (s *Service) syncPOP3(ctx context.Context, acct sqlcgen.Account, creds account.Credentials) (err error) {
	taskStarted := false
	defer func() {
		if err != nil && taskStarted {
			s.failTaskForAccount(ctx, "mailbox_sync", acct.ID, err)
		}
	}()
	inbox, err := s.mailboxes.EnsureMailbox(ctx, acct.ID, "inbox", "INBOX", "Inbox", false)
	if err != nil {
		return err
	}
	for _, spec := range []struct{ role, path, name string }{
		{"sent", "Sent", "Sent"},
		{"drafts", "Drafts", "Drafts"},
		{"trash", "Trash", "Trash"},
	} {
		if _, err := s.mailboxes.EnsureMailbox(ctx, acct.ID, spec.role, spec.path, spec.name, true); err != nil {
			return err
		}
	}
	client, err := s.dialPOP3(ctx, s.pop3Config(acct, creds))
	if err != nil {
		return fmt.Errorf("connect pop3: %w", err)
	}
	defer client.Close()
	_, _, err = client.Stat(ctx)
	if err != nil {
		return fmt.Errorf("pop3 stat: %w", err)
	}
	infos, err := client.UIDL(ctx)
	if err != nil {
		return fmt.Errorf("pop3 uidl: %w", err)
	}
	knownRows, err := s.store.ListPOP3Messages(ctx, inbox.ID)
	if err != nil {
		return err
	}
	known := make(map[string]uuid.UUID, len(knownRows))
	for _, row := range knownRows {
		if row.Pop3Uidl != nil {
			known[*row.Pop3Uidl] = row.ID
		}
	}
	if err := s.startTask(ctx, "mailbox_sync", &acct.ID, 0, int32(len(infos)), "polling POP3"); err != nil {
		s.logger.Debug("start pop3 task", "error", err)
	} else {
		taskStarted = true
	}
	seen := make(map[string]struct{}, len(infos))
	processed := int32(0)
	for _, info := range infos {
		seen[info.UIDL] = struct{}{}
		if _, exists := known[info.UIDL]; exists {
			processed++
			continue
		}
		if err := s.importPOP3Message(ctx, client, acct, inbox, info); err != nil {
			s.logger.Warn("import pop3 message", "uidl", info.UIDL, "error", err)
		}
		processed++
		s.updateTask(ctx, "mailbox_sync", &acct.ID, processed, int32(len(infos)), "polling POP3")
	}
	for uidl, id := range known {
		if _, remains := seen[uidl]; remains {
			continue
		}
		msg, err := s.store.GetMessage(ctx, id)
		if err != nil || msg.LocalFlag || msg.PreservedAt != nil {
			continue
		}
		if err := s.messages.DeleteLocal(ctx, msg); err != nil {
			s.logger.Debug("remove vanished pop3 message", "message", id, "error", err)
		}
	}
	now := time.Now().UTC()
	if err := s.store.UpdateMailboxState(ctx, sqlcgen.UpdateMailboxStateParams{
		ID: inbox.ID, Uidvalidity: nil, Uidnext: nil, Highestmodseq: nil,
		SyncState: "idle", LastSyncedAt: &now, LastError: "",
	}); err != nil {
		return err
	}
	if _, err := s.mailboxes.RefreshCounts(ctx, inbox.ID); err != nil {
		s.logger.Debug("refresh pop3 counts", "error", err)
	}
	refreshed, _ := s.store.GetMailbox(ctx, inbox.ID)
	s.hub.Publish("mailbox.updated", map[string]any{
		"mailboxId": inbox.ID.String(), "unread": refreshed.Unread, "total": refreshed.Total, "syncState": "idle",
	})
	s.completeTask(ctx, "mailbox_sync", &acct.ID)
	return nil
}

func (s *Service) importPOP3Message(ctx context.Context, client *pop3client.Client, acct sqlcgen.Account, mb sqlcgen.Mailbox, info pop3client.MessageInfo) error {
	var raw []byte
	var err error
	if acct.DownloadScope == "headers" {
		raw, err = client.Top(ctx, info.Number, 0)
		if err != nil {
			raw, err = client.Retr(ctx, info.Number)
		}
	} else {
		raw, err = client.Retr(ctx, info.Number)
	}
	if err != nil {
		return fmt.Errorf("fetch pop3 message: %w", err)
	}
	parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: s.settings.MaxAttachmentBytes(), KeepAttachmentData: false})
	if err != nil {
		parsed = &mime.Parsed{Subject: "(unparsed message)"}
	}
	rawSHA := ""
	bodyState := "missing"
	if acct.DownloadScope != "headers" {
		sha, err := s.storage.PutBlob(raw)
		if err != nil {
			return err
		}
		rawSHA = sha
		bodyState = "cached"
		if parsed.Text != "" {
			_ = s.storage.PutText(sha, []byte(parsed.Text))
		}
	}
	dedupe := message.DedupeKey("", parsed.MessageID, rawSHA, mb.ID.String(), nil)
	if rawSHA == "" {
		dedupe = "u:" + mb.ID.String() + ":uidl:" + info.UIDL
	}
	fromJSON, _ := json.Marshal(addressMapMime(parsed.From))
	toJSON, _ := json.Marshal(addressListMime(parsed.To))
	ccJSON, _ := json.Marshal(addressListMime(parsed.Cc))
	bccJSON, _ := json.Marshal(addressListMime(parsed.Bcc))
	var sentAt *time.Time
	if !parsed.SentAt.IsZero() {
		t := parsed.SentAt.UTC()
		sentAt = &t
	}
	uidl := info.UIDL
	received := time.Now().UTC()
	if !parsed.SentAt.IsZero() {
		received = parsed.SentAt.UTC()
	}
	var inserted sqlcgen.Message
	err = s.store.WithTx(ctx, func(q *sqlcgen.Queries) error {
		threadID, err := s.resolveThreadQ(ctx, q, acct.ID, parsed.InReplyTo, parsed.References, parsed.Subject, received)
		if err != nil {
			return err
		}
		row, err := q.UpsertPOP3Message(ctx, sqlcgen.UpsertPOP3MessageParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: acct.ID, MailboxID: mb.ID, Pop3Uidl: &uidl,
			MessageIDHeader: parsed.MessageID, DedupeKey: dedupe, InReplyTo: parsed.InReplyTo,
			ReferencesList: parsed.References, Subject: parsed.Subject, FromAddr: fromJSON,
			ToAddrs: toJSON, CcAddrs: ccJSON, BccAddrs: bccJSON, SentAt: sentAt,
			ReceivedAt: received, Size: int64(len(raw)), Flags: []string{}, Snippet: snippet(parsed),
			HasAttachments: len(parsed.Attachments) > 0, RawSha256: rawSHA,
			BodyState: bodyState, AttachmentsState: "missing",
		})
		if err != nil {
			return err
		}
		inserted = row
		if threadID != nil {
			if err := q.UpdateMessageThread(ctx, sqlcgen.UpdateMessageThreadParams{ID: row.ID, ThreadID: threadID}); err != nil {
				return err
			}
			if err := q.IncrementThread(ctx, sqlcgen.IncrementThreadParams{ID: *threadID, LastMessageAt: received}); err != nil {
				return err
			}
		}
		if parsed.Text != "" {
			_ = q.UpdateMessageSnippet(ctx, sqlcgen.UpdateMessageSnippetParams{ID: row.ID, Snippet: snippet(parsed)})
		}
		_, err = q.EnqueueSearchJob(ctx, sqlcgen.EnqueueSearchJobParams{
			ID: uuid.MustParse(platform.NewID()), MessageID: row.ID, Op: "index",
		})
		return err
	})
	if err != nil {
		return err
	}
	s.hub.Publish("message.new", map[string]any{
		"accountId": acct.ID.String(), "mailboxId": mb.ID.String(),
		"messageId": inserted.ID.String(), "unread": true,
	})
	if acct.DownloadScope == "headers" {
		return nil
	}
	if acct.DownloadScope == "full_with_attachments" {
		if err := s.persistAttachments(ctx, inserted, parsed); err != nil {
			s.logger.Debug("persist pop3 attachments", "message", inserted.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) persistAttachments(ctx context.Context, msg sqlcgen.Message, parsed *mime.Parsed) error {
	for _, att := range parsed.Attachments {
		sha := ""
		if len(att.Data) > 0 {
			var err error
			sha, err = s.storage.PutBlob(att.Data)
			if err != nil {
				return err
			}
		}
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(msg.ID.String()+att.PartPath))
		if _, err := s.store.InsertAttachment(ctx, sqlcgen.InsertAttachmentParams{
			ID: id, MessageID: msg.ID, PartPath: att.PartPath, Filename: att.Filename,
			ContentType: att.ContentType, Size: att.Size, Sha256: sha,
			Disposition: att.Disposition, ContentID: att.ContentID,
		}); err != nil {
			return err
		}
	}
	state := "cached"
	if len(parsed.Attachments) == 0 {
		state = "cached"
	}
	return s.store.SetMessageCacheState(ctx, sqlcgen.SetMessageCacheStateParams{
		ID: msg.ID, BodyState: msg.BodyState, AttachmentsState: state,
		Snippet: snippet(parsed), HasAttachments: len(parsed.Attachments) > 0,
	})
}

func snippet(parsed *mime.Parsed) string {
	text := parsed.Text
	if text == "" {
		text = parsed.Subject
	}
	runes := []rune(text)
	for i, r := range runes {
		if r == '\n' || r == '\r' || r == '\t' {
			runes[i] = ' '
		}
	}
	if len(runes) > 180 {
		runes = runes[:180]
	}
	return string(runes)
}

func addressMapMime(addr mime.Address) map[string]string {
	out := map[string]string{"email": addr.Email}
	if addr.Name != "" {
		out["name"] = addr.Name
	}
	return out
}

func addressListMime(addrs []mime.Address) []map[string]string {
	out := make([]map[string]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, addressMapMime(a))
	}
	return out
}

var _ = mailbox.SystemRole
