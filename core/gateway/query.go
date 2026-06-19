package gateway

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/blazing-Gael/dcms/core/schema"
	"github.com/blazing-Gael/dcms/core/store"
)

// filterKeyRe matches list filter params: filter[field] or filter[field][op].
var filterKeyRe = regexp.MustCompile(`^filter\[([a-z0-9_]+)\](?:\[([a-z_]+)\])?$`)

// validOps is the set of filter operators the API accepts.
var validOps = map[string]store.Op{
	"eq": store.Eq, "ne": store.Ne, "gt": store.Gt, "gte": store.Gte,
	"lt": store.Lt, "lte": store.Lte, "contains": store.Contains,
	"starts_with": store.StartsWith, "in": store.In, "nin": store.NotIn,
}

// badRequest builds a validation error tied to one query parameter, so it maps
// to a 422 with a helpful field message.
func badRequest(param, msg string) error {
	return &store.ValidationError{Fields: map[string]string{param: msg}}
}

// parseListQuery turns the request's URL query into a store.Query, validating
// field names against the collection's schema and coercing filter values to the
// field's type (so filters behave correctly across adapters, not just SQLite).
func (s *Server) parseListQuery(values url.Values, collection string) (store.Query, error) {
	q := store.Query{Collection: collection}

	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return q, badRequest("limit", "must be a non-negative integer")
		}
		q.Limit = n
	}

	q.Cursor = values.Get("cursor")

	if raw := values.Get("sort"); raw != "" {
		field := strings.TrimPrefix(raw, "-")
		if !s.hasColumn(collection, field) {
			return q, badRequest("sort", fmt.Sprintf("unknown field %q", field))
		}
		q.Sort = raw
	}

	if raw := values.Get("fields"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(name)
			if !s.hasColumn(collection, name) {
				return q, badRequest("fields", fmt.Sprintf("unknown field %q", name))
			}
			q.Fields = append(q.Fields, name)
		}
	}

	filters, err := s.parseFilters(values, collection)
	if err != nil {
		return q, err
	}
	q.Filters = filters

	return q, nil
}

func (s *Server) parseFilters(values url.Values, collection string) ([]store.Filter, error) {
	var filters []store.Filter
	for key, vals := range values {
		m := filterKeyRe.FindStringSubmatch(key)
		if m == nil {
			continue // not a filter param
		}
		field, opStr := m[1], m[2]
		if opStr == "" {
			opStr = "eq"
		}
		op, ok := validOps[opStr]
		if !ok {
			return nil, badRequest(key, fmt.Sprintf("unknown operator %q", opStr))
		}
		ft, known := s.fieldType(collection, field)
		if !known {
			return nil, badRequest(key, fmt.Sprintf("unknown field %q", field))
		}

		raw := vals[0]
		var value any
		if op == store.In || op == store.NotIn {
			parts := strings.Split(raw, ",")
			list := make([]any, 0, len(parts))
			for _, p := range parts {
				list = append(list, coerce(ft, strings.TrimSpace(p)))
			}
			value = list
		} else {
			value = coerce(ft, raw)
		}

		filters = append(filters, store.Filter{Field: field, Operator: op, Value: value})
	}
	return filters, nil
}

// coerce converts a string query value to the Go type implied by the field's
// schema type. Falls back to the raw string when parsing fails or the type is
// text-like (the store/DB then compares as text).
func coerce(t schema.FieldType, raw string) any {
	switch t {
	case schema.TypeNumber:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
	case schema.TypeInteger:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	case schema.TypeBoolean:
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	}
	return raw
}
