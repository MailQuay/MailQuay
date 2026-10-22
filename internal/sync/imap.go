package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// defaultEnvelopeBatch is used when the operator setting is somehow absent.
const defaultEnvelopeBatch = 50

// maxEnvelopeBatch bounds the operator setting so one FETCH response cannot
// grow without limit.
const maxEnvelopeBatch = 200

// envelopeBatchInterval spaces FETCH commands on one connection. Providers
// throttle command and byte rates as well as connection counts, so batches are
// paced even though the connection is reused.
const envelopeBatchInterval = 200 * time.Millisecond

// errConnectionLost marks a pass that stopped because the shared IMAP
// connection died, not because a mailbox is broken. syncAccount retries once
// with a fresh connection; mailbox-level state is only recorded on the last
// attempt.
var errConnectionLost = errors.New("imap connection lost")

// envelopeBatchSize returns the configured per-FETCH envelope count.
func (s *Service) envelopeBatchSize() int {
	size := s.settings.Current().EnvelopeBatchSize
	if size < 1 {
		return defaultEnvelopeBatch
	}
	if size > maxEnvelopeBatch {
		return maxEnvelopeBatch
	}
	return size
}

func (s *Service) imapConfig(acct sqlcgen.Account, creds account.Credentials) imapclient.Config {
	addr := ""
	if acct.ImapAddr != nil {
		addr = *acct.ImapAddr
	}
	return imapclient.Config{
		Address: addr, Security: acct.ImapSecurity, Username: acct.Username,
		Password: creds.Password, AccessToken: creds.AccessToken, Timeout: 30 * time.Second,
	}
}

// syncIMAP lists folders and syncs every mailbox on one connection.
//
// A dead connection aborts the pass: retryable is true while another attempt is
// still allowed, so the mailbox that hit the failure is not marked failed only
// to have the retry clear it. Mailboxes that were never attempted keep their
// state as well. The returned error wraps errConnectionLost so the caller can
// redial and rerun the pass.
func (s *Service) syncIMAP(ctx context.Context, client *imapclient.Client, acct sqlcgen.Account, retryable bool) error {
	folders, err := client.List(ctx)
	if err != nil {
		if client.Closed() {
			return fmt.Errorf("%w: list imap folders: %v", errConnectionLost, err)
		}
		return fmt.Errorf("list imap folders: %w", err)
	}
	if _, err := s.mailboxes.UpsertFromFolders(ctx, acct.ID, folders); err != nil {
		return err
	}
	snap, err := s.settings.Get(ctx)
	if err != nil {
		return err
	}
	mailboxes, err := s.mailboxes.List(ctx, acct.ID)
	if err != nil {
		return err
	}
	for _, mb := range mailboxes {
		if mb.LocalOnly || mb.SyncMode == "off" || mb.Role == "drafts" {
			continue
		}
		if mb.Role == "sent" && snap.Values.SentSyncMode == "off" {
			continue
		}
		if err := s.syncIMAPMailbox(ctx, client, acct, mb); err != nil {
			if client.Closed() {
				if !retryable {
					s.markMailboxError(ctx, acct, mb, err)
				}
				return fmt.Errorf("%w: %v", errConnectionLost, err)
			}
			s.markMailboxError(ctx, acct, mb, err)
		}
	}
	if snap.Values.DraftSyncMode != "local" {
		if draftsBox, err := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: acct.ID, Role: "drafts"}); err == nil {
			if err := s.pullDrafts(ctx, client, acct, draftsBox); err != nil {
				s.logger.Debug("pull drafts", "account", acct.ID, "error", err)
			}
		}
	}
	return nil
}

// markMailboxError records a mailbox-level failure and notifies the UI.
func (s *Service) markMailboxError(ctx context.Context, acct sqlcgen.Account, mb sqlcgen.Mailbox, cause error) {
	s.logger.Warn("sync mailbox", "account", acct.ID, "mailbox", mb.Path, "error", cause)
	_ = s.store.UpdateMailboxState(ctx, sqlcgen.UpdateMailboxStateParams{
		ID: mb.ID, Uidvalidity: mb.Uidvalidity, Uidnext: mb.Uidnext, Highestmodseq: mb.Highestmodseq,
		SyncState: "error", LastSyncedAt: mb.LastSyncedAt, LastError: cause.Error(),
	})
	s.hub.Publish("mailbox.updated", map[string]any{"mailboxId": mb.ID.String(), "syncState": "error"})
}

func (s *Service) syncIMAPMailbox(ctx context.Context, client *imapclient.Client, acct sqlcgen.Account, mb sqlcgen.Mailbox) (err error) {
	var taskType string
	// A mailbox that fails after its task started must not leave the task
	// running forever; the UI would show an endless sync.
	defer func() {
		if err != nil && taskType != "" {
			s.failTaskForAccount(ctx, taskType, acct.ID, err)
		}
	}()
	if err := s.store.UpdateMailboxState(ctx, sqlcgen.UpdateMailboxStateParams{
		ID: mb.ID, Uidvalidity: mb.Uidvalidity, Uidnext: mb.Uidnext, Highestmodseq: mb.Highestmodseq,
		SyncState: "syncing", LastSyncedAt: mb.LastSyncedAt, LastError: "",
	}); err != nil {
		return err
	}
	selected, err := client.Select(ctx, mb.Path)
	if err != nil {
		return fmt.Errorf("select %s: %w", mb.Path, err)
	}
	initial := mb.Uidvalidity == nil
	if mb.Uidvalidity != nil && uint32(*mb.Uidvalidity) != selected.UIDValidity {
		s.logger.Info("uidvalidity changed; resetting mailbox", "mailbox", mb.Path)
		if err := s.clearMailbox(ctx, mb); err != nil {
			return err
		}
		initial = true
		mb.Uidvalidity = nil
		mb.Uidnext = nil
	}
	uids, err := s.mailboxNewUIDs(ctx, client, acct, mb, initial)
	if err != nil {
		return err
	}
	taskType = "mailbox_sync"
	if initial {
		taskType = "initial_sync"
	}
	if len(uids) > 0 {
		if err := s.startTask(ctx, taskType, &acct.ID, 0, int32(len(uids)), "syncing "+mb.Path); err != nil {
			s.logger.Debug("start sync task", "error", err)
		}
	}
	done := 0
	batchSize := s.envelopeBatchSize()
	for start := 0; start < len(uids); start += batchSize {
		if start > 0 && !sleepCtx(ctx, envelopeBatchInterval) {
			return ctx.Err()
		}
		end := start + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		batch := uids[start:end]
		envelopes, err := client.FetchEnvelopes(ctx, batch)
		if err != nil {
			return fmt.Errorf("fetch envelopes: %w", err)
		}
		for _, env := range envelopes {
			if err := s.storeIMAPEnvelope(ctx, acct, mb, env); err != nil {
				s.logger.Warn("store envelope", "message", env.MessageID, "uid", env.UID, "error", err)
				continue
			}
		}
		done += len(batch)
		s.updateTask(ctx, taskType, &acct.ID, int32(done), int32(len(uids)), "syncing "+mb.Path)
	}
	if err := s.reconcileRecentFlags(ctx, client, acct, mb); err != nil {
		s.logger.Debug("reconcile flags", "mailbox", mb.Path, "error", err)
	}
	now := time.Now().UTC()
	uidValidity := int64(selected.UIDValidity)
	uidNext := int64(selected.UIDNext)
	if err := s.store.UpdateMailboxState(ctx, sqlcgen.UpdateMailboxStateParams{
		ID: mb.ID, Uidvalidity: &uidValidity, Uidnext: &uidNext, Highestmodseq: mb.Highestmodseq,
		SyncState: "idle", LastSyncedAt: &now, LastError: "",
	}); err != nil {
		return err
	}
	if _, err := s.mailboxes.RefreshCounts(ctx, mb.ID); err != nil {
		s.logger.Debug("refresh counts", "mailbox", mb.Path, "error", err)
	}
	refreshed, _ := s.store.GetMailbox(ctx, mb.ID)
	s.hub.Publish("mailbox.updated", map[string]any{
		"mailboxId": mb.ID.String(), "unread": refreshed.Unread, "total": refreshed.Total, "syncState": "idle",
	})
	s.completeTask(ctx, taskType, &acct.ID)
	return nil
}

func (s *Service) mailboxNewUIDs(ctx context.Context, client *imapclient.Client, acct sqlcgen.Account, mb sqlcgen.Mailbox, initial bool) ([]uint32, error) {
	known, err := s.store.ListMessageUIDsByMailbox(ctx, mb.ID)
	if err != nil {
		return nil, fmt.Errorf("list known uids: %w", err)
	}
	knownSet := make(map[uint32]struct{}, len(known))
	var maxUID uint32
	for _, raw := range known {
		if raw == nil {
			continue
		}
		uid := uint32(*raw)
		knownSet[uid] = struct{}{}
		if uid > maxUID {
			maxUID = uid
		}
	}
	var uids []uint32
	if len(known) == 0 && initial {
		if acct.InitialSyncCutoffAt != nil {
			uids, err = client.UIDSearchSince(ctx, *acct.InitialSyncCutoffAt)
		} else {
			uids, err = client.UIDSearchAll(ctx)
		}
	} else if len(known) == 0 {
		uids, err = client.UIDSearchAll(ctx)
	} else {
		uids, err = client.UIDSearchNew(ctx, maxUID+1)
	}
	if err != nil {
		return nil, fmt.Errorf("search uids: %w", err)
	}
	out := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if _, exists := knownSet[uid]; !exists {
			out = append(out, uid)
		}
	}
	return out, nil
}

func (s *Service) clearMailbox(ctx context.Context, mb sqlcgen.Mailbox) error {
	ids, err := s.store.ListMessageIDsByMailbox(ctx, mb.ID)
	if err != nil {
		return fmt.Errorf("list mailbox messages: %w", err)
	}
	for _, id := range ids {
		s.enqueueSearch(ctx, id.ID, "delete")
	}
	if err := s.store.DeleteMessagesByMailbox(ctx, mb.ID); err != nil {
		return fmt.Errorf("delete mailbox messages: %w", err)
	}
	if err := s.store.DeletePendingOpsByMailbox(ctx, &mb.ID); err != nil {
		s.logger.Debug("delete pending ops", "mailbox", mb.ID, "error", err)
	}
	if err := s.store.ResetMailboxUIDs(ctx, mb.ID); err != nil {
		return fmt.Errorf("reset mailbox uids: %w", err)
	}
	return nil
}

func (s *Service) reconcileRecentFlags(ctx context.Context, client *imapclient.Client, acct sqlcgen.Account, mb sqlcgen.Mailbox) error {
	if mb.Role == "sent" || mb.SyncMode == "pull" {
		return nil
	}
	uids, err := client.UIDSearchNew(ctx, 1)
	if err != nil {
		return err
	}
	if len(uids) > 200 {
		uids = uids[len(uids)-200:]
	}
	if len(uids) == 0 {
		return nil
	}
	updates, err := client.FetchFlags(ctx, uids)
	if err != nil {
		return err
	}
	for _, update := range updates {
		found, err := s.store.FindMessageByUID(ctx, sqlcgen.FindMessageByUIDParams{MailboxID: mb.ID, Uid: ptrInt64(int64(update.UID))})
		if err != nil {
			continue
		}
		if err := s.store.UpdateMessageFlags(ctx, sqlcgen.UpdateMessageFlagsParams{
			ID: found.ID, Flags: update.Flags, Keywords: found.Keywords, Modseq: int64Ptr(int64(update.ModSeq)),
		}); err != nil {
			continue
		}
	}
	return nil
}

func (s *Service) storeIMAPEnvelope(ctx context.Context, acct sqlcgen.Account, mb sqlcgen.Mailbox, env imapclient.Envelope) error {
	uid := int64(env.UID)
	dedupe := message.DedupeKey(env.GMailID, env.MessageID, "", mb.ID.String(), &uid)
	fromJSON, _ := json.Marshal(addressMap(env.From))
	toJSON, _ := json.Marshal(addressList(env.To))
	ccJSON, _ := json.Marshal(addressList(env.Cc))
	bccJSON, _ := json.Marshal(addressList(env.Bcc))
	received := env.InternalDate
	if received.IsZero() {
		received = time.Now().UTC()
	}
	var sentAt *time.Time
	if !env.SentAt.IsZero() {
		t := env.SentAt.UTC()
		sentAt = &t
	}
	msg := sqlcgen.InsertMessageParams{
		ID: uuid.MustParse(platform.NewID()), AccountID: acct.ID, MailboxID: mb.ID,
		Uid: &uid, Pop3Uidl: nil, Modseq: int64Ptr(int64(env.ModSeq)),
		MessageIDHeader: env.MessageID, ProviderMsgID: env.GMailID, DedupeKey: dedupe,
		InReplyTo: env.InReplyTo, ReferencesList: env.References,
		Subject: env.Subject, FromAddr: fromJSON, ToAddrs: toJSON, CcAddrs: ccJSON, BccAddrs: bccJSON,
		SentAt: sentAt, ReceivedAt: received.UTC(), Size: int64(env.Size),
		Flags: emptyIfNil(env.Flags), Keywords: []string{}, LocalFlag: false, PreservedAt: nil,
		Snippet: "", HasAttachments: false, RawSha256: "", BodyState: "missing", AttachmentsState: "missing",
	}
	var inserted sqlcgen.Message
	err := s.store.WithTx(ctx, func(q *sqlcgen.Queries) error {
		threadID, err := s.resolveThreadQ(ctx, q, acct.ID, env.InReplyTo, env.References, env.Subject, received)
		if err != nil {
			return err
		}
		msg.ThreadID = threadID
		row, err := q.InsertMessage(ctx, msg)
		if err != nil {
			return err
		}
		inserted = row
		if threadID != nil {
			if err := q.IncrementThread(ctx, sqlcgen.IncrementThreadParams{ID: *threadID, LastMessageAt: received}); err != nil {
				return err
			}
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
		"messageId": inserted.ID.String(), "unread": !hasFlag(env.Flags, "\\Seen"),
	})
	s.maybePrefetch(acct, mb, inserted)
	return nil
}

func (s *Service) resolveThreadQ(ctx context.Context, q *sqlcgen.Queries, accountID uuid.UUID, inReplyTo string, refs []string, subject string, received time.Time) (*uuid.UUID, error) {
	candidate := message.NormalizeMessageID(inReplyTo)
	if candidate == "" && len(refs) > 0 {
		candidate = message.NormalizeMessageID(refs[len(refs)-1])
	}
	if candidate != "" {
		parent, err := q.FindMessageByHeader(ctx, sqlcgen.FindMessageByHeaderParams{AccountID: accountID, Lower: candidate})
		if err == nil && parent.ThreadID != nil {
			return parent.ThreadID, nil
		}
	}
	id := uuid.MustParse(platform.NewID())
	_, err := q.UpsertThread(ctx, sqlcgen.UpsertThreadParams{
		ID: id, AccountID: accountID, SubjectNorm: message.SubjectThreadKey(subject),
		LastMessageAt: received, MessageCount: 0,
	})
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Service) maybePrefetch(acct sqlcgen.Account, mb sqlcgen.Mailbox, msg sqlcgen.Message) {
	if acct.DownloadScope == "headers" {
		return
	}
	if acct.DownloadScope == "full_no_attachments" && msg.Size > s.settings.BodyPrefetchMaxBytes() {
		return
	}
	s.enqueueBody(msg.ID, false)
}

func addressMap(addrs []imapclient.Address) map[string]string {
	if len(addrs) == 0 {
		return map[string]string{}
	}
	out := map[string]string{"email": addrs[0].Email}
	if addrs[0].Name != "" {
		out["name"] = addrs[0].Name
	}
	return out
}

func addressList(addrs []imapclient.Address) []map[string]string {
	out := make([]map[string]string, 0, len(addrs))
	for _, a := range addrs {
		m := map[string]string{"email": a.Email}
		if a.Name != "" {
			m["name"] = a.Name
		}
		out = append(out, m)
	}
	return out
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func ptrInt64(v int64) *int64 { return &v }
func int64Ptr(v int64) *int64 { return &v }

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
