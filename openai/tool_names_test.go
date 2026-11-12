package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
)

func TestToolNameMappingPreservesCallsAndHistory(t *testing.T) {
	original := lebro.ToolID("shopify-dev-mcp.search_docs")
	calls, err := lebro.NewModelToolCalls(lebro.ModelToolCall{ID: "old", ToolID: original, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	request := lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleAssistant, ToolCalls: calls}, {Role: lebro.RoleTool, ToolCallID: "old", Content: "done"}}, Tools: []lebro.ToolDefinition{{ID: original, InputSchema: json.RawMessage(`{"type":"object"}`)}, {ID: "plain_name", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	request.Extension = json.RawMessage(`{"tool_choice":{"type":"function","function":{"name":"shopify-dev-mcp.search_docs"}},"temperature":0.2}`)
	mapped, reverse, err := mapToolNames(request)
	if err != nil {
		t.Fatal(err)
	}
	alias := string(mapped.Tools[0].ID)
	if !validToolName(alias) || alias == string(original) || reverse[alias] != original {
		t.Fatalf("invalid alias %q", alias)
	}
	if mapped.Tools[1].ID != "plain_name" || mapped.Messages[0].ToolCalls.Values()[0].ToolID != mapped.Tools[0].ID {
		t.Fatalf("inconsistent mapping: %#v", mapped)
	}
	if !strings.Contains(string(mapped.Extension), alias) || !strings.Contains(string(mapped.Extension), `"temperature":0.2`) {
		t.Fatalf("tool choice was not mapped: %s", mapped.Extension)
	}
	if request.Tools[0].ID != original || request.Messages[0].ToolCalls.Values()[0].ToolID != original {
		t.Fatal("input request was mutated")
	}
}

func TestProviderToolCallsRestoreCanonicalID(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if len(body.Tools) != 1 || !validToolName(body.Tools[0].Function.Name) {
					t.Errorf("invalid provider tool names: %#v", body.Tools)
					return
				}
				name := body.Tools[0].Function.Name
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":\"{}\"}}]}}]}\n\n", name)
					_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, name)
				}
			}))
			defer server.Close()
			model := newAdapter(t, server, Config{APIKey: "key", Model: "model"})
			want := lebro.ToolID("shopify-dev-mcp.search_docs")
			req := lebro.ModelRequest{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "search"}}, Tools: []lebro.ToolDefinition{{ID: want, InputSchema: json.RawMessage(`{"type":"object"}`)}}}
			if !streaming {
				response, err := model.Generate(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if got := response.Message.ToolCalls.Values(); len(got) != 1 || got[0].ToolID != want {
					t.Fatalf("tool calls: %#v", got)
				}
				return
			}
			reader, err := model.Stream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			var got lebro.ToolID
			for {
				delta, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if delta.ToolCall != nil {
					got = delta.ToolCall.ToolID
				}
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestToolNameAliasesAvoidReservedNames(t *testing.T) {
	long := lebro.ToolID(strings.Repeat("x", 80))
	request := lebro.ModelRequest{Tools: []lebro.ToolDefinition{{ID: long}}}
	first, _, err := mapToolNames(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Tools = append(request.Tools, lebro.ToolDefinition{ID: first.Tools[0].ID})
	second, reverse, err := mapToolNames(request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tools[0].ID == second.Tools[1].ID || reverse[string(second.Tools[0].ID)] != long {
		t.Fatalf("tool aliases collide: %#v", second.Tools)
	}
}
