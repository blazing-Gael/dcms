package gateway_test

import (
	"net/http"
	"testing"
)

// Reverse many-to-many: from the target side, expand back to the collections
// that link to it. posts.tags → tags, so tags?expand=posts yields the posts.
func TestExpand_ReverseM2M(t *testing.T) {
	def, db := newDB(t, `
version: "1"
collections:
  tags:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title: { type: string, required: true }
      tags:  { type: relation, target: tags, many: true }
`)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/tags", `{"name":"go"}`)
	tag := dataObj(t, b)["id"].(string)
	do(t, http.MethodPost, base+"/posts", `{"title":"a","tags":["`+tag+`"]}`)
	do(t, http.MethodPost, base+"/posts", `{"title":"b","tags":["`+tag+`"]}`)

	// From the tag, expand the posts that link to it.
	_, b = do(t, http.MethodGet, base+"/tags/"+tag+"?expand=posts", "")
	posts, ok := dataObj(t, b)["posts"].([]any)
	if !ok || len(posts) != 2 {
		t.Fatalf("tags?expand=posts should yield 2 posts, got %#v", dataObj(t, b)["posts"])
	}
}

// Depth>1 expansion on a single record: expand=author.company follows two
// belongs-to hops.
func TestExpand_NestedDepthOnSingleRecord(t *testing.T) {
	def, db := newDB(t, `
version: "1"
collections:
  companies:
    fields:
      name: { type: string, required: true }
  users:
    fields:
      name:    { type: string, required: true }
      company: { type: relation, target: companies }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
`)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/companies", `{"name":"Acme"}`)
	company := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/users", `{"name":"ann","company":"`+company+`"}`)
	author := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/posts", `{"title":"x","author":"`+author+`"}`)
	postID := dataObj(t, b)["id"].(string)

	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=author.company", "")
	post := dataObj(t, b)
	authorObj, ok := post["author"].(map[string]any)
	if !ok {
		t.Fatalf("author should be an expanded object, got %#v", post["author"])
	}
	companyObj, ok := authorObj["company"].(map[string]any)
	if !ok {
		t.Fatalf("author.company should be an expanded object, got %#v", authorObj["company"])
	}
	if companyObj["name"] != "Acme" {
		t.Fatalf("nested company name = %#v, want Acme", companyObj["name"])
	}
}

// Nested expansion is rejected on lists (would risk multi-level N+1).
func TestExpand_NestedRejectedOnList(t *testing.T) {
	def, db := newDB(t, `
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
`)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	if st, body := do(t, http.MethodGet, base+"/posts?expand=author.company", ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("nested expand on a list should be 422, got %d (%v)", st, body)
	}
}
