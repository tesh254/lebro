package geminiapi

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
)

// The shared Content struct marshals into the exact generate-content wire
// shape, so asserting its JSON proves inlineData mapping for images and PDFs.
func TestParamsMapsMessageContentParts(t *testing.T) {
	model := newMappingModel("fixture-model")
	imageData := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	pdfData := base64.StdEncoding.EncodeToString([]byte("pdf-bytes"))
	imageRaw, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		t.Fatal(err)
	}
	pdfRaw, err := base64.StdEncoding.DecodeString(pdfData)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		message      lebro.Message
		wantWireJSON string
	}{
		{
			name: "text and image",
			message: lebro.Message{Role: lebro.RoleUser, ContentParts: partsFixture(t,
				lebro.MessageContentPart{Type: lebro.ContentPartText, Text: "what is in this picture?"},
				lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/png", Data: imageData},
			)},
			wantWireJSON: `{"parts":[{"text":"what is in this picture?"},{"inlineData":{"data":"` + base64.StdEncoding.EncodeToString(imageRaw) + `","mimeType":"image/png"}}],"role":"user"}`,
		},
		{
			name: "text image and pdf",
			message: lebro.Message{Role: lebro.RoleUser, ContentParts: partsFixture(t,
				lebro.MessageContentPart{Type: lebro.ContentPartText, Text: "compare"},
				lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/jpeg", Data: imageData},
				lebro.MessageContentPart{Type: lebro.ContentPartDocument, Filename: "report.pdf", MimeType: "application/pdf", Data: pdfData},
			)},
			wantWireJSON: `{"parts":[{"text":"compare"},{"inlineData":{"data":"` + base64.StdEncoding.EncodeToString(imageRaw) + `","mimeType":"image/jpeg"}},{"inlineData":{"data":"` + base64.StdEncoding.EncodeToString(pdfRaw) + `","mimeType":"application/pdf"}}],"role":"user"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, contents, _, err := model.params(lebro.ModelRequest{Model: "fixture-model", Messages: []lebro.Message{test.message}})
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) != 1 {
				t.Fatalf("contents = %#v", contents)
			}
			encoded, err := json.Marshal(contents[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.wantWireJSON {
				t.Fatalf("wire content = %s, want %s", encoded, test.wantWireJSON)
			}
		})
	}
}

// The legacy text-only request must keep its single text part shape.
func TestParamsKeepsLegacyTextPart(t *testing.T) {
	model := newMappingModel("fixture-model")
	_, contents, _, err := model.params(lebro.ModelRequest{Model: "fixture-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(contents[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"parts":[{"text":"hi"}],"role":"user"}` {
		t.Fatalf("legacy user content = %s", encoded)
	}
}

func partsFixture(t *testing.T, parts ...lebro.MessageContentPart) lebro.MessageContentParts {
	t.Helper()
	encoded, err := lebro.NewMessageContentParts(parts...)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
