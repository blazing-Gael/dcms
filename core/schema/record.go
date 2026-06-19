package schema

import (
	"fmt"
	"math"
	"regexp"
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

// typeMatches reports whether a Go value (as produced by JSON decoding) is
// compatible with the field's declared type.
func typeMatches(t FieldType, v any) bool {
	switch t {
	case TypeString, TypeText, TypeEnum, TypeDate, TypeDateTime:
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
