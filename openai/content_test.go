package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

func TestGenerateMapsMessageContentParts(t *testing.T) {
	t.Parallel()

	imageData := "aW1hZ2UtYnl0ZXM="
	pdfData := "cGRmLWJ5dGVz"
	tests := []struct {
		name    string
		message lebro.Message
		want    []map[string]any
		wantRaw string
	}{
		{
			name: "text and image",
			message: lebro.Message{Role: lebro.RoleUser, ContentParts: partsFixture(t,
				// The wire body is produced by the request's top-level JSON
				// encoder, which HTML-escapes <, >, & — the same behavior as
				// the legacy text-only path. Pin the exact encoding so any
				// wire-escaping change is detected, while the parsed block
				// comparison below proves the decoded text is untouched.
				lebro.MessageContentPart{Type: lebro.ContentPartText, Text: `see <attachment> & "notes"`},
				lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/png", Data: imageData},
			)},
			want: []map[string]any{
				{"type": "text", "text": `see <attachment> & "notes"`},
				{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + imageData}},
			},
			wantRaw: `see \u003cattachment\u003e \u0026 \"notes\"`,
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
				{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + imageData}},
				{"type": "file", "file": map[string]any{"filename": "report.pdf", "file_data": "data:application/pdf;base64," + pdfData}},
			},
			wantRaw: pdfData,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var observed observeRequest
			server := newRecordedServer(t, &observed, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, chatResponse{
					Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"done"`)}, FinishReason: "stop"}},
				})
			})
			model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
			_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{test.message}})
			if err != nil {
				t.Fatal(err)
			}
			body := observed.body(t)
			messages, _ := body["messages"].([]any)
			if len(messages) != 1 {
				t.Fatalf("messages = %#v", messages)
			}
			first, _ := messages[0].(map[string]any)
			if first["role"] != "user" {
				t.Fatalf("role = %#v", first["role"])
			}
			content, _ := first["content"].([]any)
			if len(content) != len(test.want) {
				t.Fatalf("content = %#v, want %d blocks", content, len(test.want))
			}
			for i, block := range test.want {
				got, _ := content[i].(map[string]any)
				assertDeepEqual(t, got, block)
			}
			// The raw wire body must match the pinned encoding exactly, so
			// any change to how part text travels is caught.
			if test.wantRaw != "" && !strings.Contains(string(observed.raw), test.wantRaw) {
				t.Fatalf("expected raw payload missing from body: %s", observed.raw)
			}
		})
	}
}

// The legacy text-only request must remain byte-identical: content stays a
// JSON string and no content-parts machinery appears on the wire.
func TestGenerateKeepsLegacyTextContentByteIdentical(t *testing.T) {
	t.Parallel()

	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, chatResponse{
			Choices: []chatChoice{{Message: chatChoiceMessage{Role: "assistant", Content: json.RawMessage(`"done"`)}, FinishReason: "stop"}},
		})
	})
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
	_, err := model.Generate(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(observed.raw)
	if !strings.Contains(raw, `"messages":[{"role":"user","content":"hi"}]`) {
		t.Fatalf("legacy content changed: %s", raw)
	}
	if strings.Contains(raw, "content_parts") || strings.Contains(raw, `[{"type":`) {
		t.Fatalf("content-parts machinery leaked into legacy request: %s", raw)
	}
}

// A streamed request must map multipart content exactly like Generate: both
// paths share the message mapping.
func TestStreamMapsMessageContentParts(t *testing.T) {
	t.Parallel()

	var observed observeRequest
	server := newRecordedServer(t, &observed, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	model := newAdapter(t, server, Config{APIKey: "k", Model: "gpt-4o"})
	reader, err := model.Stream(context.Background(), lebro.ModelRequest{Model: "gpt-4o", Messages: []lebro.Message{{
		Role: lebro.RoleUser,
		ContentParts: partsFixture(t,
			lebro.MessageContentPart{Type: lebro.ContentPartText, Text: "look"},
			lebro.MessageContentPart{Type: lebro.ContentPartImage, MimeType: "image/png", Data: "aW1hZ2UtYnl0ZXM="},
		),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	for {
		if _, err := reader.Next(); err != nil {
			break
		}
	}
	body := observed.body(t)
	messages, _ := body["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("streamed content = %#v", content)
	}
	imageBlock, _ := content[1].(map[string]any)
	if imageBlock["type"] != "image_url" {
		t.Fatalf("image block = %#v", imageBlock)
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

func assertDeepEqual(t *testing.T, got, want map[string]any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("block = %s, want %s", gotJSON, wantJSON)
	}
}
