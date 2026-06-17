package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/blazing-Gael/dcms/core/store"
)

// pageAll walks every page via NextCursor and returns the ids in visit order.
func pageAll(t *testing.T, a *Adapter, q store.Query) []string {
	t.Helper()
	var ids []string
	seen := map[string]bool{}
	for {
		page, err := a.Find(context.Background(), q)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		for _, r := range page.Data {
			id := r["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate id across pages: %s", id)
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if page.NextCursor == "" {
			return ids
		}
		q.Cursor = page.NextCursor
	}
}

func TestPagination_DefaultSort_WalksAllRowsOnce(t *testing.T) {
	a := newTestAdapter(t)
	migrateProducts(t, a)

	const n = 25
	for i := range n {
		seedProducts(t, a, store.Record{"title": fmt.Sprintf("p%02d", i), "price": float64(i)})
	}

	ids := pageAll(t, a, store.Query{Collection: "products", Limit: 10})
	if len(ids) != n {
		t.Fatalf("walked %d rows, want %d", len(ids), n)
	}
}

func TestPagination_SortedByField_StableOrderAcrossPages(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)

	// Many rows share the same price, forcing the id tiebreaker to do real work.
	for i := range 12 {
		seedProducts(t, a, store.Record{"title": fmt.Sprintf("p%02d", i), "price": float64(i % 3)})
	}

	// Page through price-ascending in small pages and confirm prices are non-decreasing.
	q := store.Query{Collection: "products", Sort: "price", Limit: 5}
	var prices []float64
	for {
		page, err := a.Find(ctx, q)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		for _, r := range page.Data {
			prices = append(prices, r["price"].(float64))
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(prices) != 12 {
		t.Fatalf("got %d rows, want 12", len(prices))
	}
	for i := 1; i < len(prices); i++ {
		if prices[i] < prices[i-1] {
			t.Fatalf("prices not non-decreasing at %d: %v", i, prices)
		}
	}
}

func TestPagination_LastPageHasNoCursor(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "A"},
		store.Record{"title": "B"},
	)

	// Exactly 2 rows, limit 2 → one full page, but no more rows after it.
	page, err := a.Find(ctx, store.Query{Collection: "products", Limit: 2})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("len: got %d, want 2", len(page.Data))
	}
	if page.NextCursor != "" {
		t.Fatalf("expected no next cursor on exhausted result, got %q", page.NextCursor)
	}
}

func TestPagination_CursorSortMismatch_Errors(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	seedProducts(t, a,
		store.Record{"title": "A", "price": 1.0},
		store.Record{"title": "B", "price": 2.0},
	)

	// Get a cursor built for sort "price".
	page, err := a.Find(ctx, store.Query{Collection: "products", Sort: "price", Limit: 1})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}

	// Reuse it under a different sort → must be rejected.
	_, err = a.Find(ctx, store.Query{Collection: "products", Sort: "-price", Limit: 1, Cursor: page.NextCursor})
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("cursor/sort mismatch: got %v, want ErrInvalidInput", err)
	}
}

func TestPagination_SparseFieldsStillPaginates(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	migrateProducts(t, a)
	for i := range 6 {
		seedProducts(t, a, store.Record{"title": fmt.Sprintf("p%d", i), "price": float64(i)})
	}

	// Request only "title" — id and the sort field must be added internally for
	// the cursor, then stripped from the returned records.
	page, err := a.Find(ctx, store.Query{Collection: "products", Sort: "price", Fields: []string{"title"}, Limit: 3})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor with more rows remaining")
	}
	for _, r := range page.Data {
		if _, ok := r["id"]; ok {
			t.Fatalf("id should have been stripped from sparse fieldset: %#v", r)
		}
		if _, ok := r["price"]; ok {
			t.Fatalf("price should have been stripped from sparse fieldset: %#v", r)
		}
		if _, ok := r["title"]; !ok {
			t.Fatalf("title should be present: %#v", r)
		}
	}
}
