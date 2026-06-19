package schema_test

import (
	"context"
	"testing"

	"github.com/blazing-Gael/dcms/core/schema"
	"github.com/blazing-Gael/dcms/core/store"
	"github.com/blazing-Gael/dcms/core/store/sqlite"
)

// TestSchemaToDatabase is the Phase 1.2 payoff: a YAML schema, parsed and
// translated, drives real table creation and CRUD through the store adapter —
// with no hand-written SQL or hand-built CollectionMeta anywhere.
func TestSchemaToDatabase(t *testing.T) {
	const src = `
version: "1"
collections:
  products:
    fields:
      title:
        type: string
        required: true
      slug:
        type: string
        unique: true
      price:
        type: number
      status:
        type: enum
        values: [draft, active]
        default: draft
    indexes: [status]
`
	def, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	db, err := sqlite.New(sqlite.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// Compile every collection: Diff → Migrate.
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			t.Fatalf("Diff %s: %v", meta.Name, err)
		}
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate %s: %v\nUp: %v", meta.Name, err, plan.Up)
		}
	}

	// The table now exists purely from YAML. CRUD should work, with the engine
	// supplying id + timestamps and the schema default applying.
	rec, err := db.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey", "slug": "honey", "price": 12.5},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec["id"] == "" || rec["created_at"] == nil {
		t.Fatalf("expected id + timestamps, got %#v", rec)
	}
	if rec["status"] != "draft" {
		t.Fatalf("enum default not applied: got %#v, want draft", rec["status"])
	}

	page, err := db.Find(ctx, store.Query{Collection: "products"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("Total: got %d, want 1", page.Total)
	}

	// The unique constraint declared in YAML must be enforced.
	_, err = db.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey 2", "slug": "honey"},
	})
	if err == nil {
		t.Fatal("expected unique-constraint violation on duplicate slug")
	}
}
