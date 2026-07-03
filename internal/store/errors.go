package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors — use errors.Is() to check.
var (
	// ErrNotFound is returned by FindOne and Delete when no record matches.
	ErrNotFound = errors.New("store: record not found")

	// ErrConflict is returned when a unique constraint is violated.
	ErrConflict = errors.New("store: unique constraint violation")

	// ErrInvalidInput is returned when the caller provides malformed input
	// (e.g. unknown collection name, invalid field type in WriteInput).
	ErrInvalidInput = errors.New("store: invalid input")

	// ErrTxAborted is returned when a transaction is rolled back.
	ErrTxAborted = errors.New("store: transaction aborted")

	// ErrIntegrity is returned when a database-level referential integrity
	// constraint (a foreign key) is violated. On a create/update this signals an
	// invariant breach — the gateway validation layer should have rejected the
	// reference first (see ADR-0010) — so callers map it to a logged 500. On a
	// delete it is the RESTRICT default (the row is still referenced), which
	// callers surface as a 409.
	ErrIntegrity = errors.New("store: referential integrity violation")
)

// ValidationError is returned when field-level validation fails.
// Wraps ErrInvalidInput.
type ValidationError struct {
	Fields map[string]string // field name → error message
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "store: validation failed"
	}
	names := make([]string, 0, len(e.Fields))
	for name := range e.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %s", name, e.Fields[name]))
	}
	return "store: validation failed: " + strings.Join(parts, "; ")
}

func (e *ValidationError) Unwrap() error { return ErrInvalidInput }
