package gateway

import (
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// passwordCost is the bcrypt cost for new hashes. Tests lower it (export_test.go):
// at the default cost, seeding and logging in users dominates the race-enabled
// suite and pushed it past the 10-minute test timeout on macOS CI.
var passwordCost = bcrypt.DefaultCost

// hashPassword returns a bcrypt hash suitable for storage in _users.password_hash.
// bcrypt embeds the cost and salt in the output, so verification needs nothing
// but the stored string.
func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), passwordCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// dummyHash is a valid bcrypt hash at passwordCost, computed once on first use. It lets checkPassword spend the same work on the no-account / no-local-
// password path as on a real verification, so an attacker can't tell from
// response timing whether an email has a local account (GHSA-38cf-62wh-v3g3).
var dummyHash = sync.OnceValue(func() []byte {
	// The input is irrelevant — callers never match against it, it only exists to
	// make CompareHashAndPassword do a full bcrypt round. A valid cost can't error.
	b, err := bcrypt.GenerateFromPassword([]byte("dcms constant-time placeholder"), passwordCost)
	if err != nil {
		panic("bcrypt dummy hash: " + err.Error())
	}
	return b
})

// checkPassword reports whether plain matches a stored bcrypt hash. A false is
// returned for any mismatch or malformed hash — callers must not distinguish the
// two to a client (both are "invalid credentials").
//
// When hash is empty (no such user, or an externally-authenticated account with
// no local password), it still runs one bcrypt comparison against a dummy hash
// and returns false, so the verification path takes constant time regardless of
// whether the account exists. Callers on the auth hot path must therefore call
// checkPassword unconditionally, not behind a `hash == ""` short-circuit.
func checkPassword(hash, plain string) bool {
	if hash == "" {
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(plain))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
