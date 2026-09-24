package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — 2C media editing (ADR-0035). A file field becomes an inline widget:
// upload a new file, or pick one from the library, or clear it. Uploads go through
// the same blob pipeline as the /__media API (create row → put bytes → stamp
// size/checksum), owned by the acting admin, so quotas and the inherit access rule
// still apply. Many-file fields and disabled media stay read-only.

// adminMediaEditable reports whether a field is edited inline as a media widget: a
// single-valued file field (which the schema rewrites to a relation → _media) with a
// blob backend configured. Many-file fields fall back to the relation picker.
func (s *Server) adminMediaEditable(f schema.FieldDef) bool {
	return f.Type == schema.TypeRelation && f.Target == schema.MediaCollection && !f.Many && s.mediaEnabled()
}

// adminUploadFile ingests one multipart file part into the media library and returns
// the new media id. It mirrors the API's storeUpload core (content-type allowlist,
// per-principal quota, create-then-put-then-stamp) but returns errors for the panel
// to render instead of writing an HTTP response.
func (s *Server) adminUploadFile(ctx context.Context, p principal, fh *multipart.FileHeader) (string, error) {
	if !s.mediaEnabled() {
		return "", errors.New("media is not configured")
	}
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()

	ctype := fh.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if !s.contentTypeAllowed(ctype) {
		return "", fmt.Errorf("file type %s is not allowed", ctype)
	}
	// Per-principal storage quota (issue #31), same as the API upload path.
	if q := s.opts.MediaQuota; q != nil {
		if limit := q.quotaFor(p.Roles); limit >= 0 {
			used, err := s.mediaUsage(ctx, p.ID)
			if err != nil {
				return "", err
			}
			if used+fh.Size > limit {
				return "", fmt.Errorf("storage quota exceeded (%d of %d bytes used)", used, limit)
			}
		}
	}

	created, err := s.db.Create(ctx, store.WriteInput{
		Collection: schema.MediaCollection,
		Data: store.Record{
			schema.MediaFilename:    fh.Filename,
			schema.MediaContentType: ctype,
		},
	})
	if err != nil {
		return "", err
	}
	id, _ := created["id"].(string)
	key := id

	h := sha256.New()
	counter := &countingWriter{}
	tee := io.TeeReader(f, io.MultiWriter(h, counter))
	if err := s.opts.Blob.Put(ctx, key, tee, fh.Size, ctype); err != nil {
		_ = s.db.Delete(ctx, schema.MediaCollection, id) // compensate the orphan row
		return "", err
	}
	if _, err := s.db.Update(ctx, store.WriteInput{
		Collection: schema.MediaCollection,
		Data: store.Record{
			"id":                    id,
			schema.MediaStorageKey:  key,
			schema.MediaSize:        counter.n,
			schema.MediaChecksum:    hex.EncodeToString(h.Sum(nil)),
			schema.MediaContentType: ctype,
			schema.MediaFilename:    fh.Filename,
		},
	}); err != nil {
		return "", err
	}
	return id, nil
}

// adminMediaLibrary lists recent media as picker options, labelled by filename, with
// `selected` (the field's current value) pre-checked. It's a staff tool behind the
// panel's auth + role gate, so it lists the library directly.
func (s *Server) adminMediaLibrary(r *http.Request, selected string) []adminOption {
	if !s.mediaEnabled() {
		return nil
	}
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: schema.MediaCollection, Limit: adminListLimit, Sort: "-created_at",
	})
	if err != nil {
		return nil
	}
	opts := make([]adminOption, 0, len(page.Data))
	for _, m := range page.Data {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		label, _ := m[schema.MediaFilename].(string)
		if label == "" {
			label = id
		}
		opts = append(opts, adminOption{Value: id, Label: label, Selected: id == selected})
	}
	return opts
}

// adminApplyFiles resolves the file-field inputs on a submitted form into the record:
// an uploaded part wins, else a "remove" clears (when the field is optional), else a
// library selection sets it; anything absent is left unchanged. Returns a user-facing
// message on an upload failure. Must run after the multipart form is parsed.
func (s *Server) adminApplyFiles(r *http.Request, cd schema.CollectionDef, data store.Record) string {
	if !s.mediaEnabled() {
		return ""
	}
	p := principalFromContext(r.Context())
	for _, f := range cd.Fields {
		if !s.adminMediaEditable(f) {
			continue
		}
		if r.MultipartForm != nil {
			if fhs := r.MultipartForm.File["__file_"+f.Name]; len(fhs) > 0 && fhs[0].Size > 0 {
				id, err := s.adminUploadFile(r.Context(), p, fhs[0])
				if err != nil {
					return "Could not upload " + adminLabel(f) + ": " + err.Error()
				}
				data[f.Name] = id
				continue
			}
		}
		if r.FormValue("__clear_"+f.Name) == "1" {
			if !f.Required {
				data[f.Name] = nil
			}
			continue
		}
		if v := strings.TrimSpace(r.FormValue(f.Name)); v != "" {
			data[f.Name] = v
		}
	}
	return ""
}
