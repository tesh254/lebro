package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
	"github.com/tesh254/lebro/mcp"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type echoTool struct{}

func (echoTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "echo",
		Description: "Echo back the input value",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["value"],
			"properties":{"value":{"type":"string"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["echo"],
			"properties":{"echo":{"type":"string"}},
			"additionalProperties":false
		}`),
	}
}

func (echoTool) Execute(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"echo": args.Value})
}

type failingTool struct{}

func (failingTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "fail",
		Description: "Always fails",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (failingTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("intentional failure")
}

func newTestServer(t *testing.T) *mcp.Server {
	t.Helper()
	return mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "test-server", Version: "test"},
		AuthorizeWorkflowResume: func(context.Context, lebro.RunID) error {
			return nil
		},
	})
}

func connectServer(t *testing.T, srv *mcp.Server) (*mcpsdk.ClientSession, func()) {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	// Server must connect first (SDK requirement).
	serverSession, err := srv.Connect(ctx, serverTransport)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	// Then client connects.
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	cleanup := func() {
		_ = session.Close()
		_ = serverSession.Close()
	}
	return session, cleanup
}

func toolNames(t *testing.T, session *mcpsdk.ClientSession) []string {
	t.Helper()
	ctx := context.Background()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callTool(t *testing.T, session *mcpsdk.ClientSession, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool %q: %v", name, err)
	}
	return result
}

func TestExposeTool_Success(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(echoTool{}))
	tool, ok := registry.Resolve("echo")
	if !ok {
		t.Fatal("resolve echo tool")
	}
	if err := srv.ExposeTool(tool); err != nil {
		t.Fatalf("ExposeTool: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 1 || names[0] != "echo" {
		t.Fatalf("expected [echo], got %v", names)
	}

	result := callTool(t, session, "echo", map[string]any{"value": "hello"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
	var output struct {
		Echo string `json:"echo"`
	}
	must(t, json.Unmarshal(json.RawMessage(contentText(result)), &output))
	if output.Echo != "hello" {
		t.Fatalf("expected echo=hello, got %q", output.Echo)
	}
}

func TestExposeTool_NonJSONOutputIsTextOnly(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(rawOutputTool{}))
	tool, _ := registry.Resolve("raw_output")
	must(t, srv.ExposeTool(tool))

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "raw_output", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
	if result.StructuredContent != nil {
		t.Fatalf("StructuredContent = %v, want nil for non-JSON output", result.StructuredContent)
	}
	if got := contentText(result); got != "not-json" {
		t.Fatalf("text content = %q, want %q", got, "not-json")
	}
}

func TestExposeTool_AllowListEnforced(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(echoTool{}))
	must(t, registry.Register(failingTool{}))

	echoRegistered, _ := registry.Resolve("echo")
	if err := srv.ExposeTool(echoRegistered); err != nil {
		t.Fatalf("ExposeTool: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 1 {
		t.Fatalf("expected 1 tool, got %d: %v", len(names), names)
	}
	if names[0] != "echo" {
		t.Fatalf("expected [echo], got %v", names)
	}
}

func TestExposeTool_DuplicateRejected(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(echoTool{}))
	tool, _ := registry.Resolve("echo")
	if err := srv.ExposeTool(tool); err != nil {
		t.Fatalf("first ExposeTool: %v", err)
	}
	err := srv.ExposeTool(tool)
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if !strings.Contains(err.Error(), "already exposed") {
		t.Fatalf("expected 'already exposed', got %q", err)
	}
}

func TestExposeTool_NilTool(t *testing.T) {
	srv := newTestServer(t)
	err := srv.ExposeTool(nil)
	if err == nil {
		t.Fatal("expected nil error")
	}
}

func TestExposeTool_HandlerError(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(failingTool{}))
	tool, _ := registry.Resolve("fail")
	if err := srv.ExposeTool(tool); err != nil {
		t.Fatalf("ExposeTool: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "fail", map[string]any{})
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	text := contentText(result)
	if text != "lebro/mcp: tool execution failed" {
		t.Fatalf("error content = %q", text)
	}
}

func TestExposeTool_InvalidInput(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(echoTool{}))
	tool, _ := registry.Resolve("echo")
	if err := srv.ExposeTool(tool); err != nil {
		t.Fatalf("ExposeTool: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "echo", map[string]any{"wrong_field": "x"})
	if !result.IsError {
		t.Fatal("expected IsError=true for invalid input")
	}
}

func TestExposeTool_Cancellation(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	cancelTool := &cancelCheckTool{started: make(chan struct{})}
	must(t, registry.Register(cancelTool))
	tool, _ := registry.Resolve("cancel_check")
	if err := srv.ExposeTool(tool); err != nil {
		t.Fatalf("ExposeTool: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := callToolAsync(session, ctx, "cancel_check", map[string]any{})
	waitForStart(t, cancelTool.started)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("CallTool error = %v, want context.Canceled", err)
	}
}

func TestExposeTool_DeadlineExceeded(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(deadlineTool{}))
	tool, _ := registry.Resolve("deadline")
	must(t, srv.ExposeTool(tool))

	session, cleanup := connectServer(t, srv)
	defer cleanup()
	_, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "deadline",
		Arguments: map[string]any{},
	})
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("CallTool error = %v, want deadline exceeded", err)
	}
}

func TestExposeAgent_Cancellation(t *testing.T) {
	srv := newTestServer(t)
	model := &cancellationModel{started: make(chan struct{})}
	agent := mustAgent(t, model, "cancel-agent", "")
	if err := srv.ExposeAgent(agent); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := callToolAsync(session, ctx, "agent.cancel-agent", map[string]any{
		"messages": []map[string]any{{"content": "hi"}},
	})
	waitForStart(t, model.started)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("CallTool error = %v, want context.Canceled", err)
	}
}

func TestExposeWorkflow_Cancellation(t *testing.T) {
	srv := newTestServer(t)
	started := make(chan struct{})
	wf := mustCancelWorkflow(t, "cancel-wf", started)
	if err := srv.ExposeWorkflow(wf); err != nil {
		t.Fatalf("ExposeWorkflow: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := callToolAsync(session, ctx, "workflow.cancel-wf", map[string]any{
		"input": map[string]any{},
	})
	waitForStart(t, started)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("CallTool error = %v, want context.Canceled", err)
	}
}

func TestExposeTool_EmptyInputSchema(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	noSchemaTool := &noSchemaTestTool{}
	must(t, registry.Register(noSchemaTool))
	tool, _ := registry.Resolve("no_schema")
	if err := srv.ExposeTool(tool); err != nil {
		t.Fatalf("ExposeTool with empty schema: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "no_schema", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
}

func TestExposeTool_NonObjectSchemaRejected(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	badSchemaTool := &nonObjectSchemaTestTool{}
	must(t, registry.Register(badSchemaTool))
	tool, _ := registry.Resolve("bad_schema")
	err := srv.ExposeTool(tool)
	if err == nil {
		t.Fatal("expected error for non-object input schema")
	}
	if !strings.Contains(err.Error(), "must be") {
		t.Fatalf("expected 'must be' in error, got %q", err)
	}
}

func TestExposeTool_SchemaFailureDoesNotReserveName(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	badSchemaTool := &nonObjectSchemaTestTool{}
	must(t, registry.Register(badSchemaTool))
	tool, _ := registry.Resolve("bad_schema")
	_ = srv.ExposeTool(tool)
	err := srv.ExposeTool(tool)
	if err == nil || !strings.Contains(err.Error(), "must be") {
		t.Fatalf("expected schema error on retry, got %v", err)
	}
}

func TestExposeAgent_Success(t *testing.T) {
	srv := newTestServer(t)
	model := testkit.NewModel(testkit.Text("Hello from agent"))
	agent := mustAgent(t, model, "assistant", "You are a helpful assistant.")
	if err := srv.ExposeAgent(agent); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 1 || names[0] != "agent.assistant" {
		t.Fatalf("expected [agent.assistant], got %v", names)
	}

	result := callTool(t, session, "agent.assistant", map[string]any{
		"messages": []map[string]any{
			{"content": "Hi"},
		},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
	text := contentText(result)
	if !strings.Contains(text, "Hello from agent") {
		t.Fatalf("expected agent response, got %q", text)
	}
}

func TestExposeAgent_StructuredOutputHasTextFallback(t *testing.T) {
	srv := newTestServer(t)
	model := testkit.NewModel(testkit.StructuredOutput(json.RawMessage(`{"answer":42}`)))
	agent := mustAgent(t, model, "structured", "")
	must(t, srv.ExposeAgent(agent))

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "agent.structured", map[string]any{
		"messages": []map[string]any{{"content": "answer"}},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
	if got := contentText(result); got != `{"answer":42}` {
		t.Fatalf("text content = %q, want structured JSON", got)
	}
}

func TestExposeAgent_DuplicateRejected(t *testing.T) {
	srv := newTestServer(t)
	model := testkit.NewModel(testkit.Text("Hi"))
	agent := mustAgent(t, model, "dup", "")
	if err := srv.ExposeAgent(agent); err != nil {
		t.Fatalf("first ExposeAgent: %v", err)
	}
	err := srv.ExposeAgent(agent)
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestExposeAgent_NilAgent(t *testing.T) {
	srv := newTestServer(t)
	err := srv.ExposeAgent(nil)
	if err == nil {
		t.Fatal("expected nil error")
	}
}

func TestExposeAgent_AllowListEnforced(t *testing.T) {
	srv := newTestServer(t)
	model1 := testkit.NewModel(testkit.Text("A"))
	model2 := testkit.NewModel(testkit.Text("B"))
	agent1 := mustAgent(t, model1, "alpha", "")
	if err := srv.ExposeAgent(agent1); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}
	agent2 := mustAgent(t, model2, "beta", "")

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 1 || names[0] != "agent.alpha" {
		t.Fatalf("expected [agent.alpha], got %v", names)
	}

	_ = agent2
}

func TestExposeAgent_ProviderFailure(t *testing.T) {
	srv := newTestServer(t)
	model := testkit.NewModel(testkit.Failure(errors.New("provider down")))
	agent := mustAgent(t, model, "broken", "")
	if err := srv.ExposeAgent(agent); err != nil {
		t.Fatalf("ExposeAgent: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "agent.broken", map[string]any{
		"messages": []map[string]any{
			{"content": "Hi"},
		},
	})
	if !result.IsError {
		t.Fatal("expected IsError=true for provider failure")
	}
}

func TestExposeWorkflow_Success(t *testing.T) {
	srv := newTestServer(t)
	wf := mustWorkflow(t, "sum")

	if err := srv.ExposeWorkflow(wf); err != nil {
		t.Fatalf("ExposeWorkflow: %v", err)
	}
	if err := srv.ExposeWorkflowResume(wf); err != nil {
		t.Fatalf("ExposeWorkflowResume: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 2 {
		t.Fatalf("expected 2 tools (run + resume), got %d: %v", len(names), names)
	}
	if names[0] != "workflow.sum" {
		t.Fatalf("expected workflow.sum, got %q", names[0])
	}
	if names[1] != "workflow.sum.resume" {
		t.Fatalf("expected workflow.sum.resume, got %q", names[1])
	}

	result := callTool(t, session, "workflow.sum", map[string]any{
		"input": map[string]any{"a": 3, "b": 4},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
	var output struct {
		Sum int `json:"sum"`
	}
	must(t, json.Unmarshal(json.RawMessage(contentText(result)), &output))
	if output.Sum != 7 {
		t.Fatalf("expected sum=7, got %d", output.Sum)
	}
}

func TestExposeWorkflow_InvalidArguments(t *testing.T) {
	srv := newTestServer(t)
	wf := mustWorkflow(t, "validated")
	must(t, srv.ExposeWorkflow(wf))
	must(t, srv.ExposeWorkflowResume(wf))

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	runResult := callTool(t, session, "workflow.validated", map[string]any{"unknown": true})
	if !runResult.IsError {
		t.Fatal("expected IsError=true for unknown workflow argument")
	}
	resumeResult := callTool(t, session, "workflow.validated.resume", map[string]any{})
	if !resumeResult.IsError {
		t.Fatal("expected IsError=true for missing resume run_id")
	}
}

func TestExposeWorkflow_NonJSONOutputIsTextOnly(t *testing.T) {
	srv := newTestServer(t)
	wf := mustRawOutputWorkflow(t, "raw-workflow")
	must(t, srv.ExposeWorkflow(wf))

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	result := callTool(t, session, "workflow.raw-workflow", map[string]any{"input": map[string]any{}})
	if result.IsError {
		t.Fatalf("unexpected error: %s", contentText(result))
	}
	if result.StructuredContent != nil {
		t.Fatalf("StructuredContent = %v, want nil for non-JSON output", result.StructuredContent)
	}
	if got := contentText(result); got != "not-json" {
		t.Fatalf("text content = %q, want %q", got, "not-json")
	}
}

func TestExposeWorkflow_ResumeSeparateRegistration(t *testing.T) {
	srv := newTestServer(t)
	wf := mustWorkflow(t, "sep")

	if err := srv.ExposeWorkflow(wf); err != nil {
		t.Fatalf("ExposeWorkflow: %v", err)
	}

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 1 {
		t.Fatalf("expected 1 tool (run only), got %d: %v", len(names), names)
	}
	if names[0] != "workflow.sep" {
		t.Fatalf("expected workflow.sep, got %q", names[0])
	}
}

func TestExposeWorkflow_DuplicateRejected(t *testing.T) {
	srv := newTestServer(t)
	wf := mustWorkflow(t, "dup")
	if err := srv.ExposeWorkflow(wf); err != nil {
		t.Fatalf("first ExposeWorkflow: %v", err)
	}
	err := srv.ExposeWorkflow(wf)
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestExposeWorkflowResume_DuplicateRejected(t *testing.T) {
	srv := newTestServer(t)
	wf := mustWorkflow(t, "dupr")
	must(t, srv.ExposeWorkflow(wf))
	must(t, srv.ExposeWorkflowResume(wf))
	err := srv.ExposeWorkflowResume(wf)
	if err == nil {
		t.Fatal("expected duplicate error for resume")
	}
}

func TestExposeWorkflowResume_WithoutExposeWorkflow(t *testing.T) {
	srv := newTestServer(t)
	wf := mustWorkflow(t, "noresume")
	err := srv.ExposeWorkflowResume(wf)
	if err == nil {
		t.Fatal("expected error when resume called without prior ExposeWorkflow")
	}
	if !strings.Contains(err.Error(), "requires ExposeWorkflow") {
		t.Fatalf("expected 'requires ExposeWorkflow' error, got %q", err)
	}
}

func TestExposeWorkflowResume_RequiresAuthorizer(t *testing.T) {
	srv := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "test-server", Version: "test"},
	})
	wf := mustWorkflow(t, "authorize")
	must(t, srv.ExposeWorkflow(wf))
	err := srv.ExposeWorkflowResume(wf)
	if err == nil || !strings.Contains(err.Error(), "AuthorizeWorkflowResume is required") {
		t.Fatalf("ExposeWorkflowResume error = %v", err)
	}
}

func TestExposeWorkflowResume_AuthorizationDenied(t *testing.T) {
	srv := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "test-server", Version: "test"},
		AuthorizeWorkflowResume: func(context.Context, lebro.RunID) error {
			return errors.New("denied")
		},
	})
	wf := mustWorkflow(t, "denied")
	must(t, srv.ExposeWorkflow(wf))
	must(t, srv.ExposeWorkflowResume(wf))

	session, cleanup := connectServer(t, srv)
	defer cleanup()
	result := callTool(t, session, "workflow.denied.resume", map[string]any{"run_id": "run-1"})
	if !result.IsError || contentText(result) != "lebro/mcp: workflow resume not authorized" {
		t.Fatalf("resume result = %#v", result)
	}
}

func TestExposeWorkflow_NilWorkflow(t *testing.T) {
	srv := newTestServer(t)
	err := srv.ExposeWorkflow(nil)
	if err == nil {
		t.Fatal("expected nil error")
	}
}

func TestExposeWorkflow_AllowListEnforced(t *testing.T) {
	srv := newTestServer(t)
	wf1 := mustWorkflow(t, "alpha")
	if err := srv.ExposeWorkflow(wf1); err != nil {
		t.Fatalf("ExposeWorkflow: %v", err)
	}
	wf2 := mustWorkflow(t, "beta")

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	if len(names) != 1 {
		t.Fatalf("expected 1 tool for 1 workflow, got %d: %v", len(names), names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "workflow.alpha") {
			t.Fatalf("expected only workflow.alpha*, got %q", n)
		}
	}
	_ = wf2
}

func TestServer_NilImplementation(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil Implementation")
		}
	}()
	mcp.NewServer(mcp.ServerConfig{})
}

func TestServer_NegativePageSizePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for negative PageSize")
		}
	}()
	mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "test", Version: "test"},
		PageSize:       -1,
	})
}

func TestConnect_ReturnsSession(t *testing.T) {
	srv := newTestServer(t)
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	ctx := context.Background()
	session, err := srv.Connect(ctx, serverTransport)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if session == nil {
		t.Fatal("Connect returned nil session")
	}
	_ = session.Close()
	_ = clientTransport
}

func TestNoToolsByDefault(t *testing.T) {
	srv := newTestServer(t)
	session, cleanup := connectServer(t, srv)
	defer cleanup()
	names := toolNames(t, session)
	if len(names) != 0 {
		t.Fatalf("expected 0 tools by default, got %v", names)
	}
}

func TestMixedPrimitives(t *testing.T) {
	srv := newTestServer(t)

	registry := mustRegistry(t)
	must(t, registry.Register(echoTool{}))
	tool, _ := registry.Resolve("echo")
	must(t, srv.ExposeTool(tool))

	model := testkit.NewModel(testkit.Text("agent response"))
	agent := mustAgent(t, model, "myagent", "")
	must(t, srv.ExposeAgent(agent))

	wf := mustWorkflow(t, "myworkflow")
	must(t, srv.ExposeWorkflow(wf))
	must(t, srv.ExposeWorkflowResume(wf))

	session, cleanup := connectServer(t, srv)
	defer cleanup()

	names := toolNames(t, session)
	expected := []string{"agent.myagent", "echo", "workflow.myworkflow", "workflow.myworkflow.resume"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d tools, got %d: %v", len(expected), len(names), names)
	}
	for _, exp := range expected {
		found := false
		for _, n := range names {
			if n == exp {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected tool %q in %v", exp, names)
		}
	}
}

// --- helpers ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustRegistry(t *testing.T) *lebro.ToolRegistry {
	t.Helper()
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	return registry
}

func mustAgent(t *testing.T, model lebro.Model, id string, instructions string) *lebro.Agent {
	t.Helper()
	agent, err := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           lebro.AgentID(id),
			Name:         id,
			Instructions: instructions,
			Model:        "test-model",
		},
		Model:    model,
		MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

func mustWorkflow(t *testing.T, id string) *lebro.LinearWorkflow {
	t.Helper()
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{
			ID:          lebro.WorkflowID(id),
			Name:        id,
			Description: "A test workflow that adds two numbers",
		},
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID:   "add",
					Name: "Add",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["a","b"],
						"properties":{"a":{"type":"integer"},"b":{"type":"integer"}},
						"additionalProperties":false
					}`),
					OutputSchema: json.RawMessage(`{
						"type":"object",
						"required":["sum"],
						"properties":{"sum":{"type":"integer"}},
						"additionalProperties":false
					}`),
				},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var args struct {
						A int `json:"a"`
						B int `json:"b"`
					}
					if err := json.Unmarshal(input, &args); err != nil {
						return nil, err
					}
					return json.Marshal(map[string]int{"sum": args.A + args.B})
				}),
			},
		},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow: %v", err)
	}
	return wf
}

func mustCancelWorkflow(t *testing.T, id string, started chan struct{}) *lebro.LinearWorkflow {
	t.Helper()
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{
			ID:          lebro.WorkflowID(id),
			Name:        id,
			Description: "A test workflow that blocks on context cancellation",
		},
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID:          "block",
					Name:        "Block",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				Handler: lebro.StepHandlerFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
					close(started)
					<-ctx.Done()
					return nil, ctx.Err()
				}),
			},
		},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow: %v", err)
	}
	return wf
}

func mustRawOutputWorkflow(t *testing.T, id string) *lebro.LinearWorkflow {
	t.Helper()
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: lebro.WorkflowID(id), Name: id},
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{ID: "raw", Name: "Raw"},
			Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage("not-json"), nil
			}),
		}},
	})
	if err != nil {
		t.Fatalf("NewLinearWorkflow: %v", err)
	}
	return wf
}

func contentText(result *mcpsdk.CallToolResult) string {
	for _, c := range result.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func callToolAsync(session *mcpsdk.ClientSession, ctx context.Context, name string, arguments map[string]any) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: arguments})
		errCh <- err
	}()
	return errCh
}

func waitForStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}
}

// cancelCheckTool blocks until context cancellation then returns cancelled.
type cancelCheckTool struct {
	started chan struct{}
}

func (cancelCheckTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "cancel_check",
		Description: "Checks context cancellation",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (t *cancelCheckTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	close(t.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type cancellationModel struct {
	started chan struct{}
	once    sync.Once
}

type deadlineTool struct{}

func (deadlineTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "deadline",
		Description: "Returns a deadline error",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (deadlineTool) Execute(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, context.DeadlineExceeded
}

func (m *cancellationModel) Generate(ctx context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	return lebro.ModelResponse{}, ctx.Err()
}

// noSchemaTestTool has no InputSchema or OutputSchema.
type noSchemaTestTool struct{}

func (noSchemaTestTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "no_schema",
		Description: "Tool with no schemas",
	}
}

func (noSchemaTestTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"ok": "true"})
}

// rawOutputTool has no output schema and deliberately returns non-JSON bytes.
type rawOutputTool struct{}

func (rawOutputTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "raw_output",
		Description: "Returns unchecked raw output",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (rawOutputTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage("not-json"), nil
}

// nonObjectSchemaTestTool has a non-object input schema that should be rejected.
type nonObjectSchemaTestTool struct{}

func (nonObjectSchemaTestTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "bad_schema",
		Description: "Tool with invalid schema type",
		InputSchema: json.RawMessage(`{"type":"string"}`),
	}
}

func (nonObjectSchemaTestTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"ok": "true"})
}
