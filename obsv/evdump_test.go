package obsv_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/testkit"
)

func TestDumpEvents(t *testing.T) {
	recorder := lebro.NewRunRecorder()
	registry := newRegistry(t, echoTool{id: "lookup"})
	model := testkit.NewModel(
		testkit.ToolCallResponse(testkit.ToolCall{ID: "call-1", ToolID: "lookup", Arguments: json.RawMessage(`{"query":"h"}`)}),
		testkit.Text("done"),
	)
	agent, _ := lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{ID: "agent", Model: "fixture", Tools: []lebro.ToolID{"lookup"}},
		Model:      model, Tools: registry, Listener: recorder,
	})
	agentStep, _ := lebro.NewAgentStep(agent)
	wf, _ := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "pipeline"},
		Listener:   recorder,
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "prepare"}, Handler: lebro.StepHandlerFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`"summarize"`), nil
			})},
			{Definition: lebro.StepDefinition{ID: "agent"}, Handler: agentStep},
		},
	})
	if _, err := wf.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`"start"`)}); err != nil {
		t.Logf("run err: %v", err)
	}
	for _, e := range recorder.Events() {
		t.Logf("seq=%2d %-22s run=%-16s parentRun=%-16s parentStepID=%-10s parentStep=%d stepID=%-10s step=%d err=%v",
			e.Sequence, e.Type, e.RunID, e.ParentRunID, e.ParentStepID, e.ParentStep, e.StepID, e.Step, e.Error)
	}
}
