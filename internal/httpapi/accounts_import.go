package httpapi

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/emclient"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

const (
	maxEmclientUpload  = 64 << 20
	maxEmclientUnzip   = 128 << 20
	emclientDBFilename = "accounts.dat"
)

func (s *Server) ParseEmclientImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxEmclientUpload)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.error(w, r, http.StatusRequestEntityTooLarge, "upload_too_large", "Upload exceeds the size limit.")
			return
		}
		s.error(w, r, http.StatusBadRequest, "invalid_upload", "Malformed multipart upload.")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_upload", "Missing file field.")
		return
	}
	defer file.Close()

	dir, err := os.MkdirTemp("", "mailquay-emclient-upload-")
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer os.RemoveAll(dir)

	if err := saveEmclientUpload(file, header.Filename, dir); err != nil {
		s.logger.Warn("store emclient upload", "error", err)
		s.error(w, r, http.StatusBadRequest, "invalid_file", "The upload is not a readable eM Client database.")
		return
	}
	accounts, err := emclient.ReadAccounts(r.Context(), dir, emclient.Options{MasterPassword: r.FormValue("masterPassword")})
	if err != nil {
		s.logger.Warn("parse emclient database", "error", err)
		s.error(w, r, http.StatusBadRequest, "invalid_file", "The upload is not a readable eM Client database.")
		return
	}
	preview, err := s.accounts.PrepareEMClientImport(r.Context(), accounts)
	if err != nil {
		s.internal(w, r, err)
		return
	}

	out := oapi.EmclientImportPreview{
		ImportId: preview.ID,
		Accounts: make([]oapi.EmclientImportAccount, 0, len(preview.Accounts)),
	}
	for _, candidate := range preview.Accounts {
		item := oapi.EmclientImportAccount{
			Email:           candidate.Email,
			ReceiveProtocol: oapi.EmclientImportAccountReceiveProtocol(candidate.ReceiveProtocol),
			AuthType:        oapi.EmclientImportAccountAuthType(candidate.AuthType),
			Importable:      candidate.Importable,
			Selected:        candidate.Selected,
		}
		if candidate.IMAPAddr != "" {
			addr := candidate.IMAPAddr
			item.ImapAddr = &addr
		}
		if candidate.POP3Addr != "" {
			addr := candidate.POP3Addr
			item.Pop3Addr = &addr
		}
		if candidate.SMTPAddr != "" {
			addr := candidate.SMTPAddr
			item.SmtpAddr = &addr
		}
		if candidate.Reason != "" {
			reason := candidate.Reason
			item.Reason = &reason
		}
		out.Accounts = append(out.Accounts, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ApplyEmclientImport(w http.ResponseWriter, r *http.Request, importId openapiUUID) {
	var body oapi.EmclientImportRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	result, err := s.accounts.ApplyEMClientImport(r.Context(), importId, body.Emails)
	if err != nil {
		if errors.Is(err, account.ErrImportSessionNotFound) {
			s.error(w, r, http.StatusNotFound, "import_not_found", "The import session expired; upload the file again.")
			return
		}
		s.internal(w, r, err)
		return
	}
	out := oapi.EmclientImportResult{
		Created: result.Created,
		Skipped: result.Skipped,
		Failed:  make([]oapi.EmclientImportFailure, 0, len(result.Failed)),
	}
	for _, failure := range result.Failed {
		out.Failed = append(out.Failed, oapi.EmclientImportFailure{Email: failure.Email, Error: failure.Error})
	}
	writeJSON(w, http.StatusOK, out)
}

// saveEmclientUpload accepts either a raw accounts.dat file or a zip archive
// containing accounts.dat with its optional WAL and shared-memory files.
func saveEmclientUpload(src io.Reader, filename, dir string) error {
	upload := filepath.Join(dir, "upload.bin")
	dst, err := os.OpenFile(upload, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create upload: %w", err)
	}
	written, err := io.Copy(dst, io.LimitReader(src, maxEmclientUpload+1))
	if err != nil {
		dst.Close()
		return fmt.Errorf("store upload: %w", err)
	}
	if written > maxEmclientUpload {
		dst.Close()
		return errors.New("upload exceeds the size limit")
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close upload: %w", err)
	}

	header := make([]byte, 4)
	file, err := os.Open(upload)
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	_, readErr := io.ReadFull(file, header)
	file.Close()
	if readErr == nil && string(header) == "PK\x03\x04" {
		return extractEmclientZip(upload, dir)
	}
	if strings.EqualFold(filepath.Ext(filename), ".zip") {
		return extractEmclientZip(upload, dir)
	}
	if err := os.Rename(upload, filepath.Join(dir, emclientDBFilename)); err != nil {
		return fmt.Errorf("store database: %w", err)
	}
	return nil
}

func extractEmclientZip(path, dir string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()

	var total int64
	found := false
	for _, entry := range reader.File {
		name := filepath.Base(strings.ReplaceAll(entry.Name, "\\", "/"))
		switch name {
		case emclientDBFilename, "accounts.dat-wal", "accounts.dat-shm":
		default:
			continue
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if total+int64(entry.UncompressedSize64) > maxEmclientUnzip {
			return errors.New("zip contents exceed the size limit")
		}
		src, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open zip entry: %w", err)
		}
		dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			src.Close()
			return fmt.Errorf("create zip entry: %w", err)
		}
		written, copyErr := io.Copy(dst, io.LimitReader(src, maxEmclientUnzip-total+1))
		total += written
		if total > maxEmclientUnzip {
			copyErr = errors.New("zip contents exceed the size limit")
		}
		src.Close()
		if closeErr := dst.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return fmt.Errorf("extract zip entry: %w", copyErr)
		}
		if name == emclientDBFilename {
			found = true
		}
	}
	if !found {
		return errors.New("zip does not contain accounts.dat")
	}
	return nil
}
