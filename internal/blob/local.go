package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// local is a filesystem-backed Store. Objects are written under a base directory,
// sharded by the first two characters of the key to avoid one enormous directory.
// It is proxy-served: URL always returns "" so the gateway streams bytes itself.
type local struct {
	dir string
}

// safeKey guards against path traversal: keys are engine-generated (a media id,
// optionally with an extension), so this allowlist is more than sufficient and
// makes splicing a key into a filesystem path safe.
var safeKey = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func newLocal(dir string) (*local, error) {
	if dir == "" {
		return nil, fmt.Errorf("blob: local driver requires a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blob: create %q: %w", dir, err)
	}
	return &local{dir: dir}, nil
}

func (l *local) path(key string) (string, error) {
	if !safeKey.MatchString(key) {
		return "", fmt.Errorf("blob: invalid key %q", key)
	}
	shard := key[:2]
	if len(key) < 2 {
		shard = key
	}
	return filepath.Join(l.dir, shard, key), nil
}

func (l *local) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// Write to a temp file then rename, so a reader never sees a half-written blob.
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

func (l *local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l *local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// URL is empty: local blobs are streamed through the gateway proxy.
func (l *local) URL(key string) string { return "" }
