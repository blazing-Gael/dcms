package gateway_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// mediaAuthServer mounts an auth-enabled gateway with a temp-dir blob store and the
// given extra options merged in (Blob + Authenticator are always set).
func mediaAuthServer(t *testing.T, src string, opts gateway.Options) (*httptest.Server, store.Adapter) {
	t.Helper()
	def, db := newDB(t, src)
	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	opts.Blob = bs
	opts.Authenticator = gateway.NewSessionAuthenticator(db)
	srv := mount(t, def, db, opts)
	return srv, db
}

// uploadFileAs POSTs a multipart file with a bearer token.
func uploadFileAs(t *testing.T, url, token, filename, contentType string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
		"Content-Type":        {contentType},
	}
	pw, _ := mw.CreatePart(h)
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
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const inheritMediaSchema = `
version: "1"
auth:
  roles:
    editor: { label: Editor }
  session:
    ttl: 1h
collections:
  _media:
    access:
      read: inherit
  articles:
    fields:
      title: { type: string, required: true }
      cover: { type: file }
    access:
      read: public
      create: [editor]
  backups:
    fields:
      blob: { type: file }
    access:
      read: owner
      create: authenticated
`

func TestMediaInherit_ReferencedRecordDecidesAccess(t *testing.T) {
	srv, db := mediaAuthServer(t, inheritMediaSchema, gateway.Options{})
	seedUser(t, db, "ed@x.com", "pw-editor-123", "editor")
	seedUser(t, db, "alice@x.com", "pw-alice-123")
	seedUser(t, db, "bob@x.com", "pw-bob-12345")
	ed := login(t, srv.URL, "ed@x.com", "pw-editor-123")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-123")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-12345")
	media := srv.URL + "/__media"
	api := srv.URL + "/api/v1"
	data := []byte("some-bytes")

	// The editor uploads an editorial image and puts it on a PUBLIC article.
	_, ib := uploadFileAs(t, media, ed, "cover.png", "image/png", data)
	imgID := dataObj(t, ib)["id"].(string)
	if st, _ := doAs(t, http.MethodPost, api+"/articles", ed, `{"title":"A","cover":"`+imgID+`"}`); st != http.StatusCreated {
		t.Fatalf("create article: %d", st)
	}

	// Alice uploads a private backup and points an owner-only `backups` row at it.
	_, bb := uploadFileAs(t, media, alice, "backup.bin", "application/octet-stream", data)
	bakID := dataObj(t, bb)["id"].(string)
	if st, _ := doAs(t, http.MethodPost, api+"/backups", alice, `{"blob":"`+bakID+`"}`); st != http.StatusCreated {
		t.Fatalf("create backup: %d", st)
	}

	// The editorial image is readable by anyone — it's on a public article.
	if st, _ := do(t, http.MethodGet, media+"/"+imgID, ""); st != http.StatusOK {
		t.Fatalf("anon read of publicly-referenced image: got %d, want 200", st)
	}
	// The backup is NOT readable by another user or anon — its only reference is an
	// owner-scoped row they can't read. This is the leak #30 closes.
	if st, _ := doAs(t, http.MethodGet, media+"/"+bakID, bob, ""); st != http.StatusNotFound {
		t.Fatalf("bob read of alice's backup: got %d, want 404", st)
	}
	if st, _ := do(t, http.MethodGet, media+"/"+bakID, ""); st != http.StatusNotFound {
		t.Fatalf("anon read of a backup: got %d, want 404", st)
	}
	// Alice (the uploader and the owner of the referencing row) can read it.
	if st, _ := doAs(t, http.MethodGet, media+"/"+bakID, alice, ""); st != http.StatusOK {
		t.Fatalf("alice read of her own backup: got %d, want 200", st)
	}
	// The /raw byte path follows the same rule.
	if st, _ := doAs(t, http.MethodGet, media+"/"+bakID+"/raw", bob, ""); st != http.StatusNotFound {
		t.Fatalf("bob /raw of alice's backup: got %d, want 404", st)
	}

	// An unreferenced upload is visible only to its uploader.
	_, ub := uploadFileAs(t, media, ed, "orphan.png", "image/png", data)
	orphanID := dataObj(t, ub)["id"].(string)
	if st, _ := doAs(t, http.MethodGet, media+"/"+orphanID, ed, ""); st != http.StatusOK {
		t.Fatalf("uploader read of own unreferenced media: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, media+"/"+orphanID, bob, ""); st != http.StatusNotFound {
		t.Fatalf("other user read of unreferenced media: got %d, want 404", st)
	}
}

func TestMediaInherit_OnlyValidOnMediaRead(t *testing.T) {
	// `inherit` anywhere but the _media read rule is a schema error.
	for _, src := range []string{
		"version: \"1\"\ncollections:\n  posts:\n    fields:\n      t: { type: string }\n    access:\n      read: inherit\n",
		"version: \"1\"\ncollections:\n  _media:\n    access:\n      create: inherit\n",
	} {
		if _, err := schema.Parse([]byte(src)); err == nil {
			t.Fatalf("expected an inherit-placement error for:\n%s", src)
		}
	}
}
