package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// ErrNotFound is returned for missing mailboxes.
var ErrNotFound = errors.New("mailbox not found")

// Counts is the global sidebar counter set.
type Counts struct {
	All    int64 `json:"all"`
	Unread int64 `json:"unread"`
	Marked int64 `json:"marked"`
	Drafts int64 `json:"drafts"`
	Sent   int64 `json:"sent"`
	Trash  int64 `json:"trash"`
}

// AccountGroup is one sidebar account section.
type AccountGroup struct {
	Account sqlcgen.Account   `json:"account"`
	Folders []sqlcgen.Mailbox `json:"folders"`
	Other   []sqlcgen.Mailbox `json:"other"`
}

// Tree is the complete sidebar model.
type Tree struct {
	Accounts []AccountGroup `json:"accounts"`
	Counts   Counts         `json:"counts"`
}

// Service serves folder data and maintains discovered mailboxes.
type Service struct {
	store *store.Store
}

// NewService builds the mailbox service.
func NewService(st *store.Store) *Service { return &Service{store: st} }

// List returns mailboxes for an account.
func (s *Service) List(ctx context.Context, accountID uuid.UUID) ([]sqlcgen.Mailbox, error) {
	return s.store.ListMailboxes(ctx, accountID)
}

// Get loads one mailbox.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (sqlcgen.Mailbox, error) {
	mb, err := s.store.GetMailbox(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Mailbox{}, ErrNotFound
		}
		return sqlcgen.Mailbox{}, fmt.Errorf("load mailbox: %w", err)
	}
	return mb, nil
}

// Tree builds the sidebar tree with global counters.
func (s *Service) Tree(ctx context.Context) (Tree, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return Tree{}, fmt.Errorf("list accounts: %w", err)
	}
	tree := Tree{Accounts: make([]AccountGroup, 0, len(accounts))}
	for _, acct := range accounts {
		folders, err := s.store.ListMailboxes(ctx, acct.ID)
		if err != nil {
			return Tree{}, fmt.Errorf("list mailboxes: %w", err)
		}
		group := AccountGroup{Account: acct, Folders: []sqlcgen.Mailbox{}, Other: []sqlcgen.Mailbox{}}
		for _, mb := range folders {
			if SystemRole(mb.Role) {
				group.Folders = append(group.Folders, mb)
			} else {
				group.Other = append(group.Other, mb)
			}
		}
		tree.Accounts = append(tree.Accounts, group)
	}
	counts, err := s.counts(ctx)
	if err != nil {
		return Tree{}, err
	}
	tree.Counts = counts
	return tree, nil
}

func (s *Service) counts(ctx context.Context) (Counts, error) {
	var c Counts
	if v, err := s.store.CountUnifiedAll(ctx); err == nil {
		c.All = v
	}
	if v, err := s.store.CountUnifiedUnread(ctx); err == nil {
		c.Unread = v
	}
	if v, err := s.store.CountMarked(ctx); err == nil {
		c.Marked = v
	}
	if v, err := s.store.CountDrafts(ctx); err == nil {
		c.Drafts = v
	}
	if v, err := s.store.CountByMailboxRole(ctx, "sent"); err == nil {
		c.Sent = v
	}
	if v, err := s.store.CountByMailboxRole(ctx, "trash"); err == nil {
		c.Trash = v
	}
	return c, nil
}

// UpsertFromFolders stores a discovered IMAP folder set and returns the
// resulting mailboxes keyed by path. All Mail folders are skipped.
func (s *Service) UpsertFromFolders(ctx context.Context, accountID uuid.UUID, folders []imapclient.Folder) ([]sqlcgen.Mailbox, error) {
	resolved := AssignRoles(folders, s.folderConfig(ctx, accountID))
	out := make([]sqlcgen.Mailbox, 0, len(folders))
	for i, folder := range folders {
		resolvedRole := resolved[i]
		if resolvedRole.Skip {
			continue
		}
		attrs, _ := json.Marshal(folder.Attributes)
		mb, err := s.store.UpsertMailbox(ctx, sqlcgen.UpsertMailboxParams{
			ID:            uuid.MustParse(platform.NewID()),
			AccountID:     accountID,
			Path:          folder.Path,
			Name:          folder.Name,
			Delimiter:     folder.Delimiter,
			Role:          resolvedRole.Role,
			Attributes:    attrs,
			LocalOnly:     false,
			SyncMode:      defaultSyncMode(resolvedRole.Role),
			Uidvalidity:   nil,
			Uidnext:       nil,
			Highestmodseq: nil,
		})
		if err != nil {
			return nil, fmt.Errorf("upsert mailbox %q: %w", folder.Path, err)
		}
		out = append(out, mb)
	}
	// A local fallback folder (Sent or Drafts) may still hold a role that a
	// remote folder now owns.
	if err := s.ApplySpecialFolders(ctx, accountID); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplySpecialFolders re-resolves the role of every stored remote folder after
// the account special folder settings change. It lets the sidebar and the
// delete target follow the new configuration without waiting for a sync pass.
func (s *Service) ApplySpecialFolders(ctx context.Context, accountID uuid.UUID) error {
	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	if acct.ReceiveProtocol != "imap" {
		return nil
	}
	rows, err := s.store.ListMailboxes(ctx, accountID)
	if err != nil {
		return fmt.Errorf("list mailboxes: %w", err)
	}
	folders := make([]imapclient.Folder, 0, len(rows))
	remote := make([]sqlcgen.Mailbox, 0, len(rows))
	for _, mb := range rows {
		if mb.LocalOnly {
			continue
		}
		var attrs []string
		_ = json.Unmarshal(mb.Attributes, &attrs)
		folders = append(folders, imapclient.Folder{
			Path: mb.Path, Name: mb.Name, Delimiter: mb.Delimiter, Attributes: attrs,
		})
		remote = append(remote, mb)
	}
	cfg := FolderConfig{AutoDetect: acct.AutoDetectSpecialFolders, SpecialFolders: ParseSpecialFolders(acct.SpecialFolders)}
	resolved := AssignRoles(folders, cfg)
	claimed := make(map[string]struct{}, len(SpecialRoleNames))
	for i, role := range resolved {
		if role.Skip {
			continue
		}
		if remote[i].Role != role.Role {
			if err := s.store.SetMailboxRole(ctx, sqlcgen.SetMailboxRoleParams{ID: remote[i].ID, Role: role.Role}); err != nil {
				return fmt.Errorf("set mailbox role: %w", err)
			}
		}
		if IsSpecialRole(role.Role) {
			claimed[role.Role] = struct{}{}
		}
	}
	for _, mb := range rows {
		if !mb.LocalOnly || !IsSpecialRole(mb.Role) {
			continue
		}
		if _, ok := claimed[mb.Role]; !ok {
			continue
		}
		if err := s.store.SetMailboxRole(ctx, sqlcgen.SetMailboxRoleParams{ID: mb.ID, Role: "custom"}); err != nil {
			return fmt.Errorf("demote local mailbox: %w", err)
		}
	}
	return nil
}

// folderConfig loads the per-account special folder configuration. Missing
// accounts keep automatic detection with no manual choices.
func (s *Service) folderConfig(ctx context.Context, accountID uuid.UUID) FolderConfig {
	cfg := FolderConfig{AutoDetect: true, SpecialFolders: map[string]string{}}
	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return cfg
	}
	cfg.AutoDetect = acct.AutoDetectSpecialFolders
	cfg.SpecialFolders = ParseSpecialFolders(acct.SpecialFolders)
	return cfg
}

// EnsureMailbox creates a local folder when it does not exist.
func (s *Service) EnsureMailbox(ctx context.Context, accountID uuid.UUID, role, path, name string, localOnly bool) (sqlcgen.Mailbox, error) {
	existing, err := s.store.GetMailboxByPath(ctx, sqlcgen.GetMailboxByPathParams{AccountID: accountID, Path: path})
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Mailbox{}, fmt.Errorf("lookup mailbox: %w", err)
	}
	attrs, _ := json.Marshal([]string{})
	return s.store.UpsertMailbox(ctx, sqlcgen.UpsertMailboxParams{
		ID:            uuid.MustParse(platform.NewID()),
		AccountID:     accountID,
		Path:          path,
		Name:          name,
		Delimiter:     "/",
		Role:          role,
		Attributes:    attrs,
		LocalOnly:     localOnly,
		SyncMode:      defaultSyncMode(role),
		Uidvalidity:   nil,
		Uidnext:       nil,
		Highestmodseq: nil,
	})
}

// RefreshCounts recomputes total and unread for a mailbox.
func (s *Service) RefreshCounts(ctx context.Context, mailboxID uuid.UUID) (sqlcgen.Mailbox, error) {
	mb, err := s.Get(ctx, mailboxID)
	if err != nil {
		return sqlcgen.Mailbox{}, err
	}
	total, err := s.store.CountMessagesInMailbox(ctx, mailboxID)
	if err != nil {
		return sqlcgen.Mailbox{}, fmt.Errorf("count messages: %w", err)
	}
	unread, err := s.store.CountUnreadInMailbox(ctx, mailboxID)
	if err != nil {
		return sqlcgen.Mailbox{}, fmt.Errorf("count unread: %w", err)
	}
	if err := s.store.UpdateMailboxCounts(ctx, sqlcgen.UpdateMailboxCountsParams{ID: mailboxID, Total: int32(total), Unread: int32(unread)}); err != nil {
		return sqlcgen.Mailbox{}, fmt.Errorf("update counts: %w", err)
	}
	mb.Total = int32(total)
	mb.Unread = int32(unread)
	return mb, nil
}

func defaultSyncMode(role string) string {
	if role == "sent" {
		return "pull"
	}
	return "full"
}
