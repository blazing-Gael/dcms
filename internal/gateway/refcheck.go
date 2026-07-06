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

	// The (field, target, id) references this body makes, in order. A richtext
	// field may reference several target collections, so target lives per-ref, not
	// per-field.
	type ref struct {
		field, target, id string
	}
	var refs []ref
	// target collection → distinct ids to look up across all fields.
	want := map[string]map[string]bool{}
	// Field errors we can decide without a query (e.g. an in-content reference to a
	// collection that doesn't exist).
	fields := map[string]string{}

	add := func(field, target, id string) {
		if id == "" {
			return
		}
		refs = append(refs, ref{field: field, target: target, id: id})
		if want[target] == nil {
			want[target] = map[string]bool{}
		}
		want[target][id] = true
	}

	for _, f := range cd.Fields {
		v, present := data[f.Name]
		if !present {
			continue
		}
		switch {
		case f.Type == schema.TypeRelation && f.Many:
			// A malformed m2m value is a type error the field validator already
			// caught; here we only harvest the string ids.
			if arr, ok := v.([]any); ok {
				for _, e := range arr {
					if id, ok := e.(string); ok {
						add(f.Name, f.Target, id)
					}
				}
			}
		case f.Type == schema.TypeRelation:
			if id, ok := v.(string); ok {
				add(f.Name, f.Target, id)
			}
		case f.Type == schema.TypeRichText:
			// In-content references (image → _media, reference nodes → a named
			// collection). They ride the same batched check as relations (ADR-0014).
			for _, rr := range f.RichTextRefs(v) {
				if !s.knownCollection(rr.Collection) {
					fields[f.Name] = fmt.Sprintf("references an unknown collection %q", rr.Collection)
					continue
				}
				add(f.Name, rr.Collection, rr.ID)
			}
		}
	}

	if len(want) == 0 {
		return validationOrNil(fields)
	}

	present := make(map[string]map[string]bool, len(want))
	for target, idset := range want {
		found, err := s.existingIDs(ctx, db, target, idset)
		if err != nil {
			return err
		}
		present[target] = found
	}

	// Collect the missing ids per field, keeping targets distinct so the message
	// names which collection each dangling id was expected in.
	type miss struct {
		target string
		id     string
	}
	byField := map[string][]miss{}
	seen := map[string]bool{} // field|target|id, so a repeated ref is reported once
	for _, r := range refs {
		if present[r.target][r.id] {
			continue
		}
		k := r.field + "|" + r.target + "|" + r.id
		if seen[k] {
			continue
		}
		seen[k] = true
		byField[r.field] = append(byField[r.field], miss{r.target, r.id})
	}
	for field, misses := range byField {
		if _, already := fields[field]; already {
			continue // an unknown-collection error already covers this field
		}
		byTarget := map[string][]string{}
		var order []string
		for _, m := range misses {
			if _, ok := byTarget[m.target]; !ok {
				order = append(order, m.target)
			}
			byTarget[m.target] = append(byTarget[m.target], m.id)
		}
		sort.Strings(order)
		parts := make([]string, 0, len(order))
		for _, target := range order {
			ids := byTarget[target]
			sort.Strings(ids)
			parts = append(parts, fmt.Sprintf("no such %s record: %s", target, strings.Join(ids, ", ")))
		}
		fields[field] = strings.Join(parts, "; ")
	}
	return validationOrNil(fields)
}

// validationOrNil returns a *store.ValidationError for a non-empty field-error map,
// or nil when there are no problems.
func validationOrNil(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	return &store.ValidationError{Fields: fields}
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
