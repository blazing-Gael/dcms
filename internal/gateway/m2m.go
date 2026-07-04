package gateway

import (
	"context"
	"fmt"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// writeWithLinks performs a base write (create or update) together with any
// many-to-many link replacements. With no m2m fields in the body it writes
// directly; otherwise the base write and the link updates run in one
// transaction, so a record and its relations commit or roll back together.
func (s *Server) writeWithLinks(
	ctx context.Context,
	collection string,
	data store.Record,
	write func(context.Context, store.DB, store.Record) (store.Record, error),
) (store.Record, error) {
	base, links := s.splitM2M(collection, data)
	if len(links) == 0 {
		return write(ctx, s.db, base)
	}
	var rec store.Record
	err := s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		var e error
		if rec, e = write(ctx, tx, base); e != nil {
			return e
		}
		id, _ := rec["id"].(string)
		for field, ids := range links {
			if e = s.replaceLinks(ctx, tx, collection, field, id, ids); e != nil {
				return e
			}
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

	page, err := s.db.Find(ctx, store.Query{
		Collection: target,
		Filters:    []store.Filter{{Field: "id", Operator: store.In, Value: ids}},
		Limit:      len(ids),
	})
	if err != nil {
		return err
	}
	for _, r := range page.Data {
		s.coerceExpanded(target, r)
	}
	if page.Data == nil {
		rec[field] = []store.Record{}
	} else {
		rec[field] = page.Data
	}
	return nil
}

// maxM2MExpand caps how many links we expand for one record. A generous ceiling
// that keeps a pathological record from fetching unboundedly; real pagination of
// relations is a later refinement.
const maxM2MExpand = 1000
