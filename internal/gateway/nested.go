package gateway

import (
	"context"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// maxNestDepth bounds how deep an inline-write payload may nest related records,
// so a pathological (or malicious) body can't drive unbounded recursion.
const maxNestDepth = 10

// asRecord views a decoded JSON value as a record if it is an object.
func asRecord(v any) (store.Record, bool) {
	switch m := v.(type) {
	case store.Record:
		return m, true
	case map[string]any:
		return store.Record(m), true
	default:
		return nil, false
	}
}

// hasInlineRelations reports whether the body carries any inline related object
// — a belongs-to given as an object instead of an id, or a many-to-many list
// containing an object. Such bodies take the transactional nested-write path;
// everything else stays on the plain write path.
func (s *Server) hasInlineRelations(collection string, data store.Record) bool {
	for _, f := range s.collections[collection].Fields {
		if f.Type != schema.TypeRelation {
			continue
		}
		v, ok := data[f.Name]
		if !ok {
			continue
		}
		if !f.Many {
			if _, isObj := asRecord(v); isObj {
				return true
			}
			continue
		}
		if arr, ok := v.([]any); ok {
			for _, e := range arr {
				if _, isObj := asRecord(e); isObj {
					return true
				}
			}
		}
	}
	return false
}

// resolveInlineRelations replaces inline related objects in data with the ids of
// records created for them inside tx, recursively. A belongs-to object is created
// in its target collection and the field is set to the new id; a many-to-many
// list has each object item created and swapped for its id (string ids pass
// through). After this runs, every relation field holds ids, so the normal
// validation and link path applies unchanged.
func (s *Server) resolveInlineRelations(ctx context.Context, tx store.DB, collection string, data store.Record, depth int) error {
	if depth > maxNestDepth {
		return &store.ValidationError{Fields: map[string]string{collection: "related-record nesting is too deep"}}
	}
	for _, f := range s.collections[collection].Fields {
		if f.Type != schema.TypeRelation {
			continue
		}
		v, ok := data[f.Name]
		if !ok {
			continue
		}
		if !f.Many {
			if obj, isObj := asRecord(v); isObj {
				child, err := s.createRecord(ctx, tx, f.Target, obj, depth+1)
				if err != nil {
					return err
				}
				data[f.Name] = child["id"]
			}
			continue
		}
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		for i, e := range arr {
			if obj, isObj := asRecord(e); isObj {
				child, err := s.createRecord(ctx, tx, f.Target, obj, depth+1)
				if err != nil {
					return err
				}
				arr[i] = child["id"]
			}
		}
	}
	return nil
}

// createRecord creates one record and all of its relations inside tx: it first
// creates any inline related records (substituting their ids), then validates,
// checks that referenced ids resolve, writes the base row, and replaces its
// many-to-many links. Recursive via resolveInlineRelations. Returns the stored
// record. The whole tree commits or rolls back together with the caller's tx.
func (s *Server) createRecord(ctx context.Context, tx store.DB, collection string, data store.Record, depth int) (store.Record, error) {
	if err := s.resolveInlineRelations(ctx, tx, collection, data, depth); err != nil {
		return nil, err
	}
	if errs := s.collections[collection].ValidateCreate(data); errs != nil {
		return nil, &store.ValidationError{Fields: errs}
	}
	if err := s.checkReferences(ctx, tx, collection, data); err != nil {
		return nil, err
	}
	base, links := s.splitM2M(collection, data)
	rec, err := tx.Create(ctx, store.WriteInput{Collection: collection, Data: base})
	if err != nil {
		return nil, err
	}
	id, _ := rec["id"].(string)
	for field, ids := range links {
		if err := s.replaceLinks(ctx, tx, collection, field, id, ids); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// updateRecord is createRecord's PATCH counterpart: inline related objects are
// still *created* (there is no inline update), but the base row is updated rather
// than inserted. Runs inside tx so the record, its new relations, and its link
// changes are atomic.
func (s *Server) updateRecord(ctx context.Context, tx store.DB, collection string, data store.Record, depth int) (store.Record, error) {
	if err := s.resolveInlineRelations(ctx, tx, collection, data, depth); err != nil {
		return nil, err
	}
	if errs := s.collections[collection].ValidateUpdate(data); errs != nil {
		return nil, &store.ValidationError{Fields: errs}
	}
	if err := s.checkReferences(ctx, tx, collection, data); err != nil {
		return nil, err
	}
	base, links := s.splitM2M(collection, data)
	rec, err := tx.Update(ctx, store.WriteInput{Collection: collection, Data: base})
	if err != nil {
		return nil, err
	}
	id, _ := rec["id"].(string)
	for field, ids := range links {
		if err := s.replaceLinks(ctx, tx, collection, field, id, ids); err != nil {
			return nil, err
		}
	}
	return rec, nil
}
