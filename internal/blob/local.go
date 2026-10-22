package blob

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// local is a filesystem-backed Store. Objects are written under a base directory,
// sharded by the first two characters of the asset id to avoid one enormous directory.
// It is proxy-served: URL always returns "" so the gateway streams bytes itself.
//
// A primary file has a single-segment key "{id}" and lives at "{shard}/{id}". A
// derived file (ADR-0037) has a multi-segment key "{id}/{path}" and lives under a
// SIBLING directory "{shard}/{id}.d/{path}" — never under "{shard}/{id}" itself, so
// the primary file and the derived subtree never collide on a filesystem (one can't be
// both a file and a directory). Asset ids are UUIDv7 (no dots), so an "{id}.d" sibling
// can never clash with another asset's primary key.
type local struct {
	dir string
}

// safeSeg guards against path traversal: each key segment is engine-generated (a media
// id, or a derived path component), so this allowlist plus the explicit "."/".."
// rejection makes splicing into a filesystem path safe.
var safeSeg = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func safeKeySeg(s string) bool {
	return s != "" && s != "." && s != ".." && safeSeg.MatchString(s)
}

func newLocal(dir string) (*local, error) {
	if dir == "" {
		return nil, fmt.Errorf("blob: local driver requires a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blob: create %q: %w", dir, err)
	}
	return &local{dir: dir}, nil
}

func shardOf(id string) string {
	if len(id) >= 2 {
		return id[:2]
	}
	return id
}

// derivedDir is the sibling directory holding an asset's derived files.
func (l *local) derivedDir(id string) string {
	return filepath.Join(l.dir, shardOf(id), id+".d")
}

// path maps a (validated) logical key to its filesystem location.
func (l *local) path(key string) (string, error) {
	segs := strings.Split(key, "/")
	for _, s := range segs {
		if !safeKeySeg(s) {
			return "", fmt.Errorf("blob: invalid key %q", key)
		}
	}
	id := segs[0]
	if len(segs) == 1 {
		return filepath.Join(l.dir, shardOf(id), id), nil
	}
	return filepath.Join(append([]string{l.derivedDir(id)}, segs[1:]...)...), nil
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

// List returns the derived-file keys under prefix ("{id}/"). It walks the asset's
// sibling ".d" directory and reports keys in logical "{id}/{path}" form (slash
// separated). A missing directory (no derived files) yields an empty slice.
func (l *local) List(ctx context.Context, prefix string) ([]string, error) {
	id := strings.TrimSuffix(prefix, "/")
	if !safeKeySeg(id) {
		return nil, fmt.Errorf("blob: invalid prefix %q", prefix)
	}
	base := l.derivedDir(id)
	var keys []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll // no derived files yet
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		keys = append(keys, id+"/"+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// DeletePrefix removes an asset's derived-file subtree ("{id}/"). The primary file at
// "{id}" is removed separately via Delete. Removing a missing subtree is not an error.
func (l *local) DeletePrefix(ctx context.Context, prefix string) error {
	id := strings.TrimSuffix(prefix, "/")
	if !safeKeySeg(id) {
		return fmt.Errorf("blob: invalid prefix %q", prefix)
	}
	return os.RemoveAll(l.derivedDir(id))
}

// URL is empty: local blobs are streamed through the gateway proxy.
func (l *local) URL(key string) string { return "" }
