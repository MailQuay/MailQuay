package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/mailquay/mailquay/internal/auth"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

func (s *Server) GetAuthStatus(w http.ResponseWriter, r *http.Request) {
	needs, err := s.auth.NeedsSetup(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	scheme := auth.RequestScheme(r, s.settings.Current().TrustedProxies)
	status := oapi.AuthStatus{
		NeedsSetup: needs,
		Security: oapi.SecurityInfo{
			InsecureTransport: auth.InsecureTransport(r, s.settings.Current().TrustedProxies, s.settings.Current().InsecureAllow),
			Scheme:            scheme,
			Host:              r.Host,
		},
	}
	if !needs {
		sess, ok := UserFrom(r.Context())
		if !ok || sess == nil {
			s.error(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		user := mapUser(sess.User)
		status.User = &user
		if cookie, err := r.Cookie(auth.CSRFCookie); err == nil && cookie.Value != "" {
			status.CsrfToken = &cookie.Value
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) SetupOwner(w http.ResponseWriter, r *http.Request) {
	var body oapi.SetupRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	sess, err := s.auth.Setup(r.Context(), body.Token, body.Username, body.Password, r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidSetupToken):
			s.error(w, r, http.StatusBadRequest, "invalid_setup_token", "The setup token is invalid.")
		case errors.Is(err, auth.ErrSetupClosed):
			s.error(w, r, http.StatusConflict, "setup_closed", "The owner account already exists.")
		default:
			s.error(w, r, http.StatusBadRequest, "setup_failed", err.Error())
		}
		return
	}
	s.setAuthCookies(w, r, sess)
	writeJSON(w, http.StatusCreated, authResponse(sess))
}

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var body oapi.LoginRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	sess, err := s.auth.Login(r.Context(), body.Username, body.Password, r.UserAgent(), auth.ClientIP(r, s.settings.Current().TrustedProxies))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			s.error(w, r, http.StatusTooManyRequests, "rate_limited", "Too many attempts. Try again later.")
		case errors.Is(err, auth.ErrInvalidCredentials):
			s.error(w, r, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password.")
		default:
			s.internal(w, r, err)
		}
		return
	}
	s.setAuthCookies(w, r, sess)
	writeJSON(w, http.StatusOK, authResponse(sess))
}

func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(auth.SessionCookie)
	if err == nil {
		_ = s.auth.Logout(r.Context(), cookie.Value)
	}
	s.clearAuthCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var body oapi.ChangePasswordRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	user, ok := s.user(r.Context())
	if !ok {
		s.error(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	cookie, _ := r.Cookie(auth.SessionCookie)
	token := ""
	if cookie != nil {
		token = cookie.Value
	}
	if err := s.auth.ChangePassword(r.Context(), user.ID, body.CurrentPassword, body.NewPassword, token); err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			s.error(w, r, http.StatusUnauthorized, "invalid_credentials", "The current password is incorrect.")
			return
		}
		s.error(w, r, http.StatusBadRequest, "password_change_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	var body oapi.UpdateProfileRequest
	if err := decodeJSON(r, &body); err != nil {
		s.error(w, r, http.StatusBadRequest, "invalid_request", "Malformed request body.")
		return
	}
	user, ok := s.user(r.Context())
	if !ok {
		s.error(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
		return
	}
	updated, err := s.auth.UpdateProfile(r.Context(), user.ID, body.DisplayName, body.Locale, body.Theme)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapUser(updated))
}

func (s *Server) setAuthCookies(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	secure := auth.RequestScheme(r, s.settings.Current().TrustedProxies) == "https"
	sameSite := http.SameSiteLaxMode
	if len(s.settings.Current().CORSOrigins) > 0 {
		secure = true
		sameSite = http.SameSiteNoneMode
	}
	maxAge := int(time.Until(sess.ExpiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: sess.Token, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: sameSite, MaxAge: maxAge, Expires: sess.ExpiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name: auth.CSRFCookie, Value: sess.CSRFToken, Path: "/", HttpOnly: false,
		Secure: secure, SameSite: sameSite, MaxAge: maxAge, Expires: sess.ExpiresAt,
	})
}

func (s *Server) clearAuthCookies(w http.ResponseWriter, r *http.Request) {
	secure := auth.RequestScheme(r, s.settings.Current().TrustedProxies) == "https"
	sameSite := http.SameSiteLaxMode
	if len(s.settings.Current().CORSOrigins) > 0 {
		secure = true
		sameSite = http.SameSiteNoneMode
	}
	for _, name := range []string{auth.SessionCookie, auth.CSRFCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", HttpOnly: name == auth.SessionCookie,
			Secure: secure, SameSite: sameSite, MaxAge: -1, Expires: time.Unix(1, 0),
		})
	}
}

func authResponse(sess *auth.Session) oapi.AuthResponse {
	expires := sess.ExpiresAt
	return oapi.AuthResponse{User: mapUser(sess.User), CsrfToken: sess.CSRFToken, ExpiresAt: &expires}
}
