package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/mime"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// Body implements message.Content.
func (s *Service) Body(ctx context.Context, msg sqlcgen.Message, force bool) (message.Body, error) {
	acct, err := s.store.GetAccount(ctx, msg.AccountID)
	if err != nil {
		return message.Body{}, fmt.Errorf("load account: %w", err)
	}
	if !force && acct.DownloadScope != "headers" && msg.RawSha256 != "" && msg.BodyState == "cached" && s.storage.HasBlob(msg.RawSha256) {
		raw, err := s.readBlob(msg.RawSha256)
		if err != nil {
			return message.Body{}, err
		}
		return s.presentBody(ctx, msg, raw, false)
	}
	raw, err := s.fetchRaw(ctx, acct, msg)
	if err != nil {
		return message.Body{}, err
	}
	if acct.DownloadScope == "headers" {
		return s.presentBody(ctx, msg, raw, true)
	}
	if err := s.cacheRaw(ctx, acct, msg, raw); err != nil {
		s.logger.Warn("cache message body", "message", msg.ID, "error", err)
	}
	return s.presentBody(ctx, msg, raw, true)
}

// Raw implements message.Content.
func (s *Service) Raw(ctx context.Context, msg sqlcgen.Message) ([]byte, error) {
	if msg.RawSha256 != "" && s.storage.HasBlob(msg.RawSha256) {
		return s.readBlob(msg.RawSha256)
	}
	acct, err := s.store.GetAccount(ctx, msg.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load account: %w", err)
	}
	raw, err := s.fetchRaw(ctx, acct, msg)
	if err != nil {
		return nil, err
	}
	if err := s.cacheRaw(ctx, acct, msg, raw); err != nil {
		s.logger.Debug("cache raw", "message", msg.ID, "error", err)
	}
	return raw, nil
}

// AttachmentBytes returns decoded attachment content, fetching on demand.
func (s *Service) AttachmentBytes(ctx context.Context, msg sqlcgen.Message, att sqlcgen.Attachment) ([]byte, error) {
	if att.Sha256 != "" && s.storage.HasBlob(att.Sha256) {
		return s.readBlob(att.Sha256)
	}
	raw, err := s.Raw(ctx, msg)
	if err != nil {
		return nil, err
	}
	parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: s.settings.MaxAttachmentBytes(), KeepAttachmentData: true})
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}
	for _, candidate := range parsed.Attachments {
		if candidate.PartPath == att.PartPath {
			if len(candidate.Data) == 0 {
				return nil, errors.New("attachment content unavailable")
			}
			return candidate.Data, nil
		}
	}
	return nil, message.ErrNotFound
}

func (s *Service) readBlob(sha string) ([]byte, error) {
	reader, err := s.storage.OpenBlob(sha)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return readAllLimit(reader, s.settings.MaxAttachmentBytes()+32<<20)
}

func (s *Service) presentBody(ctx context.Context, msg sqlcgen.Message, raw []byte, fetched bool) (message.Body, error) {
	parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: s.settings.MaxAttachmentBytes(), KeepAttachmentData: false})
	if err != nil {
		return message.Body{}, fmt.Errorf("parse message: %w", err)
	}
	attachments, err := s.store.ListAttachments(ctx, msg.ID)
	if err != nil {
		return message.Body{}, err
	}
	return message.Body{
		HTML: parsed.HTML, Text: parsed.Text, RemoteImagesBlocked: containsRemoteImage(parsed.HTML),
		FetchedFromServer: fetched, Attachments: attachments,
	}, nil
}

func (s *Service) cacheRaw(ctx context.Context, acct sqlcgen.Account, msg sqlcgen.Message, raw []byte) error {
	sha, err := s.storage.PutBlob(raw)
	if err != nil {
		return err
	}
	keepAttachments := acct.DownloadScope == "full_with_attachments"
	parsed, err := mime.Parse(raw, mime.Options{
		MaxAttachmentBytes: s.settings.MaxAttachmentBytes(),
		KeepAttachmentData: keepAttachments,
		KeepInlineImages:   true,
	})
	if err != nil {
		return err
	}
	if parsed.Text != "" {
		if err := s.storage.PutText(sha, []byte(parsed.Text)); err != nil {
			s.logger.Debug("put text cache", "message", msg.ID, "error", err)
		}
	}
	for _, att := range parsed.Attachments {
		inlineImage := strings.HasPrefix(att.ContentType, "image/") &&
			(att.Disposition == "inline" || att.ContentID != "")
		if !keepAttachments && !inlineImage {
			continue
		}
		attSHA := ""
		if len(att.Data) > 0 {
			attSHA, err = s.storage.PutBlob(att.Data)
			if err != nil {
				return err
			}
		}
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(msg.ID.String()+att.PartPath))
		if _, err := s.store.InsertAttachment(ctx, sqlcgen.InsertAttachmentParams{
			ID: id, MessageID: msg.ID, PartPath: att.PartPath, Filename: att.Filename,
			ContentType: att.ContentType, Size: att.Size, Sha256: attSHA,
			Disposition: att.Disposition, ContentID: att.ContentID,
		}); err != nil {
			return err
		}
	}
	attState := "cached"
	if !keepAttachments {
		attState = "missing"
	}
	if err := s.store.SetMessageCacheState(ctx, sqlcgen.SetMessageCacheStateParams{
		ID: msg.ID, BodyState: "cached", AttachmentsState: attState,
		Snippet: snippet(parsed), HasAttachments: len(parsed.Attachments) > 0,
	}); err != nil {
		return err
	}
	if err := s.store.SetMessageRawInfo(ctx, sqlcgen.SetMessageRawInfoParams{
		ID: msg.ID, RawSha256: sha, BodyState: "cached",
	}); err != nil {
		return err
	}
	if msg.DedupeKey == "" || len(msg.DedupeKey) < 2 || msg.DedupeKey[0] != 'p' {
		newKey := message.DedupeKey(msg.ProviderMsgID, msg.MessageIDHeader, sha, msg.MailboxID.String(), msg.Uid)
		if newKey != msg.DedupeKey {
			_ = s.store.SetMessageDedupeKey(ctx, sqlcgen.SetMessageDedupeKeyParams{ID: msg.ID, DedupeKey: newKey})
		}
	}
	s.enqueueSearch(ctx, msg.ID, "index")
	return nil
}

func (s *Service) fetchRaw(ctx context.Context, acct sqlcgen.Account, msg sqlcgen.Message) ([]byte, error) {
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		return nil, err
	}
	mb, err := s.store.GetMailbox(ctx, msg.MailboxID)
	if err != nil {
		return nil, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if acct.ReceiveProtocol == "imap" {
		if msg.Uid == nil {
			return nil, errors.New("message has no IMAP uid")
		}
		client, err := s.dialIMAP(fetchCtx, s.imapConfig(acct, creds))
		if err != nil {
			return nil, fmt.Errorf("connect imap: %w", err)
		}
		defer client.Close()
		if _, err := client.Select(fetchCtx, mb.Path); err != nil {
			return nil, fmt.Errorf("select mailbox: %w", err)
		}
		raw, err := client.FetchRaw(fetchCtx, uint32(*msg.Uid))
		if err != nil {
			return nil, fmt.Errorf("fetch raw: %w", err)
		}
		return raw, nil
	}
	if msg.Pop3Uidl == nil {
		return nil, errors.New("message has no POP3 UIDL")
	}
	client, err := s.dialPOP3(fetchCtx, s.pop3Config(acct, creds))
	if err != nil {
		return nil, fmt.Errorf("connect pop3: %w", err)
	}
	defer client.Close()
	infos, err := client.UIDL(fetchCtx)
	if err != nil {
		return nil, fmt.Errorf("pop3 uidl: %w", err)
	}
	for _, info := range infos {
		if info.UIDL == *msg.Pop3Uidl {
			raw, err := client.Retr(fetchCtx, info.Number)
			if err != nil {
				return nil, fmt.Errorf("pop3 retr: %w", err)
			}
			_ = client.Quit(fetchCtx)
			return raw, nil
		}
	}
	return nil, errors.New("message no longer exists on the POP3 server")
}

func containsRemoteImage(html string) bool {
	for i := 0; i+4 < len(html); i++ {
		if (html[i] == 's' || html[i] == 'S') && (html[i+1] == 'r' || html[i+1] == 'R') && (html[i+2] == 'c' || html[i+2] == 'C') {
			return true
		}
	}
	return false
}
