package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fakeS3 spins up an in-process S3-compatible server with one bucket, so the
// adapter is exercised over real HTTP + SigV4 without any external service.
func fakeS3(t *testing.T, bucket string) Store {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	ts := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(ts.Close)

	s, err := New(Config{
		Driver:         "s3",
		Endpoint:       ts.URL, // http://127.0.0.1:port → scheme sets UseSSL=false
		Region:         "us-east-1",
		Bucket:         bucket,
		AccessKey:      "test",
		SecretKey:      "testsecret",
		ForcePathStyle: true, // gofakes3, like MinIO/SeaweedFS, is path-style
	})
	if err != nil {
		t.Fatalf("New s3: %v", err)
	}
	return s
}

func TestS3_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := fakeS3(t, "media")

	// A slash in the key exercises the M3 prefix layout ({id}/original).
	key := "019f2c08/original"
	payload := []byte("s3-object-bytes")
	if err := s.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: got %q", got)
	}

	// A missing key is a clean ErrNotFound, not a mid-stream failure.
	if _, err := s.Get(ctx, "nope/original"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	// Deleting a missing key is idempotent.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}

	// No public base URL configured → served through the gateway proxy.
	if s.URL(key) != "" {
		t.Errorf("URL without public_base_url should be empty, got %q", s.URL(key))
	}
}

func TestS3_PublicBaseURL(t *testing.T) {
	backend := s3mem.New()
	_ = backend.CreateBucket("assets")
	ts := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(ts.Close)

	s, err := New(Config{
		Driver: "s3", Endpoint: ts.URL, Bucket: "assets", Region: "auto",
		AccessKey: "k", SecretKey: "s", ForcePathStyle: true,
		PublicBaseURL: "https://cdn.example.com/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.URL("a/b.png"); got != "https://cdn.example.com/a/b.png" {
		t.Fatalf("public URL = %q", got)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	tt := func(b bool) *bool { return &b }
	cases := []struct {
		in         string
		useSSL     *bool
		wantHost   string
		wantSecure bool
	}{
		{"https://s3.amazonaws.com", nil, "s3.amazonaws.com", true},
		{"http://localhost:9000", nil, "localhost:9000", false},
		{"localhost:9000", nil, "localhost:9000", true},             // default TLS
		{"http://localhost:9000", tt(true), "localhost:9000", true}, // explicit override wins
		{"https://minio.local/", nil, "minio.local", true},          // trailing slash trimmed
	}
	for _, c := range cases {
		host, secure := normalizeEndpoint(c.in, c.useSSL)
		if host != c.wantHost || secure != c.wantSecure {
			t.Errorf("normalizeEndpoint(%q,%v) = (%q,%v), want (%q,%v)", c.in, c.useSSL, host, secure, c.wantHost, c.wantSecure)
		}
	}
}

func TestS3_RequiresBucketAndEndpoint(t *testing.T) {
	if _, err := New(Config{Driver: "s3", Endpoint: "x"}); err == nil {
		t.Error("s3 without bucket should error")
	}
	if _, err := New(Config{Driver: "s3", Bucket: "b"}); err == nil {
		t.Error("s3 without endpoint should error")
	}
}
