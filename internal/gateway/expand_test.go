package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// seedBlog creates two users and three posts (2 by Sam, 1 by Kai) and returns
// the user ids.
func seedBlog(t *testing.T, base string) (sam, kai string) {
	t.Helper()
	_, b := do(t, http.MethodPost, base+"/users", `{"name":"Sam"}`)
	sam = dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/users", `{"name":"Kai"}`)
	kai = dataObj(t, b)["id"].(string)
	for _, body := range []string{
		`{"title":"A","author":"` + sam + `"}`,
		`{"title":"B","author":"` + sam + `"}`,
		`{"title":"C","author":"` + kai + `"}`,
	} {
		if st, resp := do(t, http.MethodPost, base+"/posts", body); st != http.StatusCreated {
			t.Fatalf("seed post: %d %v", st, resp)
		}
	}
	return sam, kai
}

func TestExpand_BelongsToInlinesObject(t *testing.T) {
	def, db := newDB(t, relSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"
	sam, _ := seedBlog(t, base)

	// Find Sam's first post, then GET it expanded.
	_, list := do(t, http.MethodGet, base+"/posts?filter[author]="+sam+"&limit=1", "")
	postID := list["data"].([]any)[0].(map[string]any)["id"].(string)

	// Without expand: author is the id string.
	_, body := do(t, http.MethodGet, base+"/posts/"+postID, "")
	if _, isStr := dataObj(t, body)["author"].(string); !isStr {
		t.Fatalf("author should be a string id without expand, got %#v", dataObj(t, body)["author"])
	}

	// With expand: author is the inlined user object.
	_, body = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=author", "")
	author, ok := dataObj(t, body)["author"].(map[string]any)
	if !ok {
		t.Fatalf("expanded author should be an object, got %#v", dataObj(t, body)["author"])
	}
	if author["id"] != sam || author["name"] != "Sam" {
		t.Fatalf("expanded author wrong: %#v", author)
	}
}

func TestExpand_BelongsToInListIsBatched(t *testing.T) {
	def, db := newDB(t, relSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"
	seedBlog(t, base)

	_, body := do(t, http.MethodGet, base+"/posts?expand=author&sort=title", "")
	arr := body["data"].([]any)
	if len(arr) != 3 {
		t.Fatalf("want 3 posts, got %d", len(arr))
	}
	for _, item := range arr {
		post := item.(map[string]any)
		author, ok := post["author"].(map[string]any)
		if !ok {
			t.Fatalf("each post's author should be inlined, got %#v", post["author"])
		}
		if author["name"] == nil {
			t.Fatalf("inlined author missing name: %#v", author)
		}
	}
}

func TestExpand_HasManyInverseOnSingleGet(t *testing.T) {
	def, db := newDB(t, relSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"
	sam, _ := seedBlog(t, base)

	// users has no "posts" field, but posts.author points at users → inverse.
	_, body := do(t, http.MethodGet, base+"/users/"+sam+"?expand=posts", "")
	posts, ok := dataObj(t, body)["posts"].([]any)
	if !ok {
		t.Fatalf("expanded posts should be a list, got %#v", dataObj(t, body)["posts"])
	}
	if len(posts) != 2 {
		t.Fatalf("Sam should have 2 posts, got %d", len(posts))
	}
}

func TestExpand_Errors(t *testing.T) {
	def, db := newDB(t, relSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"
	seedBlog(t, base)

	// Grab a real post id.
	_, list := do(t, http.MethodGet, base+"/posts?limit=1", "")
	postID := list["data"].([]any)[0].(map[string]any)["id"].(string)

	// Unknown expand token on a single record → 422.
	if st, _ := do(t, http.MethodGet, base+"/posts/"+postID+"?expand=nope", ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("unknown expand: got %d, want 422", st)
	}
	// Expanding a non-relation scalar field → 422 (not 500).
	if st, _ := do(t, http.MethodGet, base+"/posts/"+postID+"?expand=title", ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("expand scalar field: got %d, want 422", st)
	}
	// has-many on a list is disallowed → 422.
	if st, _ := do(t, http.MethodGet, base+"/users?expand=posts", ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("has-many on list: got %d, want 422", st)
	}
}
