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
// place. Tokens may be dotted paths ("author.company") for nested expansion: the
// first segment is expanded here, then the remainder is applied recursively to
// the expanded target(s). A belongs-to segment replaces the FK id with the
// fetched object; an inverse (has-many or reverse many-to-many) segment adds a
// key holding the list of related records. Unknown/ambiguous segments are client
// errors (→ 422 via badRequest).
//
// Call this AFTER response validation: at validation time the FK is still the
// string id the schema promises; expansion changes the shape afterward. Nested
// expansion is single-record only — lists reject dotted tokens (see
// expandListRecords) to avoid multi-level N+1.
func (s *Server) expandRecord(ctx context.Context, collection string, rec store.Record, tokens []string) error {
	for seg, subs := range groupExpand(tokens) {
		target, err := s.expandOne(ctx, collection, rec, seg)
		if err != nil {
			return err
		}
		// Recurse for any non-leaf sub-paths, into whatever this segment produced.
		var childPaths []string
		for _, s := range subs {
			if s != "" {
				childPaths = append(childPaths, s)
			}
		}
		if len(childPaths) == 0 {
			continue
		}
		switch v := rec[seg].(type) {
		case store.Record:
			if err := s.expandRecord(ctx, target, v, childPaths); err != nil {
				return err
			}
		case []store.Record:
			for _, child := range v {
				if err := s.expandRecord(ctx, target, child, childPaths); err != nil {
					return err
				}
			}
		}
		// A segment that resolved to a bare id (dangling ref) can't be recursed
		// into; that's not an error, just nothing more to expand.
	}
	return nil
}

// groupExpand splits dotted expand tokens by their first segment, collecting the
// remaining path of each. "author" → {"author": [""]}; "author.company" →
// {"author": ["company"]}; both together → {"author": ["", "company"]}.
func groupExpand(tokens []string) map[string][]string {
	g := map[string][]string{}
	for _, t := range tokens {
		head, rest, _ := strings.Cut(t, ".")
		g[head] = append(g[head], rest)
	}
	return g
}

// expandOne expands a single field on a record and returns the collection of the
// expanded target (so the caller can recurse for nested paths). It handles
// belongs-to, forward many-to-many, and inverse edges (has-many + reverse m2m).
func (s *Server) expandOne(ctx context.Context, collection string, rec store.Record, field string) (string, error) {
	cd := s.collections[collection]
	if target, ok := cd.BelongsTo(field); ok {
		return target, s.expandBelongsTo(ctx, target, rec, field)
	}
	if target, ok := cd.ManyToMany(field); ok {
		return target, s.expandM2M(ctx, collection, target, rec, field)
	}
	return s.expandInverse(ctx, collection, rec, field)
}

// expandInverse resolves an inverse expand token (a source collection name) to
// the single relation edge that justifies it — either a belongs-to pointing back
// (has-many) or a many-to-many pointing back (reverse m2m) — and loads it. None
// or more than one match across both kinds is a client error.
func (s *Server) expandInverse(ctx context.Context, collection string, rec store.Record, field string) (string, error) {
	var belongs []schema.Inverse
	for _, inv := range s.schema.InverseRelations(collection) {
		if inv.Source == field {
			belongs = append(belongs, inv)
		}
	}
	var many []schema.M2MInverse
	for _, inv := range s.schema.InverseM2M(collection) {
		if inv.Source == field {
			many = append(many, inv)
		}
	}
	switch total := len(belongs) + len(many); {
	case total == 0:
		return "", badRequest("expand", fmt.Sprintf("cannot expand %q on %q", field, collection))
	case total > 1:
		return "", badRequest("expand",
			fmt.Sprintf("%q is ambiguous — %q relates to %q through more than one field; disambiguation is not supported yet", field, field, collection))
	}

	id := fmt.Sprint(rec["id"])
	if len(belongs) == 1 {
		list, err := s.findRelated(ctx, belongs[0], id)
		if err != nil {
			return "", err
		}
		rec[field] = list
		return belongs[0].Source, nil
	}
	list, err := s.findRelatedM2M(ctx, many[0], id)
	if err != nil {
		return "", err
	}
	rec[field] = list
	return many[0].Source, nil
}

// findRelatedM2M loads the source records linked to targetID through a reverse
// many-to-many edge: read the join rows where target_id = targetID, then batch
// fetch the source records.
func (s *Server) findRelatedM2M(ctx context.Context, inv schema.M2MInverse, targetID string) ([]store.Record, error) {
	table := schema.JoinTableName(inv.Source, inv.Field)
	linkPage, err := s.db.Find(ctx, store.Query{
		Collection: table,
		Filters:    []store.Filter{{Field: "target_id", Operator: store.Eq, Value: targetID}},
		Limit:      maxM2MExpand,
	})
	if err != nil {
		return nil, err
	}
	var ids []any
	for _, l := range linkPage.Data {
		if sid, ok := l["source_id"].(string); ok {
			ids = append(ids, sid)
		}
	}
	if len(ids) == 0 {
		return []store.Record{}, nil
	}
	page, err := s.db.Find(ctx, store.Query{
		Collection: inv.Source,
		Filters:    []store.Filter{{Field: "id", Operator: store.In, Value: ids}},
		Limit:      len(ids),
	})
	if err != nil {
		return nil, err
	}
	for _, r := range page.Data {
		s.coerceExpanded(inv.Source, r)
	}
	if page.Data == nil {
		return []store.Record{}, nil
	}
	return page.Data, nil
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
	s.coerceExpanded(target, related)
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
	for _, r := range page.Data {
		s.coerceExpanded(inv.Source, r)
	}
	if page.Data == nil {
		return []store.Record{}, nil
	}
	return page.Data, nil
}


// expandListRecords expands a page of records. Only belongs-to tokens are
// allowed on lists (has-many on a list is expensive and disallowed for now);
// belongs-to is batched — distinct ids fetched once — to avoid N+1 queries.
func (s *Server) expandListRecords(ctx context.Context, collection string, recs []store.Record, tokens []string) error {
	cd := s.collections[collection]
	for _, tok := range tokens {
		if strings.Contains(tok, ".") {
			return badRequest("expand", fmt.Sprintf("nested expansion (%q) is only supported on single-record reads, not lists", tok))
		}
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
	byID := make(map[string]store.Record, len(page.Data))
	for _, tr := range page.Data {
		s.coerceExpanded(target, tr)
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
