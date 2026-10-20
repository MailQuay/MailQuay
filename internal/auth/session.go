package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

var (
	// ErrInvalidCredentials is returned for unknown users or wrong passwords.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrInvalidSetupToken is returned when the one-time setup token does not match.
	ErrInvalidSetupToken = errors.New("invalid setup token")
	// ErrUnauthorized means the session is missing or expired.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrSetupClosed means the owner account already exists.
	ErrSetupClosed = errors.New("setup already completed")
	// ErrRateLimited means too many attempts were made recently.
	ErrRateLimited = errors.New("rate limited")
)

const (
	SessionCookie = "mailquay_session"
	CSRFCookie    = "mailquay_csrf"
)

// Session is the result of a successful authentication.
type Session struct {
	User      sqlcgen.User
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

// Service implements owner authentication.
type Service struct {
	store      *store.Store
	settings   *settings.Service
	logger     *slog.Logger
	setupToken string
	limiter    *limiter
	mu         sync.Mutex
}

// NewService creates the authentication service.
func NewService(st *store.Store, settingsSvc *settings.Service, logger *slog.Logger, setupToken string) *Service {
	return &Service{
		store:      st,
		settings:   settingsSvc,
		logger:     logger,
		setupToken: setupToken,
		limiter:    newLimiter(10, time.Minute),
	}
}

// NeedsSetup reports whether the owner account exists.
func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return n == 0, nil
}

// SetupToken returns the one-time token shown at startup.
func (s *Service) SetupToken() string { return s.setupToken }

// Setup creates the owner account and a session.
func (s *Service) Setup(ctx context.Context, token, username, password, userAgent string) (*Session, error) {
	if strings.TrimSpace(token) == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.setupToken)) != 1 {
		return nil, ErrInvalidSetupToken
	}
	needs, err := s.NeedsSetup(ctx)
	if err != nil {
		return nil, err
	}
	if !needs {
		return nil, ErrSetupClosed
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	user, err := s.store.CreateUser(ctx, sqlcgen.CreateUserParams{
		ID:           uuid.MustParse(platform.NewID()),
		Username:     strings.TrimSpace(username),
		DisplayName:  strings.TrimSpace(username),
		PasswordHash: hash,
		Locale:       "en",
		Theme:        "system",
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSetupClosed
		}
		return nil, fmt.Errorf("create owner: %w", err)
	}
	s.mu.Lock()
	s.setupToken = ""
	s.mu.Unlock()
	return s.createSession(ctx, user, userAgent)
}

// Login verifies credentials and creates a session.
func (s *Service) Login(ctx context.Context, username, password, userAgent, ip string) (*Session, error) {
	if !s.limiter.Allow(ip) {
		return nil, ErrRateLimited
	}
	user, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("load user: %w", err)
	}
	if !VerifyPassword(password, user.PasswordHash) {
		return nil, ErrInvalidCredentials
	}
	s.limiter.Reset(ip)
	return s.createSession(ctx, user, userAgent)
}

func (s *Service) createSession(ctx context.Context, user sqlcgen.User, userAgent string) (*Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("read session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(s.settings.SessionTTL())
	_, err = s.store.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID:        uuid.MustParse(platform.NewID()),
		UserID:    user.ID,
		TokenHash: sum[:],
		ExpiresAt: expires,
		UserAgent: truncate(userAgent, 400),
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	_ = s.store.DeleteExpiredSessions(ctx)
	return &Session{User: user, Token: token, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// Authenticate resolves a session cookie value.
func (s *Service) Authenticate(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(token))
	user, err := s.store.GetSessionUser(ctx, sum[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("load session: %w", err)
	}
	expires := time.Now().UTC().Add(s.settings.SessionTTL())
	if err := s.store.TouchSession(ctx, sqlcgen.TouchSessionParams{TokenHash: sum[:], ExpiresAt: expires}); err != nil {
		s.logger.Warn("touch session", "error", err)
	}
	return &Session{User: user.User, Token: token, ExpiresAt: expires}, nil
}

// Logout deletes the current session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	return s.store.DeleteSession(ctx, sum[:])
}

// ChangePassword updates the password and revokes other sessions.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next, currentToken string) error {
	user, err := s.store.GetUser(ctx)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if user.ID != userID || !VerifyPassword(current, user.PasswordHash) {
		return ErrInvalidCredentials
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.UpdateUserPassword(ctx, sqlcgen.UpdateUserPasswordParams{ID: userID, PasswordHash: hash}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	sum := sha256.Sum256([]byte(currentToken))
	return s.store.DeleteOtherUserSessions(ctx, sqlcgen.DeleteOtherUserSessionsParams{UserID: userID, TokenHash: sum[:]})
}

// UpdateProfile saves display name, locale and theme.
func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, displayName, locale, theme string) (sqlcgen.User, error) {
	return s.store.UpdateUserProfile(ctx, sqlcgen.UpdateUserProfileParams{
		ID: userID, DisplayName: displayName, Locale: locale, Theme: theme,
	})
}

// CurrentUser loads the owner record.
func (s *Service) CurrentUser(ctx context.Context) (sqlcgen.User, error) {
	return s.store.GetUser(ctx)
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "duplicate key value")
}

// ValidCSRF compares the header token with the cookie value.
func ValidCSRF(cookie, header string) bool {
	if cookie == "" || header == "" || len(cookie) != len(header) {
		return false
	}
	return subtleCompare(cookie, header)
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
