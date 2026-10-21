package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/discovery"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

func (s *Server) ListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.accounts.List(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]oapi.Account, 0, len(accounts))
	for _, acct := range accounts {
		ids, _ := s.ids.List(r.Context(), acct.ID)
		out = append(out, mapAccount(acct, ids))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (s *Server) CreateAccount(w http.ResponseWriter, r *http.Request) {
	var body oapi.AccountInput
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	in := accountInput(body)
	if snap, err := s.settings.Get(r.Context()); err == nil {
		if in.InitialSyncRange == "" {
			in.InitialSyncRange = snap.Values.DefaultInitialSyncRange
		}
		if in.DownloadScope == "" {
			in.DownloadScope = snap.Values.DefaultDownloadScope
		}
	}
	acct, err := s.accounts.Create(r.Context(), in)
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "account_invalid", err.Error())
		return
	}
	ids, _ := s.ids.List(r.Context(), acct.ID)
	writeJSON(w, http.StatusCreated, mapAccount(acct, ids))
}

func (s *Server) GetAccount(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	acct, err := s.accounts.Get(r.Context(), accountID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	ids, _ := s.ids.List(r.Context(), acct.ID)
	writeJSON(w, http.StatusOK, mapAccount(acct, ids))
}

func (s *Server) UpdateAccount(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	var body oapi.AccountInput
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	acct, err := s.accounts.Update(r.Context(), accountID, accountInput(body))
	if err != nil {
		if errors.Is(err, account.ErrNotFound) {
			s.error(w, r, http.StatusNotFound, "not_found", "Account not found.")
			return
		}
		s.error(w, r, http.StatusBadRequest, "account_invalid", err.Error())
		return
	}
	ids, _ := s.ids.List(r.Context(), acct.ID)
	writeJSON(w, http.StatusOK, mapAccount(acct, ids))
}

func (s *Server) DeleteAccount(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	if err := s.accounts.Delete(r.Context(), accountID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) ReconcileAccount(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	if err := s.accounts.Reconcile(r.Context(), accountID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) TestAccount(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	result, err := s.accounts.Test(r.Context(), accountID)
	if err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.error(w, r, http.StatusBadGateway, "connection_test_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, oapi.ConnectionTestResult{
		ReceiveOk: result.ReceiveOK, SmtpOk: result.SMTPOK, Errors: result.Errors,
	})
}

func (s *Server) DiscoverAccount(w http.ResponseWriter, r *http.Request) {
	var body oapi.DiscoveryRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	result, err := discovery.Discover(r.Context(), string(body.Email))
	if err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_email", "Enter a valid email address.")
		return
	}
	out := oapi.DiscoveryResult{
		Domain: result.Domain, Provider: result.Provider,
		Source:          oapi.DiscoveryResultSource(result.Source),
		ReceiveProtocol: oapi.DiscoveryResultReceiveProtocol(result.ReceiveProtocol),
		Imap:            mapDiscoveryEndpoint(result.IMAP),
		Pop3:            mapDiscoveryEndpoint(result.POP3),
		Smtp:            mapDiscoveryEndpoint(result.SMTP),
	}
	if result.OAuthProvider != "" {
		provider := oapi.DiscoveryResultOauthProvider(result.OAuthProvider)
		out.OauthProvider = &provider
	}
	writeJSON(w, http.StatusOK, out)
}

func mapDiscoveryEndpoint(endpoint *discovery.Endpoint) *oapi.DiscoveryEndpoint {
	if endpoint == nil {
		return nil
	}
	return &oapi.DiscoveryEndpoint{
		Addr: endpoint.Addr, Security: oapi.DiscoveryEndpointSecurity(endpoint.Security),
	}
}

func (s *Server) SyncAccount(w http.ResponseWriter, r *http.Request, id oapi.AccountID) {
	accountID, ok := toUUID(id)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "invalid_id", "Invalid account id.")
		return
	}
	if _, err := s.accounts.Get(r.Context(), accountID); err != nil {
		if mapAccountError(err, w, r, s) {
			return
		}
		s.internal(w, r, err)
		return
	}
	s.sync.TriggerAccount(accountID)
	w.WriteHeader(http.StatusAccepted)
}

func accountInput(body oapi.AccountInput) account.Input {
	authType := string(body.AuthType)
	receive := string(body.ReceiveProtocol)
	in := account.Input{
		Name: body.Name, Email: string(body.Email), Username: body.Username,
		AuthType: authType, ReceiveProtocol: receive,
		Provider: derefString(body.Provider),
		Password: derefString(body.Password), OAuthProvider: derefEnum(body.OauthProvider),
		OAuthRefreshToken: derefString(body.OauthRefreshToken),
		IMAPAddr:          derefString(body.ImapAddr), POP3Addr: derefString(body.Pop3Addr),
		SMTPAddr: derefString(body.SmtpAddr), SMTPHeloHostname: derefString(body.SmtpHeloHostname),
		SMTPUsername:       derefString(body.SmtpUsername),
		SMTPPassword:       derefString(body.SmtpPassword),
		SyncEnabled:        boolValue(body.SyncEnabled, true),
		InitialSyncRange:   derefString((*string)(body.InitialSyncRange)),
		DownloadScope:      derefString((*string)(body.DownloadScope)),
		SentCopyMode:       derefString((*string)(body.SentCopyMode)),
		POP3LeaveOnServer:  boolValue(body.Pop3LeaveOnServer, true),
		POP3DeleteOnServer: boolValue(body.Pop3DeleteOnServer, false),
	}
	if body.ImapSecurity != nil {
		in.IMAPSecurity = string(*body.ImapSecurity)
	}
	if body.Pop3Security != nil {
		in.POP3Security = string(*body.Pop3Security)
	}
	if body.SmtpSecurity != nil {
		in.SMTPSecurity = string(*body.SmtpSecurity)
	}
	return in
}

var _ = uuid.Nil
