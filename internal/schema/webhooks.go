package schema

// Webhook delivery collections (ADR-0021, M-B phase 2). These back the delivery
// subsystem that pushes _events to configured endpoints; the _events log itself
// stays webhook-agnostic. Both are injected alongside _events (when any
// collection opts into events) — they are small and stay empty until webhooks are
// configured at runtime, and webhook endpoints are runtime config, not schema, so
// there is nothing schema-time to gate on.

// WebhookDeliveriesCollection is the per-(event, endpoint) retry ledger.
const WebhookDeliveriesCollection = "_webhook_deliveries"

// WebhookStateCollection holds one row per endpoint: the enqueue cursor (the last
// event id fanned out to that endpoint), so the worker never re-scans delivered
// events and a new endpoint can start from "now" rather than replaying history.
const WebhookStateCollection = "_webhook_state"

// Delivery row fields.
const (
	WebhookDeliveryEvent     = "event_id"        // the _events row this delivers
	WebhookDeliveryEndpoint  = "endpoint"        // configured endpoint name
	WebhookDeliveryStatus    = "status"          // pending | delivered | failed | dead
	WebhookDeliveryAttempts  = "attempts"        // delivery attempts so far
	WebhookDeliveryNextAt    = "next_attempt_at" // earliest time to (re)try, RFC3339
	WebhookDeliveryLastError = "last_error"      // last failure detail (truncated)
	WebhookDeliveryDelivered = "delivered_at"    // when a 2xx was received, RFC3339
)

// Delivery status values.
const (
	WebhookPending   = "pending"
	WebhookDelivered = "delivered"
	WebhookFailed    = "failed" // transient failure, scheduled for retry
	WebhookDead      = "dead"   // exhausted max attempts (dead-letter)
)

// State row fields.
const (
	WebhookStateEndpoint = "endpoint" // the endpoint name (key)
	WebhookStateCursor   = "cursor"   // last enqueued event id
)

func webhookDeliveriesCollectionDef() CollectionDef {
	return CollectionDef{
		Name: WebhookDeliveriesCollection,
		Fields: []FieldDef{
			{Name: WebhookDeliveryEvent, Type: TypeString, Required: true},
			{Name: WebhookDeliveryEndpoint, Type: TypeString, Required: true},
			{Name: WebhookDeliveryStatus, Type: TypeString, Required: true},
			{Name: WebhookDeliveryAttempts, Type: TypeInteger},
			{Name: WebhookDeliveryNextAt, Type: TypeString},
			{Name: WebhookDeliveryLastError, Type: TypeText},
			{Name: WebhookDeliveryDelivered, Type: TypeString},
		},
		Indexes: []Index{
			// The deliver scan: due rows for an endpoint by status + schedule.
			{Columns: []string{WebhookDeliveryEndpoint, WebhookDeliveryStatus, WebhookDeliveryNextAt}},
		},
	}
}

func webhookStateCollectionDef() CollectionDef {
	return CollectionDef{
		Name: WebhookStateCollection,
		Fields: []FieldDef{
			{Name: WebhookStateEndpoint, Type: TypeString, Required: true, Unique: true},
			{Name: WebhookStateCursor, Type: TypeString},
		},
	}
}

// injectWebhooks appends the webhook-delivery collections when any collection
// emits events. Called after injectEvents.
func (s *SchemaDefinition) injectWebhooks() {
	if s.AnyEvents() {
		s.Collections = append(s.Collections,
			webhookDeliveriesCollectionDef(),
			webhookStateCollectionDef())
	}
}
