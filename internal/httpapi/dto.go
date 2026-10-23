package httpapi

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func mapUser(u sqlcgen.User) oapi.User {
	return oapi.User{
		Id: openapi_types.UUID(u.ID), Username: u.Username, DisplayName: u.DisplayName,
		Locale: u.Locale, Theme: u.Theme,
	}
}

func mapAccount(a sqlcgen.Account) oapi.Account {
	state := "disconnected"
	switch a.SyncState {
	case "connected", "syncing", "error":
		state = a.SyncState
	}
	out := oapi.Account{
		Id: openapi_types.UUID(a.ID), Name: a.Name, Email: a.Email, Provider: a.Provider,
		Username: a.Username, AuthType: oapi.AccountAuthType(a.AuthType),
		HasPassword: boolPtr(len(a.PasswordEnc) > 0), HasOAuth: boolPtr(len(a.OauthRefreshEnc) > 0),
		ReceiveProtocol:  oapi.AccountReceiveProtocol(a.ReceiveProtocol),
		ImapSecurity:     secPtr(oapi.AccountImapSecurity(a.ImapSecurity)),
		Pop3Security:     secPtr(oapi.AccountPop3Security(a.Pop3Security)),
		SmtpSecurity:     secPtr(oapi.AccountSmtpSecurity(a.SmtpSecurity)),
		SmtpUsername:     a.SmtpUsername,
		HasSmtpPassword:  boolPtr(len(a.SmtpPasswordEnc) > 0),
		SmtpHeloHostname: &a.SmtpHeloHostname, SyncEnabled: a.SyncEnabled,
		InitialSyncRange:  oapi.AccountInitialSyncRange(a.InitialSyncRange),
		DownloadScope:     oapi.AccountDownloadScope(a.DownloadScope),
		SentCopyMode:      oapi.AccountSentCopyMode(a.SentCopyMode),
		Pop3LeaveOnServer: boolPtr(a.Pop3LeaveOnServer), Pop3DeleteOnServer: boolPtr(a.Pop3DeleteOnServer),
		AutoDetectSpecialFolders: boolPtr(a.AutoDetectSpecialFolders),
		SpecialFolders:           mapSpecialFolders(a.SpecialFolders),
		State:                    oapi.AccountState(state), LastError: strPtr(a.LastError), CreatedAt: a.CreatedAt.UTC(),
		DefaultSendName: a.DefaultSendName, DefaultReplyTo: a.DefaultReplyTo,
	}
	if a.OauthProvider != nil {
		out.OauthProvider = a.OauthProvider
	}
	if a.ImapAddr != nil {
		out.ImapAddr = a.ImapAddr
	}
	if a.Pop3Addr != nil {
		out.Pop3Addr = a.Pop3Addr
	}
	if a.SmtpAddr != nil {
		out.SmtpAddr = a.SmtpAddr
	}
	if a.InitialSyncCutoffAt != nil {
		t := a.InitialSyncCutoffAt.UTC()
		out.InitialSyncCutoffAt = &t
	}
	return out
}

func mapMailbox(m sqlcgen.Mailbox) oapi.Mailbox {
	attrs := []string{}
	_ = json.Unmarshal(m.Attributes, &attrs)
	out := oapi.Mailbox{
		Id: openapi_types.UUID(m.ID), AccountId: openapi_types.UUID(m.AccountID),
		Path: m.Path, Name: m.Name, Delimiter: strPtr(m.Delimiter),
		Role: oapi.MailboxRole(m.Role), Attributes: &attrs, LocalOnly: m.LocalOnly,
		SyncMode: oapi.MailboxSyncMode(m.SyncMode), Total: int(m.Total), Unread: int(m.Unread),
		SyncState: oapi.MailboxSyncState(m.SyncState), LastError: strPtr(m.LastError),
	}
	if m.Uidvalidity != nil {
		v := int(*m.Uidvalidity)
		out.Uidvalidity = &v
	}
	if m.Uidnext != nil {
		v := int(*m.Uidnext)
		out.Uidnext = &v
	}
	if m.LastSyncedAt != nil {
		t := m.LastSyncedAt.UTC()
		out.LastSyncedAt = &t
	}
	return out
}

func mapMessage(m sqlcgen.Message) oapi.MessageSummary {
	out := oapi.MessageSummary{
		Id: openapi_types.UUID(m.ID), AccountId: openapi_types.UUID(m.AccountID),
		MailboxId: openapi_types.UUID(m.MailboxID), Subject: m.Subject,
		ReceivedAt: m.ReceivedAt.UTC(), Size: int(m.Size),
		Unread: !hasFlag(m.Flags, "\\Seen"), Marked: m.LocalFlag,
		IsArchive: boolPtr(false), HasAttachments: m.HasAttachments, BodyState: m.BodyState,
		AttachmentsState: strPtr(m.AttachmentsState), Preserved: boolPtr(m.PreservedAt != nil),
		Snippet: strPtr(m.Snippet), AccountName: nil, AccountEmail: nil,
	}
	if m.ThreadID != nil {
		id := openapi_types.UUID(*m.ThreadID)
		out.ThreadId = &id
	}
	if len(m.FromAddr) > 0 {
		var addr oapi.Address
		if json.Unmarshal(m.FromAddr, &addr) == nil && addr.Email != "" {
			out.From = &addr
		}
	}
	out.To = decodeAddresses(m.ToAddrs)
	out.Cc = decodeAddresses(m.CcAddrs)
	if m.SentAt != nil {
		t := m.SentAt.UTC()
		out.SentAt = &t
	}
	return out
}

func mapMessageDetail(m sqlcgen.Message) oapi.MessageDetail {
	summary := mapMessage(m)
	detail := oapi.MessageDetail{
		Id: summary.Id, AccountId: summary.AccountId, MailboxId: summary.MailboxId,
		ThreadId: summary.ThreadId, Subject: summary.Subject, From: summary.From,
		To: summary.To, Cc: summary.Cc, SentAt: summary.SentAt, ReceivedAt: summary.ReceivedAt,
		Size: summary.Size, Unread: summary.Unread, Marked: summary.Marked,
		HasAttachments: summary.HasAttachments, BodyState: summary.BodyState,
		AttachmentsState: summary.AttachmentsState, Preserved: summary.Preserved,
		Snippet: summary.Snippet, AccountEmail: summary.AccountEmail, AccountName: summary.AccountName,
		MailboxPath: summary.MailboxPath, MailboxRole: summary.MailboxRole,
		MessageIdHeader: strPtr(m.MessageIDHeader), InReplyTo: strPtr(m.InReplyTo),
	}
	refs := m.ReferencesList
	if refs == nil {
		refs = []string{}
	}
	detail.References = &refs
	flags := m.Flags
	if flags == nil {
		flags = []string{}
	}
	detail.Flags = &flags
	keywords := m.Keywords
	if keywords == nil {
		keywords = []string{}
	}
	detail.Keywords = &keywords
	detail.Bcc = decodeAddresses(m.BccAddrs)
	return detail
}

func mapAttachment(a sqlcgen.Attachment) oapi.Attachment {
	return oapi.Attachment{
		Id: openapi_types.UUID(a.ID), Filename: a.Filename, ContentType: a.ContentType,
		Size: int(a.Size), Disposition: strPtr(a.Disposition), ContentId: strPtr(a.ContentID),
	}
}

func mapDraft(d sqlcgen.Draft) oapi.Draft {
	var headers struct {
		To      []string `json:"to"`
		Cc      []string `json:"cc"`
		Bcc     []string `json:"bcc"`
		ReplyTo string   `json:"replyTo"`
		Subject string   `json:"subject"`
	}
	_ = json.Unmarshal(d.Headers, &headers)
	var attachments []oapi.Attachment
	if len(d.Attachments) > 0 {
		var stored []struct {
			ID          string `json:"id"`
			Filename    string `json:"filename"`
			ContentType string `json:"contentType"`
			Size        int    `json:"size"`
		}
		if json.Unmarshal(d.Attachments, &stored) == nil {
			attachments = make([]oapi.Attachment, 0, len(stored))
			for _, a := range stored {
				id, _ := uuid.Parse(a.ID)
				attachments = append(attachments, oapi.Attachment{
					Id: openapi_types.UUID(id), Filename: a.Filename, ContentType: a.ContentType, Size: a.Size,
				})
			}
		}
	}
	out := oapi.Draft{
		Id: openapi_types.UUID(d.ID), AccountId: openapi_types.UUID(d.AccountID),
		InReplyTo: strPtr(d.InReplyTo), ReplyTo: strPtr(headers.ReplyTo),
		To: &headers.To, Cc: &headers.Cc, Bcc: &headers.Bcc,
		Subject: headers.Subject, BodyText: d.BodyText, BodyHtml: d.BodyHtml,
		Revision: int(d.Revision), ContentHash: strPtr(d.ContentHash),
		Status: oapi.DraftStatus(d.Status), SyncState: d.SyncState,
		LastError: strPtr(d.LastError), UpdatedAt: d.UpdatedAt.UTC(),
	}
	if attachments != nil {
		out.Attachments = &attachments
	}
	return out
}

func mapOutbox(o sqlcgen.Outbox) oapi.OutboxItem {
	out := oapi.OutboxItem{
		Id: openapi_types.UUID(o.ID), AccountId: openapi_types.UUID(o.AccountID),
		DraftId: openapi_types.UUID(o.DraftID), MessageIdHeader: strPtr(o.MessageIDHeader),
		Status: oapi.OutboxItemStatus(o.Status), Attempts: int(o.Attempts), LastError: strPtr(o.LastError),
	}
	if o.ScheduledAt != nil {
		t := o.ScheduledAt.UTC()
		out.ScheduledAt = &t
	}
	if o.SentAt != nil {
		t := o.SentAt.UTC()
		out.SentAt = &t
	}
	return out
}

func mapTask(t sqlcgen.Task) oapi.Task {
	out := oapi.Task{
		Id: openapi_types.UUID(t.ID), Type: t.Type, Status: oapi.TaskStatus(t.Status),
		Done: int(t.Done), Total: int(t.Total), Unit: t.Unit,
		Message: strPtr(t.Message), Error: strPtr(t.Error), UpdatedAt: t.UpdatedAt.UTC(),
	}
	if t.AccountID != nil {
		id := openapi_types.UUID(*t.AccountID)
		out.AccountId = &id
	}
	if t.StartedAt != nil {
		v := t.StartedAt.UTC()
		out.StartedAt = &v
	}
	if t.CompletedAt != nil {
		v := t.CompletedAt.UTC()
		out.CompletedAt = &v
	}
	return out
}

func mapArchive(a sqlcgen.LocalArchive) oapi.ArchiveEntry {
	out := oapi.ArchiveEntry{
		Id: openapi_types.UUID(a.ID), OriginAccountEmail: a.OriginAccountEmail,
		OriginMailboxPath: strPtr(a.OriginMailboxPath), MessageIdHeader: strPtr(a.MessageIDHeader),
		Subject: a.Subject, ReceivedAt: a.ReceivedAt.UTC(), Size: int(a.Size),
		HasAttachments: a.HasAttachments, ArchivedAt: a.ArchivedAt.UTC(),
	}
	if a.OriginAccountID != nil {
		id := openapi_types.UUID(*a.OriginAccountID)
		out.OriginAccountId = &id
	}
	if len(a.FromAddr) > 0 {
		var addr oapi.Address
		if json.Unmarshal(a.FromAddr, &addr) == nil && addr.Email != "" {
			out.From = &addr
		}
	}
	out.To = decodeAddresses(a.ToAddrs)
	out.Cc = decodeAddresses(a.CcAddrs)
	out.Bcc = decodeAddresses(a.BccAddrs)
	if a.SentAt != nil {
		t := a.SentAt.UTC()
		out.SentAt = &t
	}
	atts := []oapi.Attachment{}
	if len(a.Attachments) > 0 {
		var stored []struct {
			ID          string `json:"id"`
			Filename    string `json:"filename"`
			ContentType string `json:"contentType"`
			Size        int    `json:"size"`
		}
		if json.Unmarshal(a.Attachments, &stored) == nil {
			for _, item := range stored {
				id, _ := uuid.Parse(item.ID)
				atts = append(atts, oapi.Attachment{
					Id: openapi_types.UUID(id), Filename: item.Filename, ContentType: item.ContentType, Size: item.Size,
				})
			}
		}
	}
	out.Attachments = &atts
	return out
}

func decodeAddresses(raw json.RawMessage) *[]oapi.Address {
	var addrs []oapi.Address
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &addrs)
	}
	if addrs == nil {
		addrs = []oapi.Address{}
	}
	return &addrs
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func boolPtr(v bool) *bool { return &v }

func secPtr[T ~string](v T) *T { return &v }

func derefEnum[T ~string](v *T) string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func intPtr(v int) *int { return &v }

func parseCursorLimit(limit *int, cursor *string) (string, int) {
	l := 50
	if limit != nil && *limit > 0 {
		l = *limit
	}
	if l > 200 {
		l = 200
	}
	c := ""
	if cursor != nil {
		c = *cursor
	}
	return c, l
}

var _ = strconv.Itoa
var _ = time.Now

func mapSpecialFolders(raw []byte) *oapi.SpecialFolders {
	values := mailbox.ParseSpecialFolders(raw)
	out := &oapi.SpecialFolders{}
	if value, ok := values["sent"]; ok {
		out.Sent = &value
	}
	if value, ok := values["drafts"]; ok {
		out.Drafts = &value
	}
	if value, ok := values["trash"]; ok {
		out.Trash = &value
	}
	if value, ok := values["junk"]; ok {
		out.Junk = &value
	}
	if value, ok := values["archive"]; ok {
		out.Archive = &value
	}
	return out
}
