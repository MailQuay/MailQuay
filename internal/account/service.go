package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/pop3client"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/smtpclient"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// ErrNotFound is returned when an account does not exist.
var ErrNotFound = errors.New("account not found")

// Input is the editable account shape.
type Input struct {
	Name               string
	Email              string
	Provider           string
	Username           string
	AuthType           string
	Password           string
	OAuthProvider      string
	OAuthRefreshToken  string
	ReceiveProtocol    string
	IMAPAddr           string
	IMAPSecurity       string
	POP3Addr           string
	POP3Security       string
	SMTPAddr           string
	SMTPSecurity       string
	SMTPUsername       string
	SMTPPassword       string
	SMTPHeloHostname   string
	SyncEnabled        bool
	InitialSyncRange   string
	DownloadScope      string
	SentCopyMode       string
	DefaultSendName    string
	DefaultReplyTo     string
	POP3LeaveOnServer  bool
	POP3DeleteOnServer bool
	// AutoDetectSpecialFolders is nil when the caller did not send it.
	AutoDetectSpecialFolders *bool
	// SpecialFolders is nil when the caller did not send it; an empty map
	// clears the manual choices.
	SpecialFolders map[string]string
}

// Credentials holds decrypted account secrets.
type Credentials struct {
	Username     string
	Password     string
	AccessToken  string
	SMTPUsername string
	SMTPPassword string
}

// TestResult reports connection checks.
type TestResult struct {
	ReceiveOK bool
	SMTPOK    bool
	Errors    []string
}

// Service manages mailbox accounts and their encrypted credentials.
type Service struct {
	store     *store.Store
	cipher    *crypto.Cipher
	logger    *slog.Logger
	settings  *settings.Service
	imports   *importStore
	OnCreated func(ctx context.Context, accountID uuid.UUID)
	OnDeleted func(ctx context.Context, accountID uuid.UUID)
	OnUpdated func(ctx context.Context, accountID uuid.UUID)
	OnScope   func(ctx context.Context, accountID uuid.UUID)
}

// NewService builds the account service.
func NewService(st *store.Store, cipher *crypto.Cipher, logger *slog.Logger) *Service {
	return &Service{store: st, cipher: cipher, logger: logger, imports: newImportStore()}
}

// SetSettings wires runtime OAuth configuration.
func (s *Service) SetSettings(settingsSvc *settings.Service) { s.settings = settingsSvc }

// List returns active accounts.
func (s *Service) List(ctx context.Context) ([]sqlcgen.Account, error) {
	return s.store.ListAccounts(ctx)
}

// Get loads one account.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (sqlcgen.Account, error) {
	acct, err := s.store.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Account{}, ErrNotFound
		}
		return sqlcgen.Account{}, fmt.Errorf("load account: %w", err)
	}
	if acct.DeletedAt != nil {
		return sqlcgen.Account{}, ErrNotFound
	}
	return acct, nil
}

// Create validates input, encrypts secrets, and inserts the account.
func (s *Service) Create(ctx context.Context, in Input) (sqlcgen.Account, error) {
	normalized, err := s.normalize(in, true)
	if err != nil {
		return sqlcgen.Account{}, err
	}
	autoDetect := true
	if normalized.AutoDetectSpecialFolders != nil {
		autoDetect = *normalized.AutoDetectSpecialFolders
	}
	id := uuid.MustParse(platform.NewID())
	params := sqlcgen.CreateAccountParams{
		ID:                       id,
		Name:                     normalized.Name,
		Email:                    normalized.Email,
		Provider:                 normalized.Provider,
		Username:                 normalized.Username,
		AuthType:                 normalized.AuthType,
		OauthProvider:            nullable(normalized.OAuthProvider),
		OauthScopes:              nil,
		ReceiveProtocol:          normalized.ReceiveProtocol,
		ImapAddr:                 nullable(normalized.IMAPAddr),
		ImapSecurity:             normalized.IMAPSecurity,
		Pop3Addr:                 nullable(normalized.POP3Addr),
		Pop3Security:             normalized.POP3Security,
		SmtpAddr:                 nullable(normalized.SMTPAddr),
		SmtpSecurity:             normalized.SMTPSecurity,
		SmtpUsername:             nullable(normalized.SMTPUsername),
		SmtpHeloHostname:         normalized.SMTPHeloHostname,
		SyncEnabled:              normalized.SyncEnabled,
		InitialSyncRange:         normalized.InitialSyncRange,
		DownloadScope:            normalized.DownloadScope,
		InitialSyncCutoffAt:      cutoff(normalized.InitialSyncRange),
		SentCopyMode:             normalized.SentCopyMode,
		Pop3LeaveOnServer:        normalized.POP3LeaveOnServer,
		Pop3DeleteOnServer:       normalized.POP3DeleteOnServer,
		AutoDetectSpecialFolders: autoDetect,
		SpecialFolders:           mailbox.EncodeSpecialFolders(normalized.SpecialFolders),
		DefaultSendName:          normalized.DefaultSendName,
		DefaultReplyTo:           normalized.DefaultReplyTo,
	}
	if in.AuthType == "password" {
		enc, err := s.cipher.EncryptString(in.Password, passwordAAD(id))
		if err != nil {
			return sqlcgen.Account{}, err
		}
		params.PasswordEnc = enc
	}
	if in.SMTPPassword != "" {
		enc, err := s.cipher.EncryptString(in.SMTPPassword, smtpPasswordAAD(id))
		if err != nil {
			return sqlcgen.Account{}, err
		}
		params.SmtpPasswordEnc = enc
	}
	if in.AuthType == "oauth2" && in.OAuthRefreshToken != "" {
		enc, err := s.cipher.EncryptString(in.OAuthRefreshToken, oauthAAD(id))
		if err != nil {
			return sqlcgen.Account{}, err
		}
		params.OauthRefreshEnc = enc
	}
	acct, err := s.store.CreateAccount(ctx, params)
	if err != nil {
		return sqlcgen.Account{}, fmt.Errorf("create account: %w", err)
	}
	if s.OnCreated != nil {
		s.OnCreated(ctx, id)
	}
	return acct, nil
}

// Update applies changes and triggers scope reconciliation when needed.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (sqlcgen.Account, error) {
	existing, err := s.Get(ctx, id)
	if err != nil {
		return sqlcgen.Account{}, err
	}
	normalized, err := s.normalize(in, false)
	if err != nil {
		return sqlcgen.Account{}, err
	}
	params := sqlcgen.UpdateAccountParams{
		ID:                       id,
		Name:                     normalized.Name,
		Email:                    normalized.Email,
		Username:                 normalized.Username,
		AuthType:                 normalized.AuthType,
		PasswordEnc:              existing.PasswordEnc,
		OauthProvider:            nullable(normalized.OAuthProvider),
		OauthScopes:              existing.OauthScopes,
		OauthRefreshEnc:          existing.OauthRefreshEnc,
		ReceiveProtocol:          normalized.ReceiveProtocol,
		ImapAddr:                 nullable(normalized.IMAPAddr),
		ImapSecurity:             normalized.IMAPSecurity,
		Pop3Addr:                 nullable(normalized.POP3Addr),
		Pop3Security:             normalized.POP3Security,
		SmtpAddr:                 nullable(normalized.SMTPAddr),
		SmtpSecurity:             normalized.SMTPSecurity,
		SmtpUsername:             nullable(normalized.SMTPUsername),
		SmtpHeloHostname:         normalized.SMTPHeloHostname,
		SyncEnabled:              normalized.SyncEnabled,
		InitialSyncRange:         normalized.InitialSyncRange,
		DownloadScope:            normalized.DownloadScope,
		InitialSyncCutoffAt:      existing.InitialSyncCutoffAt,
		SentCopyMode:             normalized.SentCopyMode,
		Pop3LeaveOnServer:        normalized.POP3LeaveOnServer,
		Pop3DeleteOnServer:       normalized.POP3DeleteOnServer,
		AutoDetectSpecialFolders: existing.AutoDetectSpecialFolders,
		SpecialFolders:           existing.SpecialFolders,
		DefaultSendName:          normalized.DefaultSendName,
		DefaultReplyTo:           normalized.DefaultReplyTo,
	}
	if normalized.AutoDetectSpecialFolders != nil {
		params.AutoDetectSpecialFolders = *normalized.AutoDetectSpecialFolders
	}
	if normalized.SpecialFolders != nil {
		params.SpecialFolders = mailbox.EncodeSpecialFolders(normalized.SpecialFolders)
	}
	if normalized.InitialSyncRange != existing.InitialSyncRange || normalized.DownloadScope != existing.DownloadScope {
		params.InitialSyncCutoffAt = cutoff(normalized.InitialSyncRange)
	}
	if in.Password != "" {
		enc, err := s.cipher.EncryptString(in.Password, passwordAAD(id))
		if err != nil {
			return sqlcgen.Account{}, err
		}
		params.PasswordEnc = enc
	}
	if in.OAuthRefreshToken != "" {
		enc, err := s.cipher.EncryptString(in.OAuthRefreshToken, oauthAAD(id))
		if err != nil {
			return sqlcgen.Account{}, err
		}
		params.OauthRefreshEnc = enc
	}
	if normalized.SMTPUsername == "" {
		params.SmtpPasswordEnc = nil
	}
	if in.SMTPPassword != "" {
		enc, err := s.cipher.EncryptString(in.SMTPPassword, smtpPasswordAAD(id))
		if err != nil {
			return sqlcgen.Account{}, err
		}
		params.SmtpPasswordEnc = enc
	}
	updated, err := s.store.UpdateAccount(ctx, params)
	if err != nil {
		return sqlcgen.Account{}, fmt.Errorf("update account: %w", err)
	}
	if s.OnUpdated != nil {
		s.OnUpdated(ctx, id)
	}
	if normalized.InitialSyncRange != existing.InitialSyncRange || normalized.DownloadScope != existing.DownloadScope {
		if s.OnScope != nil {
			s.OnScope(ctx, id)
		}
	}
	return updated, nil
}

// Delete soft deletes the account and schedules cleanup.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.store.SoftDeleteAccount(ctx, id); err != nil {
		return fmt.Errorf("soft delete account: %w", err)
	}
	if s.OnDeleted != nil {
		s.OnDeleted(ctx, id)
	}
	return nil
}

// Reconcile queues a scope reconciliation for the account.
func (s *Service) Reconcile(ctx context.Context, id uuid.UUID) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if s.OnScope != nil {
		s.OnScope(ctx, id)
	}
	return nil
}

// Credentials decrypts the stored secret for protocol use.
func (s *Service) Credentials(ctx context.Context, acct sqlcgen.Account) (Credentials, error) {
	creds := Credentials{Username: acct.Username, SMTPUsername: acct.Username}
	if acct.SmtpUsername != nil && strings.TrimSpace(*acct.SmtpUsername) != "" {
		creds.SMTPUsername = strings.TrimSpace(*acct.SmtpUsername)
	}
	if acct.AuthType == "password" {
		if len(acct.PasswordEnc) == 0 {
			return creds, errors.New("account has no stored password")
		}
		password, err := s.cipher.DecryptString(acct.PasswordEnc, passwordAAD(acct.ID))
		if err != nil {
			return creds, fmt.Errorf("decrypt password: %w", err)
		}
		creds.Password = password
		creds.SMTPPassword = password
		if len(acct.SmtpPasswordEnc) > 0 {
			smtpPassword, err := s.cipher.DecryptString(acct.SmtpPasswordEnc, smtpPasswordAAD(acct.ID))
			if err != nil {
				return creds, fmt.Errorf("decrypt smtp password: %w", err)
			}
			creds.SMTPPassword = smtpPassword
		}
		return creds, nil
	}
	token, err := s.AccessToken(ctx, acct)
	if err != nil {
		return creds, err
	}
	creds.AccessToken = token
	return creds, nil
}

// Test checks the saved receive and SMTP configuration.
func (s *Service) Test(ctx context.Context, id uuid.UUID) (TestResult, error) {
	acct, err := s.Get(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	creds, err := s.Credentials(ctx, acct)
	if err != nil {
		return TestResult{}, err
	}
	result := TestResult{Errors: []string{}}
	timeout := 15 * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if acct.ReceiveProtocol == "imap" && acct.ImapAddr != nil {
		c, err := imapclient.Dial(ctx, imapclient.Config{
			Address: *acct.ImapAddr, Security: acct.ImapSecurity, Username: acct.Username,
			Password: creds.Password, AccessToken: creds.AccessToken, Timeout: timeout,
		})
		if err != nil {
			result.Errors = append(result.Errors, "IMAP: "+err.Error())
		} else {
			if _, err := c.List(ctx); err != nil {
				result.Errors = append(result.Errors, "IMAP: "+err.Error())
			} else {
				result.ReceiveOK = true
			}
			_ = c.Close()
		}
	} else if acct.Pop3Addr != nil {
		c, err := pop3client.Dial(ctx, pop3client.Config{
			Address: *acct.Pop3Addr, Security: acct.Pop3Security, Username: acct.Username,
			Password: creds.Password, Timeout: timeout,
		})
		if err != nil {
			result.Errors = append(result.Errors, "POP3: "+err.Error())
		} else {
			if _, _, err := c.Stat(ctx); err != nil {
				result.Errors = append(result.Errors, "POP3: "+err.Error())
			} else {
				result.ReceiveOK = true
			}
			_ = c.Quit(ctx)
			_ = c.Close()
		}
	}
	if acct.SmtpAddr != nil && *acct.SmtpAddr != "" {
		err := smtpclient.Test(ctx, smtpclient.Config{
			Address: *acct.SmtpAddr, Security: acct.SmtpSecurity, Username: creds.SMTPUsername,
			Password: creds.SMTPPassword, AccessToken: creds.AccessToken,
			HELOHostname: acct.SmtpHeloHostname, Timeout: timeout,
		})
		if err != nil {
			result.Errors = append(result.Errors, "SMTP: "+err.Error())
		} else {
			result.SMTPOK = true
		}
	}
	return result, nil
}

func (s *Service) normalize(in Input, creating bool) (Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Username = strings.TrimSpace(in.Username)
	in.SMTPUsername = strings.TrimSpace(in.SMTPUsername)
	in.AuthType = defaultString(in.AuthType, "password")
	in.ReceiveProtocol = defaultString(in.ReceiveProtocol, "imap")
	in.IMAPSecurity = defaultString(in.IMAPSecurity, "implicit_tls")
	in.POP3Security = defaultString(in.POP3Security, "implicit_tls")
	in.SMTPSecurity = defaultString(in.SMTPSecurity, "starttls")
	in.InitialSyncRange = defaultString(in.InitialSyncRange, "all")
	in.DownloadScope = defaultString(in.DownloadScope, "full_no_attachments")
	in.SentCopyMode = defaultString(in.SentCopyMode, "auto")
	in.Provider = defaultString(in.Provider, "custom")
	in.DefaultSendName = strings.TrimSpace(in.DefaultSendName)
	in.DefaultReplyTo = strings.TrimSpace(in.DefaultReplyTo)
	if in.DefaultReplyTo != "" {
		if _, err := mail.ParseAddressList(in.DefaultReplyTo); err != nil {
			return in, errors.New("defaultReplyTo must be a valid address")
		}
	}
	if in.Email == "" {
		return in, errors.New("email is required")
	}
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return in, err
	}
	in.Email = email
	if in.Name == "" {
		in.Name = email
	}
	if in.Username == "" {
		return in, errors.New("username is required")
	}
	if in.AuthType != "password" && in.AuthType != "oauth2" {
		return in, errors.New("authType must be password or oauth2")
	}
	if creating && in.AuthType == "password" && in.Password == "" {
		return in, errors.New("password is required")
	}
	if in.AuthType == "oauth2" && in.OAuthProvider == "" {
		return in, errors.New("oauthProvider is required for OAuth2 accounts")
	}
	switch in.ReceiveProtocol {
	case "imap":
		if in.IMAPAddr == "" {
			return in, errors.New("imapAddr is required for IMAP accounts")
		}
		in.POP3Addr = ""
	case "pop3":
		if in.POP3Addr == "" {
			return in, errors.New("pop3Addr is required for POP3 accounts")
		}
		in.IMAPAddr = ""
	default:
		return in, errors.New("receiveProtocol must be imap or pop3")
	}
	for name, addr := range map[string]string{"imap": in.IMAPAddr, "pop3": in.POP3Addr, "smtp": in.SMTPAddr} {
		if addr == "" {
			continue
		}
		normalized, err := NormalizeAddress(addr)
		if err != nil {
			return in, fmt.Errorf("%s: %w", name, err)
		}
		if err := ValidatePort(normalized); err != nil {
			return in, fmt.Errorf("%s: %w", name, err)
		}
		switch name {
		case "imap":
			in.IMAPAddr = normalized
		case "pop3":
			in.POP3Addr = normalized
		case "smtp":
			in.SMTPAddr = normalized
		}
	}
	for _, sec := range []string{in.IMAPSecurity, in.POP3Security, in.SMTPSecurity} {
		if sec != "implicit_tls" && sec != "starttls" && sec != "none" {
			return in, errors.New("security mode must be implicit_tls, starttls or none")
		}
	}
	if !validRange(in.InitialSyncRange) {
		return in, errors.New("initialSyncRange must be all, 1d, 3d, 1w, 1m or 1y")
	}
	if !validScope(in.DownloadScope) {
		return in, errors.New("downloadScope must be headers, full_no_attachments or full_with_attachments")
	}
	if in.SpecialFolders != nil {
		normalized, err := normalizeSpecialFolders(in.SpecialFolders)
		if err != nil {
			return in, err
		}
		in.SpecialFolders = normalized
	}
	if in.SentCopyMode != "auto" && in.SentCopyMode != "append" && in.SentCopyMode != "none" {
		return in, errors.New("sentCopyMode must be auto, append or none")
	}
	if creating && in.AuthType == "password" {
		in.POP3LeaveOnServer = true
	}
	return in, nil
}

func validRange(v string) bool {
	switch v {
	case "all", "1d", "3d", "1w", "1m", "1y":
		return true
	}
	return false
}

func validScope(v string) bool {
	switch v {
	case "headers", "full_no_attachments", "full_with_attachments":
		return true
	}
	return false
}

func cutoff(v string) *time.Time {
	var d time.Duration
	switch v {
	case "1d":
		d = 24 * time.Hour
	case "3d":
		d = 72 * time.Hour
	case "1w":
		d = 7 * 24 * time.Hour
	case "1m":
		d = 30 * 24 * time.Hour
	case "1y":
		d = 365 * 24 * time.Hour
	default:
		return nil
	}
	t := time.Now().UTC().Add(-d)
	return &t
}

func passwordAAD(id uuid.UUID) []byte     { return []byte("account:" + id.String() + ":password") }
func smtpPasswordAAD(id uuid.UUID) []byte { return []byte("account:" + id.String() + ":smtp_password") }
func oauthAAD(id uuid.UUID) []byte        { return []byte("account:" + id.String() + ":oauth_refresh") }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// normalizeSpecialFolders trims manual folder paths, drops empty entries and
// rejects unknown roles or two roles pointing at the same folder.
func normalizeSpecialFolders(values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	seen := make(map[string]string, len(values))
	for role, path := range values {
		path = strings.TrimSpace(path)
		if !mailbox.IsSpecialRole(role) {
			return nil, fmt.Errorf("unknown special folder role %q", role)
		}
		if path == "" {
			continue
		}
		if other, ok := seen[path]; ok {
			return nil, fmt.Errorf("special folders %s and %s point to the same folder", other, role)
		}
		seen[path] = role
		out[role] = path
	}
	return out, nil
}
