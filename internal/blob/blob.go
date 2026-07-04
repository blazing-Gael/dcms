// Package blob is the storage abstraction for file bytes (ADR-0011).
//
// It is deliberately separate from the store package: store holds structured
// records (and its interface is locked), whereas blob holds opaque bytes keyed by
// a string. File *metadata* lives in the _media collection via store; the bytes
// live here. A local-disk adapter ships first; a single S3-compatible adapter
// (MinIO, SeaweedFS, Cloudflare R2, AWS S3, …) slots in behind the same interface.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotFound is returned by Get/Delete when no object exists for a key.
var ErrNotFound = errors.New("blob: object not found")

// Store is the byte-storage contract every backend implements.
type Store interface {
	// Put writes the object at key, reading exactly the bytes from r. size and
	// contentType are advisory metadata some backends record (S3 object headers);
	// backends that don't store them (local disk) may ignore them — the canonical
	// content type lives on the _media row.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Get opens the object at key for reading. Returns ErrNotFound if absent.
	// The caller must Close the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the object at key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error

	// URL returns an absolute URL a client may fetch the object from directly
	// (e.g. an S3/R2 object or signed URL), or "" when this store is served
	// through the gateway's /__media/{id}/raw proxy (local disk).
	URL(key string) string
}

// Config selects and locates a blob backend. Kept free of any dependency on the
// config package so blob stays a leaf; the engine maps runtime config onto it.
type Config struct {
	Driver string // "local" or "s3"
	Dir    string // local: base directory

	// S3-compatible driver (MinIO, SeaweedFS, Cloudflare R2, AWS S3, …).
	Endpoint       string // host[:port], scheme optional — e.g. "s3.amazonaws.com", "localhost:9000"
	Region         string // e.g. "us-east-1"; R2 uses "auto"
	Bucket         string
	AccessKey      string // supplied via env only (never a config file)
	SecretKey      string // supplied via env only (never a config file)
	UseSSL         *bool  // nil = infer from endpoint scheme (default TLS)
	ForcePathStyle bool   // true for MinIO/SeaweedFS (path-style addressing)
	PublicBaseURL  string // if set, URL() = this + "/" + key (public bucket / CDN); else proxied
}

// New constructs the blob store described by cfg.
func New(cfg Config) (Store, error) {
	switch strings.ToLower(cfg.Driver) {
	case "", "local":
		return newLocal(cfg.Dir)
	case "s3", "s3-compatible":
		return newS3(cfg)
	default:
		return nil, fmt.Errorf("blob: unknown driver %q (supported: local, s3)", cfg.Driver)
	}
}
