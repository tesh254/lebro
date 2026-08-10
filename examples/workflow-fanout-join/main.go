// The workflow-fanout-join example runs a workflow with a fan-out step that
// executes two independent branches concurrently and joins their outputs in
// declaration order. No network or API key required.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "fanout-join-demo", Name: "Fan-Out and Join"},
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID: "fanout",
					FanOut: &lebro.FanOut{
						MaxParallel: 2,
						Branches: []lebro.FanOutBranch{
							{
								Name: "enrichment",
								Steps: []lebro.Step{{
									Definition: lebro.StepDefinition{ID: "enrich"},
									Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
										time.Sleep(50 * time.Millisecond)
										var v struct {
											ID string `json:"id"`
										}
										_ = json.Unmarshal(input, &v)
										return json.Marshal(map[string]any{
											"id":      v.ID,
											"enriched": true,
										})
									}),
								}},
							},
							{
								Name: "risk-check",
								Steps: []lebro.Step{{
									Definition: lebro.StepDefinition{ID: "risk"},
									Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
										time.Sleep(30 * time.Millisecond)
										var v struct {
											ID string `json:"id"`
										}
										_ = json.Unmarshal(input, &v)
										return json.Marshal(map[string]any{
											"id":    v.ID,
											"score": 42,
										})
									}),
								}},
							},
						},
					},
				},
			},
			{
				Definition: lebro.StepDefinition{ID: "summarize"},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var branches []struct {
						Name   string          `json:"name"`
						Output json.RawMessage `json:"output"`
					}
					if err := json.Unmarshal(input, &branches); err != nil {
						return nil, err
					}
					summary := map[string]any{}
					for _, b := range branches {
						var data map[string]any
						_ = json.Unmarshal(b.Output, &data)
						summary[b.Name] = data
					}
					return json.Marshal(summary)
				}),
			},
		},
	})
	if err != nil {
		return err
	}

	var mu sync.Mutex
	writef(output, &mu, "running fan-out with 2 concurrent branches...\n")

	result, err := wf.Run(context.Background(), lebro.WorkflowRunInput{
		Input: json.RawMessage(`{"id":"user-123"}`),
	})
	if err != nil {
		return err
	}

	writef(output, &mu, "status: %s\n", result.Status)
	writef(output, &mu, "fan-out joins: %d\n", len(result.FanOut))
	if len(result.FanOut) > 0 {
		join := result.FanOut[0]
		writef(output, &mu, "  join step: %s, status: %s, branches: %d\n", join.StepID, join.Status, len(join.Branches))
		for _, br := range join.Branches {
			writef(output, &mu, "    branch %q: %s\n", br.Name, br.Status)
		}
	}

	var summary map[string]any
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return err
	}
	writef(output, &mu, "summary: %s\n", prettyJSON(summary))
	return nil
}

func writef(output io.Writer, mu *sync.Mutex, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

func prettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
