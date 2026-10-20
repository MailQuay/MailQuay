package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/search"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func archiveSummary(row sqlcgen.LocalArchive) oapi.MessageSummary {
	origin := ""
	if row.OriginAccountID != nil {
		origin = row.OriginAccountID.String()
	}
	accountUUID, _ := uuid.Parse(origin)
	mailboxUUID := uuid.Nil
	summary := oapi.MessageSummary{
		Id: openapiUUID(row.ID), AccountId: openapiUUID(accountUUID), MailboxId: openapiUUID(mailboxUUID),
		Subject: row.Subject, ReceivedAt: row.ReceivedAt.UTC(), Size: int(row.Size),
		Unread: false, Marked: false, IsArchive: boolPtr(true), HasAttachments: row.HasAttachments, BodyState: "cached",
		Snippet: strPtr(""), Preserved: boolPtr(true), AccountEmail: &row.OriginAccountEmail,
		MailboxPath: strPtr(row.OriginMailboxPath), MailboxRole: strPtr("archive"),
	}
	if len(row.FromAddr) > 0 {
		var addr oapi.Address
		if json.Unmarshal(row.FromAddr, &addr) == nil && addr.Email != "" {
			summary.From = &addr
		}
	}
	summary.To = decodeAddresses(row.ToAddrs)
	summary.Cc = decodeAddresses(row.CcAddrs)
	return summary
}

func (s *Server) SearchMessages(w http.ResponseWriter, r *http.Request, params oapi.SearchMessagesParams) {
	if s.index == nil {
		s.error(w, r, http.StatusServiceUnavailable, "search_unavailable", "The search index is not available.")
		return
	}
	cursor, limit := parseCursorLimit(params.Limit, params.Cursor)
	q := search.Query{Limit: limit, Cursor: cursor}
	if params.Q != nil {
		q.Text = *params.Q
	}
	if params.Mode != nil {
		q.Mode = string(*params.Mode)
	}
	if params.AccountId != nil {
		q.AccountID = params.AccountId.String()
	}
	if params.MailboxId != nil {
		q.MailboxID = params.MailboxId.String()
	}
	if params.Recipient != nil {
		q.Recipient = *params.Recipient
	}
	if params.Sender != nil {
		q.Sender = *params.Sender
	}
	q.HasAttachment = params.HasAttachment
	result, err := s.index.Search(r.Context(), q)
	if err != nil {
		if errors.Is(err, search.ErrInvalidCursor) {
			s.error(w, r, http.StatusBadRequest, "invalid_cursor", "The search cursor is invalid.")
			return
		}
		if errors.Is(err, search.ErrInvalidQuery) {
			s.error(w, r, http.StatusBadRequest, "invalid_query", "The search query is invalid.")
			return
		}
		s.error(w, r, http.StatusServiceUnavailable, "search_unavailable", "The search index is unavailable.")
		return
	}
	ids := make([]uuid.UUID, 0, len(result.Hits))
	rewrites := make(map[uuid.UUID]search.Hit)
	for _, hit := range result.Hits {
		id, err := uuid.Parse(hit.ID)
		if err != nil {
			continue
		}
		ids = append(ids, id)
		rewrites[id] = hit
	}
	rows, err := s.store.ListMessagesByIDs(r.Context(), ids)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	byID := make(map[uuid.UUID]sqlcgen.Message, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]oapi.MessageSummary, 0, len(ids))
	seen := make(map[string]struct{})
	for _, id := range ids {
		hit := rewrites[id]
		if hit.IsArchive {
			entry, err := s.store.GetArchive(r.Context(), id)
			if err != nil {
				continue
			}
			out = append(out, archiveSummary(entry))
			continue
		}
		found, ok := byID[id]
		if !ok {
			continue
		}
		key := found.AccountID.String() + ":" + found.DedupeKey
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s.mapMessages(r, []sqlcgen.Message{found})[0])
	}
	page := oapi.MessagePage{Messages: out}
	if result.NextCursor != "" {
		next := result.NextCursor
		page.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, page)
}
