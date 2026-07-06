package schema

import (
	"fmt"
	"strings"
)

// Rich content (ADR-0014). A richtext value is a structured, portable-text-style
// document: a JSON array of nodes. This file owns the document model — the known
// node/mark/block vocabulary, the per-field allowlist defaults, the structural
// validator, and the harvesting of in-content references — so the gateway (value
// validation, batched reference checks) and codegen all agree on one shape.

// Node/annotation/block vocabulary the engine understands. Anything outside these
// sets is rejected at schema-compile time (for the field's allowlists) or at
// request time (for a document's contents).
const (
	// blockType is the core text block; always allowed regardless of a field's
	// `blocks` list (which gates only the custom, non-text block types below).
	blockType = "block"
	spanType  = "span"
)

// knownDecorators are the inline marks that need no definition (formatting).
var knownDecorators = map[string]bool{
	"strong": true, "em": true, "code": true, "underline": true, "strike": true,
}

// knownAnnotations are mark/block kinds that carry data (a markDef or a node):
// a link has an href; a reference points at a record.
var knownAnnotations = map[string]bool{
	"link": true, "reference": true,
}

// knownBlocks are the custom (non-text) block `_type`s a field may allow.
var knownBlocks = map[string]bool{
	"image": true, "code": true, "embed": true, "reference": true,
}

// Default per-field allowlists, used when a richtext field omits the list.
var (
	defaultStyles = []string{"normal", "h1", "h2", "h3", "h4", "blockquote"}
	defaultMarks  = []string{"strong", "em", "code", "link"}
	defaultBlocks = []string{"image"}
)

// maxRichTextNodes bounds the total node count of one document so a pathological
// body can't turn a single write into an unbounded validation walk.
const maxRichTextNodes = 10000

// safeLinkSchemes are the URL schemes a link annotation's href may use. A
// relative href (no scheme) is always allowed; javascript:/data: etc. are not.
var safeLinkSchemes = map[string]bool{"http": true, "https": true, "mailto": true, "tel": true}

func (f FieldDef) styles() []string {
	if len(f.Styles) > 0 {
		return f.Styles
	}
	return defaultStyles
}

func (f FieldDef) marks() []string {
	if len(f.Marks) > 0 {
		return f.Marks
	}
	return defaultMarks
}

func (f FieldDef) blocks() []string {
	if len(f.Blocks) > 0 {
		return f.Blocks
	}
	return defaultBlocks
}

// validateRichTextConfig checks a richtext field's allowlists at schema-compile
// time: marks must be known decorators/annotations and blocks must be known block
// types (styles are free-form renderer labels). Returns a message per problem.
func (f FieldDef) validateRichTextConfig() []string {
	var issues []string
	for _, m := range f.Marks {
		if !knownDecorators[m] && !knownAnnotations[m] {
			issues = append(issues, fmt.Sprintf("unknown mark %q (want a decorator: strong/em/code/underline/strike, or an annotation: link/reference)", m))
		}
	}
	for _, b := range f.Blocks {
		if !knownBlocks[b] {
			issues = append(issues, fmt.Sprintf("unknown block type %q (want image/code/embed/reference)", b))
		}
	}
	return issues
}

// validateRichText validates a decoded richtext value against this field's
// allowlists. It returns "" when the value is a well-formed document, or a single
// user-facing message describing the first problem. Reference *existence* is not
// checked here (that needs the DB — the gateway does it, batched); this is the
// pure structural layer.
func (f FieldDef) validateRichText(v any) string {
	nodes, ok := v.([]any)
	if !ok {
		return "must be a rich-content document (an array of blocks)"
	}
	styleOK := sliceSet(f.styles())
	markOK := sliceSet(f.marks())
	blockOK := sliceSet(f.blocks())

	count := 0
	for i, raw := range nodes {
		count++
		if count > maxRichTextNodes {
			return fmt.Sprintf("document is too large (over %d nodes)", maxRichTextNodes)
		}
		node, ok := raw.(map[string]any)
		if !ok {
			return fmt.Sprintf("block %d must be an object", i)
		}
		typ, _ := node["_type"].(string)
		if typ == "" {
			return fmt.Sprintf("block %d is missing its _type", i)
		}
		if typ == blockType {
			if msg := validateTextBlock(node, styleOK, markOK, &count); msg != "" {
				return fmt.Sprintf("block %d: %s", i, msg)
			}
			continue
		}
		// A custom (non-text) block must be allowed by the field and well-formed.
		if !blockOK[typ] {
			return fmt.Sprintf("block %d: block type %q is not allowed on this field", i, typ)
		}
		if msg := validateCustomBlock(typ, node); msg != "" {
			return fmt.Sprintf("block %d: %s", i, msg)
		}
	}
	return ""
}

// validateTextBlock validates a `block` node: its style, its markDefs, and its
// child spans (including that each span mark resolves to a decorator or a markDef
// declared on this block). It advances count by the number of spans so the node
// cap covers spans too.
func validateTextBlock(node map[string]any, styleOK, markOK map[string]bool, count *int) string {
	if style, ok := node["style"].(string); ok && style != "" && !styleOK[style] {
		return fmt.Sprintf("style %q is not allowed on this field", style)
	}

	// markDefs: collect declared annotation keys and validate each definition.
	defKeys := map[string]bool{}
	if rawDefs, present := node["markDefs"]; present {
		defs, ok := rawDefs.([]any)
		if !ok {
			return "markDefs must be an array"
		}
		for _, rd := range defs {
			def, ok := rd.(map[string]any)
			if !ok {
				return "each markDef must be an object"
			}
			key, _ := def["_key"].(string)
			if key == "" {
				return "each markDef needs a _key"
			}
			defKeys[key] = true
			dtype, _ := def["_type"].(string)
			if !markOK[dtype] {
				return fmt.Sprintf("markDef %q uses type %q, which is not allowed on this field", key, dtype)
			}
			if msg := validateAnnotation(dtype, def); msg != "" {
				return msg
			}
		}
	}

	rawChildren, present := node["children"]
	if !present {
		return "" // an empty block (e.g. a spacer) is allowed
	}
	children, ok := rawChildren.([]any)
	if !ok {
		return "children must be an array of spans"
	}
	for _, rc := range children {
		*count++
		span, ok := rc.(map[string]any)
		if !ok {
			return "each child must be a span object"
		}
		if t, _ := span["_type"].(string); t != spanType {
			return fmt.Sprintf("child _type must be %q", spanType)
		}
		if _, ok := span["text"].(string); !ok {
			return "a span needs string text"
		}
		if rawMarks, ok := span["marks"]; ok {
			marks, ok := rawMarks.([]any)
			if !ok {
				return "span marks must be an array of strings"
			}
			for _, rm := range marks {
				m, ok := rm.(string)
				if !ok {
					return "span marks must be strings"
				}
				// A mark is a decorator allowed on the field, or the key of a
				// markDef declared on this block.
				if markOK[m] || defKeys[m] {
					continue
				}
				return fmt.Sprintf("span mark %q is neither an allowed decorator nor a declared markDef", m)
			}
		}
	}
	return ""
}

// validateAnnotation checks an annotation definition's own required data.
func validateAnnotation(dtype string, def map[string]any) string {
	switch dtype {
	case "link":
		href, _ := def["href"].(string)
		if href == "" {
			return "a link markDef needs an href"
		}
		if !safeHref(href) {
			return "a link href must use a safe scheme (http, https, mailto, tel, or be relative)"
		}
	case "reference":
		if _, ok := def["collection"].(string); !ok {
			return "a reference markDef needs a collection"
		}
		if _, ok := def["ref"].(string); !ok {
			return "a reference markDef needs a ref (the target record id)"
		}
	}
	return ""
}

// validateCustomBlock checks a non-text block's required fields.
func validateCustomBlock(typ string, node map[string]any) string {
	switch typ {
	case "image":
		if _, ok := node["ref"].(string); !ok {
			return "an image block needs a ref (a media asset id)"
		}
	case "reference":
		if _, ok := node["collection"].(string); !ok {
			return "a reference block needs a collection"
		}
		if _, ok := node["ref"].(string); !ok {
			return "a reference block needs a ref (the target record id)"
		}
	case "code":
		if _, ok := node["code"].(string); !ok {
			return "a code block needs a code string"
		}
	case "embed":
		if _, ok := node["url"].(string); !ok {
			return "an embed block needs a url"
		}
	}
	return ""
}

// safeHref reports whether a link href is safe to store/render: a relative href
// (no scheme) or one using an allowlisted scheme. Blocks javascript:/data:/etc.
func safeHref(href string) bool {
	i := strings.IndexByte(href, ':')
	slash := strings.IndexByte(href, '/')
	// No scheme, or the ':' comes after the first '/' (so it's part of a path,
	// e.g. "/a:b") → relative, allowed.
	if i < 0 || (slash >= 0 && slash < i) {
		return true
	}
	return safeLinkSchemes[strings.ToLower(href[:i])]
}

// RichTextRef is one in-content reference harvested from a richtext value: the
// target collection and the referenced record id.
type RichTextRef struct {
	Collection string
	ID         string
}

// RichTextRefs walks a richtext value and returns every in-content reference:
// image blocks (→ the media collection) and `reference` nodes/annotations (→ the
// named collection). Best-effort — it tolerates a malformed document (the
// structural validator reports those) and simply skips nodes it can't read. The
// gateway feeds these into its batched existence check (no N+1).
func (f FieldDef) RichTextRefs(v any) []RichTextRef {
	nodes, ok := v.([]any)
	if !ok {
		return nil
	}
	var refs []RichTextRef
	add := func(collection, id string) {
		if collection != "" && id != "" {
			refs = append(refs, RichTextRef{Collection: collection, ID: id})
		}
	}
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := node["_type"].(string); t {
		case "image":
			ref, _ := node["ref"].(string)
			add(MediaCollection, ref)
		case "reference":
			coll, _ := node["collection"].(string)
			ref, _ := node["ref"].(string)
			add(coll, ref)
		case blockType:
			if defs, ok := node["markDefs"].([]any); ok {
				for _, rd := range defs {
					def, ok := rd.(map[string]any)
					if !ok {
						continue
					}
					if dt, _ := def["_type"].(string); dt == "reference" {
						coll, _ := def["collection"].(string)
						ref, _ := def["ref"].(string)
						add(coll, ref)
					}
				}
			}
		}
	}
	return refs
}

func sliceSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}
