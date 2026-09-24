package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — 2C object_list editor (ADR-0035, issue #6). A repeatable group of
// fields, edited as a list of rows: each row renders the inner field widgets, and a
// small served script (object_list.js) clones a hidden template row to add and
// removes a row on demand. Existing rows still render and edit with JS off; only
// add/remove needs it. On submit the array is reconstructed from indexed field
// names (`<field>[<i>].<sub>`), so it round-trips through the normal write pipeline.

// adminObjectListData is one object_list field's editor: its existing rows, a blank
// template row (with an __IDX__ placeholder the script rewrites), and the count so
// the script numbers new rows without colliding.
type adminObjectListData struct {
	Name, Label, Help string
	Rows              [][]adminField
	Template          []adminField
	Count             int
}

// objectListPlaceholder is the row index the client-side script substitutes when it
// clones the template row.
const objectListPlaceholder = "__IDX__"

// adminObjectLists builds the editors for a collection's object_list fields. `rec`
// supplies stored rows on edit; `submitted` (an error re-render) takes precedence.
func (s *Server) adminObjectLists(r *http.Request, cd schema.CollectionDef, rec, submitted store.Record) []adminObjectListData {
	src := rec
	if submitted != nil {
		src = submitted
	}
	var out []adminObjectListData
	for _, f := range cd.Fields {
		if f.Type != schema.TypeObjectList {
			continue
		}
		od := adminObjectListData{Name: f.Name, Label: adminLabel(f), Help: adminObjectListHelp(f)}
		elems := adminObjectListValue(src, f.Name)
		for i, elem := range elems {
			od.Rows = append(od.Rows, s.adminObjectRow(r, f, strconv.Itoa(i), elem))
		}
		od.Count = len(elems)
		od.Template = s.adminObjectRow(r, f, objectListPlaceholder, nil)
		out = append(out, od)
	}
	return out
}

// adminObjectRow builds the inner field widgets for one row, named
// `<field>[<idx>].<sub>`. `idx` is a number for a real row or the placeholder for the
// template. `elem` supplies current values (nil for a blank/template row).
func (s *Server) adminObjectRow(r *http.Request, f schema.FieldDef, idx string, elem map[string]any) []adminField {
	row := make([]adminField, 0, len(f.Of))
	for _, sub := range f.Of {
		name := f.Name + "[" + idx + "]." + sub.Name
		var val any
		if elem != nil {
			val = elem[sub.Name]
		}
		row = append(row, s.adminObjectInnerField(r, sub, name, val))
	}
	return row
}

// adminObjectInnerField renders one inner field of an object_list element. It covers
// the inner types issue #6 allows: scalars, enum, and a single relation (rendered as
// a label picker, media included). Inline file upload inside a row is not supported —
// a media inner field picks from the library.
func (s *Server) adminObjectInnerField(r *http.Request, sub schema.FieldDef, name string, val any) adminField {
	af := adminField{Name: name, Label: adminLabel(sub), Value: adminRawString(val), Required: sub.Required, Widget: "input", InputType: "text"}
	switch sub.Type {
	case schema.TypeText:
		af.Widget = "textarea"
	case schema.TypeBoolean:
		af.Widget = "checkbox"
	case schema.TypeNumber, schema.TypeInteger:
		af.InputType = "number"
	case schema.TypeDate:
		af.InputType = "date"
	case schema.TypeEnum:
		af.Widget = "select"
		af.Options = markSelected(enumOptions(sub.Values), adminRawString(val))
	case schema.TypeRelation:
		af.Widget = "select"
		sel := map[string]bool{}
		if v := strings.TrimSpace(adminRawString(val)); v != "" {
			sel[v] = true
		}
		af.Options = s.adminRelationOptions(r, sub.Target, sel)
	}
	return af
}

// enumOptions turns a value list into plain (value == label) options.
func enumOptions(values []string) []adminOption {
	opts := make([]adminOption, 0, len(values))
	for _, v := range values {
		opts = append(opts, adminOption{Value: v, Label: v})
	}
	return opts
}

// adminObjectListHelp summarizes the row constraints for the editor header.
func adminObjectListHelp(f schema.FieldDef) string {
	switch {
	case f.Min != nil && f.Max != nil:
		return fmt.Sprintf("%s–%s items", trimF(*f.Min), trimF(*f.Max))
	case f.Min != nil:
		return "at least " + trimF(*f.Min) + " items"
	case f.Max != nil:
		return "up to " + trimF(*f.Max) + " items"
	}
	return ""
}

func trimF(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// adminObjectListValue reads a stored/submitted object_list value into a slice of
// element maps (the JSON column surfaces as []any of map[string]any).
func adminObjectListValue(src store.Record, name string) []map[string]any {
	if src == nil {
		return nil
	}
	arr, ok := src[name].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// adminParseObjectLists reconstructs each object_list field's array from the indexed
// form fields, coercing inner values by type. A row whose every field is empty is
// dropped (blank/template rows), so validation sees only real items. The field is
// only written when the form actually rendered its editor (a hidden marker), so a
// form without the section never clears it.
func (s *Server) adminParseObjectLists(cd schema.CollectionDef, r *http.Request, data store.Record) {
	for _, f := range cd.Fields {
		if f.Type != schema.TypeObjectList {
			continue
		}
		if r.FormValue("__ol_"+f.Name) != "1" {
			continue // this form didn't render the editor → leave the field untouched
		}
		indices := objectListIndices(r, f.Name)
		arr := make([]any, 0, len(indices))
		for _, i := range indices {
			elem := map[string]any{}
			empty := true
			for _, sub := range f.Of {
				key := fmt.Sprintf("%s[%d].%s", f.Name, i, sub.Name)
				switch sub.Type {
				case schema.TypeBoolean:
					elem[sub.Name] = r.FormValue(key) == "true"
				case schema.TypeNumber, schema.TypeInteger:
					raw := strings.TrimSpace(r.FormValue(key))
					if raw == "" {
						continue
					}
					empty = false
					if n, err := strconv.ParseFloat(raw, 64); err == nil {
						elem[sub.Name] = n
					} else {
						elem[sub.Name] = raw // let validation report the bad number
					}
				default:
					raw := strings.TrimSpace(r.FormValue(key))
					if raw == "" {
						continue
					}
					empty = false
					elem[sub.Name] = raw
				}
			}
			if !empty {
				arr = append(arr, elem)
			}
		}
		data[f.Name] = arr
	}
}

// objectListIndices returns the sorted, de-duplicated row indices present in the form
// for one object_list field (from keys like `field[3].sub`).
func objectListIndices(r *http.Request, field string) []int {
	prefix := field + "["
	seen := map[int]bool{}
	for key := range r.Form {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := key[len(prefix):]
		end := strings.IndexByte(rest, ']')
		if end <= 0 {
			continue
		}
		if n, err := strconv.Atoi(rest[:end]); err == nil {
			seen[n] = true
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
