package schema

// IdempotencyCollection is the engine-managed store backing idempotent writes
// (ADR-0018). Like _sessions it is injected unconditionally and, with its leading
// underscore, is not JSON-CRUD routable — it is written only by the gateway's
// idempotency machinery, never by a client.
const IdempotencyCollection = "_idempotency"

// _idempotency field names, shared between the injected collection and the
// gateway that reserves/finalizes rows.
const (
	IdempKey          = "key"           // hash(principal ‖ raw Idempotency-Key); unique
	IdempFingerprint  = "fingerprint"   // hash(method ‖ path ‖ body) — detects key reuse
	IdempStatus       = "status"        // in_progress | done
	IdempResponseCode = "response_code" // the HTTP status to replay
	IdempResponseBody = "response_body" // the exact JSON body to replay
	IdempExpiresAt    = "expires_at"    // TTL horizon; expired rows are treated as absent
)

// _idempotency status values.
const (
	IdempStatusInProgress = "in_progress"
	IdempStatusDone       = "done"
)

// idempotencyCollectionDef is the canonical shape of the _idempotency collection.
// key is unique — that constraint is the concurrency gate that stops a racing
// duplicate from executing twice (ADR-0018).
func idempotencyCollectionDef() CollectionDef {
	return CollectionDef{
		Name: IdempotencyCollection,
		Fields: []FieldDef{
			{Name: IdempKey, Type: TypeString, Required: true, Unique: true},
			{Name: IdempFingerprint, Type: TypeString, Required: true},
			{Name: IdempStatus, Type: TypeString, Required: true},
			{Name: IdempResponseCode, Type: TypeInteger},
			{Name: IdempResponseBody, Type: TypeText},
			{Name: IdempExpiresAt, Type: TypeDateTime, Required: true},
		},
		// Indexed by expiry so the periodic sweep of dead rows is cheap.
		Indexes: []Index{
			{Columns: []string{IdempExpiresAt}},
		},
	}
}

// injectIdempotency appends the engine-managed _idempotency collection. Like
// _users/_sessions it is injected after Validate (its reserved name would trip
// the reserved-name check otherwise) and appended last to keep user collection
// indices stable.
func (s *SchemaDefinition) injectIdempotency() {
	s.Collections = append(s.Collections, idempotencyCollectionDef())
}
