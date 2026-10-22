// Package sign is the signed-URL wire contract for DCMS media delivery (ADR-0037).
//
// A signed URL lets an authorized caller hand a private media path to something that
// cannot present credentials — an <img>/<video> tag, an email link, a CDN edge. The
// access check runs once when the URL is minted; verification afterwards is a keyed
// hash plus an expiry check, with no database and no session, so it is cheap enough
// per video segment and a CDN can cache the bytes (the cache key excludes the token).
//
// Like pkg/auth, this package has no dependency on the rest of DCMS: the token format
// is a documented contract (a CDN edge or a standalone streaming module may verify it
// independently), so it stays importable on its own and is covered by conformance
// tests. The format is versioned; once published it changes only compatibly.
//
// Token format (v1):
//
//	token   = base64url(payload) "." base64url(mac)
//	payload = exp "\n" scope "\n" principal      (principal empty ⇒ shareable)
//	mac     = HMAC-SHA256(secret, id "\n" payload)
//	exp     = decimal unix seconds
//
// Both halves are base64url (no padding), so a token is a single URL path segment and
// the "." separator is path-safe. scope is a path *prefix*: a token authorizes any
// path under it (e.g. scope "hls/" covers a whole ladder), so one token serves a
// playlist and all its segments.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// b64 is unpadded base64url — URL/path safe, no '/', '+' or '='.
var b64 = base64.RawURLEncoding

// Signer mints and verifies tokens. It holds a current secret and any number of
// previous ones: Sign always uses the current secret, Verify accepts any of them, so
// rotating a secret in (dropping the oldest) revokes every URL signed with it without
// a flag day. A Signer is immutable and safe for concurrent use.
type Signer struct {
	secrets [][]byte // [0] = current (used to sign); all are accepted on verify
}

// New returns a Signer whose current secret is `current` and whose `previous` secrets
// are still accepted on verify (for rotation). Empty secrets are ignored; New returns
// nil if no non-empty secret is supplied, so a caller can treat nil as "signing off".
func New(current string, previous ...string) *Signer {
	var secrets [][]byte
	for _, s := range append([]string{current}, previous...) {
		if s != "" {
			secrets = append(secrets, []byte(s))
		}
	}
	if len(secrets) == 0 {
		return nil
	}
	return &Signer{secrets: secrets}
}

// Sign returns a token authorizing fetches of id's files under the scope prefix until
// exp. principal is baked into the signature when non-empty (a caller-bound token for
// leak tracing / watermarking); empty means a shareable token, so every authorized
// viewer gets the identical URL and a CDN can share one cache entry. Verification does
// not require a session either way — binding changes the signature and is recorded on
// serve, it is not enforced at the edge.
func (s *Signer) Sign(id, scope string, exp time.Time, principal string) string {
	payload := encodePayload(exp, scope, principal)
	mac := s.mac(s.secrets[0], id, payload)
	return b64.EncodeToString([]byte(payload)) + "." + b64.EncodeToString(mac)
}

// Verify reports whether token authorizes fetching id's file at path at time `now`.
// It verifies the MAC under any accepted secret (constant-time), then — only once the
// payload is trusted — checks the expiry and that path falls under the signed scope.
func (s *Signer) Verify(id, path, token string, now time.Time) bool {
	rawPayload, mac, ok := splitToken(token)
	if !ok {
		return false
	}
	if !s.macValid(id, rawPayload, mac) {
		return false
	}
	exp, scope, _, ok := decodePayload(rawPayload)
	if !ok {
		return false
	}
	if !now.Before(exp) {
		return false // expired (exp is exclusive)
	}
	return strings.HasPrefix(path, scope)
}

// mac computes HMAC-SHA256(secret, id "\n" payload).
func (s *Signer) mac(secret []byte, id, payload string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(id))
	h.Write([]byte("\n"))
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// macValid reports whether mac matches the payload under any accepted secret, in
// constant time (no early exit that leaks which secret, or that a prefix matched).
func (s *Signer) macValid(id, payload string, mac []byte) bool {
	ok := false
	for _, secret := range s.secrets {
		if hmac.Equal(s.mac(secret, id, payload), mac) {
			ok = true
		}
	}
	return ok
}

// encodePayload joins the signed fields. scope and principal never contain "\n"
// (scope is a path prefix, principal an id), so newline is an unambiguous separator.
func encodePayload(exp time.Time, scope, principal string) string {
	return strconv.FormatInt(exp.Unix(), 10) + "\n" + scope + "\n" + principal
}

func decodePayload(payload string) (exp time.Time, scope, principal string, ok bool) {
	parts := strings.SplitN(payload, "\n", 3)
	if len(parts) != 3 {
		return time.Time{}, "", "", false
	}
	secs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", "", false
	}
	return time.Unix(secs, 0), parts[1], parts[2], true
}

// splitToken decodes a token into its raw payload string and MAC bytes.
func splitToken(token string) (payload string, mac []byte, ok bool) {
	p, m, found := strings.Cut(token, ".")
	if !found {
		return "", nil, false
	}
	pb, err := b64.DecodeString(p)
	if err != nil {
		return "", nil, false
	}
	mb, err := b64.DecodeString(m)
	if err != nil {
		return "", nil, false
	}
	return string(pb), mb, true
}

// BucketExpiry rounds now+ttl up to the next multiple of window (from the epoch), so
// repeated signs within the same window return the identical expiry — and therefore
// the identical token — which keeps browser and CDN caching effective. A zero or
// negative window returns now+ttl unrounded.
func BucketExpiry(now time.Time, ttl, window time.Duration) time.Time {
	exp := now.Add(ttl)
	if window <= 0 {
		return exp
	}
	return exp.Truncate(window).Add(window)
}
