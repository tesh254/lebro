package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/mcp"
)

// newRemoteServer builds a plain SDK server standing in for a third-party MCP
// server, so client tests exercise a real protocol round-trip rather than a
// hand-rolled fake.
func newRemoteServer(t *testing.T, configure func(*mcpsdk.Server)) *mcpsdk.Server {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "remote-server", Version: "test"}, nil)
	if configure != nil {
		configure(server)
	}
	return server
}

// connectClient wires a lebro Client to an SDK server over in-memory
// transports and returns the connected client.
func connectClient(t *testing.T, server *mcpsdk.Server, serverName string) *mcp.Client {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "lebro-client", Version: "test"},
		ServerName:     serverName,
	})
	if err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("client connect: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = serverSession.Close()
	})
	return client
}

// addRemoteEcho registers an echo tool with declared input and output schemas.
func addRemoteEcho(server *mcpsdk.Server) {
	server.AddTool(&mcpsdk.Tool{
		Name:        "echo",
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
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(map[string]string{"echo": args.Value})
		if err != nil {
			return nil, err
		}
		return &mcpsdk.CallToolResult{
			Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: string(payload)}},
			StructuredContent: json.RawMessage(payload),
		}, nil
	})
}

func discoverOne(t *testing.T, client *mcp.Client, id string) lebro.Tool {
	t.Helper()
	tools, err := client.DiscoverTools(context.Background())
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	for _, tool := range tools {
		if string(tool.Definition().ID) == id {
			return tool
		}
	}
	t.Fatalf("tool %q not discovered; got %d tools", id, len(tools))
	return nil
}

// registerAndResolve puts a discovered tool through the same registry boundary
// application code would use, which is what makes the schema checks apply.
func registerAndResolve(t *testing.T, tool lebro.Tool) *lebro.RegisteredTool {
	t.Helper()
	registry := mustRegistry(t)
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	registered, ok := registry.Resolve(tool.Definition().ID)
	if !ok {
		t.Fatalf("Resolve %q: not found", tool.Definition().ID)
	}
	return registered
}

func TestDiscoverTools_AdaptsRemoteDefinition(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	client := connectClient(t, server, "remote")

	tools, err := client.DiscoverTools(context.Background())
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("discovered %d tools, want 1", len(tools))
	}

	def := tools[0].Definition()
	if def.ID != "remote.echo" {
		t.Errorf("ID = %q, want %q", def.ID, "remote.echo")
	}
	if def.Description != "Echo back the input value" {
		t.Errorf("Description = %q", def.Description)
	}

	var input map[string]any
	if err := json.Unmarshal(def.InputSchema, &input); err != nil {
		t.Fatalf("InputSchema is not valid JSON: %v", err)
	}
	if input["type"] != "object" {
		t.Errorf("InputSchema type = %v, want object", input["type"])
	}
	if len(def.OutputSchema) == 0 {
		t.Error("OutputSchema is empty; the server advertised one")
	}
}

func TestDiscoverTools_RegistersInToolRegistry(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	client := connectClient(t, server, "remote")

	registered := registerAndResolve(t, discoverOne(t, client, "remote.echo"))
	if got := registered.Definition().ID; got != "remote.echo" {
		t.Errorf("registered ID = %q, want %q", got, "remote.echo")
	}
}

func TestRemoteTool_RoundTripPreservesArgumentsAndResult(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.echo"))

	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"value":"hello"}`),
	})
	if result.State != lebro.ToolExecutionSucceeded {
		t.Fatalf("State = %q, want %q (err: %v)", result.State, lebro.ToolExecutionSucceeded, result.Err)
	}

	var output struct {
		Echo string `json:"echo"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if output.Echo != "hello" {
		t.Errorf("echo = %q, want %q", output.Echo, "hello")
	}
}

func TestRemoteTool_InvalidArgumentsRejectedLocally(t *testing.T) {
	var called bool
	server := newRemoteServer(t, func(s *mcpsdk.Server) {
		s.AddTool(&mcpsdk.Tool{
			Name: "strict",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"required":["value"],
				"properties":{"value":{"type":"string"}},
				"additionalProperties":false
			}`),
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			called = true
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "{}"}},
			}, nil
		})
	})
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.strict"))

	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"value":42}`),
	})
	if result.State != lebro.ToolExecutionInvalidInput {
		t.Fatalf("State = %q, want %q", result.State, lebro.ToolExecutionInvalidInput)
	}
	if called {
		t.Error("remote handler ran; invalid arguments should not reach the wire")
	}
}

func TestRemoteTool_OutputValidatedAgainstAdvertisedSchema(t *testing.T) {
	server := newRemoteServer(t, func(s *mcpsdk.Server) {
		s.AddTool(&mcpsdk.Tool{
			Name:        "liar",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{
				"type":"object",
				"required":["echo"],
				"properties":{"echo":{"type":"string"}},
				"additionalProperties":false
			}`),
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{
				Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"unexpected":true}`}},
				StructuredContent: json.RawMessage(`{"unexpected":true}`),
			}, nil
		})
	})
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.liar"))

	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{}`),
	})
	if result.State != lebro.ToolExecutionInvalidOutput {
		t.Fatalf("State = %q, want %q (err: %v)", result.State, lebro.ToolExecutionInvalidOutput, result.Err)
	}
}

// newTextServer builds a server whose tool declares no output schema and
// replies with the given content.
func newTextServer(t *testing.T, content ...mcpsdk.Content) *mcpsdk.Server {
	t.Helper()
	return newRemoteServer(t, func(s *mcpsdk.Server) {
		s.AddTool(&mcpsdk.Tool{
			Name:        "freeform",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: content}, nil
		})
	})
}

// textEnvelope is the fixed output shape of a remote tool that declared no
// output schema of its own.
type textEnvelope struct {
	Text                string   `json:"text"`
	SkippedContentTypes []string `json:"skipped_content_types"`
}

func executeFreeform(t *testing.T, client *mcp.Client) textEnvelope {
	t.Helper()
	registered := registerAndResolve(t, discoverOne(t, client, "remote.freeform"))
	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{}`),
	})
	if result.State != lebro.ToolExecutionSucceeded {
		t.Fatalf("State = %q, want %q (err: %v)", result.State, lebro.ToolExecutionSucceeded, result.Err)
	}
	var envelope textEnvelope
	if err := json.Unmarshal(result.Output, &envelope); err != nil {
		t.Fatalf("unmarshal output %q: %v", result.Output, err)
	}
	return envelope
}

func TestRemoteTool_NoOutputSchemaAdvertisesTextEnvelope(t *testing.T) {
	server := newTextServer(t, &mcpsdk.TextContent{Text: "plain text answer"})
	client := connectClient(t, server, "remote")

	// The adapter publishes a schema even though the server declared none, so
	// the tool's output shape is validated rather than merely hoped for.
	tool := discoverOne(t, client, "remote.freeform")
	if len(tool.Definition().OutputSchema) == 0 {
		t.Fatal("OutputSchema is empty; the adapter should advertise the text envelope")
	}

	envelope := executeFreeform(t, client)
	if envelope.Text != "plain text answer" {
		t.Errorf("text = %q, want %q", envelope.Text, "plain text answer")
	}
	if len(envelope.SkippedContentTypes) != 0 {
		t.Errorf("skipped_content_types = %v, want empty", envelope.SkippedContentTypes)
	}
}

// A tool without a declared output schema must produce the same shape whether
// or not its text happens to parse as JSON. Returning JSON-looking text
// verbatim would make the tool's output shape depend on the response.
func TestRemoteTool_TextEnvelopeShapeIsStableAcrossResponses(t *testing.T) {
	for _, testCase := range []struct {
		name string
		text string
	}{
		{name: "json text", text: `{"echo":"hi"}`},
		{name: "plain text", text: "hi"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTextServer(t, &mcpsdk.TextContent{Text: testCase.text})
			client := connectClient(t, server, "remote")

			if envelope := executeFreeform(t, client); envelope.Text != testCase.text {
				t.Errorf("text = %q, want %q", envelope.Text, testCase.text)
			}
		})
	}
}

func TestRemoteTool_NonTextContentIsReportedNotDropped(t *testing.T) {
	server := newTextServer(t,
		&mcpsdk.TextContent{Text: "see attached"},
		&mcpsdk.ImageContent{Data: []byte("fake"), MIMEType: "image/png"},
		&mcpsdk.ImageContent{Data: []byte("fake2"), MIMEType: "image/png"},
	)
	client := connectClient(t, server, "remote")

	envelope := executeFreeform(t, client)
	if envelope.Text != "see attached" {
		t.Errorf("text = %q, want %q", envelope.Text, "see attached")
	}
	// Duplicates collapse: the caller needs to know an image was dropped, not
	// how many.
	if len(envelope.SkippedContentTypes) != 1 || envelope.SkippedContentTypes[0] != "image" {
		t.Errorf("skipped_content_types = %v, want [image]", envelope.SkippedContentTypes)
	}
}

// A server that advertises an output schema and then returns non-JSON text has
// broken its own contract. Reporting that as an invocation failure names the
// real problem, rather than surfacing a confusing schema mismatch.
func TestRemoteTool_DeclaredSchemaWithNonJSONTextIsInvocationFailure(t *testing.T) {
	server := newRemoteServer(t, func(s *mcpsdk.Server) {
		s.AddTool(&mcpsdk.Tool{
			Name:         "inconsistent",
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "not json"}},
			}, nil
		})
	})
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.inconsistent"))

	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{}`),
	})
	if result.State != lebro.ToolExecutionHandlerError {
		t.Fatalf("State = %q, want %q", result.State, lebro.ToolExecutionHandlerError)
	}
	if !errors.Is(result.Err, mcp.ErrRemoteInvocation) {
		t.Errorf("errors.Is(err, ErrRemoteInvocation) = false; err = %v", result.Err)
	}
}

func TestRemoteTool_RemoteToolErrorIsDistinguishable(t *testing.T) {
	server := newRemoteServer(t, func(s *mcpsdk.Server) {
		s.AddTool(&mcpsdk.Tool{
			Name:        "broken",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			result := &mcpsdk.CallToolResult{}
			result.SetError(errors.New("upstream API rejected the request"))
			return result, nil
		})
	})
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.broken"))

	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{}`),
	})
	if result.State != lebro.ToolExecutionHandlerError {
		t.Fatalf("State = %q, want %q", result.State, lebro.ToolExecutionHandlerError)
	}
	if !errors.Is(result.Err, mcp.ErrRemoteToolError) {
		t.Errorf("errors.Is(err, ErrRemoteToolError) = false; err = %v", result.Err)
	}
	if errors.Is(result.Err, mcp.ErrRemoteInvocation) {
		t.Error("a tool-level error must not be reported as an invocation failure")
	}

	var remoteErr *mcp.RemoteToolError
	if !errors.As(result.Err, &remoteErr) {
		t.Fatalf("errors.As(err, *RemoteToolError) = false; err = %v", result.Err)
	}
	if remoteErr.ServerName != "remote" || remoteErr.ToolName != "broken" {
		t.Errorf("RemoteToolError = {%q, %q}, want {%q, %q}", remoteErr.ServerName, remoteErr.ToolName, "remote", "broken")
	}
}

func TestRemoteTool_InvocationFailureIsDistinguishable(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.echo"))

	// Closing the session breaks the transport without touching the tool, so
	// the next call fails on the wire rather than inside the remote handler.
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	result := registered.Execute(context.Background(), lebro.ToolExecutionRequest{
		Arguments: json.RawMessage(`{"value":"hello"}`),
	})
	if result.State != lebro.ToolExecutionHandlerError {
		t.Fatalf("State = %q, want %q", result.State, lebro.ToolExecutionHandlerError)
	}
	if !errors.Is(result.Err, mcp.ErrRemoteInvocation) {
		t.Errorf("errors.Is(err, ErrRemoteInvocation) = false; err = %v", result.Err)
	}
	if errors.Is(result.Err, mcp.ErrRemoteToolError) {
		t.Error("an invocation failure must not be reported as a tool-level error")
	}
}

func TestRemoteTool_CancellationPropagates(t *testing.T) {
	blocked := make(chan struct{})
	released := make(chan struct{})
	server := newRemoteServer(t, func(s *mcpsdk.Server) {
		s.AddTool(&mcpsdk.Tool{
			Name:        "slow",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			close(blocked)
			<-ctx.Done()
			close(released)
			return nil, ctx.Err()
		})
	})
	client := connectClient(t, server, "remote")
	registered := registerAndResolve(t, discoverOne(t, client, "remote.slow"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan lebro.ToolExecutionResult, 1)
	go func() {
		done <- registered.Execute(ctx, lebro.ToolExecutionRequest{
			Arguments: json.RawMessage(`{}`),
		})
	}()

	<-blocked
	cancel()

	result := <-done
	if result.State != lebro.ToolExecutionCancelled {
		t.Fatalf("State = %q, want %q (err: %v)", result.State, lebro.ToolExecutionCancelled, result.Err)
	}
	<-released
}

func TestDiscoverTools_FailureIsDistinguishable(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	client := connectClient(t, server, "remote")

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := client.DiscoverTools(context.Background())
	if err == nil {
		t.Fatal("DiscoverTools succeeded after the session closed")
	}
	if !errors.Is(err, mcp.ErrRemoteDiscovery) {
		t.Errorf("errors.Is(err, ErrRemoteDiscovery) = false; err = %v", err)
	}
	if errors.Is(err, mcp.ErrRemoteInvocation) {
		t.Error("a discovery failure must not be reported as an invocation failure")
	}

	var discoveryErr *mcp.RemoteDiscoveryError
	if !errors.As(err, &discoveryErr) {
		t.Fatalf("errors.As(err, *RemoteDiscoveryError) = false; err = %v", err)
	}
	if discoveryErr.ServerName != "remote" {
		t.Errorf("ServerName = %q, want %q", discoveryErr.ServerName, "remote")
	}
}

func TestDiscoverTools_FollowsPagination(t *testing.T) {
	const toolCount = 5
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "remote-server", Version: "test"},
		&mcpsdk.ServerOptions{PageSize: 2},
	)
	for i := range toolCount {
		server.AddTool(&mcpsdk.Tool{
			Name:        fmt.Sprintf("tool%d", i),
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "{}"}},
			}, nil
		})
	}
	client := connectClient(t, server, "remote")

	tools, err := client.DiscoverTools(context.Background())
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != toolCount {
		t.Fatalf("discovered %d tools, want %d; pagination was not followed", len(tools), toolCount)
	}

	seen := make(map[lebro.ToolID]struct{}, len(tools))
	for _, tool := range tools {
		seen[tool.Definition().ID] = struct{}{}
	}
	for i := range toolCount {
		id := lebro.ToolID(fmt.Sprintf("remote.tool%d", i))
		if _, ok := seen[id]; !ok {
			t.Errorf("tool %q missing from discovery", id)
		}
	}
}

func TestDiscoverTools_NamespacesByServerName(t *testing.T) {
	serverA := newRemoteServer(t, addRemoteEcho)
	serverB := newRemoteServer(t, addRemoteEcho)
	clientA := connectClient(t, serverA, "alpha")
	clientB := connectClient(t, serverB, "beta")

	registry := mustRegistry(t)
	for _, client := range []*mcp.Client{clientA, clientB} {
		tools, err := client.DiscoverTools(context.Background())
		if err != nil {
			t.Fatalf("DiscoverTools: %v", err)
		}
		for _, tool := range tools {
			if err := registry.Register(tool); err != nil {
				t.Fatalf("Register %q: %v", tool.Definition().ID, err)
			}
		}
	}

	// Both servers advertise "echo"; the server name prefix is what keeps them
	// from colliding in a single registry.
	for _, id := range []lebro.ToolID{"alpha.echo", "beta.echo"} {
		if _, ok := registry.Resolve(id); !ok {
			t.Errorf("tool %q not registered", id)
		}
	}
}

func TestDiscoverTools_NotConnected(t *testing.T) {
	client := mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "lebro-client", Version: "test"},
		ServerName:     "remote",
	})

	_, err := client.DiscoverTools(context.Background())
	if err == nil {
		t.Fatal("DiscoverTools succeeded without a connection")
	}
	if !errors.Is(err, mcp.ErrRemoteDiscovery) {
		t.Errorf("errors.Is(err, ErrRemoteDiscovery) = false; err = %v", err)
	}
}

func TestNewClient_RequiresServerName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewClient did not panic on an empty ServerName")
		}
	}()
	mcp.NewClient(mcp.ClientConfig{
		Implementation: &mcpsdk.Implementation{Name: "lebro-client", Version: "test"},
	})
}

func TestClient_ConnectTwiceFails(t *testing.T) {
	server := newRemoteServer(t, addRemoteEcho)
	client := connectClient(t, server, "remote")

	_, secondTransport := mcpsdk.NewInMemoryTransports()
	if err := client.Connect(context.Background(), secondTransport); err == nil {
		t.Error("second Connect succeeded; the client already had a session")
	}
}
