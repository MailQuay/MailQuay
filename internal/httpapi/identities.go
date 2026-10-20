package httpapi

import (
	"errors"
	"net/http"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/identity"
)

func (s *Server) ListIdentities(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	rows, err := s.ids.List(r.Context(), accountID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]oapi.Identity, 0, len(rows))
	for _, row := range rows {
		out = append(out, mapIdentity(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": out})
}

func (s *Server) CreateIdentity(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	var body oapi.IdentityInput
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	row, err := s.ids.Create(r.Context(), accountID, identityInput(body))
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "identity_invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, mapIdentity(row))
}

func (s *Server) UpdateIdentity(w http.ResponseWriter, r *http.Request, id oapi.AccountID, email oapi.IdentityEmail) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	var body oapi.IdentityInput
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	row, err := s.ids.Update(r.Context(), accountID, email, identityInput(body))
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			s.error(w, r, http.StatusNotFound, "not_found", "Identity not found.")
			return
		}
		s.error(w, r, http.StatusBadRequest, "identity_invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mapIdentity(row))
}

func (s *Server) DeleteIdentity(w http.ResponseWriter, r *http.Request, id oapi.AccountID, email oapi.IdentityEmail) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	err := s.ids.Delete(r.Context(), accountID, email)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrDefaultNotDeletable):
			s.error(w, r, http.StatusBadRequest, "default_identity", "The default identity cannot be deleted.")
		case errors.Is(err, identity.ErrNotFound):
			s.error(w, r, http.StatusNotFound, "not_found", "Identity not found.")
		default:
			s.internal(w, r, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) SetDefaultIdentity(w http.ResponseWriter, r *http.Request, id oapi.AccountID, email oapi.IdentityEmail) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	if err := s.ids.SetDefault(r.Context(), accountID, email); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			s.error(w, r, http.StatusNotFound, "not_found", "Identity not found.")
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func identityInput(body oapi.IdentityInput) identity.Input {
	bcc := []string{}
	if body.Bcc != nil {
		bcc = *body.Bcc
	}
	return identity.Input{
		Email: body.Email, Name: body.Name, Organization: derefString(body.Organization),
		ReplyTo: derefString(body.ReplyTo), Bcc: bcc,
		SignatureHTML: derefString(body.SignatureHtml), SignatureText: derefString(body.SignatureText),
		SignatureMode: string(body.SignatureMode), IsDefault: boolValue(body.IsDefault, false),
	}
}
