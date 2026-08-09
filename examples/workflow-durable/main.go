// The workflow-durable example runs a two-step linear workflow bound to a
// file-backed SQLite store so the run record and per-step snapshots survive a
// process restart. It runs the workflow, closes the store, reopens the same
// file, and prints the persisted run plus the ordered snapshot boundaries.
// No network or API key required.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tesh254/lebro"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	ctx := context.Background()
	dsn := filepath.Join(os.TempDir(), "lebro-workflow-durable.db")
	_ = os.Remove(dsn)

	store, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "durable-double-and-add", Name: "Durable Double and Add One", Version: "v1"},
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{ID: "double"},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var n int
					if err := json.Unmarshal(input, &n); err != nil {
						return nil, err
					}
					return json.Marshal(n * 2)
				}),
			},
			{
				Definition: lebro.StepDefinition{ID: "add-one"},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var n int
					if err := json.Unmarshal(input, &n); err != nil {
						return nil, err
					}
					return json.Marshal(n + 1)
				}),
			},
		},
		Store: store,
	})
	if err != nil {
		return err
	}

	result, err := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`5`)})
	if err != nil {
		var wfErr *lebro.WorkflowError
		if errors.As(err, &wfErr) {
			return fmt.Errorf("workflow %s at step %q: %w", wfErr.Kind, wfErr.StepID, wfErr)
		}
		return err
	}

	writef(output, "live status: %s\n", result.Status)
	writef(output, "live output: %s\n", string(result.Output))

	if err := store.Close(); err != nil {
		return err
	}

	reopened, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		return err
	}

	run, err := reopened.WorkflowRuns().GetWorkflowRun(ctx, result.ID)
	if err != nil {
		return err
	}
	writef(output, "persisted status: %s\n", run.Status)
	writef(output, "persisted version: %s\n", run.WorkflowVersion)
	writef(output, "persisted current step: %d (%s)\n", run.CurrentStep, run.CurrentStepID)
	writef(output, "persisted step outputs: %s\n", renderOutputs(run.StepOutputs))

	snapshots, err := reopened.WorkflowSnapshots().ListWorkflowSnapshots(ctx, result.ID, lebro.PageRequest{})
	if err != nil {
		return err
	}
	writef(output, "persisted snapshots: %d\n", len(snapshots.Records))
	return nil
}

func renderOutputs(outputs []json.RawMessage) string {
	parts := make([]string, 0, len(outputs))
	for _, output := range outputs {
		parts = append(parts, string(output))
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += ", "
		}
		out += part
	}
	return out
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
