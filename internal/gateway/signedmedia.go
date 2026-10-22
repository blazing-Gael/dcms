package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/pkg/sign"
)

// Signed media URLs + multi-file assets (ADR-0037). A signed URL lets an authorized
// caller hand a private media path to something that can't send credentials — an
// <img>/<video> tag, an email link, a CDN edge. The access check runs once when the
// URL is minted (the record read, gated by the media rule / membership); afterwards
// serving is HMAC + expiry only, with no session and no DB on the per-segment path.

// URLSigner mints and verifies signed media URLs (ADR-0037). The default is HMAC
// (DCMS verifies); the seam lets a host swap in CDN token-auth or S3/R2 presigned GETs
// without touching the media handlers. Nil on Options ⇒ signing is off.
type URLSigner interface {
	// SignedURL returns a URL that serves id's file at `filePath`, valid for ttl (0 or
	// over the configured maximum = the signer's default/max), authorizing any path
	// under `scope` (a prefix, so one URL's token covers a whole ladder). principal
	// binds the token when non-empty.
	SignedURL(id, scope, filePath string, ttl time.Duration, principal string) string
	// Verify reports whether token authorizes fetching id's file at filePath now, and
	// the token's remaining lifetime — so a cache's max-age can be bounded to it.
	Verify(id, filePath, token string) (ok bool, remaining time.Duration)
}

// hmacURLSigner is the built-in URLSigner backed by pkg/sign. It composes gateway
// paths (/__media/{id}/s/{token}/{filePath}); a CDN/presign signer would instead
// return its own absolute URLs through the same interface.
type hmacURLSigner struct {
	signer *sign.Signer
	ttl    time.Duration
	window time.Duration
	now    func() time.Time
}

// NewHMACURLSigner builds the default signer. secret is required; previous secrets are
// still accepted on verify (key rotation). ttl bounds a URL's lifetime; window buckets
// expiry so repeated signs return the same URL (CDN/browser caching). Returns nil if
// no secret is supplied, so a caller can treat "no secret" as "signing off".
func NewHMACURLSigner(secret string, previous []string, ttl, window time.Duration) URLSigner {
	s := sign.New(secret, previous...)
	if s == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if window <= 0 {
		window = time.Hour
	}
	return &hmacURLSigner{signer: s, ttl: ttl, window: window, now: time.Now}
}

func (h *hmacURLSigner) SignedURL(id, scope, filePath string, ttl time.Duration, principal string) string {
	// The configured TTL is the ceiling: a caller can ask for less, but never more —
	// so an authorized user can't mint a year-long shareable link to paid content.
	if ttl <= 0 || ttl > h.ttl {
		ttl = h.ttl
	}
	exp := sign.BucketExpiry(h.now(), ttl, h.window)
	tok := h.signer.Sign(id, scope, exp, principal)
	return mediaBasePath + "/" + id + "/s/" + tok + "/" + filePath
}

func (h *hmacURLSigner) Verify(id, filePath, token string) (bool, time.Duration) {
	ok, exp := h.signer.VerifyExpiry(id, filePath, token, h.now())
	if !ok {
		return false, 0
	}
	return true, time.Until(exp)
}

// safeFilePath reports whether a derived-file path is safe to splice into a blob key:
// a relative, multi-segment path with no empty, "." or ".." segments. The blob layer
// validates again, but rejecting early gives a clean 422.
func safeFilePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// derivedContentType infers a content type from a derived file's extension, adding the
// HLS types Go's mime table omits. Used on the signed per-segment path, which must stay
// DB-free — the type comes from the path, not a stored row.
func derivedContentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".m4s", ".mp4":
		return "video/mp4"
	case ".vtt":
		return "text/vtt"
	}
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// serveBlobBytes streams a blob key with Range/ETag support (http.ServeContent). It is
// the shared tail of /raw, the authorized derived-file read, and the signed path.
func (s *Server) serveBlobBytes(w http.ResponseWriter, r *http.Request, key, contentType, etag, cacheControl string) {
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
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
	}
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	if rs, ok := body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, path.Base(key), time.Time{}, rs) // Range + If-None-Match aware
		return
	}
	_, _ = io.Copy(w, body)
}

// handleMediaSignedRaw serves bytes for a valid signed URL — HMAC + expiry only, no
// session and (for derived files) no DB, so a CDN can cache it and a video player can
// fetch a segment every few seconds. An invalid/expired token is 403.
func (s *Server) handleMediaSignedRaw(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	id := chi.URLParam(r, "id")
	token := chi.URLParam(r, "token")
	filePath := chi.URLParam(r, "*")
	ok, remaining := URLSignerVerify(s.opts.URLSigner, id, filePath, token)
	if !ok {
		writeError(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: "invalid or expired media token"})
		return
	}
	if filePath == "" {
		// Primary file: one DB read for its content type + checksum (not an auth check).
		// This is the <img src> path, not the per-segment hot path, so a read is fine.
		rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, id)
		if err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
		ct, _ := rec[schema.MediaContentType].(string)
		ck, _ := rec[schema.MediaChecksum].(string)
		s.serveBlobBytes(w, r, id, ct, ck, derivedCacheControl(filePath, remaining))
		return
	}
	if !safeFilePath(filePath) {
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "no such file"})
		return
	}
	// Derived file: type from the path, no DB.
	s.serveBlobBytes(w, r, id+"/"+filePath, derivedContentType(filePath), "", derivedCacheControl(filePath, remaining))
}

// URLSignerVerify guards a nil signer (signing disabled) and returns the verify result.
func URLSignerVerify(signer URLSigner, id, filePath, token string) (bool, time.Duration) {
	if signer == nil {
		return false, 0
	}
	return signer.Verify(id, filePath, token)
}

// derivedCacheControl bounds a cache's max-age to the token's remaining lifetime, so a
// shared cache never serves bytes past token expiry or a secret rotation. A manifest
// (.m3u8/.mpd) is marked no-cache because it changes — a live HLS playlist is rewritten
// continuously; other files (segments, renditions) are immutable within the window.
func derivedCacheControl(filePath string, remaining time.Duration) string {
	secs := max(int(remaining.Seconds()), 0)
	switch strings.ToLower(path.Ext(filePath)) {
	case ".m3u8", ".mpd":
		return "no-cache"
	}
	return fmt.Sprintf("public, max-age=%d, immutable", secs)
}

// handleMediaSign issues a signed URL for an asset after the normal access check — for
// players and hooks that need a credential-free URL (e.g. an HLS playlist). Body:
// {"scope": "hls/", "path": "hls/index.m3u8", "ttl_seconds": 3600}.
func (s *Server) handleMediaSign(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	if s.opts.URLSigner == nil {
		writeError(w, http.StatusNotImplemented, apiError{Code: "NOT_IMPLEMENTED", Message: "media URL signing is not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, id)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if !s.mediaReadable(r.Context(), rec) {
		s.mediaNotFound(w)
		return
	}
	var body struct {
		Scope      string `json:"scope"`
		Path       string `json:"path"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if r.ContentLength != 0 {
		if data, err := decodeBody(r); err == nil {
			body.Scope, _ = data["scope"].(string)
			body.Path, _ = data["path"].(string)
			if v, ok := data["ttl_seconds"].(float64); ok {
				body.TTLSeconds = int(v)
			}
		}
	}
	if body.Path != "" && !safeFilePath(body.Path) {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "invalid file path"})
		return
	}
	url := s.opts.URLSigner.SignedURL(id, body.Scope, body.Path, time.Duration(body.TTLSeconds)*time.Second, "")
	writeData(w, http.StatusOK, store.Record{"url": url})
}

// handleMediaFilePut adds a derived file under an asset (ADR-0037): PUT
// /__media/{id}/files/{path}. Authorized like replacing the asset's bytes (update),
// size/type-capped, and quota-checked against the asset owner.
func (s *Server) handleMediaFilePut(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	id := chi.URLParam(r, "id")
	filePath := chi.URLParam(r, "*")
	if !safeFilePath(filePath) {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "invalid file path"})
		return
	}
	if !s.authorizeRecordWrite(w, r, schema.MediaCollection, id, schema.ActionUpdate) {
		return
	}
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, id)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = derivedContentType(filePath)
	}
	if !s.contentTypeAllowed(contentType) {
		writeError(w, http.StatusUnsupportedMediaType, apiError{Code: "UNSUPPORTED_MEDIA_TYPE", Message: "content type " + contentType + " is not allowed"})
		return
	}

	// Cap the body and buffer it once, so we know the true size (for quota + blob Put)
	// even when Content-Length is absent. bytes.NewReader below reuses this buffer —
	// no second copy.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload())
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{Code: "TOO_LARGE", Message: fmt.Sprintf("upload exceeds the %d-byte limit", s.maxUpload())})
			return
		}
		writeError(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: "could not read request body"})
		return
	}
	newSize := int64(len(data))

	key := id + "/" + filePath
	// Net byte delta: overwriting an existing derived file only costs the difference,
	// so repeated segment uploads don't over-count against the quota.
	oldSize, _, err := s.opts.Blob.Stat(r.Context(), key)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	delta := newSize - oldSize

	// Quota headroom against the asset owner (ADR-0028): derived bytes land in the
	// owner's budget (mediaUsage counts size + derived_size), checked at admission.
	owner, _ := rec[createdByField].(string)
	if q := s.opts.MediaQuota; q != nil && owner != "" {
		p := principalFromContext(r.Context())
		roles := p.Roles
		if owner != p.ID {
			roles = nil // the owner's quota, evaluated at its default tier
		}
		if limit := q.quotaFor(roles); limit >= 0 {
			used, err := s.mediaUsage(r.Context(), owner)
			if err != nil {
				writeStoreError(w, s.logger, r, err)
				return
			}
			if used+delta > limit {
				writeError(w, http.StatusRequestEntityTooLarge, apiError{Code: "QUOTA_EXCEEDED", Message: "storage quota exceeded"})
				return
			}
		}
	}

	// Reserve the quota by bumping derived_size BEFORE writing bytes, so a crash can
	// only ever over-count (fail safe), never under-count past the limit. Compensate
	// if the blob write then fails.
	if err := s.addDerivedSize(r.Context(), rec, delta); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if err := s.opts.Blob.Put(r.Context(), key, bytes.NewReader(data), newSize, contentType); err != nil {
		_ = s.addDerivedSize(r.Context(), rec, -delta) // best-effort rollback of the reservation
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeData(w, http.StatusCreated, store.Record{"path": filePath, "size": newSize})
}

// addDerivedSize atomically adjusts an asset's derived-byte total by delta, inside a
// transaction that re-reads the row so concurrent derived-file writes don't lose an
// update. The total never goes below zero.
func (s *Server) addDerivedSize(ctx context.Context, rec store.Record, delta int64) error {
	if delta == 0 {
		return nil
	}
	id, _ := rec["id"].(string)
	return s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		cur, err := tx.FindOne(ctx, schema.MediaCollection, id)
		if err != nil {
			return err
		}
		n := max(asInt64(cur[schema.MediaDerivedSize])+delta, 0)
		_, err = tx.Update(ctx, store.WriteInput{Collection: schema.MediaCollection, Data: store.Record{"id": id, schema.MediaDerivedSize: n}})
		return err
	})
}

// asInt64 coerces a stored numeric value (int64 from the store, float64 after a JSON
// round-trip) to int64.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// handleMediaFileList lists an asset's derived files: GET /__media/{id}/files.
func (s *Server) handleMediaFileList(w http.ResponseWriter, r *http.Request) {
	if !s.mediaEnabled() {
		s.mediaUnavailable(w)
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, id)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if !s.mediaReadable(r.Context(), rec) {
		s.mediaNotFound(w)
		return
	}
	keys, err := s.opts.Blob.List(r.Context(), id+"/")
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	files := make([]store.Record, 0, len(keys))
	for _, k := range keys {
		files = append(files, store.Record{"path": strings.TrimPrefix(k, id+"/")})
	}
	writeData(w, http.StatusOK, store.Record{"files": files})
}
