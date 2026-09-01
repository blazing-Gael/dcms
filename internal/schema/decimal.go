package schema

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Decimal scale bounds (ADR-0017). The default of two fractional digits matches
// most currencies; the cap keeps the whole-number range large within int64.
const (
	DefaultDecimalScale = 2
	MaxDecimalScale     = 9
)

// DecimalScale returns the effective scale for a decimal field — the declared
// value, or DefaultDecimalScale when unset.
func (f FieldDef) DecimalScale() int {
	if f.Scale != nil {
		return *f.Scale
	}
	return DefaultDecimalScale
}

// validateDecimal checks a request value for a decimal field (ADR-0017): it must
// be an exact string (a JSON number is rejected — it is already a lossy float by
// the time it arrives), it must parse to the declared scale without silent
// rounding or int64 overflow, and it must satisfy any min/max bound. Returns ""
// when the value is acceptable.
func (f FieldDef) validateDecimal(v any) string {
	s, ok := v.(string)
	if !ok {
		return "must be a decimal string (in quotes), e.g. \"12.50\", not a number"
	}
	scale := f.DecimalScale()
	n, err := ParseDecimal(s, scale)
	if err != nil {
		return err.Error()
	}
	// Bounds are compared in minor units so the check is exact. A schema author's
	// float bound is rounded to the field's scale to form the integer threshold.
	pow := math.Pow10(scale)
	if f.Min != nil && n < int64(math.Round(*f.Min*pow)) {
		return fmt.Sprintf("must be >= %s", trimNum(*f.Min))
	}
	if f.Max != nil && n > int64(math.Round(*f.Max*pow)) {
		return fmt.Sprintf("must be <= %s", trimNum(*f.Max))
	}
	return ""
}

// EncodeDecimals rewrites every present decimal field in a validated write body
// from its exact string form to int64 minor units — the shape the store persists
// (ADR-0017). It runs after validation (which guarantees each value parses to the
// field's scale) and before the store write, in place. A value that somehow fails
// to parse is left untouched rather than silently zeroed.
func (c CollectionDef) EncodeDecimals(data map[string]any) {
	for _, f := range c.Fields {
		if f.Type != TypeDecimal {
			continue
		}
		s, ok := data[f.Name].(string)
		if !ok {
			continue
		}
		if n, err := ParseDecimal(s, f.DecimalScale()); err == nil {
			data[f.Name] = n
		}
	}
}

// ParseDecimal converts a decimal string like "12.50" into an exact integer
// count of minor units at the given scale ("12.50" at scale 2 → 1250). It rejects
// a value with more fractional digits than the scale allows — money is never
// silently rounded — and a value that overflows int64 (ADR-0017).
func ParseDecimal(s string, scale int) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("must be a decimal string, e.g. \"12.50\"")
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}

	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0" // ".5" → "0.5"
	}
	if !isDigits(intPart) || (fracPart != "" && !isDigits(fracPart)) {
		return 0, fmt.Errorf("must be a decimal number")
	}
	if len(fracPart) > scale {
		return 0, fmt.Errorf("must have at most %d decimal place(s)", scale)
	}

	// Pad the fraction out to the full scale; the whole run of digits is then an
	// integer count of minor units. ParseInt reports overflow for us.
	digits := intPart + fracPart + strings.Repeat("0", scale-len(fracPart))
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("is out of range")
	}
	if neg {
		n = -n
	}
	return n, nil
}

// FormatDecimal renders minor units back to a fixed-scale decimal string
// (1250 at scale 2 → "12.50").
func FormatDecimal(n int64, scale int) string {
	if scale <= 0 {
		return strconv.FormatInt(n, 10)
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.FormatInt(n, 10)
	for len(digits) <= scale { // ensure at least one whole digit before the point
		digits = "0" + digits
	}
	point := len(digits) - scale
	out := digits[:point] + "." + digits[point:]
	if neg {
		out = "-" + out
	}
	return out
}

// decimalMinorUnits coerces the numeric forms a store may hand back for a
// decimal (INTEGER) column into int64 minor units. Reports false for anything
// else (e.g. an already-formatted string), so the caller leaves it untouched.
func decimalMinorUnits(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case int32:
		return int64(n), true
	default:
		return 0, false
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
