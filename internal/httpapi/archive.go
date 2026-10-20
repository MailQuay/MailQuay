package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/message"
	mailmime "github.com/mailquay/mailquay/internal/mime"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func (s *Server) ListArchives(w http.ResponseWriter, r *http.Request, params oapi.ListArchivesParams) {
	cursor, limit := parseCursorLimit(params.Limit, params.Cursor)
	received, id, err := message.DecodeCursor(cursor)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	var cursorTime *time.Time
	var cursorID *uuid.UUID
	if !received.IsZero() {
		cursorTime = &received
		cursorID = &id
	}
	paramsSQL := sqlcgen.ListArchivesParams{PageLimit: int32(limit + 1), CursorArchived: cursorTime, CursorID: cursorID}
	rows, err := s.store.ListArchives(r.Context(), paramsSQL)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = message.EncodeCursor(last.ArchivedAt, last.ID)
	}
	items := make([]oapi.ArchiveEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, mapArchive(row))
	}
	resp := oapi.ArchivePage{Archives: items}
	if next != "" {
		resp.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) GetArchive(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	archiveID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid archive id.")
		return
	}
	row, err := s.store.GetArchive(r.Context(), archiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.error(w, r, http.StatusNotFound, "not_found", "Archive not found.")
			return
		}
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapArchive(row))
}

func (s *Server) GetArchiveRaw(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	row, ok := s.getArchive(r, w, id)
	if !ok {
		return
	}
	data, err := s.readArchiveRaw(row.RawSha256)
	if err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Archive content unavailable.")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(row.Subject) + ".eml"}))
	_, _ = w.Write(data)
}

func (s *Server) DownloadArchiveAttachment(w http.ResponseWriter, r *http.Request, id openapiUUID, attachmentId oapi.AttachmentID) {
	row, ok := s.getArchive(r, w, id)
	if !ok {
		return
	}
	attachmentID, ok := toUUID(attachmentId)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid attachment id.")
		return
	}
	raw, err := s.readArchiveRaw(row.RawSha256)
	if err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Archive content unavailable.")
		return
	}
	parsed, err := mailmime.Parse(raw, mailmime.Options{MaxAttachmentBytes: 25 << 20, KeepAttachmentData: true})
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "parse_failed", "Archive could not be parsed.")
		return
	}
	for _, att := range parsed.Attachments {
		candidate := uuid.NewSHA1(uuid.NameSpaceOID, []byte(row.RawSha256+att.PartPath))
		if candidate == attachmentID {
			writeAttachment(w, att.Filename, att.ContentType, att.Data)
			return
		}
	}
	s.error(w, r, http.StatusNotFound, "not_found", "Attachment not found.")
}

func (s *Server) getArchive(r *http.Request, w http.ResponseWriter, id openapiUUID) (sqlcgen.LocalArchive, bool) {
	archiveID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid archive id.")
		return sqlcgen.LocalArchive{}, false
	}
	row, err := s.store.GetArchive(r.Context(), archiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.error(w, r, http.StatusNotFound, "not_found", "Archive not found.")
			return sqlcgen.LocalArchive{}, false
		}
		s.internal(w, r, err)
		return sqlcgen.LocalArchive{}, false
	}
	return row, true
}

func (s *Server) readArchiveRaw(sha string) ([]byte, error) {
	reader, err := s.blobs.OpenBlob(sha)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return readLimited(reader, 64<<20)
}

func (s *Server) GetArchiveBody(w http.ResponseWriter, r *http.Request, id openapiUUID) {
	row, ok := s.getArchive(r, w, id)
	if !ok {
		return
	}
	raw, err := s.readArchiveRaw(row.RawSha256)
	if err != nil {
		s.error(w, r, http.StatusNotFound, "not_found", "Archive content unavailable.")
		return
	}
	parsed, err := mailmime.Parse(raw, mailmime.Options{MaxAttachmentBytes: s.settings.MaxAttachmentBytes()})
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "parse_failed", "Archive could not be parsed.")
		return
	}
	attachments := make([]oapi.Attachment, 0, len(parsed.Attachments))
	for _, att := range parsed.Attachments {
		attID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(row.RawSha256+att.PartPath))
		attachments = append(attachments, oapi.Attachment{
			Id: openapiUUID(attID), Filename: att.Filename, ContentType: att.ContentType,
			Size: int(att.Size), Disposition: &att.Disposition, ContentId: &att.ContentID,
		})
	}
	writeJSON(w, http.StatusOK, oapi.MessageBody{
		Html: parsed.HTML, Text: parsed.Text, RemoteImagesBlocked: containsRemoteImage(parsed.HTML),
		FetchedFromServer: boolPtr(false), Attachments: attachments,
	})
}

func containsRemoteImage(html string) bool {
	return strings.Contains(html, "data-src=")
}
