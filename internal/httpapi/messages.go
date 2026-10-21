package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/netpace"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func (s *Server) ListMessages(w http.ResponseWriter, r *http.Request, params oapi.ListMessagesParams) {
	view := "mailbox"
	if params.View != nil {
		view = string(*params.View)
	}
	cursor, limit := parseCursorLimit(params.Limit, params.Cursor)
	opts := message.ListOptions{View: view, Cursor: cursor, Limit: limit}
	if params.AccountId != nil {
		id, ok := toUUID(*params.AccountId)
		if !ok {
			s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
			return
		}
		opts.AccountID = &id
	}
	if params.MailboxId != nil {
		id, ok := toUUID(*params.MailboxId)
		if !ok {
			s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid mailbox id.")
			return
		}
		opts.MailboxID = &id
	}
	rows, next, err := s.messages.List(r.Context(), opts)
	if err != nil {
		if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unknown view") || strings.Contains(err.Error(), "cursor") {
			s.error(w, r, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		s.internal(w, r, err)
		return
	}
	out := s.mapMessages(r, rows)
	resp := oapi.MessagePage{Messages: out}
	if next != "" {
		resp.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) GetMessage(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	msg, err := s.messages.Get(r.Context(), messageID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	detail := mapMessageDetail(msg)
	atts, err := s.messages.Attachments(r.Context(), messageID)
	if err == nil {
		list := make([]oapi.Attachment, 0, len(atts))
		for _, att := range atts {
			list = append(list, mapAttachment(att))
		}
		detail.Attachments = &list
	}
	s.enrichOne(r, &detail)
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) GetMessageBody(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	force := r.URL.Query().Get("refresh") == "true"
	body, err := s.messages.Body(netpace.WithInteractive(r.Context()), messageID, force)
	if err != nil {
		switch {
		case errors.Is(err, message.ErrNotFound):
			s.error(w, r, http.StatusNotFound, "not_found", "Message not found.")
		case errors.Is(err, message.ErrContentUnavailable):
			s.error(w, r, http.StatusServiceUnavailable, "content_unavailable", "The message body is unavailable.")
		default:
			s.error(w, r, http.StatusServiceUnavailable, "content_unavailable", "The message body could not be fetched.")
		}
		return
	}
	attachments := make([]oapi.Attachment, 0, len(body.Attachments))
	for _, att := range body.Attachments {
		attachments = append(attachments, mapAttachment(att))
	}
	writeJSON(w, http.StatusOK, oapi.MessageBody{
		Html: body.HTML, Text: body.Text, RemoteImagesBlocked: body.RemoteImagesBlocked,
		FetchedFromServer: boolPtr(body.FetchedFromServer), Attachments: attachments,
	})
}

func (s *Server) GetRawMessage(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	msg, err := s.messages.Get(r.Context(), messageID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	raw, err := s.sync.Raw(netpace.WithInteractive(r.Context()), msg)
	if err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Raw message unavailable.")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(msg.Subject) + ".eml"}))
	_, _ = w.Write(raw)
}

func (s *Server) DownloadAttachment(w http.ResponseWriter, r *http.Request, id oapi.MessageID, attachmentId oapi.AttachmentID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	attachmentID, ok := toUUID(attachmentId)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid attachment id.")
		return
	}
	msg, err := s.messages.Get(r.Context(), messageID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	att, err := s.messages.Attachment(r.Context(), messageID, attachmentID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	data, err := s.sync.AttachmentBytes(netpace.WithInteractive(r.Context()), msg, att)
	if err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Attachment content unavailable.")
		return
	}
	writeAttachment(w, att.Filename, att.ContentType, data)
}

func (s *Server) UpdateMessage(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	var body oapi.MessageUpdate
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	var unread *bool
	if body.Unread != nil {
		unread = body.Unread
	}
	msg, err := s.messages.Update(r.Context(), messageID, unread, body.Marked)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapMessageDetail(msg))
}

func (s *Server) MoveMessage(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	var body oapi.MoveMessageRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	mailboxID, ok := toUUID(body.MailboxId)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid mailbox id.")
		return
	}
	if err := s.messages.Move(r.Context(), messageID, mailboxID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.error(w, r, http.StatusBadRequest, "move_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) DeleteMessage(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	if err := s.messages.Delete(r.Context(), messageID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) ArchiveMessage(w http.ResponseWriter, r *http.Request, id oapi.MessageID) {
	messageID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid message id.")
		return
	}
	entry, err := s.messages.Archive(r.Context(), messageID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.error(w, r, http.StatusServiceUnavailable, "archive_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, mapArchive(entry))
}

func (s *Server) BatchMessages(w http.ResponseWriter, r *http.Request) {
	var body oapi.BatchRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	result := oapi.BatchResult{Succeeded: []openapiUUID{}, Failed: []struct {
		Error string      `json:"error"`
		Id    openapiUUID `json:"id"`
	}{}}
	for _, rawID := range body.Ids {
		id, ok := toUUID(rawID)
		if !ok {
			continue
		}
		var err error
		switch body.Action {
		case "read":
			v := false
			_, err = s.messages.Update(r.Context(), id, &v, nil)
		case "unread":
			v := true
			_, err = s.messages.Update(r.Context(), id, &v, nil)
		case "mark":
			v := true
			_, err = s.messages.Update(r.Context(), id, nil, &v)
		case "unmark":
			v := false
			_, err = s.messages.Update(r.Context(), id, nil, &v)
		case "archive":
			_, err = s.messages.Archive(r.Context(), id)
		case "delete":
			err = s.messages.Delete(r.Context(), id)
		case "move":
			if body.MailboxId == nil {
				err = errors.New("mailboxId is required for move")
			} else if mailboxID, ok := toUUID(*body.MailboxId); ok {
				err = s.messages.Move(r.Context(), id, mailboxID)
			} else {
				err = errors.New("invalid mailbox id")
			}
		default:
			err = errors.New("unsupported action")
		}
		if err != nil {
			result.Failed = append(result.Failed, struct {
				Error string      `json:"error"`
				Id    openapiUUID `json:"id"`
			}{Id: rawID, Error: err.Error()})
			continue
		}
		result.Succeeded = append(result.Succeeded, rawID)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) GetThread(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	threadID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid thread id.")
		return
	}
	rows, err := s.store.ListThreadMessages(r.Context(), &threadID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": s.mapMessages(r, rows)})
}

func (s *Server) ListDeleteQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListPendingDeletes(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	items := make([]oapi.DeleteQueueItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, oapi.DeleteQueueItem{
			Id: openapiUUID(row.ID), MessageId: openapiUUID(row.MessageID),
			AccountId: openapiUUID(row.AccountID), MailboxId: openapiUUID(row.MailboxID),
			DeleteAfter: row.DeleteAfter.UTC(), Status: row.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) mapMessages(r *http.Request, rows []sqlcgen.Message) []oapi.MessageSummary {
	accounts, _ := s.store.ListAccounts(r.Context())
	accountMap := make(map[uuid.UUID]sqlcgen.Account, len(accounts))
	for _, acct := range accounts {
		accountMap[acct.ID] = acct
	}
	mailboxMap := make(map[uuid.UUID]sqlcgen.Mailbox)
	out := make([]oapi.MessageSummary, 0, len(rows))
	for _, row := range rows {
		item := mapMessage(row)
		if acct, ok := accountMap[row.AccountID]; ok {
			item.AccountEmail = &acct.Email
			item.AccountName = &acct.Name
		}
		mb, ok := mailboxMap[row.MailboxID]
		if !ok {
			mb, _ = s.store.GetMailbox(r.Context(), row.MailboxID)
			mailboxMap[row.MailboxID] = mb
		}
		if mb.ID != uuid.Nil {
			item.MailboxPath = &mb.Path
			role := string(mb.Role)
			item.MailboxRole = &role
		}
		out = append(out, item)
	}
	return out
}

func (s *Server) enrichOne(r *http.Request, detail *oapi.MessageDetail) {}

func writeAttachment(w http.ResponseWriter, filename, contentType string, data []byte) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(filename)}))
	w.Header().Set("Content-Length", intToString(len(data)))
	_, _ = w.Write(data)
}

func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.TrimSpace(name)
	if name == "" {
		return "attachment"
	}
	return name
}

func intToString(v int) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

var _ = base64.RawURLEncoding
