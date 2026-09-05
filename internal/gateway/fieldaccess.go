package gateway

import (
	"context"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Field-level access (ADR-0016 milestone 2). Unlike collection rules, which gate
// whole operations, field rules shape a record that the caller is already allowed
// to touch:
//
//   - read  → *mask*: an unauthorized reader still gets the record, minus the field.
//   - write → *filter*: an unauthorized writer's value for the field is dropped,
//     not rejected, so round-tripping a masked record never 4xxs on a field the
//     client never saw.
//
// Both are enforced only when auth is enabled and only for collections that
// actually declare a field rule (HasFieldAccess), so the common path pays nothing.

// maskReadFields removes fields the principal may not read from an outgoing
// record. `owner`-scoped field rules compare the record's created_by; on a record
// with no owner column the owner check fails closed (field masked).
func (s *Server) maskReadFields(ctx context.Context, collection string, rec store.Record) {
	if !s.authEnabled() {
		return
	}
	cd, ok := s.collections[collection]
	if !ok || !cd.HasFieldAccess() {
		return
	}
	p := principalFromContext(ctx)
	for _, f := range cd.Fields {
		if f.Access == nil || f.Access.Read == nil {
			continue
		}
		if !s.fieldPermitted(*f.Access.Read, p, rec) {
			delete(rec, f.Name)
		}
	}
}

// stripUnwritableFields drops fields the principal may not write from an inbound
// body, before validation and persistence. Dropping (not rejecting) means a
// client that PATCHes back a record it read does not fail on a field it was never
// allowed to see or set; the stored value is simply left untouched.
//
// On create there is no prior record (isCreate), so `owner` write rules resolve
// as authenticated (you are about to become the owner). On update, an `owner`
// write rule needs the record's created_by, so the current row is loaded lazily —
// once, and only when such a field is actually present in the body.
func (s *Server) stripUnwritableFields(ctx context.Context, collection, id string, data store.Record, isCreate bool) {
	if !s.authEnabled() {
		return
	}
	cd, ok := s.collections[collection]
	if !ok || !cd.HasFieldAccess() {
		return
	}
	p := principalFromContext(ctx)

	var current store.Record
	currentLoaded := isCreate // creates never load; treat as "resolved, nil"
	loadCurrent := func() store.Record {
		if !currentLoaded {
			currentLoaded = true
			if rec, err := s.db.FindOne(ctx, collection, id); err == nil {
				current = rec
			}
		}
		return current
	}

	for _, f := range cd.Fields {
		if f.Access == nil || f.Access.Write == nil {
			continue
		}
		if _, present := data[f.Name]; !present {
			continue
		}
		rule := *f.Access.Write
		var cur store.Record
		// Load the stored row only when an owner comparison can matter — a bare
		// `owner` rule or a composite that includes one (e.g. any: [admin, owner]).
		if rule.MentionsOwner() && !isCreate {
			cur = loadCurrent()
		}
		if !s.fieldWritable(rule, p, cur) {
			delete(data, f.Name)
		}
	}
}

// fieldPermitted evaluates a field read rule against a specific record.
func (s *Server) fieldPermitted(rule schema.Rule, p principal, rec store.Record) bool {
	switch d, field := evalRule(rule, p); d {
	case allow:
		return true
	case ownerScope:
		owner, _ := rec[field].(string)
		return owner != "" && owner == p.ID
	default:
		return false
	}
}

// fieldWritable evaluates a field write rule. `current` is the pre-loaded record
// for an update (owner comparison), or nil for a create — where `owner` collapses
// to authenticated, mirroring authorizeCreate.
func (s *Server) fieldWritable(rule schema.Rule, p principal, current store.Record) bool {
	switch d, field := evalRule(rule, p); d {
	case allow:
		return true
	case ownerScope:
		if current == nil {
			return p.Authenticated
		}
		owner, _ := current[field].(string)
		return owner != "" && owner == p.ID
	default:
		return false
	}
}
