package schema

// EventsCollection is the engine-managed, append-only change log (ADR-0021, M-B).
// One row is written per state change on a collection that opts into `events:
// true`, in the same transaction as the write. It backs the change feed and (in a
// later phase) webhook delivery. It is injected only when at least one collection
// opts in — event capture is pure overhead for a schema that never uses it.
const EventsCollection = "_events"

// Event row field names, shared between the injected collection and the gateway's
// event capture + change-feed handlers. The audit columns supply *when*
// (created_at) and *who* (created_by), so the log needs no separate timestamp or
// actor field.
const (
	EventCollection = "collection"  // the source collection name
	EventRecordID   = "record_id"   // the id of the affected record
	EventType       = "event"       // one of the Event* constants below
	EventFromStatus = "from_status" // lifecycle status before the change (nullable)
	EventToStatus   = "to_status"   // lifecycle status after the change (nullable)
)

// Event type values. Create/update/delete cover ordinary writes; the rest are the
// lifecycle transitions (ADR-0012), where the destination is the whole signal a
// publishing workflow cares about.
const (
	EventCreated     = "created"
	EventUpdated     = "updated"
	EventDeleted     = "deleted"      // hard delete (or purge of a soft-deleted row)
	EventSoftDeleted = "soft_deleted" // DELETE on a soft_delete collection (trashed)
	EventPublished   = "published"
	EventUnpublished = "unpublished"
	EventArchived    = "archived"
	EventRestored    = "restored"
)

// eventsCollectionDef is the canonical shape of the _events collection. Rows are
// read back in id order for the change feed (the primary-key index), and filtered
// by `collection` for a per-collection feed — hence the index.
func eventsCollectionDef() CollectionDef {
	return CollectionDef{
		Name: EventsCollection,
		Fields: []FieldDef{
			{Name: EventCollection, Type: TypeString, Required: true},
			{Name: EventRecordID, Type: TypeString, Required: true},
			{Name: EventType, Type: TypeString, Required: true},
			{Name: EventFromStatus, Type: TypeString},
			{Name: EventToStatus, Type: TypeString},
		},
		Indexes: []Index{
			{Columns: []string{EventCollection}},
		},
	}
}

// AnyEvents reports whether any collection opts into the change log.
func (s *SchemaDefinition) AnyEvents() bool {
	for _, c := range s.Collections {
		if c.Events {
			return true
		}
	}
	return false
}

// injectEvents appends the engine-managed _events collection when any collection
// opts in. Like _revisions (and unlike _media) it is conditional: the log is pure
// overhead for a schema that never emits events.
func (s *SchemaDefinition) injectEvents() {
	if s.AnyEvents() {
		s.Collections = append(s.Collections, eventsCollectionDef())
	}
}
