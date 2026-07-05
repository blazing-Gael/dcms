package sqlite

import (
	"context"
	"testing"

	"github.com/blazing-Gael/dcms/internal/store"
)

// TestFilter_IsNullNotNull covers the null-testing operators added for lifecycle
// filtering (ADR-0012): they must select rows by whether a nullable column holds
// SQL NULL, without binding a value.
func TestFilter_IsNullNotNull(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	if err := a.Migrate(ctx, store.MigrationPlan{Up: []string{`
		CREATE TABLE items (
			id         TEXT PRIMARY KEY,
			deleted_at TEXT,
			created_at TEXT,
			updated_at TEXT
		);`}}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	live, err := a.Create(ctx, store.WriteInput{Collection: "items", Data: store.Record{}})
	if err != nil {
		t.Fatalf("create live: %v", err)
	}
	trashed, err := a.Create(ctx, store.WriteInput{Collection: "items", Data: store.Record{"deleted_at": "2026-01-01T00:00:00Z"}})
	if err != nil {
		t.Fatalf("create trashed: %v", err)
	}

	isNull, err := a.Find(ctx, store.Query{
		Collection: "items",
		Filters:    []store.Filter{{Field: "deleted_at", Operator: store.IsNull}},
	})
	if err != nil {
		t.Fatalf("IsNull find: %v", err)
	}
	if len(isNull.Data) != 1 || isNull.Data[0]["id"] != live["id"] {
		t.Fatalf("IsNull should return only the live row, got %#v", isNull.Data)
	}

	notNull, err := a.Find(ctx, store.Query{
		Collection: "items",
		Filters:    []store.Filter{{Field: "deleted_at", Operator: store.NotNull}},
	})
	if err != nil {
		t.Fatalf("NotNull find: %v", err)
	}
	if len(notNull.Data) != 1 || notNull.Data[0]["id"] != trashed["id"] {
		t.Fatalf("NotNull should return only the trashed row, got %#v", notNull.Data)
	}
}
