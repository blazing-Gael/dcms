package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/store"
)

// migrateRelations builds a small relational schema at the DB layer: users,
// tags, posts (belongs-to users via author), and a posts_tags join table whose
// endpoints cascade on delete. It exercises the foreign-key DDL emitted by
// columnDefSQL (ADR-0010).
func migrateRelations(t *testing.T, a *Adapter) {
	t.Helper()
	metas := []store.CollectionMeta{
		{Name: "users", Columns: []store.ColumnMeta{
			{Name: "id", Type: "string"},
			{Name: "name", Type: "string", Nullable: true},
		}},
		{Name: "tags", Columns: []store.ColumnMeta{
			{Name: "id", Type: "string"},
			{Name: "name", Type: "string", Nullable: true},
		}},
		{Name: "posts", Columns: []store.ColumnMeta{
			{Name: "id", Type: "string"},
			{Name: "title", Type: "string", Nullable: true},
			{Name: "author", Type: "string", Nullable: true, References: "users"},
		}},
		{Name: "posts_tags", Columns: []store.ColumnMeta{
			{Name: "id", Type: "string"},
			{Name: "source_id", Type: "string", References: "posts", OnDelete: "cascade"},
			{Name: "target_id", Type: "string", References: "tags", OnDelete: "cascade"},
		}, Indexes: []store.IndexMeta{{Columns: []string{"source_id", "target_id"}, Unique: true}}},
	}
	ctx := context.Background()
	for _, m := range metas {
		plan, err := a.Diff(ctx, m)
		if err != nil {
			t.Fatalf("Diff %s: %v", m.Name, err)
		}
		if err := a.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate %s: %v", m.Name, err)
		}
	}
}

// Introspect must report foreign keys, and a re-Diff of the same schema must be
// a no-op — otherwise every migrate would try (and fail) to re-add the FK.
func TestFK_IntrospectRoundTripsAndDiffIsIdempotent(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateRelations(t, a)

	posts, err := a.Introspect(ctx, "posts")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	var author *store.ColumnMeta
	for i := range posts.Columns {
		if posts.Columns[i].Name == "author" {
			author = &posts.Columns[i]
		}
	}
	if author == nil || author.References != "users" {
		t.Fatalf("introspected author FK = %#v, want References=users", author)
	}

	desired := store.CollectionMeta{Name: "posts", Columns: []store.ColumnMeta{
		{Name: "id", Type: "string"},
		{Name: "title", Type: "string", Nullable: true},
		{Name: "author", Type: "string", Nullable: true, References: "users"},
	}}
	plan, err := a.Diff(ctx, desired)
	if err != nil {
		t.Fatalf("Diff (idempotent): %v", err)
	}
	if len(plan.Up) != 0 {
		t.Fatalf("re-Diff of an unchanged schema should be empty, got %#v", plan.Up)
	}
}

// Adding/changing a foreign key on an existing column can't be done by the
// additive migrator; Diff must refuse with a clear error, not drift.
func TestFK_DiffRejectsForeignKeyChangeOnExistingColumn(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	// posts.author starts with NO foreign key.
	if err := a.Migrate(ctx, store.MigrationPlan{Up: []string{
		`CREATE TABLE posts (id TEXT PRIMARY KEY, author TEXT);`,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now the schema wants author to reference users.
	desired := store.CollectionMeta{Name: "posts", Columns: []store.ColumnMeta{
		{Name: "id", Type: "string"},
		{Name: "author", Type: "string", Nullable: true, References: "users"},
	}}
	_, err := a.Diff(ctx, desired)
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("adding an FK to an existing column: got %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "table rebuild") {
		t.Fatalf("error should explain the rebuild limitation, got: %v", err)
	}
}

// Retrofitting a required column onto an existing table would fail cryptically in
// SQLite; Diff surfaces a clear error first.
func TestFK_DiffRejectsRequiredColumnOnExistingTable(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	if err := a.Migrate(ctx, store.MigrationPlan{Up: []string{
		`CREATE TABLE posts (id TEXT PRIMARY KEY, title TEXT);`,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	desired := store.CollectionMeta{Name: "posts", Columns: []store.ColumnMeta{
		{Name: "id", Type: "string"},
		{Name: "title", Type: "string", Nullable: true},
		{Name: "author", Type: "string", Nullable: false, References: "users"}, // required relation added later
	}}
	_, err := a.Diff(ctx, desired)
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("adding a required column: got %v, want ErrInvalidInput", err)
	}
}

// A dangling belongs-to insert must be refused by the database, proving that
// foreign_keys is enforced on the pooled connection actually serving the write —
// not just the one that happened to run the PRAGMA at open time.
func TestFK_DanglingReferenceRejected(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateRelations(t, a)

	_, err := a.Create(ctx, store.WriteInput{
		Collection: "posts",
		Data:       store.Record{"title": "x", "author": "does-not-exist"},
	})
	if !errors.Is(err, store.ErrIntegrity) {
		t.Fatalf("dangling FK insert: got %v, want store.ErrIntegrity", err)
	}
}

// Deleting a still-referenced parent is blocked (RESTRICT default) and surfaces
// as ErrIntegrity so the gateway can answer 409.
func TestFK_DeleteReferencedParentRejected(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateRelations(t, a)

	u, err := a.Create(ctx, store.WriteInput{Collection: "users", Data: store.Record{"name": "ann"}})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	uid := u["id"].(string)
	if _, err := a.Create(ctx, store.WriteInput{
		Collection: "posts", Data: store.Record{"title": "x", "author": uid},
	}); err != nil {
		t.Fatalf("create post: %v", err)
	}

	if err := a.Delete(ctx, "users", uid); !errors.Is(err, store.ErrIntegrity) {
		t.Fatalf("delete referenced user: got %v, want store.ErrIntegrity", err)
	}
}

// ON DELETE CASCADE on the join table cleans link rows when an endpoint is
// removed — the free orphan cleanup that motivated DB FKs on join tables.
func TestFK_JoinTableCascadesOnEndpointDelete(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateRelations(t, a)

	u, _ := a.Create(ctx, store.WriteInput{Collection: "users", Data: store.Record{"name": "ann"}})
	tag, _ := a.Create(ctx, store.WriteInput{Collection: "tags", Data: store.Record{"name": "go"}})
	post, err := a.Create(ctx, store.WriteInput{
		Collection: "posts", Data: store.Record{"title": "x", "author": u["id"]},
	})
	if err != nil {
		t.Fatalf("create post: %v", err)
	}
	if _, err := a.Create(ctx, store.WriteInput{
		Collection: "posts_tags",
		Data:       store.Record{"source_id": post["id"], "target_id": tag["id"]},
	}); err != nil {
		t.Fatalf("create link: %v", err)
	}

	if err := a.Delete(ctx, "tags", tag["id"].(string)); err != nil {
		t.Fatalf("delete linked tag should cascade, got: %v", err)
	}
	page, err := a.Find(ctx, store.Query{Collection: "posts_tags"})
	if err != nil {
		t.Fatalf("find links: %v", err)
	}
	if len(page.Data) != 0 {
		t.Fatalf("link rows should be cascade-deleted, got %d: %#v", len(page.Data), page.Data)
	}
}
