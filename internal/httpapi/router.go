package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mailquay/mailquay/internal/auth"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/httpapi/oapi"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/webassets"
)

type requestIDKeyType int

const requestIDKey requestIDKeyType = 1

// NewRouter builds the HTTP handler with middleware and static assets.
func NewRouter(srv *Server, hub *events.Hub, logger *slog.Logger) http.Handler {
	router := chi.NewRouter()
	router.Use(requestIDMiddleware, recoveryMiddleware(logger), loggingMiddleware(logger), securityHeaders, corsMiddleware(srv.settings))

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	api := chi.NewRouter()
	api.Use(srv.sessionMiddleware)
	api.Use(srv.csrfMiddleware)
	oapi.HandlerFromMux(srv, api)
	router.Mount("/api/v1", api)

	if assets := webassets.Handler(); assets != nil {
		router.Handle("/*", assets)
	} else {
		router.NotFound(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		})
	}
	return router
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			buf := make([]byte, 8)
			if _, err := rand.Read(buf); err == nil {
				id = hex.EncodeToString(buf)
			}
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic", "path", r.URL.Path, "panic", rec)
					http.Error(w, `{"error":{"code":"internal_error","message":"Internal server error."}}`, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			if strings.HasPrefix(r.URL.Path, "/api/") {
				logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
			}
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// Remote mail images are stripped by the frontend sanitizer until the
		// user allows the sender, so img-src must permit those opt-in requests.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: http: https:; font-src 'self' data:; connect-src 'self'; frame-src 'self' data: blob:; object-src 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(settingsSvc *settings.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allowed := settingsSvc.Current().CORSOrigins
			if origin != "" && len(allowed) > 0 {
				if containsString(allowed, origin) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Idempotency-Key")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
					w.Header().Set("Vary", "Origin")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := apiPath(r.URL.Path)
		public := path == "/auth/setup" || path == "/auth/login" || strings.HasPrefix(path, "/oauth/")
		var sess *auth.Session
		if cookie, err := r.Cookie(auth.SessionCookie); err == nil && cookie.Value != "" {
			if loaded, err := s.auth.Authenticate(r.Context(), cookie.Value); err == nil {
				sess = loaded
				s.refreshSessionCookie(w, r, loaded)
			}
		}
		if path == "/auth/me" {
			if sess != nil {
				r = r.WithContext(WithUser(r.Context(), sess))
			}
			next.ServeHTTP(w, r)
			return
		}
		if public {
			next.ServeHTTP(w, r)
			return
		}
		if sess == nil {
			if needs, err := s.auth.NeedsSetup(r.Context()); err == nil && needs {
				s.error(w, r, http.StatusConflict, "setup_required", "The owner account has not been created yet.")
				return
			}
			s.error(w, r, http.StatusUnauthorized, "unauthorized", "Authentication is required.")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), sess)))
	})
}

func (s *Server) refreshSessionCookie(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	values := s.settings.Current()
	secure := auth.RequestScheme(r, values.TrustedProxies) == "https"
	sameSite := http.SameSiteLaxMode
	if len(values.CORSOrigins) > 0 {
		secure = true
		sameSite = http.SameSiteNoneMode
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: sess.Token, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: sameSite,
		Expires: sess.ExpiresAt.UTC(),
	})
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		path := apiPath(r.URL.Path)
		if path == "/auth/setup" || path == "/auth/login" || strings.HasPrefix(path, "/oauth/") {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r, s.settings.Current().CORSOrigins) {
			s.error(w, r, http.StatusForbidden, "origin_rejected", "The request origin is not allowed.")
			return
		}
		cookie, err := r.Cookie(auth.CSRFCookie)
		if err != nil || !auth.ValidCSRF(cookie.Value, r.Header.Get("X-CSRF-Token")) {
			s.error(w, r, http.StatusForbidden, "csrf_failed", "The CSRF token is missing or invalid.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request, corsOrigins []string) bool {
	origin := r.Header.Get("Origin")
	if origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if strings.EqualFold(parsed.Host, r.Host) {
			return true
		}
		if containsString(corsOrigins, origin) {
			return true
		}
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	return true
}

func apiPath(path string) string {
	return strings.TrimPrefix(path, "/api/v1")
}
