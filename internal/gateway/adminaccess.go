package gateway

import (
	"net/http"

	"github.com/blazing-Gael/dcms/internal/schema"
)

// Admin panel — access map (ADR-0035, phase 2 core). A read-only, schema-derived
// matrix of who may do what per collection (and per field), so an operator can see
// their authorization posture without reading YAML. Access is schema-defined, so
// this is view-only; changing it is a schema edit + redeploy.

type adminAccessRow struct {
	Collection                   string
	Read, Create, Update, Delete string
	Preview, Publish             string
	Fields                       []adminFieldAccessRow
}

type adminFieldAccessRow struct {
	Name, Read, Write string
}

func (s *Server) adminAccessMap(w http.ResponseWriter, r *http.Request) {
	var rows []adminAccessRow
	for _, c := range s.schema.Collections {
		if !s.routableCollection(c.Name) {
			continue
		}
		row := adminAccessRow{
			Collection: c.Name,
			Read:       describeRule(c.AccessRule(schema.ActionRead)),
			Create:     describeRule(c.AccessRule(schema.ActionCreate)),
			Update:     describeRule(c.AccessRule(schema.ActionUpdate)),
			Delete:     describeRule(c.AccessRule(schema.ActionDelete)),
		}
		if c.Publishing {
			row.Preview = describeRule(c.AccessRule(schema.ActionPreview))
			row.Publish = describeRule(c.AccessRule(schema.ActionPublish))
		}
		for _, f := range c.Fields {
			if f.Access == nil {
				continue
			}
			fr := adminFieldAccessRow{Name: f.Name}
			if f.Access.Read != nil {
				fr.Read = describeRule(*f.Access.Read)
			}
			if len(f.Access.WriteTransitions) > 0 {
				fr.Write = "transitions"
			} else if f.Access.Write != nil {
				fr.Write = describeRule(*f.Access.Write)
			}
			row.Fields = append(row.Fields, fr)
		}
		rows = append(rows, row)
	}
	s.renderAdmin(w, r, "access", &adminPage{Title: "Access map", Data: rows})
}

// describeRule renders one access rule as a compact, human phrase.
func describeRule(rule schema.Rule) string {
	switch rule.Kind {
	case schema.RulePublic:
		return "public"
	case schema.RuleAuthenticated:
		return "authenticated"
	case schema.RuleRoles:
		return joinComma(rule.Roles)
	case schema.RuleOwner:
		return "owner"
	case schema.RuleOwnerField:
		return "owner:" + rule.Field
	case schema.RuleInherit:
		return "inherit"
	case schema.RuleAny:
		parts := make([]string, 0, len(rule.Any))
		for _, sub := range rule.Any {
			parts = append(parts, describeRule(sub))
		}
		return joinWith(parts, " or ")
	default:
		return string(rule.Kind)
	}
}

func joinWith(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}
