package schema

// APITokensCollection is the engine-managed store for long-lived, revocable
// machine credentials (issue #8, foreseen in ADR-0016 §2). A token authenticates
// a non-human caller — an SSG build step, a CI job, a webhook receiver reading
// back the changed record — as a first-class principal with its own roles, so
// every `access:` rule applies unchanged. Like _sessions, only the token hash is
// stored, it is injected unconditionally, and it is not JSON-CRUD routable
// (managed via the `dcms token` CLI).
const APITokensCollection = "_api_tokens"

// _api_tokens field names. created_by (an audit column) records who minted the
// token; the token's own row id is what its writes stamp as actor, so an audit
// trail distinguishes "the builder did this" from "an editor did this".
const (
	APITokenName       = "name"       // human label, e.g. "ci" or "ssg-build"
	APITokenHash       = "token_hash" // sha256 of the raw token; never the token
	APITokenRoles      = "roles"      // JSON list of role names the token holds
	APITokenExpiresAt  = "expires_at" // nullable: empty ⇒ never expires
	APITokenLastUsedAt = "last_used_at"
)

// apiTokensCollectionDef is the canonical shape of the _api_tokens collection.
// token_hash is unique so a presented token resolves with one indexed lookup.
func apiTokensCollectionDef() CollectionDef {
	return CollectionDef{
		Name: APITokensCollection,
		Fields: []FieldDef{
			{Name: APITokenName, Type: TypeString, Required: true},
			{Name: APITokenHash, Type: TypeString, Required: true, Unique: true},
			{Name: APITokenRoles, Type: TypeJSON},
			{Name: APITokenExpiresAt, Type: TypeDateTime},
			{Name: APITokenLastUsedAt, Type: TypeDateTime},
		},
	}
}

// injectAPITokens appends the engine-managed _api_tokens collection. Injected
// unconditionally, like _auth_tokens — machine credentials are a standalone
// capability, not tied to any schema directive.
func (s *SchemaDefinition) injectAPITokens() {
	s.Collections = append(s.Collections, apiTokensCollectionDef())
}
