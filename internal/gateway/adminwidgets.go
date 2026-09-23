package gateway

import (
	"net/http"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — 2B editing widgets (ADR-0035): human-readable relation pickers and
// value-scoped enum selects, so a non-technical client picks "Desserts", never a
// UUID, and only sees the status changes their role may actually make. Display
// labels are inferred from the schema (no config), and every option list honors the
// caller's read access so the panel never leaks a record they couldn't otherwise see.

// adminOption is one choice in a select or checklist: the stored value, its human
// label, and whether it's currently chosen.
type adminOption struct {
	Value, Label string
	Selected     bool
}

// adminLabelField picks the field to show as a record's human label, with zero
// schema config (the decision in ADR-0035 2B): the first of title/name/label/slug
// that exists as a text field, else the first string/text field, else "id".
func adminLabelField(cd schema.CollectionDef) string {
	for _, pref := range []string{"title", "name", "label", "slug"} {
		for _, f := range cd.Fields {
			if f.Name == pref && (f.Type == schema.TypeString || f.Type == schema.TypeText) {
				return f.Name
			}
		}
	}
	for _, f := range cd.Fields {
		if f.Type == schema.TypeString || f.Type == schema.TypeText {
			return f.Name
		}
	}
	return "id"
}

// adminRecordLabel renders a record's inferred label, falling back to its id so a
// dropdown option or row title is never blank.
func adminRecordLabel(cd schema.CollectionDef, rec store.Record) string {
	id, _ := rec["id"].(string)
	lf := adminLabelField(cd)
	if lf == "id" {
		return id
	}
	if s := strings.TrimSpace(adminRawString(rec[lf])); s != "" {
		return s
	}
	return id
}

// adminRelationOptions loads the choices for a belongs-to / m2m picker on the target
// collection, labeled for humans and scoped to what the caller may read. `selected`
// marks the currently-chosen target ids. Capped at adminListLimit; any selected id
// beyond the cap is kept (by raw id) so editing never silently drops a relation.
func (s *Server) adminRelationOptions(r *http.Request, target string, selected map[string]bool) []adminOption {
	tcd, ok := s.collections[target]
	if !ok {
		return nil
	}
	filters, ok := s.adminReadFilters(r, target)
	if !ok {
		return nil // caller can't read the target collection at all
	}
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: target, Filters: filters, Limit: adminListLimit, Sort: "-created_at",
	})
	if err != nil {
		return nil
	}
	opts := make([]adminOption, 0, len(page.Data))
	seen := map[string]bool{}
	for _, rec := range page.Data {
		tcd.CoerceResponse(rec)
		id, _ := rec["id"].(string)
		if id == "" {
			continue
		}
		seen[id] = true
		opts = append(opts, adminOption{Value: id, Label: adminRecordLabel(tcd, rec), Selected: selected[id]})
	}
	for id := range selected {
		if id != "" && !seen[id] {
			opts = append(opts, adminOption{Value: id, Label: id, Selected: true})
		}
	}
	return opts
}

// adminM2MSelected returns the target ids currently linked to a record for a
// many-to-many field, read straight from the join table.
func (s *Server) adminM2MSelected(r *http.Request, collection, field, sourceID string) map[string]bool {
	out := map[string]bool{}
	if sourceID == "" {
		return out
	}
	table := schema.JoinTableName(collection, field)
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: table,
		Filters:    []store.Filter{{Field: "source_id", Operator: store.Eq, Value: sourceID}},
		Limit:      adminListLimit,
	})
	if err != nil {
		return out
	}
	for _, l := range page.Data {
		if tid, ok := l["target_id"].(string); ok && tid != "" {
			out[tid] = true
		}
	}
	return out
}

// adminEnumOptions builds the choices for an enum select. When the field declares
// value-scoped write rules (#27), only the transitions the caller may actually make
// from the current value are offered (plus the current value itself), so the panel
// can never lead a non-technical user into a 403. `rec` is the record being edited
// (nil on create), used for the current value and any owner-scoped `who` check.
func (s *Server) adminEnumOptions(f schema.FieldDef, p principal, rec store.Record, isCreate bool) []adminOption {
	curVal, _ := rec[f.Name].(string)
	gated := s.authEnabled() && f.Access != nil && len(f.Access.WriteTransitions) > 0
	var opts []adminOption
	for _, v := range f.Values {
		if gated && v != curVal {
			if s.evalTransition(f.Access.WriteTransitions, p, rec, curVal, v, isCreate) != transitionAllowed {
				continue
			}
		}
		opts = append(opts, adminOption{Value: v, Label: v, Selected: v == curVal})
	}
	return opts
}
