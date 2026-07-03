package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// DefaultBaseURL is used when meta.base_url is empty.
const DefaultBaseURL = "/api/v1"

// ContractHash is a stable fingerprint of the schema. Any change to the schema
// changes the hash, letting clients detect that a generated SDK/spec no longer
// matches the live server. SchemaDefinition contains only structs and slices (no
// maps), so its JSON encoding — and therefore this hash — is deterministic.
func (s *SchemaDefinition) ContractHash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// ContractVersion combines the schema version with the contract hash, e.g.
// "1+9f2c0a1b3d4e5f60". Used as the OpenAPI info.version and stamped into SDKs.
func (s *SchemaDefinition) ContractVersion() string {
	v := s.Version
	if v == "" {
		v = "1"
	}
	return v + "+" + s.ContractHash()
}

// BaseURL returns the API base path: meta.base_url (sanitized) or the default.
// A clean prefix has a leading slash and no trailing slash.
func (s *SchemaDefinition) BaseURL() string {
	b := strings.TrimSpace(s.Meta.BaseURL)
	if b == "" {
		return DefaultBaseURL
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return strings.TrimRight(b, "/")
}
