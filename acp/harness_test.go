package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"

	acpadapter "github.com/tesh254/lebro/acp"
)

// protectedTool records executions; tests assert at-most-once semantics.
type protectedTool struct {
	mu         sync.Mutex
	disabled   bool
	executions int
	lastArgs   string
}

func (t *protectedTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "acp_test.protected",
		Description: "A protected tool requiring approval",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
	}
}

func (t *protectedTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled {
		return nil, fmt.Errorf("acp test: handler executed while disabled")
	}
	t.executions++
	t.lastArgs = string(input)
	return json.RawMessage(`{"executed":true}`), nil
}

// executionCount returns the number of handler invocations.
func (t *protectedTool) executionCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.executions
}

// harness wires an in-process ACP client to a served lebro agent over io
// pipes, mirroring the SDK's own connection tests.
type harness struct {
	t      *testing.T
	client *acp.ClientSideConnection
	server *acpadapter.Server
	tool   *protectedTool
	model  *streamingFixtureModel
	events *updateLog
	pipes  [2]*io.PipeWriter
}

// start builds the server, agent, and pipe connections from a config.
func (h *harness) start(t *testing.T, agentConfig *lebro.AgentConfig) {
	t.Helper()
	server, err := acpadapter.NewServer(acpadapter.ServerConfig{
		AgentConfig: agentConfig,
		AgentInfo:   acp.Implementation{Name: "acp-test", Version: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ExposeAgent(); err != nil {
		t.Fatal(err)
	}
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()

	h.server = server
	h.client = acp.NewClientSideConnection(&clientFuncs{log: h.events}, c2aW, a2cR)
	server.ServeForTest(a2cW, c2aR)
	h.pipes = [2]*io.PipeWriter{c2aW, a2cW}

	t.Cleanup(func() {
		_ = c2aW.Close()
		_ = a2cW.Close()
	})
}

// hangUp closes both pipe writers, simulating a client disconnect while a
// permission request is pending.
func (h *harness) hangUp() {
	_ = h.pipes[0].Close()
	_ = h.pipes[1].Close()
}

// newExpiryHarness builds a harness whose approval policy sets a short TTL,
// so a decision submitted after the TTL lapses fails closed.
func newExpiryHarness(t *testing.T, steps ...lebro.ModelResponse) *harness {
	h := &harness{
		t:      t,
		tool:   &protectedTool{},
		events: newUpdateLog(),
	}
	store := lebro.NewMemoryStore()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	compiler := lebrojsonschema.NewCompiler()
	registry, err := lebro.NewToolRegistry(compiler)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(h.tool); err != nil {
		t.Fatal(err)
	}
	model := newStreamingModel(steps...)
	policy := lebro.ToolApprovalPolicyFunc(func(ctx context.Context, invocation lebro.ToolApprovalInvocation) (lebro.ToolApprovalResolution, error) {
		return lebro.ToolApprovalResolution{Outcome: lebro.ToolApprovalRequire, TTL: 10 * time.Millisecond}, nil
	})
	h.start(t, &lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "acp-test-agent",
			Instructions: "Test agent.",
			Tools:        []lebro.ToolID{"acp_test.protected"},
		},
		Model:              model,
		Tools:              registry,
		Store:              store,
		ToolApprovalPolicy: policy,
	})
	return h
}

type recordedUpdate struct {
	SessionID string
	Update    acp.SessionUpdate
}

// updateLog records session updates in delivery order.
type updateLog struct {
	mu      sync.Mutex
	updates []recordedUpdate
	// permission gates let tests script permission outcomes.
	allow chan string // option id, buffered
}

func newUpdateLog() *updateLog {
	return &updateLog{allow: make(chan string, 8)}
}

func (l *updateLog) record(notification acp.SessionNotification) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.updates = append(l.updates, recordedUpdate{SessionID: string(notification.SessionId), Update: notification.Update})
}

// snapshot copies the recorded updates.
func (l *updateLog) snapshot() []recordedUpdate {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedUpdate(nil), l.updates...)
}

// textUpdateCount returns how many updates were recorded.
func (l *updateLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.updates)
}

// text chunks collected in order.
func (l *updateLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out string
	for _, u := range l.updates {
		if u.Update.AgentMessageChunk != nil && u.Update.AgentMessageChunk.Content.Text != nil {
			out += u.Update.AgentMessageChunk.Content.Text.Text
		}
	}
	return out
}

// toolCalls returns (toolCallId, status) pairs in order.
func (l *updateLog) toolCalls() [][2]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out [][2]string
	for _, u := range l.updates {
		switch {
		case u.Update.ToolCall != nil:
			out = append(out, [2]string{string(u.Update.ToolCall.ToolCallId), string(u.Update.ToolCall.Status)})
		case u.Update.ToolCallUpdate != nil:
			status := ""
			if u.Update.ToolCallUpdate.Status != nil {
				status = string(*u.Update.ToolCallUpdate.Status)
			}
			out = append(out, [2]string{string(u.Update.ToolCallUpdate.ToolCallId), status})
		}
	}
	return out
}

// clientFuncs implements acp.Client; SessionUpdate records updates and
// RequestPermission consumes the scripted outcome.
type clientFuncs struct {
	log *updateLog
}

func (c *clientFuncs) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}

func (c *clientFuncs) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}

func (c *clientFuncs) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	c.log.record(params)
	return nil
}

func (c *clientFuncs) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	select {
	case optionID := <-c.log.allow:
		if optionID == "" {
			return acp.RequestPermissionResponse{}, acp.NewRequestCancelled(nil)
		}
		return acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{
				Selected: &acp.RequestPermissionOutcomeSelected{
					OptionId: acp.PermissionOptionId(optionID),
					Outcome:  "selected",
				},
			},
		}, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{
			Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}},
		}, nil
	}
}

// terminal methods required by the interface but unused in tests.
func (c *clientFuncs) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}
func (c *clientFuncs) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}
func (c *clientFuncs) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}
func (c *clientFuncs) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}
func (c *clientFuncs) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}

// newHarness starts a served agent over in-memory pipes. It returns the
// client connection and the handles tests need to script behavior.
func newHarness(t *testing.T, steps ...lebro.ModelResponse) *harness {
	t.Helper()

	store := lebro.NewMemoryStore()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	compiler := lebrojsonschema.NewCompiler()
	tool := &protectedTool{}
	registry, err := lebro.NewToolRegistry(compiler)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}

	model := newStreamingModel(steps...)
	agentConfig := &lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "acp-test-agent",
			Instructions: "Test agent.",
			Tools:        []lebro.ToolID{"acp_test.protected"},
		},
		Model: model,
		Tools: registry,
		Store: store,
	}
	h := &harness{
		t:      t,
		tool:   tool,
		model:  model,
		events: newUpdateLog(),
	}
	h.start(t, agentConfig)
	return h
}

// withGate returns a harness whose first model call blocks until the gate
// closes — used to hold a turn open across cancellations and overlaps.
func gatedHarness(t *testing.T, gate chan struct{}, steps ...lebro.ModelResponse) *harness {
	h := newHarness(t, steps...)
	h.model.mu.Lock()
	h.model.gate = gate
	h.model.mu.Unlock()
	return h
}

// newHarnessWithStore builds a harness backed by a caller-supplied store, for
// tests that need production-shaped persistence (SQLite) under concurrency.
func newHarnessWithStore(t *testing.T, store lebro.Store, steps ...lebro.ModelResponse) *harness {
	h := &harness{
		t:      t,
		tool:   &protectedTool{},
		events: newUpdateLog(),
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	compiler := lebrojsonschema.NewCompiler()
	registry, err := lebro.NewToolRegistry(compiler)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(h.tool); err != nil {
		t.Fatal(err)
	}
	model := newStreamingModel(steps...)
	h.model = model
	h.start(t, &lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "acp-test-agent",
			Instructions: "Test agent.",
			Tools:        []lebro.ToolID{"acp_test.protected"},
		},
		Model: model,
		Tools: registry,
		Store: store,
	})
	return h
}

// newSQLiteTestStore returns a private file-backed SQLite store closed when
// the test ends.
func newSQLiteTestStore(t *testing.T) *lebro.SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acp-test.db")
	store, err := lebro.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// initialize performs the handshake.
func (h *harness) initialize(ctx context.Context) {
	h.t.Helper()
	response, err := h.client.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "test-client", Version: "0"},
	})
	if err != nil {
		h.t.Fatalf("initialize: %v", err)
	}
	if response.ProtocolVersion != acp.ProtocolVersionNumber {
		h.t.Fatalf("unexpected protocol version %d", response.ProtocolVersion)
	}
}

// newSession creates a session.
func (h *harness) newSession(ctx context.Context) acp.SessionId {
	h.t.Helper()
	response, err := h.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/tmp", McpServers: []acp.McpServer{}})
	if err != nil {
		h.t.Fatalf("newSession: %v", err)
	}
	return response.SessionId
}

// prompt sends a text prompt and waits for the stop reason.
func (h *harness) prompt(ctx context.Context, session acp.SessionId, text string) (acp.PromptResponse, error) {
	return h.client.Prompt(ctx, acp.PromptRequest{
		SessionId: session,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
}

// mustPrompt sends a prompt and fails the test on error.
func (h *harness) mustPrompt(ctx context.Context, session acp.SessionId, text string) acp.PromptResponse {
	h.t.Helper()
	response, err := h.prompt(ctx, session, text)
	if err != nil {
		h.t.Fatalf("prompt: %v", err)
	}
	return response
}

// promptBlocks sends raw content blocks.
func (h *harness) promptBlocks(ctx context.Context, session acp.SessionId, blocks []acp.ContentBlock) (acp.PromptResponse, error) {
	return h.client.Prompt(ctx, acp.PromptRequest{
		SessionId: session,
		Prompt:    blocks,
	})
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
