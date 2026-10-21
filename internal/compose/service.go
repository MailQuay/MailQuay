package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/identity"
	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mailsession"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/netpace"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

var (
	// ErrNotFound is returned for missing drafts or outbox rows.
	ErrNotFound = errors.New("draft not found")
	// ErrRevisionConflict means the draft was changed elsewhere.
	ErrRevisionConflict = errors.New("draft revision conflict")
	// ErrInvalidDraft is returned for invalid compose input.
	ErrInvalidDraft = errors.New("invalid draft")
)

// DraftInput is the editable draft payload.
type DraftInput struct {
	IdentityAccountID uuid.UUID
	IdentityEmail     string
	InReplyTo         string
	To                []string
	Cc                []string
	Bcc               []string
	Subject           string
	BodyText          string
	BodyHTML          string
	AttachmentIDs     []uuid.UUID
	Revision          int32
}

// SendInput starts delivery for a draft.
type SendInput struct {
	DraftID           uuid.UUID
	IdentityAccountID uuid.UUID
	IdentityEmail     string
	ScheduledAt       *time.Time
}

// DraftAttachment is stored in drafts.attachments.
type DraftAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type draftHeaders struct {
	To         []string `json:"to"`
	Cc         []string `json:"cc"`
	Bcc        []string `json:"bcc"`
	Subject    string   `json:"subject"`
	References []string `json:"references,omitempty"`
}

// Service manages drafts, sending and the outbox.
type Service struct {
	store      *store.Store
	accounts   *account.Service
	identities *identity.Service
	settings   *settings.Service
	messages   *message.Service
	blobs      *storage.Store
	hub        *events.Hub
	logger     *slog.Logger
	pacer      *netpace.Pacer
	sessions   *mailsession.Pool
	trigger    chan uuid.UUID
}

// NewService builds the compose service.
func NewService(st *store.Store, accounts *account.Service, identities *identity.Service,
	settingsSvc *settings.Service, messages *message.Service, blobs *storage.Store,
	hub *events.Hub, logger *slog.Logger, pacer *netpace.Pacer, sessions *mailsession.Pool) *Service {
	return &Service{
		store: st, accounts: accounts, identities: identities, settings: settingsSvc,
		messages: messages, blobs: blobs, hub: hub, logger: logger, pacer: pacer,
		sessions: sessions, trigger: make(chan uuid.UUID, 32),
	}
}

// dialIMAP paces connection attempts per remote host before dialing.
func (s *Service) dialIMAP(ctx context.Context, cfg imapclient.Config) (*imapclient.Client, error) {
	if err := s.pacer.Wait(ctx, cfg.Address); err != nil {
		return nil, err
	}
	return imapclient.Dial(ctx, cfg)
}

// imapDialer returns the background dialer for an account session.
func (s *Service) imapDialer(acct sqlcgen.Account, creds account.Credentials) mailsession.DialFunc {
	cfg := imapclient.Config{
		Address: deref(acct.ImapAddr), Security: acct.ImapSecurity, Username: acct.Username,
		Password: creds.Password, AccessToken: creds.AccessToken, Timeout: 30 * time.Second,
	}
	return func(ctx context.Context, interactive bool) (*imapclient.Client, error) {
		if interactive {
			return imapclient.Dial(ctx, cfg)
		}
		return s.dialIMAP(ctx, cfg)
	}
}

// List returns drafts ordered by update time.
func (s *Service) List(ctx context.Context, accountID *uuid.UUID) ([]sqlcgen.Draft, error) {
	return s.store.ListDrafts(ctx, sqlcgen.ListDraftsParams{AccountID: accountID, PageLimit: 200})
}

// Get loads a draft.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (sqlcgen.Draft, error) {
	draft, err := s.store.GetDraft(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Draft{}, ErrNotFound
		}
		return sqlcgen.Draft{}, fmt.Errorf("load draft: %w", err)
	}
	return draft, nil
}

// Create makes a new local draft.
func (s *Service) Create(ctx context.Context, in DraftInput) (sqlcgen.Draft, error) {
	if err := s.validateIdentity(ctx, in.IdentityAccountID, in.IdentityEmail); err != nil {
		return sqlcgen.Draft{}, err
	}
	headers, err := encodeHeaders(in)
	if err != nil {
		return sqlcgen.Draft{}, err
	}
	attachments, err := s.resolveAttachments(ctx, in.AttachmentIDs)
	if err != nil {
		return sqlcgen.Draft{}, err
	}
	contentHash := hashContent(in, attachments)
	draft, err := s.store.CreateDraft(ctx, sqlcgen.CreateDraftParams{
		ID: uuid.MustParse(platform.NewID()), AccountID: in.IdentityAccountID,
		IdentityAccountID: in.IdentityAccountID, IdentityEmail: strings.ToLower(in.IdentityEmail),
		InReplyTo: in.InReplyTo, Headers: headers, BodyText: in.BodyText, BodyHtml: in.BodyHTML,
		Attachments: attachments, Revision: 1, ContentHash: contentHash, Origin: "new",
		SyncState: "local", Status: "editing",
	})
	if err != nil {
		return sqlcgen.Draft{}, fmt.Errorf("create draft: %w", err)
	}
	return draft, nil
}

// Update saves draft content with optimistic revision checking.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in DraftInput) (sqlcgen.Draft, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return sqlcgen.Draft{}, err
	}
	if in.Revision == 0 {
		in.Revision = current.Revision
	}
	if err := s.validateIdentity(ctx, in.IdentityAccountID, in.IdentityEmail); err != nil {
		return sqlcgen.Draft{}, err
	}
	headers, err := encodeHeaders(in)
	if err != nil {
		return sqlcgen.Draft{}, err
	}
	attachments, err := s.resolveAttachments(ctx, in.AttachmentIDs)
	if err != nil {
		return sqlcgen.Draft{}, err
	}
	contentHash := hashContent(in, attachments)
	draft, err := s.store.UpdateDraft(ctx, sqlcgen.UpdateDraftParams{
		ID: id, IdentityAccountID: in.IdentityAccountID, IdentityEmail: strings.ToLower(in.IdentityEmail),
		InReplyTo: in.InReplyTo, Headers: headers, BodyText: in.BodyText, BodyHtml: in.BodyHTML,
		Attachments: attachments, ContentHash: contentHash, Revision: in.Revision,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Draft{}, ErrRevisionConflict
		}
		return sqlcgen.Draft{}, fmt.Errorf("update draft: %w", err)
	}
	_ = s.store.DeleteOutboxByDraft(ctx, id)
	return draft, nil
}

// Delete removes a draft.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	return s.store.DeleteDraft(ctx, id)
}

// Send queues a draft for delivery.
func (s *Service) Send(ctx context.Context, in SendInput) (sqlcgen.Outbox, error) {
	draft, err := s.Get(ctx, in.DraftID)
	if err != nil {
		return sqlcgen.Outbox{}, err
	}
	if draft.Status == "sent" {
		return sqlcgen.Outbox{}, errors.New("draft was already sent")
	}
	if err := s.validateIdentity(ctx, in.IdentityAccountID, in.IdentityEmail); err != nil {
		return sqlcgen.Outbox{}, err
	}
	var hd draftHeaders
	_ = json.Unmarshal(draft.Headers, &hd)
	if len(hd.To) == 0 && len(hd.Cc) == 0 && len(hd.Bcc) == 0 {
		return sqlcgen.Outbox{}, errors.New("draft has no recipients")
	}
	messageID := makeMessageID(in.IdentityEmail)
	status := "pending"
	next := time.Now().UTC()
	var scheduled *time.Time
	if in.ScheduledAt != nil && in.ScheduledAt.After(time.Now().UTC()) {
		t := in.ScheduledAt.UTC()
		scheduled = &t
		status = "scheduled"
		next = t
	}
	outbox, err := s.store.CreateOutbox(ctx, sqlcgen.CreateOutboxParams{
		ID: uuid.MustParse(platform.NewID()), AccountID: in.IdentityAccountID,
		IdentityAccountID: in.IdentityAccountID, IdentityEmail: strings.ToLower(in.IdentityEmail),
		DraftID: draft.ID, MessageIDHeader: messageID, Status: status,
		ScheduledAt: scheduled, NextAttemptAt: next,
	})
	if err != nil {
		return sqlcgen.Outbox{}, fmt.Errorf("create outbox: %w", err)
	}
	_ = s.store.SetDraftStatus(ctx, sqlcgen.SetDraftStatusParams{ID: draft.ID, Status: "sending", LastError: ""})
	s.publishOutbox(outbox)
	if status == "pending" {
		s.triggerWorker(outbox.ID)
	}
	return outbox, nil
}

// ListOutbox returns active outbox records.
func (s *Service) ListOutbox(ctx context.Context) ([]sqlcgen.Outbox, error) {
	return s.store.ListOutbox(ctx)
}

// Retry resets a failed outbox record.
func (s *Service) Retry(ctx context.Context, id uuid.UUID) error {
	if _, err := s.store.GetOutbox(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := s.store.RetryOutbox(ctx, sqlcgen.RetryOutboxParams{ID: id, NextAttemptAt: time.Now().UTC(), LastError: ""}); err != nil {
		return err
	}
	s.triggerWorker(id)
	return nil
}

// Cancel stops a pending or scheduled send while keeping the draft.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) error {
	outbox, err := s.store.GetOutbox(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := s.store.CancelOutbox(ctx, id); err != nil {
		return err
	}
	_ = s.store.SetDraftStatus(ctx, sqlcgen.SetDraftStatusParams{ID: outbox.DraftID, Status: "editing", LastError: ""})
	outbox.Status = "cancelled"
	s.publishOutbox(outbox)
	return nil
}

// Reschedule moves a scheduled send.
func (s *Service) Reschedule(ctx context.Context, id uuid.UUID, scheduledAt time.Time) (sqlcgen.Outbox, error) {
	outbox, err := s.store.RescheduleOutbox(ctx, sqlcgen.RescheduleOutboxParams{ID: id, ScheduledAt: &scheduledAt})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Outbox{}, ErrNotFound
		}
		return sqlcgen.Outbox{}, err
	}
	s.publishOutbox(outbox)
	return outbox, nil
}

// SyncDraft pushes a draft to the remote Drafts folder when mirroring is enabled.
func (s *Service) SyncDraft(ctx context.Context, id uuid.UUID) error {
	draft, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return s.pushDraft(ctx, draft)
}

func (s *Service) validateIdentity(ctx context.Context, accountID uuid.UUID, email string) error {
	if accountID == uuid.Nil || email == "" {
		return errors.New("identityAccountId and identityEmail are required")
	}
	if _, err := s.accounts.Get(ctx, accountID); err != nil {
		return err
	}
	if _, err := s.identities.Get(ctx, accountID, email); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return errors.New("identity does not belong to the account")
		}
		return err
	}
	return nil
}

func (s *Service) resolveAttachments(ctx context.Context, ids []uuid.UUID) (json.RawMessage, error) {
	if len(ids) == 0 {
		return json.RawMessage("[]"), nil
	}
	rows, err := s.store.ListUploadedAttachments(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(ids) {
		return nil, errors.New("one or more attachments were not found")
	}
	out := make([]DraftAttachment, 0, len(rows))
	for _, row := range rows {
		out = append(out, DraftAttachment{
			ID: row.ID.String(), Filename: row.Filename, ContentType: row.ContentType,
			Size: row.Size, SHA256: row.Sha256,
		})
	}
	raw, _ := json.Marshal(out)
	return raw, nil
}

// Upload stores an attachment and returns its metadata.
func (s *Service) Upload(ctx context.Context, filename, contentType string, data []byte) (sqlcgen.UploadedAttachment, error) {
	if limit := s.settings.MaxAttachmentBytes(); int64(len(data)) > limit {
		return sqlcgen.UploadedAttachment{}, fmt.Errorf("attachment exceeds the %d byte limit", limit)
	}
	sha, err := s.blobs.PutBlob(data)
	if err != nil {
		return sqlcgen.UploadedAttachment{}, err
	}
	return s.store.CreateUploadedAttachment(ctx, sqlcgen.CreateUploadedAttachmentParams{
		ID: uuid.MustParse(platform.NewID()), Filename: sanitizeFilename(filename),
		ContentType: defaultContentType(contentType), Size: int64(len(data)), Sha256: sha,
	})
}

func encodeHeaders(in DraftInput) (json.RawMessage, error) {
	if _, err := parseAddressList(in.To); err != nil {
		return nil, err
	}
	if _, err := parseAddressList(in.Cc); err != nil {
		return nil, err
	}
	if _, err := parseAddressList(in.Bcc); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(draftHeaders{To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject})
	return raw, err
}

func hashContent(in DraftInput, attachments json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(in.Subject))
	h.Write([]byte{0})
	h.Write([]byte(in.BodyText))
	h.Write([]byte{0})
	h.Write([]byte(in.BodyHTML))
	h.Write([]byte{0})
	h.Write(attachments)
	return hex.EncodeToString(h.Sum(nil))
}

func makeMessageID(email string) string {
	domain := "mailquay.local"
	if _, err := mail.ParseAddress(email); err == nil {
		parts := strings.SplitN(email, "@", 2)
		if len(parts) == 2 && parts[1] != "" {
			domain = parts[1]
		}
	}
	id, _ := uuid.NewV7()
	return "<" + id.String() + "@" + domain + ">"
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.TrimSpace(name)
	if name == "" {
		return "attachment"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func defaultContentType(ctype string) string {
	if strings.TrimSpace(ctype) == "" {
		return "application/octet-stream"
	}
	return ctype
}

func (s *Service) publishOutbox(outbox sqlcgen.Outbox) {
	payload := map[string]any{
		"outboxId": outbox.ID.String(), "status": outbox.Status, "draftId": outbox.DraftID.String(),
	}
	if outbox.LastError != "" {
		payload["error"] = outbox.LastError
	}
	s.hub.Publish("outbox.updated", payload)
}
