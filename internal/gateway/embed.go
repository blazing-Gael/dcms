package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/pkg/auth"
)

// Embedding API (ADR-0038): what package dcms exposes to custom routes, hooks and
// other in-process Go code. Every method runs the same core as the HTTP API, as the
// given principal, so the two can't disagree.

// ErrUnknownCollection: the collection isn't reachable through the API (unknown, or
// an engine collection like _users — media goes through the media methods).
var ErrUnknownCollection = errors.New("unknown collection")

// asContext returns ctx acting as p: p is the principal (and the actor stamped on
// writes, as withPrincipal does), and the lifecycle view is reset to the default,
// published-only one — a preview token or ?status on an enclosing HTTP request must
// not widen what the Go API sees.
func asContext(ctx context.Context, p principal) context.Context {
	ctx = auth.NewContext(ctx, p)
	if p.Authenticated {
		ctx = store.WithActor(ctx, p.ID)
	}
	return context.WithValue(ctx, visibilityKey, visibility{})
}

func (s *Server) checkCollection(collection string) error {
	if !s.routableCollection(collection) {
		return fmt.Errorf("%w %q", ErrUnknownCollection, collection)
	}
	return nil
}

// canResult maps a core error to Can's answer: allowed, not allowed (denied, absent
// or hidden — the 404 policy), or a real failure.
func canResult(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, ErrForbidden), errors.Is(err, ErrUnauthenticated):
		return false, nil
	default:
		return false, err
	}
}

// Can reports whether p may perform action on record id of collection (id is
// ignored for create). False covers both "not allowed" and "not found / not
// visible"; err is reserved for real failures.
func (s *Server) Can(ctx context.Context, p principal, action schema.AccessAction, collection, id string) (bool, error) {
	if err := s.checkCollection(collection); err != nil {
		return false, err
	}
	ctx = asContext(ctx, p)
	cd := s.collections[collection]
	var err error
	switch action {
	case schema.ActionRead:
		_, err = s.getCore(ctx, collection, id)
	case schema.ActionCreate:
		err = s.authzCreate(ctx, collection)
	case schema.ActionUpdate, schema.ActionDelete:
		err = s.authzRecordWrite(ctx, collection, id, action)
	case schema.ActionPublish:
		if !cd.Publishing {
			return false, nil
		}
		err = s.authzPublish(ctx, collection, id)
	case schema.ActionHistory:
		if !s.revised(collection) {
			return false, nil
		}
		return s.historyReadable(ctx, collection, id), nil
	default:
		return false, fmt.Errorf("dcms: Can: unsupported action %q", action)
	}
	if err == nil && action != schema.ActionRead && action != schema.ActionCreate {
		// A rule that allows outright doesn't load the record; "may p update id" is
		// false for a record that doesn't exist.
		_, err = s.db.FindOne(ctx, collection, id)
	}
	return canResult(err)
}

// ReadFilters returns the filters that confine a query on collection to what p may
// read — the read scope plus the default lifecycle view — for AND-ing onto your
// own query. ok is false when p may read nothing.
func (s *Server) ReadFilters(ctx context.Context, p principal, collection string) (filters []store.Filter, ok bool, err error) {
	if err := s.checkCollection(collection); err != nil {
		return nil, false, err
	}
	ctx = asContext(ctx, p)
	scoped, err := s.readScope(ctx, collection)
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthenticated) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return append(scoped, s.lifecycleFiltersFor(ctx, collection)...), true, nil
}

// ScopedStore reads and writes as one principal, enforcing everything the HTTP API
// does for them (ADR-0038). Errors: store.ErrNotFound (absent or hidden),
// ErrForbidden / ErrUnauthenticated (denied), *store.ValidationError,
// ErrVersionConflict, ErrReferenced, *HookError.
type ScopedStore struct {
	s *Server
	p principal
}

// As returns a store acting as p.
func (s *Server) As(p principal) *ScopedStore { return &ScopedStore{s: s, p: p} }

// Get returns record id as p sees it.
func (ss *ScopedStore) Get(ctx context.Context, collection, id string) (store.Record, error) {
	if err := ss.s.checkCollection(collection); err != nil {
		return nil, err
	}
	ctx = asContext(ctx, ss.p)
	rec, err := ss.s.getCore(ctx, collection, id)
	if err != nil {
		return nil, err
	}
	return ss.s.present(ctx, collection, rec), nil
}

// List runs q as p: confined to p's rows and the published view, with fields p may
// not read masked. Filtering or sorting on such a field is rejected, as over HTTP,
// and so are subquery filters (they could probe another collection past its rules).
func (ss *ScopedStore) List(ctx context.Context, q store.Query) (store.Page, error) {
	if err := ss.s.checkCollection(q.Collection); err != nil {
		return store.Page{}, err
	}
	ctx = asContext(ctx, ss.p)
	if err := ss.s.guardQuery(ctx, &q); err != nil {
		return store.Page{}, err
	}
	page, err := ss.s.listCore(ctx, q)
	if err != nil {
		return store.Page{}, err
	}
	for _, rec := range page.Data {
		ss.s.present(ctx, q.Collection, rec)
	}
	return page, nil
}

// Create creates a record as p: create rule, write-field rules, validation, hooks,
// events, revisions — the HTTP create pipeline.
func (ss *ScopedStore) Create(ctx context.Context, collection string, data store.Record) (store.Record, error) {
	if err := ss.s.checkCollection(collection); err != nil {
		return nil, err
	}
	ctx = asContext(ctx, ss.p)
	if err := ss.s.authzCreate(ctx, collection); err != nil {
		return nil, err
	}
	rec, err := ss.s.createCore(ctx, collection, maps(data))
	if err != nil {
		return nil, err
	}
	return ss.s.present(ctx, collection, rec), nil
}

// Update applies a partial update to record id as p. expect, if non-nil, is the
// version the caller read (optimistic concurrency); it requires `concurrency: true`.
func (ss *ScopedStore) Update(ctx context.Context, collection, id string, data store.Record, expect *int64) (store.Record, error) {
	if err := ss.s.checkCollection(collection); err != nil {
		return nil, err
	}
	if err := ss.s.checkExpect(collection, expect); err != nil {
		return nil, err
	}
	ctx = asContext(ctx, ss.p)
	if err := ss.s.authzRecordWrite(ctx, collection, id, schema.ActionUpdate); err != nil {
		return nil, err
	}
	rec, err := ss.s.updateCore(ctx, collection, id, maps(data), expect)
	if err != nil {
		return nil, err
	}
	return ss.s.present(ctx, collection, rec), nil
}

// Delete deletes record id as p: trashed on a soft-delete collection unless purge,
// else a hard delete (ErrReferenced while still referenced).
func (ss *ScopedStore) Delete(ctx context.Context, collection, id string, expect *int64, purge bool) error {
	if err := ss.s.checkCollection(collection); err != nil {
		return err
	}
	if err := ss.s.checkExpect(collection, expect); err != nil {
		return err
	}
	ctx = asContext(ctx, ss.p)
	if err := ss.s.authzRecordWrite(ctx, collection, id, schema.ActionDelete); err != nil {
		return err
	}
	return ss.s.deleteCore(ctx, collection, id, expect, purge)
}

// checkExpect refuses a version precondition on a collection without
// `concurrency`, as the HTTP API does for If-Match — never silently ignored.
func (s *Server) checkExpect(collection string, expect *int64) error {
	if expect != nil && !s.versioned(collection) {
		return fmt.Errorf("%w: a version precondition needs `concurrency: true` on %q", store.ErrInvalidInput, collection)
	}
	return nil
}

// maps copies a caller's record so the core's in-place edits (managed fields
// stripped, id stamped, decimals encoded) don't leak back into it.
func maps(data store.Record) store.Record {
	out := make(store.Record, len(data))
	for k, v := range data {
		out[k] = v
	}
	return out
}

// guardQuery applies the HTTP list query's field rules to a Go-built query
// (GHSA-x4q6-jjx6-rwxm): every filter and the sort must name a column; a field p
// may not read is "unknown"; a record-scoped field rule ANDs its scope in; sort
// needs an unconditional read. Subquery values are refused outright.
func (s *Server) guardQuery(ctx context.Context, q *store.Query) error {
	p := principalFromContext(ctx)
	unknown := func(field string) error {
		return &store.ValidationError{Fields: map[string]string{field: fmt.Sprintf("unknown field %q", field)}}
	}
	var scopes []store.Filter
	seen := map[string]bool{}
	var walk func([]store.Filter) error
	walk = func(fs []store.Filter) error {
		for _, f := range fs {
			if f.Operator == store.And || f.Operator == store.Or {
				children, _ := f.Value.([]store.Filter)
				if err := walk(children); err != nil {
					return err
				}
				continue
			}
			switch f.Value.(type) {
			case store.Subquery, *store.Subquery:
				return fmt.Errorf("%w: subquery filters are not allowed in a scoped query", store.ErrInvalidInput)
			}
			if !s.hasColumn(q.Collection, f.Field) {
				return unknown(f.Field)
			}
			ok, scoped := s.fieldQueryAccess(q.Collection, f.Field, p)
			if !ok {
				return unknown(f.Field)
			}
			if scoped != nil && !seen[f.Field] {
				seen[f.Field] = true
				scopes = append(scopes, scoped...)
			}
		}
		return nil
	}
	if err := walk(q.Filters); err != nil {
		return err
	}
	if q.Sort != "" {
		field := strings.TrimPrefix(q.Sort, "-")
		if !s.hasColumn(q.Collection, field) {
			return unknown(field)
		}
		if ok, scoped := s.fieldQueryAccess(q.Collection, field, p); !ok || scoped != nil {
			return unknown(field)
		}
	}
	q.Filters = append(slices.Clone(q.Filters), scopes...)
	return nil
}

// PutFile writes a derived file under media asset id as p — the PUT
// /__media/{id}/files/{path} pipeline: update authorization, extension allowlist,
// size cap, owner quota. Returns the stored size.
func (s *Server) PutFile(ctx context.Context, p principal, id, filePath string, r io.Reader) (int64, error) {
	return s.putDerivedFile(asContext(ctx, p), id, filePath, r)
}

// SignedURLAs mints a signed URL for asset id after the same read check and
// validation as POST /__media/{id}/sign. The URL is path-relative.
func (s *Server) SignedURLAs(ctx context.Context, p principal, id, scope, filePath string, ttl time.Duration) (string, error) {
	if err := s.authzSign(asContext(ctx, p), id); err != nil {
		return "", err
	}
	return s.signURL(id, scope, filePath, ttl)
}

// SignedURL mints a signed URL with no access check — for system work such as a
// job emailing a certificate link. Scope and path are still validated.
func (s *Server) SignedURL(id, scope, filePath string, ttl time.Duration) (string, error) {
	if !s.mediaEnabled() {
		return "", errMediaUnavailable
	}
	return s.signURL(id, scope, filePath, ttl)
}
