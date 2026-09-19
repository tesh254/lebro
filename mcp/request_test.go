package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		body := mustRequestScopedRPC(t, httpServer.URL, "tools/list", map[string]any{}, caller)
		if !strings.Contains(body, `"tenant.`+caller+`"`) || strings.Contains(body, "tenant."+otherCaller(caller)) {
			t.Fatalf("tools/list for %q = %s", caller, body)
		}
	}

	var wg sync.WaitGroup
	for _, caller := range []string{"alpha", "beta"} {
		caller := caller
		wg.Go(func() {
			body, err := requestScopedRPC(httpServer.URL, "tools/call", map[string]any{"name": "tenant." + caller, "arguments": map[string]any{}}, caller)
			if err != nil {
				t.Errorf("tools/call for %q: %v", caller, err)
				return
			}
			if !strings.Contains(body, `"caller":"`+caller+`"`) {
				t.Errorf("tools/call for %q = %s", caller, body)
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}

	if body := mustRequestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "tenant.alpha", "arguments": map[string]any{}}, "denied"); !strings.Contains(body, "error") {
		t.Fatalf("denied tools/call = %s, want error", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("denied call invoked adapter %d times", got)
	}

	revoked.Store(true)
	if body := mustRequestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "tenant.alpha", "arguments": map[string]any{}}, "alpha"); !strings.Contains(body, "error") {
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
	response, err := requestScopedResponse(httpServer.URL, "tools/list", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized || string(body) != "invalid access token\n" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
}

func TestRequestScopedExposure_AgentAndWorkflowRevocation(t *testing.T) {
	var agentCalls, workflowCalls atomic.Int64
	var revoked atomic.Bool
	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "request-scoped-runs", Version: "test"},
		RequestResolver: func(r *http.Request) (mcp.RequestExposure, error) {
			caller, _ := r.Context().Value(callerContextKey{}).(string)
			if revoked.Load() || caller == "denied" {
				return mcp.RequestExposure{}, nil
			}
			return mcp.RequestExposure{
				Agents: []mcp.AgentAdapter{{
					Definition: lebro.WorkflowDefinition{ID: lebro.WorkflowID("published-agent-" + caller), Description: "Published agent"},
					Run: func(ctx context.Context, _ lebro.RunInput) (lebro.RunResult, error) {
						if got, _ := ctx.Value(callerContextKey{}).(string); got != caller {
							return lebro.RunResult{}, context.Canceled
						}
						agentCalls.Add(1)
						return lebro.RunResult{Messages: []lebro.Message{{Role: lebro.RoleAssistant, Content: caller}}}, nil
					},
				}},
				Workflows: []mcp.WorkflowAdapter{{
					Definition: lebro.WorkflowDefinition{ID: lebro.WorkflowID("published-workflow-" + caller), Description: "Published workflow"},
					Run: func(ctx context.Context, _ lebro.WorkflowRunInput) (lebro.WorkflowRunResult, error) {
						if got, _ := ctx.Value(callerContextKey{}).(string); got != caller {
							return lebro.WorkflowRunResult{}, context.Canceled
						}
						workflowCalls.Add(1)
						return lebro.WorkflowRunResult{Output: json.RawMessage(`{"caller":"` + caller + `"}`)}, nil
					},
				}},
			}, nil
		},
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := r.Header.Get("X-Caller")
		server.StreamableHTTPHandler(nil).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller)))
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	for _, caller := range []string{"alpha", "beta"} {
		body := mustRequestScopedRPC(t, httpServer.URL, "tools/list", map[string]any{}, caller)
		for _, name := range []string{"agent.published-agent-" + caller, "workflow.published-workflow-" + caller} {
			if !strings.Contains(body, name) {
				t.Fatalf("tools/list for %q missing %q: %s", caller, name, body)
			}
		}
	}

	var wg sync.WaitGroup
	for _, call := range []struct {
		caller, name string
		arguments    map[string]any
	}{
		{"alpha", "agent.published-agent-alpha", map[string]any{"messages": []map[string]string{{"content": "hello"}}}},
		{"beta", "workflow.published-workflow-beta", map[string]any{}},
	} {
		call := call
		wg.Go(func() {
			if _, err := requestScopedRPC(httpServer.URL, "tools/call", map[string]any{"name": call.name, "arguments": call.arguments}, call.caller); err != nil {
				t.Errorf("tools/call %q: %v", call.name, err)
			}
		})
	}
	wg.Wait()
	if agentCalls.Load() != 1 || workflowCalls.Load() != 1 {
		t.Fatalf("calls = agent:%d workflow:%d", agentCalls.Load(), workflowCalls.Load())
	}

	for _, name := range []string{"agent.published-agent-alpha", "workflow.published-workflow-alpha"} {
		body := mustRequestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}, "denied")
		if !strings.Contains(body, "error") {
			t.Fatalf("denied %q = %s", name, body)
		}
	}
	revoked.Store(true)
	for _, name := range []string{"agent.published-agent-alpha", "workflow.published-workflow-alpha"} {
		body := mustRequestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}, "alpha")
		if !strings.Contains(body, "error") {
			t.Fatalf("revoked %q = %s", name, body)
		}
	}
	if agentCalls.Load() != 1 || workflowCalls.Load() != 1 {
		t.Fatalf("denied/revoked calls ran adapters: agent:%d workflow:%d", agentCalls.Load(), workflowCalls.Load())
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

func TestRequestScopedExposure_AsyncAgentTasks(t *testing.T) {
	store := newRequestScopedTaskStore()
	release := make(chan struct{})
	var runs atomic.Int64
	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "request-scoped-tasks", Version: "test"},
		Tasks: &mcp.TaskConfig{
			Store: store,
			Identity: func(ctx context.Context) (json.RawMessage, error) {
				caller, _ := ctx.Value(callerContextKey{}).(string)
				return json.RawMessage(`{"caller":"` + caller + `"}`), nil
			},
			Context: func(ctx context.Context, record mcp.TaskRecord) (context.Context, error) {
				var identity struct {
					Caller string `json:"caller"`
				}
				if err := json.Unmarshal(record.Identity, &identity); err != nil {
					return nil, err
				}
				return context.WithValue(ctx, callerContextKey{}, identity.Caller), nil
			},
			Authorize: func(context.Context, mcp.TaskRecord) error { return nil },
			NewID: func() (string, error) {
				return "task-" + strconv.FormatInt(store.count()+1, 10), nil
			},
			PollInterval: time.Millisecond,
		},
		RequestResolver: func(r *http.Request) (mcp.RequestExposure, error) {
			caller, _ := r.Context().Value(callerContextKey{}).(string)
			return mcp.RequestExposure{AsyncAgents: []mcp.AsyncAgentAdapter{{
				Adapter: mcp.AgentAdapter{
					Definition: lebro.WorkflowDefinition{ID: "async-agent", Description: "Async agent"},
					Run: func(ctx context.Context, _ lebro.RunInput) (lebro.RunResult, error) {
						runs.Add(1)
						<-release
						if got, _ := ctx.Value(callerContextKey{}).(string); got != caller {
							return lebro.RunResult{}, errors.New("identity not restored")
						}
						return lebro.RunResult{Messages: []lebro.Message{{Role: lebro.RoleAssistant, Content: "done " + caller}}}, nil
					},
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

	body := mustRequestScopedRPC(t, httpServer.URL, "tools/list", map[string]any{}, "alpha")
	if !strings.Contains(body, "agent.async-agent") {
		t.Fatalf("tools/list missing async agent: %s", body)
	}

	created := mustTasksRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "agent.async-agent", "arguments": map[string]any{"messages": []map[string]string{{"content": "hello"}}}}, "alpha", true)
	taskID := requestScopedTaskID(t, created)
	if !strings.Contains(created, `"status":"working"`) {
		t.Fatalf("task creation response = %s", created)
	}

	working := mustTasksRPC(t, httpServer.URL, "tasks/get", map[string]any{"taskId": taskID}, "alpha", true)
	if !strings.Contains(working, `"status":"working"`) {
		t.Fatalf("tasks/get while working = %s", working)
	}

	close(release)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var completed string
	for i := 0; i < 500; i++ {
		completed = mustTasksRPC(t, httpServer.URL, "tasks/get", map[string]any{"taskId": taskID}, "alpha", true)
		if strings.Contains(completed, `"status":"completed"`) {
			break
		}
		<-ticker.C
	}
	if !strings.Contains(completed, `"status":"completed"`) || !strings.Contains(completed, "done alpha") {
		t.Fatalf("final tasks/get = %s", completed)
	}
	if runs.Load() != 1 {
		t.Fatalf("agent ran %d times, want 1", runs.Load())
	}

	// Task state queries themselves require the negotiated extension.
	if body := mustTasksRPC(t, httpServer.URL, "tasks/get", map[string]any{"taskId": taskID}, "alpha", false); !strings.Contains(body, "-32021") {
		t.Fatalf("unnegotiated tasks/get = %s, want capability error", body)
	}
}

func TestRequestScopedExposure_AsyncFallbackAndRequireTasks(t *testing.T) {
	server := mcp.NewServer(mcp.ServerConfig{
		Implementation: &mcpsdk.Implementation{Name: "request-scoped-fallback", Version: "test"},
		Tasks: &mcp.TaskConfig{
			Store:        newRequestScopedTaskStore(),
			Identity:     func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
			Context:      func(ctx context.Context, _ mcp.TaskRecord) (context.Context, error) { return ctx, nil },
			Authorize:    func(context.Context, mcp.TaskRecord) error { return nil },
			PollInterval: time.Millisecond,
		},
		RequestResolver: func(*http.Request) (mcp.RequestExposure, error) {
			return mcp.RequestExposure{AsyncAgents: []mcp.AsyncAgentAdapter{
				{
					Adapter: mcp.AgentAdapter{
						Definition: lebro.WorkflowDefinition{ID: "fallback-agent", Description: "Falls back"},
						Run: func(context.Context, lebro.RunInput) (lebro.RunResult, error) {
							return lebro.RunResult{Messages: []lebro.Message{{Role: lebro.RoleAssistant, Content: "instant"}}}, nil
						},
					},
				},
				{
					Adapter: mcp.AgentAdapter{
						Definition: lebro.WorkflowDefinition{ID: "strict-agent", Description: "Requires tasks"},
						Run: func(context.Context, lebro.RunInput) (lebro.RunResult, error) {
							return lebro.RunResult{}, errors.New("must not run without tasks")
						},
					},
					Options: mcp.AsyncEntryOptions{RequireTasks: true},
				},
			}}, nil
		},
	})
	httpServer := httptest.NewServer(server.StreamableHTTPHandler(nil))
	defer httpServer.Close()

	body := mustRequestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "agent.fallback-agent", "arguments": map[string]any{}}, "alpha")
	if !strings.Contains(body, "instant") {
		t.Fatalf("synchronous fallback = %s", body)
	}
	if body := mustRequestScopedRPC(t, httpServer.URL, "tools/call", map[string]any{"name": "agent.strict-agent", "arguments": map[string]any{}}, "alpha"); !strings.Contains(body, "-32021") {
		t.Fatalf("RequireTasks call = %s, want capability error", body)
	}
}

type requestScopedTaskStore struct {
	mu      sync.Mutex
	records map[string]mcp.TaskRecord
}

func newRequestScopedTaskStore() *requestScopedTaskStore {
	return &requestScopedTaskStore{records: make(map[string]mcp.TaskRecord)}
}

func (s *requestScopedTaskStore) count() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.records))
}

func (s *requestScopedTaskStore) CreateTask(_ context.Context, record mcp.TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[record.ID]; ok {
		return errors.New("task already exists")
	}
	s.records[record.ID] = record
	return nil
}

func (s *requestScopedTaskStore) GetTask(_ context.Context, id string) (mcp.TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return mcp.TaskRecord{}, errors.New("task not found")
	}
	return record, nil
}

func (s *requestScopedTaskStore) UpdateTask(_ context.Context, record mcp.TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.records[record.ID]
	if !ok {
		return errors.New("task not found")
	}
	if current.Version != record.Version {
		return errors.New("task version conflict")
	}
	record.Version++
	s.records[record.ID] = record
	return nil
}

// mustTasksRPC issues one stateless JSON-RPC request with the 2026-07-28
// protocol metadata, optionally negotiating the Tasks extension. It returns
// the raw response body so tests assert on the wire shape.
func mustTasksRPC(t *testing.T, url, method string, params map[string]any, caller string, tasks bool) string {
	t.Helper()
	capabilities := map[string]any{}
	if tasks {
		capabilities["extensions"] = map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}
	}
	meta := map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "test"},
		"io.modelcontextprotocol/clientCapabilities": capabilities,
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": mergeRequestScopedParams(params, meta)})
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
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mergeRequestScopedParams(params, meta map[string]any) map[string]any {
	merged := make(map[string]any, len(params)+1)
	for key, value := range params {
		merged[key] = value
	}
	merged["_meta"] = meta
	return merged
}

func requestScopedTaskID(t *testing.T, body string) string {
	t.Helper()
	var response struct {
		Result struct {
			TaskID string `json:"taskId"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(requestScopedJSONBody(body)), &response); err != nil || response.Result.TaskID == "" {
		t.Fatalf("task id from %s: %v", body, err)
	}
	return response.Result.TaskID
}

// requestScopedJSONBody returns the JSON-RPC message from a response body.
// The Streamable HTTP transport may answer either with a JSON document or
// with a single SSE event, so both shapes are accepted here.
func requestScopedJSONBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return strings.TrimSpace(data)
		}
	}
	return body
}

func mustRequestScopedRPC(t *testing.T, url, method string, params map[string]any, caller string) string {
	t.Helper()
	body, err := requestScopedRPC(url, method, params, caller)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func requestScopedRPC(url, method string, params map[string]any, caller string) (string, error) {
	response, err := requestScopedResponse(url, method, params, caller)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func requestScopedResponse(url, method string, params map[string]any, caller string) (*http.Response, error) {
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
		return nil, err
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	requestParams := request["params"].(map[string]any)
	for key, value := range params {
		requestParams[key] = value
	}
	body, err = json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
		return nil, err
	}
	return response, nil
}

func otherCaller(caller string) string {
	if caller == "alpha" {
		return "beta"
	}
	return "alpha"
}
