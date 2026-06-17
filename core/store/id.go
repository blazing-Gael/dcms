package store

import "github.com/google/uuid"

// IDGenerator produces a new primary-key id for a record. It is injected into an
// adapter via its Config, so the id strategy is fully configurable — callers can
// keep the default, switch strategies, or supply their own.
//
// Whatever the strategy, ids are opaque strings: stable, portable, and safe to
// expose (see STORE_INTERFACE.md → IDs).
type IDGenerator func() (string, error)

// UUIDv7 generates a time-ordered UUID (RFC 9562 v7). This is the default.
//
// v7 embeds a millisecond timestamp prefix, so ids sort chronologically and
// insert near-sequentially — far better index locality and write performance
// than random v4. Trade-off: a v7 id reveals its creation time. For data where
// that matters, switch the collection/adapter to UUIDv4.
func UUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// UUIDv4 generates a fully random UUID — no embedded timestamp, no ordering.
// Use it when an id must not leak creation timing.
func UUIDv4() (string, error) {
	return uuid.NewString(), nil
}

// DefaultIDGenerator is used when an adapter Config leaves IDGen unset.
var DefaultIDGenerator IDGenerator = UUIDv7
