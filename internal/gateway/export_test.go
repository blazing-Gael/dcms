package gateway

import "golang.org/x/crypto/bcrypt"

// Tests hash many passwords (every seeded user and login); the minimum bcrypt cost
// keeps the race-enabled suite well inside the test timeout. Production keeps
// bcrypt.DefaultCost.
func init() { passwordCost = bcrypt.MinCost }
