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

// visibility captures how the current request may see lifecycle-managed records
// (ADR-0012). The zero value is the public view: only live, non-trashed content.
// status/includeDeleted are honored only when preview is true (a valid preview
// token was presented), so unauthenticated callers can never widen their view.
type visibility struct {
	preview        bool
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

// visibilityFor computes the request's visibility. The preview bypass requires a
// configured token, matched constant-time against the X-DCMS-Preview header (or
// a preview_token query param). Only then are the status / include_deleted params
// read; otherwise they are ignored.
func (s *Server) visibilityFor(r *http.Request) visibility {
	tok := s.opts.PreviewToken
	if tok == "" {
		return visibility{}
	}
	got := r.Header.Get("X-DCMS-Preview")
	if got == "" {
		got = r.URL.Query().Get("preview_token")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
		return visibility{}
	}
	q := r.URL.Query()
	v := visibility{
		preview:        true,
		status:         strings.ToLower(strings.TrimSpace(q.Get("status"))),
		includeDeleted: strings.ToLower(strings.TrimSpace(q.Get("include_deleted"))),
	}
	// A valid preview token defaults to showing every publishing state (the admin
	// view); trashed records still require an explicit include_deleted. An explicit
	// ?status narrows it.
	if v.status == "" {
		v.status = "any"
	}
	return v
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
