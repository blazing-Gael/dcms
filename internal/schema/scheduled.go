package schema

// ScheduledPublishesCollection is the engine-managed outbox of pending *future*
// go-lives (issue #28). A scheduled publish (a future `_published_at`) becomes
// visible when the clock passes it, with no write — so the change feed and
// webhooks, which are write-driven (ADR-0021), would never report it. When a
// publish transition sets a future `_published_at`, a marker row is enqueued here
// in that same transaction; a background worker emits a `went_live` event when the
// clock crosses `due_at` and deletes the marker (exactly-once by construction).
//
// This is the ONLY time-triggered behaviour in core — it exists solely to keep the
// change feed's promise, not to be a general scheduler (see ADR-0026). Injected
// only when at least one collection has both `publishing` and `events`.
const ScheduledPublishesCollection = "_scheduled_publishes"

// Scheduled-publish marker fields. The audit columns supply when/who enqueued it.
const (
	ScheduledCollection = "collection" // the source collection name
	ScheduledRecordID   = "record_id"  // the record whose go-live is pending
	ScheduledDueAt      = "due_at"     // when it goes live (= _published_at), RFC3339
)

func scheduledPublishesCollectionDef() CollectionDef {
	return CollectionDef{
		Name: ScheduledPublishesCollection,
		Fields: []FieldDef{
			{Name: ScheduledCollection, Type: TypeString, Required: true},
			{Name: ScheduledRecordID, Type: TypeString, Required: true},
			{Name: ScheduledDueAt, Type: TypeString, Required: true},
		},
		Indexes: []Index{
			{Columns: []string{ScheduledDueAt}},                         // worker scans due <= now
			{Columns: []string{ScheduledCollection, ScheduledRecordID}}, // reconcile by record
		},
	}
}

// AnyScheduledEvents reports whether any collection both publishes and emits
// events — the only case where a scheduled go-live needs a `went_live` event, and
// so the only case where the marker outbox is worth injecting.
func (s *SchemaDefinition) AnyScheduledEvents() bool {
	for _, c := range s.Collections {
		if c.Publishing && c.Events {
			return true
		}
	}
	return false
}

// injectScheduledPublishes appends the engine-managed go-live outbox when any
// collection both publishes and emits events. Conditional, like _events itself.
func (s *SchemaDefinition) injectScheduledPublishes() {
	if s.AnyScheduledEvents() {
		s.Collections = append(s.Collections, scheduledPublishesCollectionDef())
	}
}
