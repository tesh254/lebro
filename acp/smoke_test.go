package acp_test

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExampleACPAgentSmoke runs the examples/acp-agent binary over real stdio
// pipes and drives initialize -> session/new -> prompt to completion, proving
// the shipped example works with a plain newline-delimited JSON-RPC client.
func TestExampleACPAgentSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test builds and runs the example binary")
	}
	// Build once into a temp dir; the smoke test then speaks plain
	// newline-delimited JSON-RPC to the binary over real stdio pipes.
	bin := filepath.Join(t.TempDir(), "acp-agent")
	build := exec.Command("go", "build", "-o", bin, "github.com/tesh254/lebro/examples/acp-agent")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build example: %v: %s", err, out)
	}
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Register the logging cleanup first so it runs after the kill/wait
	// cleanup: the child must be dead and reaped before the buffer is read.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent stderr: %s", stderrBuf.String())
		}
	})
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	send := func(v map[string]any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, '\n')
		if _, err := stdin.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	// request id 0: initialize
	send(map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			"clientCapabilities": map[string]any{
				"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
			},
			"clientInfo": map[string]any{"name": "smoke", "version": "0"},
		},
	})
	reader := bufio.NewReader(stdout)
	deadline := time.Now().Add(60 * time.Second)
	readLine := func() (string, bool) {
		type result struct {
			line string
			ok   bool
		}
		ch := make(chan result, 1)
		go func() {
			line, err := reader.ReadString('\n')
			ch <- result{line, err == nil}
		}()
		select {
		case r := <-ch:
			return r.line, r.ok
		case <-time.After(15 * time.Second):
			return "", false
		}
	}

	sessionID := ""
	permissionAnswered := false
	promptReplied := false
	updateText := ""
	// Read messages; respond to session/new, permission request; stop at the
	// prompt response.
	for time.Now().Before(deadline) {
		line, ok := readLine()
		if !ok {
			break
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
			Params struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					ToolCallID    string `json:"toolCallId"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		switch {
		case msg.ID != nil && *msg.ID == 0 && msg.Result != nil && msg.Method == "":
			send(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "session/new",
				"params": map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}},
			})
		case msg.ID != nil && *msg.ID == 1 && msg.Result != nil && msg.Method == "":
			var res struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Result, &res)
			sessionID = res.SessionID
			// Send the real prompt now.
			send(map[string]any{
				"jsonrpc": "2.0", "id": 2, "method": "session/prompt",
				"params": map[string]any{
					"sessionId": sessionID,
					"prompt":    []any{map[string]any{"type": "text", "text": "refund order A-1024"}},
				},
			})
		case msg.ID != nil && *msg.ID == 2 && msg.Error != nil:
			t.Fatalf("prompt errored: %s", line)
		case msg.ID != nil && *msg.ID == 2 && msg.Result != nil:
			send(map[string]any{
				"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
				"params": map[string]any{
					"sessionId": sessionID,
					"prompt":    []any{map[string]any{"type": "text", "text": "what happened?"}},
				},
			})
		case msg.ID != nil && *msg.ID == 3 && msg.Error != nil:
			t.Fatalf("second prompt errored: %s", line)
		case msg.ID != nil && *msg.ID == 3 && msg.Result != nil:
			promptReplied = true
		case msg.Method == "session/update":
			// The example model answers the first turn with the pending
			// refund request and the second turn with its confirmation;
			// capture the text so a fresh-thread regression (second turn
			// answering without history) fails.
			var note struct {
				Params struct {
					Update struct {
						SessionUpdate string `json:"sessionUpdate"`
						Content       struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"update"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &note); err == nil && note.Params.Update.SessionUpdate == "agent_message_chunk" && note.Params.Update.Content.Text != "" {
				updateText += note.Params.Update.Content.Text
			}
		case msg.Method == "session/request_permission":
			permissionAnswered = true
			var req struct {
				ID     int `json:"id"`
				Params struct {
					Options []struct {
						OptionID string `json:"optionId"`
					} `json:"options"`
				} `json:"params"`
			}
			_ = json.Unmarshal([]byte(line), &req)
			optionID := "reject"
			for _, o := range req.Params.Options {
				if o.OptionID == "allow" {
					optionID = "allow"
				}
			}
			send(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"outcome": map[string]any{"outcome": "selected", "optionId": optionID},
				},
			})
		}
		if promptReplied {
			break
		}
	}
	if sessionID == "" {
		t.Fatal("never received session id")
	}
	if !permissionAnswered {
		t.Fatal("agent never requested permission")
	}
	if !promptReplied {
		t.Fatal("agent never replied to the prompt")
	}
	// The second turn must observe the first turn's context: the example
	// agent's demo model only confirms the refund after the transcript
	// contains a tool result, so a session that dropped its history would
	// request the refund again instead of confirming it.
	if !strings.Contains(updateText, "approved and issued") {
		t.Fatalf("second turn did not observe session history; updates = %q", updateText)
	}
}
