package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DocumentMimeTypePDF is the only document media type message parts carry.
// Adapters map it to each provider's native PDF input.
const DocumentMimeTypePDF = "application/pdf"

// ContentPartKind identifies the modality of one message content part.
type ContentPartKind string

const (
	ContentPartText     ContentPartKind = "text"
	ContentPartImage    ContentPartKind = "image"
	ContentPartDocument ContentPartKind = "document"
)

// MessageContentPart is one ordered element of a multipart message. Text parts
// carry UTF-8 text; image and document parts carry a MIME type and
// base64-encoded bytes (document parts also carry a filename). Part values are
// created through the New*Part constructors, which validate every field, so a
// constructed part is always well formed.
type MessageContentPart struct {
	Type     ContentPartKind `json:"type"`
	Text     string          `json:"text,omitempty"`
	MimeType string          `json:"mime_type,omitempty"`
	Filename string          `json:"filename,omitempty"`
	Data     string          `json:"data,omitempty"`
}

// Validate checks the invariants every provider adapter can rely on. String
// fields must be valid UTF-8 so canonical encoding cannot silently corrupt
// them, media types must be well-formed, and binary payloads must be clean,
// non-empty standard base64.
func (p MessageContentPart) Validate() error {
	for _, field := range [...]struct{ name, value string }{{"text", p.Text}, {"mime type", p.MimeType}, {"filename", p.Filename}} {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("lebro: content part %s is not valid UTF-8", field.name)
		}
	}
	switch p.Type {
	case ContentPartText:
		if p.Text == "" {
			return errors.New("lebro: text content part requires text")
		}
		if p.MimeType != "" || p.Filename != "" || p.Data != "" {
			return errors.New("lebro: text content part must not carry binary fields")
		}
	case ContentPartImage:
		// Media types are case-insensitive; the image family check accepts any
		// case and adapters normalize where their provider requires it.
		if !validMIMETypeSyntax(p.MimeType) || !strings.HasPrefix(strings.ToLower(p.MimeType), "image/") {
			return fmt.Errorf("lebro: image content part requires a valid image/* media type, got %q", p.MimeType)
		}
		if p.Filename != "" || p.Text != "" {
			return errors.New("lebro: image content part must not carry text or filename fields")
		}
		if !validBase64(p.Data) {
			return errors.New("lebro: image content part requires non-empty valid base64 data")
		}
	case ContentPartDocument:
		if !validMIMETypeSyntax(p.MimeType) || !strings.EqualFold(p.MimeType, DocumentMimeTypePDF) {
			return fmt.Errorf("lebro: document content parts support only %s, got %q", DocumentMimeTypePDF, p.MimeType)
		}
		if p.Filename == "" {
			return errors.New("lebro: document content part requires a filename")
		}
		if p.Text != "" {
			return errors.New("lebro: document content part must not carry a text field")
		}
		if !validBase64(p.Data) {
			return errors.New("lebro: document content part requires non-empty valid base64 data")
		}
	default:
		return fmt.Errorf("lebro: unknown content part type %q", p.Type)
	}
	return nil
}

// validMIMETypeSyntax reports whether value is a type/subtype pair whose sides
// are non-empty RFC 2045 tokens. Parameters are rejected: a part carries
// exactly one media type.
func validMIMETypeSyntax(value string) bool {
	typeName, subtype, ok := strings.Cut(value, "/")
	if !ok || subtype == "" || strings.Contains(subtype, "/") {
		return false
	}
	return isMIMEToken(typeName) && isMIMEToken(subtype)
}

// isMIMEToken reports whether value is a non-empty RFC 2045 token.
func isMIMEToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$&-^_.+", r):
		default:
			return false
		}
	}
	return true
}

// validBase64 reports whether data is clean standard base64 — no ignored
// whitespace — whose decoded payload is non-empty, so adapters never transmit
// an empty image or document.
func validBase64(data string) bool {
	if data == "" {
		return false
	}
	for _, r := range data {
		if unicode.IsSpace(r) {
			return false
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	return err == nil && len(decoded) > 0
}

// MessageContentParts is an immutable, canonical encoding of ordered content
// parts. Its opaque representation keeps Message comparable and ensures values
// can only be created through validation or JSON decoding.
type MessageContentParts struct {
	encoded string
}

// NewMessageContentParts validates and canonically encodes ordered content
// parts. An empty argument list produces the zero value.
func NewMessageContentParts(parts ...MessageContentPart) (MessageContentParts, error) {
	if len(parts) == 0 {
		return MessageContentParts{}, nil
	}
	for i, part := range parts {
		if err := part.Validate(); err != nil {
			return MessageContentParts{}, fmt.Errorf("lebro: content part %d: %w", i, err)
		}
	}
	// Encode without HTML escaping so persisted bytes stay faithful to source
	// text, filenames, and base64 payloads (json.Marshal would escape <, >, &
	// inside strings).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(parts); err != nil {
		return MessageContentParts{}, fmt.Errorf("lebro: encode message content parts: %w", err)
	}
	return MessageContentParts{encoded: string(bytes.TrimSpace(buf.Bytes()))}, nil
}

// Values decodes ordered content parts into a caller-owned slice.
func (p MessageContentParts) Values() []MessageContentPart {
	if p.IsZero() {
		return nil
	}
	var parts []MessageContentPart
	// encoded is private and every construction path validates it first.
	_ = json.Unmarshal([]byte(p.encoded), &parts)
	return parts
}

// IsZero reports whether the collection contains no parts.
func (p MessageContentParts) IsZero() bool { return p.encoded == "" }

// MarshalJSON writes content parts as an array rather than a quoted string.
func (p MessageContentParts) MarshalJSON() ([]byte, error) {
	if p.IsZero() {
		return []byte("[]"), nil
	}
	return []byte(p.encoded), nil
}

// UnmarshalJSON validates and canonically encodes an array of content parts,
// so corrupted or malformed persisted values fail loudly instead of decoding
// into parts that silently disappear at a provider boundary.
func (p *MessageContentParts) UnmarshalJSON(data []byte) error {
	if p == nil {
		return errors.New("lebro: unmarshal message content parts into nil receiver")
	}
	if string(data) == "null" {
		*p = MessageContentParts{}
		return nil
	}
	var parts []MessageContentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("lebro: decode message content parts: %w", err)
	}
	encoded, err := NewMessageContentParts(parts...)
	if err != nil {
		return err
	}
	*p = encoded
	return nil
}

// NewTextPart creates an ordered text content part.
func NewTextPart(text string) (MessageContentPart, error) {
	part := MessageContentPart{Type: ContentPartText, Text: text}
	if err := part.Validate(); err != nil {
		return MessageContentPart{}, err
	}
	return part, nil
}

// NewImagePart creates an image content part from a MIME type (for example
// "image/png") and base64-encoded image bytes.
func NewImagePart(mimeType, base64Data string) (MessageContentPart, error) {
	part := MessageContentPart{Type: ContentPartImage, MimeType: mimeType, Data: base64Data}
	if err := part.Validate(); err != nil {
		return MessageContentPart{}, err
	}
	return part, nil
}

// NewDocumentPart creates a document content part from a filename, MIME type,
// and base64-encoded document bytes. Only application/pdf documents are
// accepted; adapters send them through each provider's native PDF input.
func NewDocumentPart(filename, mimeType, base64Data string) (MessageContentPart, error) {
	part := MessageContentPart{Type: ContentPartDocument, Filename: filename, MimeType: mimeType, Data: base64Data}
	if err := part.Validate(); err != nil {
		return MessageContentPart{}, err
	}
	return part, nil
}

// NewTextAttachmentPart wraps decoded UTF-8 text file contents as an XML
// attachment inside a normal text content part:
//
//	<attachment filename="notes.md" media_type="text/markdown">
//	...actual file contents...
//	</attachment>
//
// The wrapper marks the span as user-provided attachment data; it does not
// grant the contents instruction priority, and providers treat it as ordinary
// text. Attribute values and text contents are XML-escaped. Text that cannot
// be represented as valid XML — invalid UTF-8 or characters outside the XML
// 1.0 Char production — is rejected instead of silently corrupted. File
// reading and character-encoding conversion stay with the caller.
func NewTextAttachmentPart(filename, mimeType, text string) (MessageContentPart, error) {
	if strings.TrimSpace(filename) == "" {
		return MessageContentPart{}, errors.New("lebro: attachment filename is required")
	}
	if strings.TrimSpace(mimeType) == "" {
		return MessageContentPart{}, errors.New("lebro: attachment media type is required")
	}
	if err := validateXMLRepresentable(filename, "attachment filename"); err != nil {
		return MessageContentPart{}, err
	}
	if err := validateXMLRepresentable(mimeType, "attachment media type"); err != nil {
		return MessageContentPart{}, err
	}
	if err := validateXMLRepresentable(text, "attachment text"); err != nil {
		return MessageContentPart{}, err
	}
	wrapped := "<attachment filename=\"" + xmlEscapeAttribute(filename) + "\" media_type=\"" + xmlEscapeAttribute(mimeType) + "\">\n" + xmlEscapeText(text) + "</attachment>"
	return MessageContentPart{Type: ContentPartText, Text: wrapped}, nil
}

// validateXMLRepresentable rejects values that cannot appear inside an XML
// 1.0 document: invalid UTF-8 and characters outside the Char production
// (#x9 | #xA | #xD | [#x20-#xD7FF] | [#xE000-#xFFFD] | [#x10000-#x10FFFF]).
func validateXMLRepresentable(value, what string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("lebro: %s is not valid UTF-8", what)
	}
	for _, r := range value {
		if !xmlCharAllowed(r) {
			return fmt.Errorf("lebro: %s contains character %q that cannot appear in an XML document", what, r)
		}
	}
	return nil
}

func xmlCharAllowed(r rune) bool {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return true
	case r >= 0x20 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	default:
		return false
	}
}

// xmlEscapeAttribute escapes a value for use inside a double-quoted XML
// attribute. Whitespace is escaped as character references so parsers do not
// normalize it away.
func xmlEscapeAttribute(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"\n", "&#xA;",
		"\r", "&#xD;",
		"\t", "&#x9;",
	).Replace(value)
}

// xmlEscapeText escapes text content. Escaping > also breaks any literal "]]>"
// sequence, and carriage returns are preserved as character references
// instead of being normalized to newlines by XML parsers.
func xmlEscapeText(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\r", "&#xD;",
	).Replace(value)
}
