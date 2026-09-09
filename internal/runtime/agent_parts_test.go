package runtime

import (
	"context"
	"strings"
	"testing"
)

// A user message with ordered content parts must reach the model intact and be
// persisted, and the persisted parts must replay into the next run's model
// request — proving parts cannot silently disappear at a persistence boundary.
func TestAgentRunsAndReplaysMessageContentParts(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	parts, err := NewMessageContentParts(
		MessageContentPart{Type: ContentPartText, Text: "what does the report say?"},
		MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: "aW1hZ2UtYnl0ZXM="},
		MessageContentPart{Type: ContentPartDocument, Filename: "report.pdf", MimeType: DocumentMimeTypePDF, Data: "cGRmLWJ5dGVz"},
	)
	if err != nil {
		t.Fatal(err)
	}

	var firstRequest ModelRequest
	first := &capturingModel{capture: &firstRequest, response: textResponse("the report says so").response}
	agent, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "parts-agent"},
		Model:      first,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(ctx, RunInput{
		ThreadID: "thread-parts-run",
		Messages: []Message{{Role: RoleUser, ContentParts: parts}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}

	seen := firstRequest.Messages[0].ContentParts.Values()
	if len(seen) != 3 ||
		seen[0].Type != ContentPartText || seen[0].Text != "what does the report say?" ||
		seen[1].Type != ContentPartImage || seen[1].MimeType != "image/png" || seen[1].Data != "aW1hZ2UtYnl0ZXM=" ||
		seen[2].Type != ContentPartDocument || seen[2].Filename != "report.pdf" || seen[2].Data != "cGRmLWJ5dGVz" {
		t.Fatalf("model received parts = %#v", seen)
	}

	// A second run on the same thread must replay the persisted multipart
	// message ahead of the new input.
	var secondRequest ModelRequest
	second := &capturingModel{capture: &secondRequest, response: textResponse("again").response}
	agent2, err := NewAgent(AgentConfig{
		Definition: AgentDefinition{ID: "parts-agent"},
		Model:      second,
		Store:      store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent2.Run(ctx, RunInput{
		ThreadID: "thread-parts-run",
		Messages: []Message{{Role: RoleUser, Content: "thanks"}},
	}); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}

	if len(secondRequest.Messages) != 3 {
		t.Fatalf("replayed transcript has %d messages, want 3: %#v", len(secondRequest.Messages), secondRequest.Messages)
	}
	replayed := secondRequest.Messages[0].ContentParts.Values()
	if len(replayed) != 3 || replayed[2].Filename != "report.pdf" || replayed[1].Data != "aW1hZ2UtYnl0ZXM=" {
		t.Fatalf("replayed parts = %#v", replayed)
	}
	if secondRequest.Messages[1].Role != RoleAssistant || secondRequest.Messages[2].Content != "thanks" {
		t.Fatalf("replayed transcript = %#v", secondRequest.Messages)
	}
}

// A parts-only user message must still produce a routing task for networks by
// falling back to its text parts.
func TestNetworkTaskFallsBackToTextParts(t *testing.T) {
	text, err := NewTextAttachmentPart("notes.md", "text/markdown", "deploy the service")
	if err != nil {
		t.Fatal(err)
	}
	parts, err := NewMessageContentParts(
		text,
		MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: "aW1hZ2UtYnl0ZXM="},
	)
	if err != nil {
		t.Fatal(err)
	}
	task, err := networkTask([]Message{{Role: RoleUser, ContentParts: parts}})
	if err != nil {
		t.Fatalf("networkTask() error = %v", err)
	}
	if task != text.Text {
		t.Fatalf("task = %q, want attachment text", task)
	}
	if _, err := networkTask([]Message{{Role: RoleUser, ContentParts: partsForTest(MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: "aW1hZ2UtYnl0ZXM="})}}); err == nil {
		t.Fatal("image-only parts must not yield a task")
	}
}

// Ambiguous input must fail with the documented mutual-exclusion error at the
// network boundary instead of being silently routed.
func TestNetworkTaskRejectsAmbiguousMessage(t *testing.T) {
	parts, err := NewMessageContentParts(MessageContentPart{Type: ContentPartText, Text: "attached"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = networkTask([]Message{{Role: RoleUser, Content: "text", ContentParts: parts}})
	if err == nil || !strings.Contains(err.Error(), "both content and content parts") {
		t.Fatalf("error = %v, want mutual-exclusion error", err)
	}
}

// A newest multipart user message without text parts is the actual latest
// turn: routing must fail rather than silently reuse an older user task.
func TestNetworkTaskDoesNotFallBackPastMultipartMessage(t *testing.T) {
	parts, err := NewMessageContentParts(MessageContentPart{Type: ContentPartImage, MimeType: "image/png", Data: "aW1hZ2UtYnl0ZXM="})
	if err != nil {
		t.Fatal(err)
	}
	_, err = networkTask([]Message{
		{Role: RoleUser, Content: "older task"},
		{Role: RoleAssistant, Content: "older answer"},
		{Role: RoleUser, ContentParts: parts},
	})
	if err == nil || !strings.Contains(err.Error(), "non-empty user message") {
		t.Fatalf("error = %v, want non-empty user message error", err)
	}
}
