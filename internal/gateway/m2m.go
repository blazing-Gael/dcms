package gateway

import (
	"context"
	"fmt"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// writeWithLinks performs a base write (create or update) together with any
// many-to-many link replacements and, on a revisioned collection, a captured
// revision (operation). With none of those needed it writes directly; otherwise
// the base write, link updates, and revision run in one transaction, so a record
// and everything recording it commit or roll back together.
func (s *Server) writeWithLinks(
	ctx context.Context,
	collection string,
	operation string,
	data store.Record,
	write func(context.Context, store.DB, store.Record) (store.Record, error),
) (store.Record, error) {
	base, links := s.splitM2M(collection, data)
	if len(links) == 0 && !s.needsWriteTx(collection) {
		return write(ctx, s.db, base)
	}
	var rec store.Record
	err := s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		var e error
		// Before* hook (ADR-0031): may mutate the base record or reject the write.
		if ev, ok := beforeWriteEvent(operation); ok {
			if base, e = s.beforeWrite(ctx, tx, collection, ev, base); e != nil {
				return e
			}
		}
		if rec, e = write(ctx, tx, base); e != nil {
			return e
		}
		id, _ := rec["id"].(string)
		for field, ids := range links {
			if e = s.replaceLinks(ctx, tx, collection, field, id, ids); e != nil {
				return e
			}
		}
		if e = s.captureWrite(ctx, tx, collection, rec, operation); e != nil {
			return e
		}
		// After* hook: reacts to the completed write, in the same transaction.
		if ev, ok := afterWriteEvent(operation); ok {
			return s.afterWrite(ctx, tx, collection, ev, rec)
		}
		return nil
	})
	return rec, err
}

// splitM2M partitions a request body into the columns written to the base table
// and the many-to-many link sets (field → target ids). m2m fields are removed
// from the base data because they have no column on the collection's own table.
// Validation has already confirmed each m2m value is a list of string ids.
func (s *Server) splitM2M(collection string, data store.Record) (base store.Record, links map[string][]string) {
	m2m := map[string]bool{}
	for _, f := range s.collections[collection].Fields {
		if f.Type == schema.TypeRelation && f.Many {
			m2m[f.Name] = true
		}
	}
	base = make(store.Record, len(data))
	links = map[string][]string{}
	for k, v := range data {
		if !m2m[k] {
			base[k] = v
			continue
		}
		arr, _ := v.([]any)
		ids := make([]string, 0, len(arr))
		for _, e := range arr {
			if id, ok := e.(string); ok {
				ids = append(ids, id)
			}
		}
		links[k] = ids
	}
	return base, links
}

// replaceLinks makes the join table hold exactly targetIDs for a source record:
// it clears the source's existing links, then inserts the new set. Inserts go
// through the store's Create so each link row gets its id and audit columns
// stamped automatically; the unique (source_id,target_id) index keeps it
// idempotent. Runs on whatever DB it's given — the caller supplies a tx.
func (s *Server) replaceLinks(ctx context.Context, db store.DB, collection, field, sourceID string, targetIDs []string) error {
	table := schema.JoinTableName(collection, field)
	if _, err := db.RawExec(ctx, `DELETE FROM "`+table+`" WHERE source_id = $1`, sourceID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, tid := range targetIDs {
		if tid == "" || seen[tid] {
			continue
		}
		seen[tid] = true
		_, err := db.Create(ctx, store.WriteInput{
			Collection: table,
			Data:       store.Record{"source_id": sourceID, "target_id": tid},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// expandM2M loads a many-to-many field's targets onto a record: read the link
// rows for this source, then batch-fetch the target records.
func (s *Server) expandM2M(ctx context.Context, collection, target string, rec store.Record, field string) error {
	table := schema.JoinTableName(collection, field)
	sourceID := fmt.Sprint(rec["id"])

	linkPage, err := s.db.Find(ctx, store.Query{
		Collection: table,
		Filters:    []store.Filter{{Field: "source_id", Operator: store.Eq, Value: sourceID}},
		Limit:      maxM2MExpand,
	})
	if err != nil {
		return err
	}
	var ids []any
	for _, l := range linkPage.Data {
		if tid, ok := l["target_id"].(string); ok {
			ids = append(ids, tid)
		}
	}
	if len(ids) == 0 {
		rec[field] = []store.Record{}
		return nil
	}

	filters := []store.Filter{{Field: "id", Operator: store.In, Value: ids}}
	filters = append(filters, s.lifecycleFiltersFor(ctx, target)...)
	page, err := s.db.Find(ctx, store.Query{
		Collection: target,
		Filters:    filters,
		Limit:      len(ids),
	})
	if err != nil {
		return err
	}
	rec[field] = s.coerceExpandedList(ctx, target, page.Data)
	return nil
}

// maxM2MExpand caps how many links we expand for one record. A generous ceiling
// that keeps a pathological record from fetching unboundedly; real pagination of
// relations is a later refinement.
const maxM2MExpand = 1000

// maxM2MListExpand caps how many related rows are inlined PER RECORD when a
// many-to-many is expanded on a list (issue #29). It is tighter than the
// single-record maxM2MExpand because a list multiplies the cost by the page size;
// a record with more links than this has its relation truncated, reported via
// meta.expand_truncated so a consumer can fetch that record's relation directly.
const maxM2MListExpand = 100

// findAllUpTo pages through a query until it is exhausted or `ceiling` rows have
// been collected, returning the rows and whether the ceiling cut it short. The
// store caps a single Find at 100 rows (a locked invariant), so a batched
// expansion that may match more than that must paginate rather than silently see
// only the first page.
func (s *Server) findAllUpTo(ctx context.Context, q store.Query, ceiling int) ([]store.Record, bool, error) {
	q.SkipCount = true // internal fetch; no COUNT needed
	var out []store.Record
	for {
		page, err := s.db.Find(ctx, q)
		if err != nil {
			return nil, false, err
		}
		out = append(out, page.Data...)
		if len(out) >= ceiling {
			more := len(out) > ceiling || page.NextCursor != ""
			return out[:ceiling], more, nil
		}
		if page.NextCursor == "" {
			return out, false, nil
		}
		q.Cursor = page.NextCursor
	}
}

// expandM2MBatch expands a forward many-to-many field across a whole page (issue
// #29): it pages the join table for every source id on the page, then fetches the
// distinct targets filtered by the request's lifecycle view. It mirrors
// expandBelongsToBatch, so a static build fetches a page's relations in
// O(links/100) queries instead of one request per record. Targets are filtered
// through coerceExpandedList (the target's read rule + field masks), so expansion
// never exposes a row a direct read would withhold. Returns whether any record's
// relation was truncated (capped at maxM2MListExpand, or the page's total links
// exceeded what could be displayed).
func (s *Server) expandM2MBatch(ctx context.Context, collection, target string, recs []store.Record, field string) (bool, error) {
	table := schema.JoinTableName(collection, field)

	seen := map[string]bool{}
	var sourceIDs []any
	for _, r := range recs {
		if id, ok := r["id"].(string); ok && id != "" && !seen[id] {
			seen[id] = true
			sourceIDs = append(sourceIDs, id)
		}
	}
	if len(sourceIDs) == 0 {
		return false, nil
	}

	// Gather the page's links, bounded to what every record could display at most,
	// so a pathological page can't scan unboundedly; hitting the bound is truncation.
	linkCeiling := len(sourceIDs) * maxM2MListExpand
	links, truncated, err := s.findAllUpTo(ctx, store.Query{
		Collection: table,
		Filters:    []store.Filter{{Field: "source_id", Operator: store.In, Value: sourceIDs}},
	}, linkCeiling)
	if err != nil {
		return false, err
	}

	// Group target ids per source in link order, capping each source at the
	// per-record limit; collect the distinct target ids to fetch once.
	linksBySource := make(map[string][]string, len(sourceIDs))
	targetSeen := map[string]bool{}
	var targetIDs []any
	for _, l := range links {
		sid, _ := l["source_id"].(string)
		tid, _ := l["target_id"].(string)
		if sid == "" || tid == "" {
			continue
		}
		if len(linksBySource[sid]) >= maxM2MListExpand {
			truncated = true
			continue
		}
		linksBySource[sid] = append(linksBySource[sid], tid)
		if !targetSeen[tid] {
			targetSeen[tid] = true
			targetIDs = append(targetIDs, tid)
		}
	}

	byID := make(map[string]store.Record, len(targetIDs))
	if len(targetIDs) > 0 {
		filters := []store.Filter{{Field: "id", Operator: store.In, Value: targetIDs}}
		filters = append(filters, s.lifecycleFiltersFor(ctx, target)...)
		rows, _, err := s.findAllUpTo(ctx, store.Query{Collection: target, Filters: filters}, len(targetIDs))
		if err != nil {
			return truncated, err
		}
		for _, tr := range s.coerceExpandedList(ctx, target, rows) {
			if id, ok := tr["id"].(string); ok {
				byID[id] = tr
			}
		}
	}

	// Attach per record, preserving link order and dropping hidden/unreadable
	// targets. Always a non-nil slice, so an empty relation serializes as [].
	for _, r := range recs {
		id, _ := r["id"].(string)
		list := make([]store.Record, 0, len(linksBySource[id]))
		for _, tid := range linksBySource[id] {
			if obj, ok := byID[tid]; ok {
				list = append(list, obj)
			}
		}
		r[field] = list
	}
	return truncated, nil
}
