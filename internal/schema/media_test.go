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

	// _media is always injected, last.
	last := def.Collections[len(def.Collections)-1]
	if last.Name != MediaCollection {
		t.Fatalf("_media not injected (got %q)", last.Name)
	}
	cols := map[string]bool{}
	for _, f := range last.Fields {
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
	// Users may not declare _media.
	if _, err := Parse([]byte(`
version: "1"
collections:
  _media:
    fields:
      name: { type: string }
`)); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("declaring _media should be reserved-name error, got %v", err)
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
