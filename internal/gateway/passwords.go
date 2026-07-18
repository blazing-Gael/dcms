package gateway

import "golang.org/x/crypto/bcrypt"

// hashPassword returns a bcrypt hash suitable for storage in _users.password_hash.
// The default cost is used; bcrypt embeds the cost and salt in the output, so
// verification needs nothing but the stored string.
func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// checkPassword reports whether plain matches a stored bcrypt hash. A false is
// returned for any mismatch or malformed hash — callers must not distinguish the
// two to a client (both are "invalid credentials").
func checkPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
