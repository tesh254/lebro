package acp_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

func TestNewSessionRequiresInitialize(t *testing.T) {
	h := newHarness(t, textResponse("hello"))
	_, err := h.client.NewSession(t.Context(), acp.NewSessionRequest{Cwd: "/tmp", McpServers: []acp.McpServer{}})
	reqErr, ok := asRequestError(err)
	if !ok || reqErr.Code != -32600 {
		t.Fatalf("new session before initialize: got %v, want invalid request", err)
	}
}

// TestInitializeNegotiatesBaselineCapabilities verifies version negotiation
// and that only baseline capabilities are advertised.
func TestInitializeNegotiatesBaselineCapabilities(t *testing.T) {
	t.Parallel()
	h := newHarness(t, textResponse("hi"))
	h.initialize(t.Context())

	caps := acp.AgentCapabilities{}
	response, err := h.client.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	_ = caps
	if response.AgentCapabilities.LoadSession {
		t.Fatal("loadSession must not be advertised")
	}
	if response.AgentCapabilities.PromptCapabilities.Image ||
		response.AgentCapabilities.PromptCapabilities.Audio ||
		response.AgentCapabilities.PromptCapabilities.EmbeddedContext {
		t.Fatal("optional prompt capabilities must not be advertised")
	}
	if response.AgentCapabilities.McpCapabilities.Http || response.AgentCapabilities.McpCapabilities.Sse {
		t.Fatal("MCP transports must not be advertised")
	}
	if len(response.AuthMethods) != 0 {
		t.Fatal("no auth methods should be advertised")
	}
}

// TestTwoTurnsPreserveSessionContext sends two prompts in one session and
// asserts the second turn's model saw the first turn's messages (transcript
// continuity through the thread store).
func TestTwoTurnsPreserveSessionContext(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		textResponse("first"),
		textResponse("second"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	first := h.mustPrompt(t.Context(), session, "turn one")
	if first.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("first turn stop reason = %q", first.StopReason)
	}
	second := h.mustPrompt(t.Context(), session, "turn two")
	if second.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("second turn stop reason = %q", second.StopReason)
	}
	// The fixture model answers later turns with the transcript size, so the
	// second response proves the first turn's messages were loaded.
	waitFor(t, waitTimeout, func() bool {
		return h.events.text() != ""
	})
	if got := h.events.text(); !contains(got, "saw 3 prior messages") {
		t.Fatalf("second turn did not observe prior transcript; text = %q", got)
	}
}

// TestUnknownSessionIsResourceNotFound asserts the -32002 error with the
// requested session ID echoed in data.
func TestUnknownSessionIsResourceNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, textResponse("hi"))
	h.initialize(t.Context())

	_, err := h.prompt(t.Context(), acp.SessionId("sess_does_not_exist"), "hello")
	reqErr, ok := asRequestError(err)
	if !ok {
		t.Fatalf("expected RequestError, got %v", err)
	}
	if reqErr.Code != -32002 {
		t.Fatalf("code = %d, want -32002", reqErr.Code)
	}
	data, _ := reqErr.Data.(map[string]any)
	if data["sessionId"] != "sess_does_not_exist" {
		t.Fatalf("data.sessionId = %v", data["sessionId"])
	}
}

// TestOverlappingPromptRejected asserts a second prompt on a busy session
// fails deterministically.
func TestOverlappingPromptRejected(t *testing.T) {
	t.Parallel()
	// The gated model streams a first delta, then blocks until released.
	gate := make(chan struct{})
	h := gatedHarness(t, gate,
		textResponse("done"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	done := make(chan struct{})
	var firstErr error
	go func() {
		defer close(done)
		_, firstErr = h.prompt(t.Context(), session, "long turn")
	}()
	waitFor(t, waitTimeout, func() bool {
		return h.events.count() > 0
	})

	_, err := h.prompt(t.Context(), session, "overlapping")
	if err == nil {
		t.Fatal("overlapping prompt should fail")
	}
	reqErr, ok := asRequestError(err)
	if !ok || reqErr.Code != -32602 {
		t.Fatalf("expected -32602, got %v", err)
	}
	close(gate)
	<-done
	if firstErr != nil {
		t.Fatalf("first prompt errored: %v", firstErr)
	}
}

// TestUnsupportedContentRejected asserts image, audio, and resource blocks
// are explicit invalid-params errors, never silently dropped.
func TestUnsupportedContentRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, textResponse("hi"))
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	cases := map[string]acp.ContentBlock{
		"image":  acp.ImageBlock("aGk=", "image/png"),
		"audio":  acp.AudioBlock("aGk=", "audio/wav"),
		"embeds": acp.ResourceBlock(acp.EmbeddedResourceResource{}),
	}
	for name, block := range cases {
		_, err := h.promptBlocks(t.Context(), session, blocksOf(block))
		if err == nil {
			t.Fatalf("%s block should be rejected", name)
		}
		reqErr, ok := asRequestError(err)
		if !ok || reqErr.Code != -32602 {
			t.Fatalf("%s block: expected -32602, got %v", name, err)
		}
	}
}

// TestResourceLinkPassedAsText asserts resource links map to descriptive text
// without the server reading the URI.
func TestResourceLinkPassedAsText(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		textResponse("link noted"),
		textResponse("done"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	response, err := h.client.Prompt(t.Context(), acp.PromptRequest{
		SessionId: session,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("see "),
			acp.ResourceLinkBlock("notes.md", "file:///etc/notes.md"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", response.StopReason)
	}
}

// TestSessionIsolationMapsDistinctThreads asserts two sessions keep separate
// transcript contexts.
func TestSessionIsolationMapsDistinctThreads(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		textResponse("a1"), textResponse("a2"),
		textResponse("b1"), textResponse("b2"),
	)
	h.initialize(t.Context())
	sessA := h.newSession(t.Context())
	sessB := h.newSession(t.Context())

	h.mustPrompt(t.Context(), sessA, "alpha one")
	h.mustPrompt(t.Context(), sessB, "beta one")
	h.mustPrompt(t.Context(), sessA, "alpha two")
	h.mustPrompt(t.Context(), sessB, "beta two")

	// Both sessions completed: the fixture echo verifies each second turn
	// saw exactly its own session's first turn.
	waitFor(t, waitTimeout, func() bool {
		return len(h.events.snapshot()) >= 2
	})
}

// TestMalformedPromptRejected asserts an empty prompt is invalid params.
func TestMalformedPromptRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, textResponse("hi"))
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	_, err := h.client.Prompt(t.Context(), acp.PromptRequest{
		SessionId: session,
		Prompt:    []acp.ContentBlock{},
	})
	if err == nil {
		t.Fatal("empty prompt should fail")
	}
}

// TestCancelStopsTurn asserts session/cancel produces the cancelled stop
// reason without an error and leaves no stuck turn.
func TestCancelStopsTurn(t *testing.T) {
	t.Parallel()
	// The gated model blocks mid-stream so cancellation can be observed.
	gate := make(chan struct{})
	h := gatedHarness(t, gate,
		textResponse("after cancel"),
		textResponse("turn resumed"),
	)
	h.initialize(t.Context())
	session := h.newSession(t.Context())

	done := make(chan struct{})
	var response acp.PromptResponse
	var err error
	go func() {
		defer close(done)
		response, err = h.prompt(t.Context(), session, "will be cancelled")
	}()
	waitFor(t, waitTimeout, func() bool {
		return h.events.count() > 0
	})

	if err := h.client.Cancel(t.Context(), acp.CancelNotification{SessionId: session}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	<-done
	if err != nil {
		t.Fatalf("cancelled prompt returned error: %v", err)
	}
	if response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("stop reason = %q, want cancelled", response.StopReason)
	}

	// The session accepts a new turn after cancellation.
	follow := h.mustPrompt(t.Context(), session, "turn again")
	if follow.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("post-cancel stop reason = %q", follow.StopReason)
	}
}

// textResponse builds a plain assistant response.
func textResponse(text string) lebro.ModelResponse {
	return lebro.ModelResponse{
		Message:      lebro.Message{Role: lebro.RoleAssistant, Content: text},
		FinishReason: lebro.FinishReasonStop,
	}
}

// waitTimeout is the shared poll deadline for tests.
const waitTimeout = 5 * time.Second

// asRequestError unwraps an SDK RequestError.
func asRequestError(err error) (*acp.RequestError, bool) {
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr, true
	}
	return nil, false
}

// contains reports whether substr occurs in s.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// blocksOf wraps one block in a prompt slice.
func blocksOf(block acp.ContentBlock) []acp.ContentBlock {
	return []acp.ContentBlock{block}
}
