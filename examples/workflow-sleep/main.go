// The workflow-sleep example shows durable delay steps. Run returns as soon as
// it persists the wait; a Scheduler tick later resumes the run from its saved
// snapshot without re-running preceding work.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tesh254/lebro"
)

func main() { must(run(os.Stdout)) }

func run(output io.Writer) error {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := lebro.NewMemoryStore()
	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "reminder"}, Store: store, Clock: lebro.NewFixedClock(now), IDSource: lebro.NewFixedIDSource([]lebro.RunID{"reminder-run"}, nil),
		Steps: []lebro.Step{
			{Definition: lebro.StepDefinition{ID: "wait", Sleep: &lebro.Sleep{Duration: time.Hour}}},
			{Definition: lebro.StepDefinition{ID: "send"}, Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) { return input, nil })},
		},
	})
	if err != nil {
		return err
	}
	result, err := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`{"message":"hello"}`)})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "initial status: %s\n", result.Status)
	scheduler, err := lebro.NewScheduler(lebro.SchedulerConfig{Store: store, Resolver: lebro.WorkflowMap{"reminder": wf}, Clock: lebro.NewFixedClock(now.Add(time.Hour))})
	if err != nil {
		return err
	}
	if _, err := scheduler.Tick(ctx, now.Add(time.Hour)); err != nil {
		return err
	}
	run, err := store.WorkflowRuns().GetWorkflowRun(ctx, result.ID)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "final status: %s output: %s\n", run.Status, run.Output)
	return err
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
