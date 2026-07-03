package store

import "context"

// ctxKey is a private context key type so values set here can't collide with
// keys from other packages.
type ctxKey int

const actorKey ctxKey = iota

// WithActor returns a context carrying the id of the principal performing the
// operation. Adapters use it to stamp created_by / updated_by and to attribute
// audit-trail entries.
//
// This is identity for ATTRIBUTION ONLY — it answers "who did this" for audit,
// never "are they allowed" (that authorization decision belongs to the gateway,
// above the store). The actor is set by a trusted caller (the HTTP gateway from a
// verified principal, or the plugin host from a plugin's assigned identity); it
// is never taken from untrusted client input.
func WithActor(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, actorKey, actorID)
}

// ActorFromContext returns the actor id set by WithActor, or "" if none (an
// unauthenticated or system-internal operation).
func ActorFromContext(ctx context.Context) string {
	s, _ := ctx.Value(actorKey).(string)
	return s
}
