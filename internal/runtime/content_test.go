package runtime

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const testImageData = "aW1hZ2UtYnl0ZXM="     // "image-bytes"
const testPDFData = "cGRmLWJ5dGVz"           // "pdf-bytes"
const testMalformedData = "not!valid@base64" // invalid base64

func partsForTest(parts ...MessageContentPart) MessageContentParts {
	encoded, err := NewMessageContentParts(parts...)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestContentPartValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		part    MessageContentPart
		wantErr bool
	}{
		{name: "text", part: MessageContentPart{Type: ContentPartText, Text: "hello"}},
		{name: "image png", part: MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: testImageData}},
		{name: "image jpeg", part: MessageContentPart{Type: ContentPartImage, MimeType: "image/jpeg", Data: testImageData}},
		{name: "document pdf", part: MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF, Data: testPDFData}},
		{name: "unknown type", part: MessageContentPart{Type: "audio", Data: testImageData}, wantErr: true},
		{name: "empty text", part: MessageContentPart{Type: ContentPartText}, wantErr: true},
		{name: "text with data", part: MessageContentPart{Type: ContentPartText, Text: "hello", Data: testImageData}, wantErr: true},
		{name: "text with mime", part: MessageContentPart{Type: ContentPartText, Text: "hello", MimeType: "text/plain"}, wantErr: true},
		{name: "image without mime", part: MessageContentPart{Type: ContentPartImage, Data: testImageData}, wantErr: true},
		{name: "image non-image mime", part: MessageContentPart{Type: ContentPartImage, MimeType: "application/pdf", Data: testImageData}, wantErr: true},
		{name: "image without data", part: MessageContentPart{Type: ContentPartImage, MimeType: "image/png"}, wantErr: true},
		{name: "image invalid base64", part: MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: testMalformedData}, wantErr: true},
		{name: "image with filename", part: MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Filename: "pic.png", Data: testImageData}, wantErr: true},
		{name: "document non-pdf", part: MessageContentPart{Type: ContentPartDocument, Filename: "doc.docx", MimeType: "application/msword", Data: testPDFData}, wantErr: true},
		{name: "document without filename", part: MessageContentPart{Type: ContentPartDocument, MimeType: DocumentMimeTypePDF, Data: testPDFData}, wantErr: true},
		{name: "document without data", part: MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF}, wantErr: true},
		{name: "document invalid base64", part: MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF, Data: testMalformedData}, wantErr: true},
		{name: "document with text", part: MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF, Data: testPDFData, Text: "extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.part.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewMessageContentPartsValidation(t *testing.T) {
	t.Parallel()

	if _, err := NewMessageContentParts(); err != nil {
		t.Fatalf("empty parts error = %v", err)
	}
	if _, err := NewMessageContentParts(MessageContentPart{Type: ContentPartText}); err == nil {
		t.Fatal("invalid part accepted")
	}
	if _, err := NewMessageContentParts(
		MessageContentPart{Type: ContentPartText, Text: "ok"},
		MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: testMalformedData},
	); err == nil || !strings.Contains(err.Error(), "content part 1") {
		t.Fatalf("error = %v, want invalid part at index 1", err)
	}
}

func TestMessageContentPartsRoundTrip(t *testing.T) {
	t.Parallel()

	want, err := NewMessageContentParts(
		MessageContentPart{Type: ContentPartText, Text: "look at this"},
		MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: testImageData},
		MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF, Data: testPDFData},
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got MessageContentParts
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
	values := got.Values()
	if len(values) != 3 || values[0].Text != "look at this" || values[1].MimeType != "image/png" || values[2].Filename != "report.pdf" {
		t.Fatalf("values = %#v", values)
	}
}

func TestMessageContentPartsZeroValue(t *testing.T) {
	t.Parallel()

	var zero MessageContentParts
	if !zero.IsZero() || zero.Values() != nil {
		t.Fatal("zero value must be empty")
	}
	encoded, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("encoded zero = %s, want []", encoded)
	}
	var decoded MessageContentParts
	if err := json.Unmarshal([]byte(`null`), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.IsZero() {
		t.Fatal("null must decode to the zero value")
	}
}

func TestMessageContentPartsRejectCorruptEncoding(t *testing.T) {
	t.Parallel()

	var parts MessageContentParts
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"ok"},{"type":"audio"}]`), &parts); err == nil {
		t.Fatal("malformed stored parts must fail to decode")
	}
}

func TestMessageValidateContentParts(t *testing.T) {
	t.Parallel()

	valid := partsForTest(
		MessageContentPart{Type: ContentPartText, Text: "see attached"},
		MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: testImageData},
	)
	tests := []struct {
		name    string
		message Message
		wantErr bool
	}{
		{name: "user parts", message: Message{Role: RoleUser, ContentParts: valid}},
		{name: "content and parts", message: Message{Role: RoleUser, Content: "hello", ContentParts: valid}, wantErr: true},
		{name: "parts on system", message: Message{Role: RoleSystem, ContentParts: valid}, wantErr: true},
		{name: "parts on assistant", message: Message{Role: RoleAssistant, ContentParts: valid}, wantErr: true},
		{name: "parts on tool", message: Message{Role: RoleTool, ToolCallID: "call_1", ContentParts: valid}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.message.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMessageWithPartsRoundTripInTranscriptJSON(t *testing.T) {
	t.Parallel()

	want := Message{Role: RoleUser, ContentParts: partsForTest(
		MessageContentPart{Type: ContentPartText, Text: "look at this"},
		MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF, Data: testPDFData},
	)}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

// Legacy text-only transcripts must decode unchanged: the new field is absent
// from their JSON and must not reject or alter the decoded message.
func TestLegacyTextOnlyMessageDecodes(t *testing.T) {
	t.Parallel()

	var got Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &got); err != nil {
		t.Fatal(err)
	}
	want := Message{Role: RoleUser, Content: "hello"}
	if got != want {
		t.Fatalf("decoded = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("legacy message validation = %v", err)
	}
}

func TestNewTextAttachmentPart(t *testing.T) {
	t.Parallel()

	part, err := NewTextAttachmentPart("notes.md", "text/markdown", "line one\nline two")
	if err != nil {
		t.Fatal(err)
	}
	want := "<attachment filename=\"notes.md\" media_type=\"text/markdown\">\nline one\nline two</attachment>"
	if part.Type != ContentPartText || part.Text != want {
		t.Fatalf("part = %#v, want text %q", part, want)
	}
	if err := part.Validate(); err != nil {
		t.Fatalf("attachment part validation = %v", err)
	}
}

func TestNewTextAttachmentPartEscapes(t *testing.T) {
	t.Parallel()

	text := "quote \" amp & lt < gt > cr \r newline \n tab \t end]]>"
	part, err := NewTextAttachmentPart(`we"ird<name>`, "text/plain+xml", text)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(part.Text, `we"ird<name>`) {
		t.Fatalf("attribute value not escaped: %q", part.Text)
	}
	if !strings.Contains(part.Text, `filename="we&quot;ird&lt;name&gt;"`) {
		t.Fatalf("attribute escaping wrong: %q", part.Text)
	}
	if !strings.Contains(part.Text, "amp &amp; lt &lt; gt &gt; cr &#xD; newline \n tab \t end]]&gt;") {
		t.Fatalf("text escaping wrong: %q", part.Text)
	}
	if !strings.HasPrefix(part.Text, "<attachment ") || !strings.HasSuffix(part.Text, "</attachment>") {
		t.Fatalf("wrapper structure wrong: %q", part.Text)
	}
	// Round-trip invariant: no raw < survives inside the text payload, so no
	// XML parser can mistake payload text for markup.
	payload := part.Text[strings.Index(part.Text, ">\n")+2:]
	payload = strings.TrimSuffix(payload, "</attachment>")
	if strings.Contains(payload, "<") {
		t.Fatalf("raw XML markup leaked into payload: %q", payload)
	}
}

func TestNewTextAttachmentPartRejectsUnrepresentableText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		mimeType string
		text     string
	}{
		{name: "empty filename", filename: " ", mimeType: "text/plain", text: "ok"},
		{name: "empty mime", filename: "notes.md", mimeType: "", text: "ok"},
		{name: "control char in text", filename: "notes.md", mimeType: "text/plain", text: "bad\x00control"},
		{name: "escape char in text", filename: "notes.md", mimeType: "text/plain", text: "bad\x1b[31mcolor"},
		{name: "vertical tab", filename: "notes.md", mimeType: "text/plain", text: "bad\x0bform"},
		{name: "control char in filename", filename: "bad\x07bell.md", mimeType: "text/plain", text: "ok"},
		{name: "U+FFFE in text", filename: "notes.md", mimeType: "text/plain", text: "bad￾"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewTextAttachmentPart(tt.filename, tt.mimeType, tt.text); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestNewTextAttachmentPartEmptyText(t *testing.T) {
	t.Parallel()

	part, err := NewTextAttachmentPart("empty.txt", "text/plain", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "<attachment filename=\"empty.txt\" media_type=\"text/plain\">\n</attachment>"
	if part.Text != want {
		t.Fatalf("part = %q, want %q", part.Text, want)
	}
}

func TestBase64RoundTripMatchesDecoder(t *testing.T) {
	t.Parallel()

	raw := []byte{0x00, 0xff, 0x10, 'l', 'e', 'b', 'r', 'o'}
	encoded := base64.StdEncoding.EncodeToString(raw)
	part, err := NewImagePart("image/png", encoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(part.Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("decoded = %q, want %q", decoded, raw)
	}
}
