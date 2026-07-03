package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// refCheckBatch bounds how many ids go into one `id IN (…)` lookup, matching the
// store's list limit so a large link set is verified in a few queries rather than
// being silently truncated.
const refCheckBatch = 100

// checkReferences is the validation layer of referential integrity (ADR-0010):
// before a write, every belongs-to id and every many-to-many link id present in
// the body is checked to resolve to an existing target record. Lookups are
// batched — one `id IN (…)` query per referenced collection (chunked), never one
// per row — so this adds a handful of queries regardless of how many relations or
// links a payload carries, honoring the no-N+1 mandate.
//
// A miss is a client error: it returns a *store.ValidationError naming the
// offending field, which the gateway maps to a 422. This layer owns user-facing
// reference errors; the database foreign keys are a backstop only and must never
// be the source of a user-facing error.
func (s *Server) checkReferences(ctx context.Context, db store.DB, collection string, data store.Record) error {
	cd := s.collections[collection]

	// field → (target collection, ids it references in this body).
	type ref struct {
		target string
		ids    []string
	}
	refs := map[string]*ref{}
	// target collection → distinct ids to look up across all fields.
	want := map[string]map[string]bool{}

	add := func(field, target, id string) {
		if id == "" {
			return
		}
		r := refs[field]
		if r == nil {
			r = &ref{target: target}
			refs[field] = r
		}
		r.ids = append(r.ids, id)
		if want[target] == nil {
			want[target] = map[string]bool{}
		}
		want[target][id] = true
	}

	for _, f := range cd.Fields {
		v, present := data[f.Name]
		if !present || f.Type != schema.TypeRelation {
			continue
		}
		if f.Many {
			// A malformed m2m value is a type error the field validator already
			// caught; here we only harvest the string ids.
			if arr, ok := v.([]any); ok {
				for _, e := range arr {
					if id, ok := e.(string); ok {
						add(f.Name, f.Target, id)
					}
				}
			}
			continue
		}
		if id, ok := v.(string); ok {
			add(f.Name, f.Target, id)
		}
	}

	if len(want) == 0 {
		return nil
	}

	present := make(map[string]map[string]bool, len(want))
	for target, idset := range want {
		found, err := s.existingIDs(ctx, db, target, idset)
		if err != nil {
			return err
		}
		present[target] = found
	}

	fields := map[string]string{}
	for field, r := range refs {
		seen := map[string]bool{}
		var missing []string
		for _, id := range r.ids {
			if present[r.target][id] || seen[id] {
				continue
			}
			seen[id] = true
			missing = append(missing, id)
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			fields[field] = fmt.Sprintf("no such %s record: %s", r.target, strings.Join(missing, ", "))
		}
	}
	if len(fields) > 0 {
		return &store.ValidationError{Fields: fields}
	}
	return nil
}

// existingIDs returns the subset of idset that exists in collection, fetched in
// chunks of refCheckBatch so a set larger than the store's list limit is still
// checked completely.
func (s *Server) existingIDs(ctx context.Context, db store.DB, collection string, idset map[string]bool) (map[string]bool, error) {
	all := make([]any, 0, len(idset))
	for id := range idset {
		all = append(all, id)
	}
	found := make(map[string]bool, len(all))
	for start := 0; start < len(all); start += refCheckBatch {
		end := start + refCheckBatch
		if end > len(all) {
			end = len(all)
		}
		chunk := all[start:end]
		page, err := db.Find(ctx, store.Query{
			Collection: collection,
			Filters:    []store.Filter{{Field: "id", Operator: store.In, Value: chunk}},
			Fields:     []string{"id"},
			Limit:      len(chunk),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Data {
			if id, ok := r["id"].(string); ok {
				found[id] = true
			}
		}
	}
	return found, nil
}
