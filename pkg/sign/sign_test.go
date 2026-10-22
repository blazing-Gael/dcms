package sign

import (
	"strings"
	"testing"
	"time"
)

// Conformance suite for the signed-URL wire contract (ADR-0037). A CDN edge or a
// standalone verifier must behave exactly as these tests assert.

const (
	secret  = "test-secret-aaaaaaaaaaaaaaaaaaaa"
	secret2 = "rotated-secret-bbbbbbbbbbbbbbbbbb"
)

func TestSign_VerifyRoundTrip(t *testing.T) {
	s := New(secret)
	now := time.Unix(1_000_000, 0)
	tok := s.Sign("asset1", "hls/", now.Add(time.Hour), "")
	if !s.Verify("asset1", "hls/index.m3u8", tok, now) {
		t.Fatal("valid token for a path under scope should verify")
	}
	if !s.Verify("asset1", "hls/seg/000.ts", tok, now) {
		t.Fatal("any path under the scope prefix should verify (one token, whole ladder)")
	}
}

func TestVerify_RejectsWrongAsset(t *testing.T) {
	s := New(secret)
	now := time.Unix(1_000_000, 0)
	tok := s.Sign("asset1", "", now.Add(time.Hour), "")
	if s.Verify("asset2", "x", tok, now) {
		t.Fatal("a token minted for asset1 must not verify for asset2")
	}
}

func TestVerify_RejectsPathOutsideScope(t *testing.T) {
	s := New(secret)
	now := time.Unix(1_000_000, 0)
	tok := s.Sign("asset1", "hls/", now.Add(time.Hour), "")
	if s.Verify("asset1", "secret/original.mp4", tok, now) {
		t.Fatal("a path outside the signed scope prefix must not verify")
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	s := New(secret)
	now := time.Unix(1_000_000, 0)
	tok := s.Sign("asset1", "", now.Add(time.Hour), "")
	if s.Verify("asset1", "x", tok, now.Add(2*time.Hour)) {
		t.Fatal("an expired token must not verify")
	}
	// Exactly at exp is expired (exclusive boundary).
	if s.Verify("asset1", "x", tok, now.Add(time.Hour)) {
		t.Fatal("exp is exclusive — a token at its exact expiry must not verify")
	}
}

func TestVerify_RejectsTampered(t *testing.T) {
	s := New(secret)
	now := time.Unix(1_000_000, 0)
	tok := s.Sign("asset1", "hls/", now.Add(time.Hour), "")
	// Flip a character in the MAC half.
	bad := tok[:len(tok)-1] + flip(tok[len(tok)-1:])
	if s.Verify("asset1", "hls/x", bad, now) {
		t.Fatal("a tampered token must not verify")
	}
	if s.Verify("asset1", "hls/x", "garbage", now) {
		t.Fatal("a malformed token must not verify")
	}
}

// A secret still in the accepted set verifies; one rotated out does not.
func TestRotation(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := New(secret)
	tok := old.Sign("asset1", "", now.Add(time.Hour), "")

	// After rotation, `secret` is the previous secret — still accepted.
	rotated := New(secret2, secret)
	if !rotated.Verify("asset1", "x", tok, now) {
		t.Fatal("a token signed with the now-previous secret must still verify")
	}
	// Rotate again, dropping `secret` entirely → the old token is revoked.
	rotatedAgain := New(secret2)
	if rotatedAgain.Verify("asset1", "x", tok, now) {
		t.Fatal("a token whose secret is no longer accepted must be revoked")
	}
	// The new signer signs under the current (new) secret.
	if !rotated.Verify("asset1", "x", rotated.Sign("asset1", "", now.Add(time.Hour), ""), now) {
		t.Fatal("the current secret must sign verifiable tokens")
	}
}

// Principal binding changes the signature (so a bound URL differs from a shareable
// one and is traceable), while a shareable token stays identical across viewers.
func TestPrincipalBinding(t *testing.T) {
	s := New(secret)
	exp := time.Unix(2_000_000, 0)
	shareable := s.Sign("asset1", "", exp, "")
	bound := s.Sign("asset1", "", exp, "user-123")
	if shareable == bound {
		t.Fatal("a principal-bound token must differ from a shareable one")
	}
	// Two shareable signings with the same inputs are byte-identical (CDN-cacheable).
	if shareable != s.Sign("asset1", "", exp, "") {
		t.Fatal("shareable tokens with identical inputs must be identical")
	}
}

// Bucketing makes repeated signs within a window return the identical token.
func TestBucketExpiry_StableWithinWindow(t *testing.T) {
	window := time.Hour
	t0 := time.Unix(1_000_000, 0)
	t1 := t0.Add(20 * time.Minute) // same hour-bucket as t0+ttl
	if a, b := BucketExpiry(t0, 30*time.Minute, window), BucketExpiry(t1, 30*time.Minute, window); !a.Equal(b) {
		t.Fatalf("expiries within a window must match: %v vs %v", a, b)
	}
	s := New(secret)
	if s.Sign("a", "", BucketExpiry(t0, 30*time.Minute, window), "") !=
		s.Sign("a", "", BucketExpiry(t1, 30*time.Minute, window), "") {
		t.Fatal("bucketed tokens within a window must be identical (cacheable)")
	}
	// A later window yields a different (later) expiry.
	if !BucketExpiry(t0.Add(2*time.Hour), 30*time.Minute, window).After(BucketExpiry(t0, 30*time.Minute, window)) {
		t.Fatal("a later window must bucket to a later expiry")
	}
}

func TestNew_NilWhenNoSecret(t *testing.T) {
	if New("") != nil {
		t.Fatal("New with no secret should return nil (signing disabled)")
	}
	if New("", "") != nil {
		t.Fatal("New with only empty secrets should return nil")
	}
}

// A token must be a single URL path segment: no '/', and the only '.' is the separator.
func TestToken_IsPathSafe(t *testing.T) {
	s := New(secret)
	tok := s.Sign("asset1", "hls/sub dir/", time.Unix(2_000_000, 0), "user/with/slashes")
	if strings.ContainsAny(tok, "/+= ") {
		t.Fatalf("token must be path-safe (no / + = space), got %q", tok)
	}
	if strings.Count(tok, ".") != 1 {
		t.Fatalf("token must contain exactly one '.' separator, got %q", tok)
	}
	// Even with slashes in scope/principal, it still round-trips.
	if !s.Verify("asset1", "hls/sub dir/x", tok, time.Unix(1_000_000, 0)) {
		t.Fatal("token with slashes in scope should still verify a path under it")
	}
}

func flip(s string) string {
	if s == "A" {
		return "B"
	}
	return "A"
}
