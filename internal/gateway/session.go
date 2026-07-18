package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// sessionCookie is the name of the opaque-session cookie set at login. Clients
// may instead send the token as `Authorization: Bearer <token>`.
const sessionCookie = "dcms_session"

// defaultSessionTTL is how long a session lasts when auth.session.ttl is unset.
const defaultSessionTTL = 7 * 24 * time.Hour

// newSessionToken returns a fresh, high-entropy opaque token (256 bits, URL-safe).
// The raw token is handed to the client exactly once; only its hash is stored.
func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken maps an opaque token to the value stored in _sessions.token_hash. A
// leak of the sessions table therefore does not hand out live tokens.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// sessionTokenFromRequest extracts the opaque token from either the Authorization
// bearer header (API clients) or the session cookie (browsers). Empty means the
// request is anonymous.
func sessionTokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// sessionAuthenticator resolves a principal from an opaque session token backed
// by the _sessions collection (ADR-0016). It is the M1 implementation of the
// Authenticator seam; external OIDC is a separate implementation.
type sessionAuthenticator struct {
	db  store.Adapter
	now func() time.Time
}

// NewSessionAuthenticator builds the opaque-session Authenticator over db.
func NewSessionAuthenticator(db store.Adapter) Authenticator {
	return &sessionAuthenticator{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// Authenticate looks up the request's session and, if valid and unexpired, loads
// the owning user into a principal. An absent/invalid/expired token yields the
// anonymous principal (not an error); a store failure is a real error.
func (a *sessionAuthenticator) Authenticate(r *http.Request) (principal, error) {
	tok := sessionTokenFromRequest(r)
	if tok == "" {
		return principal{}, nil
	}
	sess, err := a.findSession(r.Context(), hashToken(tok))
	if err != nil {
		return principal{}, err
	}
	if sess == nil || a.expired(sess) {
		return principal{}, nil // unknown or stale token → anonymous
	}
	userID, _ := sess[schema.SessionUserID].(string)
	if userID == "" {
		return principal{}, nil
	}
	user, err := a.db.FindOne(r.Context(), schema.UsersCollection, userID)
	if err != nil {
		// A session whose user vanished (deleted) is treated as anonymous; the
		// cascade should normally have removed it already.
		return principal{}, nil
	}
	return principal{id: userID, roles: rolesOf(user), authenticated: true}, nil
}

// findSession returns the session row for a token hash, or nil if none exists.
func (a *sessionAuthenticator) findSession(ctx context.Context, tokenHash string) (store.Record, error) {
	page, err := a.db.Find(ctx, store.Query{
		Collection: schema.SessionsCollection,
		Filters:    []store.Filter{{Field: schema.SessionTokenHash, Operator: store.Eq, Value: tokenHash}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Data) == 0 {
		return nil, nil
	}
	return page.Data[0], nil
}

// expired reports whether a session's expires_at is in the past.
func (a *sessionAuthenticator) expired(sess store.Record) bool {
	exp, _ := sess[schema.SessionExpiresAt].(string)
	if exp == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return true
	}
	return !t.After(a.now())
}

// rolesOf decodes a _users record's roles field (a JSON list, which the adapter
// may surface as []any, a JSON string, or []byte) into role-name strings.
func rolesOf(user store.Record) []string {
	switch v := user[schema.UserRoles].(type) {
	case []any:
		return stringsOf(v)
	case []string:
		return v
	case string:
		return decodeRoleList([]byte(v))
	case []byte:
		return decodeRoleList(v)
	default:
		return nil
	}
}

func decodeRoleList(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return stringsOf(out)
}

func stringsOf(vals []any) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
