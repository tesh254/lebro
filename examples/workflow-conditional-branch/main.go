// The workflow-conditional-branch example runs a workflow with a branching
// step that routes input to one of two named paths based on a JSON field.
// No network or API key required.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition:     lebro.WorkflowDefinition{ID: "route-by-tier", Name: "Route by Tier"},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID:          "router",
					InputSchema: json.RawMessage(`{"type":"object","required":["tier"],"properties":{"tier":{"type":"string"}}}`),
					Branches: []lebro.Branch{
						{
							Name: "premium",
							Condition: func(_ context.Context, input json.RawMessage) (bool, error) {
								var v struct {
									Tier string `json:"tier"`
								}
								_ = json.Unmarshal(input, &v)
								return v.Tier == "premium", nil
							},
							Steps: []lebro.Step{{
								Definition: lebro.StepDefinition{
									ID:           "premium-handler",
									OutputSchema: json.RawMessage(`{"type":"object","required":["plan"],"properties":{"plan":{"type":"string"}}}`),
								},
								Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
									return json.Marshal(map[string]string{"plan": "enterprise"})
								}),
							}},
						},
						{
							Name: "standard",
							Condition: func(_ context.Context, input json.RawMessage) (bool, error) {
								var v struct {
									Tier string `json:"tier"`
								}
								_ = json.Unmarshal(input, &v)
								return v.Tier == "standard", nil
							},
							Steps: []lebro.Step{{
								Definition: lebro.StepDefinition{
									ID:           "standard-handler",
									OutputSchema: json.RawMessage(`{"type":"object","required":["plan"],"properties":{"plan":{"type":"string"}}}`),
								},
								Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
									return json.Marshal(map[string]string{"plan": "pro"})
								}),
							}},
						},
					},
					Default: "standard",
				},
			},
		},
	})
	if err != nil {
		return err
	}

	result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{
		Input: json.RawMessage(`{"tier":"premium"}`),
	})
	if err != nil {
		var wfErr *lebro.WorkflowError
		if errors.As(err, &wfErr) {
			return fmt.Errorf("workflow %s at step %q: %w", wfErr.Kind, wfErr.StepID, wfErr)
		}
		return err
	}

	var out struct {
		Plan string `json:"plan"`
	}
	if err := result.DecodeOutput(&out); err != nil {
		return err
	}
	writef(output, "status: %s\n", result.Status)
	writef(output, "plan: %s\n", out.Plan)
	writef(output, "path: %v\n", result.Path)
	return nil
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
