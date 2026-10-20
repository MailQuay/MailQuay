package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mime"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

type pulledDraftAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

func (s *Service) pullDrafts(ctx context.Context, client *imapclient.Client, acct sqlcgen.Account, box sqlcgen.Mailbox) error {
	selected, err := client.Select(ctx, box.Path)
	if err != nil {
		return err
	}
	uids, err := client.UIDSearchNew(ctx, 1)
	if err != nil {
		return err
	}
	if len(uids) > 200 {
		uids = uids[len(uids)-200:]
	}
	existing, err := s.store.ListDrafts(ctx, sqlcgen.ListDraftsParams{AccountID: &acct.ID, PageLimit: 500})
	if err != nil {
		return err
	}
	known := make(map[int64]struct{}, len(existing))
	for _, draft := range existing {
		if draft.RemoteUid != nil {
			known[*draft.RemoteUid] = struct{}{}
		}
	}
	identity, err := s.store.GetDefaultIdentity(ctx, acct.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if identity.Email == "" {
		identity.Email = acct.Email
		identity.Name = acct.Name
	}
	for _, uid := range uids {
		if _, ok := known[int64(uid)]; ok {
			continue
		}
		raw, err := client.FetchRaw(ctx, uid)
		if err != nil {
			s.logger.Debug("fetch remote draft", "uid", uid, "error", err)
			continue
		}
		parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: s.settings.MaxAttachmentBytes(), KeepAttachmentData: true})
		if err != nil {
			continue
		}
		headers := map[string]any{
			"to": addressStrings(parsed.To), "cc": addressStrings(parsed.Cc),
			"bcc": addressStrings(parsed.Bcc), "subject": parsed.Subject,
		}
		if len(parsed.References) > 0 {
			headers["references"] = parsed.References
		}
		headerRaw, _ := json.Marshal(headers)
		atts := make([]pulledDraftAttachment, 0, len(parsed.Attachments))
		for _, att := range parsed.Attachments {
			if len(att.Data) == 0 {
				continue
			}
			sha, err := s.storage.PutBlob(att.Data)
			if err != nil {
				continue
			}
			attID := uuid.MustParse(platform.NewID())
			if _, err := s.store.CreateUploadedAttachment(ctx, sqlcgen.CreateUploadedAttachmentParams{
				ID: attID, Filename: att.Filename, ContentType: att.ContentType,
				Size: int64(len(att.Data)), Sha256: sha,
			}); err != nil {
				continue
			}
			atts = append(atts, pulledDraftAttachment{
				ID: attID.String(), Filename: att.Filename, ContentType: att.ContentType,
				Size: int64(len(att.Data)), SHA256: sha,
			})
		}
		attRaw, _ := json.Marshal(atts)
		remoteUID := int64(uid)
		uidValidity := int64(selected.UIDValidity)
		_, err = s.store.CreateDraft(ctx, sqlcgen.CreateDraftParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: acct.ID,
			IdentityAccountID: acct.ID, IdentityEmail: identity.Email,
			InReplyTo: parsed.InReplyTo, Headers: headerRaw, BodyText: parsed.Text, BodyHtml: parsed.HTML,
			Attachments: attRaw, Revision: 1, ContentHash: "", Origin: "pull",
			RemoteUid: &remoteUID, RemoteUidvalidity: &uidValidity,
			SyncState: "pulled", Status: "editing",
		})
		if err != nil {
			s.logger.Debug("import remote draft", "uid", uid, "error", err)
		}
	}
	return nil
}

func addressStrings(addrs []mime.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Name != "" {
			out = append(out, a.Name+" <"+a.Email+">")
		} else if a.Email != "" {
			out = append(out, a.Email)
		}
	}
	return out
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace
var _ = time.Now
