package acp_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
)

func TestStdioMCPToolsAreSessionScoped(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-server")
	if output, err := exec.Command("go", "build", "-o", binary, "github.com/tesh254/lebro/examples/mcp-server").CombinedOutput(); err != nil {
		t.Fatalf("build MCP server: %v: %s", err, output)
	}
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	call := lebro.ModelToolCall{ID: "remote-call", ToolID: "remote.calc.add", Arguments: json.RawMessage(`{"a":2,"b":3}`)}
	response := lebro.ModelResponse{Message: lebro.Message{Role: lebro.RoleAssistant, ToolCalls: mustToolCalls(call)}, FinishReason: lebro.FinishReasonToolCalls}
	h := newHarness(t, response, textResponse("done"), textResponse("other"))
	var mu sync.Mutex
	var toolSets [][]lebro.ToolDefinition
	h.model.onCall = func(_ int, request lebro.ModelRequest) {
		mu.Lock()
		defer mu.Unlock()
		toolSets = append(toolSets, append([]lebro.ToolDefinition(nil), request.Tools...))
	}
	h.initialize(t.Context())
	result, err := h.client.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{{Stdio: &acp.McpServerStdio{Name: "remote", Command: filepath.Base(binary), Args: []string{}, Env: []acp.EnvVariable{}}}}})
	if err != nil {
		t.Fatalf("create MCP session: %v", err)
	}
	h.events.allow <- "allow"
	h.mustPrompt(t.Context(), result.SessionId, "add numbers")
	calls := h.events.toolCalls()
	if len(calls) < 3 || calls[len(calls)-1][1] != "completed" {
		t.Fatalf("remote tool did not complete: %v", calls)
	}
	other := h.newSession(t.Context())
	h.mustPrompt(t.Context(), other, "another session")
	mu.Lock()
	defer mu.Unlock()
	if len(toolSets) < 3 {
		t.Fatalf("observed %d model calls", len(toolSets))
	}
	for _, tool := range toolSets[len(toolSets)-1] {
		if tool.ID == "remote.calc.add" {
			t.Fatal("MCP tool leaked into another session")
		}
	}
}
