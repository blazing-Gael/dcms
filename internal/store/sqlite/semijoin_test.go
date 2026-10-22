package sqlite

import (
	"context"
	"testing"

	"github.com/blazing-Gael/dcms/internal/store"
)

// Conformance suite for the ADR-0036 store primitives: the Subquery semi-join value
// and the And/Or boolean filter nodes. Every adapter (SQLite now; Postgres/Couchbase
// later) must pass these — the behaviours here are the wire contract the authz layer
// relies on.

// seedSemiJoin creates two authors and three posts, returning the author ids. It
// reuses the relations schema (users, posts, tags, posts_tags).
func seedSemiJoin(t *testing.T, a *Adapter) (alice, bob string) {
	t.Helper()
	ctx := context.Background()
	migrateRelations(t, a)
	mk := func(coll string, data store.Record) string {
		rec, err := a.Create(ctx, store.WriteInput{Collection: coll, Data: data})
		if err != nil {
			t.Fatalf("create %s: %v", coll, err)
		}
		return rec["id"].(string)
	}
	alice = mk("users", store.Record{"name": "alice"})
	bob = mk("users", store.Record{"name": "bob"})
	mk("posts", store.Record{"title": "a1", "author": alice})
	mk("posts", store.Record{"title": "a2", "author": alice})
	mk("posts", store.Record{"title": "b1", "author": bob})
	return alice, bob
}

func titlesOf(page store.Page) map[string]bool {
	out := map[string]bool{}
	for _, r := range page.Data {
		if t, ok := r["title"].(string); ok {
			out[t] = true
		}
	}
	return out
}

// A Subquery as an In value compiles to a semi-join: posts whose author is in the set
// of user ids named "alice".
func TestSubquery_In_SemiJoin(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	seedSemiJoin(t, a)

	page, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters: []store.Filter{{
			Field: "author", Operator: store.In, Value: store.Subquery{
				Collection: "users",
				Select:     "id",
				Filters:    []store.Filter{{Field: "name", Operator: store.Eq, Value: "alice"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := titlesOf(page)
	if len(got) != 2 || !got["a1"] || !got["a2"] {
		t.Fatalf("want alice's posts {a1,a2}, got %v", got)
	}
}

// NotIn with a Subquery is the complement.
func TestSubquery_NotIn_SemiJoin(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	seedSemiJoin(t, a)

	page, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters: []store.Filter{{
			Field: "author", Operator: store.NotIn, Value: store.Subquery{
				Collection: "users",
				Select:     "id",
				Filters:    []store.Filter{{Field: "name", Operator: store.Eq, Value: "alice"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := titlesOf(page)
	if len(got) != 1 || !got["b1"] {
		t.Fatalf("want non-alice posts {b1}, got %v", got)
	}
}

// An Or node combines two leaves: title = a1 OR author in (bob). Proves grouping is
// parenthesized (not flattened into the surrounding AND).
func TestOr_Node_CombinesBranches(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	_, bob := seedSemiJoin(t, a)

	page, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters: []store.Filter{{
			Operator: store.Or, Value: []store.Filter{
				{Field: "title", Operator: store.Eq, Value: "a1"},
				{Field: "author", Operator: store.Eq, Value: bob},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := titlesOf(page)
	if len(got) != 2 || !got["a1"] || !got["b1"] {
		t.Fatalf("want {a1,b1}, got %v", got)
	}
}

// The OR must bind tighter than the surrounding top-level AND: (status filter) AND
// (title=a1 OR author=bob) must not leak a2. Here we AND a title!=a2 with the OR to
// show the group is isolated by parentheses.
func TestOr_Node_ParenthesizedUnderTopLevelAnd(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	alice, bob := seedSemiJoin(t, a)

	page, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters: []store.Filter{
			{Field: "author", Operator: store.Ne, Value: alice}, // excludes a1,a2
			{Operator: store.Or, Value: []store.Filter{
				{Field: "title", Operator: store.Eq, Value: "a1"}, // excluded by the AND above
				{Field: "author", Operator: store.Eq, Value: bob},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := titlesOf(page)
	if len(got) != 1 || !got["b1"] {
		t.Fatalf("want {b1} (a1 excluded by the AND, proving the OR is grouped), got %v", got)
	}
}

// An Or containing a Subquery branch — the authz "owner OR member" shape in miniature:
// title = a1 OR author IN (users named bob).
func TestOr_Node_WithSubqueryBranch(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	seedSemiJoin(t, a)

	page, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters: []store.Filter{{
			Operator: store.Or, Value: []store.Filter{
				{Field: "title", Operator: store.Eq, Value: "a1"},
				{Field: "author", Operator: store.In, Value: store.Subquery{
					Collection: "users", Select: "id",
					Filters: []store.Filter{{Field: "name", Operator: store.Eq, Value: "bob"}},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := titlesOf(page)
	if len(got) != 2 || !got["a1"] || !got["b1"] {
		t.Fatalf("want {a1,b1}, got %v", got)
	}
}

// A nested And inside an Or: (title=a1) OR (author=alice AND title=a2).
func TestAnd_Nested_InsideOr(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	alice, _ := seedSemiJoin(t, a)

	page, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters: []store.Filter{{
			Operator: store.Or, Value: []store.Filter{
				{Field: "title", Operator: store.Eq, Value: "a1"},
				{Operator: store.And, Value: []store.Filter{
					{Field: "author", Operator: store.Eq, Value: alice},
					{Field: "title", Operator: store.Eq, Value: "a2"},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	got := titlesOf(page)
	if len(got) != 2 || !got["a1"] || !got["a2"] {
		t.Fatalf("want {a1,a2}, got %v", got)
	}
}

// An empty And/Or group is caller error (ErrInvalidInput), not a silent all/none.
func TestBooleanGroup_Empty_IsError(t *testing.T) {
	ctx := context.Background()
	a := newTestAdapter(t)
	seedSemiJoin(t, a)

	_, err := a.Find(ctx, store.Query{
		Collection: "posts",
		Filters:    []store.Filter{{Operator: store.Or, Value: []store.Filter{}}},
	})
	if err == nil {
		t.Fatal("want error for an empty OR group, got nil")
	}
}
