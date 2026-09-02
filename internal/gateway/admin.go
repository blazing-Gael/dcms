package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Bootstrap helpers (ADR-0016). These write directly to the store — used by the
// `dcms admin create` CLI command and the first-run env seed — so a fresh backend
// is never locked out and no credential lands in a committed config file.

// ErrUserExists is returned by CreateUser when the email is already taken.
var ErrUserExists = errors.New("a user with that email already exists")

// ErrInvalidEmail is returned by CreateUser when the email is not a valid address.
var ErrInvalidEmail = errors.New("a valid email is required")

// CreateUser hashes password and inserts a _users row with the given roles. The
// email is normalized (trimmed + lower-cased) so accounts are case-insensitive
// and a header-injection payload can't reach the mailer. roles is stored as a
// JSON list. It fails with ErrUserExists if the (normalized) email is taken.
func CreateUser(ctx context.Context, db store.Adapter, email, password string, roles []string) (store.Record, error) {
	if password == "" {
		return nil, errors.New("email and password are required")
	}
	email, ok := normalizeEmail(email)
	if !ok {
		return nil, ErrInvalidEmail
	}
	page, err := db.Find(ctx, store.Query{
		Collection: schema.UsersCollection,
		Filters:    []store.Filter{{Field: schema.UserEmail, Operator: store.Eq, Value: email}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Data) > 0 {
		return nil, ErrUserExists
	}

	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	if roles == nil {
		roles = []string{}
	}
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return nil, err
	}
	return db.Create(ctx, store.WriteInput{
		Collection: schema.UsersCollection,
		Data: store.Record{
			schema.UserEmail:        email,
			schema.UserPasswordHash: hash,
			schema.UserRoles:        string(rolesJSON),
		},
	})
}

// EnsureSeedAdmin creates a first admin from env-supplied credentials, but only
// when no users exist yet — so it is safe to call on every startup and never
// overwrites or duplicates an existing account. Empty credentials mean "not
// configured" and it no-ops. The seeded user gets the "admin" role.
func EnsureSeedAdmin(ctx context.Context, db store.Adapter, email, password string, logger *slog.Logger) error {
	if email == "" || password == "" {
		return nil
	}
	page, err := db.Find(ctx, store.Query{Collection: schema.UsersCollection, Limit: 1, SkipCount: true})
	if err != nil {
		return err
	}
	if len(page.Data) > 0 {
		return nil // already bootstrapped
	}
	if _, err := CreateUser(ctx, db, email, password, []string{"admin"}); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}
	if logger != nil {
		logger.Info("seeded first admin user from environment", "email", email)
	}
	return nil
}
