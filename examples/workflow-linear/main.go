// The workflow-linear example runs a two-step linear workflow with schema-backed
// handoffs against a real JSON Schema compiler. The first step doubles a number;
// the second adds one. No network or API key required.
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
		Definition:     lebro.WorkflowDefinition{ID: "double-and-add", Name: "Double and Add One"},
		SchemaCompiler: lebrojsonschema.NewCompiler(),
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID:           "double",
					InputSchema:  json.RawMessage(`{"type":"integer"}`),
					OutputSchema: json.RawMessage(`{"type":"integer"}`),
				},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var n int
					if err := json.Unmarshal(input, &n); err != nil {
						return nil, err
					}
					return json.Marshal(n * 2)
				}),
			},
			{
				Definition: lebro.StepDefinition{
					ID:           "add-one",
					InputSchema:  json.RawMessage(`{"type":"integer"}`),
					OutputSchema: json.RawMessage(`{"type":"integer"}`),
				},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var n int
					if err := json.Unmarshal(input, &n); err != nil {
						return nil, err
					}
					return json.Marshal(n + 1)
				}),
			},
		},
	})
	if err != nil {
		return err
	}

	result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{
		Input: json.RawMessage(`5`),
	})
	if err != nil {
		var wfErr *lebro.WorkflowError
		if errors.As(err, &wfErr) {
			return fmt.Errorf("workflow %s at step %q: %w", wfErr.Kind, wfErr.StepID, wfErr)
		}
		return err
	}

	var final int
	if err := result.DecodeOutput(&final); err != nil {
		return err
	}
	writef(output, "status: %s\n", result.Status)
	writef(output, "output: %d\n", final)
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
