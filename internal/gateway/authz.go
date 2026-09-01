package gateway

import (
	"context"
	"net/http"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// createdByField is the audit column that records who created a record; `owner`
// access rules (ADR-0016) compare the principal against it.
const createdByField = "created_by"

// decision is the outcome of evaluating a collection-scoped access rule that does
// not yet know a specific record.
type decision int

const (
	allow      decision = iota // the principal satisfies the rule outright
	deny                       // the principal does not satisfy the rule
	ownerScope                 // an `owner` rule: the caller is authenticated but
	// the check must be narrowed to records they created
)

// evalRule evaluates a rule at collection scope. `owner` returns ownerScope
// (unless the caller is anonymous, which can never own anything → deny); the
// record-level owner comparison is done by the caller, which alone has the record.
func evalRule(rule schema.Rule, p principal) decision {
	switch rule.Kind {
	case schema.RulePublic:
		return allow
	case schema.RuleAuthenticated:
		if p.authenticated {
			return allow
		}
		return deny
	case schema.RuleRoles:
		for _, role := range rule.Roles {
			if p.hasRole(role) {
				return allow
			}
		}
		return deny
	case schema.RuleOwner:
		if !p.authenticated {
			return deny
		}
		return ownerScope
	case schema.RuleAny:
		// Logical OR over sub-rules. Combine their decisions by precedence:
		// an outright allow (e.g. an admin) beats owner-scope (narrow to own
		// rows) beats deny. This lets `any: [admin, owner]` give admins full
		// access while everyone else stays owner-scoped — with no new cases in
		// the enforcement helpers, which already handle allow/ownerScope/deny.
		result := deny
		for _, sub := range rule.Any {
			switch evalRule(sub, p) {
			case allow:
				return allow
			case ownerScope:
				result = ownerScope
			}
		}
		return result
	default:
		return deny
	}
}

// denyWrite writes the 403 for a forbidden write (create/update/delete/transition).
// The route exists; the caller may not act on it (ADR-0016). An anonymous caller
// gets 401 so a client knows to authenticate rather than give up.
func (s *Server) denyWrite(w http.ResponseWriter, p principal) {
	if !p.authenticated {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	writeError(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: "you do not have permission to perform this action"})
}

// authorizeCreate authorizes a create. `owner` collapses to "authenticated" — you
// become the owner of what you create. Returns false (and writes the error) when
// denied.
func (s *Server) authorizeCreate(w http.ResponseWriter, r *http.Request, collection string) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	rule := s.collections[collection].AccessRule(schema.ActionCreate)
	switch evalRule(rule, p) {
	case allow, ownerScope:
		return true
	default:
		s.denyWrite(w, p)
		return false
	}
}

// listReadFilters authorizes a list read and returns any owner-scoping filters to
// append to the query. A denied role-gated read is a 403 (unlike a single read,
// which 404s — a list endpoint's existence is not a secret). `owner` narrows the
// query to the caller's own rows rather than forbidding the endpoint.
func (s *Server) listReadFilters(w http.ResponseWriter, r *http.Request, collection string) ([]store.Filter, bool) {
	if !s.authEnabled() {
		return nil, true
	}
	p := principalFromContext(r.Context())
	rule := s.collections[collection].AccessRule(schema.ActionRead)
	switch evalRule(rule, p) {
	case allow:
		return nil, true
	case ownerScope:
		return []store.Filter{{Field: createdByField, Operator: store.Eq, Value: p.id}}, true
	default:
		s.denyWrite(w, p)
		return nil, false
	}
}

// recordReadable reports whether a fetched record passes the read rule. For a
// role/public/authenticated rule it is a collection-scope decision; for `owner`
// it compares created_by to the principal. The caller turns a false into a 404
// (never 403 — that would leak the record's existence, per ADR-0016/0012).
func (s *Server) recordReadable(ctx context.Context, collection string, rec store.Record) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(ctx)
	rule := s.collections[collection].AccessRule(schema.ActionRead)
	switch evalRule(rule, p) {
	case allow:
		return true
	case ownerScope:
		owner, _ := rec[createdByField].(string)
		return owner != "" && owner == p.id
	default:
		return false
	}
}

// authorizeCollectionRead reports whether the principal may read the collection
// at all. It is used by auxiliary read endpoints (e.g. revision history) where
// record-level owner scoping isn't applied — `owner` counts as permitted here,
// and the caller turns a false into a 404 so nothing leaks.
func (s *Server) authorizeCollectionRead(r *http.Request, collection string) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	return evalRule(s.collections[collection].AccessRule(schema.ActionRead), p) != deny
}

// authorizeRecordWrite authorizes an update/delete (and lifecycle transitions).
// For a collection-scope rule (public/authenticated/roles) no record is needed.
// For `owner` it loads the record and compares created_by; a missing record is a
// 404 and a non-owner is a 403. Returns false (and writes the error) when denied.
func (s *Server) authorizeRecordWrite(w http.ResponseWriter, r *http.Request, collection, id string, action schema.AccessAction) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	rule := s.collections[collection].AccessRule(action)
	switch evalRule(rule, p) {
	case allow:
		return true
	case ownerScope:
		rec, err := s.db.FindOne(r.Context(), collection, id)
		if err != nil {
			// Not found (or any load error) → let the handler's own path report it;
			// but for owner scope we must decide here. Treat not-found as 404.
			writeStoreError(w, s.logger, r, err)
			return false
		}
		owner, _ := rec[createdByField].(string)
		if owner != "" && owner == p.id {
			return true
		}
		s.denyWrite(w, p)
		return false
	default:
		s.denyWrite(w, p)
		return false
	}
}
