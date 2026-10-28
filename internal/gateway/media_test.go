package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
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
	// URL is the proxy route, version-stamped so a replace busts caches.
	if u, _ := rec["url"].(string); !strings.HasPrefix(u, "/__media/"+id+"/raw?v=") {
		t.Errorf("derived url = %#v, want /__media/%s/raw?v=…", rec["url"], id)
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
	if u, _ := img["url"].(string); !strings.HasPrefix(u, "/__media/"+id+"/raw?v=") {
		t.Errorf("expanded media should carry a versioned url, got %#v", img["url"])
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

// TestMedia_RawServedSafely pins GHSA-74xf-v9pf-rhj4: uploader-controlled bytes
// must never be rendered as script on the DCMS origin. Every /raw response carries
// nosniff + a locked-down CSP, and anything not safe to render inline (HTML, SVG,
// XML, unknown types) is served as an attachment; raster images stay inline.
func TestMedia_RawServedSafely(t *testing.T) {
	root := mediaServer(t, mediaProductSchema)
	media := root + "/__media"

	get := func(id string) *http.Response {
		resp, err := http.Get(media + "/" + id + "/raw")
		if err != nil {
			t.Fatalf("raw: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	upload := func(filename, ct string, data []byte) string {
		st, body := uploadFile(t, media, "file", filename, ct, data)
		if st != http.StatusCreated {
			t.Fatalf("upload %s: got %d (%v)", filename, st, body)
		}
		return dataObj(t, body)["id"].(string)
	}

	// Dangerous uploads are downloaded, never rendered.
	for _, tc := range []struct{ name, filename, ct string }{
		{"html", "x.html", "text/html"},
		{"svg", "x.svg", "image/svg+xml"},
		{"xml", "x.xml", "text/xml"},
		{"xhtml", "x.xhtml", "application/xhtml+xml"},
		{"unknown", "x.bin", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := upload(tc.filename, tc.ct, []byte(`<script>alert(document.cookie)</script>`))
			resp := get(id)
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
				t.Errorf("CSP = %q, want a locked-down policy", csp)
			}
			if disp := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment") {
				t.Errorf("Content-Disposition = %q, want attachment", disp)
			}
		})
	}

	// A raster image renders inline (preview must still work) but keeps nosniff.
	id := upload("logo.png", "image/png", []byte("\x89PNG\r\n\x1a\nfake"))
	resp := get(id)
	if disp := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "inline") {
		t.Errorf("image Content-Disposition = %q, want inline", disp)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("image X-Content-Type-Options = %q, want nosniff", got)
	}
}

// bucketURLStore wraps a local store but reports a direct object URL, standing in
// for a public S3/R2 bucket.
type bucketURLStore struct{ blob.Store }

func (bucketURLStore) URL(key string) string { return "https://bucket.example/" + key }

// TestMedia_DirectURLsOnlyForSafeTypes pins GHSA-74xf-v9pf-rhj4 on direct-URL
// backends: a bucket serves the uploader's type with none of DCMS's headers, so
// only safe-inline types may be handed out (or redirected to) directly; HTML/SVG
// stay on /raw. Also: PDFs keep nosniff without the sandbox CSP, and a non-ASCII
// filename is RFC 2231 encoded.
func TestMedia_DirectURLsOnlyForSafeTypes(t *testing.T) {
	def, db := newDB(t, mediaProductSchema)
	local, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	srv := mount(t, def, db, gateway.Options{Blob: bucketURLStore{local}})
	media := srv.URL + "/__media"
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for _, tc := range []struct {
		filename, ct string
		direct       bool
	}{
		{"logo.png", "image/png", true},
		{"x.html", "text/html", false},
		{"x.svg", "image/svg+xml", false},
	} {
		_, body := uploadFile(t, media, "file", tc.filename, tc.ct, []byte("data"))
		rec := dataObj(t, body)
		u, _ := rec["url"].(string)
		if got := strings.HasPrefix(u, "https://bucket.example/"); got != tc.direct {
			t.Errorf("%s url = %q, direct=%v want %v", tc.ct, u, got, tc.direct)
		}
		resp, err := noFollow.Get(media + "/" + rec["id"].(string) + "/raw")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if redirected := resp.StatusCode == http.StatusFound; redirected != tc.direct {
			t.Errorf("%s /raw status %d, redirect=%v want %v", tc.ct, resp.StatusCode, redirected, tc.direct)
		}
		if !tc.direct && resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s served without nosniff", tc.ct)
		}
	}

	// PDF (served by DCMS itself, no bucket): inline, nosniff, no sandbox CSP; a
	// non-ASCII name survives encoded.
	plain := mediaServer(t, mediaProductSchema) + "/__media"
	_, body := uploadFile(t, plain, "file", "নোট.pdf", "application/pdf", []byte("%PDF-1.4"))
	resp, err := http.Get(plain + "/" + dataObj(t, body)["id"].(string) + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Security-Policy") != "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("pdf headers: CSP=%q nosniff=%q", resp.Header.Get("Content-Security-Policy"), resp.Header.Get("X-Content-Type-Options"))
	}
	if d := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(d, "inline") || !strings.Contains(d, "filename*=utf-8''") {
		t.Errorf("pdf Content-Disposition = %q, want inline with an RFC 2231 filename", d)
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

func TestMedia_CacheHeadersAndVersionedURL(t *testing.T) {
	root := mediaServer(t, mediaProductSchema)
	media := root + "/__media"

	_, b := uploadFile(t, media, "file", "a.txt", "text/plain", []byte("one"))
	rec := dataObj(t, b)
	id := rec["id"].(string)
	url1, _ := rec["url"].(string)

	// The raw endpoint sets an ETag (from the checksum) and Cache-Control.
	resp, _ := http.Get(media + "/" + id + "/raw")
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" || resp.Header.Get("Cache-Control") == "" {
		t.Fatalf("raw should set ETag + Cache-Control, got etag=%q cc=%q", etag, resp.Header.Get("Cache-Control"))
	}

	// A conditional request with the matching ETag → 304 Not Modified.
	req, _ := http.NewRequest(http.MethodGet, media+"/"+id+"/raw", nil)
	req.Header.Set("If-None-Match", etag)
	cond, _ := http.DefaultClient.Do(req)
	cond.Body.Close()
	if cond.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match should give 304, got %d", cond.StatusCode)
	}

	// Replacing the bytes changes the checksum → a different versioned URL, so
	// caches holding the old bytes are busted.
	_, b = uploadFile(t, media+"/"+id, "file", "a.txt", "text/plain", []byte("two"))
	url2, _ := dataObj(t, b)["url"].(string)
	if url2 == url1 || url2 == "" {
		t.Fatalf("replace should change the versioned url: url1=%q url2=%q", url1, url2)
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
