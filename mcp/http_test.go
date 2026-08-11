package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tesh254/lebro"
)

type blockingTool struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (blockingTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "block",
		Description: "Blocks until the request is cancelled",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (t blockingTool) Execute(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	close(t.started)
	<-ctx.Done()
	close(t.cancelled)
	return nil, ctx.Err()
}

func TestStreamableHTTPHandler_ServesAllowListStatelessly(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	must(t, registry.Register(echoTool{}))
	tool, ok := registry.Resolve("echo")
	if !ok {
		t.Fatal("registered echo tool not found")
	}
	must(t, srv.ExposeTool(tool))

	httpServer := httptest.NewServer(srv.StreamableHTTPHandler(nil))
	defer httpServer.Close()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test-client", "version": "test"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	})
	must(t, err)
	request, err := http.NewRequest(http.MethodPost, httpServer.URL, bytes.NewReader(body))
	must(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/list")

	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("tools/list status = %d, want %d; body = %s", response.StatusCode, http.StatusOK, responseBody)
	}
	if sessionID := response.Header.Get("Mcp-Session-Id"); sessionID != "" {
		t.Fatalf("stateless response returned Mcp-Session-Id %q", sessionID)
	}
	responseBody, _ := io.ReadAll(response.Body)
	const sseMessagePrefix = "event: message\ndata: "
	message := strings.TrimSpace(strings.TrimPrefix(string(responseBody), sseMessagePrefix))
	if message == string(responseBody) {
		t.Fatalf("tools/list response missing SSE message: %s", responseBody)
	}
	var result struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	must(t, json.Unmarshal([]byte(message), &result))
	if len(result.Result.Tools) != 1 || result.Result.Tools[0].Name != "echo" {
		t.Fatalf("tools/list = %+v, want only echo", result.Result.Tools)
	}
}

func TestStreamableHTTPHandler_PropagatesRequestCancellation(t *testing.T) {
	srv := newTestServer(t)
	registry := mustRegistry(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	must(t, registry.Register(blockingTool{started: started, cancelled: cancelled}))
	tool, ok := registry.Resolve("block")
	if !ok {
		t.Fatal("registered blocking tool not found")
	}
	must(t, srv.ExposeTool(tool))

	httpServer := httptest.NewServer(srv.StreamableHTTPHandler(nil))
	defer httpServer.Close()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test-client", "version": "test"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
			"name":      "block",
			"arguments": map[string]any{},
		},
	})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, bytes.NewReader(body))
	must(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "block")

	responseDone := make(chan struct{})
	go func() {
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		close(responseDone)
	}()
	waitForStart(t, started)
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("tool did not receive request cancellation")
	}
	select {
	case <-responseDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP request did not finish after cancellation")
	}
}
