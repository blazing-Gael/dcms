package schema

import (
	"slices"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/store"
)

// base is a valid enrollment-gated schema; each test mutates one piece to assert a
// specific validation (ADR-0036).
const existsBase = `
version: "1"
collections:
  courses:
    fields:
      title: { type: string }
  enrollments:
    fields:
      user:   { type: relation, target: _users }
      course: { type: relation, target: courses }
      status: { type: enum, values: [active, cancelled] }
  lessons:
    fields:
      course: { type: relation, target: courses }
    access:
      read:
        exists:
          in: enrollments
          match: { course: course }
          principal: user
          where: { status: active }
`

func TestExists_ValidSchemaParses(t *testing.T) {
	if _, err := Parse([]byte(existsBase)); err != nil {
		t.Fatalf("valid exists schema should parse, got: %v", err)
	}
}

func TestExists_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		rule string
		want string
	}{
		{
			name: "undeclared in collection",
			rule: `{ in: nope, match: { course: course }, principal: user }`,
			want: `exists.in "nope" is not a declared collection`,
		},
		{
			name: "match this not a relation",
			rule: `{ in: enrollments, match: { course: title }, principal: user }`,
			want: `exists.match "title" must be a belongs-to relation on this collection`,
		},
		{
			name: "match in not a relation",
			rule: `{ in: enrollments, match: { status: course }, principal: user }`,
			want: `exists.match "status" must be a belongs-to relation on "enrollments"`,
		},
		{
			name: "principal not relation to _users",
			rule: `{ in: enrollments, match: { course: course }, principal: course }`,
			want: `must relate to _users`,
		},
		{
			name: "where unknown field",
			rule: `{ in: enrollments, match: { course: course }, principal: user, where: { nope: 1 } }`,
			want: `exists.where "nope" is not a field of "enrollments"`,
		},
		{
			name: "missing in",
			rule: `{ match: { course: course }, principal: user }`,
			want: "`in` (the membership collection) is required",
		},
		{
			name: "missing principal",
			rule: `{ in: enrollments, match: { course: course } }`,
			want: "`principal`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(existsBase, `exists:
          in: enrollments
          match: { course: course }
          principal: user
          where: { status: active }`, `exists: `+tc.rule, 1)
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatalf("want error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// match sides must point at the same entity.
func TestExists_MatchDifferentTargetsRejected(t *testing.T) {
	src := `
version: "1"
collections:
  courses:
    fields: { title: { type: string } }
  rooms:
    fields: { name: { type: string } }
  enrollments:
    fields:
      user: { type: relation, target: _users }
      room: { type: relation, target: rooms }
  lessons:
    fields:
      course: { type: relation, target: courses }
    access:
      read:
        exists: { in: enrollments, match: { room: course }, principal: user }
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "different collections") {
		t.Fatalf("want a different-collections error, got: %v", err)
	}
}

// The membership rule must auto-declare the supporting index on (principal, match_in).
func TestExists_AutoIndexOnMembershipCollection(t *testing.T) {
	def, err := Parse([]byte(existsBase))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var enr store.CollectionMeta
	found := false
	for _, m := range def.CollectionMetas() {
		if m.Name == "enrollments" {
			enr, found = m, true
		}
	}
	if !found {
		t.Fatal("no enrollments meta")
	}
	wantCols := []string{"user", "course"}
	has := false
	for _, idx := range enr.Indexes {
		if slices.Equal(idx.Columns, wantCols) {
			has = true
		}
	}
	if !has {
		t.Fatalf("want an auto index on %v, got indexes %+v", wantCols, enr.Indexes)
	}
}
