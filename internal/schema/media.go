package schema

// MediaCollection is the engine-managed collection that stores file metadata
// (ADR-0011). It is reserved (users can't declare it) and injected into every
// schema. A `file` field is sugar for a relation targeting it; the bytes
// themselves live in a blob store, keyed by StorageKeyField.
const MediaCollection = "_media"

// Media metadata field names, referenced by the gateway's media handlers so the
// two stay in lockstep with the injected collection below.
const (
	MediaFilename    = "filename"
	MediaContentType = "content_type"
	MediaSize        = "size"
	MediaStorageKey  = "storage_key"
	MediaChecksum    = "checksum"
	MediaWidth       = "width"
	MediaHeight      = "height"
	MediaAlt         = "alt"
	MediaTitle       = "title"
	MediaCaption     = "caption"
)

// mediaCollectionDef is the canonical shape of the _media collection. All fields
// are optional at the schema layer: media rows are never created from JSON (the
// upload handler sets filename/content_type/size/storage_key), and the editable
// metadata (alt/title/caption) is genuinely optional.
func mediaCollectionDef() CollectionDef {
	return CollectionDef{
		Name: MediaCollection,
		Fields: []FieldDef{
			{Name: MediaFilename, Type: TypeString},
			{Name: MediaContentType, Type: TypeString},
			{Name: MediaSize, Type: TypeInteger},
			{Name: MediaStorageKey, Type: TypeString},
			{Name: MediaChecksum, Type: TypeString},
			{Name: MediaWidth, Type: TypeInteger},
			{Name: MediaHeight, Type: TypeInteger},
			{Name: MediaAlt, Type: TypeString},
			{Name: MediaTitle, Type: TypeString},
			{Name: MediaCaption, Type: TypeString},
		},
	}
}

// injectMedia finalizes a validated schema for media support (ADR-0011): it
// rewrites every `file` field into a relation targeting _media (preserving
// `many` for galleries and any on_delete policy), then appends the engine-managed
// _media collection itself. Appending last keeps user collection indices stable.
//
// _media is injected unconditionally — the media library is a standalone feature,
// so an operator can upload and manage assets even with no `file` field declared.
func (s *SchemaDefinition) injectMedia() {
	// A schema may name _media purely to attach an `access:` block (validated in
	// Validate). Lift that policy off and drop the placeholder, so the engine's
	// definition is the only _media collection that survives.
	var access *AccessRules
	kept := s.Collections[:0]
	for _, c := range s.Collections {
		if c.Name == MediaCollection {
			access = c.Access
			continue
		}
		kept = append(kept, c)
	}
	s.Collections = kept

	for ci := range s.Collections {
		for fi := range s.Collections[ci].Fields {
			f := &s.Collections[ci].Fields[fi]
			if f.Type == TypeFile {
				f.Type = TypeRelation
				f.Target = MediaCollection
			}
			// A `file` inside an object_list element is the same sugar (issue #6):
			// rewrite it to a _media relation so the element carries a media id.
			for oi := range f.Of {
				if in := &f.Of[oi]; in.Type == TypeFile {
					in.Type = TypeRelation
					in.Target = MediaCollection
				}
			}
		}
	}
	def := mediaCollectionDef()
	def.Access = access // nil ⇒ the engine default: public read, authenticated write
	s.Collections = append(s.Collections, def)
}
