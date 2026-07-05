package gateway_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

const previewTok = "secret-preview-token"

const lifecycleSchema = `
version: "1"
collections:
  articles:
    publishing: true
    soft_delete: true
    fields:
      title: { type: string, required: true }
`

// getJSON does a GET, optionally with the preview token, returning status + body.
func getJSON(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	rq, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		rq.Header.Set("X-DCMS-Preview", token)
	}
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func listLen(t *testing.T, body map[string]any) int {
	t.Helper()
	d, _ := body["data"].([]any)
	return len(d)
}

func TestLifecycle_PublishingAndSoftDelete(t *testing.T) {
	def, db := newDB(t, lifecycleSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok, ValidateResponses: true})
	api := srv.URL + "/api/v1/articles"

	// Create → defaults to draft: invisible to the public, visible with preview.
	_, b := do(t, http.MethodPost, api, `{"title":"Hello"}`)
	id := dataObj(t, b)["id"].(string)

	if _, lb := getJSON(t, api, ""); listLen(t, lb) != 0 {
		t.Fatal("draft should not appear in the public list")
	}
	if st, _ := getJSON(t, api+"/"+id, ""); st != http.StatusNotFound {
		t.Fatalf("public get of a draft should be 404, got %d", st)
	}
	if _, lb := getJSON(t, api+"?status=draft", previewTok); listLen(t, lb) != 1 {
		t.Fatal("preview status=draft should list the draft")
	}
	if st, _ := getJSON(t, api+"/"+id, previewTok); st != http.StatusOK {
		t.Fatalf("preview get of a draft should be 200, got %d", st)
	}

	// A non-preview request cannot widen its view, even asking for status=any.
	if _, lb := getJSON(t, api+"?status=any", ""); listLen(t, lb) != 0 {
		t.Fatal("status=any without a token must be ignored")
	}

	// Publish → live.
	if st, _ := do(t, http.MethodPost, api+"/"+id+"/publish", ""); st != http.StatusOK {
		t.Fatalf("publish: %d", st)
	}
	if _, lb := getJSON(t, api, ""); listLen(t, lb) != 1 {
		t.Fatal("published article should appear publicly")
	}
	if st, _ := getJSON(t, api+"/"+id, ""); st != http.StatusOK {
		t.Fatalf("public get of a published article should be 200, got %d", st)
	}

	// Schedule for the future → published but not yet visible.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if st, _ := do(t, http.MethodPost, api+"/"+id+"/publish", `{"at":"`+future+`"}`); st != http.StatusOK {
		t.Fatalf("schedule: %d", st)
	}
	if st, _ := getJSON(t, api+"/"+id, ""); st != http.StatusNotFound {
		t.Fatalf("scheduled (future) article should be hidden publicly, got %d", st)
	}
	if _, lb := getJSON(t, api+"?status=scheduled", previewTok); listLen(t, lb) != 1 {
		t.Fatal("preview status=scheduled should list the scheduled article")
	}

	// Archive → hidden from public and from the drafts view; shows under archived.
	if st, _ := do(t, http.MethodPost, api+"/"+id+"/archive", ""); st != http.StatusOK {
		t.Fatalf("archive: %d", st)
	}
	if st, _ := getJSON(t, api+"/"+id, ""); st != http.StatusNotFound {
		t.Fatalf("archived article should be hidden publicly, got %d", st)
	}
	if _, lb := getJSON(t, api+"?status=draft", previewTok); listLen(t, lb) != 0 {
		t.Fatal("archived article should not show under status=draft")
	}
	if _, lb := getJSON(t, api+"?status=archived", previewTok); listLen(t, lb) != 1 {
		t.Fatal("archived article should show under status=archived")
	}

	// Re-publish, then soft-delete → trashed (hidden), restorable.
	do(t, http.MethodPost, api+"/"+id+"/publish", "")
	if st, _ := do(t, http.MethodDelete, api+"/"+id, ""); st != http.StatusNoContent {
		t.Fatalf("soft delete: %d", st)
	}
	if _, lb := getJSON(t, api, ""); listLen(t, lb) != 0 {
		t.Fatal("soft-deleted article should be hidden publicly")
	}
	if _, lb := getJSON(t, api+"?include_deleted=only", previewTok); listLen(t, lb) != 1 {
		t.Fatal("include_deleted=only should list the trashed article")
	}
	if st, _ := do(t, http.MethodPost, api+"/"+id+"/restore", ""); st != http.StatusOK {
		t.Fatalf("restore: %d", st)
	}
	if _, lb := getJSON(t, api, ""); listLen(t, lb) != 1 {
		t.Fatal("restored article should be public again")
	}

	// Purge → gone for good, even with the preview token.
	if st, _ := do(t, http.MethodDelete, api+"/"+id+"?purge=true", ""); st != http.StatusNoContent {
		t.Fatalf("purge: %d", st)
	}
	if st, _ := getJSON(t, api+"/"+id, previewTok); st != http.StatusNotFound {
		t.Fatalf("purged article should be 404 even with preview, got %d", st)
	}
}

// A client must not be able to set managed lifecycle columns through a normal
// write; they are stripped, so the record stays a draft and hidden.
func TestLifecycle_ClientCannotSetManagedColumns(t *testing.T) {
	def, db := newDB(t, lifecycleSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok})
	api := srv.URL + "/api/v1/articles"

	_, b := do(t, http.MethodPost, api, `{"title":"Sneaky","_status":"published","_published_at":"2000-01-01T00:00:00Z"}`)
	id := dataObj(t, b)["id"].(string)

	if st, _ := getJSON(t, api+"/"+id, ""); st != http.StatusNotFound {
		t.Fatalf("a client-forged _status must be ignored (stays draft/hidden), got %d", st)
	}
}

const lifecycleExpandSchema = `
version: "1"
collections:
  authors:
    publishing: true
    fields:
      name: { type: string, required: true }
  posts:
    publishing: true
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: authors }
`

func TestLifecycle_ExpansionRespectsVisibility(t *testing.T) {
	def, db := newDB(t, lifecycleExpandSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok, ValidateResponses: true})
	authors := srv.URL + "/api/v1/authors"
	posts := srv.URL + "/api/v1/posts"

	// A draft author, and a post that references it and is itself published.
	_, b := do(t, http.MethodPost, authors, `{"name":"Ada"}`)
	authorID := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, posts, `{"title":"P","author":"`+authorID+`"}`)
	postID := dataObj(t, b)["id"].(string)
	do(t, http.MethodPost, posts+"/"+postID+"/publish", "")

	// belongs-to: the draft author is hidden, so expand leaves the id, not an object.
	_, gb := getJSON(t, posts+"/"+postID+"?expand=author", "")
	if _, isObj := dataObj(t, gb)["author"].(map[string]any); isObj {
		t.Fatal("a hidden (draft) belongs-to target must not be inlined")
	}
	if s, _ := dataObj(t, gb)["author"].(string); s != authorID {
		t.Fatalf("hidden target should stay as its id, got %#v", dataObj(t, gb)["author"])
	}

	// Publish the author → now it inlines.
	do(t, http.MethodPost, authors+"/"+authorID+"/publish", "")
	_, gb = getJSON(t, posts+"/"+postID+"?expand=author", "")
	if obj, ok := dataObj(t, gb)["author"].(map[string]any); !ok || obj["id"] != authorID {
		t.Fatalf("a published belongs-to target should inline, got %#v", dataObj(t, gb)["author"])
	}

	// has-many: author?expand=posts shows only published posts.
	_, b = do(t, http.MethodPost, posts, `{"title":"Draft P2","author":"`+authorID+`"}`)
	_ = b
	_, gb = getJSON(t, authors+"/"+authorID+"?expand=posts", "")
	pl, _ := dataObj(t, gb)["posts"].([]any)
	if len(pl) != 1 {
		t.Fatalf("expand=posts should hide the draft post, got %d posts", len(pl))
	}
}
