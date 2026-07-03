package gateway_test

import (
	"net/http"
	"testing"
)

const nestedSchema = `
version: "1"
collections:
  companies:
    fields:
      name: { type: string, required: true }
  users:
    fields:
      name:    { type: string, required: true }
      company: { type: relation, target: companies }
  tags:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
      tags:   { type: relation, target: tags, many: true }
`

// A belongs-to given as an object is created inline and linked in one call.
func TestNested_BelongsToInlineCreate(t *testing.T) {
	def, db := newDB(t, nestedSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	st, b := do(t, http.MethodPost, base+"/posts", `{"title":"x","author":{"name":"Ann"}}`)
	if st != http.StatusCreated {
		t.Fatalf("inline author create: got %d (%v)", st, b)
	}
	postID := dataObj(t, b)["id"].(string)

	// The author was created and linked; expand resolves to the new user.
	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=author", "")
	author, ok := dataObj(t, b)["author"].(map[string]any)
	if !ok || author["name"] != "Ann" {
		t.Fatalf("expanded author should be the inline-created user, got %#v", dataObj(t, b)["author"])
	}

	// And it's a real, independently-addressable users row.
	_, b = do(t, http.MethodGet, base+"/users", "")
	if len(b["data"].([]any)) != 1 {
		t.Fatalf("inline create should have produced exactly one user, got %#v", b["data"])
	}
}

// A many-to-many list may mix existing ids with inline objects.
func TestNested_M2MMixedIdsAndObjects(t *testing.T) {
	def, db := newDB(t, nestedSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/tags", `{"name":"existing"}`)
	existing := dataObj(t, b)["id"].(string)

	st, b := do(t, http.MethodPost, base+"/posts",
		`{"title":"x","author":"`+author+`","tags":["`+existing+`",{"name":"fresh"}]}`)
	if st != http.StatusCreated {
		t.Fatalf("mixed m2m: got %d (%v)", st, b)
	}
	postID := dataObj(t, b)["id"].(string)

	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=tags", "")
	tags := dataObj(t, b)["tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("post should link 2 tags (one existing, one inline), got %#v", tags)
	}
}

// Inline creates nest recursively: post → author → company, all in one write.
func TestNested_DeepCreate(t *testing.T) {
	def, db := newDB(t, nestedSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	st, b := do(t, http.MethodPost, base+"/posts",
		`{"title":"x","author":{"name":"Ann","company":{"name":"Acme"}}}`)
	if st != http.StatusCreated {
		t.Fatalf("deep nested create: got %d (%v)", st, b)
	}
	postID := dataObj(t, b)["id"].(string)

	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=author.company", "")
	author := dataObj(t, b)["author"].(map[string]any)
	company, ok := author["company"].(map[string]any)
	if !ok || company["name"] != "Acme" {
		t.Fatalf("nested company should be created and linked, got %#v", author["company"])
	}
}

// A nested write is atomic: if any part fails validation, nothing is written.
func TestNested_InvalidChildRollsBackEverything(t *testing.T) {
	def, db := newDB(t, nestedSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	// The inline author is missing its required "name" → the whole tree fails.
	st, _ := do(t, http.MethodPost, base+"/posts", `{"title":"x","author":{}}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("invalid inline child should be 422, got %d", st)
	}

	// Neither the post nor a stray user should have been created.
	_, posts := do(t, http.MethodGet, base+"/posts", "")
	if n := len(posts["data"].([]any)); n != 0 {
		t.Fatalf("no post should exist after rollback, got %d", n)
	}
	_, users := do(t, http.MethodGet, base+"/users", "")
	if n := len(users["data"].([]any)); n != 0 {
		t.Fatalf("no user should exist after rollback, got %d", n)
	}
}

// PATCH may also create an inline related record.
func TestNested_UpdateInlineCreate(t *testing.T) {
	def, db := newDB(t, nestedSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/posts", `{"title":"x","author":"`+author+`"}`)
	postID := dataObj(t, b)["id"].(string)

	// Reassign the author to a brand-new user created inline.
	st, b := do(t, http.MethodPatch, base+"/posts/"+postID, `{"author":{"name":"Bob"}}`)
	if st != http.StatusOK {
		t.Fatalf("inline author on PATCH: got %d (%v)", st, b)
	}
	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=author", "")
	if dataObj(t, b)["author"].(map[string]any)["name"] != "Bob" {
		t.Fatalf("author should be the newly-created Bob, got %#v", dataObj(t, b)["author"])
	}
}
