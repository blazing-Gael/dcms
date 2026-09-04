package schema

// NotificationsCollection is the engine-managed outbox for outbound account
// notifications — password-reset email today (ADR-0019), more later. It makes
// delivery durable and retried: a notification is enqueued in the request path
// and a background worker delivers it via the configured transport (ADR-0021
// phase 3). Injected unconditionally, like the other identity collections.
const NotificationsCollection = "_notifications"

// Notification row fields. The audit columns supply when/who enqueued it.
const (
	NotificationTo        = "recipient"       // destination (email address)
	NotificationKind      = "kind"            // e.g. "password_reset"
	NotificationLink      = "link"            // the action URL delivered to the user
	NotificationStatus    = "status"          // pending | failed | dead
	NotificationAttempts  = "attempts"        // delivery attempts so far
	NotificationNextAt    = "next_attempt_at" // earliest time to (re)try, RFC3339
	NotificationLastError = "last_error"      // last failure detail (truncated)
)

// Notification status values. A successfully-delivered notification is deleted
// rather than kept, so the row (which holds the raw action link) does not linger
// beyond delivery; only pending/failed/dead rows exist.
const (
	NotificationPending = "pending"
	NotificationFailed  = "failed"
	NotificationDead    = "dead"
)

func notificationsCollectionDef() CollectionDef {
	return CollectionDef{
		Name: NotificationsCollection,
		Fields: []FieldDef{
			{Name: NotificationTo, Type: TypeString, Required: true},
			{Name: NotificationKind, Type: TypeString, Required: true},
			{Name: NotificationLink, Type: TypeText},
			{Name: NotificationStatus, Type: TypeString, Required: true},
			{Name: NotificationAttempts, Type: TypeInteger},
			{Name: NotificationNextAt, Type: TypeString},
			{Name: NotificationLastError, Type: TypeText},
		},
		Indexes: []Index{
			{Columns: []string{NotificationStatus, NotificationNextAt}},
		},
	}
}

// injectNotifications appends the engine-managed _notifications outbox. Injected
// unconditionally, alongside the identity collections it serves.
func (s *SchemaDefinition) injectNotifications() {
	s.Collections = append(s.Collections, notificationsCollectionDef())
}
