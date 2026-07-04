package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
)

const mediaProductSchema = `
version: "1"
collections:
  products:
    fields:
      name:  { type: string, required: true }
      image: { type: file }
`

// mediaServer mounts a gateway with a temp-dir local blob store.
func mediaServer(t *testing.T, src string) (base string) {
	t.Helper()
	def, db := newDB(t, src)
	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	srv := mount(t, def, db, gateway.Options{Blob: bs})
	return srv.URL
}

// uploadFile POSTs a multipart file to url and returns the status + parsed body.
func uploadFile(t *testing.T, url, field, filename, contentType string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="` + field + `"; filename="` + filename + `"`}
	h["Content-Type"] = []string{contentType}
	pw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	pw.Write(data)
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMedia_UploadServeReferenceExpand(t *testing.T) {
	root := mediaServer(t, mediaProductSchema)
	media := root + "/__media"
	api := root + "/api/v1"
	png := []byte("\x89PNG\r\n\x1a\n-fake-image-bytes")

	// Upload.
	st, body := uploadFile(t, media, "file", "logo.png", "image/png", png)
	if st != http.StatusCreated {
		t.Fatalf("upload: got %d (%v)", st, body)
	}
	rec := dataObj(t, body)
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatalf("upload returned no id: %#v", rec)
	}
	if rec["content_type"] != "image/png" {
		t.Errorf("content_type = %#v, want image/png", rec["content_type"])
	}
	if rec["url"] != "/__media/"+id+"/raw" {
		t.Errorf("derived url = %#v", rec["url"])
	}
	if size, _ := rec["size"].(float64); int(size) != len(png) {
		t.Errorf("size = %#v, want %d", rec["size"], len(png))
	}

	// Serve raw bytes.
	resp, err := http.Get(media + "/" + id + "/raw")
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("raw content-type = %q", resp.Header.Get("Content-Type"))
	}
	if !bytes.Equal(got, png) {
		t.Errorf("raw bytes mismatch")
	}

	// Reference it from a product, then expand.
	_, b := do(t, http.MethodPost, api+"/products", `{"name":"widget","image":"`+id+`"}`)
	productID := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodGet, api+"/products/"+productID+"?expand=image", "")
	img, ok := dataObj(t, b)["image"].(map[string]any)
	if !ok || img["id"] != id {
		t.Fatalf("expanded image should be the media object, got %#v", dataObj(t, b)["image"])
	}
	if img["url"] != "/__media/"+id+"/raw" {
		t.Errorf("expanded media should carry url, got %#v", img["url"])
	}

	// Where-used: from the media asset, expand the products referencing it.
	_, b = do(t, http.MethodGet, media+"/"+id+"?expand=products", "")
	users, ok := dataObj(t, b)["products"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("where-used expand should list 1 product, got %#v", dataObj(t, b)["products"])
	}

	// _media must not be reachable through the public collection API.
	if st, _ := do(t, http.MethodGet, api+"/_media", ""); st != http.StatusNotFound {
		t.Errorf("/_media should not be routable, got %d", st)
	}
}

func TestMedia_ReplaceKeepsIdAndReferences(t *testing.T) {
	root := mediaServer(t, mediaProductSchema)
	media := root + "/__media"

	_, body := uploadFile(t, media, "file", "a.txt", "text/plain", []byte("version one"))
	id := dataObj(t, body)["id"].(string)

	// Replace the bytes at the same id.
	st, body := uploadFile(t, media+"/"+id, "file", "b.txt", "text/plain", []byte("version two"))
	if st != http.StatusOK {
		t.Fatalf("replace: got %d (%v)", st, body)
	}
	if dataObj(t, body)["id"] != id {
		t.Fatalf("replace changed the id: %#v", dataObj(t, body)["id"])
	}
	resp, _ := http.Get(media + "/" + id + "/raw")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "version two" {
		t.Fatalf("replaced bytes = %q, want 'version two'", got)
	}
}

func TestMedia_DeleteReferencedIs409(t *testing.T) {
	root := mediaServer(t, mediaProductSchema)
	media := root + "/__media"
	api := root + "/api/v1"

	_, body := uploadFile(t, media, "file", "a.png", "image/png", []byte("bytes"))
	id := dataObj(t, body)["id"].(string)
	do(t, http.MethodPost, api+"/products", `{"name":"widget","image":"`+id+`"}`)

	// Referenced by a product (on_delete defaults to restrict) → 409.
	if st, _ := do(t, http.MethodDelete, media+"/"+id, ""); st != http.StatusConflict {
		t.Fatalf("deleting referenced media should be 409, got %d", st)
	}
}

func TestMedia_ContentTypeRestriction(t *testing.T) {
	def, db := newDB(t, mediaProductSchema)
	bs, _ := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	srv := mount(t, def, db, gateway.Options{Blob: bs, AllowedContentTypes: []string{"image/"}})
	media := srv.URL + "/__media"

	if st, _ := uploadFile(t, media, "file", "a.png", "image/png", []byte("x")); st != http.StatusCreated {
		t.Errorf("image upload should be allowed, got %d", st)
	}
	if st, _ := uploadFile(t, media, "file", "a.exe", "application/octet-stream", []byte("x")); st != http.StatusUnsupportedMediaType {
		t.Errorf("non-image upload should be 415, got %d", st)
	}
}

func TestMedia_DisabledWhenNoBlobStore(t *testing.T) {
	def, db := newDB(t, mediaProductSchema)
	srv := mount(t, def, db) // no Blob configured
	if st, _ := uploadFile(t, srv.URL+"/__media", "file", "a.png", "image/png", []byte("x")); st != http.StatusServiceUnavailable {
		t.Errorf("upload without a blob store should be 503, got %d", st)
	}
}
