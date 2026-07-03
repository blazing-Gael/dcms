package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// parseExpand pulls the comma-separated `expand` query param into tokens.
func parseExpand(values url.Values) []string {
	raw := values.Get("expand")
	if raw == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// expandRecord resolves each expand token against one record, mutating it in
// place. A belongs-to token replaces the FK id with the fetched target object;
// an inverse (has-many) token adds a new key holding the list of related
// records. Unknown/ambiguous tokens are client errors (→ 422 via badRequest).
//
// Call this AFTER response validation: at validation time the FK is still the
// string id the schema promises; expansion changes the shape afterward.
func (s *Server) expandRecord(ctx context.Context, collection string, rec store.Record, tokens []string) error {
	cd := s.collections[collection]
	for _, tok := range tokens {
		if target, ok := cd.BelongsTo(tok); ok {
			if err := s.expandBelongsTo(ctx, target, rec, tok); err != nil {
				return err
			}
			continue
		}
		if target, ok := cd.ManyToMany(tok); ok {
			if err := s.expandM2M(ctx, collection, target, rec, tok); err != nil {
				return err
			}
			continue
		}
		inv, err := s.inverseFor(collection, tok)
		if err != nil {
			return err
		}
		list, err := s.findRelated(ctx, inv, fmt.Sprint(rec["id"]))
		if err != nil {
			return err
		}
		rec[tok] = list
	}
	return nil
}

// expandBelongsTo fetches the single target of a belongs-to field and inlines it.
func (s *Server) expandBelongsTo(ctx context.Context, target string, rec store.Record, field string) error {
	idv, _ := rec[field].(string)
	if idv == "" {
		return nil // nothing referenced
	}
	related, err := s.db.FindOne(ctx, target, idv)
	if errors.Is(err, store.ErrNotFound) {
		return nil // dangling reference: leave the id as-is rather than 500
	}
	if err != nil {
		return err
	}
	s.collections[target].CoerceResponse(related)
	rec[field] = related
	return nil
}

// findRelated fetches the has-many children of parentID via an inverse edge.
func (s *Server) findRelated(ctx context.Context, inv schema.Inverse, parentID string) ([]store.Record, error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: inv.Source,
		Filters:    []store.Filter{{Field: inv.Field, Operator: store.Eq, Value: parentID}},
	})
	if err != nil {
		return nil, err
	}
	src := s.collections[inv.Source]
	for _, r := range page.Data {
		src.CoerceResponse(r)
	}
	if page.Data == nil {
		return []store.Record{}, nil
	}
	return page.Data, nil
}

// inverseFor resolves a has-many expand token (a collection name) to the single
// relation edge that justifies it, or a client error if there is none or the
// token is ambiguous.
func (s *Server) inverseFor(collection, tok string) (schema.Inverse, error) {
	var matches []schema.Inverse
	for _, inv := range s.schema.InverseRelations(collection) {
		if inv.Source == tok {
			matches = append(matches, inv)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return schema.Inverse{}, badRequest("expand", fmt.Sprintf("cannot expand %q on %q", tok, collection))
	default:
		return schema.Inverse{}, badRequest("expand",
			fmt.Sprintf("%q is ambiguous — %q has multiple relations to %q; disambiguation is not supported yet", tok, tok, collection))
	}
}

// expandListRecords expands a page of records. Only belongs-to tokens are
// allowed on lists (has-many on a list is expensive and disallowed for now);
// belongs-to is batched — distinct ids fetched once — to avoid N+1 queries.
func (s *Server) expandListRecords(ctx context.Context, collection string, recs []store.Record, tokens []string) error {
	cd := s.collections[collection]
	for _, tok := range tokens {
		target, ok := cd.BelongsTo(tok)
		if !ok {
			return badRequest("expand", fmt.Sprintf("cannot expand %q on a list (only belongs-to relations are expandable in lists)", tok))
		}
		if err := s.expandBelongsToBatch(ctx, target, recs, tok); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) expandBelongsToBatch(ctx context.Context, target string, recs []store.Record, field string) error {
	seen := map[string]bool{}
	var ids []any
	for _, r := range recs {
		if idv, ok := r[field].(string); ok && idv != "" && !seen[idv] {
			seen[idv] = true
			ids = append(ids, idv)
		}
	}
	if len(ids) == 0 {
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
	tcd := s.collections[target]
	byID := make(map[string]store.Record, len(page.Data))
	for _, tr := range page.Data {
		tcd.CoerceResponse(tr)
		if idv, ok := tr["id"].(string); ok {
			byID[idv] = tr
		}
	}
	for _, r := range recs {
		if idv, ok := r[field].(string); ok {
			if obj, found := byID[idv]; found {
				r[field] = obj
			}
		}
	}
	return nil
}
