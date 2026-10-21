package account

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/emclient"
	"github.com/mailquay/mailquay/internal/platform"
)

// ErrImportSessionNotFound is returned when an eM Client import preview has
// expired or was never created.
var ErrImportSessionNotFound = errors.New("emclient import session not found or expired")

const importSessionTTL = 15 * time.Minute

// ImportAccount is a parsed account candidate that is safe to send to the
// client. It never contains credentials.
type ImportAccount struct {
	Email           string
	ReceiveProtocol string
	IMAPAddr        string
	POP3Addr        string
	SMTPAddr        string
	AuthType        string
	Importable      bool
	Selected        bool
	Reason          string
}

// ImportPreview is the result of parsing an eM Client database.
type ImportPreview struct {
	ID       uuid.UUID
	Accounts []ImportAccount
}

// ImportFailure reports one account that could not be created.
type ImportFailure struct {
	Email string
	Error string
}

// ImportResult reports the outcome of creating the selected accounts.
type ImportResult struct {
	Created []string
	Skipped []string
	Failed  []ImportFailure
}

type importSession struct {
	accounts map[string]emclient.Account
	expires  time.Time
}

type importStore struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]*importSession
}

func newImportStore() *importStore {
	return &importStore{sessions: make(map[uuid.UUID]*importSession)}
}

func (s *importStore) put(accounts map[string]emclient.Account) uuid.UUID {
	id := uuid.MustParse(platform.NewID())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	s.sessions[id] = &importSession{accounts: accounts, expires: time.Now().Add(importSessionTTL)}
	time.AfterFunc(importSessionTTL, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if session, ok := s.sessions[id]; ok && time.Now().After(session.expires) {
			delete(s.sessions, id)
		}
	})
	return id
}

func (s *importStore) get(id uuid.UUID) map[string]emclient.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	session, ok := s.sessions[id]
	if !ok {
		return nil
	}
	out := make(map[string]emclient.Account, len(session.accounts))
	for email, acct := range session.accounts {
		out[email] = acct
	}
	return out
}

func (s *importStore) remove(id uuid.UUID, emails []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return
	}
	for _, email := range emails {
		delete(session.accounts, email)
	}
	if len(session.accounts) == 0 {
		delete(s.sessions, id)
	}
}

func (s *importStore) expireLocked() {
	now := time.Now()
	for id, session := range s.sessions {
		if now.After(session.expires) {
			delete(s.sessions, id)
		}
	}
}

// PrepareEMClientImport filters parsed accounts, marks duplicates and OAuth2
// accounts as not importable, and keeps the decrypted credentials in memory
// until the import is applied or the session expires.
func (s *Service) PrepareEMClientImport(ctx context.Context, accounts []emclient.Account) (ImportPreview, error) {
	existing, err := s.List(ctx)
	if err != nil {
		return ImportPreview{}, err
	}
	existingEmails := make(map[string]bool, len(existing))
	for _, acct := range existing {
		existingEmails[strings.ToLower(acct.Email)] = true
	}

	candidates, sessionAccounts := buildImportPreview(accounts, existingEmails)
	preview := ImportPreview{Accounts: candidates}
	if len(sessionAccounts) > 0 {
		preview.ID = s.imports.put(sessionAccounts)
	}
	return preview, nil
}

func buildImportPreview(accounts []emclient.Account, existingEmails map[string]bool) ([]ImportAccount, map[string]emclient.Account) {
	byEmail := make(map[string]emclient.Account)
	for _, acct := range accounts {
		acct.Email = strings.ToLower(strings.TrimSpace(acct.Email))
		if acct.Email == "" {
			continue
		}
		previous, ok := byEmail[acct.Email]
		if !ok || (!importable(previous) && importable(acct)) {
			byEmail[acct.Email] = acct
		}
	}

	emails := make([]string, 0, len(byEmail))
	for email := range byEmail {
		emails = append(emails, email)
	}
	sort.Strings(emails)

	candidates := make([]ImportAccount, 0, len(emails))
	sessionAccounts := make(map[string]emclient.Account)
	for _, email := range emails {
		acct := byEmail[email]
		candidate := ImportAccount{
			Email:           email,
			ReceiveProtocol: acct.ReceiveProtocol,
			IMAPAddr:        acct.IMAPAddr,
			POP3Addr:        acct.POP3Addr,
			SMTPAddr:        acct.SMTPAddr,
			AuthType:        acct.AuthType,
		}
		switch {
		case existingEmails[email]:
			candidate.Reason = "duplicate"
		case acct.AuthType != "password":
			candidate.Reason = "oauth2"
		case acct.SkipCode != "":
			candidate.Reason = acct.SkipCode
		case !importable(acct):
			candidate.Reason = "invalid"
		default:
			candidate.Importable = true
			candidate.Selected = true
			sessionAccounts[email] = acct
		}
		candidates = append(candidates, candidate)
	}
	return candidates, sessionAccounts
}

// ApplyEMClientImport creates the requested accounts from a prepared session.
// Only accounts marked importable are accepted; unknown emails are skipped.
func (s *Service) ApplyEMClientImport(ctx context.Context, id uuid.UUID, emails []string) (ImportResult, error) {
	parsed := s.imports.get(id)
	if parsed == nil {
		return ImportResult{}, ErrImportSessionNotFound
	}
	existing, err := s.List(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	existingEmails := make(map[string]bool, len(existing))
	for _, acct := range existing {
		existingEmails[strings.ToLower(acct.Email)] = true
	}

	result := ImportResult{Created: []string{}, Skipped: []string{}, Failed: []ImportFailure{}}
	seen := make(map[string]bool)
	for _, raw := range emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		acct, ok := parsed[email]
		if !ok || existingEmails[email] {
			result.Skipped = append(result.Skipped, email)
			continue
		}
		if _, err := s.Create(ctx, inputFromEMClient(acct)); err != nil {
			result.Failed = append(result.Failed, ImportFailure{Email: email, Error: err.Error()})
			continue
		}
		result.Created = append(result.Created, email)
	}
	if len(result.Created) > 0 {
		s.imports.remove(id, result.Created)
	}
	return result, nil
}

func importable(acct emclient.Account) bool {
	return acct.SkipCode == "" && acct.AuthType == "password" && acct.Password != ""
}

func inputFromEMClient(acct emclient.Account) Input {
	return Input{
		Name:              acct.Name,
		Email:             acct.Email,
		Provider:          acct.Provider,
		Username:          acct.Username,
		AuthType:          "password",
		Password:          acct.Password,
		ReceiveProtocol:   acct.ReceiveProtocol,
		IMAPAddr:          acct.IMAPAddr,
		IMAPSecurity:      acct.IMAPSecurity,
		POP3Addr:          acct.POP3Addr,
		POP3Security:      acct.POP3Security,
		SMTPAddr:          acct.SMTPAddr,
		SMTPSecurity:      acct.SMTPSecurity,
		SMTPUsername:      acct.SMTPUsername,
		SMTPPassword:      acct.SMTPPassword,
		SyncEnabled:       acct.SyncEnabled,
		InitialSyncRange:  "all",
		DownloadScope:     "full_no_attachments",
		SentCopyMode:      "auto",
		POP3LeaveOnServer: true,
	}
}
