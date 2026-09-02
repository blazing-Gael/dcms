package schema

// AuthTokensCollection is the engine-managed store for single-use account tokens
// (ADR-0019 phase 2): password reset today, email verification / invites later.
// Injected like _sessions and not JSON-CRUD routable; the raw token is never
// stored — only its hash, exactly as sessions do.
const AuthTokensCollection = "_auth_tokens"

// _auth_tokens field names.
const (
	AuthTokenHash      = "token_hash" // sha256 of the raw token; never the token
	AuthTokenUserID    = "user_id"    // belongs-to _users
	AuthTokenPurpose   = "purpose"    // reset | verify | invite
	AuthTokenExpiresAt = "expires_at"
	AuthTokenUsedAt    = "used_at" // set when consumed (defense in depth; rows are also deleted)
)

// _auth_tokens purpose values.
const (
	AuthTokenPurposeReset  = "reset"
	AuthTokenPurposeVerify = "verify"
	AuthTokenPurposeInvite = "invite"
)

// authTokensCollectionDef is the canonical shape of the _auth_tokens collection.
// token_hash is unique; user_id cascades so deleting a user drops their tokens.
func authTokensCollectionDef() CollectionDef {
	return CollectionDef{
		Name: AuthTokensCollection,
		Fields: []FieldDef{
			{Name: AuthTokenHash, Type: TypeString, Required: true, Unique: true},
			{Name: AuthTokenUserID, Type: TypeRelation, Target: UsersCollection, Required: true, OnDelete: "cascade"},
			{Name: AuthTokenPurpose, Type: TypeString, Required: true},
			{Name: AuthTokenExpiresAt, Type: TypeDateTime, Required: true},
			{Name: AuthTokenUsedAt, Type: TypeDateTime},
		},
		Indexes: []Index{
			{Columns: []string{AuthTokenExpiresAt}}, // for the expiry sweep
		},
	}
}

// injectAuthTokens appends the engine-managed _auth_tokens collection. Must run
// after injectAuth so its user_id relation target (_users) already exists.
func (s *SchemaDefinition) injectAuthTokens() {
	s.Collections = append(s.Collections, authTokensCollectionDef())
}
