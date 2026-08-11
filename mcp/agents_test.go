package mcp_test

import (
	"context"
	"testing"

	"github.com/tesh254/lebro"
)

type panickingModel struct{}

func (panickingModel) Generate(context.Context, lebro.ModelRequest) (lebro.ModelResponse, error) {
	panic("provider panic")
}

func TestExposeAgent_PanicReturnsToolError(t *testing.T) {
	srv := newTestServer(t)
	agent := mustAgent(t, panickingModel{}, "panic", "")
	must(t, srv.ExposeAgent(agent))

	session, cleanup := connectServer(t, srv)
	defer cleanup()
	result := callTool(t, session, "agent.panic", map[string]any{
		"messages": []map[string]any{{"content": "Hi"}},
	})
	if !result.IsError {
		t.Fatal("expected IsError=true for model panic")
	}
}
