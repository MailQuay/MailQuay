package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/compose"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

func (s *Server) ListDrafts(w http.ResponseWriter, r *http.Request, params oapi.ListDraftsParams) {
	var accountID *uuid.UUID
	if params.AccountId != nil {
		if id, ok := toUUID(*params.AccountId); ok {
			accountID = &id
		}
	}
	rows, err := s.compose.List(r.Context(), accountID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]oapi.Draft, 0, len(rows))
	for _, row := range rows {
		out = append(out, mapDraft(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": out})
}

func (s *Server) CreateDraft(w http.ResponseWriter, r *http.Request) {
	var body oapi.DraftInput
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	in, err := draftInput(body, nil)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_draft", err.Error())
		return
	}
	row, err := s.compose.Create(r.Context(), in)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_draft", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, mapDraft(row))
}

func (s *Server) GetDraft(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	draftID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid draft id.")
		return
	}
	row, err := s.compose.Get(r.Context(), draftID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapDraft(row))
}

func (s *Server) UpdateDraft(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	draftID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid draft id.")
		return
	}
	var body oapi.DraftUpdateRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	base := oapi.DraftInput{
		AttachmentIds: body.AttachmentIds, Bcc: body.Bcc, BodyHtml: body.BodyHtml, BodyText: body.BodyText,
		Cc: body.Cc, IdentityAccountId: body.IdentityAccountId, IdentityEmail: body.IdentityEmail,
		InReplyTo: body.InReplyTo, Subject: body.Subject, To: body.To,
	}
	in, err := draftInput(base, &body.Revision)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_draft", err.Error())
		return
	}
	row, err := s.compose.Update(r.Context(), draftID, in)
	if err != nil {
		switch {
		case errors.Is(err, compose.ErrRevisionConflict):
			s.error(w, r, http.StatusConflict, "revision_conflict", "The draft changed in another window.")
		case errors.Is(err, compose.ErrNotFound):
			s.error(w, r, http.StatusNotFound, "not_found", "Draft not found.")
		default:
			s.error(w, r, http.StatusBadRequest, "invalid_draft", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, mapDraft(row))
}

func (s *Server) DeleteDraft(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	draftID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid draft id.")
		return
	}
	if err := s.compose.Delete(r.Context(), draftID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) SyncDraft(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	draftID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid draft id.")
		return
	}
	if err := s.compose.SyncDraft(r.Context(), draftID); err != nil {
		s.error(w, r, http.StatusBadGateway, "draft_sync_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) SendMessage(w http.ResponseWriter, r *http.Request) {
	var body oapi.SendRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	draftID, ok := toUUID(body.DraftId)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid draft id.")
		return
	}
	identityAccountID, ok := toUUID(body.IdentityAccountId)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid identity account id.")
		return
	}
	out, err := s.compose.Send(r.Context(), compose.SendInput{
		DraftID: draftID, IdentityAccountID: identityAccountID, IdentityEmail: body.IdentityEmail,
		ScheduledAt: body.ScheduledAt,
	})
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "send_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, mapOutbox(out))
}

func (s *Server) UploadAttachment(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(26 << 20); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_upload", "Malformed multipart upload.")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_upload", "Missing file field.")
		return
	}
	defer file.Close()
	limit := s.settings.MaxAttachmentBytes()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		s.error(w, r, http.StatusRequestEntityTooLarge, "attachment_too_large", "Attachment exceeds the configured limit.")
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	row, err := s.compose.Upload(r.Context(), header.Filename, contentType, data)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "upload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, oapi.UploadedAttachment{
		Id: openapiUUID(row.ID), Filename: row.Filename, ContentType: row.ContentType, Size: int(row.Size),
	})
}

func (s *Server) ListOutbox(w http.ResponseWriter, r *http.Request) {
	rows, err := s.compose.ListOutbox(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	items := make([]oapi.OutboxItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, mapOutbox(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) RescheduleOutbox(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	outboxID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid outbox id.")
		return
	}
	var body oapi.RescheduleRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	if body.ScheduledAt.Before(time.Now().UTC()) {
		s.error(w, r, http.StatusBadRequest, "invalid_schedule", "scheduledAt must be in the future.")
		return
	}
	row, err := s.compose.Reschedule(r.Context(), outboxID, body.ScheduledAt.UTC())
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.error(w, r, http.StatusBadRequest, "reschedule_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mapOutbox(row))
}

func (s *Server) RetryOutbox(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	outboxID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid outbox id.")
		return
	}
	if err := s.compose.Retry(r.Context(), outboxID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) CancelOutbox(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	outboxID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid outbox id.")
		return
	}
	if err := s.compose.Cancel(r.Context(), outboxID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func draftInput(body oapi.DraftInput, revision *int) (compose.DraftInput, error) {
	accountID, ok := toUUID(body.IdentityAccountId)
	if !ok {
		return compose.DraftInput{}, errors.New("invalid identityAccountId")
	}
	in := compose.DraftInput{
		IdentityAccountID: accountID, IdentityEmail: strings.ToLower(strings.TrimSpace(body.IdentityEmail)),
		InReplyTo: derefString(body.InReplyTo), Subject: body.Subject,
		BodyText: body.BodyText, BodyHTML: body.BodyHtml,
	}
	if body.To != nil {
		in.To = *body.To
	}
	if body.Cc != nil {
		in.Cc = *body.Cc
	}
	if body.Bcc != nil {
		in.Bcc = *body.Bcc
	}
	if body.AttachmentIds != nil {
		for _, raw := range *body.AttachmentIds {
			if id, ok := toUUID(raw); ok {
				in.AttachmentIDs = append(in.AttachmentIDs, id)
			}
		}
	}
	if revision != nil {
		in.Revision = int32(*revision)
	}
	return in, nil
}
