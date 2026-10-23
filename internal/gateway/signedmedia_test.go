package gateway_test

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// Signed media URLs + multi-file assets (ADR-0037), and their join with membership
// (ADR-0036): an enrollment gates the record read that mints a signed video URL, which
// then serves segments credential-free.

const signSecret = "sign-secret-aaaaaaaaaaaaaaaaaaaaaaaa"

func testSigner() gateway.URLSigner {
	return gateway.NewHMACURLSigner(signSecret, nil, 0, 0)
}

// getRaw does a plain GET (optionally with a bearer token) and returns status, body
// bytes, and the Content-Type header.
func getRaw(t *testing.T, url, token string) (int, []byte, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header.Get("Content-Type")
}

// putRaw PUTs a raw body with a bearer token and returns the status.
func putRaw(t *testing.T, url, token, contentType string, data []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func dataStr(body map[string]any, key string) string {
	d, _ := body["data"].(map[string]any)
	s, _ := d[key].(string)
	return s
}

const signMediaSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session: { ttl: 1h }
collections:
  _media:
    access:
      read: inherit
  courses:
    fields: { title: { type: string } }
    access: { read: public, create: [admin] }
  enrollments:
    fields:
      user:   { type: relation, target: _users }
      course: { type: relation, target: courses }
    access: { read: [admin], create: [admin] }
  lessons:
    fields:
      course: { type: relation, target: courses }
      video:  { type: file }
    access:
      read:
        any:
          - admin
          - exists: { in: enrollments, match: { course: course }, principal: user }
      create: [admin]
`

// The full edtech path: an enrolled student reads a lesson (membership, #64), which
// yields a signed video URL (#65) that serves bytes with no credentials; a
// non-enrolled student can reach neither the lesson nor the media.
func TestSignedMedia_EnrollmentGatedVideo(t *testing.T) {
	srv, db := mediaAuthServer(t, signMediaSchema, gateway.Options{URLSigner: testSigner()})
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "bob@x.com", "pw-bob-123456")

	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-123456")

	// Admin uploads the lesson video, creates the course + lesson, enrolls Alice.
	_, up := uploadFileAs(t, srv.URL+"/__media", admin, "lesson.mp4", "video/mp4", []byte("VIDEO-BYTES"))
	videoID := recordID(t, up)
	_, cr := doAs(t, http.MethodPost, srv.URL+"/api/v1/courses", admin, `{"title":"Go 101"}`)
	courseID := recordID(t, cr)
	doAs(t, http.MethodPost, srv.URL+"/api/v1/lessons", admin, `{"course":"`+courseID+`","video":"`+videoID+`"}`)
	doAs(t, http.MethodPost, srv.URL+"/api/v1/enrollments", admin, `{"user":"`+aliceID+`","course":"`+courseID+`"}`)

	// Alice (enrolled) can read the media via inherit→membership, and gets a SIGNED url.
	st, mediaBody := doAs(t, http.MethodGet, srv.URL+"/__media/"+videoID, alice, "")
	if st != http.StatusOK {
		t.Fatalf("alice read media: got %d, want 200", st)
	}
	url := dataStr(mediaBody, "url")
	if !strings.Contains(url, "/s/") {
		t.Fatalf("gated media url should be signed, got %q", url)
	}

	// The signed url serves bytes with NO credentials.
	code, body, _ := getRaw(t, srv.URL+url, "")
	if code != http.StatusOK || string(body) != "VIDEO-BYTES" {
		t.Fatalf("signed fetch: got %d %q, want 200 VIDEO-BYTES", code, body)
	}

	// A tampered token is 403 (not 404 — the resource id is in the URL). Corrupt the
	// token itself (prefix a char), not the trailing ?v= cache-buster.
	bad := strings.Replace(url, "/s/", "/s/X", 1)
	if code, _, _ := getRaw(t, srv.URL+bad, ""); code != http.StatusForbidden {
		t.Fatalf("tampered token: got %d, want 403", code)
	}

	// Bob (not enrolled) can reach neither the media nor, therefore, any signed url.
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/__media/"+videoID, bob, ""); st != http.StatusNotFound {
		t.Fatalf("bob read media: got %d, want 404", st)
	}
}

// Multi-file assets: an HLS ladder lives under the asset, is listed and served through
// one scoped token, and is removed with the asset.
func TestSignedMedia_MultiFileHLS(t *testing.T) {
	srv, db := mediaAuthServer(t, signMediaSchema, gateway.Options{URLSigner: testSigner()})
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")

	_, up := uploadFileAs(t, srv.URL+"/__media", admin, "lesson.mp4", "video/mp4", []byte("ORIGINAL"))
	videoID := recordID(t, up)
	_, cr := doAs(t, http.MethodPost, srv.URL+"/api/v1/courses", admin, `{"title":"Go 101"}`)
	courseID := recordID(t, cr)
	_, lr := doAs(t, http.MethodPost, srv.URL+"/api/v1/lessons", admin, `{"course":"`+courseID+`","video":"`+videoID+`"}`)
	lessonID := recordID(t, lr)
	doAs(t, http.MethodPost, srv.URL+"/api/v1/enrollments", admin, `{"user":"`+aliceID+`","course":"`+courseID+`"}`)

	// Admin adds a derived HLS ladder under the asset.
	base := srv.URL + "/__media/" + videoID
	if code := putRaw(t, base+"/files/hls/index.m3u8", admin, "application/vnd.apple.mpegurl", []byte("#EXTM3U")); code != http.StatusCreated {
		t.Fatalf("put playlist: got %d, want 201", code)
	}
	if code := putRaw(t, base+"/files/hls/seg0.ts", admin, "video/mp2t", []byte("SEGMENT0")); code != http.StatusCreated {
		t.Fatalf("put segment: got %d, want 201", code)
	}

	// Listing the derived files (as the enrolled student, via inherit).
	_, list := doAs(t, http.MethodGet, base+"/files", alice, "")
	if files, _ := list["data"].(map[string]any)["files"].([]any); len(files) != 2 {
		t.Fatalf("file list: want 2, got %v", list)
	}

	// Alice signs an HLS URL (scope covers the whole ladder) and plays it credential-free.
	_, signed := doAs(t, http.MethodPost, base+"/sign", alice, `{"scope":"hls/","path":"hls/index.m3u8"}`)
	playlistURL := dataStr(signed, "url")
	if !strings.Contains(playlistURL, "/s/") || !strings.HasSuffix(playlistURL, "hls/index.m3u8") {
		t.Fatalf("signed playlist url looks wrong: %q", playlistURL)
	}
	if code, body, ct := getRaw(t, srv.URL+playlistURL, ""); code != http.StatusOK || string(body) != "#EXTM3U" || ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("playlist fetch: got %d %q ct=%q", code, body, ct)
	}
	// The SAME token (same scope prefix) serves a segment — swap the path in the URL.
	segURL := strings.Replace(playlistURL, "hls/index.m3u8", "hls/seg0.ts", 1)
	if code, body, ct := getRaw(t, srv.URL+segURL, ""); code != http.StatusOK || string(body) != "SEGMENT0" || ct != "video/mp2t" {
		t.Fatalf("segment fetch via same token: got %d %q ct=%q", code, body, ct)
	}

	// A token scoped to hls/ must NOT serve a path outside that scope.
	escaped := strings.Replace(playlistURL, "hls/index.m3u8", "secret.key", 1)
	if code, _, _ := getRaw(t, srv.URL+escaped, ""); code != http.StatusForbidden {
		t.Fatalf("out-of-scope path: got %d, want 403", code)
	}

	// Deleting the asset removes the derived subtree too. The lesson references the
	// video (on_delete: restrict), so unlink it first — a referenced asset is a 409.
	if st, _ := doAs(t, http.MethodDelete, base, admin, ""); st != http.StatusConflict {
		t.Fatalf("delete referenced asset: got %d, want 409", st)
	}
	doAs(t, http.MethodDelete, srv.URL+"/api/v1/lessons/"+lessonID, admin, "")
	if st, _ := doAs(t, http.MethodDelete, base, admin, ""); st != http.StatusNoContent {
		t.Fatalf("delete asset: got %d, want 204", st)
	}
	if code, _, _ := getRaw(t, srv.URL+segURL, ""); code != http.StatusNotFound {
		t.Fatalf("segment after asset delete: got %d, want 404", code)
	}
}

// Derived files count against the asset owner's storage quota (ADR-0037): a derived
// PUT that would push the owner over the limit is refused, and the running total is
// correct on overwrite.
func TestSignedMedia_DerivedFilesCountAgainstQuota(t *testing.T) {
	srv, db := mediaAuthServer(t, signMediaSchema, gateway.Options{
		URLSigner:  testSigner(),
		MediaQuota: &gateway.MediaQuotaOptions{Default: 20, Roles: map[string]int64{}},
	})
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")

	_, up := uploadFileAs(t, srv.URL+"/__media", admin, "v.mp4", "video/mp4", []byte("ORIGINAL")) // 8 bytes
	videoID := recordID(t, up)
	base := srv.URL + "/__media/" + videoID

	// 8 (primary) + 20 (derived) = 28 > 20 → refused. Without derived accounting this
	// would wrongly pass, since the primary alone is under the limit.
	if code := putRaw(t, base+"/files/big.bin", admin, "application/octet-stream", bytes.Repeat([]byte("x"), 20)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota derived PUT: got %d, want 413", code)
	}
	// 8 + 5 = 13 ≤ 20 → allowed.
	if code := putRaw(t, base+"/files/ok.bin", admin, "application/octet-stream", []byte("12345")); code != http.StatusCreated {
		t.Fatalf("within-quota derived PUT: got %d, want 201", code)
	}
	// Overwriting ok.bin (5B) with 7B costs only the +2 delta: 13 + 2 = 15 ≤ 20.
	if code := putRaw(t, base+"/files/ok.bin", admin, "application/octet-stream", []byte("1234567")); code != http.StatusCreated {
		t.Fatalf("overwrite within quota: got %d, want 201 (delta accounting)", code)
	}
}

// Signed delivery sets cache headers that respect the token (ADR-0037 review): a
// changing manifest is no-cache, a segment is cacheable but bounded by the token's
// remaining lifetime so expiry/rotation take effect at the edge.
func TestSignedMedia_CacheHeaders(t *testing.T) {
	srv, db := mediaAuthServer(t, signMediaSchema, gateway.Options{URLSigner: testSigner()})
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")
	_, up := uploadFileAs(t, srv.URL+"/__media", admin, "v.mp4", "video/mp4", []byte("ORIG"))
	videoID := recordID(t, up)
	base := srv.URL + "/__media/" + videoID
	putRaw(t, base+"/files/hls/index.m3u8", admin, "application/vnd.apple.mpegurl", []byte("#EXTM3U"))
	putRaw(t, base+"/files/hls/seg0.ts", admin, "video/mp2t", []byte("SEG"))

	_, signed := doAs(t, http.MethodPost, base+"/sign", admin, `{"scope":"hls/","path":"hls/index.m3u8"}`)
	playlistURL := dataStr(signed, "url")

	cc := func(url string) string {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()
		return resp.Header.Get("Cache-Control")
	}
	if got := cc(srv.URL + playlistURL); got != "no-cache" {
		t.Fatalf("playlist Cache-Control: got %q, want no-cache (it changes)", got)
	}
	segURL := strings.Replace(playlistURL, "hls/index.m3u8", "hls/seg0.ts", 1)
	if got := cc(srv.URL + segURL); !strings.HasPrefix(got, "public, max-age=") || !strings.Contains(got, "immutable") {
		t.Fatalf("segment Cache-Control: got %q, want public bounded + immutable", got)
	}
}

// /sign must not honour an arbitrarily long ttl_seconds — the configured TTL is the
// ceiling (ADR-0037), so an authorized user can't mint a years-long link to paid
// content. The signer's TTL and bucket window are both 1h here, so a signed URL's
// expiry (and thus the served segment's max-age) can be at most ttl + window = 2h.
func TestSignedMedia_SignTTLClamped(t *testing.T) {
	srv, db := mediaAuthServer(t, signMediaSchema, gateway.Options{URLSigner: testSigner()})
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")
	_, up := uploadFileAs(t, srv.URL+"/__media", admin, "v.mp4", "video/mp4", []byte("ORIG"))
	videoID := recordID(t, up)
	base := srv.URL + "/__media/" + videoID
	putRaw(t, base+"/files/seg.ts", admin, "video/mp2t", []byte("SEG"))

	// Ask for ~100 years.
	_, signed := doAs(t, http.MethodPost, base+"/sign", admin, `{"path":"seg.ts","ttl_seconds":3153600000}`)
	url := dataStr(signed, "url")

	cc := func(u string) string {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
		return resp.Header.Get("Cache-Control")
	}
	maxAge := parseMaxAge(t, cc(srv.URL+url))
	const ceiling = 2 * 60 * 60 // ttl (1h) + window (1h)
	if maxAge <= 0 || maxAge > ceiling {
		t.Fatalf("clamped TTL: segment max-age=%d, want 0 < maxAge <= %d (not the ~100y requested)", maxAge, ceiling)
	}
}

func parseMaxAge(t *testing.T, cacheControl string) int {
	t.Helper()
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("bad max-age %q in %q", v, cacheControl)
			}
			return n
		}
	}
	t.Fatalf("no max-age in %q", cacheControl)
	return 0
}

// Without a configured signer, a gated library falls back to the credentialed /raw
// proxy and /sign reports not-implemented.
func TestSignedMedia_NoSignerFallsBack(t *testing.T) {
	srv, db := mediaAuthServer(t, signMediaSchema, gateway.Options{}) // no URLSigner
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")

	_, up := uploadFileAs(t, srv.URL+"/__media", admin, "x.mp4", "video/mp4", []byte("X"))
	videoID := recordID(t, up)

	// Admin can always read their own upload (inherit baseline); url is the /raw proxy.
	_, mediaBody := doAs(t, http.MethodGet, srv.URL+"/__media/"+videoID, admin, "")
	if url := dataStr(mediaBody, "url"); !strings.Contains(url, "/raw") {
		t.Fatalf("without a signer, gated url should be the /raw proxy, got %q", url)
	}
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/__media/"+videoID+"/sign", admin, `{}`); st != http.StatusNotImplemented {
		t.Fatalf("sign without signer: got %d, want 501", st)
	}
}
