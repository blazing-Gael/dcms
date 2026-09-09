package schema

import (
	"strings"
	"testing"
)

func TestMedia_FileFieldBecomesRelationToMedia(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  products:
    fields:
      name:    { type: string, required: true }
      image:   { type: file }
      gallery: { type: file, many: true }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// _media is always injected (after user collections, before identity ones).
	var media *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == MediaCollection {
			media = &def.Collections[i]
		}
	}
	if media == nil {
		t.Fatalf("_media not injected")
	}
	cols := map[string]bool{}
	for _, f := range media.Fields {
		cols[f.Name] = true
	}
	for _, want := range []string{MediaFilename, MediaContentType, MediaSize, MediaStorageKey} {
		if !cols[want] {
			t.Errorf("_media missing field %q", want)
		}
	}

	products := def.Collections[0]
	image := products.Fields[1]
	if image.Type != TypeRelation || image.Target != MediaCollection || image.Many {
		t.Errorf("image should be a belongs-to relation to _media, got %#v", image)
	}
	gallery := products.Fields[2]
	if gallery.Type != TypeRelation || gallery.Target != MediaCollection || !gallery.Many {
		t.Errorf("gallery should be a many-to-many relation to _media, got %#v", gallery)
	}

	// belongs-to file → FK column on products referencing _media.
	meta := products.ToCollectionMeta()
	found := false
	for _, c := range meta.Columns {
		if c.Name == "image" {
			found = true
			if c.References != MediaCollection {
				t.Errorf("image column should reference _media, got %q", c.References)
			}
		}
	}
	if !found {
		t.Fatal("image FK column not generated")
	}

	// m2m file → join table products_gallery targeting _media.
	joinFound := false
	for _, m := range def.CollectionMetas() {
		if m.Name == "products_gallery" {
			joinFound = true
		}
	}
	if !joinFound {
		t.Fatal("gallery join table not generated")
	}
}

func TestMedia_ReservedAndImplicitTarget(t *testing.T) {
	// _media is engine-managed: users may not add fields to it.
	if _, err := Parse([]byte(`
version: "1"
collections:
  _media:
    fields:
      name: { type: string }
`)); err == nil || !strings.Contains(err.Error(), "engine-managed") {
		t.Fatalf("declaring _media fields should error, got %v", err)
	}

	// But naming it to attach an `access:` block is allowed — the only way to
	// make the media library anything but the default public-read (ADR-0011).
	def, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  _media:
    access:
      read: [admin]
`))
	if err != nil {
		t.Fatalf("declaring _media access should be allowed, got %v", err)
	}
	var media []CollectionDef
	for _, c := range def.Collections {
		if c.Name == MediaCollection {
			media = append(media, c)
		}
	}
	if len(media) != 1 {
		t.Fatalf("expected exactly one _media collection, got %d", len(media))
	}
	// The declared policy is carried onto the engine's definition, and the
	// engine's own fields survive rather than being replaced by the placeholder.
	rule := media[0].AccessRule(ActionRead)
	if rule.Kind != RuleRoles || len(rule.Roles) != 1 || rule.Roles[0] != "admin" {
		t.Fatalf("declared _media read rule not applied: %#v", rule)
	}
	if _, ok := media[0].field(MediaStorageKey); !ok {
		t.Fatalf("engine-managed _media fields were lost")
	}

	// A file field must not set an explicit target.
	if _, err := Parse([]byte(`
version: "1"
collections:
  products:
    fields:
      image: { type: file, target: something }
`)); err == nil || !strings.Contains(err.Error(), "do not set 'target'") {
		t.Fatalf("file with explicit target should error, got %v", err)
	}
}
