package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

// The params struct marshals into the exact Messages API request body, so
// asserting its JSON proves the wire shape for image and document blocks.
func TestParamsMapsMessageContentParts(t *testing.T) {
	model, err := New(Config{APIKey: "key", Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	imageData := "aW1hZ2UtYnl0ZXM="
	pdfData := "cGRmLWJ5dGVz"
	tests := []struct {
		name    string
		message lebro.Message
		want    []map[string]any
	}{
		{
			name: "text and image",
			message: lebro.Message{Role: lebro.RoleUser, ContentParts: partsFixture(t,
				lebro.MessageContentPart{Type: lebro.ContentPartText, Text: "what is in this picture?"},
				lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/png", Data: imageData},
			)},
			want: []map[string]any{
				{"type": "text", "text": "what is in this picture?"},
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": imageData}},
			},
		},
		{
			name: "text image and pdf",
			message: lebro.Message{Role: lebro.RoleUser, ContentParts: partsFixture(t,
				lebro.MessageContentPart{Type: lebro.ContentPartText, Text: "compare"},
				lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/jpeg", Data: imageData},
				lebro.MessageContentPart{Type: lebro.ContentPartDocument, Filename: "report.pdf", MimeType: "application/pdf", Data: pdfData},
			)},
			want: []map[string]any{
				{"type": "text", "text": "compare"},
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": imageData}},
				{"type": "document", "title": "report.pdf", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": pdfData}},
			},
		},
		{
			name: "mixed-case image media type normalizes to the documented lowercase set",
			message: lebro.Message{Role: lebro.RoleUser, ContentParts: partsFixture(t,
				lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/PNG", Data: imageData},
			)},
			want: []map[string]any{
				{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": imageData}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params, err := model.params(lebro.ModelRequest{Model: "fixture-model", Messages: []lebro.Message{test.message}})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Messages []struct {
					Role    string           `json:"role"`
					Content []map[string]any `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
				t.Fatalf("messages = %#v", body.Messages)
			}
			content := body.Messages[0].Content
			if len(content) != len(test.want) {
				t.Fatalf("content = %#v, want %d blocks", content, len(test.want))
			}
			for i, block := range test.want {
				got := normalizeAnthropicBlock(content[i])
				wantJSON, _ := json.Marshal(block)
				gotJSON, _ := json.Marshal(got)
				if string(gotJSON) != string(wantJSON) {
					t.Fatalf("block %d = %s, want %s", i, gotJSON, wantJSON)
				}
			}
		})
	}
}

// normalizeAnthropicBlock drops keys the SDK may emit for elided defaults so
// assertions stay focused on the fields the mapping owns.
func normalizeAnthropicBlock(block map[string]any) map[string]any {
	filtered := map[string]any{}
	for key, value := range block {
		switch key {
		case "cache_control", "citations":
			continue
		}
		if source, ok := value.(map[string]any); ok {
			cleanSource := map[string]any{}
			for sourceKey, sourceValue := range source {
				if sourceKey == "cache_control" {
					continue
				}
				cleanSource[sourceKey] = sourceValue
			}
			filtered[key] = cleanSource
			continue
		}
		filtered[key] = value
	}
	return filtered
}

// The legacy text-only request must keep its single text block shape.
func TestParamsKeepsLegacyTextBlock(t *testing.T) {
	model, err := New(Config{APIKey: "key", Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	params, err := model.params(lebro.ModelRequest{Model: "fixture-model", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"content":[{"text":"hi","type":"text"}]`; !jsonCompactContains(string(encoded), want) {
		t.Fatalf("legacy user message changed: %s", encoded)
	}
}

// Anthropic accepts only four image media types; anything else fails locally
// with a normalized invalid-request ModelError naming the offending type.
func TestParamsRejectsUnsupportedImageMediaType(t *testing.T) {
	model, err := New(Config{APIKey: "key", Model: "fixture-model"})
	if err != nil {
		t.Fatal(err)
	}
	parts, err := lebro.NewMessageContentParts(lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/tiff", Data: "aW1hZ2UtYnl0ZXM="})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.params(lebro.ModelRequest{Model: "fixture-model", Messages: []lebro.Message{{Role: lebro.RoleUser, ContentParts: parts}}})
	modelErr, ok := err.(*lebro.ModelError)
	if !ok || modelErr.Kind != lebro.ModelErrorInvalidRequest {
		t.Fatalf("error = %v, want invalid-request model error", err)
	}
	if modelErr.Provider != "anthropic" || !strings.Contains(modelErr.Message, "image/tiff") {
		t.Fatalf("model error = %#v", modelErr)
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

// jsonCompactContains reports whether needle appears in the compact JSON
// encoding of haystack, tolerating SDK field-order differences.
func jsonCompactContains(haystack, needle string) bool {
	var value any
	if err := json.Unmarshal([]byte(haystack), &value); err != nil {
		return false
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return strings.Contains(string(compact), needle)
}
