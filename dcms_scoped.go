package dcms

import (
	"context"
	"io"
	"time"
)

// ScopedStore reads and writes as one principal (ADR-0038), enforcing exactly what
// the HTTP API enforces for them. Get it from App.As or Request.Store.
//
// Errors: ErrNotFound (absent or hidden from the principal), ErrForbidden /
// ErrUnauthenticated (denied), *ValidationError, ErrVersionConflict, ErrReferenced,
// *HookError (a hook refused the write), ErrUnknownCollection.
type ScopedStore struct {
	app *App
	p   Principal
}

// Principal returns who this store acts as.
func (s ScopedStore) Principal() Principal { return s.p }

// Get returns record id as the principal sees it: ErrNotFound if it's absent,
// unpublished/trashed, or outside their read rule; fields they may not read are
// removed.
func (s ScopedStore) Get(ctx context.Context, collection, id string) (Record, error) {
	gw, err := s.app.gateway(ctx)
	if err != nil {
		return nil, err
	}
	return gw.As(s.p).Get(ctx, collection, id)
}

// List runs q confined to the principal's rows and the published view, with masked
// fields removed. Filtering or sorting on a field they may not read is a
// *ValidationError ("unknown field"), as over HTTP; subquery filters are refused.
func (s ScopedStore) List(ctx context.Context, q Query) (Page, error) {
	gw, err := s.app.gateway(ctx)
	if err != nil {
		return Page{}, err
	}
	return gw.As(s.p).List(ctx, q)
}

// Create creates a record as the principal: create rule, write-field rules,
// validation, hooks, events and revisions. data is not modified.
func (s ScopedStore) Create(ctx context.Context, collection string, data Record) (Record, error) {
	gw, err := s.app.gateway(ctx)
	if err != nil {
		return nil, err
	}
	return gw.As(s.p).Create(ctx, collection, data)
}

// Update applies a partial update to record id as the principal. data is not
// modified. Pass IfVersion for optimistic concurrency.
func (s ScopedStore) Update(ctx context.Context, collection, id string, data Record, opts ...WriteOption) (Record, error) {
	gw, err := s.app.gateway(ctx)
	if err != nil {
		return nil, err
	}
	o := writeOptions(opts)
	return gw.As(s.p).Update(ctx, collection, id, data, o.expect)
}

// Delete deletes record id as the principal. On a soft-delete collection it is
// trashed unless Purge is passed. Pass IfVersion for optimistic concurrency.
func (s ScopedStore) Delete(ctx context.Context, collection, id string, opts ...WriteOption) error {
	gw, err := s.app.gateway(ctx)
	if err != nil {
		return err
	}
	o := writeOptions(opts)
	return gw.As(s.p).Delete(ctx, collection, id, o.expect, o.purge)
}

// WriteOption adjusts an Update or Delete.
type WriteOption func(*writeOpts)

type writeOpts struct {
	expect *int64
	purge  bool
}

func writeOptions(opts []WriteOption) writeOpts {
	var o writeOpts
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// IfVersion makes the write conditional on the record still being at version v
// (its `_version`); otherwise ErrVersionConflict. Needs `concurrency: true`.
func IfVersion(v int64) WriteOption { return func(o *writeOpts) { o.expect = &v } }

// Purge hard-deletes a record on a soft-delete collection instead of trashing it.
func Purge() WriteOption { return func(o *writeOpts) { o.purge = true } }

// Media is in-process access to the media library's derived files and signed URLs
// (ADR-0037/0038). Get it from App.Media.
type Media struct{ app *App }

// PutFile writes a derived file (an HLS segment, a thumbnail) under asset assetID
// as p — the PUT /__media/{id}/files/{path} pipeline: update authorization on the
// asset, extension allowlist, size cap, and the owner's storage quota. Returns the
// stored size.
func (m Media) PutFile(ctx context.Context, p Principal, assetID, path string, r io.Reader) (int64, error) {
	gw, err := m.app.gateway(ctx)
	if err != nil {
		return 0, err
	}
	return gw.PutFile(ctx, p, assetID, path, r)
}

// SignedURLAs mints a signed, credential-free URL for assetID after checking p may
// read the asset — the POST /__media/{id}/sign checks. scope ("" or a prefix ending
// in "/") is what the token covers; path is the file the URL points at. The URL is
// path-relative: prefix your public origin.
func (m Media) SignedURLAs(ctx context.Context, p Principal, assetID, scope, path string, ttl time.Duration) (string, error) {
	gw, err := m.app.gateway(ctx)
	if err != nil {
		return "", err
	}
	return gw.SignedURLAs(ctx, p, assetID, scope, path, ttl)
}

// SignedURL mints a signed URL with no access check, for system work such as a job
// emailing a certificate link. scope and path are still validated. Prefer
// SignedURLAs whenever there's a user to check.
func (m Media) SignedURL(ctx context.Context, assetID, scope, path string, ttl time.Duration) (string, error) {
	gw, err := m.app.gateway(ctx)
	if err != nil {
		return "", err
	}
	return gw.SignedURL(assetID, scope, path, ttl)
}
