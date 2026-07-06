package schema

// RevisionsCollection is the engine-managed, reserved collection that stores
// append-only version history (ADR-0013). It is injected only when at least one
// collection opts into `revisions: true`.
const RevisionsCollection = "_revisions"

// Revision row field names, shared between the injected collection and the
// gateway's revision handlers.
const (
	RevisionCollection = "collection" // the source collection name
	RevisionRecordID   = "record_id"  // the id of the record this version belongs to
	RevisionVersion    = "version"    // per-record monotonic counter (1 = create)
	RevisionOperation  = "operation"  // create | update | publish | ... | restore | delete
	RevisionData       = "data"       // full JSON snapshot of the record at this version
)

// revisionsCollectionDef is the canonical shape of the _revisions collection. The
// audit columns supply who (created_by) and when (created_at); an index on
// (collection, record_id, version) lets the gateway find a record's latest
// version cheaply. Version density is guaranteed by the gateway, not a DB unique
// constraint.
func revisionsCollectionDef() CollectionDef {
	return CollectionDef{
		Name: RevisionsCollection,
		Fields: []FieldDef{
			{Name: RevisionCollection, Type: TypeString, Required: true},
			{Name: RevisionRecordID, Type: TypeString, Required: true},
			{Name: RevisionVersion, Type: TypeInteger, Required: true},
			{Name: RevisionOperation, Type: TypeString},
			{Name: RevisionData, Type: TypeJSON},
		},
		Indexes: []Index{
			{Columns: []string{RevisionCollection, RevisionRecordID, RevisionVersion}},
		},
	}
}

// AnyRevisions reports whether any collection opts into version history.
func (s *SchemaDefinition) AnyRevisions() bool {
	for _, c := range s.Collections {
		if c.Revisions {
			return true
		}
	}
	return false
}

// injectRevisions appends the engine-managed _revisions collection when any
// collection opts in. Unlike _media it is not injected unconditionally: history
// storage is pure overhead for a schema that never uses it.
func (s *SchemaDefinition) injectRevisions() {
	if s.AnyRevisions() {
		s.Collections = append(s.Collections, revisionsCollectionDef())
	}
}
