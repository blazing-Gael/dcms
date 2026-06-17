package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/core/store"
)

// migrateAudited creates a table that carries the full audit column set.
func migrateAudited(t *testing.T, a *Adapter) {
	t.Helper()
	err := a.Migrate(context.Background(), store.MigrationPlan{Up: []string{`
		CREATE TABLE notes (
			id         TEXT PRIMARY KEY,
			body       TEXT,
			created_at TEXT,
			updated_at TEXT,
			created_by TEXT,
			updated_by TEXT
		);`,
	}})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

func TestCreate_StampsActorFromContext(t *testing.T) {
	a := newTestAdapter(t)
	migrateAudited(t, a)

	ctx := store.WithActor(context.Background(), "user-123")
	rec, err := a.Create(ctx, store.WriteInput{Collection: "notes", Data: store.Record{"body": "hi"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec["created_by"] != "user-123" {
		t.Fatalf("created_by: got %#v, want user-123", rec["created_by"])
	}
	if rec["updated_by"] != "user-123" {
		t.Fatalf("updated_by: got %#v, want user-123", rec["updated_by"])
	}
}

func TestCreate_ClientCannotSpoofCreatedBy(t *testing.T) {
	a := newTestAdapter(t)
	migrateAudited(t, a)

	// A trusted actor is present; a client-supplied created_by must be ignored.
	ctx := store.WithActor(context.Background(), "real-user")
	rec, err := a.Create(ctx, store.WriteInput{
		Collection: "notes",
		Data:       store.Record{"body": "hi", "created_by": "attacker"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec["created_by"] != "real-user" {
		t.Fatalf("created_by should be the trusted actor, got %#v", rec["created_by"])
	}
}

func TestCreate_NoActor_LeavesCreatedByUnset(t *testing.T) {
	a := newTestAdapter(t)
	migrateAudited(t, a)

	rec, err := a.Create(context.Background(), store.WriteInput{Collection: "notes", Data: store.Record{"body": "hi"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := rec["created_by"]; ok {
		t.Fatalf("created_by should be unset without an actor, got %#v", rec["created_by"])
	}
}

func TestUpdate_StampsUpdatedByNotCreatedBy(t *testing.T) {
	a := newTestAdapter(t)
	migrateAudited(t, a)

	created, err := a.Create(store.WithActor(context.Background(), "creator"),
		store.WriteInput{Collection: "notes", Data: store.Record{"body": "v1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created["id"].(string)

	updated, err := a.Update(store.WithActor(context.Background(), "editor"),
		store.WriteInput{Collection: "notes", Data: store.Record{"id": id, "body": "v2"}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated["created_by"] != "creator" {
		t.Fatalf("created_by must be preserved, got %#v", updated["created_by"])
	}
	if updated["updated_by"] != "editor" {
		t.Fatalf("updated_by: got %#v, want editor", updated["updated_by"])
	}
}

func TestCreate_DefaultIDIsUUIDv7(t *testing.T) {
	a := newTestAdapter(t)
	migrateProducts(t, a)

	rec, err := a.Create(context.Background(), store.WriteInput{Collection: "products", Data: store.Record{"title": "x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := rec["id"].(string)
	// UUID v7: 36 chars, version nibble '7' at index 14.
	if len(id) != 36 || id[14] != '7' {
		t.Fatalf("expected a v7 UUID, got %q", id)
	}
}

func TestConfig_CustomIDGeneratorIsUsed(t *testing.T) {
	n := 0
	a, err := New(Config{
		Path: ":memory:",
		IDGen: func() (string, error) {
			n++
			return "note_" + string(rune('a'+n-1)), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ad := a.(*Adapter)
	migrateProducts(t, ad)

	rec, err := ad.Create(context.Background(), store.WriteInput{Collection: "products", Data: store.Record{"title": "x"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := rec["id"].(string); !strings.HasPrefix(got, "note_") {
		t.Fatalf("custom IDGen not used: id=%q", got)
	}
}
