package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/auth"
	"github.com/mailquay/mailquay/internal/compose"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/identity"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/search"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
	syncservice "github.com/mailquay/mailquay/internal/sync"
)

// Server implements the generated HTTP interface.
type Server struct {
	store     *store.Store
	cfg       *config.Config
	auth      *auth.Service
	accounts  *account.Service
	ids       *identity.Service
	mailboxes *mailbox.Service
	messages  *message.Service
	compose   *compose.Service
	settings  *settings.Service
	sync      *syncservice.Service
	index     *search.Index
	hub       *events.Hub
	blobs     *storage.Store
	logger    *slog.Logger
	oauth     *OAuth
	cipher    *crypto.Cipher
}

// NewServer builds the HTTP server.
func NewServer(st *store.Store, cfg *config.Config, authSvc *auth.Service, accounts *account.Service,
	ids *identity.Service, mailboxes *mailbox.Service, messages *message.Service, composeSvc *compose.Service,
	settingsSvc *settings.Service, syncSvc *syncservice.Service, index *search.Index, hub *events.Hub,
	blobs *storage.Store, logger *slog.Logger, cipher *crypto.Cipher) *Server {
	return &Server{
		store: st, cfg: cfg, auth: authSvc, accounts: accounts, ids: ids, mailboxes: mailboxes,
		messages: messages, compose: composeSvc, settings: settingsSvc, sync: syncSvc,
		index: index, hub: hub, blobs: blobs, logger: logger, cipher: cipher,
		oauth: NewOAuth(settingsSvc, st, accounts, cipher),
	}
}

type openapiUUID = openapi_types.UUID

type ctxKey int

const userKey ctxKey = 1

// WithUser stores the authenticated session in a request context.
func WithUser(ctx context.Context, sess *auth.Session) context.Context {
	return context.WithValue(ctx, userKey, sess)
}

// UserFrom returns the authenticated session, if any.
func UserFrom(ctx context.Context) (*auth.Session, bool) {
	sess, ok := ctx.Value(userKey).(*auth.Session)
	return sess, ok
}

func (s *Server) user(ctx context.Context) (sqlcgen.User, bool) {
	sess, ok := UserFrom(ctx)
	if !ok || sess == nil {
		return sqlcgen.User{}, false
	}
	return sess.User, true
}

func (s *Server) error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	payload := oapi.ErrorResponse{}
	payload.Error.Code = code
	payload.Error.Message = message
	if id, ok := r.Context().Value(requestIDKey).(string); ok {
		payload.Error.RequestId = &id
	}
	writeJSON(w, status, payload)
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", "path", r.URL.Path, "error", err)
	s.error(w, r, http.StatusInternalServerError, "internal_error", "An unexpected error occurred.")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(r *http.Request, target any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 3<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func toUUID(id interface{ String() string }) (uuid.UUID, bool) {
	parsed, err := uuid.Parse(id.String())
	return parsed, err == nil
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func boolValue(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func mapAccountError(err error, w http.ResponseWriter, r *http.Request, s *Server) bool {
	switch {
	case errors.Is(err, account.ErrNotFound), errors.Is(err, message.ErrNotFound),
		errors.Is(err, mailbox.ErrNotFound), errors.Is(err, identity.ErrNotFound),
		errors.Is(err, compose.ErrNotFound):
		s.error(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return true
	case errors.Is(err, auth.ErrUnauthorized):
		s.error(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return true
	}
	return false
}

func trimLower(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("content exceeds limit")
	}
	return data, nil
}
