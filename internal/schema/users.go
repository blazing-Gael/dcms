package schema

// Engine-managed identity collections (ADR-0016). Both are reserved (users can't
// declare them) and injected like _media; neither is JSON-CRUD routable (the
// leading underscore excludes them). _users is administered through the auth
// surface (login / dcms admin); _sessions is never client-writable.
const (
	UsersCollection    = "_users"
	SessionsCollection = "_sessions"
)

// _users field names, shared between the injected collection and the gateway's
// auth handlers so the two stay in lockstep.
const (
	UserEmail        = "email"
	UserPasswordHash = "password_hash" // never serialized in a response
	UserRoles        = "roles"         // JSON list of role names
	UserName         = "name"
	UserStatus       = "status" // active | disabled (empty treated as active)
)

// _users status values (ADR-0019). An empty/absent status is treated as active
// so the additive migration needs no backfill.
const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// _sessions field names.
const (
	SessionTokenHash = "token_hash" // sha256 of the opaque token; never the token itself
	SessionUserID    = "user_id"    // belongs-to _users
	SessionExpiresAt = "expires_at"
)

// usersCollectionDef is the canonical shape of the _users collection. email is
// the login identifier (unique); password_hash is optional so externally
// authenticated users (OIDC, later) can exist without one. roles is a JSON list.
func usersCollectionDef() CollectionDef {
	return CollectionDef{
		Name: UsersCollection,
		Fields: []FieldDef{
			{Name: UserEmail, Type: TypeString, Required: true, Unique: true},
			{Name: UserPasswordHash, Type: TypeString},
			{Name: UserRoles, Type: TypeJSON},
			{Name: UserName, Type: TypeString},
			{Name: UserStatus, Type: TypeString}, // ADR-0019; empty ⇒ active
		},
	}
}

// sessionsCollectionDef is the canonical shape of the _sessions collection. Only
// the token hash is stored (never the opaque token); the belongs-to user_id
// cascades so deleting a user revokes their sessions.
func sessionsCollectionDef() CollectionDef {
	return CollectionDef{
		Name: SessionsCollection,
		Fields: []FieldDef{
			{Name: SessionTokenHash, Type: TypeString, Required: true, Unique: true},
			{Name: SessionUserID, Type: TypeRelation, Target: UsersCollection, Required: true, OnDelete: "cascade"},
			{Name: SessionExpiresAt, Type: TypeDateTime, Required: true},
		},
		Indexes: []Index{
			{Columns: []string{SessionTokenHash}},
		},
	}
}

// injectAuth appends the engine-managed identity collections. Like _media they
// are injected unconditionally in M1 (provider defaults to local): identity is a
// standalone feature, so an operator can create users and log in even before any
// collection declares an `access:` rule. Appended last, so user collection
// indices stay stable. Must run after _media/_revisions so _users can be a
// relation target if ever needed, and after Validate (injected names are
// reserved and would otherwise trip the reserved-name check).
func (s *SchemaDefinition) injectAuth() {
	s.Collections = append(s.Collections, usersCollectionDef(), sessionsCollectionDef())
}
