package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// apiTokenPrefix marks a long-lived machine token (issue #8), so a presented
// bearer token routes to the _api_tokens lookup instead of the _sessions one —
// one indexed query, no ambiguity, and a leaked token is recognizable in logs.
const apiTokenPrefix = "dcms_pat_"

// apiTokenLastUsedThrottle bounds how often a token's last_used_at is rewritten,
// so a busy token (an SSG polling /_changes) adds at most one write per minute to
// the read path rather than one per request.
const apiTokenLastUsedThrottle = time.Minute

// newAPIToken returns a fresh raw token (prefix + 256 bits, URL-safe). The raw
// token is shown to the operator exactly once; only its hash is stored.
func newAPIToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateAPIToken mints a machine token with the given name and roles, optionally
// expiring at expiresAt (zero ⇒ never). It stores only the hash and returns the
// raw token once, plus the stored row (whose id is the token's principal id).
func CreateAPIToken(ctx context.Context, db store.Adapter, name string, roles []string, expiresAt time.Time) (string, store.Record, error) {
	if name == "" {
		return "", nil, fmt.Errorf("token name is required")
	}
	raw, err := newAPIToken()
	if err != nil {
		return "", nil, err
	}
	if roles == nil {
		roles = []string{}
	}
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return "", nil, err
	}
	data := store.Record{
		schema.APITokenName:  name,
		schema.APITokenHash:  hashToken(raw),
		schema.APITokenRoles: string(rolesJSON),
	}
	if !expiresAt.IsZero() {
		data[schema.APITokenExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	}
	rec, err := db.Create(ctx, store.WriteInput{Collection: schema.APITokensCollection, Data: data})
	if err != nil {
		return "", nil, err
	}
	return raw, rec, nil
}

// ListAPITokens returns the stored tokens (metadata only — the hash is never
// useful to a caller and the raw token is unrecoverable).
func ListAPITokens(ctx context.Context, db store.Adapter) ([]store.Record, error) {
	page, err := db.Find(ctx, store.Query{Collection: schema.APITokensCollection, Limit: 500, SkipCount: true})
	if err != nil {
		return nil, err
	}
	for _, r := range page.Data {
		delete(r, schema.APITokenHash)
	}
	return page.Data, nil
}

// RevokeAPIToken deletes a token by id, making it stop working immediately.
func RevokeAPIToken(ctx context.Context, db store.Adapter, id string) error {
	return db.Delete(ctx, schema.APITokensCollection, id)
}

// resolveAPIToken maps a raw machine token to a principal: look up its hash,
// reject an expired one, and (throttled) record last_used_at so an unused token
// is visible. The principal's id is the token's own row id — so its writes stamp
// the token as actor, distinguishing machine writes from a human's.
func (a *sessionAuthenticator) resolveAPIToken(ctx context.Context, raw string) (principal, error) {
	page, err := a.db.Find(ctx, store.Query{
		Collection: schema.APITokensCollection,
		Filters:    []store.Filter{{Field: schema.APITokenHash, Operator: store.Eq, Value: hashToken(raw)}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return principal{}, err
	}
	if len(page.Data) == 0 {
		return principal{}, nil // unknown token → anonymous, not an error
	}
	tok := page.Data[0]
	if exp, _ := tok[schema.APITokenExpiresAt].(string); exp != "" && pastRFC3339(exp) {
		return principal{}, nil // expired → anonymous
	}
	id, _ := tok["id"].(string)
	a.touchAPIToken(ctx, tok)
	return principal{ID: id, Roles: rolesFromValue(tok[schema.APITokenRoles]), Authenticated: true}, nil
}

// touchAPIToken advances last_used_at, at most once per throttle window, so an
// unused token is visible without adding a write to every request.
func (a *sessionAuthenticator) touchAPIToken(ctx context.Context, tok store.Record) {
	id, _ := tok["id"].(string)
	if id == "" {
		return
	}
	now := a.now()
	if last, _ := tok[schema.APITokenLastUsedAt].(string); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && now.Sub(t) < apiTokenLastUsedThrottle {
			return
		}
	}
	_, _ = a.db.Update(ctx, store.WriteInput{Collection: schema.APITokensCollection, Data: store.Record{
		"id":                      id,
		schema.APITokenLastUsedAt: now.UTC().Format(time.RFC3339),
	}})
}
