package gateway_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// A collection's read rule must hold on every path that can return one of its
// records — not just GET /{collection}/{id}. These cover the indirect paths:
// relation expansion (both directions), the reference manifest, revision
// history, and the media byte path. Each one was a way around the rule before.

const expandAuthzSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  secrets:
    fields:
      codename: { type: string, required: true }
    access:
      read:   [admin]
      create: [admin]
  authors:
    fields:
      name: { type: string, required: true }
    access:
      read:   public
      create: [admin]
  posts:
    fields:
      title:  { type: string, required: true }
      secret: { type: relation, target: secrets }
    access:
      read:   public
      create: [admin]
  notes:
    fields:
      body:   { type: string, required: true }
      author: { type: relation, target: authors }
    access:
      read:   owner
      create: authenticated
`

// A belongs-to into a role-gated collection stays a bare id for a caller who may
// not read the target, rather than inlining the record.
func TestExpand_BelongsToRespectsTargetReadRule(t *testing.T) {
	srv, db := newServerWith(t, expandAuthzSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	base := srv.URL + "/api/v1"

	st, body := doAs(t, http.MethodPost, base+"/secrets", boss, `{"codename":"OPERATION-X"}`)
	if st != http.StatusCreated {
		t.Fatalf("create secret: %d %v", st, body)
	}
	secretID := dataObj(t, body)["id"].(string)

	st, body = doAs(t, http.MethodPost, base+"/posts", boss, `{"title":"Hello","secret":"`+secretID+`"}`)
	if st != http.StatusCreated {
		t.Fatalf("create post: %d %v", st, body)
	}
	postID := dataObj(t, body)["id"].(string)

	// Anonymous: the post is public, the secret is not.
	_, body = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=secret", "")
	switch v := dataObj(t, body)["secret"].(type) {
	case string:
		if v != secretID {
			t.Fatalf("expected the bare id, got %q", v)
		}
	default:
		t.Fatalf("anonymous caller got an admin-only record through expand: %#v", v)
	}

	// The same expand in a list is batched through a different code path.
	_, body = do(t, http.MethodGet, base+"/posts?expand=secret", "")
	list, _ := body["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected 1 post, got %d", len(list))
	}
	if _, leaked := list[0].(map[string]any)["secret"].(map[string]any); leaked {
		t.Fatalf("anonymous caller got an admin-only record through a batched list expand")
	}

	// An admin still gets the inlined object.
	_, body = doAs(t, http.MethodGet, base+"/posts/"+postID+"?expand=secret", boss, "")
	obj, ok := dataObj(t, body)["secret"].(map[string]any)
	if !ok || obj["codename"] != "OPERATION-X" {
		t.Fatalf("admin should still get the expanded secret, got %#v", dataObj(t, body)["secret"])
	}
}

// An inverse (has-many) expand from a public parent must not hand out
// owner-scoped children belonging to someone else.
func TestExpand_InverseRespectsOwnerScope(t *testing.T) {
	srv, db := newServerWith(t, expandAuthzSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "mallory@x.com", "pw-mallory-12")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	mallory := login(t, srv.URL, "mallory@x.com", "pw-mallory-12")
	base := srv.URL + "/api/v1"

	st, body := doAs(t, http.MethodPost, base+"/authors", boss, `{"name":"Shared"}`)
	if st != http.StatusCreated {
		t.Fatalf("create author: %d %v", st, body)
	}
	authorID := dataObj(t, body)["id"].(string)

	st, body = doAs(t, http.MethodPost, base+"/notes", alice, `{"body":"ALICE PRIVATE","author":"`+authorID+`"}`)
	if st != http.StatusCreated {
		t.Fatalf("create note: %d %v", st, body)
	}

	for _, tc := range []struct{ name, token string }{
		{"anonymous", ""},
		{"another user", mallory},
	} {
		_, body := doAs(t, http.MethodGet, base+"/authors/"+authorID+"?expand=notes", tc.token, "")
		notes, _ := dataObj(t, body)["notes"].([]any)
		if len(notes) != 0 {
			t.Errorf("%s expanded %d owner-scoped note(s): %#v", tc.name, len(notes), notes)
		}
	}

	// The owner still sees their own note through the same expand.
	_, body = doAs(t, http.MethodGet, base+"/authors/"+authorID+"?expand=notes", alice, "")
	notes, _ := dataObj(t, body)["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("the owner should see their own note, got %#v", notes)
	}
}

const revisionAuthzSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  notes:
    revisions: true
    fields:
      body: { type: string, required: true }
    access:
      read:   owner
      create: authenticated
      update: owner
  profiles:
    revisions: true
    fields:
      name: { type: string, required: true }
      salary:
        type: number
        access:
          read:  [admin]
          write: [admin]
    access:
      read:   public
      create: [admin]
      update: [admin]
`

// History is a read of the record, so an owner rule gates it the same way — and
// a denial is a 404, never a 403.
func TestRevisions_HistoryRespectsOwnerScope(t *testing.T) {
	srv, db := newServerWith(t, revisionAuthzSchema)
	seedUser(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "mallory@x.com", "pw-mallory-12")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	mallory := login(t, srv.URL, "mallory@x.com", "pw-mallory-12")
	base := srv.URL + "/api/v1"

	st, body := doAs(t, http.MethodPost, base+"/notes", alice, `{"body":"ALICE SECRET V1"}`)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, body)
	}
	id := dataObj(t, body)["id"].(string)

	for _, path := range []string{"/notes/" + id + "/revisions", "/notes/" + id + "/revisions/1"} {
		if st, _ := doAs(t, http.MethodGet, base+path, mallory, ""); st != http.StatusNotFound {
			t.Errorf("non-owner GET %s: got %d, want 404", path, st)
		}
	}

	// The owner still has their own history.
	st, body = doAs(t, http.MethodGet, base+"/notes/"+id+"/revisions/1", alice, "")
	if st != http.StatusOK {
		t.Fatalf("owner GET revision: %d %v", st, body)
	}
	snap, _ := dataObj(t, body)["data"].(map[string]any)
	if snap["body"] != "ALICE SECRET V1" {
		t.Fatalf("owner should see their own snapshot, got %#v", snap)
	}
}

// A revision snapshot is a whole record, so it carries the same field mask a
// live read applies.
func TestRevisions_SnapshotAppliesFieldMasking(t *testing.T) {
	srv, db := newServerWith(t, revisionAuthzSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")
	base := srv.URL + "/api/v1"

	st, body := doAs(t, http.MethodPost, base+"/profiles", boss, `{"name":"Sam","salary":250000}`)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, body)
	}
	id := dataObj(t, body)["id"].(string)

	for _, tc := range []struct{ name, token string }{
		{"anonymous", ""},
		{"non-admin", worker},
	} {
		st, body := doAs(t, http.MethodGet, base+"/profiles/"+id+"/revisions/1", tc.token, "")
		if st != http.StatusOK {
			t.Fatalf("%s GET revision: %d %v", tc.name, st, body)
		}
		snap, _ := dataObj(t, body)["data"].(map[string]any)
		if _, leaked := snap["salary"]; leaked {
			t.Errorf("%s read the masked salary through a revision snapshot", tc.name)
		}
		if snap["name"] != "Sam" {
			t.Errorf("%s should still see unmasked fields, got %#v", tc.name, snap)
		}
	}

	// An admin still sees the masked field.
	_, body = doAs(t, http.MethodGet, base+"/profiles/"+id+"/revisions/1", boss, "")
	snap, _ := dataObj(t, body)["data"].(map[string]any)
	if snap["salary"] == nil {
		t.Fatalf("admin should still see salary in the snapshot, got %#v", snap)
	}
}

// authMediaServer builds an auth-enabled server with a local blob store.
func authMediaServer(t *testing.T, src string) (*httptest.Server, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	db, err := sqlite.New(sqlite.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	srv := httptest.NewServer(gateway.New(def, db, nil, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
		Blob:          bs,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, db
}

// uploadAs POSTs a one-file multipart body, optionally bearing a token.
func uploadAs(t *testing.T, url, token, filename string, data []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, err := mw.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
		"Content-Type":        {"text/plain"},
	})
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	pw.Write(data)
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

const mediaAuthzSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  docs:
    fields:
      title: { type: string, required: true }
      attachment: { type: file }
`

// The media library defaults to public read (serving an image needs no
// credential) but authenticated write — anonymous upload, edit and delete are
// refused.
func TestMedia_WritesRequireAuthentication(t *testing.T) {
	srv, db := authMediaServer(t, mediaAuthzSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")

	if st, body := uploadAs(t, srv.URL+"/__media", "", "evil.txt", []byte("payload")); st != http.StatusUnauthorized {
		t.Fatalf("anonymous upload: got %d, want 401 (%s)", st, body)
	}

	st, body := uploadAs(t, srv.URL+"/__media", boss, "ok.txt", []byte("payload"))
	if st != http.StatusCreated {
		t.Fatalf("admin upload: got %d, want 201 (%s)", st, body)
	}
	_, listBody := doAs(t, http.MethodGet, srv.URL+"/__media", boss, "")
	items, _ := listBody["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 media row, got %d", len(items))
	}
	id := items[0].(map[string]any)["id"].(string)

	// Anonymous metadata edit and delete are both refused.
	if st, _ := do(t, http.MethodPatch, srv.URL+"/__media/"+id, `{"alt":"hijacked"}`); st != http.StatusUnauthorized {
		t.Errorf("anonymous patch: got %d, want 401", st)
	}
	if st, _ := do(t, http.MethodDelete, srv.URL+"/__media/"+id, ""); st != http.StatusUnauthorized {
		t.Errorf("anonymous delete: got %d, want 401", st)
	}
	if st, body := uploadAs(t, srv.URL+"/__media/"+id, "", "swap.txt", []byte("swapped")); st != http.StatusUnauthorized {
		t.Errorf("anonymous replace: got %d, want 401 (%s)", st, body)
	}

	// Reads stay open by default, so a public site can serve the bytes.
	if st, _ := do(t, http.MethodGet, srv.URL+"/__media/"+id, ""); st != http.StatusOK {
		t.Errorf("anonymous metadata read: got %d, want 200", st)
	}
	resp, err := http.Get(srv.URL + "/__media/" + id + "/raw")
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous raw read: got %d, want 200", resp.StatusCode)
	}
}

const privateMediaSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  _media:
    access:
      read: [admin]
  docs:
    fields:
      title: { type: string, required: true }
      attachment: { type: file }
`

// Declaring _media with an `access:` block is how an operator makes the media
// library private; the byte path is gated by the same rule as the metadata.
func TestMedia_ReadRuleIsConfigurable(t *testing.T) {
	srv, db := authMediaServer(t, privateMediaSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")

	st, body := uploadAs(t, srv.URL+"/__media", boss, "contract.txt", []byte("CONFIDENTIAL"))
	if st != http.StatusCreated {
		t.Fatalf("admin upload: got %d, want 201 (%s)", st, body)
	}
	_, listBody := doAs(t, http.MethodGet, srv.URL+"/__media", boss, "")
	items, _ := listBody["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("admin should list 1 media row, got %d", len(items))
	}
	id := items[0].(map[string]any)["id"].(string)

	for _, tc := range []struct{ name, token string }{
		{"anonymous", ""},
		{"non-admin", worker},
	} {
		if st, _ := doAs(t, http.MethodGet, srv.URL+"/__media", tc.token, ""); st != http.StatusForbidden && st != http.StatusUnauthorized {
			t.Errorf("%s list: got %d, want 401/403", tc.name, st)
		}
		if st, _ := doAs(t, http.MethodGet, srv.URL+"/__media/"+id, tc.token, ""); st != http.StatusNotFound {
			t.Errorf("%s metadata read: got %d, want 404", tc.name, st)
		}
		if st, _ := doAs(t, http.MethodGet, srv.URL+"/__media/"+id+"/raw", tc.token, ""); st != http.StatusNotFound {
			t.Errorf("%s raw read: got %d, want 404", tc.name, st)
		}
	}

	// The admin still gets the bytes.
	st, body = func() (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/__media/"+id+"/raw", nil)
		req.Header.Set("Authorization", "Bearer "+boss)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("admin raw: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}()
	if st != http.StatusOK || string(body) != "CONFIDENTIAL" {
		t.Fatalf("admin raw read: got %d %q", st, body)
	}
}
