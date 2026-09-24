package gateway_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel 2C: inline media upload + library picker on a file field.

const adminMediaSchema = `
version: "1"
auth:
  roles:
    admin: { label: Admin }
collections:
  products:
    access:
      read:   authenticated
      create: authenticated
      update: authenticated
      delete: authenticated
    fields:
      name:  { type: string, required: true }
      image: { type: file }
`

func newAdminMediaServer(t *testing.T) (string, store.Adapter) {
	t.Helper()
	def, db := newDB(t, adminMediaSchema)
	bs, err := blob.New(blob.Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	srv := mount(t, def, db, gateway.Options{Authenticator: gateway.NewSessionAuthenticator(db), Blob: bs})
	return srv.URL, db
}

// postMultipart submits a multipart form (fields + one file part) with the jar's
// cookies, following redirects.
func postMultipart(t *testing.T, c *http.Client, url string, fields map[string]string, fileField, filename string, data []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileField != "" {
		h := map[string][]string{
			"Content-Disposition": {`form-data; name="` + fileField + `"; filename="` + filename + `"`},
			"Content-Type":        {"image/png"},
		}
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		pw.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("multipart post: %v", err)
	}
	return resp.StatusCode, readAll(resp)
}

func TestAdmin2C_MediaUploadAndPicker(t *testing.T) {
	base, db := newAdminMediaServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	// Create a product (no image yet).
	tok := csrfToken(t, c, base)
	resp, _ := c.PostForm(base+"/__admin/c/products", url.Values{"name": {"Widget"}, "csrf": {tok}})
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "products", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)

	// The edit form is multipart and offers a file input for the image field.
	_, form := getBody(t, c, base+"/__admin/c/products/"+id)
	if !strings.Contains(form, `enctype="multipart/form-data"`) || !strings.Contains(form, `name="__file_image"`) {
		t.Fatalf("edit form should be a multipart form with a file input:\n%s", excerpt(form, "image"))
	}

	// Upload an image through the panel edit form.
	tok = csrfToken(t, c, base)
	png := []byte("\x89PNG\r\n\x1a\n-fake-bytes-for-test")
	st, _ := postMultipart(t, c, base+"/__admin/c/products/"+id,
		map[string]string{"name": "Widget", "csrf": tok}, "__file_image", "hero.png", png)
	if st != http.StatusOK && st != http.StatusSeeOther {
		t.Fatalf("upload submit status %d", st)
	}

	// A media row was created and the product now references it.
	media, _ := db.Find(context.Background(), store.Query{Collection: "_media", SkipCount: true})
	if len(media.Data) != 1 {
		t.Fatalf("want 1 media row after upload, got %d", len(media.Data))
	}
	mediaID, _ := media.Data[0]["id"].(string)
	rec, _ := db.FindOne(context.Background(), "products", id)
	if rec["image"] != mediaID {
		t.Fatalf("product.image should point at the uploaded media %q, got %v", mediaID, rec["image"])
	}

	// The edit form now previews the current image and lists it in the library picker.
	_, form = getBody(t, c, base+"/__admin/c/products/"+id)
	if !strings.Contains(form, "admin-media") || !strings.Contains(form, mediaID) {
		t.Fatalf("edit form should preview the image and list it in the library picker:\n%s", excerpt(form, "image"))
	}

	// Clearing removes the reference.
	tok = csrfToken(t, c, base)
	resp, _ = c.PostForm(base+"/__admin/c/products/"+id, url.Values{"name": {"Widget"}, "__clear_image": {"1"}, "csrf": {tok}})
	resp.Body.Close()
	rec, _ = db.FindOne(context.Background(), "products", id)
	if v, ok := rec["image"]; ok && v != nil && v != "" {
		t.Fatalf("image should be cleared, got %v", rec["image"])
	}
}
