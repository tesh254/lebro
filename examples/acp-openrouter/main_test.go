package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigurationAndInspectText(t *testing.T) {
	if _, err := newServer("", "model", ""); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := newServer("key", "", ""); err == nil {
		t.Fatal("missing model accepted")
	}
	result, err := (inspectText{}).Execute(t.Context(), json.RawMessage(`{"text":"hello 世界"}`))
	if err != nil || string(result) != `{"characters":8,"words":2}` {
		t.Fatalf("result %s, error %v", result, err)
	}
}

func TestStdioConversationUsesOpenRouterAndPreservesHistory(t *testing.T) {
	var mu sync.Mutex
	var requests [][]map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected provider request: %s", r.URL.Path)
			http.Error(w, "bad request", 400)
			return
		}
		var body struct {
			Model    string           `json:"model"`
			Messages []map[string]any `json:"messages"`
			Stream   bool             `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad json", 400)
			return
		}
		if body.Model != "test/model" {
			t.Errorf("model=%q stream=%v", body.Model, body.Stream)
		}
		mu.Lock()
		requests = append(requests, body.Messages)
		call := len(requests)
		mu.Unlock()
		if call == 4 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"three words"},"finish_reason":"stop"}]}`)
			return
		}
		if !body.Stream {
			t.Error("expected a streaming request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 3 {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"inspect_text\",\"arguments\":\"{\\\"text\\\":\\\"hello from Zed\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture response\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	binary := filepath.Join(t.TempDir(), "acp-openrouter")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY=test-key", "OPENROUTER_MODEL=test/model", "OPENROUTER_BASE_URL="+provider.URL)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	reader := bufio.NewReader(stdout)
	permissionSeen := false
	send := func(v any) {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fmt.Fprintf(stdin, "%s\n", data); err != nil {
			t.Fatal(err)
		}
	}
	readResponse := func(id float64) map[string]any {
		t.Helper()
		type result struct {
			msg map[string]any
			err error
		}
		done := make(chan result, 1)
		go func() {
			for {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					done <- result{err: err}
					return
				}
				var msg map[string]any
				if err := json.Unmarshal(line, &msg); err != nil {
					done <- result{err: err}
					return
				}
				if msg["method"] == "session/request_permission" {
					permissionSeen = true
					response := map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow"}}}
					data, err := json.Marshal(response)
					if err != nil {
						done <- result{err: err}
						return
					}
					if _, err := fmt.Fprintf(stdin, "%s\n", data); err != nil {
						done <- result{err: err}
						return
					}
				}
				if msg["id"] == id && msg["method"] == nil {
					done <- result{msg: msg}
					return
				}
			}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("read response: %v", got.err)
			}
			if got.msg["error"] != nil {
				t.Fatalf("ACP error: %v", got.msg["error"])
			}
			return got.msg
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for ACP response")
			return nil
		}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}})
	readResponse(1)
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}})
	created := readResponse(2)
	session, ok := created["result"].(map[string]any)["sessionId"].(string)
	if !ok || session == "" {
		t.Fatalf("session: %v", created)
	}
	for i, message := range []string{"remember blue", "what color?"} {
		id := float64(3 + i)
		send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": map[string]any{"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": message}}}})
		response := readResponse(id)
		if response["result"].(map[string]any)["stopReason"] != "end_turn" {
			t.Fatalf("prompt result: %v", response)
		}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "session/prompt", "params": map[string]any{"sessionId": session, "prompt": []any{map[string]any{"type": "text", "text": "inspect hello from Zed"}}}})
	response := readResponse(5)
	if response["result"].(map[string]any)["stopReason"] != "end_turn" || !permissionSeen {
		t.Fatalf("tool prompt result=%v permission=%v", response, permissionSeen)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("got %d provider requests", len(requests))
	}
	encoded, err := json.Marshal(requests[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "remember blue") || !strings.Contains(string(encoded), "fixture response") || !strings.Contains(string(encoded), "what color?") {
		t.Fatalf("second turn lacks history: %s", encoded)
	}
	toolResult, err := json.Marshal(requests[3])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toolResult), `\"words\":3`) {
		t.Fatalf("tool result missing from continuation: %s", toolResult)
	}
}
