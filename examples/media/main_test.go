package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	schema "github.com/tesh254/lebro/jsonschema"
)

type fixtureImages struct{ calls int }

func (f *fixtureImages) GenerateImages(ctx context.Context, r lebro.ImageRequest) (lebro.ImageResult, error) {
	f.calls++
	return lebro.ImageResult{Images: []lebro.MediaContent{{Asset: lebro.MediaAsset{Kind: lebro.MediaImage, MIMEType: "image/png"}, Data: []byte("fixture image")}}}, nil
}

type fixtureChat struct{}

func (fixtureChat) Generate(ctx context.Context, r lebro.ModelRequest) (lebro.ModelResponse, error) {
	for _, m := range r.Messages {
		if m.Role == lebro.RoleTool {
			return lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, Content: "Generated asset is ready."}, FinishReason: lebro.FinishReasonStop}, nil
		}
	}
	calls, e := lebro.NewModelToolCalls(lebro.ModelToolCall{ID: "call-image", ToolID: "generate_image", Arguments: json.RawMessage(`{"prompt":"draw a landscape"}`)})
	return lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: calls}, FinishReason: lebro.FinishReasonToolCalls}, e
}
func TestAgentImageToolAndTranscriptReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	images := &fixtureImages{}
	tool, e := lebro.NewImageTool(lebro.ImageToolConfig{Generator: images, Sink: diskSink{dir: dir}})
	if e != nil {
		t.Fatal(e)
	}
	registry, e := lebro.NewToolRegistry(schema.NewCompiler())
	if e != nil {
		t.Fatal(e)
	}
	if e = registry.Register(tool); e != nil {
		t.Fatal(e)
	}
	agent, e := lebro.NewAgent(lebro.AgentConfig{Definition: lebro.AgentDefinition{ID: "image-agent", Tools: []lebro.ToolID{"generate_image"}}, Model: fixtureChat{}, Tools: registry})
	if e != nil {
		t.Fatal(e)
	}
	result, e := agent.Run(ctx, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "draw"}}})
	if e != nil {
		t.Fatal(e)
	}
	if images.calls != 1 {
		t.Fatal("did not generate")
	}
	var artifact lebro.MediaArtifactResult
	for _, m := range result.Messages {
		if m.Role == lebro.RoleTool {
			if e = json.Unmarshal([]byte(m.Content), &artifact); e != nil {
				t.Fatal(e)
			}
		}
	}
	if len(artifact.Assets) != 1 {
		t.Fatalf("missing result %+v", result.Messages)
	}
	b, e := os.ReadFile(filepath.Join(dir, artifact.Assets[0].Locator))
	if e != nil || string(b) != "fixture image" {
		t.Fatal("missing stored file")
	}
	if _, e = agent.Run(ctx, lebro.RunInput{Messages: result.Messages}); e != nil {
		t.Fatal(e)
	}
	if images.calls != 1 {
		t.Fatal("transcript replay regenerated media")
	}
}
func TestDiskSinkPublishesAndBounds(t *testing.T) {
	dir := t.TempDir()
	sink := diskSink{dir: dir}
	a, e := sink.PutMedia(context.Background(), lebro.MediaOperation{ID: "test"}, lebro.MediaAsset{}, strings.NewReader("audio"))
	if e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(filepath.Join(dir, a.Locator))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	b, e := io.ReadAll(f)
	if e != nil || string(b) != "audio" {
		t.Fatal(e)
	}
}

func TestRunnableMediaFlowsWithProviderFixtures(t *testing.T) {
	var pngBytes bytes.Buffer
	_ = png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images", "/images/generations":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngBytes.Bytes())}}})
		case "/videos":
			w.WriteHeader(202)
			_, _ = io.WriteString(w, `{"id":"job","status":"pending"}`)
		case "/videos/job":
			_, _ = io.WriteString(w, `{"id":"job","status":"completed","unsigned_urls":["unused"]}`)
		case "/videos/job/content":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = io.WriteString(w, "video")
		case "/audio/transcriptions":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"text":"hello"}`)
		case "/audio/speech":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = io.WriteString(w, "audio")
		case "/chat/completions":
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Hello back"},"finish_reason":"stop"}]}`)
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("OPENROUTER_API_KEY", "fixture")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENROUTER_BASE_URL", server.URL)
	dir := t.TempDir()
	recording := filepath.Join(dir, "recording.wav")
	if e := os.WriteFile(recording, append([]byte("RIFF0000WAVE"), make([]byte, 100)...), 0600); e != nil {
		t.Fatal(e)
	}
	originalFlags, originalArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine = originalFlags; os.Args = originalArgs }()
	for _, tc := range []struct{ mode, provider, model string }{{"image", "openrouter", "openai/gpt-image-1"}, {"image", "openai", "gpt-image-1"}, {"video", "openrouter", "google/veo-3.1"}, {"resume", "openrouter", "google/veo-3.1"}, {"transcribe", "openai", "whisper-1"}, {"voice", "openai", "whisper-1"}, {"voice", "openrouter", "openai/whisper-1"}, {"speak", "openrouter", "openai/gpt-4o-mini-tts"}, {"speak-stream", "openrouter", "openai/gpt-4o-mini-tts"}, {"agent", "openrouter", "openai/gpt-image-1"}} {
		t.Run(tc.mode+tc.provider, func(t *testing.T) {
			dbPath := filepath.Join(dir, tc.mode+"-"+tc.provider+".db")
			opID := "example-op"
			if tc.mode == "resume" {
				opID = "resume-op"
				flag.CommandLine = flag.NewFlagSet("media", flag.ContinueOnError)
				os.Args = []string{"media", "-mode", "video", "-provider", tc.provider, "-model", tc.model, "-operation", opID, "-db", dbPath, "-output", filepath.Join(dir, "assets"), "-input", recording, "-chat-model", "chat"}
				if e := run(); e != nil {
					t.Fatal(e)
				}
			}
			flag.CommandLine = flag.NewFlagSet("media", flag.ContinueOnError)
			os.Args = []string{"media", "-mode", tc.mode, "-provider", tc.provider, "-model", tc.model, "-operation", opID, "-db", dbPath, "-output", filepath.Join(dir, "assets"), "-input", recording, "-chat-model", "chat"}
			if e := run(); e != nil {
				t.Fatal(e)
			}
		})
	}
	for _, args := range [][]string{{}, {"-mode", "resume", "-model", "google/veo-3.1"}, {"-mode", "unknown", "-model", "unknown"}, {"-mode", "image", "-provider", "invalid", "-model", "test"}, {"-mode", "transcribe", "-provider", "openai", "-model", "whisper-1", "-input", "missing-file"}} {
		flag.CommandLine = flag.NewFlagSet("media", flag.ContinueOnError)
		os.Args = append([]string{"media"}, args...)
		if e := run(); e == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
	if _, e := chatAgent("openai", "", nil, nil); e == nil {
		t.Fatal("accepted missing chat model")
	}
}
