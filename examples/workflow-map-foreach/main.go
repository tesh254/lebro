// The workflow-map-foreach example selects an input array declaratively, then
// processes every item with bounded concurrency while retaining input order.
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
		panic(err)
	}
}

func run(output io.Writer) error {
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "map-foreach-demo", Name: "Map and ForEach"},
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "select-items", Map: &lebro.Map{Path: "items"}}},
			{
				Definition: lebro.StepDefinition{
					ID: "double-items",
					ForEach: &lebro.ForEach{
						MaxParallel: 2,
						Steps: []lebro.Step{{
							Definition: lebro.StepDefinition{ID: "double"},
							Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
								var value int
								if err := json.Unmarshal(input, &value); err != nil {
									return nil, err
								}
								return json.Marshal(value * 2)
							}),
						}},
					},
				},
			},
		},
	})
	if err != nil {
		return err
	}
	result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{Input: json.RawMessage(`{"items":[1,2,3]}`)})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "status: %s\noutput: %s\n", result.Status, result.Output)
	return err
}
