package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/mcp"
)

type callerContextKey struct{}

func TestRequestScopedExposure_IsolatedConcurrentAndRevocable(t *testing.T) {
	var calls atomic.Int64
	var revoked atomic.Bool
	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "request-scoped", Version: "test"},
		RequestResolver: func(r *http.Request) (mcp.RequestExposure, error) {
			caller, _ := r.Context().Value(callerContextKey{}).(string)
			if revoked.Load() || caller == "denied" {
				return mcp.RequestExposure{}, nil
			}
			return mcp.RequestExposure{Tools: []mcp.ToolAdapter{{
				Definition: lebro.ToolDefinition{ID: lebro.ToolID("tenant." + caller), InputSchema: json.RawMessage(`{"type":"object"}`)},
				Execute: func(ctx context.Context, _ lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
					if got, _ := ctx.Value(callerContextKey{}).(string); got != caller {
						return lebro.ToolExecutionResult{State: lebro.ToolExecutionHandlerError}
					}
					calls.Add(1)
					return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded, Output: json.RawMessage(`{"caller":"` + caller + `"}`)}
				},
			}}}, nil
		},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := r.Header.Get("X-Caller")
		server.StreamableHTTPHandler(nil).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller)))
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	for _, caller := range []string{"alpha", "beta"} {
		body := requestScopedRPC(t, httpServer.URL, "tools/list", map[string]any{}, caller)
		if !strings.Contains(body, `"tenant.`+caller+`"`) || strings.Contains(body, "tenant."+otherCaller(caller)) {
			t.Fatalf("tools/list for %q = %s", caller, body)
		}
	}

	var wg sync.WaitGroup
	for _, caller := range []string{"alpha", "beta"} {
		caller := caller
		wg.Go(func() {
			body := requestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "tenant." + caller, "arguments": map[string]any{}}, caller)
			if !strings.Contains(body, `"caller":"`+caller+`"`) {
				t.Errorf("tools/call for %q = %s", caller, body)
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}

	if body := requestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "tenant.alpha", "arguments": map[string]any{}}, "denied"); !strings.Contains(body, "error") {
		t.Fatalf("denied tools/call = %s, want error", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("denied call invoked adapter %d times", got)
	}

	revoked.Store(true)
	if body := requestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "tenant.alpha", "arguments": map[string]any{}}, "alpha"); !strings.Contains(body, "error") {
		t.Fatalf("revoked tools/call = %s, want error", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("revoked call invoked adapter %d times", got)
	}
}

func TestRequestResolver_PublicError(t *testing.T) {
	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "request-error", Version: "test"},
		RequestResolver: func(*http.Request) (mcp.RequestExposure, error) {
			return mcp.RequestExposure{}, &mcp.RequestResolutionError{StatusCode: http.StatusUnauthorized, Message: "invalid access token"}
		},
	})
	httpServer := httptest.NewServer(server.StreamableHTTPHandler(nil))
	defer httpServer.Close()
	response := requestScopedResponse(t, httpServer.URL, "tools/list", map[string]any{}, "")
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized || string(body) != "invalid access token\n" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
}

func TestExposeToolAdapter_ValidatesSchemaBoundary(t *testing.T) {
	server := newTestServer(t)
	var calls atomic.Int64
	if err := server.ExposeToolAdapter(mcp.ToolAdapter{
		Definition: lebro.ToolDefinition{
			ID:           "adapter",
			InputSchema:  json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`),
		},
		Execute: func(_ context.Context, _ lebro.ToolExecutionRequest) lebro.ToolExecutionResult {
			calls.Add(1)
			return lebro.ToolExecutionResult{State: lebro.ToolExecutionSucceeded, Output: json.RawMessage(`{"wrong":true}`)}
		},
	}); err != nil {
		t.Fatal(err)
	}
	session, cleanup := connectServer(t, server)
	defer cleanup()
	if result := callTool(t, session, "adapter", map[string]any{}); !result.IsError {
		t.Fatalf("invalid input result = %#v, want tool error", result)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid input invoked adapter %d times", got)
	}
	if result := callTool(t, session, "adapter", map[string]any{"value": "ok"}); !result.IsError {
		t.Fatalf("invalid output result = %#v, want tool error", result)
	}
}

func requestScopedRPC(t *testing.T, url, method string, params map[string]any, caller string) string {
	t.Helper()
	response := requestScopedResponse(t, url, method, params, caller)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requestScopedResponse(t *testing.T, url, method string, params map[string]any, caller string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method,
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "test"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	requestParams := request["params"].(map[string]any)
	for key, value := range params {
		requestParams[key] = value
	}
	body, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
	}
	req.Header.Set("X-Caller", caller)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func otherCaller(caller string) string {
	if caller == "alpha" {
		return "beta"
	}
	return "alpha"
}
