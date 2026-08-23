// workflow-loops demonstrates bounded post-condition workflow loops.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/tesh254/lebro"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "retry-until-ready"},
		Steps: []lebro.Step{{Definition: lebro.StepDefinition{ID: "poll", DoUntil: &lebro.DoUntil{
			MaxIterations: 3,
			Steps: []lebro.Step{{Definition: lebro.StepDefinition{ID: "attempt"}, Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				var state struct {
					Attempts int `json:"attempts"`
				}
				if err := json.Unmarshal(input, &state); err != nil {
					return nil, err
				}
				state.Attempts++
				return json.Marshal(state)
			})}},
			Condition: func(_ context.Context, output json.RawMessage, _ int) (bool, error) {
				var state struct {
					Attempts int `json:"attempts"`
				}
				if err := json.Unmarshal(output, &state); err != nil {
					return false, err
				}
				return state.Attempts >= 2, nil
			},
		}}}},
	})
	if err != nil {
		return err
	}
	result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`{"attempts":0}`)})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(result.Output))
	return err
}
