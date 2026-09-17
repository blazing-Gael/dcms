package schema

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FieldErrors maps a field name to a human-readable validation message.
// It is empty/nil when a record is valid.
type FieldErrors map[string]string

// ValidateCreate validates a record being created: required fields must be
// present (unless they have a default), and every supplied value must satisfy
// its field's type and constraints.
func (c CollectionDef) ValidateCreate(data map[string]any) FieldErrors {
	return c.validateRecord(data, true)
}

// ValidateUpdate validates a partial (PATCH) update: required is NOT enforced
// (absent fields are simply untouched), but every supplied value is still
// validated against its field's type and constraints.
func (c CollectionDef) ValidateUpdate(data map[string]any) FieldErrors {
	return c.validateRecord(data, false)
}

func (c CollectionDef) validateRecord(data map[string]any, isCreate bool) FieldErrors {
	errs := FieldErrors{}

	byName := make(map[string]FieldDef, len(c.Fields))
	for _, f := range c.Fields {
		byName[f.Name] = f
	}

	// Reject keys that aren't declared fields. "id" is always permitted (client
	// may supply it on create; the gateway sets it from the URL on update).
	// Engine-managed columns are not accepted as input.
	for k := range data {
		if k == "id" {
			continue
		}
		if _, ok := byName[k]; !ok {
			errs[k] = "unknown field"
		}
	}

	for _, f := range c.Fields {
		v, present := data[f.Name]

		if !present || v == nil {
			// Missing on create with no default → required violation.
			if isCreate && f.Required && f.Default == nil {
				errs[f.Name] = "is required"
			}
			continue
		}

		// A many-to-many relation value is a list of target ids.
		if f.Type == TypeRelation && f.Many {
			arr, ok := v.([]any)
			if !ok {
				errs[f.Name] = "must be a list of ids"
				continue
			}
			for _, e := range arr {
				if _, ok := e.(string); !ok {
					errs[f.Name] = "must be a list of string ids"
					break
				}
			}
			continue
		}

		// A richtext value is a structured document (ADR-0014). Validate its shape
		// against the field's allowlists here; in-content reference existence is
		// checked at the gateway (batched), like relation ids.
		if f.Type == TypeRichText {
			if msg := f.validateRichText(v); msg != "" {
				errs[f.Name] = msg
			}
			continue
		}

		// An object_list value is a JSON array of small records, each matching the
		// field's `of` shape (issue #6). The whole list is replace-on-write, so each
		// element is validated as a complete record; inner relation/file existence is
		// checked at the gateway (batched), like top-level and richtext references.
		if f.Type == TypeObjectList {
			if msg := f.validateObjectList(v); msg != "" {
				errs[f.Name] = msg
			}
			continue
		}

		// A decimal crosses the wire as an exact string, never a JSON number
		// (ADR-0017): a number would already be a lossy float by the time it
		// arrives. Reject numbers explicitly, and validate the string parses to
		// the declared scale with no silent rounding and no int64 overflow.
		if f.Type == TypeDecimal {
			if msg := f.validateDecimal(v); msg != "" {
				errs[f.Name] = msg
			}
			continue
		}

		if !typeMatches(f.Type, v) {
			errs[f.Name] = "must be of type " + string(f.Type)
			continue
		}

		if f.Type == TypeEnum {
			s, _ := v.(string)
			if !containsString(f.Values, s) {
				errs[f.Name] = "must be one of: " + strings.Join(f.Values, ", ")
				continue
			}
		}

		if msg := constraintError(f, v); msg != "" {
			errs[f.Name] = msg
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// validateObjectList checks an object_list value: a JSON array within the field's
// min/max length, whose every element is an object matching the `of` shape. It
// reuses validateRecord for each element (create semantics — a supplied list fully
// specifies its elements) so the inner fields get the exact same type and
// constraint checks as top-level ones. A `_key` string per element is accepted (it
// carries per-element identity for admin-UI reordering) but not required. Inner
// relation/file existence is verified at the gateway, not here. Returns "" if valid.
func (f FieldDef) validateObjectList(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return "must be a list of objects"
	}
	n := float64(len(arr))
	if f.Min != nil && n < *f.Min {
		return fmt.Sprintf("must have at least %s items", trimNum(*f.Min))
	}
	if f.Max != nil && n > *f.Max {
		return fmt.Sprintf("must have at most %s items", trimNum(*f.Max))
	}
	elemDef := CollectionDef{Fields: f.Of}
	for i, e := range arr {
		elem, ok := e.(map[string]any)
		if !ok {
			return fmt.Sprintf("item %d must be an object", i)
		}
		// Split off the optional per-element key; the rest is validated as a record.
		var body map[string]any
		if _, hasKey := elem[objectListKey]; hasKey {
			if _, isStr := elem[objectListKey].(string); !isStr {
				return fmt.Sprintf("item %d: %s must be a string", i, objectListKey)
			}
			body = make(map[string]any, len(elem))
			for k, val := range elem {
				if k != objectListKey {
					body[k] = val
				}
			}
		} else {
			body = elem
		}
		if fe := elemDef.validateRecord(body, true); fe != nil {
			return fmt.Sprintf("item %d: %s", i, fe.oneLine())
		}
	}
	return ""
}

// oneLine renders field errors as a single deterministic message, for embedding a
// nested (object_list element) failure into its parent field's error.
func (e FieldErrors) oneLine() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + " " + e[k]
	}
	return strings.Join(parts, "; ")
}

// typeMatches reports whether a Go value (as produced by JSON decoding) is
// compatible with the field's declared type.
func typeMatches(t FieldType, v any) bool {
	switch t {
	case TypeString, TypeText, TypeEnum, TypeDate, TypeDateTime, TypeRelation, TypeDecimal:
		// A relation value is the target record's id (a string); a decimal is an
		// exact string on the wire and (after CoerceResponse) in a response.
		_, ok := v.(string)
		return ok
	case TypeBoolean:
		_, ok := v.(bool)
		return ok
	case TypeNumber:
		_, ok := toFloat(v)
		return ok
	case TypeInteger:
		f, ok := toFloat(v)
		return ok && f == math.Trunc(f)
	case TypeJSON:
		return true // any JSON value is acceptable
	case TypeObjectList:
		_, ok := v.([]any) // a decoded JSON array (issue #6)
		return ok
	default:
		return true
	}
}

// constraintError checks min/max, pattern, and date parseability. Returns "" if
// the value is fine. Assumes typeMatches has already passed.
func constraintError(f FieldDef, v any) string {
	switch f.Type {
	case TypeString, TypeText:
		s := v.(string)
		n := float64(len([]rune(s)))
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("must be at least %s characters", trimNum(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("must be at most %s characters", trimNum(*f.Max))
		}
		if f.Pattern != "" {
			if re, err := regexp.Compile(f.Pattern); err == nil && !re.MatchString(s) {
				return "has an invalid format"
			}
		}
	case TypeNumber, TypeInteger:
		n, _ := toFloat(v)
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("must be >= %s", trimNum(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("must be <= %s", trimNum(*f.Max))
		}
	case TypeDate:
		if _, err := time.Parse("2006-01-02", v.(string)); err != nil {
			return "must be a date (YYYY-MM-DD)"
		}
	case TypeDateTime:
		if _, err := time.Parse(time.RFC3339, v.(string)); err != nil {
			return "must be an RFC3339 datetime"
		}
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// trimNum renders a float bound in its shortest form (e.g. 200, 0.5).
func trimNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
