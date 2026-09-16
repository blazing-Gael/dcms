package gateway

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// visibility captures how the current request WANTS to see lifecycle-managed
// records (ADR-0012). The zero value is the public view: only live, non-trashed
// content. status/includeDeleted are always parsed from the query, but whether
// they are HONORED is decided per collection/record by preview eligibility
// (tokenPreview, or the `preview` access rule — ADR-0023), never by these fields
// alone. The effectiveListVisibility / recordPreviewVisible helpers are the only
// safe consumers; a raw visibility must not be handed to lifecycleFilters for a
// caller whose preview eligibility hasn't been checked, or ?status=draft would
// leak drafts.
type visibility struct {
	tokenPreview   bool   // a valid shared preview token was presented (full admin view)
	status         string // "", draft, published, archived, scheduled, any
	includeDeleted string // "", true, only
}

type ctxKey int

const visibilityKey ctxKey = iota

// withVisibility resolves the request's visibility once and stashes it in the
// context, so the top-level query, the get-one check, and every expansion path
// share one decision.
func (s *Server) withVisibility(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), visibilityKey, s.visibilityFor(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func visibilityFromContext(ctx context.Context) visibility {
	v, _ := ctx.Value(visibilityKey).(visibility)
	return v
}

// visibilityFor computes the request's desired visibility. The status /
// include_deleted params are ALWAYS parsed (an identity-based previewer may
// request a hidden state — ADR-0023); whether they are honored is decided later
// per collection. A valid shared preview token, matched constant-time against the
// X-DCMS-Preview header (or a preview_token query param), sets tokenPreview and
// defaults status to the admin "any" view, preserving the token's behaviour.
func (s *Server) visibilityFor(r *http.Request) visibility {
	q := r.URL.Query()
	v := visibility{
		status:         strings.ToLower(strings.TrimSpace(q.Get("status"))),
		includeDeleted: strings.ToLower(strings.TrimSpace(q.Get("include_deleted"))),
	}
	if tok := s.opts.PreviewToken; tok != "" {
		got := r.Header.Get("X-DCMS-Preview")
		if got == "" {
			got = q.Get("preview_token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1 {
			v.tokenPreview = true
			if v.status == "" {
				v.status = "any" // token defaults to the full admin view
			}
		}
	}
	return v
}

// requestsHidden reports whether the requested view asks for any non-public state
// — a hidden publishing state or trashed rows. Only such a request needs preview
// eligibility; the default (published-only, active) is open to everyone and never
// narrows the read view.
func requestsHidden(v visibility) bool {
	switch v.status {
	case schema.StatusDraft, schema.StatusArchived, "scheduled", "any":
		return true
	}
	switch v.includeDeleted {
	case "true", "all", "only":
		return true
	}
	return false
}

// previewDecision resolves the identity `preview` rule (ADR-0023) for a collection
// against the request's principal, mirroring evalRule's allow/ownerScope/deny plus
// the owner column for ownerScope. A collection with no preview rule is deny (only
// the shared token, handled separately, can widen the view). The token is NOT
// folded in here; callers check tokenPreview first.
func (s *Server) previewDecision(ctx context.Context, collection string) (decision, string) {
	rule, ok := s.collections[collection].PreviewRule()
	if !ok {
		return deny, ""
	}
	return evalRule(rule, principalFromContext(ctx))
}

// effectiveListVisibility returns the visibility to apply to a list/expansion
// query and any owner-scoping filters the `preview` rule adds. It is the safe way
// to consume a request's visibility: it downgrades to the public view unless the
// caller is preview-eligible for the requested hidden states. The token honours
// the request as-is (no owner narrowing); the identity path honours hidden states
// only when the preview rule allows, narrowing to owned rows on ownerScope.
func (s *Server) effectiveListVisibility(ctx context.Context, collection string) (visibility, []store.Filter) {
	v := visibilityFromContext(ctx)
	if v.tokenPreview {
		return v, nil
	}
	if !requestsHidden(v) {
		return visibility{}, nil // default public view — preview irrelevant
	}
	// The caller reached here only by opting into hidden rows (requestsHidden). If
	// they set include_deleted but no status, that means "any status" (e.g. show my
	// trash whatever its publish state) — otherwise the published-only default would
	// hide a trashed draft.
	if v.status == "" {
		v.status = "any"
	}
	switch d, field := s.previewDecision(ctx, collection); d {
	case allow:
		return v, nil
	case ownerScope:
		p := principalFromContext(ctx)
		return v, []store.Filter{{Field: field, Operator: store.Eq, Value: p.ID}}
	default:
		return visibility{}, nil // not eligible → ignore the hidden request
	}
}

// lifecycleFiltersFor is the store-filter form of effectiveListVisibility: the
// lifecycle filters for the effective view plus any preview owner filter. Every
// list and list-expansion path uses it so ?status is honoured only for eligible
// callers.
func (s *Server) lifecycleFiltersFor(ctx context.Context, collection string) []store.Filter {
	ev, previewFilters := s.effectiveListVisibility(ctx, collection)
	return append(s.lifecycleFilters(collection, ev), previewFilters...)
}

// writeHidden reports whether a write (update/delete/transition) must be refused
// as not-found because its target is hidden from the caller under the `preview`
// rule (ADR-0023) — so write agrees with get-one. Only collections that declare a
// preview rule gate writes this way; without one, writes behave as before
// (visibility ignored), keeping existing schemas unchanged. A missing record
// returns false so the write path surfaces the not-found itself.
func (s *Server) writeHidden(ctx context.Context, collection, id string) bool {
	if _, ok := s.collections[collection].PreviewRule(); !ok {
		return false
	}
	rec, err := s.db.FindOne(ctx, collection, id)
	if err != nil {
		return false
	}
	return !s.recordPreviewVisible(ctx, collection, rec)
}

// recordPreviewVisible reports whether a fetched record may be shown to the
// caller, combining lifecycle visibility with preview eligibility (ADR-0023). A
// publicly-visible record (published, live, not trashed) is always visible; a
// hidden one requires the shared token (honouring its requested view) or the
// identity preview rule (allow, or ownerScope with the record's owner matching).
func (s *Server) recordPreviewVisible(ctx context.Context, collection string, rec store.Record) bool {
	v := visibilityFromContext(ctx)
	if v.tokenPreview {
		return s.recordVisible(collection, rec, v) // token: honour its requested view
	}
	if s.recordVisible(collection, rec, visibility{}) {
		return true // publicly visible regardless of identity
	}
	return s.previewEligible(ctx, collection, rec) // hidden: only the preview rule
}

// previewEligible reports whether the caller may see this record's hidden state —
// the record-level preview decision alone, WITHOUT the public fallback. The
// shared token grants it outright; otherwise the collection's `preview` rule must
// admit the caller (allow, or ownerScope with the record's owner matching). Used
// where "can this identity see behind the curtain" is the question independent of
// whether the record happens to be public — e.g. revision history (issue #24).
func (s *Server) previewEligible(ctx context.Context, collection string, rec store.Record) bool {
	if visibilityFromContext(ctx).tokenPreview {
		return true
	}
	switch d, field := s.previewDecision(ctx, collection); d {
	case allow:
		return true
	case ownerScope:
		owner, _ := rec[field].(string)
		return owner != "" && owner == principalFromContext(ctx).ID
	default:
		return false
	}
}

// nowUTC is the wall clock used for publish scheduling and the published_at<=now
// predicate. Returned as time.Time so the store normalizes it to the same RFC3339
// UTC text it stores, keeping the comparison a plain lexicographic one.
func nowUTC() time.Time { return time.Now().UTC() }

// lifecycleFilters returns the store filters that enforce the request's view of a
// collection's lifecycle. Empty for collections without a lifecycle, or when the
// view imposes no constraint (preview ?status=any / ?include_deleted=true).
func (s *Server) lifecycleFilters(collection string, v visibility) []store.Filter {
	cd, ok := s.collections[collection]
	if !ok {
		return nil
	}
	var fs []store.Filter
	if cd.Publishing {
		switch v.status {
		case "any":
			// no status constraint
		case "scheduled":
			fs = append(fs,
				store.Filter{Field: schema.LifecycleStatus, Operator: store.Eq, Value: schema.StatusPublished},
				store.Filter{Field: schema.LifecyclePublishedAt, Operator: store.Gt, Value: nowUTC()})
		case schema.StatusDraft, schema.StatusPublished, schema.StatusArchived:
			fs = append(fs, store.Filter{Field: schema.LifecycleStatus, Operator: store.Eq, Value: v.status})
		default: // "" (public) or an unrecognized value → live only
			fs = append(fs,
				store.Filter{Field: schema.LifecycleStatus, Operator: store.Eq, Value: schema.StatusPublished},
				store.Filter{Field: schema.LifecyclePublishedAt, Operator: store.Lte, Value: nowUTC()})
		}
	}
	if cd.SoftDelete {
		switch v.includeDeleted {
		case "true", "all":
			// include trashed and active — no filter
		case "only":
			fs = append(fs, store.Filter{Field: schema.LifecycleDeletedAt, Operator: store.NotNull})
		default: // active only
			fs = append(fs, store.Filter{Field: schema.LifecycleDeletedAt, Operator: store.IsNull})
		}
	}
	return fs
}

// recordVisible reports whether a fetched record is visible under the request's
// view — the in-memory mirror of lifecycleFilters, used for get-one (→ 404 when
// hidden) and to decide whether a belongs-to target may be inlined.
func (s *Server) recordVisible(collection string, rec store.Record, v visibility) bool {
	cd, ok := s.collections[collection]
	if !ok {
		return true
	}
	if cd.Publishing {
		status, _ := rec[schema.LifecycleStatus].(string)
		switch v.status {
		case "any":
			// visible regardless of status
		case "scheduled":
			if status != schema.StatusPublished || publishedAtReached(rec) {
				return false
			}
		case schema.StatusDraft, schema.StatusPublished, schema.StatusArchived:
			if status != v.status {
				return false
			}
		default: // public / unrecognized → live only
			if status != schema.StatusPublished || !publishedAtReached(rec) {
				return false
			}
		}
	}
	if cd.SoftDelete {
		deleted := isSet(rec[schema.LifecycleDeletedAt])
		switch v.includeDeleted {
		case "true", "all":
		case "only":
			if !deleted {
				return false
			}
		default:
			if deleted {
				return false
			}
		}
	}
	return true
}

// publishedAtReached reports whether a record's _published_at is set and not in
// the future. A null/absent published_at means "never published" → not reached.
func publishedAtReached(rec store.Record) bool {
	pa, ok := rec[schema.LifecyclePublishedAt].(string)
	if !ok || pa == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, pa)
	if err != nil {
		return false
	}
	return !t.After(time.Now().UTC())
}

func isSet(v any) bool {
	s, ok := v.(string)
	return v != nil && (!ok || s != "")
}

// stripManagedFields removes engine-managed columns from a client-supplied body.
// Every managed column (audit + lifecycle) is underscore-prefixed, so clients can
// never set _status/_published_at/_deleted_at directly — those change only via the
// transition endpoints. (`id` is set from the URL and is not underscore-prefixed.)
func stripManagedFields(data store.Record) {
	for k := range data {
		if strings.HasPrefix(k, "_") {
			delete(data, k)
		}
	}
}
