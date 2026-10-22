package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/mime"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

var (
	// ErrNotFound is returned for missing messages.
	ErrNotFound = errors.New("message not found")
	// ErrContentUnavailable means the body could not be fetched.
	ErrContentUnavailable = errors.New("message content unavailable")
)

// Body is the presentation form of a message body.
type Body struct {
	HTML                string
	Text                string
	RemoteImagesBlocked bool
	FetchedFromServer   bool
	Attachments         []sqlcgen.Attachment
}

// Content is implemented by the sync service.
type Content interface {
	Body(ctx context.Context, msg sqlcgen.Message, force bool) (Body, error)
	Raw(ctx context.Context, msg sqlcgen.Message) ([]byte, error)
}

// ArchiveIndexer indexes an archive document.
type ArchiveIndexer func(ctx context.Context, entry sqlcgen.LocalArchive) error

// ListOptions filters message listing.
type ListOptions struct {
	View      string
	AccountID *uuid.UUID
	MailboxID *uuid.UUID
	Cursor    string
	Limit     int
}

// Service implements message state changes shared by HTTP and sync.
type Service struct {
	store     *store.Store
	hub       *events.Hub
	content   Content
	archives  *storage.Store
	logger    *slog.Logger
	OnArchive ArchiveIndexer
	// AttachmentLimit returns the current upload/attachment cap.
	AttachmentLimit func() int64
}

// NewService builds the message service.
func NewService(st *store.Store, hub *events.Hub, archives *storage.Store, logger *slog.Logger) *Service {
	return &Service{store: st, hub: hub, archives: archives, logger: logger}
}

// SetContent wires the sync-backed content provider.
func (s *Service) SetContent(content Content) { s.content = content }

// List returns a cursor page.
func (s *Service) List(ctx context.Context, opts ListOptions) ([]sqlcgen.Message, string, error) {
	if opts.Limit <= 0 || opts.Limit > 200 {
		opts.Limit = 50
	}
	received, id, err := DecodeCursor(opts.Cursor)
	if err != nil {
		return nil, "", err
	}
	var cursorReceived *time.Time
	var cursorID *uuid.UUID
	if !received.IsZero() {
		cursorReceived = &received
		cursorID = &id
	}
	limit := int32(opts.Limit + 1)
	var items []sqlcgen.Message
	switch opts.View {
	case "mailbox":
		if opts.MailboxID == nil {
			return nil, "", errors.New("mailboxId is required for the mailbox view")
		}
		rows, err := s.store.ListMailboxMessages(ctx, sqlcgen.ListMailboxMessagesParams{
			MailboxID: *opts.MailboxID, CursorReceived: cursorReceived, CursorID: cursorID, PageLimit: limit,
		})
		if err != nil {
			return nil, "", fmt.Errorf("list mailbox messages: %w", err)
		}
		items = rows
	case "unread":
		if opts.MailboxID != nil {
			rows, err := s.store.ListMailboxUnreadMessages(ctx, sqlcgen.ListMailboxUnreadMessagesParams{
				MailboxID: *opts.MailboxID, CursorReceived: cursorReceived, CursorID: cursorID, PageLimit: limit,
			})
			if err != nil {
				return nil, "", fmt.Errorf("list mailbox unread messages: %w", err)
			}
			items = rows
			break
		}
		fallthrough
	case "all", "marked":
		marked := opts.View == "marked"
		params := sqlcgen.ListUnifiedMessagesParams{
			CursorReceived: cursorReceived, CursorID: cursorID, PageLimit: limit, AccountID: opts.AccountID,
		}
		if opts.View == "unread" {
			v := true
			params.Unread = &v
		}
		if marked {
			v := true
			params.Marked = &v
			include := true
			params.IncludePreserved = &include
		}
		rows, err := s.store.ListUnifiedMessages(ctx, params)
		if err != nil {
			return nil, "", fmt.Errorf("list unified messages: %w", err)
		}
		for _, row := range rows {
			items = append(items, messageFromUnified(row))
		}
	case "sent", "trash":
		rows, err := s.store.ListMessagesByRole(ctx, sqlcgen.ListMessagesByRoleParams{
			Role: opts.View, AccountID: opts.AccountID, CursorReceived: cursorReceived,
			CursorID: cursorID, PageLimit: limit,
		})
		if err != nil {
			return nil, "", fmt.Errorf("list %s messages: %w", opts.View, err)
		}
		for _, row := range rows {
			items = append(items, messageFromRoleRow(row))
		}
	default:
		return nil, "", fmt.Errorf("unknown view %q", opts.View)
	}
	next := ""
	if len(items) > opts.Limit {
		items = items[:opts.Limit]
		last := items[len(items)-1]
		next = EncodeCursor(last.ReceivedAt, last.ID)
	}
	return items, next, nil
}

// Get loads a message by ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (sqlcgen.Message, error) {
	msg, err := s.store.GetMessage(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Message{}, ErrNotFound
		}
		return sqlcgen.Message{}, fmt.Errorf("load message: %w", err)
	}
	return msg, nil
}

// Body returns the body and attachment metadata, fetching on demand.
func (s *Service) Body(ctx context.Context, id uuid.UUID, force bool) (Body, error) {
	msg, err := s.Get(ctx, id)
	if err != nil {
		return Body{}, err
	}
	if s.content == nil {
		return Body{}, ErrContentUnavailable
	}
	return s.content.Body(ctx, msg, force)
}

// Raw returns the original MIME bytes.
func (s *Service) Raw(ctx context.Context, id uuid.UUID) ([]byte, error) {
	msg, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.content == nil {
		return nil, ErrContentUnavailable
	}
	return s.content.Raw(ctx, msg)
}

// Attachments lists attachment metadata for a message.
func (s *Service) Attachments(ctx context.Context, id uuid.UUID) ([]sqlcgen.Attachment, error) {
	return s.store.ListAttachments(ctx, id)
}

// Attachment loads a single attachment.
func (s *Service) Attachment(ctx context.Context, messageID, attachmentID uuid.UUID) (sqlcgen.Attachment, error) {
	att, err := s.store.GetAttachment(ctx, sqlcgen.GetAttachmentParams{MessageID: messageID, ID: attachmentID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Attachment{}, ErrNotFound
		}
		return sqlcgen.Attachment{}, fmt.Errorf("load attachment: %w", err)
	}
	return att, nil
}

// Update changes read and marked state across all copies with the same dedupe key.
func (s *Service) Update(ctx context.Context, id uuid.UUID, unread, marked *bool) (sqlcgen.Message, error) {
	msg, err := s.Get(ctx, id)
	if err != nil {
		return sqlcgen.Message{}, err
	}
	acct, err := s.store.GetAccount(ctx, msg.AccountID)
	if err != nil {
		return sqlcgen.Message{}, fmt.Errorf("load account: %w", err)
	}
	mb, err := s.store.GetMailbox(ctx, msg.MailboxID)
	if err != nil {
		return sqlcgen.Message{}, fmt.Errorf("load mailbox: %w", err)
	}
	if unread != nil {
		flags := toggleFlag(msg.Flags, "\\Seen", !*unread)
		if err := s.store.UpdateFlagsByDedupe(ctx, sqlcgen.UpdateFlagsByDedupeParams{
			AccountID: msg.AccountID, DedupeKey: msg.DedupeKey, Flags: flags, Keywords: msg.Keywords,
		}); err != nil {
			return sqlcgen.Message{}, fmt.Errorf("update flags: %w", err)
		}
		s.refreshDedupeMailboxCounts(ctx, msg)
		if acct.ReceiveProtocol == "imap" && mb.Role != "sent" && !mb.LocalOnly {
			payload, _ := json.Marshal(map[string]any{"add": !*unread, "flags": []string{"\\Seen"}})
			_, err := s.store.EnqueuePendingOp(ctx, sqlcgen.EnqueuePendingOpParams{
				ID: uuid.MustParse(platform.NewID()), AccountID: msg.AccountID, MailboxID: &msg.MailboxID,
				MessageID: &msg.ID, Uid: msg.Uid, Kind: "flags", Payload: payload,
			})
			if err != nil {
				s.logger.Warn("queue flag op", "message", id, "error", err)
			}
		}
		msg.Flags = flags
	}
	if marked != nil {
		if err := s.store.SetMessagesLocalFlagByDedupe(ctx, sqlcgen.SetMessagesLocalFlagByDedupeParams{
			AccountID: msg.AccountID, DedupeKey: msg.DedupeKey, LocalFlag: *marked,
		}); err != nil {
			return sqlcgen.Message{}, fmt.Errorf("update mark: %w", err)
		}
		msg.LocalFlag = *marked
		if *marked && s.content != nil {
			msgID := msg.ID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				if _, err := s.content.Body(ctx, msg, false); err != nil {
					s.logger.Warn("cache marked message body", "message", msgID, "error", err)
					s.reportMarkFailure(ctx, msg.AccountID, msgID, err)
				}
			}()
		}
	}
	msg.UpdatedAt = time.Now().UTC()
	s.publishMessage(msg)
	return msg, nil
}

// Move relocates a message locally and queues the IMAP operation.
func (s *Service) Move(ctx context.Context, id, mailboxID uuid.UUID) error {
	msg, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	target, err := s.store.GetMailbox(ctx, mailboxID)
	if err != nil {
		return fmt.Errorf("load target mailbox: %w", err)
	}
	if target.AccountID != msg.AccountID {
		return errors.New("target mailbox belongs to another account")
	}
	acct, err := s.store.GetAccount(ctx, msg.AccountID)
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	src, _ := s.store.GetMailbox(ctx, msg.MailboxID)
	if err := s.store.MoveMessage(ctx, sqlcgen.MoveMessageParams{ID: id, MailboxID: mailboxID, Uid: nil}); err != nil {
		return fmt.Errorf("move message: %w", err)
	}
	if acct.ReceiveProtocol == "imap" && !target.LocalOnly && src.Role != "sent" {
		payload, _ := json.Marshal(map[string]string{"dest": target.Path})
		if _, err := s.store.EnqueuePendingOp(ctx, sqlcgen.EnqueuePendingOpParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: msg.AccountID, MailboxID: &msg.MailboxID,
			MessageID: &msg.ID, Uid: msg.Uid, Kind: "move", Payload: payload,
		}); err != nil {
			s.logger.Warn("queue move op", "message", id, "error", err)
		}
	}
	srcID := msg.MailboxID
	msg.MailboxID = mailboxID
	s.refreshMailboxCounts(ctx, srcID)
	if mailboxID != srcID {
		s.refreshMailboxCounts(ctx, mailboxID)
	}
	s.publishMessage(msg)
	return nil
}

// refreshDedupeMailboxCounts refreshes every mailbox holding a copy of msg.
func (s *Service) refreshDedupeMailboxCounts(ctx context.Context, msg sqlcgen.Message) {
	ids, err := s.store.ListMailboxIDsForDedupe(ctx, sqlcgen.ListMailboxIDsForDedupeParams{
		AccountID: msg.AccountID, DedupeKey: msg.DedupeKey,
	})
	if err != nil {
		s.logger.Warn("list message mailboxes", "message", msg.ID, "error", err)
		ids = []uuid.UUID{msg.MailboxID}
	}
	for _, mailboxID := range ids {
		s.refreshMailboxCounts(ctx, mailboxID)
	}
}

// refreshMailboxCounts recomputes stored total and unread counters and pushes
// them to the sidebar. Read changes, moves and deletes alter the counts
// immediately, but the mailbox sync pass that normally recomputes them can be
// minutes away.
func (s *Service) refreshMailboxCounts(ctx context.Context, mailboxID uuid.UUID) {
	total, err := s.store.CountMessagesInMailbox(ctx, mailboxID)
	if err != nil {
		s.logger.Warn("count mailbox messages", "mailbox", mailboxID, "error", err)
		return
	}
	unread, err := s.store.CountUnreadInMailbox(ctx, mailboxID)
	if err != nil {
		s.logger.Warn("count mailbox unread", "mailbox", mailboxID, "error", err)
		return
	}
	if err := s.store.UpdateMailboxCounts(ctx, sqlcgen.UpdateMailboxCountsParams{
		ID: mailboxID, Total: int32(total), Unread: int32(unread),
	}); err != nil {
		s.logger.Warn("update mailbox counts", "mailbox", mailboxID, "error", err)
		return
	}
	s.hub.Publish("mailbox.updated", map[string]any{
		"mailboxId": mailboxID.String(), "total": total, "unread": unread,
	})
}

// Delete moves a message to Trash or permanently removes it when already there.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	msg, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	acct, err := s.store.GetAccount(ctx, msg.AccountID)
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	mb, err := s.store.GetMailbox(ctx, msg.MailboxID)
	if err != nil {
		return fmt.Errorf("load mailbox: %w", err)
	}
	settings, err := s.store.ListSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	deleteMode, deleteDays := deletePolicy(settings)
	if acct.ReceiveProtocol == "pop3" {
		if mb.Role != "trash" {
			trash, trashErr := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: msg.AccountID, Role: "trash"})
			if trashErr == nil && trash.ID != mb.ID {
				if err := s.Move(ctx, id, trash.ID); err != nil {
					return err
				}
				if deleteMode == "local_queue" {
					_, err := s.store.EnqueueDelete(ctx, sqlcgen.EnqueueDeleteParams{
						ID: uuid.MustParse(platform.NewID()), MessageID: msg.ID, AccountID: msg.AccountID,
						MailboxID: trash.ID, Uid: nil, DeleteAfter: time.Now().UTC().Add(time.Duration(deleteDays) * 24 * time.Hour),
					})
					if err != nil {
						return fmt.Errorf("enqueue delete: %w", err)
					}
				}
				return nil
			}
		}
		return s.PermanentlyDelete(ctx, msg, acct)
	}
	target, targetErr := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: msg.AccountID, Role: "trash"})
	if targetErr != nil {
		// Servers without a Trash folder usually still expose Junk; keeping the
		// message there leaves it recoverable until the provider cleans it up.
		target, targetErr = s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: msg.AccountID, Role: "junk"})
	}
	if targetErr == nil && mb.ID != target.ID {
		if err := s.Move(ctx, id, target.ID); err != nil {
			return err
		}
		if deleteMode == "local_queue" {
			_, err := s.store.EnqueueDelete(ctx, sqlcgen.EnqueueDeleteParams{
				ID: uuid.MustParse(platform.NewID()), MessageID: msg.ID, AccountID: msg.AccountID,
				MailboxID: target.ID, Uid: msg.Uid, DeleteAfter: time.Now().UTC().Add(time.Duration(deleteDays) * 24 * time.Hour),
			})
			if err != nil {
				return fmt.Errorf("enqueue delete: %w", err)
			}
		}
		return nil
	}
	if err := s.PermanentlyDelete(ctx, msg, acct); err != nil {
		return err
	}
	return nil
}

// PermanentlyDelete removes local data and queues remote cleanup.
func (s *Service) PermanentlyDelete(ctx context.Context, msg sqlcgen.Message, acct sqlcgen.Account) error {
	if acct.ReceiveProtocol == "imap" {
		payload, _ := json.Marshal(map[string]any{})
		if _, err := s.store.EnqueuePendingOp(ctx, sqlcgen.EnqueuePendingOpParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: msg.AccountID, MailboxID: &msg.MailboxID,
			MessageID: &msg.ID, Uid: msg.Uid, Kind: "delete", Payload: payload,
		}); err != nil {
			s.logger.Warn("queue delete op", "message", msg.ID, "error", err)
		}
	}
	if acct.ReceiveProtocol == "pop3" && acct.Pop3DeleteOnServer && msg.Pop3Uidl != nil {
		payload, _ := json.Marshal(map[string]string{"uidl": *msg.Pop3Uidl})
		if _, err := s.store.EnqueuePendingOp(ctx, sqlcgen.EnqueuePendingOpParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: msg.AccountID, MailboxID: &msg.MailboxID,
			MessageID: &msg.ID, Uid: nil, Kind: "pop3_delete", Payload: payload,
		}); err != nil {
			s.logger.Warn("queue pop3 delete op", "message", msg.ID, "error", err)
		}
	}
	if err := s.DeleteLocal(ctx, msg); err != nil {
		return err
	}
	s.refreshMailboxCounts(ctx, msg.MailboxID)
	return nil
}

// DeleteLocal removes the row and queues a search deletion.
func (s *Service) DeleteLocal(ctx context.Context, msg sqlcgen.Message) error {
	if err := s.store.DeleteMessage(ctx, msg.ID); err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	if _, err := s.store.EnqueueSearchJob(ctx, sqlcgen.EnqueueSearchJobParams{
		ID: uuid.MustParse(platform.NewID()), MessageID: msg.ID, Op: "delete",
	}); err != nil {
		s.logger.Warn("queue search delete", "message", msg.ID, "error", err)
	}
	s.hub.Publish("message.removed", map[string]any{"messageId": msg.ID.String(), "mailboxId": msg.MailboxID.String()})
	return nil
}

// Archive copies the full message into local storage.
func (s *Service) Archive(ctx context.Context, id uuid.UUID) (sqlcgen.LocalArchive, error) {
	msg, err := s.Get(ctx, id)
	if err != nil {
		return sqlcgen.LocalArchive{}, err
	}
	if s.content == nil {
		return sqlcgen.LocalArchive{}, ErrContentUnavailable
	}
	raw, err := s.content.Raw(ctx, msg)
	if err != nil {
		return sqlcgen.LocalArchive{}, err
	}
	parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: s.maxAttachmentBytes(), KeepAttachmentData: true})
	if err != nil {
		return sqlcgen.LocalArchive{}, fmt.Errorf("parse archive source: %w", err)
	}
	sha, err := s.archives.PutBlob(raw)
	if err != nil {
		return sqlcgen.LocalArchive{}, err
	}
	acct, _ := s.store.GetAccount(ctx, msg.AccountID)
	mb, _ := s.store.GetMailbox(ctx, msg.MailboxID)
	attachments := make([]map[string]any, 0, len(parsed.Attachments))
	for i, att := range parsed.Attachments {
		attachments = append(attachments, map[string]any{
			"id":       uuid.NewSHA1(uuid.NameSpaceOID, []byte(sha+att.PartPath)).String(),
			"filename": att.Filename, "contentType": att.ContentType, "size": att.Size,
			"disposition": att.Disposition, "contentId": att.ContentID, "index": i,
		})
	}
	fromJSON, _ := json.Marshal(addressJSON(parsed.From.Name, parsed.From.Email))
	toJSON, _ := json.Marshal(addressesJSON(parsed.To))
	ccJSON, _ := json.Marshal(addressesJSON(parsed.Cc))
	bccJSON, _ := json.Marshal(addressesJSON(parsed.Bcc))
	attsJSON, _ := json.Marshal(attachments)
	textSHA := ""
	if parsed.Text != "" {
		textSHA = sha
		_ = s.archives.PutText(sha, []byte(parsed.Text))
	}
	entry, err := s.store.InsertArchive(ctx, sqlcgen.InsertArchiveParams{
		ID: uuid.MustParse(platform.NewID()), OriginAccountID: &msg.AccountID,
		OriginAccountEmail: acct.Email, OriginMailboxPath: mb.Path,
		MessageIDHeader: msg.MessageIDHeader, Subject: parsed.Subject,
		FromAddr: fromJSON, ToAddrs: toJSON, CcAddrs: ccJSON, BccAddrs: bccJSON,
		SentAt: nullableTime(parsed.SentAt), ReceivedAt: msg.ReceivedAt, Size: int64(len(raw)),
		RawSha256: sha, TextSha256: textSHA, HasAttachments: len(parsed.Attachments) > 0,
		Attachments: attsJSON,
	})
	if err != nil {
		return sqlcgen.LocalArchive{}, fmt.Errorf("insert archive: %w", err)
	}
	if s.OnArchive != nil {
		if err := s.OnArchive(ctx, entry); err != nil {
			s.logger.Warn("index archive", "archive", entry.ID, "error", err)
		}
	}
	s.hub.Publish("archive.created", map[string]any{"archiveId": entry.ID.String()})
	return entry, nil
}

// UpdateSnippet stores a derived snippet.
func (s *Service) UpdateSnippet(ctx context.Context, id uuid.UUID, snippet string) {
	if err := s.store.UpdateMessageSnippet(ctx, sqlcgen.UpdateMessageSnippetParams{ID: id, Snippet: snippet}); err != nil {
		s.logger.Warn("update snippet", "message", id, "error", err)
	}
}

func (s *Service) maxAttachmentBytes() int64 {
	if s.AttachmentLimit != nil {
		return s.AttachmentLimit()
	}
	return 25 << 20
}

func (s *Service) publishMessage(msg sqlcgen.Message) {
	s.hub.Publish("message.updated", map[string]any{
		"messageId": msg.ID.String(), "mailboxId": msg.MailboxID.String(),
		"fields": []string{"flags", "localFlag"},
	})
}

func toggleFlag(flags []string, flag string, add bool) []string {
	out := make([]string, 0, len(flags)+1)
	found := false
	for _, f := range flags {
		if strings.EqualFold(f, flag) {
			found = true
			if !add {
				continue
			}
		}
		out = append(out, f)
	}
	if add && !found {
		out = append(out, flag)
	}
	return out
}

func deletePolicy(rows []sqlcgen.Setting) (string, int) {
	mode, days := "provider_auto", 30
	for _, row := range rows {
		switch row.Key {
		case "delete_mode":
			var v string
			if json.Unmarshal(row.Value, &v) == nil && v != "" {
				mode = v
			}
		case "delete_queue_days":
			var v int
			if json.Unmarshal(row.Value, &v) == nil && v > 0 {
				days = v
			}
		}
	}
	return mode, days
}

func messageFromUnified(row sqlcgen.ListUnifiedMessagesRow) sqlcgen.Message {
	return sqlcgen.Message{
		ID: row.ID, AccountID: row.AccountID, MailboxID: row.MailboxID, Uid: row.Uid,
		Pop3Uidl: row.Pop3Uidl, Modseq: row.Modseq, MessageIDHeader: row.MessageIDHeader,
		ProviderMsgID: row.ProviderMsgID, DedupeKey: row.DedupeKey, InReplyTo: row.InReplyTo,
		ReferencesList: row.ReferencesList, ThreadID: row.ThreadID, Subject: row.Subject,
		FromAddr: row.FromAddr, ToAddrs: row.ToAddrs, CcAddrs: row.CcAddrs, BccAddrs: row.BccAddrs,
		SentAt: row.SentAt, ReceivedAt: row.ReceivedAt, Size: row.Size, Flags: row.Flags,
		Keywords: row.Keywords, LocalFlag: row.LocalFlag, PreservedAt: row.PreservedAt,
		Snippet: row.Snippet, HasAttachments: row.HasAttachments, RawSha256: row.RawSha256,
		BodyState: row.BodyState, AttachmentsState: row.AttachmentsState,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func addressJSON(name, email string) map[string]string {
	out := map[string]string{"email": email}
	if name != "" {
		out["name"] = name
	}
	return out
}

func addressesJSON(addrs []mime.Address) []map[string]string {
	out := make([]map[string]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, addressJSON(a.Name, a.Email))
	}
	return out
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *Service) reportMarkFailure(ctx context.Context, accountID, messageID uuid.UUID, cause error) {
	now := time.Now().UTC()
	task, err := s.store.UpsertTask(ctx, sqlcgen.UpsertTaskParams{
		ID: uuid.MustParse(platform.NewID()), Type: "mark_cache", AccountID: &accountID,
		Status: "failed", Unit: "messages", Message: "caching marked mail", StartedAt: &now,
	})
	if err != nil {
		return
	}
	_ = s.store.FailTask(ctx, sqlcgen.FailTaskParams{ID: task.ID, Error: cause.Error()})
	s.hub.Publish("task.updated", map[string]any{
		"taskId": task.ID.String(), "type": "mark_cache", "accountId": accountID.String(),
		"status": "failed", "done": 0, "total": 1, "unit": "messages",
		"error": cause.Error(), "messageId": messageID.String(),
	})
}

func messageFromRoleRow(row sqlcgen.ListMessagesByRoleRow) sqlcgen.Message {
	return sqlcgen.Message{
		ID: row.ID, AccountID: row.AccountID, MailboxID: row.MailboxID, Uid: row.Uid,
		Pop3Uidl: row.Pop3Uidl, Modseq: row.Modseq, MessageIDHeader: row.MessageIDHeader,
		ProviderMsgID: row.ProviderMsgID, DedupeKey: row.DedupeKey, InReplyTo: row.InReplyTo,
		ReferencesList: row.ReferencesList, ThreadID: row.ThreadID, Subject: row.Subject,
		FromAddr: row.FromAddr, ToAddrs: row.ToAddrs, CcAddrs: row.CcAddrs, BccAddrs: row.BccAddrs,
		SentAt: row.SentAt, ReceivedAt: row.ReceivedAt, Size: row.Size, Flags: row.Flags,
		Keywords: row.Keywords, LocalFlag: row.LocalFlag, PreservedAt: row.PreservedAt,
		Snippet: row.Snippet, HasAttachments: row.HasAttachments, RawSha256: row.RawSha256,
		BodyState: row.BodyState, AttachmentsState: row.AttachmentsState,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
