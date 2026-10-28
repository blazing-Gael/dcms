package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Transport-free core (ADR-0038). Authorization and the CRUD/media operations take
// a context carrying the principal and return values or typed errors; the HTTP
// handlers parse, call the core, and map the error with writeStoreError. The Go
// embedding API calls the same functions, so the two can't diverge.

var (
	// ErrForbidden: an authenticated caller may not perform the action (HTTP 403).
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthenticated: an anonymous caller may not perform the action (HTTP 401).
	ErrUnauthenticated = errors.New("authentication required")
	// ErrReferenced: a hard delete refused because other records still reference
	// this one (on_delete: restrict, ADR-0010) (HTTP 409).
	ErrReferenced = errors.New("record is still referenced by other records")
	// ErrVersionConflict: an optimistic-concurrency precondition failed (HTTP 412).
	ErrVersionConflict = errVersionConflict
)

// Media outcomes (ADR-0037/0038). Go callers test them with errors.Is; over HTTP a
// MediaError carries its own status and message.
var (
	ErrMediaUnavailable     = errors.New("media storage is not configured")
	ErrSigningDisabled      = errors.New("media URL signing is not configured")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	ErrTooLarge             = errors.New("upload too large")
	ErrQuotaExceeded        = errors.New("storage quota exceeded")
)

// MediaError is a media failure with the HTTP status and message it renders as.
// errors.Is matches its kind (one of the Err… values above, or store.ErrInvalidInput).
type MediaError struct {
	Status        int
	Code, Message string
	kind          error
}

func (e *MediaError) Error() string        { return e.Message }
func (e *MediaError) Is(target error) bool { return target == e.kind }

var (
	errMediaUnavailable = &MediaError{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE",
		Message: "media storage is not configured", kind: ErrMediaUnavailable}
	errSigningDisabled = &MediaError{Status: http.StatusNotImplemented, Code: "NOT_IMPLEMENTED",
		Message: "media URL signing is not configured", kind: ErrSigningDisabled}
	errInvalidFilePath = &MediaError{Status: http.StatusUnprocessableEntity, Code: "VALIDATION_ERROR",
		Message: "invalid file path", kind: store.ErrInvalidInput}
)

// denyErr is the denial for p: 401 for an anonymous caller (so a client knows to
// authenticate), 403 otherwise.
func denyErr(p principal) error {
	if !p.Authenticated {
		return ErrUnauthenticated
	}
	return ErrForbidden
}

// authzCreate authorizes a create. On create there is no record yet, so a scoped
// rule (owner / owner_field / exists) collapses to "authenticated" — every scope
// leaf already requires a principal, so `scope` implies authenticated. You become
// the owner of what you create; membership is enforced on later reads/writes.
func (s *Server) authzCreate(ctx context.Context, collection string) error {
	if !s.authEnabled() {
		return nil
	}
	p := principalFromContext(ctx)
	switch d, _ := evalRule(s.collections[collection].AccessRule(schema.ActionCreate), p); d {
	case allow, scope:
		return nil
	default:
		return denyErr(p)
	}
}

// readScope authorizes a list read and returns the filters that narrow it to the
// caller's rows (nil when the caller may read every row). A denied read is an error
// (a list endpoint's existence is not a secret, so it is 401/403, not 404).
func (s *Server) readScope(ctx context.Context, collection string) ([]store.Filter, error) {
	if !s.authEnabled() {
		return nil, nil
	}
	p := principalFromContext(ctx)
	switch d, filters := evalRule(s.collections[collection].AccessRule(schema.ActionRead), p); d {
	case allow:
		return nil, nil
	case scope:
		return filters, nil
	default:
		return nil, denyErr(p)
	}
}

// authzWriteRule authorizes a write on one record against rule. A scoped rule loads
// the record so a genuinely missing one is store.ErrNotFound, then tests it: the
// record exists but the caller is not in scope → denied (the 404-vs-403 split).
func (s *Server) authzWriteRule(ctx context.Context, collection, id string, rule schema.Rule) error {
	if !s.authEnabled() {
		return nil
	}
	p := principalFromContext(ctx)
	switch d, filters := evalRule(rule, p); d {
	case allow:
		return nil
	case scope:
		rec, err := s.db.FindOne(ctx, collection, id)
		if err != nil {
			return err
		}
		if s.recordInScope(ctx, collection, rec, filters) {
			return nil
		}
		return denyErr(p)
	default:
		return denyErr(p)
	}
}

// authzRecordWrite authorizes an update or delete of one record: a record the
// caller may not preview does not exist for them (ADR-0023) — not-found before the
// rule's 403, so write agrees with get-one — then the action's rule.
func (s *Server) authzRecordWrite(ctx context.Context, collection, id string, action schema.AccessAction) error {
	if s.writeHidden(ctx, collection, id) {
		return store.ErrNotFound
	}
	return s.authzWriteRule(ctx, collection, id, s.collections[collection].AccessRule(action))
}

// ── CRUD core ────────────────────────────────────────────────────────────────
//
// The write functions assume the caller already authorized the action (authzCreate
// / authzRecordWrite): the HTTP handlers authorize before decoding the body, so a
// denied caller gets 401/403 rather than a body error.

// getCore loads one record as the caller sees it: absent, hidden by lifecycle and
// preview (ADR-0012/0023), or outside the read rule (ADR-0016) are all
// store.ErrNotFound — never forbidden, so neither existence nor an owner boundary
// leaks. The record is returned unshaped (see present).
func (s *Server) getCore(ctx context.Context, collection, id string) (store.Record, error) {
	rec, err := s.db.FindOne(ctx, collection, id)
	if err != nil {
		return nil, err
	}
	if !s.recordPreviewVisible(ctx, collection, rec) || !s.recordReadable(ctx, collection, rec) {
		return nil, store.ErrNotFound
	}
	return rec, nil
}

// listCore runs a list query as the caller: the read scope and the lifecycle
// filters for the caller's view are ANDed onto q's own filters. Records come back
// unshaped.
func (s *Server) listCore(ctx context.Context, q store.Query) (store.Page, error) {
	scoped, err := s.readScope(ctx, q.Collection)
	if err != nil {
		return store.Page{}, err
	}
	filters := slices.Clone(q.Filters)
	filters = append(filters, scoped...)
	// Hide non-live / trashed records unless the caller is preview-eligible for the
	// requested view (ADR-0012/0023) — includes any preview owner-scoping filter.
	q.Filters = append(filters, s.lifecycleFiltersFor(ctx, q.Collection)...)
	return s.db.Find(ctx, q)
}

// createCore creates a record from a decoded body: managed and unwritable fields
// dropped (a forbidden value-scoped transition is an error), then either the inline
// related-object tree in one transaction or validation, decimal encoding, reference
// checks and the write with hooks/events/revisions/m2m links.
func (s *Server) createCore(ctx context.Context, collection string, data store.Record) (store.Record, error) {
	stripManagedFields(data) // _status/_published_at/_deleted_at change only via transitions
	// isCreate: `owner` write rules resolve as authenticated (you become the owner of
	// what you create).
	if err := s.stripUnwritableFields(ctx, collection, "", data, true); err != nil {
		return nil, err
	}

	// Inline related objects → create the whole tree transactionally.
	if s.hasInlineRelations(collection, data) {
		var rec store.Record
		err := s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
			var e error
			if data, e = s.beforeWrite(ctx, tx, collection, BeforeCreate, data); e != nil {
				return e
			}
			if rec, e = s.createRecord(ctx, tx, collection, data, 0); e != nil {
				return e
			}
			if e = s.captureWrite(ctx, tx, collection, rec, "create"); e != nil {
				return e
			}
			return s.afterWrite(ctx, tx, collection, AfterCreate, rec)
		})
		return rec, err
	}

	if errs := s.collections[collection].ValidateCreate(data); errs != nil {
		return nil, &store.ValidationError{Fields: errs}
	}
	// Decimal strings → int64 minor units before the store write (ADR-0017).
	s.collections[collection].EncodeDecimals(data)
	if err := s.checkReferences(ctx, s.db, collection, data); err != nil {
		return nil, err
	}
	return s.writeWithLinks(ctx, collection, "create", data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
		return db.Create(ctx, store.WriteInput{Collection: collection, Data: base})
	})
}

// updateCore applies a partial update to record id. expect is the optimistic-
// concurrency precondition (nil = none, issue #26).
func (s *Server) updateCore(ctx context.Context, collection, id string, data store.Record, expect *int64) (store.Record, error) {
	stripManagedFields(data)
	data["id"] = id // the id is the target, never taken from the body
	// On update an `owner` write rule is checked against the stored record, loaded
	// lazily.
	if err := s.stripUnwritableFields(ctx, collection, id, data, false); err != nil {
		return nil, err
	}

	// Inline related objects → resolve + update transactionally.
	if s.hasInlineRelations(collection, data) {
		var rec store.Record
		err := s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
			if e := s.applyVersion(ctx, tx, collection, data, expect); e != nil {
				return e
			}
			var e error
			if data, e = s.beforeWrite(ctx, tx, collection, BeforeUpdate, data); e != nil {
				return e
			}
			if rec, e = s.updateRecord(ctx, tx, collection, data, 0); e != nil {
				return e
			}
			if s.revised(collection) {
				if e = s.captureRevision(ctx, tx, collection, rec, "update"); e != nil {
					return e
				}
			}
			return s.afterWrite(ctx, tx, collection, AfterUpdate, rec)
		})
		return rec, err
	}

	if errs := s.collections[collection].ValidateUpdate(data); errs != nil {
		return nil, &store.ValidationError{Fields: errs}
	}
	s.collections[collection].EncodeDecimals(data)
	if err := s.checkReferences(ctx, s.db, collection, data); err != nil {
		return nil, err
	}
	return s.writeWithLinks(ctx, collection, "update", data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
		if e := s.applyVersion(ctx, db, collection, base, expect); e != nil {
			return nil, e
		}
		return db.Update(ctx, store.WriteInput{Collection: collection, Data: base})
	})
}

// deleteCore deletes record id. A soft-delete collection trashes it (reversible
// via /restore) unless purge; a hard delete honours on_delete: restrict, returning
// ErrReferenced while the record is still referenced (ADR-0010/0012).
func (s *Server) deleteCore(ctx context.Context, collection, id string, expect *int64, purge bool) error {
	if s.collections[collection].SoftDelete && !purge {
		_, err := s.updateAndRevise(ctx, collection,
			store.Record{"id": id, schema.LifecycleDeletedAt: nowUTC()}, "delete", expect)
		return err
	}
	if err := s.deleteRecord(ctx, collection, id, expect); err != nil {
		if errors.Is(err, store.ErrIntegrity) {
			return ErrReferenced
		}
		return err
	}
	return nil
}

// present shapes a record for a caller outside HTTP: declared types, secrets
// redacted, and fields the caller may not read masked — what an HTTP response
// carries (writeRecord does the same plus response validation and expansion).
func (s *Server) present(ctx context.Context, collection string, rec store.Record) store.Record {
	s.collections[collection].CoerceResponse(rec)
	redactSecrets(collection, rec)
	s.maskReadFields(ctx, collection, rec)
	return rec
}

// authzPublish authorizes a publishing transition: hidden ⇒ not found, then the
// `publish` rule if declared, else update (issue #23).
func (s *Server) authzPublish(ctx context.Context, collection, id string) error {
	if s.writeHidden(ctx, collection, id) {
		return store.ErrNotFound
	}
	rule := s.collections[collection].AccessRule(schema.ActionUpdate)
	if pr, ok := s.collections[collection].PublishRule(); ok {
		rule = pr
	}
	return s.authzWriteRule(ctx, collection, id, rule)
}
