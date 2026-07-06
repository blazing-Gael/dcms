package gateway

import (
	"context"
	"fmt"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Read-side expansion of rich content (ADR-0014, ADR-0015). `?expand=<richtext
// field>` resolves the records an in-content reference points at — image blocks
// (→ the media library, so a renderer gets the asset's url) and `reference`
// nodes/annotations (→ the named collection) — into a deduplicated manifest
// returned at the response root as `included`. The AST itself stays id-only, so
// documents remain cacheable, a shared reference is serialized once across a list,
// and reference cycles are harmless. A reference that is missing or hidden by the
// request's lifecycle view is simply absent from the manifest — never a 500.
//
// Resolution is batched exactly like relation expansion: references are collected
// across every document first, then fetched with one `id IN (…)` query per target
// collection, so expanding a list of records costs a handful of queries, not one
// per reference (honoring the no-N+1 mandate).

// expandRichTextDocs resolves the in-content references of a richtext field across
// a set of documents in one batched pass, adding the visible targets to the shared
// manifest m. docs may contain nil entries (a record with no value for the field),
// which are skipped.
func (s *Server) expandRichTextDocs(ctx context.Context, fd schema.FieldDef, docs [][]any, m refManifest) error {
	// Collect the referenced ids, grouped by target collection, across all docs.
	want := map[string]map[string]bool{}
	distinct := 0
	for _, doc := range docs {
		for _, r := range fd.RichTextRefs(doc) {
			// An unknown reference collection can't be resolved; it was already a
			// write-time validation error, so here we just skip it.
			if !s.knownCollection(r.Collection) {
				continue
			}
			if want[r.Collection] == nil {
				want[r.Collection] = map[string]bool{}
			}
			if !want[r.Collection][r.ID] {
				distinct++
				if distinct > maxResolvedRefs {
					return badRequest("expand", fmt.Sprintf("too many in-content references to resolve (over %d); narrow the page or omit expand", maxResolvedRefs))
				}
			}
			want[r.Collection][r.ID] = true
		}
	}

	// Fetch the visible target records, one batched query per collection, into the
	// manifest keyed collection:id (deduped across every document).
	for collection, idset := range want {
		byID, err := s.loadVisibleByIDs(ctx, collection, idset)
		if err != nil {
			return err
		}
		for _, rec := range byID {
			m.add(collection, rec)
		}
	}
	return nil
}

// loadVisibleByIDs batch-fetches the records of a collection whose ids are in
// idset and that are visible under the request's lifecycle view, returned keyed
// by id. Chunked by refCheckBatch so a set larger than the store's list limit is
// still fetched completely — a handful of queries, never one per id.
func (s *Server) loadVisibleByIDs(ctx context.Context, collection string, idset map[string]bool) (map[string]store.Record, error) {
	all := make([]any, 0, len(idset))
	for id := range idset {
		all = append(all, id)
	}
	out := make(map[string]store.Record, len(all))
	for start := 0; start < len(all); start += refCheckBatch {
		end := min(start+refCheckBatch, len(all))
		chunk := all[start:end]
		filters := []store.Filter{{Field: "id", Operator: store.In, Value: chunk}}
		filters = append(filters, s.lifecycleFilters(collection, visibilityFromContext(ctx))...)
		page, err := s.db.Find(ctx, store.Query{
			Collection: collection,
			Filters:    filters,
			Limit:      len(chunk),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Data {
			s.coerceExpanded(collection, r)
			if id, ok := r["id"].(string); ok {
				out[id] = r
			}
		}
	}
	return out, nil
}

// docOf reads a coerced richtext field value as a document (an array of nodes).
// Returns nil when the field is absent or not an array (nothing to expand).
func docOf(v any) []any {
	doc, _ := v.([]any)
	return doc
}
