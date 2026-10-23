package compose

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// PushDrafts mirrors recently edited drafts for accounts in mirror mode.
func (s *Service) PushDrafts(ctx context.Context) {
	snap, err := s.settings.Get(ctx)
	if err != nil || snap.Values.DraftSyncMode != "mirror" {
		return
	}
	delay := time.Duration(snap.Values.DraftPushDelaySeconds) * time.Second
	drafts, err := s.store.ListDraftsToPush(ctx, time.Now().UTC().Add(-delay))
	if err != nil {
		return
	}
	for _, draft := range drafts {
		if err := s.pushDraft(ctx, draft); err != nil {
			s.logger.Debug("push draft", "draft", draft.ID, "error", err)
		}
	}
}

func (s *Service) pushDraft(ctx context.Context, draft sqlcgen.Draft) error {
	if draft.Status == "sent" || draft.SyncState == "conflict" {
		return nil
	}
	acct, err := s.accounts.Get(ctx, draft.AccountID)
	if err != nil {
		return err
	}
	if acct.ReceiveProtocol != "imap" || acct.ImapAddr == nil {
		return nil
	}
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		return err
	}
	draftsBox, err := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: acct.ID, Role: "drafts"})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		draftsBox, err = s.store.UpsertMailbox(ctx, sqlcgen.UpsertMailboxParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: acct.ID, Path: "Drafts", Name: "Drafts",
			Delimiter: "/", Role: "drafts", Attributes: []byte("[]"), LocalOnly: false,
			SyncMode: "off", Uidvalidity: nil, Uidnext: nil, Highestmodseq: nil,
		})
		if err != nil {
			return err
		}
	}
	msgID := "<" + draft.ID.String() + "@" + messageDomain(acct.Email) + ">"
	raw, err := s.buildDraftMIME(ctx, acct, draft, msgID)
	if err != nil {
		return err
	}
	var uid uint32
	err = s.sessions.Do(ctx, acct.ID, false, s.imapDialer(acct, creds), func(ctx context.Context, client *imapclient.Client) error {
		appended, err := client.Append(ctx, draftsBox.Path, []string{"\\Draft"}, time.Now().UTC(), raw)
		if err != nil {
			return fmt.Errorf("append draft: %w", err)
		}
		uid = appended
		if draft.RemoteUid != nil {
			if _, err := client.Select(ctx, draftsBox.Path); err == nil {
				old := uint32(*draft.RemoteUid)
				if err := client.StoreFlags(ctx, old, []string{"\\Deleted"}, true); err == nil {
					_ = client.Expunge(ctx, []uint32{old})
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var uidValidity *int64
	if draftsBox.Uidvalidity != nil {
		uidValidity = draftsBox.Uidvalidity
	}
	remoteUID := int64(uid)
	_ = s.store.SetDraftRemote(ctx, sqlcgen.SetDraftRemoteParams{
		ID: draft.ID, RemoteUid: &remoteUID, RemoteUidvalidity: uidValidity, SyncState: "pushed",
	})
	return nil
}

func messageDomain(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[1]
	}
	return "mailquay.local"
}
