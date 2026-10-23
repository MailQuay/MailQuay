package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/smtpclient"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

const outboxLease = 5 * time.Minute

// Run starts the outbox dispatcher.
func (s *Service) Run(ctx context.Context) {
	_ = s.store.RecoverStuckOutbox(ctx)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			s.dispatch(ctx, sem, &wg)
		case <-s.trigger:
			s.dispatch(ctx, sem, &wg)
		}
	}
}

func (s *Service) triggerWorker(id uuid.UUID) {
	select {
	case s.trigger <- id:
	default:
	}
}

func (s *Service) dispatch(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	lease := time.Now().UTC().Add(outboxLease)
	rows, err := s.store.ClaimDueOutbox(ctx, sqlcgen.ClaimDueOutboxParams{LeaseUntil: &lease, Limit: 5})
	if err != nil {
		s.logger.Warn("claim outbox", "error", err)
		return
	}
	for _, row := range rows {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func(outbox sqlcgen.Outbox) {
			defer wg.Done()
			defer func() { <-sem }()
			sendCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			s.sendOne(sendCtx, outbox)
		}(row)
	}
}

func (s *Service) sendOne(ctx context.Context, outbox sqlcgen.Outbox) {
	draft, err := s.store.GetDraft(ctx, outbox.DraftID)
	if err != nil {
		s.failPermanent(ctx, outbox, err)
		return
	}
	acct, err := s.accounts.Get(ctx, outbox.AccountID)
	if err != nil {
		s.failPermanent(ctx, outbox, err)
		return
	}
	if acct.SmtpAddr == nil || *acct.SmtpAddr == "" {
		s.failPermanent(ctx, outbox, errors.New("account has no SMTP server configured"))
		return
	}
	raw, err := s.buildDraftMIME(ctx, acct, draft, outbox.MessageIDHeader)
	if err != nil {
		s.failPermanent(ctx, outbox, err)
		return
	}
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		s.failPermanent(ctx, outbox, err)
		return
	}
	var recipients []string
	var headers draftHeaders
	_ = json.Unmarshal(draft.Headers, &headers)
	for _, rawAddr := range append(append(append([]string{}, headers.To...), headers.Cc...), headers.Bcc...) {
		if addr, err := mail.ParseAddress(rawAddr); err == nil {
			recipients = append(recipients, addr.Address)
		}
	}
	recipients = uniqueStrings(recipients)
	if len(recipients) == 0 {
		s.failPermanent(ctx, outbox, errors.New("no valid recipients"))
		return
	}
	err = smtpclient.Send(ctx, smtpclient.Config{
		Address: *acct.SmtpAddr, Security: acct.SmtpSecurity, Username: creds.SMTPUsername,
		Password: creds.SMTPPassword, AccessToken: creds.AccessToken,
		HELOHostname: acct.SmtpHeloHostname, Timeout: 60 * time.Second,
	}, acct.Email, recipients, raw)
	if err != nil {
		s.handleSendError(ctx, outbox, acct, err)
		return
	}
	if err := s.storeSentCopy(ctx, acct, draft, raw); err != nil {
		s.logger.Warn("store sent copy", "outbox", outbox.ID, "error", err)
		_ = s.store.MarkOutboxCopyFailed(ctx, sqlcgen.MarkOutboxCopyFailedParams{ID: outbox.ID, LastError: err.Error()})
		_ = s.store.SetDraftStatus(ctx, sqlcgen.SetDraftStatusParams{ID: draft.ID, Status: "copy_failed", LastError: err.Error()})
		outbox.Status = "copy_failed"
		outbox.LastError = err.Error()
		s.publishOutbox(outbox)
		return
	}
	_ = s.store.MarkOutboxSent(ctx, outbox.ID)
	_ = s.store.SetDraftSent(ctx, sqlcgen.SetDraftSentParams{ID: draft.ID, SentMessageID: nil})
	outbox.Status = "sent"
	outbox.LastError = ""
	s.publishOutbox(outbox)
	s.reportTask(ctx, outbox, "completed", "")
}

func (s *Service) handleSendError(ctx context.Context, outbox sqlcgen.Outbox, acct sqlcgen.Account, cause error) {
	limit := s.settings.RetryLimit(ctx)
	permanent := isPermanentSMTPError(cause)
	if permanent || int(outbox.Attempts) > limit {
		s.failPermanent(ctx, outbox, cause)
		return
	}
	delay := platform.Backoff(int(outbox.Attempts)+1, 30*time.Second, 15*time.Minute)
	next := time.Now().UTC().Add(delay)
	if err := s.store.RetryOutbox(ctx, sqlcgen.RetryOutboxParams{ID: outbox.ID, NextAttemptAt: next, LastError: cause.Error()}); err != nil {
		s.logger.Warn("schedule outbox retry", "outbox", outbox.ID, "error", err)
	}
	outbox.Status = "pending"
	outbox.LastError = cause.Error()
	s.publishOutbox(outbox)
	s.reportTask(ctx, outbox, "running", cause.Error())
}

func (s *Service) failPermanent(ctx context.Context, outbox sqlcgen.Outbox, cause error) {
	_ = s.store.MarkOutboxFailed(ctx, sqlcgen.MarkOutboxFailedParams{ID: outbox.ID, LastError: cause.Error()})
	_ = s.store.SetDraftStatus(ctx, sqlcgen.SetDraftStatusParams{ID: outbox.DraftID, Status: "failed", LastError: cause.Error()})
	outbox.Status = "failed"
	outbox.LastError = cause.Error()
	s.publishOutbox(outbox)
	s.reportTask(ctx, outbox, "failed", cause.Error())
}

func (s *Service) reportTask(ctx context.Context, outbox sqlcgen.Outbox, status, errText string) {
	task, err := s.store.UpsertTask(ctx, sqlcgen.UpsertTaskParams{
		ID: uuid.MustParse(platform.NewID()), Type: "outbox_send", AccountID: &outbox.AccountID,
		Status: "running", Done: 0, Total: 1, Unit: "messages", Message: "sending mail", StartedAt: ptrTime(time.Now().UTC()),
	})
	if err != nil {
		return
	}
	payload := map[string]any{
		"taskId": task.ID.String(), "type": "outbox_send", "accountId": outbox.AccountID.String(),
		"status": status, "done": task.Done, "total": 1, "unit": "messages",
	}
	switch status {
	case "failed":
		_ = s.store.FailTask(ctx, sqlcgen.FailTaskParams{ID: task.ID, Error: errText})
		payload["error"] = errText
	case "completed":
		_ = s.store.CompleteTask(ctx, task.ID)
	default:
		_ = s.store.UpdateTaskProgress(ctx, sqlcgen.UpdateTaskProgressParams{ID: task.ID, Done: 1, Total: 1, Message: "sending mail"})
	}
	s.hub.Publish("task.updated", payload)
	if status == "completed" {
		s.hub.Publish("task.removed", map[string]any{"taskId": task.ID.String()})
	}
}

func (s *Service) buildDraftMIME(ctx context.Context, acct sqlcgen.Account, draft sqlcgen.Draft, messageID string) ([]byte, error) {
	var headers draftHeaders
	if err := json.Unmarshal(draft.Headers, &headers); err != nil {
		return nil, fmt.Errorf("decode draft headers: %w", err)
	}
	to, err := parseAddressList(headers.To)
	if err != nil {
		return nil, err
	}
	cc, err := parseAddressList(headers.Cc)
	if err != nil {
		return nil, err
	}
	bcc, err := parseAddressList(headers.Bcc)
	if err != nil {
		return nil, err
	}
	var atts []DraftAttachment
	_ = json.Unmarshal(draft.Attachments, &atts)
	blobs := make([]attachmentBlob, 0, len(atts))
	for _, att := range atts {
		if att.SHA256 == "" {
			continue
		}
		data, err := s.readAttachment(att.SHA256)
		if err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", att.Filename, err)
		}
		blobs = append(blobs, attachmentBlob{Filename: att.Filename, ContentType: att.ContentType, Data: data})
	}
	text, html := draft.BodyText, draft.BodyHtml
	return buildMIME(messageParts{
		From: senderAddress(acct), To: to, Cc: cc, Bcc: bcc,
		ReplyTo: headers.ReplyTo, Subject: headers.Subject,
		MessageID: messageID, InReplyTo: draft.InReplyTo, References: headers.References,
		Date: time.Now().UTC(), Text: text, HTML: html, Attachments: blobs,
	})
}

func (s *Service) readAttachment(sha string) ([]byte, error) {
	reader, err := s.blobs.OpenBlob(sha)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data := make([]byte, 0, 1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	return data, nil
}

func (s *Service) storeSentCopy(ctx context.Context, acct sqlcgen.Account, draft sqlcgen.Draft, raw []byte) error {
	if acct.ReceiveProtocol == "imap" {
		switch acct.SentCopyMode {
		case "append":
			if err := s.appendToSent(ctx, acct, draft, raw); err != nil {
				return err
			}
		}
		return nil
	}
	return s.storeLocalSent(ctx, acct, draft, raw)
}

func (s *Service) appendToSent(ctx context.Context, acct sqlcgen.Account, draft sqlcgen.Draft, raw []byte) error {
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		return err
	}
	sent, err := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: acct.ID, Role: "sent"})
	if err != nil {
		return fmt.Errorf("sent mailbox not found: %w", err)
	}
	return s.sessions.Do(ctx, acct.ID, false, s.imapDialer(acct, creds), func(ctx context.Context, client *imapclient.Client) error {
		if _, err := client.Append(ctx, sent.Path, []string{"\\Seen"}, time.Now().UTC(), raw); err != nil {
			return err
		}
		return nil
	})
}

func (s *Service) storeLocalSent(ctx context.Context, acct sqlcgen.Account, draft sqlcgen.Draft, raw []byte) error {
	sent, err := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: acct.ID, Role: "sent"})
	if err != nil {
		created, err := s.store.UpsertMailbox(ctx, sqlcgen.UpsertMailboxParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: acct.ID, Path: "Sent", Name: "Sent",
			Delimiter: "/", Role: "sent", Attributes: []byte("[]"), LocalOnly: true,
			SyncMode: "off", Uidvalidity: nil, Uidnext: nil, Highestmodseq: nil,
		})
		if err != nil {
			return err
		}
		sent = created
	}
	sha, err := s.blobs.PutBlob(raw)
	if err != nil {
		return err
	}
	var headers draftHeaders
	_ = json.Unmarshal(draft.Headers, &headers)
	toJSON, _ := json.Marshal(headers.To)
	ccJSON, _ := json.Marshal(headers.Cc)
	bccJSON, _ := json.Marshal(headers.Bcc)
	sender := senderAddress(acct)
	fromJSON, _ := json.Marshal(map[string]string{"name": sender.Name, "email": sender.Address})
	received := time.Now().UTC()
	headerID := makeMessageID(acct.Email)
	dedupe := message.DedupeKey("", headerID, sha, sent.ID.String(), nil)
	row, err := s.store.InsertMessage(ctx, sqlcgen.InsertMessageParams{
		ID: uuid.MustParse(platform.NewID()), AccountID: acct.ID, MailboxID: sent.ID,
		Uid: nil, Pop3Uidl: nil, Modseq: nil, MessageIDHeader: headerID,
		ProviderMsgID: "", DedupeKey: dedupe, InReplyTo: draft.InReplyTo,
		Subject: headers.Subject, FromAddr: fromJSON, ToAddrs: toJSON, CcAddrs: ccJSON, BccAddrs: bccJSON,
		SentAt: &received, ReceivedAt: received, Size: int64(len(raw)), Flags: []string{"\\Seen"},
		Keywords: []string{}, LocalFlag: false, PreservedAt: nil, Snippet: truncateRunes(draft.BodyText, 180),
		HasAttachments: false, RawSha256: sha, BodyState: "cached", AttachmentsState: "missing",
	})
	if err != nil {
		return err
	}
	if draft.BodyText != "" {
		_ = s.blobs.PutText(sha, []byte(draft.BodyText))
	}
	_, _ = s.store.EnqueueSearchJob(ctx, sqlcgen.EnqueueSearchJobParams{
		ID: uuid.MustParse(platform.NewID()), MessageID: row.ID, Op: "index",
	})
	_ = s.store.UpdateMailboxCounts(ctx, sqlcgen.UpdateMailboxCountsParams{ID: sent.ID, Total: sent.Total + 1, Unread: sent.Unread})
	return nil
}

func isPermanentSMTPError(err error) bool {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "authentication") || strings.Contains(text, "auth failed") ||
		strings.Contains(text, "535") || strings.Contains(text, "5.7.") {
		return true
	}
	for _, code := range []string{"501", "502", "503", "504", "550", "551", "552", "553", "554"} {
		if strings.Contains(text, code) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	return out
}

func ptrTime(t time.Time) *time.Time { return &t }

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// SendNow attempts one outbox record immediately (integration tests and admin retries).
func (s *Service) SendNow(ctx context.Context, outboxID uuid.UUID) error {
	outbox, err := s.store.GetOutbox(ctx, outboxID)
	if err != nil {
		return err
	}
	s.sendOne(ctx, outbox)
	return nil
}

// senderAddress builds the From header for an account. The default send name
// falls back to the account display name when it is not configured.
func senderAddress(acct sqlcgen.Account) mail.Address {
	name := strings.TrimSpace(acct.DefaultSendName)
	if name == "" {
		name = acct.Name
	}
	return mail.Address{Name: name, Address: acct.Email}
}
