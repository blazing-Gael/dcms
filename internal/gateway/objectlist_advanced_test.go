package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// pages own a fixed list of cards; each card may point at an author (a belongs-to
// relation to a real collection) and an image (a _media file). This exercises the
// inner-reference path with a non-media target, alongside media.
const objectListRelSchema = `
version: "1"
collections:
  authors:
    fields:
      name: { type: string, required: true }
  pages:
    fields:
      key: { type: string, required: true, unique: true }
      cards:
        type: object_list
        of:
          title:  { type: string, required: true }
          author: { type: relation, target: authors }
          image:  { type: file }
`

// seedAuthor creates an author and returns its id.
func seedAuthor(t *testing.T, base, name string) string {
	t.Helper()
	st, body := do(t, http.MethodPost, base+"/authors", `{"name":"`+name+`"}`)
	if st != http.StatusCreated {
		t.Fatalf("seed author: %d %v", st, body)
	}
	return dataObj(t, body)["id"].(string)
}

func TestObjectList_InnerBelongsToRelation(t *testing.T) {
	def, db := newDB(t, objectListRelSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	author := seedAuthor(t, base, "Ada")

	// A valid inner relation id is accepted and round-trips.
	st, body := do(t, http.MethodPost, base+"/pages",
		`{"key":"home","cards":[{"title":"Card","author":"`+author+`"}]}`)
	if st != http.StatusCreated {
		t.Fatalf("valid inner relation: %d %v", st, body)
	}
	cards := dataObj(t, body)["cards"].([]any)
	if got := cards[0].(map[string]any)["author"]; got != author {
		t.Fatalf("inner author id = %v, want %s", got, author)
	}

	// A dangling inner relation id is a 422, exactly like a top-level one.
	st, _ = do(t, http.MethodPost, base+"/pages",
		`{"key":"other","cards":[{"title":"Card","author":"00000000-0000-7000-8000-000000000000"}]}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("dangling inner relation should be 422, got %d", st)
	}
}

func TestObjectList_MixedReferencesBatched(t *testing.T) {
	def, db := newDB(t, objectListRelSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	a1 := seedAuthor(t, base, "One")
	a2 := seedAuthor(t, base, "Two")

	// Several elements, each referencing a different author — all resolve on the
	// single batched pass (no per-element query), so the create succeeds.
	body := `{"key":"home","cards":[
		{"title":"A","author":"` + a1 + `"},
		{"title":"B","author":"` + a2 + `"},
		{"title":"C"}
	]}`
	if st, resp := do(t, http.MethodPost, base+"/pages", body); st != http.StatusCreated {
		t.Fatalf("mixed valid references: %d %v", st, resp)
	}

	// One dangling id among several valid ones still fails the whole write.
	bad := `{"key":"x","cards":[
		{"title":"A","author":"` + a1 + `"},
		{"title":"B","author":"00000000-0000-7000-8000-000000000000"}
	]}`
	if st, _ := do(t, http.MethodPost, base+"/pages", bad); st != http.StatusUnprocessableEntity {
		t.Fatalf("one dangling among valid should be 422, got %d", st)
	}
}

func TestObjectList_PatchReplacesWholeList(t *testing.T) {
	def, db := newDB(t, objectListRelSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, body := do(t, http.MethodPost, base+"/pages",
		`{"key":"home","cards":[{"_key":"a","title":"First"},{"_key":"b","title":"Second"}]}`)
	id := dataObj(t, body)["id"].(string)

	// PATCH the list to a single element — it fully replaces, not merges.
	st, patched := do(t, http.MethodPatch, base+"/pages/"+id, `{"cards":[{"_key":"c","title":"Only"}]}`)
	if st != http.StatusOK {
		t.Fatalf("patch list: %d %v", st, patched)
	}
	cards := dataObj(t, patched)["cards"].([]any)
	if len(cards) != 1 || cards[0].(map[string]any)["_key"] != "c" {
		t.Fatalf("patch should replace the whole list, got %#v", cards)
	}

	// A PATCH that omits cards leaves the (replaced) list untouched.
	st, other := do(t, http.MethodPatch, base+"/pages/"+id, `{"key":"home2"}`)
	if st != http.StatusOK {
		t.Fatalf("patch other field: %d %v", st, other)
	}
	if cards := dataObj(t, other)["cards"].([]any); len(cards) != 1 {
		t.Fatalf("omitting cards on patch should leave them untouched, got %#v", cards)
	}

	// PATCH to an empty list clears it.
	st, cleared := do(t, http.MethodPatch, base+"/pages/"+id, `{"cards":[]}`)
	if st != http.StatusOK {
		t.Fatalf("patch to empty: %d %v", st, cleared)
	}
	if cards := dataObj(t, cleared)["cards"].([]any); len(cards) != 0 {
		t.Fatalf("patch to [] should clear, got %#v", cards)
	}
}

func TestObjectList_KeyPreservedAndOrdered(t *testing.T) {
	def, db := newDB(t, objectListRelSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, body := do(t, http.MethodPost, base+"/pages",
		`{"key":"home","cards":[{"_key":"z","title":"Zed"},{"_key":"a","title":"Ay"}]}`)
	id := dataObj(t, body)["id"].(string)

	// Order and per-element _key survive the JSON round-trip (via a fresh GET).
	_, got := do(t, http.MethodGet, base+"/pages/"+id, "")
	cards := dataObj(t, got)["cards"].([]any)
	c0, c1 := cards[0].(map[string]any), cards[1].(map[string]any)
	if c0["_key"] != "z" || c0["title"] != "Zed" || c1["_key"] != "a" || c1["title"] != "Ay" {
		t.Fatalf("order/_key not preserved: %#v", cards)
	}
}

func TestObjectList_DeepTypeMismatchRejected(t *testing.T) {
	def, db := newDB(t, objectListRelSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	// A non-string where an inner string is declared → 422 (the element is validated
	// through the same type checks as a top-level record).
	st, _ := do(t, http.MethodPost, base+"/pages", `{"key":"home","cards":[{"title":123}]}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("inner type mismatch should be 422, got %d", st)
	}
}

func TestObjectList_OmittedAndEmptyAtCreate(t *testing.T) {
	def, db := newDB(t, objectListRelSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	// The list is optional: a create omitting it succeeds.
	if st, body := do(t, http.MethodPost, base+"/pages", `{"key":"home"}`); st != http.StatusCreated {
		t.Fatalf("omitting an optional object_list should be allowed, got %d %v", st, body)
	}
	// An explicit empty list is accepted and round-trips as [].
	st, body := do(t, http.MethodPost, base+"/pages", `{"key":"other","cards":[]}`)
	if st != http.StatusCreated {
		t.Fatalf("empty list at create: %d %v", st, body)
	}
	if cards, ok := dataObj(t, body)["cards"].([]any); !ok || len(cards) != 0 {
		t.Fatalf("empty list should round-trip as [], got %#v", dataObj(t, body)["cards"])
	}
}
