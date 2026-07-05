package schema

import "testing"

func TestLifecycle_ManagedColumnsInjected(t *testing.T) {
	src := `
version: "1"
collections:
  articles:
    publishing: true
    soft_delete: true
    fields:
      title: { type: string, required: true }
  tags:
    fields:
      name: { type: string }
`
	def, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	find := func(collection string) map[string]struct {
		nullable bool
		def      any
	} {
		for _, m := range def.CollectionMetas() {
			if m.Name == collection {
				out := map[string]struct {
					nullable bool
					def      any
				}{}
				for _, c := range m.Columns {
					out[c.Name] = struct {
						nullable bool
						def      any
					}{c.Nullable, c.Default}
				}
				return out
			}
		}
		t.Fatalf("collection %q not found in metas", collection)
		return nil
	}

	// articles opted in → managed columns present, with the right shape.
	art := find("articles")
	st, ok := art[LifecycleStatus]
	if !ok {
		t.Fatal("articles missing _status")
	}
	if st.nullable {
		t.Error("_status should be NOT NULL")
	}
	if st.def != StatusDraft {
		t.Errorf("_status default = %#v, want %q", st.def, StatusDraft)
	}
	if pa, ok := art[LifecyclePublishedAt]; !ok || !pa.nullable {
		t.Error("_published_at should be present and nullable")
	}
	if da, ok := art[LifecycleDeletedAt]; !ok || !da.nullable {
		t.Error("_deleted_at should be present and nullable")
	}

	// tags didn't opt in → no managed columns.
	tags := find("tags")
	for _, n := range []string{LifecycleStatus, LifecyclePublishedAt, LifecycleDeletedAt} {
		if _, ok := tags[n]; ok {
			t.Errorf("tags should not have %q (no lifecycle directive)", n)
		}
	}
}

func TestLifecycle_ReservedColumnsRejected(t *testing.T) {
	for _, name := range []string{"_status", "_published_at", "_deleted_at"} {
		src := `
version: "1"
collections:
  articles:
    fields:
      ` + name + `: { type: string }
`
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("declaring %q should be rejected as reserved", name)
		}
	}
}
