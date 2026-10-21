package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/auth"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// OAuth implements the authorization-code + PKCE flow for mail providers.
type OAuth struct {
	cfg      *config.Config
	settings *settings.Service
	store    *store.Store
	accounts *account.Service
	cipher   *crypto.Cipher
}

// NewOAuth builds the OAuth helper.
func NewOAuth(cfg *config.Config, settingsSvc *settings.Service, st *store.Store, accounts *account.Service, cipher *crypto.Cipher) *OAuth {
	return &OAuth{cfg: cfg, settings: settingsSvc, store: st, accounts: accounts, cipher: cipher}
}

type providerConfig struct {
	authURL  string
	tokenURL string
	scope    string
	clientID string
	secret   string
	extra    url.Values
}

func (o *OAuth) provider(name string) (providerConfig, bool) {
	values := o.settings.Current()
	switch name {
	case "google":
		return providerConfig{
			authURL: "https://accounts.google.com/o/oauth2/v2/auth", tokenURL: "https://oauth2.googleapis.com/token",
			scope: "https://mail.google.com/", clientID: values.GoogleClientID, secret: o.settings.OAuthSecret("google"),
			extra: url.Values{"access_type": {"offline"}, "prompt": {"consent"}},
		}, values.GoogleClientID != ""
	case "microsoft":
		return providerConfig{
			authURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			tokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
			scope:    "offline_access https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send",
			clientID: values.MicrosoftClientID, secret: o.settings.OAuthSecret("microsoft"),
		}, values.MicrosoftClientID != ""
	}
	return providerConfig{}, false
}

func (o *OAuth) redirectURI(r *http.Request, provider string) string {
	if o.cfg.PublicURL != "" {
		return strings.TrimRight(o.cfg.PublicURL, "/") + "/api/v1/oauth/" + provider + "/callback"
	}
	scheme := auth.RequestScheme(r, o.cfg.TrustedProxies)
	return scheme + "://" + r.Host + "/api/v1/oauth/" + provider + "/callback"
}

// Start redirects the browser to the provider.
func (o *OAuth) Start(w http.ResponseWriter, r *http.Request, provider string, accountID uuid.UUID) {
	conf, ok := o.provider(provider)
	if !ok {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=provider_not_configured", http.StatusFound)
		return
	}
	if _, err := o.accounts.Get(r.Context(), accountID); err != nil {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=account_not_found", http.StatusFound)
		return
	}
	state, err := randomURLToken(32)
	if err != nil {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=state_failed", http.StatusFound)
		return
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=state_failed", http.StatusFound)
		return
	}
	enc, err := o.cipher.EncryptString(verifier, []byte("oauth_state:"+state))
	if err != nil {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=state_failed", http.StatusFound)
		return
	}
	if err := o.store.CreateOAuthState(r.Context(), sqlcgen.CreateOAuthStateParams{
		State: state, Provider: provider, VerifierEnc: enc,
		RedirectAfter: "account:" + accountID.String(), ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}); err != nil {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=state_failed", http.StatusFound)
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	params := url.Values{
		"client_id": {conf.clientID}, "response_type": {"code"},
		"redirect_uri": {o.redirectURI(r, provider)}, "scope": {conf.scope},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	for key, values := range conf.extra {
		params[key] = values
	}
	http.Redirect(w, r, conf.authURL+"?"+params.Encode(), http.StatusFound)
}

// Callback exchanges the authorization code and stores the refresh token.
func (o *OAuth) Callback(w http.ResponseWriter, r *http.Request, provider string, code, state, errParam string) {
	if errParam != "" {
		redirectOAuth(w, r, "error", errParam)
		return
	}
	if code == "" || state == "" {
		redirectOAuth(w, r, "error", "missing_code")
		return
	}
	stored, err := o.store.ConsumeOAuthState(r.Context(), state)
	if err != nil || stored.Provider != provider {
		redirectOAuth(w, r, "error", "invalid_state")
		return
	}
	verifier, err := o.cipher.DecryptString(stored.VerifierEnc, []byte("oauth_state:"+state))
	if err != nil {
		redirectOAuth(w, r, "error", "state_decrypt_failed")
		return
	}
	accountID, err := uuid.Parse(strings.TrimPrefix(stored.RedirectAfter, "account:"))
	if err != nil {
		redirectOAuth(w, r, "error", "account_missing")
		return
	}
	conf, ok := o.provider(provider)
	if !ok {
		redirectOAuth(w, r, "error", "provider_not_configured")
		return
	}
	refresh, err := o.exchange(r.Context(), conf, code, verifier, o.redirectURI(r, provider))
	if err != nil {
		redirectOAuth(w, r, "error", "exchange_failed")
		return
	}
	scopes := strings.Fields(conf.scope)
	if err := o.accounts.SetOAuthToken(r.Context(), accountID, provider, refresh, scopes); err != nil {
		redirectOAuth(w, r, "error", "store_failed")
		return
	}
	redirectOAuth(w, r, "connected", "")
}

func (o *OAuth) exchange(ctx context.Context, conf providerConfig, code, verifier, redirectURI string) (string, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirectURI}, "client_id": {conf.clientID},
		"client_secret": {conf.secret}, "code_verifier": {verifier},
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conf.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.RefreshToken == "" {
		return "", fmt.Errorf("provider did not return a refresh token: %s %s", parsed.Error, parsed.ErrorDesc)
	}
	return parsed.RefreshToken, nil
}

func randomURLToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func redirectOAuth(w http.ResponseWriter, r *http.Request, status, message string) {
	target := "/settings/accounts?oauth=" + url.QueryEscape(status)
	if message != "" {
		target += "&message=" + url.QueryEscape(message)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) OauthStart(w http.ResponseWriter, r *http.Request, provider oapi.OAuthProvider, params oapi.OauthStartParams) {
	if params.AccountId == nil {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=account_required", http.StatusFound)
		return
	}
	accountID, ok := toUUID(*params.AccountId)
	if !ok {
		http.Redirect(w, r, "/settings/accounts?oauth=error&message=invalid_account", http.StatusFound)
		return
	}
	s.oauth.Start(w, r, string(provider), accountID)
}

func (s *Server) OauthCallback(w http.ResponseWriter, r *http.Request, provider oapi.OAuthProvider, params oapi.OauthCallbackParams) {
	code, state, errParam := "", "", ""
	if params.Code != nil {
		code = *params.Code
	}
	if params.State != nil {
		state = *params.State
	}
	if params.Error != nil {
		errParam = *params.Error
	}
	s.oauth.Callback(w, r, string(provider), code, state, errParam)
}
