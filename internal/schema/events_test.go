package schema

import "testing"

func TestEvents_InjectedOnlyWhenOptedIn(t *testing.T) {
	withEvents := `
version: "1"
collections:
  posts:
    fields:
      title: { type: string, required: true }
    events: true
`
	def, err := Parse([]byte(withEvents))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !def.AnyEvents() {
		t.Fatal("AnyEvents() should be true when a collection opts in")
	}
	if def.collection(EventsCollection) == nil {
		t.Fatalf("_events collection should be injected; collections: %v", names(def))
	}

	without := `
version: "1"
collections:
  posts:
    fields:
      title: { type: string, required: true }
`
	def2, err := Parse([]byte(without))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if def2.AnyEvents() {
		t.Fatal("AnyEvents() should be false with no opt-in")
	}
	if def2.collection(EventsCollection) != nil {
		t.Fatal("_events must not be injected when no collection opts in")
	}
}

// collection returns the CollectionDef with the given name, or nil.
func (s *SchemaDefinition) collection(name string) *CollectionDef {
	for i := range s.Collections {
		if s.Collections[i].Name == name {
			return &s.Collections[i]
		}
	}
	return nil
}

func names(def *SchemaDefinition) []string {
	out := make([]string, len(def.Collections))
	for i, c := range def.Collections {
		out[i] = c.Name
	}
	return out
}
