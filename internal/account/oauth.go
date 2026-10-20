package account

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

type cachedToken struct {
	value     string
	expiresAt time.Time
}

var tokenCache sync.Map

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// AccessToken returns a valid access token for an OAuth2 account.
func (s *Service) AccessToken(ctx context.Context, acct sqlcgen.Account) (string, error) {
	if acct.AuthType != "oauth2" {
		return "", nil
	}
	if cached, ok := tokenCache.Load(acct.ID); ok {
		tok := cached.(cachedToken)
		if tok.value != "" && time.Now().Before(tok.expiresAt.Add(-30*time.Second)) {
			return tok.value, nil
		}
	}
	refreshToken, err := s.refreshToken(acct)
	if err != nil {
		return "", err
	}
	provider := strings.ToLower(deref(acct.OauthProvider))
	endpoint, clientID, clientSecret := s.oauthClient(provider)
	if endpoint == "" {
		return "", fmt.Errorf("OAuth provider %q is not configured", provider)
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	if provider == "microsoft" {
		form.Set("scope", "offline_access https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("refresh oauth token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode oauth response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("oauth refresh failed: %s %s", parsed.Error, parsed.ErrorDesc)
	}
	expires := time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	if parsed.ExpiresIn <= 0 {
		expires = time.Now().Add(50 * time.Minute)
	}
	tokenCache.Store(acct.ID, cachedToken{value: parsed.AccessToken, expiresAt: expires})
	return parsed.AccessToken, nil
}

// SetOAuthToken stores an encrypted refresh token on the account.
func (s *Service) SetOAuthToken(ctx context.Context, id uuid.UUID, provider, refreshToken string, scopes []string) error {
	enc, err := s.cipher.EncryptString(refreshToken, oauthAAD(id))
	if err != nil {
		return err
	}
	if err := s.store.UpdateAccountOAuth(ctx, sqlcgen.UpdateAccountOAuthParams{
		ID: id, OauthProvider: &provider, OauthScopes: scopes, OauthRefreshEnc: enc,
	}); err != nil {
		return err
	}
	tokenCache.Delete(id)
	return nil
}

func (s *Service) refreshToken(acct sqlcgen.Account) (string, error) {
	if len(acct.OauthRefreshEnc) == 0 {
		return "", fmt.Errorf("account has no OAuth refresh token")
	}
	return s.cipher.DecryptString(acct.OauthRefreshEnc, oauthAAD(acct.ID))
}

func (s *Service) oauthClient(provider string) (endpoint, clientID, secret string) {
	if s.settings == nil {
		return "", "", ""
	}
	values := s.settings.Current()
	switch provider {
	case "google":
		return "https://oauth2.googleapis.com/token", values.GoogleClientID, s.settings.OAuthSecret("google")
	case "microsoft":
		return "https://login.microsoftonline.com/common/oauth2/v2.0/token", values.MicrosoftClientID, s.settings.OAuthSecret("microsoft")
	}
	return "", "", ""
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
