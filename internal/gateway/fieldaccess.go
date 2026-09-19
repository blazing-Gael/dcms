package gateway

import (
	"context"
	"errors"
	"net/http"

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
// A disallowed value-scoped transition (issue #27) is a loud 403 naming the field,
// not the silent drop used for a plain unwritable field — a submit that quietly
// didn't happen is worse than an error.
type fieldForbiddenError struct{ field string }

func (e *fieldForbiddenError) Error() string {
	return "field " + e.field + ": transition not permitted"
}

// writeFieldWriteError renders a field-write error, or reports false if err is nil.
// A forbidden transition (issue #27) is a 403 naming the field; any other error is
// an internal fault. Callers use it to short-circuit a write.
func (s *Server) writeFieldWriteError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var fe *fieldForbiddenError
	if errors.As(err, &fe) {
		writeError(w, http.StatusForbidden, apiError{
			Code:    "FORBIDDEN",
			Message: "you may not set '" + fe.field + "' to that value",
			Fields:  map[string]string{fe.field: "transition not permitted"},
		})
		return true
	}
	writeStoreError(w, s.logger, r, err)
	return true
}

func (s *Server) stripUnwritableFields(ctx context.Context, collection, id string, data store.Record, isCreate bool) error {
	if !s.authEnabled() {
		return nil
	}
	cd, ok := s.collections[collection]
	if !ok || !cd.HasFieldAccess() {
		return nil
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
		if f.Access == nil {
			continue
		}
		if _, present := data[f.Name]; !present {
			continue
		}

		// Value-scoped write rules (issue #27): permit / 403 / silent-drop by the
		// attempted transition, not just by identity.
		if len(f.Access.WriteTransitions) > 0 {
			newVal, ok := data[f.Name].(string)
			if !ok {
				continue // non-string value; validation will reject it as a 422
			}
			cur := loadCurrent() // nil on create → current value ""
			curVal, _ := cur[f.Name].(string)
			switch s.evalTransition(f.Access.WriteTransitions, p, cur, curVal, newVal, isCreate) {
			case transitionAllowed:
				// keep it
			case transitionForbidden:
				return &fieldForbiddenError{field: f.Name}
			case transitionDropped:
				delete(data, f.Name)
			}
			continue
		}

		if f.Access.Write == nil {
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
	return nil
}

// transitionOutcome is the decision for a value-scoped write.
type transitionOutcome int

const (
	transitionAllowed   transitionOutcome = iota // the write proceeds
	transitionForbidden                          // a rule applies but forbids this transition → 403
	transitionDropped                            // no rule applies to the caller → drop silently
)

// evalTransition decides a value-scoped write. A rule "applies" when its `who` is
// satisfied for the caller; among applying rules the transition is permitted when
// the current value is in `from` (ignored on create) and the new value in `to` (an
// empty set means "any"). A no-op (new == current, on update) is always allowed so
// a round-tripped record never trips.
func (s *Server) evalTransition(rules []schema.TransitionRule, p principal, current store.Record, curVal, newVal string, isCreate bool) transitionOutcome {
	if !isCreate && newVal == curVal {
		return transitionAllowed
	}
	applied := false
	for _, tr := range rules {
		if !s.fieldWritable(tr.Who, p, current) {
			continue
		}
		applied = true
		if !inSetOrAny(tr.To, newVal) {
			continue
		}
		if isCreate || inSetOrAny(tr.From, curVal) {
			return transitionAllowed
		}
	}
	if applied {
		return transitionForbidden
	}
	return transitionDropped
}

// inSetOrAny reports whether v is in set, or the set is empty ("any value").
func inSetOrAny(set []string, v string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
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
