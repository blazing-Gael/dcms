package blob

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3store is the S3-compatible Store. One adapter reaches every S3-API backend —
// MinIO, SeaweedFS, Cloudflare R2, AWS S3, Backblaze B2, DigitalOcean Spaces —
// differing only by config (endpoint, region, path-style, credentials).
type s3store struct {
	client        *minio.Client
	bucket        string
	publicBaseURL string
}

func newS3(cfg Config) (*s3store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("blob: s3 driver requires a bucket")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("blob: s3 driver requires an endpoint")
	}
	host, secure := normalizeEndpoint(cfg.Endpoint, cfg.UseSSL)
	// MinIO/SeaweedFS address buckets path-style; AWS/R2 use virtual-host (auto).
	lookup := minio.BucketLookupAuto
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: s3 client: %w", err)
	}
	return &s3store{
		client:        client,
		bucket:        cfg.Bucket,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
	}, nil
}

// normalizeEndpoint strips a scheme (minio wants host[:port]) and decides TLS. An
// explicit UseSSL wins; otherwise the scheme is the hint, defaulting to TLS.
func normalizeEndpoint(endpoint string, useSSL *bool) (host string, secure bool) {
	host, secure = endpoint, true
	switch {
	case strings.HasPrefix(host, "https://"):
		host, secure = strings.TrimPrefix(host, "https://"), true
	case strings.HasPrefix(host, "http://"):
		host, secure = strings.TrimPrefix(host, "http://"), false
	}
	host = strings.TrimRight(host, "/")
	if useSSL != nil {
		secure = *useSSL
	}
	return host, secure
}

func (s *s3store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return fmt.Errorf("blob: s3 put %q: %w", key, err)
	}
	return nil
}

func (s *s3store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	// Stat first so a missing object is a clean ErrNotFound rather than an error
	// that only surfaces mid-stream.
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil // *minio.Object is an io.ReadSeekCloser → Range-serveable
}

func (s *s3store) Delete(ctx context.Context, key string) error {
	// S3 delete is idempotent: removing a missing key is not an error.
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *s3store) URL(key string) string {
	if s.publicBaseURL == "" {
		return "" // no public base → served through the gateway proxy
	}
	return s.publicBaseURL + "/" + key
}
