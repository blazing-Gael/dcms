package gateway

import (
	"encoding/json"
	"html"
	"html/template"
	"net/http"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — read-only rendering of the field types the panel can't yet edit
// (richtext, file/media, object_list, json). A moderator has to be able to *read* a
// story and see its images to act on it; that's most of the value of a rich editor
// for far less work. Everything is escaped and wrapped safely — combined with the
// panel's strict CSP (no inline script, no third-party origins), rendered content
// can't execute.

// adminReadField is one read-only field shown on the edit page: a label and its
// pre-rendered, already-escaped HTML.
type adminReadField struct {
	Label string
	HTML  template.HTML
}

// adminReadOnlyFields renders the non-editable fields that carry content worth
// reading while moderating. Editable fields (handled by the form) are skipped.
func (s *Server) adminReadOnlyFields(r *http.Request, cd schema.CollectionDef, rec store.Record) []adminReadField {
	if rec == nil {
		return nil
	}
	var out []adminReadField
	for _, f := range cd.Fields {
		if adminEditable(f) {
			continue
		}
		v, ok := rec[f.Name]
		if !ok || v == nil {
			continue
		}
		var body template.HTML
		switch f.Type {
		case schema.TypeRichText:
			body = renderRichText(v)
		case schema.TypeFile:
			body = s.renderMediaField(r, v)
		default: // object_list, json — show the structured value, readably
			body = renderJSONBlock(v)
		}
		if strings.TrimSpace(string(body)) == "" {
			continue
		}
		out = append(out, adminReadField{Label: adminLabel(f), HTML: body})
	}
	return out
}

// ── richtext (portable-text-style, ADR-0014) → safe HTML ───────────────────────

// renderRichText turns a stored richtext document into read-only HTML. All span
// text is escaped; only a fixed set of formatting tags and vetted link hrefs are
// emitted. Embedded non-text blocks (image/embed/reference) render as a labelled
// placeholder rather than being resolved — enough to read the piece.
func renderRichText(v any) template.HTML {
	nodes, ok := v.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	listOpen := false
	closeList := func() {
		if listOpen {
			b.WriteString("</ul>")
			listOpen = false
		}
	}
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := node["_type"].(string); t != "block" {
			closeList()
			b.WriteString(renderCustomBlock(node))
			continue
		}
		inner := renderSpans(node)
		if _, isList := node["listItem"]; isList {
			if !listOpen {
				b.WriteString("<ul>")
				listOpen = true
			}
			b.WriteString("<li>" + inner + "</li>")
			continue
		}
		closeList()
		tag := richBlockTag(node)
		b.WriteString("<" + tag + ">" + inner + "</" + tag + ">")
	}
	closeList()
	return template.HTML(b.String()) //nolint:gosec // content is escaped span-by-span above
}

// richBlockTag maps a block style to a safe HTML tag.
func richBlockTag(node map[string]any) string {
	switch style, _ := node["style"].(string); style {
	case "h1", "h2", "h3", "h4":
		return style
	case "blockquote":
		return "blockquote"
	default:
		return "p"
	}
}

// renderSpans renders a text block's children, escaping every span's text and
// wrapping it in the decorator tags / vetted link its marks name.
func renderSpans(node map[string]any) string {
	defs := map[string]map[string]any{}
	if rawDefs, ok := node["markDefs"].([]any); ok {
		for _, rd := range rawDefs {
			if def, ok := rd.(map[string]any); ok {
				if key, _ := def["_key"].(string); key != "" {
					defs[key] = def
				}
			}
		}
	}
	children, _ := node["children"].([]any)
	var b strings.Builder
	for _, rc := range children {
		span, ok := rc.(map[string]any)
		if !ok {
			continue
		}
		text, _ := span["text"].(string)
		pre, post := "", ""
		if rawMarks, ok := span["marks"].([]any); ok {
			for _, rm := range rawMarks {
				m, _ := rm.(string)
				op, cl := markTags(m, defs)
				pre, post = op+pre, post+cl
			}
		}
		b.WriteString(pre + html.EscapeString(text) + post)
	}
	return b.String()
}

// markTags returns the open/close tags for one span mark: a formatting decorator,
// or a link markDef with a vetted href. Anything unknown contributes no tags (the
// text still renders).
func markTags(m string, defs map[string]map[string]any) (open, close string) {
	switch m {
	case "strong":
		return "<strong>", "</strong>"
	case "em":
		return "<em>", "</em>"
	case "code":
		return "<code>", "</code>"
	case "underline":
		return "<u>", "</u>"
	case "strike":
		return "<s>", "</s>"
	}
	if def, ok := defs[m]; ok {
		if dtype, _ := def["_type"].(string); dtype == "link" {
			if href, _ := def["href"].(string); safeHref(href) {
				return `<a href="` + html.EscapeString(href) + `" rel="noopener nofollow noreferrer">`, "</a>"
			}
		}
	}
	return "", ""
}

// renderCustomBlock renders a non-text block as a labelled placeholder — enough to
// know it's there while reading, without resolving media or embedding third-party
// content (which the CSP would block anyway).
func renderCustomBlock(node map[string]any) string {
	t, _ := node["_type"].(string)
	switch t {
	case "image":
		return `<p class="muted">🖼 [image]</p>`
	case "embed":
		return `<p class="muted">🔗 [embed]</p>`
	case "reference":
		return `<p class="muted">↪ [reference]</p>`
	case "code":
		if code, ok := node["code"].(string); ok {
			return "<pre>" + html.EscapeString(code) + "</pre>"
		}
	}
	return `<p class="muted">[` + html.EscapeString(t) + `]</p>`
}

// safeHref rejects the script-bearing URL schemes; relative and http(s)/mailto/tel
// links pass. Defense in depth — the document was already validated on write, and
// the panel CSP forbids inline script regardless.
func safeHref(h string) bool {
	h = strings.TrimSpace(h)
	if h == "" {
		return false
	}
	l := strings.ToLower(h)
	for _, bad := range []string{"javascript:", "data:", "vbscript:"} {
		if strings.HasPrefix(l, bad) {
			return false
		}
	}
	return true
}

// ── media (file fields) ────────────────────────────────────────────────────────

// renderMediaField resolves a file field's media reference(s) and renders each as an
// inline image (for image types) or a download link, using the same derived URL the
// API serves. A reference that can't be resolved (or has no URL because no blob
// backend is configured) renders as a filename or a plain note.
func (s *Server) renderMediaField(r *http.Request, v any) template.HTML {
	var ids []string
	switch t := v.(type) {
	case string:
		if t != "" {
			ids = []string{t}
		}
	case []any:
		for _, e := range t {
			if id, ok := e.(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(s.renderOneMedia(r, id))
	}
	return template.HTML(b.String()) //nolint:gosec // url/filename escaped below
}

func (s *Server) renderOneMedia(r *http.Request, id string) string {
	rec, err := s.db.FindOne(r.Context(), schema.MediaCollection, id)
	if err != nil || rec == nil {
		return `<p class="muted">[missing file ` + html.EscapeString(id) + `]</p>`
	}
	filename, _ := rec[schema.MediaFilename].(string)
	ctype, _ := rec[schema.MediaContentType].(string)
	s.addMediaURL(rec) // derives rec["url"], drops storage_key
	url, _ := rec["url"].(string)
	if url == "" {
		if filename != "" {
			return `<p class="muted">📎 ` + html.EscapeString(filename) + ` (no download URL — blob backend not configured)</p>`
		}
		return `<p class="muted">📎 [file]</p>`
	}
	eu := html.EscapeString(url)
	if strings.HasPrefix(strings.ToLower(ctype), "image/") {
		return `<img class="admin-media" src="` + eu + `" alt="` + html.EscapeString(filename) + `">`
	}
	label := filename
	if label == "" {
		label = "Download file"
	}
	return `<p><a href="` + eu + `" rel="noopener">📎 ` + html.EscapeString(label) + `</a></p>`
}

// renderJSONBlock pretty-prints a structured value (object_list, json) into an
// escaped <pre> so it's readable without an editor.
func renderJSONBlock(v any) template.HTML {
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return template.HTML("<pre>" + html.EscapeString(string(pretty)) + "</pre>") //nolint:gosec // escaped
}
