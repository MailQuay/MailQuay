package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// ErrNotFound is returned for missing identities.
var ErrNotFound = errors.New("identity not found")

// ErrDefaultNotDeletable protects the account default identity.
var ErrDefaultNotDeletable = errors.New("default identity cannot be deleted")

// Input is the editable identity shape.
type Input struct {
	Email         string
	Name          string
	Organization  string
	ReplyTo       string
	Bcc           []string
	SignatureHTML string
	SignatureText string
	SignatureMode string
	IsDefault     bool
}

// Service manages sender identities.
type Service struct {
	store *store.Store
}

// NewService builds the identity service.
func NewService(st *store.Store) *Service { return &Service{store: st} }

// List returns identities for an account.
func (s *Service) List(ctx context.Context, accountID uuid.UUID) ([]sqlcgen.Identity, error) {
	return s.store.ListIdentities(ctx, accountID)
}

// Get loads one identity by email.
func (s *Service) Get(ctx context.Context, accountID uuid.UUID, email string) (sqlcgen.Identity, error) {
	id, err := s.store.GetIdentity(ctx, sqlcgen.GetIdentityParams{AccountID: accountID, Lower: email})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Identity{}, ErrNotFound
		}
		return sqlcgen.Identity{}, fmt.Errorf("load identity: %w", err)
	}
	return id, nil
}

// Create adds an identity and optionally makes it the default.
func (s *Service) Create(ctx context.Context, accountID uuid.UUID, in Input) (sqlcgen.Identity, error) {
	normalized, err := normalize(in)
	if err != nil {
		return sqlcgen.Identity{}, err
	}
	var created sqlcgen.Identity
	err = s.store.WithTx(ctx, func(q *sqlcgen.Queries) error {
		count, err := q.CountIdentities(ctx, accountID)
		if err != nil {
			return err
		}
		isDefault := normalized.IsDefault || count == 0
		if isDefault {
			if err := q.ClearDefaultIdentity(ctx, accountID); err != nil {
				return err
			}
		}
		created, err = q.CreateIdentity(ctx, sqlcgen.CreateIdentityParams{
			AccountID:     accountID,
			Email:         normalized.Email,
			Name:          normalized.Name,
			Organization:  normalized.Organization,
			ReplyTo:       normalized.ReplyTo,
			Bcc:           normalized.Bcc,
			SignatureHtml: normalized.SignatureHTML,
			SignatureText: normalized.SignatureText,
			SignatureMode: normalized.SignatureMode,
			IsDefault:     isDefault,
		})
		return err
	})
	if err != nil {
		return sqlcgen.Identity{}, fmt.Errorf("create identity: %w", err)
	}
	return created, nil
}

// Update changes an existing identity.
func (s *Service) Update(ctx context.Context, accountID uuid.UUID, email string, in Input) (sqlcgen.Identity, error) {
	normalized, err := normalize(in)
	if err != nil {
		return sqlcgen.Identity{}, err
	}
	var updated sqlcgen.Identity
	err = s.store.WithTx(ctx, func(q *sqlcgen.Queries) error {
		if normalized.IsDefault {
			if err := q.ClearDefaultIdentity(ctx, accountID); err != nil {
				return err
			}
		}
		var err error
		updated, err = q.UpdateIdentity(ctx, sqlcgen.UpdateIdentityParams{
			AccountID:     accountID,
			Lower:         email,
			Name:          normalized.Name,
			Organization:  normalized.Organization,
			ReplyTo:       normalized.ReplyTo,
			Bcc:           normalized.Bcc,
			SignatureHtml: normalized.SignatureHTML,
			SignatureText: normalized.SignatureText,
			SignatureMode: normalized.SignatureMode,
		})
		if err != nil {
			return err
		}
		if normalized.IsDefault {
			return q.SetDefaultIdentity(ctx, sqlcgen.SetDefaultIdentityParams{AccountID: accountID, Lower: email})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlcgen.Identity{}, ErrNotFound
		}
		return sqlcgen.Identity{}, fmt.Errorf("update identity: %w", err)
	}
	return updated, nil
}

// Delete removes a non-default identity.
func (s *Service) Delete(ctx context.Context, accountID uuid.UUID, email string) error {
	current, err := s.Get(ctx, accountID, email)
	if err != nil {
		return err
	}
	if current.IsDefault {
		return ErrDefaultNotDeletable
	}
	return s.store.DeleteIdentity(ctx, sqlcgen.DeleteIdentityParams{AccountID: accountID, Lower: email})
}

// SetDefault makes one identity the account default.
func (s *Service) SetDefault(ctx context.Context, accountID uuid.UUID, email string) error {
	if _, err := s.Get(ctx, accountID, email); err != nil {
		return err
	}
	return s.store.WithTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.ClearDefaultIdentity(ctx, accountID); err != nil {
			return err
		}
		return q.SetDefaultIdentity(ctx, sqlcgen.SetDefaultIdentityParams{AccountID: accountID, Lower: email})
	})
}

// Default returns the account default identity or the lowest-sorted identity.
func (s *Service) Default(ctx context.Context, accountID uuid.UUID) (sqlcgen.Identity, error) {
	id, err := s.store.GetDefaultIdentity(ctx, accountID)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Identity{}, fmt.Errorf("load default identity: %w", err)
	}
	list, err := s.store.ListIdentities(ctx, accountID)
	if err != nil || len(list) == 0 {
		return sqlcgen.Identity{}, ErrNotFound
	}
	return list[0], nil
}

func normalize(in Input) (Input, error) {
	email, err := account.NormalizeEmail(in.Email)
	if err != nil {
		return in, err
	}
	in.Email = email
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = email
	}
	in.SignatureMode = strings.TrimSpace(in.SignatureMode)
	if in.SignatureMode == "" {
		in.SignatureMode = "all"
	}
	switch in.SignatureMode {
	case "all", "new", "reply", "forward", "none":
	default:
		return in, errors.New("signatureMode must be all, new, reply, forward or none")
	}
	for i, addr := range in.Bcc {
		_, email := account.SplitEmail(addr)
		if email == "" {
			return in, fmt.Errorf("invalid bcc address %q", addr)
		}
		in.Bcc[i] = email
	}
	return in, nil
}
