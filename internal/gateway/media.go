package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// ── Media library (ADR-0011) ─────────────────────────────────────────────────
//
// Metadata rows live in the _media collection (via the store); the bytes live in
// the blob store. These endpoints own the byte path — upload, replace, serve —
// while everything else (referencing media from a record, expanding it,
// referential integrity, "where used") reuses the relation machinery.

func (s *Server) mediaEnabled() bool { return s.opts.Blob != nil }

func (s *Server) mediaUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, apiError{
		Code: "UNAVAILABLE", Message: "media storage is not configured",
	})
}

func (s *Server) maxUpload() int64 {
	if s.opts.MaxUploadBytes > 0 {
		return s.opts.MaxUploadBytes
	}
	return defaultMaxUploadBytes
}

// contentTypeAllowed matches a content type against the configured allowlist. An
// entry ending in "/" (e.g. "image/") is a prefix match; anything else is exact.
// An empty allowlist accepts everything.
func (s *Server) contentTypeAllowed(ct string) bool {
	if len(s.opts.AllowedContentTypes) == 0 {
		return true
	}
	for _, a := range s.opts.AllowedContentTypes {
		if strings.HasSuffix(a, "/") {
			if strings.HasPrefix(ct, a) {
				return true
			}
		} else if ct == a {
			return true
		}
	}
	return false
}

// addMediaURL injects the derived, non-stored `url` onto a media record: the
// backend's direct URL when it serves bytes itself (S3/R2), else the gateway's
// proxy route. The URL carries a version token derived from the checksum, so a
// replace-in-place (same key/id) yields a new URL and busts any CDN/browser
// cache holding the old bytes.
func (s *Server) addMediaURL(rec store.Record) {
	// storage_key is the internal blob path — used to build the URL, then dropped
	// so it never reaches a client (ADR-0015). addMediaURL is the choke point every
	// media serialization passes through (single, list, expansion, manifest).
	defer delete(rec, schema.MediaStorageKey)
	if s.opts.Blob == nil {
		return
	}
	key, _ := rec[schema.MediaStorageKey].(string)
	id, _ := rec["id"].(string)
	if key == "" || id == "" {
		return
	}
	ver := mediaVersion(rec)
	if u := s.opts.Blob.URL(key); u != "" {
		rec["url"] = withVersion(u, ver)
		return
	}
	rec["url"] = withVersion(mediaBasePath+"/"+id+"/raw", ver)
}

// mediaVersion returns a short, content-derived cache-busting token: the head of
// the checksum, or the updated_at timestamp as a fallback.
func mediaVersion(rec store.Record) string {
	if ck, _ := rec[schema.MediaChecksum].(string); len(ck) >= 8 {
		return ck[:8]
	}
	if ts, _ := rec["updated_at"].(string); ts != "" {
		return ts
	}
	return ""
}

// withVersion appends a `v` query param to a URL, choosing the right separator.
func withVersion(u, ver string) string {
	if ver == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "v=" + url.QueryEscape(ver)
}

// coerceExpanded shapes a related record for a response, adding the derived media
// URL when the related record is a media asset (so an expanded product.image
// carries its url like a directly-fetched media record does).
func (s *Server) coerceExpanded(ctx context.Context, collection string, rec store.Record) {
	s.collections[collection].CoerceResponse(rec)
	if collection == schema.MediaCollection {
		s.addMediaURL(rec)
	}
	redactSecrets(collection, rec)
	s.maskReadFields(ctx, collection, rec)
}

// redactSecrets strips fields that must never reach a client from a serialized
// record. Today that is _users.password_hash — the same defense-in-depth pattern
// as media's storage_key (ADR-0016): applied at every serialization choke point,
// so no expansion, manifest, or list path can leak it.
func redactSecrets(collection string, rec store.Record) {
	if collection == schema.UsersCollection {
		delete(rec, schema.UserPasswordHash)
	}
}

// writeMedia shapes and writes a single media record, adding its url and honoring
// ?expand= (which, on a media record, answers "where is this used?" via reverse
// relations).
func (s *Server) writeMedia(w http.ResponseWriter, r *http.Request, status int, rec store.Record) {
	s.collections[schema.MediaCollection].CoerceResponse(rec)
	s.addMediaURL(rec)
	var included refManifest
	if exp := parseExpand(r.URL.Query()); len(exp) > 0 {
		included = refManifest{}
		if err := s.expandRecord(r.Context(), schema.MediaCollection, rec, exp, included); err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
	}
	writeDataWith(w, r, status, rec, included)
}

func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
	q, err := s.parseListQuery(r.URL.Query(), schema.MediaCollection)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	page, err := s.db.Find(r.Context(), q)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	for _, rec := range page.Data {
		s.collections[schema.MediaCollection].CoerceResponse(rec)
		s.addMediaURL(rec)
	}
	writeListWith(w, r, page, effectiveLimit(q.Limit), nil)
}

func (s *Server) handleMediaGet(w http.ResponseWriter, r *http.Request) {
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeMedia(w, r, http.StatusOK, rec)
}

func (s *Server) handleMediaUpload(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	if rec, ok := s.storeUpload(w, r, ""); ok {
		s.writeMedia(w, r, http.StatusCreated, rec)
	}
}

// handleMediaReplace swaps the bytes behind an existing media id, keeping the id
// and every reference to it intact — the "replace image" workflow.
func (s *Server) handleMediaReplace(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := s.db.FindOne(r.Context(), schema.MediaCollection, id); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if rec, ok := s.storeUpload(w, r, id); ok {
		s.writeMedia(w, r, http.StatusOK, rec)
	}
}

// handleMediaPatch edits editable metadata only (alt/title/caption/filename); the
// bytes are replaced via handleMediaReplace, not here.
func (s *Server) handleMediaPatch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	upd := store.Record{"id": id}
	for _, k := range []string{schema.MediaAlt, schema.MediaTitle, schema.MediaCaption, schema.MediaFilename} {
		if v, ok := data[k]; ok {
			upd[k] = v
		}
	}
	rec, err := s.db.Update(r.Context(), store.WriteInput{Collection: schema.MediaCollection, Data: upd})
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeMedia(w, r, http.StatusOK, rec)
}

func (s *Server) handleMediaDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, id)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if err := s.db.Delete(r.Context(), schema.MediaCollection, id); err != nil {
		// Still referenced by a record (on_delete: restrict) → a real conflict.
		if errors.Is(err, store.ErrIntegrity) {
			writeError(w, http.StatusConflict, apiError{
				Code: "CONFLICT", Message: "media is still referenced by other records",
			})
			return
		}
		writeStoreError(w, s.logger, r, err)
		return
	}
	// Best-effort blob cleanup; the row is already gone.
	if s.opts.Blob != nil {
		if key, _ := rec[schema.MediaStorageKey].(string); key != "" {
			_ = s.opts.Blob.Delete(r.Context(), key)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMediaRaw serves the bytes: a redirect for backends that expose a direct
// URL (S3/R2), otherwise a streamed response. Streaming uses http.ServeContent so
// HTTP Range requests (video scrubbing, resumable downloads) work for free.
func (s *Server) handleMediaRaw(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	key, _ := rec[schema.MediaStorageKey].(string)
	if key == "" {
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "no file for this media record"})
		return
	}
	if u := s.opts.Blob.URL(key); u != "" {
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	body, err := s.opts.Blob.Get(r.Context(), key)
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "file bytes are missing"})
		return
	}
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	defer body.Close()

	if ct, _ := rec[schema.MediaContentType].(string); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// The checksum is a strong validator → ETag for cheap 304 revalidation.
	// Versioned URLs (?v=…) address immutable content, so cache them hard; a bare
	// /raw hit gets a short TTL and revalidates via ETag.
	if ck, _ := rec[schema.MediaChecksum].(string); ck != "" {
		w.Header().Set("ETag", `"`+ck+`"`)
	}
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	filename, _ := rec[schema.MediaFilename].(string)
	if rs, ok := body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, filename, time.Time{}, rs) // Range + If-None-Match aware
		return
	}
	_, _ = io.Copy(w, body)
}

// countingWriter tallies bytes written through it, so an upload's true size is
// known even when the multipart header omits it.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// storeUpload ingests a multipart upload: validate the file part, write the bytes
// to the blob store (computing size + checksum), and persist the _media row. When
// existingID is empty it creates a new row (its id is the storage key); otherwise
// it overwrites the bytes for that id, keeping references intact. On any failure
// it writes the HTTP error and returns ok=false (compensating a partial create).
func (s *Server) storeUpload(w http.ResponseWriter, r *http.Request, existingID string) (store.Record, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload())
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{
				Code: "TOO_LARGE", Message: fmt.Sprintf("upload exceeds the %d-byte limit", s.maxUpload()),
			})
		} else {
			writeError(w, http.StatusUnprocessableEntity, apiError{
				Code: "VALIDATION_ERROR", Message: "invalid multipart form: " + err.Error(),
			})
		}
		return nil, false
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{
			Code: "VALIDATION_ERROR", Message: "expected a multipart 'file' part",
		})
		return nil, false
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if !s.contentTypeAllowed(contentType) {
		writeError(w, http.StatusUnsupportedMediaType, apiError{
			Code: "UNSUPPORTED_MEDIA_TYPE", Message: "content type " + contentType + " is not allowed",
		})
		return nil, false
	}

	// A new asset needs its id before we can key the blob, so create the row first.
	mediaID := existingID
	if existingID == "" {
		created, err := s.db.Create(r.Context(), store.WriteInput{
			Collection: schema.MediaCollection,
			Data: store.Record{
				schema.MediaFilename:    header.Filename,
				schema.MediaContentType: contentType,
				schema.MediaAlt:         r.FormValue("alt"),
				schema.MediaTitle:       r.FormValue("title"),
				schema.MediaCaption:     r.FormValue("caption"),
			},
		})
		if err != nil {
			writeStoreError(w, s.logger, r, err)
			return nil, false
		}
		mediaID, _ = created["id"].(string)
	}
	key := mediaID

	h := sha256.New()
	counter := &countingWriter{}
	tee := io.TeeReader(file, io.MultiWriter(h, counter))
	if err := s.opts.Blob.Put(r.Context(), key, tee, header.Size, contentType); err != nil {
		if existingID == "" { // compensate the row we just created
			_ = s.db.Delete(r.Context(), schema.MediaCollection, mediaID)
		}
		writeStoreError(w, s.logger, r, err)
		return nil, false
	}

	rec, err := s.db.Update(r.Context(), store.WriteInput{
		Collection: schema.MediaCollection,
		Data: store.Record{
			"id":                    mediaID,
			schema.MediaStorageKey:  key,
			schema.MediaSize:        counter.n,
			schema.MediaChecksum:    hex.EncodeToString(h.Sum(nil)),
			schema.MediaContentType: contentType,
			schema.MediaFilename:    header.Filename,
		},
	})
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return nil, false
	}
	return rec, true
}
