package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/blazing-Gael/dcms/core/store"
)

// migrateProducts creates a products table that mirrors a Phase 1 schema:
// engine-managed id + timestamps, plus a few typed fields.
func migrateProducts(t *testing.T, a *Adapter) {
	t.Helper()
	err := a.Migrate(context.Background(), store.MigrationPlan{
		Up: []string{`
			CREATE TABLE products (
				id         TEXT PRIMARY KEY,
				title      TEXT,
				slug       TEXT UNIQUE,
				price      REAL,
				stock      INTEGER,
				created_at TEXT,
				updated_at TEXT
			);`,
		},
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

func TestCreate_GeneratesIDAndTimestamps(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	rec, err := a.Create(ctx, store.WriteInput{
		Collection: "products",
		Data:       store.Record{"title": "Honey", "price": 12.5, "stock": int64(3)},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if id, _ := rec["id"].(string); id == "" {
		t.Fatalf("Create did not generate an id: %#v", rec)
	}
	if rec["created_at"] == nil || rec["updated_at"] == nil {
		t.Fatalf("Create did not set timestamps: %#v", rec)
	}
	if rec["title"] != "Honey" {
		t.Fatalf("title round-trip: got %#v, want %q", rec["title"], "Honey")
	}
	if rec["price"] != 12.5 {
		t.Fatalf("price round-trip: got %#v (%T), want 12.5", rec["price"], rec["price"])
	}
}

func TestCreateThenFind_RoundTrip(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	for _, title := range []string{"Honey", "Ghee", "Rice"} {
		if _, err := a.Create(ctx, store.WriteInput{
			Collection: "products",
			Data:       store.Record{"title": title, "price": 10.0},
		}); err != nil {
			t.Fatalf("Create %q: %v", title, err)
		}
	}

	page, err := a.Find(ctx, store.Query{Collection: "products", Limit: 10})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("Total: got %d, want 3", page.Total)
	}
	if len(page.Data) != 3 {
		t.Fatalf("len(Data): got %d, want 3", len(page.Data))
	}
}

func TestFind_FilterSortLimit(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	seed := []store.Record{
		{"title": "Honey", "price": 30.0, "stock": int64(5)},
		{"title": "Ghee", "price": 10.0, "stock": int64(5)},
		{"title": "Rice", "price": 20.0, "stock": int64(0)},
	}
	for _, r := range seed {
		if _, err := a.Create(ctx, store.WriteInput{Collection: "products", Data: r}); err != nil {
			t.Fatalf("seed Create: %v", err)
		}
	}

	// Filter stock == 5, sort by price descending.
	page, err := a.Find(ctx, store.Query{
		Collection: "products",
		Filters:    []store.Filter{{Field: "stock", Operator: store.Eq, Value: int64(5)}},
		Sort:       "-price",
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("Total: got %d, want 2", page.Total)
	}
	if len(page.Data) != 2 {
		t.Fatalf("len(Data): got %d, want 2", len(page.Data))
	}
	if page.Data[0]["title"] != "Honey" || page.Data[1]["title"] != "Ghee" {
		t.Fatalf("sort order wrong: %v, %v", page.Data[0]["title"], page.Data[1]["title"])
	}
}

func TestFindOne_NotFound(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	_, err := a.FindOne(ctx, "products", "does-not-exist")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("FindOne missing: got %v, want ErrNotFound", err)
	}
}

func TestCreate_UniqueViolation_MapsToErrConflict(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	in := store.WriteInput{Collection: "products", Data: store.Record{"title": "Honey", "slug": "honey"}}
	if _, err := a.Create(ctx, in); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := a.Create(ctx, store.WriteInput{Collection: "products", Data: store.Record{"title": "Honey 2", "slug": "honey"}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate slug: got %v, want ErrConflict", err)
	}
}

func TestCreate_UnknownCollection_Errors(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)

	_, err := a.Create(ctx, store.WriteInput{Collection: "ghosts", Data: store.Record{"x": 1}})
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("unknown collection: got %v, want ErrInvalidInput", err)
	}
}
