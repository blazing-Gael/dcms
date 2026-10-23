package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestLocal_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := New(Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	payload := []byte("hello bytes")
	if err := s.Put(ctx, "abcd1234.txt", bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(ctx, "abcd1234.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: got %q", got)
	}

	if s.URL("abcd1234.txt") != "" {
		t.Errorf("local store should be proxy-served (empty URL)")
	}

	if err := s.Delete(ctx, "abcd1234.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "abcd1234.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	// Deleting a missing key is not an error.
	if err := s.Delete(ctx, "abcd1234.txt"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestLocal_RejectsUnsafeKeys(t *testing.T) {
	ctx := context.Background()
	s, _ := New(Config{Driver: "local", Dir: t.TempDir()})
	// Traversal and empty segments are rejected; a multi-segment derived key is NOT
	// (that is the ADR-0037 layout "{id}/{path}").
	for _, key := range []string{"../escape", "..", "", "a/../b", "a//b", "a/..", "/leading"} {
		if err := s.Put(ctx, key, bytes.NewReader([]byte("x")), 1, ""); err == nil {
			t.Errorf("Put(%q) should be rejected", key)
		}
	}
}

// Derived files (multi-segment keys) round-trip, and List/DeletePrefix see them as a
// group under the asset prefix, with the primary file untouched (ADR-0037).
func TestLocal_MultiFileAsset(t *testing.T) {
	ctx := context.Background()
	s, _ := New(Config{Driver: "local", Dir: t.TempDir()})
	put := func(key, body string) {
		if err := s.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body)), ""); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	put("01abideadbeef", "primary") // primary file at {id}
	put("01abideadbeef/hls/index.m3u8", "playlist")
	put("01abideadbeef/hls/seg0.ts", "seg0")

	// A derived file round-trips.
	rc, err := s.Get(ctx, "01abideadbeef/hls/seg0.ts")
	if err != nil {
		t.Fatalf("Get derived: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "seg0" {
		t.Fatalf("derived round-trip: got %q", got)
	}

	// List enumerates only the derived files, in logical {id}/{path} form.
	keys, err := s.List(ctx, "01abideadbeef/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List: want 2 derived files, got %v", keys)
	}
	for _, k := range keys {
		if k != "01abideadbeef/hls/index.m3u8" && k != "01abideadbeef/hls/seg0.ts" {
			t.Fatalf("unexpected derived key %q", k)
		}
	}

	// DeletePrefix removes the derived subtree but leaves the primary.
	if err := s.DeletePrefix(ctx, "01abideadbeef/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if _, err := s.Get(ctx, "01abideadbeef/hls/seg0.ts"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("derived after DeletePrefix: want ErrNotFound, got %v", err)
	}
	if rc, err := s.Get(ctx, "01abideadbeef"); err != nil {
		t.Fatalf("primary must survive DeletePrefix of derived: %v", err)
	} else {
		rc.Close()
	}
	// List of an emptied asset is empty, not an error.
	if keys, err := s.List(ctx, "01abideadbeef/"); err != nil || len(keys) != 0 {
		t.Fatalf("List after delete: want empty, got %v (err %v)", keys, err)
	}
}

func TestNew_UnknownDriver(t *testing.T) {
	if _, err := New(Config{Driver: "gopher"}); err == nil {
		t.Fatal("unknown driver should error")
	}
}
