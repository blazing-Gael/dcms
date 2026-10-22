package gateway

import (
	"context"
	"net/http"
	"sort"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// createdByField is the audit column that records who created a record; `owner`
// access rules (ADR-0016) compare the principal against it.
const createdByField = "created_by"

// decision is the outcome of evaluating an access rule before a specific record is
// known. `scope` means the caller is admitted only to rows matching the returned
// filters (ADR-0016 owner scoping, generalized by ADR-0036 to any filter — an owner
// equality, a membership semi-join, or an And/Or of them).
type decision int

const (
	allow decision = iota // the principal satisfies the rule outright
	deny                  // the principal does not satisfy the rule
	scope                 // admitted only to rows matching the returned filters
)

// evalRule evaluates a rule. When it returns `scope` it also returns the store
// filters that narrow the caller to permitted rows — to append to a list query, or
// to test one record against (recordInScope / scopedExists). It is pure: filters are
// built from the principal and the rule, with no store access, so the same result
// drives list reads, single reads, and write authorization alike.
func evalRule(rule schema.Rule, p principal) (decision, []store.Filter) {
	switch rule.Kind {
	case schema.RulePublic:
		return allow, nil
	case schema.RuleAuthenticated:
		if p.Authenticated {
			return allow, nil
		}
		return deny, nil
	case schema.RuleRoles:
		for _, role := range rule.Roles {
			if p.HasRole(role) {
				return allow, nil
			}
		}
		return deny, nil
	case schema.RuleOwner:
		if !p.Authenticated {
			return deny, nil
		}
		return scope, []store.Filter{{Field: createdByField, Operator: store.Eq, Value: p.ID}}
	case schema.RuleInherit:
		// Baseline for `_media` inherit (issue #30): the uploader always sees their
		// own uploads. The reference-based widening — readable via a record that
		// points at the file — is layered on in the media byte path (mediaReadable).
		if !p.Authenticated {
			return deny, nil
		}
		return scope, []store.Filter{{Field: createdByField, Operator: store.Eq, Value: p.ID}}
	case schema.RuleOwnerField:
		if !p.Authenticated { // an anonymous caller can never match a relation to their identity
			return deny, nil
		}
		return scope, []store.Filter{{Field: rule.Field, Operator: store.Eq, Value: p.ID}}
	case schema.RuleExists:
		// Membership (ADR-0036): admitted to rows whose match column is in the set the
		// caller is a member of. Anonymous callers can never be a member.
		if !p.Authenticated || rule.Exists == nil {
			return deny, nil
		}
		return scope, []store.Filter{existsFilter(rule.Exists, p)}
	case schema.RuleAny:
		return combineAny(rule.Any, p)
	case schema.RuleAll:
		return combineAll(rule.All, p)
	default:
		return deny, nil
	}
}

// existsFilter compiles an `exists` spec against the caller into a semi-join filter:
// `<match_this> IN (SELECT <match_in> FROM <in> WHERE <principal> = caller AND where…)`.
// The subquery is uncorrelated — it references only the caller and literal `where`
// values — which is what keeps it one indexed lookup on every backend (ADR-0036).
func existsFilter(sp *schema.ExistsSpec, p principal) store.Filter {
	sub := store.Subquery{
		Collection: sp.In,
		Select:     sp.MatchIn,
		Filters:    []store.Filter{{Field: sp.Principal, Operator: store.Eq, Value: p.ID}},
	}
	for _, k := range sortedKeys(sp.Where) {
		sub.Filters = append(sub.Filters, store.Filter{Field: k, Operator: store.Eq, Value: sp.Where[k]})
	}
	return store.Filter{Field: sp.MatchThis, Operator: store.In, Value: sub}
}

// sortedKeys returns a map's keys in sorted order, so a rule compiles to the same
// SQL every time (deterministic for caching and tests).
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// combineAny ORs sub-rules (ADR-0036). An outright `allow` (e.g. an admin) wins — the
// caller needs no filter. Otherwise the scope branches are OR-combined into one Or
// node; each branch collapses to a single leaf or an And node. No scoped branch and
// no allow ⇒ deny.
func combineAny(subs []schema.Rule, p principal) (decision, []store.Filter) {
	var branches [][]store.Filter
	for _, sub := range subs {
		switch d, f := evalRule(sub, p); d {
		case allow:
			return allow, nil
		case scope:
			branches = append(branches, f)
		}
	}
	switch len(branches) {
	case 0:
		return deny, nil
	case 1:
		return scope, branches[0]
	default:
		children := make([]store.Filter, 0, len(branches))
		for _, b := range branches {
			children = append(children, andBranch(b))
		}
		return scope, []store.Filter{{Operator: store.Or, Value: children}}
	}
}

// combineAll ANDs sub-rules (ADR-0036). Any `deny` denies the whole. Each scoped
// sub-rule contributes its filters (ANDed at the top level, so they just concatenate);
// an `allow` sub-rule contributes nothing. No filters at all ⇒ every sub-rule allowed.
func combineAll(subs []schema.Rule, p principal) (decision, []store.Filter) {
	var filters []store.Filter
	for _, sub := range subs {
		switch d, f := evalRule(sub, p); d {
		case deny:
			return deny, nil
		case scope:
			filters = append(filters, f...)
		}
	}
	if len(filters) == 0 {
		return allow, nil
	}
	return scope, filters
}

// andBranch collapses an ANDed filter list into a single filter: a bare leaf when
// there is one, else an And node — so it can sit as one child of an Or.
func andBranch(b []store.Filter) store.Filter {
	if len(b) == 1 {
		return b[0]
	}
	return store.Filter{Operator: store.And, Value: b}
}

// recordInScope reports whether an already-loaded record satisfies scope filters.
// A scope built only from simple equality leaves (owner / owner_field) is checked in
// Go with no extra query; a richer scope (membership semi-join, or an And/Or) is
// checked with one indexed existence query by id.
func (s *Server) recordInScope(ctx context.Context, collection string, rec store.Record, filters []store.Filter) bool {
	if ok, decidable := matchSimpleFilters(rec, filters); decidable {
		return ok
	}
	id, _ := rec["id"].(string)
	if id == "" {
		return false
	}
	ok, _ := s.scopedExists(ctx, collection, id, filters)
	return ok
}

// matchSimpleFilters evaluates scope filters against a record without a query, but
// only when every filter is a plain string-equality leaf (the owner/owner_field
// case). decidable is false the moment a filter is anything richer (a Subquery or an
// And/Or node), so the caller falls back to a store query that can evaluate it.
func matchSimpleFilters(rec store.Record, filters []store.Filter) (ok, decidable bool) {
	for _, f := range filters {
		if f.Operator != store.Eq || f.Field == "" {
			return false, false
		}
		want, okW := f.Value.(string)
		if !okW {
			return false, false
		}
		if got, _ := rec[f.Field].(string); got == "" || got != want {
			return false, true
		}
	}
	return true, true
}

// scopedExists reports whether the record with this id also matches the scope
// filters — the uniform way to authorize one record against any scope, including a
// membership semi-join. It is a single, limit-1, index-friendly existence query.
func (s *Server) scopedExists(ctx context.Context, collection, id string, filters []store.Filter) (bool, error) {
	q := store.Query{
		Collection: collection,
		Filters:    append([]store.Filter{{Field: "id", Operator: store.Eq, Value: id}}, filters...),
		Limit:      1,
		SkipCount:  true,
	}
	page, err := s.db.Find(ctx, q)
	if err != nil {
		s.logger.Warn("scoped authorization query failed", "collection", collection, "err", err)
		return false, err
	}
	return len(page.Data) > 0, nil
}

// denyWrite writes the 403 for a forbidden write (create/update/delete/transition).
// The route exists; the caller may not act on it (ADR-0016). An anonymous caller
// gets 401 so a client knows to authenticate rather than give up.
func (s *Server) denyWrite(w http.ResponseWriter, p principal) {
	if !p.Authenticated {
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
	// On create there is no record yet, so a scoped rule (owner / owner_field /
	// exists) collapses to "authenticated" — every scope leaf already requires a
	// principal, so `scope` implies authenticated. You become the owner of what you
	// create; membership is enforced on later reads/writes.
	switch d, _ := evalRule(rule, p); d {
	case allow, scope:
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
	switch d, filters := evalRule(rule, p); d {
	case allow:
		return nil, true
	case scope:
		return filters, true
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
	switch d, filters := evalRule(rule, p); d {
	case allow:
		return true
	case scope:
		return s.recordInScope(ctx, collection, rec, filters)
	default:
		return false
	}
}

// authorizeRecordRead reports whether the principal may read a specific record,
// identified by id. It is the read counterpart of authorizeRecordWrite, for
// auxiliary read endpoints (e.g. revision history) that serve a record's derived
// data without having loaded the record itself. A collection-scope rule
// (public/authenticated/roles) decides outright; an `owner` rule loads the record
// to compare the owner column, so history is never a way around the boundary the
// record read enforces. The caller turns a false into a 404 — never a 403 — so
// neither the record's existence nor the owner boundary leaks.
func (s *Server) authorizeRecordRead(ctx context.Context, collection, id string) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(ctx)
	switch d, filters := evalRule(s.collections[collection].AccessRule(schema.ActionRead), p); d {
	case allow:
		return true
	case scope:
		// This path has only an id (no loaded record), so it always queries — unlike
		// recordReadable, which has the record and can take the in-Go fast path. A
		// single existence query decides any scope (owner, membership, composite): the
		// row exists AND matches the filters. A store error (logged by scopedExists)
		// and a non-matching/missing row both fail closed, so derived data (e.g.
		// revision history) never leaks past the record boundary.
		ok, _ := s.scopedExists(ctx, collection, id, filters)
		return ok
	default:
		return false
	}
}

// authorizeRecordWrite authorizes an update/delete (and lifecycle transitions).
// For a collection-scope rule (public/authenticated/roles) no record is needed.
// For `owner` it loads the record and compares created_by; a missing record is a
// 404 and a non-owner is a 403. Returns false (and writes the error) when denied.
func (s *Server) authorizeRecordWrite(w http.ResponseWriter, r *http.Request, collection, id string, action schema.AccessAction) bool {
	return s.authorizeWriteRule(w, r, collection, id, s.collections[collection].AccessRule(action))
}

// authorizeWriteRule is authorizeRecordWrite against an explicit rule, so a write
// path can authorize against a rule other than the CRUD default — e.g. a
// transition against the `publish` rule (issue #23).
func (s *Server) authorizeWriteRule(w http.ResponseWriter, r *http.Request, collection, id string, rule schema.Rule) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	switch d, filters := evalRule(rule, p); d {
	case allow:
		return true
	case scope:
		// Load the record so a genuinely missing one is a 404, then test it against
		// the scope. This preserves the 404-vs-403 split: the record does not exist →
		// not found; it exists but the caller is not in scope → forbidden.
		rec, err := s.db.FindOne(r.Context(), collection, id)
		if err != nil {
			writeStoreError(w, s.logger, r, err)
			return false
		}
		if s.recordInScope(r.Context(), collection, rec, filters) {
			return true
		}
		s.denyWrite(w, p)
		return false
	default:
		s.denyWrite(w, p)
		return false
	}
}
