package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
// useful to a caller and the raw token is unrecoverable). It pages to the end so
// the listing is complete even past the store's per-query limit.
func ListAPITokens(ctx context.Context, db store.Adapter) ([]store.Record, error) {
	var out []store.Record
	cursor := ""
	for {
		page, err := db.Find(ctx, store.Query{
			Collection: schema.APITokensCollection,
			SkipCount:  true,
			Cursor:     cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Data {
			delete(r, schema.APITokenHash)
			out = append(out, r)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// RevokeAPIToken deletes a token by id, making it stop working immediately.
func RevokeAPIToken(ctx context.Context, db store.Adapter, id string) error {
	return db.Delete(ctx, schema.APITokensCollection, id)
}

// apiTokenAuthenticator resolves a machine token (issue #8) and otherwise delegates
// to the wrapped human/interactive authenticator. Making it a wrapper — rather than
// baking it into the session source — is what lets API tokens work under ANY
// provider (session or proxy_header): the token layer is universal, the wrapped
// provider handles interactive identity.
type apiTokenAuthenticator struct {
	db   store.Adapter
	now  func() time.Time
	next Authenticator
}

// WithAPITokens layers machine-token resolution over any authenticator. A bearer
// token with the dcms_pat_ prefix is resolved against _api_tokens; everything
// else falls through to next.
func WithAPITokens(db store.Adapter, next Authenticator) Authenticator {
	return &apiTokenAuthenticator{db: db, now: func() time.Time { return time.Now().UTC() }, next: next}
}

func (a *apiTokenAuthenticator) Authenticate(r *http.Request) (principal, error) {
	if tok := sessionTokenFromRequest(r); strings.HasPrefix(tok, apiTokenPrefix) {
		return a.resolve(r.Context(), tok)
	}
	return a.next.Authenticate(r)
}

// resolve maps a raw machine token to a principal: look up its hash, reject an
// expired one, and (throttled) record last_used_at so an unused token is visible.
// The principal's id is the token's own row id — so its writes stamp the token as
// actor, distinguishing machine writes from a human's.
func (a *apiTokenAuthenticator) resolve(ctx context.Context, raw string) (principal, error) {
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
	if exp := datetimeString(tok[schema.APITokenExpiresAt]); exp != "" && pastRFC3339(exp) {
		return principal{}, nil // expired → anonymous
	}
	id, _ := tok["id"].(string)
	a.touch(ctx, tok)
	return principal{ID: id, Roles: rolesFromValue(tok[schema.APITokenRoles]), Authenticated: true}, nil
}

// touch advances last_used_at, at most once per throttle window, so an unused
// token is visible without adding a write to every request.
func (a *apiTokenAuthenticator) touch(ctx context.Context, tok store.Record) {
	id, _ := tok["id"].(string)
	if id == "" {
		return
	}
	now := a.now()
	if last := datetimeString(tok[schema.APITokenLastUsedAt]); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && now.Sub(t) < apiTokenLastUsedThrottle {
			return
		}
	}
	_, _ = a.db.Update(ctx, store.WriteInput{Collection: schema.APITokensCollection, Data: store.Record{
		"id":                      id,
		schema.APITokenLastUsedAt: now.UTC().Format(time.RFC3339),
	}})
}

// datetimeString reads a stored datetime as RFC3339 text whether the adapter
// surfaced it as a string (SQLite) or a time.Time (allowed by the store contract,
// e.g. Postgres). Without this, a time.Time value would read as "" and skip the
// expiry check and the last_used_at throttle.
func datetimeString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return ""
	}
}
