package gateway_test

import (
	"net/http"
	"testing"
)

// riSchema exercises both relation kinds: a required belongs-to (posts.author →
// users) and a many-to-many (posts.tags → tags).
const riSchema = `
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  tags:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
      tags:   { type: relation, target: tags, many: true }
`

// errFields pulls the field-level messages out of an error envelope.
func errFields(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error envelope, got %#v", body)
	}
	f, _ := e["fields"].(map[string]any)
	return f
}

// A belongs-to pointing at a non-existent record is rejected by the validation
// layer with a field-named 422 — never a raw DB error (ADR-0010).
func TestRefCheck_DanglingBelongsToIs422(t *testing.T) {
	def, db := newDB(t, riSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	st, body := do(t, http.MethodPost, base+"/posts", `{"title":"x","author":"nope"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("dangling author: got %d, want 422 (%v)", st, body)
	}
	if _, ok := errFields(t, body)["author"]; !ok {
		t.Fatalf("422 should name the offending field 'author', got %#v", body)
	}
}

// A many-to-many link id that doesn't resolve is likewise a field-named 422, and
// the write must not have happened.
func TestRefCheck_DanglingM2MIs422(t *testing.T) {
	def, db := newDB(t, riSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)

	st, body := do(t, http.MethodPost, base+"/posts",
		`{"title":"x","author":"`+author+`","tags":["ghost"]}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("dangling tag: got %d, want 422 (%v)", st, body)
	}
	if _, ok := errFields(t, body)["tags"]; !ok {
		t.Fatalf("422 should name the offending field 'tags', got %#v", body)
	}

	// The rejected create must not have persisted anything.
	_, list := do(t, http.MethodGet, base+"/posts", "")
	if got := list["data"].([]any); len(got) != 0 {
		t.Fatalf("rejected create leaked a row: %#v", got)
	}
}

// The happy path: real author + real tags → 201, and the links are queryable.
func TestRefCheck_ValidReferencesSucceed(t *testing.T) {
	def, db := newDB(t, riSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/tags", `{"name":"go"}`)
	tag := dataObj(t, b)["id"].(string)

	st, body := do(t, http.MethodPost, base+"/posts",
		`{"title":"x","author":"`+author+`","tags":["`+tag+`"]}`)
	if st != http.StatusCreated {
		t.Fatalf("valid references: got %d, want 201 (%v)", st, body)
	}
}

// Deleting a record that is still referenced by a belongs-to FK is the RESTRICT
// default: a 409, enforced by the database foreign key (ADR-0010).
func TestRefCheck_DeleteReferencedParentIs409(t *testing.T) {
	def, db := newDB(t, riSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	if st, body := do(t, http.MethodPost, base+"/posts",
		`{"title":"x","author":"`+author+`"}`); st != http.StatusCreated {
		t.Fatalf("seed post: %d %v", st, body)
	}

	st, body := do(t, http.MethodDelete, base+"/users/"+author, "")
	if st != http.StatusConflict {
		t.Fatalf("delete referenced user: got %d, want 409 (%v)", st, body)
	}
}

// on_delete: cascade — deleting a parent removes the children that reference it.
func TestRefCheck_OnDeleteCascade(t *testing.T) {
	def, db := newDB(t, `
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true, on_delete: cascade }
`)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/posts", `{"title":"x","author":"`+author+`"}`)
	postID := dataObj(t, b)["id"].(string)

	// Deleting the referenced user cascades to the post.
	if st, body := do(t, http.MethodDelete, base+"/users/"+author, ""); st != http.StatusNoContent {
		t.Fatalf("cascade delete user: got %d, want 204 (%v)", st, body)
	}
	if st, _ := do(t, http.MethodGet, base+"/posts/"+postID, ""); st != http.StatusNotFound {
		t.Fatalf("post should have been cascade-deleted, got %d", st)
	}
}

// on_delete: set null — deleting a parent nulls the child's reference instead of
// blocking or cascading.
func TestRefCheck_OnDeleteSetNull(t *testing.T) {
	def, db := newDB(t, `
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, on_delete: set null }
`)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/posts", `{"title":"x","author":"`+author+`"}`)
	postID := dataObj(t, b)["id"].(string)

	if st, body := do(t, http.MethodDelete, base+"/users/"+author, ""); st != http.StatusNoContent {
		t.Fatalf("set-null delete user: got %d, want 204 (%v)", st, body)
	}
	_, b = do(t, http.MethodGet, base+"/posts/"+postID, "")
	post := dataObj(t, b)
	if _, present := post["author"]; present {
		t.Fatalf("author should be nulled (omitted) after parent delete, got %#v", post["author"])
	}
}

// Deleting an endpoint of a many-to-many relation cascades the join rows away
// (ON DELETE CASCADE on the join table) — no orphaned links, no 409.
func TestRefCheck_DeleteM2MEndpointCascadesLinks(t *testing.T) {
	def, db := newDB(t, riSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/users", `{"name":"ann"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/tags", `{"name":"go"}`)
	tag := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/posts",
		`{"title":"x","author":"`+author+`","tags":["`+tag+`"]}`)
	postID := dataObj(t, b)["id"].(string)

	// Deleting the tag succeeds (cascade removes the link row).
	if st, body := do(t, http.MethodDelete, base+"/tags/"+tag, ""); st != http.StatusNoContent {
		t.Fatalf("delete linked tag: got %d, want 204 (%v)", st, body)
	}

	// The post's tag set is now empty — the link was cleaned, not orphaned.
	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=tags", "")
	if tags := dataObj(t, b)["tags"].([]any); len(tags) != 0 {
		t.Fatalf("link rows should be cascade-deleted, got %#v", tags)
	}
}
