// Package main demonstrates a linear workflow that suspends at an approval
// step, survives a process restart, and resumes from its durable snapshot.
//
// The workflow has two steps: "double" doubles the input number, and
// "await-approval" suspends with a resume contract that requires the resume
// input to equal {"approved":true}. The example runs the workflow, closes the
// store, reopens the same SQLite file, and resumes with a valid approval so
// the second step runs. No network or API key is required.
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
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func main() {
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "lebro-workflow-suspend-resume-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dsn := filepath.Join(dir, "suspend.db")

	store, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	build := func() (*lebro.LinearWorkflow, error) {
		return lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
			Definition:     lebro.WorkflowDefinition{ID: "approval-double", Name: "Approval Double", Version: "v1"},
			SchemaCompiler: lebrojsonschema.NewCompiler(),
			Store:          store,
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
					Definition: lebro.StepDefinition{
						ID:            "await-approval",
						SuspendSchema: json.RawMessage(`{"type":"object","required":["approved"],"properties":{"approved":{"type":"boolean","const":true}}}`),
					},
					Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
						return nil, &lebro.SuspendError{Signal: lebro.SuspendSignal{
							StepID:   "await-approval",
							Contract: json.RawMessage(`{"approved":true}`),
							Payload:  json.RawMessage(`{"pending":"human approval"}`),
						}}
					}),
				},
				{
					Definition: lebro.StepDefinition{ID: "finish"},
					Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
						var approved struct {
							Approved bool `json:"approved"`
						}
						if err := json.Unmarshal(input, &approved); err != nil {
							return nil, err
						}
						return json.Marshal(map[string]any{"approved": approved.Approved, "done": true})
					}),
				},
			},
		})
	}

	wf, err := build()
	if err != nil {
		return err
	}

	result, err := wf.Run(ctx, lebro.WorkflowRunInput{Input: json.RawMessage(`5`)})
	if err != nil {
		return err
	}
	writef(output, "suspended status: %s\n", result.Status)
	writef(output, "suspended at step: %d (%s)\n", result.Suspend.Step, result.Suspend.StepID)
	writef(output, "resume contract: %s\n", result.Suspend.Contract)

	// Simulate an invalid resume: rejected without corrupting the snapshot.
	_, err = wf.Resume(ctx, lebro.WorkflowResumeInput{RunID: result.ID, Input: json.RawMessage(`{"approved":false}`)})
	if !errors.Is(err, lebro.ErrInvalidResumeInput) {
		return fmt.Errorf("expected ErrInvalidResumeInput, got %v", err)
	}
	writef(output, "invalid resume rejected: %v\n", err)

	// Close and reopen the store to prove the suspend survives a restart.
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
	store = reopened

	wf2, err := build()
	if err != nil {
		return err
	}
	resumed, err := wf2.Resume(ctx, lebro.WorkflowResumeInput{RunID: result.ID, Input: json.RawMessage(`{"approved":true}`)})
	if err != nil {
		return err
	}
	writef(output, "resumed status: %s\n", resumed.Status)
	writef(output, "resumed output: %s\n", resumed.Output)

	stored, err := store.WorkflowRuns().GetWorkflowRun(ctx, result.ID)
	if err != nil {
		return err
	}
	writef(output, "persisted final status: %s\n", stored.Status)
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}
