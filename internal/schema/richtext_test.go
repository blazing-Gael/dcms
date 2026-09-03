package schema

import "testing"

// richField is a richtext field with explicit allowlists for deterministic tests.
func richField() FieldDef {
	return FieldDef{
		Name:   "body",
		Type:   TypeRichText,
		Styles: []string{"normal", "h2", "blockquote"},
		Marks:  []string{"strong", "em", "link", "reference"},
		Blocks: []string{"image", "reference"},
	}
}

// block builds a text block with one span.
func block(style, text string, marks ...string) map[string]any {
	m := make([]any, len(marks))
	for i, s := range marks {
		m[i] = s
	}
	return map[string]any{
		"_type": "block", "style": style,
		"children": []any{map[string]any{"_type": "span", "text": text, "marks": m}},
	}
}

func TestRichText_ValidDocument(t *testing.T) {
	f := richField()
	doc := []any{
		block("h2", "Title", "strong"),
		map[string]any{
			"_type": "block", "style": "normal",
			"markDefs": []any{map[string]any{"_key": "l1", "_type": "link", "href": "https://example.com"}},
			"children": []any{map[string]any{"_type": "span", "text": "click", "marks": []any{"l1", "em"}}},
		},
		map[string]any{"_type": "image", "ref": "med_1", "alt": "a photo"},
	}
	if msg := f.validateRichText(doc); msg != "" {
		t.Fatalf("expected valid, got %q", msg)
	}
}

func TestRichText_RejectsNonArray(t *testing.T) {
	if msg := richField().validateRichText("just a string"); msg == "" {
		t.Fatal("a plain string is not a rich-content document")
	}
}

func TestRichText_DisallowedStyleMarkBlock(t *testing.T) {
	f := richField()
	if msg := f.validateRichText([]any{block("h1", "x")}); msg == "" {
		t.Fatal("h1 is not in the field's allowed styles")
	}
	if msg := f.validateRichText([]any{block("normal", "x", "underline")}); msg == "" {
		t.Fatal("underline is not an allowed mark")
	}
	if msg := f.validateRichText([]any{map[string]any{"_type": "code", "code": "x()"}}); msg == "" {
		t.Fatal("code block is not in the field's allowed blocks")
	}
}

func TestRichText_UndeclaredMarkKey(t *testing.T) {
	// A span mark that is neither a decorator nor a declared markDef key is invalid.
	f := richField()
	doc := []any{map[string]any{
		"_type": "block", "style": "normal",
		"children": []any{map[string]any{"_type": "span", "text": "x", "marks": []any{"ghost"}}},
	}}
	if msg := f.validateRichText(doc); msg == "" {
		t.Fatal("a mark referencing a non-existent markDef must be rejected")
	}
}

func TestRichText_UnsafeHref(t *testing.T) {
	f := richField()
	doc := []any{map[string]any{
		"_type": "block", "style": "normal",
		"markDefs": []any{map[string]any{"_key": "x", "_type": "link", "href": "javascript:alert(1)"}},
		"children": []any{map[string]any{"_type": "span", "text": "x", "marks": []any{"x"}}},
	}}
	if msg := f.validateRichText(doc); msg == "" {
		t.Fatal("a javascript: href must be rejected")
	}
}

func TestRichText_MissingRequiredBlockFields(t *testing.T) {
	f := richField()
	if msg := f.validateRichText([]any{map[string]any{"_type": "image"}}); msg == "" {
		t.Fatal("an image block without a ref must be rejected")
	}
	if msg := f.validateRichText([]any{map[string]any{"_type": "reference", "ref": "x"}}); msg == "" {
		t.Fatal("a reference block without a collection must be rejected")
	}
}

func TestRichText_SafeHrefVariants(t *testing.T) {
	cases := map[string]bool{
		"https://a.com":       true,
		"http://a.com":        true,
		"mailto:x@a.com":      true,
		"tel:+123":            true,
		"/relative/path":      true,
		"../up":               true,
		"page#anchor":         true, // relative, no scheme
		"javascript:alert(1)": false,
		"data:text/html,x":    false,
		"vbscript:x":          false,
		"//evil.example":      false, // protocol-relative: cross-origin, not relative
		"//evil.example/path": false,
	}
	for href, want := range cases {
		if got := safeHref(href); got != want {
			t.Errorf("safeHref(%q) = %v, want %v", href, got, want)
		}
	}
}

// TestRichText_NodeCapWithinBlock pins issue #3: the node cap must hold when the
// overage sits inside a single block, not only across blocks. The old check ran
// at the top of the block loop, so whatever accumulated in the final block was
// never tested.
func TestRichText_NodeCapWithinBlock(t *testing.T) {
	spans := func(n int) map[string]any {
		children := make([]any, n)
		for i := range children {
			children[i] = map[string]any{"_type": "span", "text": "x"}
		}
		return map[string]any{"_type": "block", "style": "normal", "children": children}
	}
	// One block whose span count exceeds the cap must be rejected.
	if msg := richField().validateRichText([]any{spans(maxRichTextNodes + 1)}); msg == "" {
		t.Fatal("expected over-cap single block to be rejected, got no error")
	}
	// A document comfortably under the cap must still validate.
	if msg := richField().validateRichText([]any{spans(10), spans(10)}); msg != "" {
		t.Fatalf("under-cap document rejected: %s", msg)
	}
}

func TestRichText_Refs(t *testing.T) {
	f := richField()
	doc := []any{
		map[string]any{"_type": "image", "ref": "med_1"},
		map[string]any{"_type": "reference", "collection": "posts", "ref": "post_1"},
		map[string]any{
			"_type": "block", "style": "normal",
			"markDefs": []any{map[string]any{"_key": "r", "_type": "reference", "collection": "authors", "ref": "auth_1"}},
			"children": []any{map[string]any{"_type": "span", "text": "x", "marks": []any{"r"}}},
		},
	}
	refs := f.RichTextRefs(doc)
	if len(refs) != 3 {
		t.Fatalf("expected 3 harvested refs, got %d: %+v", len(refs), refs)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.ID] = r.Collection
	}
	if got["med_1"] != MediaCollection || got["post_1"] != "posts" || got["auth_1"] != "authors" {
		t.Fatalf("unexpected ref targets: %+v", got)
	}
}

func TestRichText_ConfigValidation(t *testing.T) {
	bad := FieldDef{Name: "body", Type: TypeRichText, Marks: []string{"strong", "bogus"}, Blocks: []string{"image", "widget"}}
	issues := bad.validateRichTextConfig()
	if len(issues) != 2 {
		t.Fatalf("expected 2 config issues (bad mark + bad block), got %v", issues)
	}
	good := richField()
	if issues := good.validateRichTextConfig(); len(issues) != 0 {
		t.Fatalf("expected no config issues, got %v", issues)
	}
}

// A schema declaring a richtext field with a bad mark must fail to compile.
func TestRichText_SchemaValidationRejectsBadConfig(t *testing.T) {
	src := []byte(`
version: "1"
collections:
  pages:
    fields:
      body: { type: richtext, marks: [strong, nope] }
`)
	if _, err := Parse(src); err == nil {
		t.Fatal("expected schema validation to reject an unknown mark")
	}
}

// A richtext field maps to a JSON column (no new store type).
func TestRichText_TranslatesToJSONColumn(t *testing.T) {
	c := CollectionDef{Name: "pages", Fields: []FieldDef{richField()}}
	meta := c.ToCollectionMeta()
	for _, col := range meta.Columns {
		if col.Name == "body" {
			if col.Type != string(TypeJSON) {
				t.Fatalf("richtext column should be json, got %q", col.Type)
			}
			return
		}
	}
	t.Fatal("body column not found")
}
