package schema

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// knownDirectives are the collection directives the parser handles today. Used
// to make an unknown-directive error actionable (did-you-mean) — issue #35.
var knownDirectives = []string{"fields", "timestamps", "indexes", "publishing", "soft_delete", "revisions", "events", "route", "access"}

// reservedDirectives are recognized but not implemented yet (later phases). They
// are allowed with a warning rather than an error, so a schema can forward-declare
// intent while a typo of a real directive is still rejected loudly (issue #35).
var reservedDirectives = []string{"hooks", "vectorize", "i18n", "schedule"}

// ValidationError lists schema validation failures with their field paths,
// e.g. "collections.products.fields.category".
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return "schema validation failed:\n  " + strings.Join(e.Issues, "\n  ")
}

// Parse reads YAML bytes into a SchemaDefinition and validates it. A successful
// return guarantees the schema is structurally sound and safe to compile.
func Parse(src []byte) (*SchemaDefinition, error) {
	// Strict decode of the top level: an unknown top-level or `meta` key is a hard
	// error, not a silent drop, so a typo can't disable a setting with nothing said
	// (issue #35). `auth`/`collections` are captured as raw nodes and walked below,
	// where directive typos get the same treatment.
	var raw rawSchema
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("schema: parse yaml: %w", err)
	}
	def, warnings, err := raw.toDefinition()
	if err != nil {
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	def.Warnings = warnings
	// Finalize media support after validation: rewrite `file` fields to relations
	// and inject the engine-managed _media collection (ADR-0011).
	def.injectMedia()
	// Inject the _revisions collection when any collection opts in (ADR-0013).
	def.injectRevisions()
	// Inject the engine-managed identity collections _users/_sessions (ADR-0016).
	def.injectAuth()
	// Inject the engine-managed _auth_tokens collection (ADR-0019); after
	// injectAuth so its user_id relation target (_users) exists.
	def.injectAuthTokens()
	// Inject the engine-managed _api_tokens collection (issue #8).
	def.injectAPITokens()
	// Inject the engine-managed _idempotency collection (ADR-0018).
	def.injectIdempotency()
	// Inject the engine-managed _notifications outbox (ADR-0021 phase 3).
	def.injectNotifications()
	// Inject the engine-managed _events change log when any collection opts in
	// (ADR-0021, M-B), then the webhook-delivery collections beside it.
	def.injectEvents()
	def.injectWebhooks()
	return def, nil
}

// ── raw YAML shapes ─────────────────────────────────────────────────────────
//
// We decode the top level into structs but keep `collections` (and within it,
// `fields` / `indexes`) as yaml.Node, because those use shorthand forms that a
// plain struct can't express: a field may be a scalar ("string") OR a mapping
// (full form), and an index entry may be a scalar OR a list (composite).
// Walking the nodes ourselves also preserves document order — which keeps
// generated columns, migrations, and codegen deterministic.

type rawSchema struct {
	Version     string    `yaml:"version"`
	Meta        Meta      `yaml:"meta"`
	Auth        yaml.Node `yaml:"auth"`
	Collections yaml.Node `yaml:"collections"`
}

// rawField is the full ("long") form of a field definition. Unknown keys
// (Phase 2+ directives like access) are ignored by yaml's struct decoder.
type rawField struct {
	Type     string    `yaml:"type"`
	Required bool      `yaml:"required"`
	Default  any       `yaml:"default"`
	Unique   bool      `yaml:"unique"`
	Min      *float64  `yaml:"min"`
	Max      *float64  `yaml:"max"`
	Pattern  string    `yaml:"pattern"`
	Values   []string  `yaml:"values"`
	Scale    *int      `yaml:"scale"`
	Label    string    `yaml:"label"`
	Hint     string    `yaml:"hint"`
	Target   string    `yaml:"target"`
	Many     bool      `yaml:"many"`
	OnDelete string    `yaml:"on_delete"`
	Styles   []string  `yaml:"styles"`
	Marks    []string  `yaml:"marks"`
	Blocks   []string  `yaml:"blocks"`
	Access   yaml.Node `yaml:"access"`
}

type nodeEntry struct {
	Key string
	Val *yaml.Node
}

// mappingEntries returns the key/value pairs of a YAML mapping node, in order.
func mappingEntries(n *yaml.Node) ([]nodeEntry, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping, got %s", kindName(n.Kind))
	}
	out := make([]nodeEntry, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, nodeEntry{Key: n.Content[i].Value, Val: n.Content[i+1]})
	}
	return out, nil
}

func (r rawSchema) toDefinition() (*SchemaDefinition, []string, error) {
	def := &SchemaDefinition{Version: r.Version, Meta: r.Meta}
	if r.Auth.Kind != 0 {
		auth, err := parseAuth(&r.Auth)
		if err != nil {
			return nil, nil, fmt.Errorf("schema: auth: %w", err)
		}
		def.Auth = auth
	}
	if r.Collections.Kind == 0 {
		return def, nil, nil // no collections — Validate reports it
	}
	entries, err := mappingEntries(&r.Collections)
	if err != nil {
		return nil, nil, fmt.Errorf("schema: collections: %w", err)
	}
	var warnings []string
	for _, e := range entries {
		col, w, err := toCollection(e.Key, e.Val)
		if err != nil {
			return nil, nil, fmt.Errorf("schema: collections.%s: %w", e.Key, err)
		}
		warnings = append(warnings, w...)
		def.Collections = append(def.Collections, col)
	}
	return def, warnings, nil
}

func toCollection(name string, node *yaml.Node) (CollectionDef, []string, error) {
	col := CollectionDef{Name: name}
	var warnings []string
	entries, err := mappingEntries(node)
	if err != nil {
		return col, nil, err
	}
	for _, e := range entries {
		// _media is engine-managed: it accepts only an `access:` block. Reject any
		// other key by PRESENCE, here at parse time, so `timestamps: false` or
		// `fields: {}` — a present key with a zero value the decoded struct can't
		// distinguish from absent — is refused, not silently dropped. (The Validate
		// guard still covers a directly-constructed CollectionDef.)
		if name == MediaCollection && e.Key != "access" {
			return col, nil, fmt.Errorf("the media library is engine-managed — it accepts an `access:` block only, not %q", e.Key)
		}
		switch e.Key {
		case "fields":
			col.Fields, err = toFields(e.Val)
			if err != nil {
				return col, nil, fmt.Errorf("fields: %w", err)
			}
		case "timestamps":
			if err := e.Val.Decode(&col.Timestamps); err != nil {
				return col, nil, fmt.Errorf("timestamps: %w", err)
			}
		case "indexes":
			col.Indexes, err = toIndexes(e.Val)
			if err != nil {
				return col, nil, fmt.Errorf("indexes: %w", err)
			}
		case "publishing":
			if err := e.Val.Decode(&col.Publishing); err != nil {
				return col, nil, fmt.Errorf("publishing: %w", err)
			}
		case "soft_delete":
			if err := e.Val.Decode(&col.SoftDelete); err != nil {
				return col, nil, fmt.Errorf("soft_delete: %w", err)
			}
		case "revisions":
			if err := e.Val.Decode(&col.Revisions); err != nil {
				return col, nil, fmt.Errorf("revisions: %w", err)
			}
		case "events":
			if err := e.Val.Decode(&col.Events); err != nil {
				return col, nil, fmt.Errorf("events: %w", err)
			}
		case "route":
			if err := e.Val.Decode(&col.Route); err != nil {
				return col, nil, fmt.Errorf("route: %w", err)
			}
		case "access":
			col.Access, err = parseAccess(e.Val)
			if err != nil {
				return col, nil, fmt.Errorf("access: %w", err)
			}
		default:
			// A directive we recognise for a later phase (hooks/vectorize/i18n/
			// schedule) is allowed but warned about, so nobody believes it's doing
			// something yet. Anything else is a typo or a made-up key — reject it
			// loudly with a did-you-mean, rather than silently dropping a setting
			// (issue #35).
			if slices.Contains(reservedDirectives, e.Key) {
				warnings = append(warnings, fmt.Sprintf("collections.%s: `%s` is recognized but not implemented yet — it currently has no effect", name, e.Key))
				continue
			}
			return col, nil, fmt.Errorf("unknown directive %q%s", e.Key, didYouMean(e.Key, append(append([]string{}, knownDirectives...), reservedDirectives...)))
		}
	}
	return col, warnings, nil
}

// didYouMean returns " (did you mean `X`?)" when input is a near-miss for one of
// the candidates, else "". The threshold scales with word length so a small typo
// suggests a fix while a wholly unrelated key suggests nothing.
func didYouMean(input string, candidates []string) string {
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		if d := levenshtein(input, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	threshold := max(len(best)/3, 2)
	if best != "" && bestDist > 0 && bestDist <= threshold {
		return fmt.Sprintf(" (did you mean `%s`?)", best)
	}
	return ""
}

// levenshtein is the edit distance between two short strings (directive/key names).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func toFields(node *yaml.Node) ([]FieldDef, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	fields := make([]FieldDef, 0, len(entries))
	for _, e := range entries {
		f := FieldDef{Name: e.Key}
		switch e.Val.Kind {
		case yaml.ScalarNode:
			// Shorthand: `title: string`
			f.Type = FieldType(e.Val.Value)
		case yaml.MappingNode:
			// Full form: `title: { type: string, required: true, ... }`
			var rf rawField
			if err := e.Val.Decode(&rf); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Key, err)
			}
			f.Type = FieldType(rf.Type)
			f.Required = rf.Required
			f.Default = rf.Default
			f.Unique = rf.Unique
			f.Min = rf.Min
			f.Max = rf.Max
			f.Pattern = rf.Pattern
			f.Values = rf.Values
			f.Scale = rf.Scale
			f.Label = rf.Label
			f.Hint = rf.Hint
			f.Target = rf.Target
			f.Many = rf.Many
			f.OnDelete = rf.OnDelete
			f.Styles = rf.Styles
			f.Marks = rf.Marks
			f.Blocks = rf.Blocks
			if rf.Access.Kind != 0 {
				f.Access, err = parseFieldAccess(&rf.Access)
				if err != nil {
					return nil, fmt.Errorf("%s: access: %w", e.Key, err)
				}
			}
		default:
			return nil, fmt.Errorf("%s: expected a type or a field definition, got %s", e.Key, kindName(e.Val.Kind))
		}
		fields = append(fields, f)
	}
	return fields, nil
}

func toIndexes(node *yaml.Node) ([]Index, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a list, got %s", kindName(node.Kind))
	}
	indexes := make([]Index, 0, len(node.Content))
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			// Single-column index: `status`
			indexes = append(indexes, Index{Columns: []string{item.Value}})
		case yaml.SequenceNode:
			// Composite index: `[category_id, status]`
			var cols []string
			if err := item.Decode(&cols); err != nil {
				return nil, err
			}
			indexes = append(indexes, Index{Columns: cols})
		default:
			return nil, fmt.Errorf("index entry must be a field name or a list, got %s", kindName(item.Kind))
		}
	}
	return indexes, nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.DocumentNode:
		return "document"
	case yaml.AliasNode:
		return "alias"
	default:
		return "empty"
	}
}
