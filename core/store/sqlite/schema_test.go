package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/blazing-Gael/dcms/core/store"
)

// desiredProducts is a CollectionMeta as the schema compiler would produce it:
// canonical type tokens, engine-managed id + timestamps, a unique index on slug.
func desiredProducts() store.CollectionMeta {
	return store.CollectionMeta{
		Name: "products",
		Columns: []store.ColumnMeta{
			{Name: "id", Type: "string", Nullable: false},
			{Name: "title", Type: "string", Nullable: false},
			{Name: "slug", Type: "string", Nullable: false},
			{Name: "price", Type: "number", Nullable: true},
			{Name: "currency", Type: "string", Nullable: true, Default: "BDT"},
			{Name: "created_at", Type: "datetime", Nullable: true},
			{Name: "updated_at", Type: "datetime", Nullable: true},
		},
		Indexes: []store.IndexMeta{
			{Columns: []string{"slug"}, Unique: true},
		},
	}
}

func TestDiff_CreatesTable_ThenCRUDWorks(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)

	plan, err := a.Diff(ctx, desiredProducts())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(plan.Up) == 0 {
		t.Fatal("Diff produced an empty plan for a missing table")
	}
	if err := a.Migrate(ctx, plan); err != nil {
		t.Fatalf("Migrate: %v\nUp: %v", err, plan.Up)
	}

	// The table now exists purely from the schema meta — Create should work,
	// generating id + timestamps.
	rec, err := a.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey", "slug": "honey", "price": 12.5},
	})
	if err != nil {
		t.Fatalf("Create after auto-migrate: %v", err)
	}
	if rec["id"] == "" || rec["created_at"] == nil {
		t.Fatalf("expected id + timestamps, got %#v", rec)
	}

	// The default we declared should have been applied by SQLite.
	if rec["currency"] != "BDT" {
		t.Fatalf("default currency: got %#v, want BDT", rec["currency"])
	}
}

func TestDiff_Idempotent_SecondRunIsEmpty(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)

	plan, err := a.Diff(ctx, desiredProducts())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if err := a.Migrate(ctx, plan); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Re-diffing the same desired shape should find nothing to do.
	plan2, err := a.Diff(ctx, desiredProducts())
	if err != nil {
		t.Fatalf("second Diff: %v", err)
	}
	if len(plan2.Up) != 0 {
		t.Fatalf("expected empty plan on second diff, got Up=%v", plan2.Up)
	}
}

func TestDiff_Additive_AddsMissingColumnAndIndex(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)

	// Start with a base table.
	base := desiredProducts()
	plan, _ := a.Diff(ctx, base)
	if err := a.Migrate(ctx, plan); err != nil {
		t.Fatalf("Migrate base: %v", err)
	}

	// Evolve the schema: add a nullable column and a new index.
	evolved := desiredProducts()
	evolved.Columns = append(evolved.Columns, store.ColumnMeta{Name: "description", Type: "text", Nullable: true})
	evolved.Indexes = append(evolved.Indexes, store.IndexMeta{Columns: []string{"title"}})

	plan2, err := a.Diff(ctx, evolved)
	if err != nil {
		t.Fatalf("Diff evolved: %v", err)
	}
	if len(plan2.Up) != 2 {
		t.Fatalf("expected 2 changes (column + index), got %d: %v", len(plan2.Up), plan2.Up)
	}
	if err := a.Migrate(ctx, plan2); err != nil {
		t.Fatalf("Migrate evolved: %v", err)
	}

	// Confirm the new column is really there.
	meta, err := a.Introspect(ctx, "products")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	found := false
	for _, col := range meta.Columns {
		if col.Name == "description" {
			found = true
		}
	}
	if !found {
		t.Fatalf("description column not found after additive migrate: %#v", meta.Columns)
	}
}

func TestIntrospect_NotFound(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)

	_, err := a.Introspect(ctx, "ghosts")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Introspect missing table: got %v, want ErrNotFound", err)
	}
}

func TestIntrospect_ReportsColumnsAndUniqueIndex(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)

	plan, _ := a.Diff(ctx, desiredProducts())
	if err := a.Migrate(ctx, plan); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	meta, err := a.Introspect(ctx, "products")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if len(meta.Columns) != 7 {
		t.Fatalf("columns: got %d, want 7", len(meta.Columns))
	}

	var sawUniqueSlug bool
	for _, idx := range meta.Indexes {
		if idx.Unique && len(idx.Columns) == 1 && idx.Columns[0] == "slug" {
			sawUniqueSlug = true
		}
	}
	if !sawUniqueSlug {
		t.Fatalf("expected a unique index on slug, got %#v", meta.Indexes)
	}
}
