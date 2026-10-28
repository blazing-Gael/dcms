package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
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
	now := h.now()
	ok, exp := h.signer.VerifyExpiry(id, filePath, token, now)
	if !ok {
		return false, 0
	}
	return true, exp.Sub(now) // the signer's clock, not the wall clock, so tests stay deterministic
}

// safeFilePath reports whether a derived-file path is safe to splice into a blob key:
// a relative, multi-segment path with no empty, "." or ".." segments. The blob layer
// validates again, but rejecting early gives a clean 422. Control characters are
// refused too: "\n" is the signed-token payload separator.
func safeFilePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	if strings.IndexFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// derivedTypes is the fixed set of derived-file extensions and the type each is
// served as (GHSA-74xf-v9pf-rhj4). The type comes only from the extension — never
// from the uploader — and every entry is safe to render inline, so a derived file
// can't be HTML/SVG/XML on the DCMS origin.
var derivedTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".mpd":  "application/dash+xml",
	".m4s":  "video/mp4",
	".mp4":  "video/mp4",
	".m4a":  "audio/mp4",
	".ts":   "video/mp2t",
	".aac":  "audio/aac",
	".vtt":  "text/vtt",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".avif": "image/avif",
}

// derivedExtList is the sorted allowlist, for error messages.
func derivedExtList() string {
	exts := make([]string, 0, len(derivedTypes))
	for e := range derivedTypes {
		exts = append(exts, e)
	}
	slices.Sort(exts)
	return strings.Join(exts, ", ")
}

// derivedContentType returns the type a derived file is served as, from its
// extension. Used on the signed per-segment path, which must stay DB-free. An
// extension outside derivedTypes (only possible for files stored before the
// allowlist) is served as an opaque download.
func derivedContentType(p string) string {
	if ct, ok := derivedTypes[strings.ToLower(path.Ext(p))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// serveBlobBytes streams a blob key with Range/ETag support (http.ServeContent). It is
// the shared tail of /raw, the authorized derived-file read, and the signed path.
// Every response carries the media safety headers (setMediaSafetyHeaders); filename
// names the download when the type isn't safe to render inline.
func (s *Server) serveBlobBytes(w http.ResponseWriter, r *http.Request, key, contentType, filename, etag, cacheControl string) {
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
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	setMediaSafetyHeaders(w, contentType, filename)
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
		filename, _ := rec[schema.MediaFilename].(string)
		s.serveBlobBytes(w, r, id, ct, filename, ck, derivedCacheControl(filePath, remaining))
		return
	}
	if !safeFilePath(filePath) {
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "no such file"})
		return
	}
	// Derived file: type from the path, no DB.
	s.serveBlobBytes(w, r, id+"/"+filePath, derivedContentType(filePath), path.Base(filePath), "", derivedCacheControl(filePath, remaining))
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

// safeSignScope reports whether a sign scope is valid: empty (the whole asset) or a
// safe directory prefix ending in "/". The trailing slash keeps "hls" from also
// covering "hls-private/", and safeFilePath rules out "\n" (the token payload's
// separator), "..", and absolute paths.
func safeSignScope(scope string) bool {
	if scope == "" {
		return true
	}
	return strings.HasSuffix(scope, "/") && safeFilePath(strings.TrimSuffix(scope, "/"))
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
		// A malformed body is an error, not "no body": falling through would mint an
		// empty-scope token covering every file of the asset.
		data, err := decodeBody(r)
		if err != nil {
			writeDecodeError(w, err)
			return
		}
		body.Scope, _ = data["scope"].(string)
		body.Path, _ = data["path"].(string)
		if v, ok := data["ttl_seconds"].(float64); ok {
			body.TTLSeconds = int(v)
		}
	}
	if body.Path != "" && !safeFilePath(body.Path) {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "invalid file path"})
		return
	}
	if !safeSignScope(body.Scope) {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: `scope must be empty or a directory prefix ending in "/"`})
		return
	}
	if body.Scope != "" && body.Path != "" && !strings.HasPrefix(body.Path, body.Scope) {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "path must be under scope"})
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

	// The type comes from the extension alone, which must be on the derived-file
	// allowlist; the declared Content-Type is ignored. Checking the declared header but
	// serving by extension let `x.html` sent as image/png through (GHSA-74xf-v9pf-rhj4).
	if _, ok := derivedTypes[strings.ToLower(path.Ext(filePath))]; !ok {
		writeError(w, http.StatusUnsupportedMediaType, apiError{Code: "UNSUPPORTED_MEDIA_TYPE", Message: "derived files must be one of: " + derivedExtList()})
		return
	}
	contentType := derivedContentType(filePath)
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

	// Derived bytes count against the asset owner's budget (ADR-0028/0037).
	owner, _ := rec[createdByField].(string)
	limit := int64(-1) // -1 = no quota / unlimited
	if q := s.opts.MediaQuota; q != nil && owner != "" {
		p := principalFromContext(r.Context())
		roles := p.Roles
		if owner != p.ID {
			roles = nil // the owner's quota, evaluated at its default tier
		}
		limit = q.quotaFor(roles)
	}

	// Check the quota and reserve the bytes (bump derived_size) in ONE transaction, so
	// two concurrent uploads can't both pass a check against stale usage and together
	// overshoot the limit — SQLite serializes transactions, making it atomic. This runs
	// before the blob write; if the write then fails we compensate. A crash in that gap
	// over-counts for a growing file (fail safe) or slightly under-counts for a smaller
	// overwrite (delta < 0) — harmless either way, self-corrected by the next write.
	if err := s.reserveDerivedSize(r.Context(), rec, owner, delta, limit); err != nil {
		if errors.Is(err, errMediaQuota) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{Code: "QUOTA_EXCEEDED", Message: "storage quota exceeded"})
			return
		}
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

// errMediaQuota signals that a derived-file write would exceed the owner's storage
// quota; reserveDerivedSize returns it so the handler maps it to 413.
var errMediaQuota = errors.New("media storage quota exceeded")

// reserveDerivedSize checks the owner's quota and reserves delta bytes on the asset's
// derived-size counter in a single transaction (ADR-0037). Doing both inside one Tx is
// what makes the check-then-reserve atomic: SQLite runs transactions serially, so a
// concurrent upload can't read stale usage and overshoot the limit. limit < 0 means no
// quota. On an exceeded quota it returns errMediaQuota and makes no change.
func (s *Server) reserveDerivedSize(ctx context.Context, rec store.Record, owner string, delta, limit int64) error {
	if delta == 0 {
		return nil
	}
	id, _ := rec["id"].(string)
	return s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		if limit >= 0 && owner != "" {
			used, err := mediaUsageDB(ctx, tx, owner)
			if err != nil {
				return err
			}
			if used+delta > limit {
				return errMediaQuota
			}
		}
		return updateDerivedSize(ctx, tx, id, delta)
	})
}

// addDerivedSize atomically adjusts an asset's derived-byte total by delta (no quota
// check) — used to compensate a reservation when the blob write fails.
func (s *Server) addDerivedSize(ctx context.Context, rec store.Record, delta int64) error {
	if delta == 0 {
		return nil
	}
	id, _ := rec["id"].(string)
	return s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		return updateDerivedSize(ctx, tx, id, delta)
	})
}

// updateDerivedSize re-reads the row (so concurrent writes don't lose an update) and
// adds delta to derived_size, floored at zero.
func updateDerivedSize(ctx context.Context, tx store.DB, id string, delta int64) error {
	cur, err := tx.FindOne(ctx, schema.MediaCollection, id)
	if err != nil {
		return err
	}
	n := max(asInt64(cur[schema.MediaDerivedSize])+delta, 0)
	_, err = tx.Update(ctx, store.WriteInput{Collection: schema.MediaCollection, Data: store.Record{"id": id, schema.MediaDerivedSize: n}})
	return err
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
