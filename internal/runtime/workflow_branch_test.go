package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func echoHandler() StepHandler {
	return StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), input...), nil
	})
}

func jsonFieldString(input json.RawMessage, field string) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	v, _ := m[field].(string)
	return v
}

func TestBranchingStepSelectsMatchingBranch(t *testing.T) {
	t.Parallel()

	branchA := echoHandler()
	branchB := echoHandler()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "branch-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, input json.RawMessage) (bool, error) {
								return jsonFieldString(input, "route") == "a", nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: branchA},
							},
						},
						{
							Name: "b",
							Condition: func(_ context.Context, input json.RawMessage) (bool, error) {
								return jsonFieldString(input, "route") == "b", nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-b"}, Handler: branchB},
							},
						},
					},
				},
			},
			{
				Definition: StepDefinition{ID: "after"},
				Handler:    echoHandler(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{"route":"b"}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if len(result.Path) != 1 || result.Path[0] != "step-b" {
		t.Fatalf("path = %v, want [step-b]", result.Path)
	}
}

func TestBranchingStepDefaultBranch(t *testing.T) {
	t.Parallel()

	defaultRan := false

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "default-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID:      "router",
					Default: "fallback",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return false, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
							},
						},
						{
							Name: "fallback",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return false, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-fallback"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
									defaultRan = true
									return append(json.RawMessage(nil), input...), nil
								})},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !defaultRan {
		t.Fatal("default branch did not run")
	}
	if len(result.Path) != 1 || result.Path[0] != "step-fallback" {
		t.Fatalf("path = %v, want [step-fallback]", result.Path)
	}
}

func TestBranchingStepNoMatchFailsRun(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "no-match-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return false, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrWorkflowNoBranchMatched) {
		t.Fatalf("error = %v, want ErrWorkflowNoBranchMatched", err)
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) {
		t.Fatalf("error = %v, want *WorkflowError", err)
	}
	if wfErr.Kind != WorkflowErrorNoBranchMatched {
		t.Fatalf("kind = %q, want no_branch_matched", wfErr.Kind)
	}
	if wfErr.StepID != "router" {
		t.Fatalf("stepID = %q, want router", wfErr.StepID)
	}
}

func TestBranchingStepConditionErrorFailsRun(t *testing.T) {
	t.Parallel()

	condErr := errors.New("db unavailable")
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "cond-err-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return false, condErr
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrWorkflowBranchConditionFailed) {
		t.Fatalf("error = %v, want ErrWorkflowBranchConditionFailed", err)
	}
	if !strings.Contains(err.Error(), "branch \"a\" condition failed") {
		t.Fatalf("error = %v, want branch name in message", err)
	}
}

func TestBranchingStepFailureCarriesStepIdentity(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("branch step failure")
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "branch-fail-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return true, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
								{
									Definition: StepDefinition{ID: "step-a-fail"},
									Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
										return nil, handlerErr
									}),
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) {
		t.Fatalf("error = %v, want *WorkflowError", err)
	}
	if wfErr.Kind != WorkflowErrorStepFailed {
		t.Fatalf("kind = %q, want step_failed", wfErr.Kind)
	}
	if wfErr.StepID != "step-a-fail" {
		t.Fatalf("stepID = %q, want step-a-fail", wfErr.StepID)
	}
}

func TestBranchingStepEventsOrdered(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "branch-events-wf"},
		Listener:   recorder,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return true, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
							},
						},
					},
				},
			},
			{
				Definition: StepDefinition{ID: "after"},
				Handler:    echoHandler(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events := recorder.Events()
	var types []RunEventType
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []RunEventType{
		RunEventStarted,
		RunEventStepStarted,
		RunEventBranchSelected,
		RunEventStepFinished,
		RunEventStepStarted,
		RunEventStepFinished,
		RunEventStepStarted,
		RunEventStepFinished,
		RunEventSucceeded,
	}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i, ty := range types {
		if ty != want[i] {
			t.Fatalf("event %d = %q, want %q", i, ty, want[i])
		}
	}
	var branchEvent *RunEvent
	for i := range events {
		if events[i].Type == RunEventBranchSelected {
			branchEvent = &events[i]
			break
		}
	}
	if branchEvent == nil {
		t.Fatal("no branch_selected event")
	}
	if branchEvent.Branch != "a" {
		t.Fatalf("branch = %q, want a", branchEvent.Branch)
	}
	if branchEvent.StepID != "router" {
		t.Fatalf("stepID = %q, want router", branchEvent.StepID)
	}
}

func TestBranchingStepPathPersistsAcrossSteps(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "branch-persist-wf"},
		Store:      store,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return true, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
								{Definition: StepDefinition{ID: "step-a2"}, Handler: echoHandler()},
							},
						},
						{
							Name: "b",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return false, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "step-b"}, Handler: echoHandler()},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Path) != 1 || result.Path[0] != "step-a" {
		t.Fatalf("path = %v, want [step-a]", result.Path)
	}

	runs, err := store.WorkflowRuns().ListWorkflowRuns(context.Background(), WorkflowRunFilter{}, PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(runs.Records))
	}
	stored := runs.Records[0]
	if len(stored.Path) != 1 || stored.Path[0] != "step-a" {
		t.Fatalf("stored path = %v, want [step-a]", stored.Path)
	}
}

func TestNewLinearWorkflowRejectsInvalidBranchConfiguration(t *testing.T) {
	t.Parallel()

	echo := echoHandler()
	cond := func(_ context.Context, _ json.RawMessage) (bool, error) { return true, nil }

	tests := []struct {
		name string
		cfg  LinearWorkflowConfig
		want string
	}{
		{
			name: "branching step with handler",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "router",
						Branches: []Branch{{
							Name:      "a",
							Condition: cond,
							Steps:     []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
					Handler: echo,
				}},
			},
			want: "must not declare a Handler",
		},
		{
			name: "branching step with output schema",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:           "router",
						OutputSchema: json.RawMessage(`{"type":"object"}`),
						Branches: []Branch{{
							Name:      "a",
							Condition: cond,
							Steps:     []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
				}},
			},
			want: "must not declare OutputSchema",
		},
		{
			name: "branching step with suspend schema",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:            "router",
						SuspendSchema: json.RawMessage(`{"type":"object"}`),
						Branches: []Branch{{
							Name:      "a",
							Condition: cond,
							Steps:     []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
				}},
			},
			want: "must not declare SuspendSchema",
		},
		{
			name: "branching step with retry",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:    "router",
						Retry: &RetryPolicy{Attempts: 2},
						Branches: []Branch{{
							Name:      "a",
							Condition: cond,
							Steps:     []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
				}},
			},
			want: "must not declare Retry",
		},
		{
			name: "branch with empty name",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "router",
						Branches: []Branch{{
							Name:      "",
							Condition: cond,
							Steps:     []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
				}},
			},
			want: "empty Name",
		},
		{
			name: "duplicate branch name",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "router",
						Branches: []Branch{
							{Name: "a", Condition: cond, Steps: []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}}},
							{Name: "a", Condition: cond, Steps: []Step{{Definition: StepDefinition{ID: "step-a2"}, Handler: echo}}},
						},
					},
				}},
			},
			want: "duplicate branch name",
		},
		{
			name: "branch with no steps",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "router",
						Branches: []Branch{{
							Name:      "a",
							Condition: cond,
						}},
					},
				}},
			},
			want: "has no steps",
		},
		{
			name: "branch with nil condition",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "router",
						Branches: []Branch{{
							Name:  "a",
							Steps: []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
				}},
			},
			want: "nil Condition",
		},
		{
			name: "default names undeclared branch",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:      "router",
						Default: "nonexistent",
						Branches: []Branch{{
							Name:      "a",
							Condition: cond,
							Steps:     []Step{{Definition: StepDefinition{ID: "step-a"}, Handler: echo}},
						}},
					},
				}},
			},
			want: "not a declared branch",
		},
		{
			name: "duplicate step ID across branch",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "router",
						Branches: []Branch{
							{
								Name:      "a",
								Condition: cond,
								Steps:     []Step{{Definition: StepDefinition{ID: "shared"}, Handler: echo}},
							},
							{
								Name:      "b",
								Condition: cond,
								Steps:     []Step{{Definition: StepDefinition{ID: "shared"}, Handler: echo}},
							},
						},
					},
				}},
			},
			want: "already registered",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewLinearWorkflow(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewLinearWorkflow() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBranchingStepNestedBranchesSelectsCorrectPath(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "nested-wf"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "outer",
					Branches: []Branch{
						{
							Name: "outer-a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return true, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "outer-step-a"}, Handler: echoHandler()},
								{
									Definition: StepDefinition{
										ID: "inner",
										Branches: []Branch{
											{
												Name: "inner-x",
												Condition: func(_ context.Context, input json.RawMessage) (bool, error) {
													return jsonFieldString(input, "inner") == "x", nil
												},
												Steps: []Step{
													{Definition: StepDefinition{ID: "inner-step-x"}, Handler: echoHandler()},
												},
											},
											{
												Name: "inner-y",
												Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
													return true, nil
												},
												Steps: []Step{
													{Definition: StepDefinition{ID: "inner-step-y"}, Handler: echoHandler()},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{"inner":"x"}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if len(result.Path) != 2 || result.Path[0] != "outer-step-a" || result.Path[1] != "inner-step-x" {
		t.Fatalf("path = %v, want [outer-step-a inner-step-x]", result.Path)
	}
}

func TestBranchingStepSuspendAndResume(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}}
	resumeSignal := make(chan json.RawMessage, 1)
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "branch-suspend-wf"},
		Store:          store,
		SchemaCompiler: compiler,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "router",
					Branches: []Branch{
						{
							Name: "a",
							Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
								return true, nil
							},
							Steps: []Step{
								{Definition: StepDefinition{ID: "pre-suspend"}, Handler: echoHandler()},
								{
									Definition: StepDefinition{
										ID:            "suspend-step",
										SuspendSchema: json.RawMessage(`{"type":"object"}`),
									},
									Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
										return nil, &SuspendError{Signal: SuspendSignal{
											StepID:   "suspend-step",
											Contract: json.RawMessage(`{"ok":true}`),
										}}
									}),
								},
								{Definition: StepDefinition{ID: "post-suspend"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
									resumeSignal <- append(json.RawMessage(nil), input...)
									return append(json.RawMessage(nil), input...), nil
								})},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSuspended {
		t.Fatalf("status = %q, want suspended", result.Status)
	}
	if len(result.Path) != 1 || result.Path[0] != "pre-suspend" {
		t.Fatalf("path = %v, want [pre-suspend]", result.Path)
	}

	resumeResult, err := wf.Resume(context.Background(), WorkflowResumeInput{
		RunID: result.ID,
		Input: json.RawMessage(`{"ok":true}`),
	})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumeResult.Status != RunStatusSucceeded {
		t.Fatalf("resume status = %q, want succeeded", resumeResult.Status)
	}
	if len(resumeResult.Path) != 1 || resumeResult.Path[0] != "pre-suspend" {
		t.Fatalf("resume path = %v, want [pre-suspend]", resumeResult.Path)
	}

	received := <-resumeSignal
	if string(received) != `{"ok":true}` {
		t.Fatalf("post-suspend received = %s, want {\"ok\":true}", received)
	}
}

func TestBranchingStepInputSchemaValidation(t *testing.T) {
	t.Parallel()

	compiler := stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{validationErr: &ValidationError{Target: ValidationTargetStepInput, Issues: []ValidationIssue{{Path: "/", Keyword: "type", Message: "expected object"}}}}, nil
	}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "branch-input-wf"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID:          "router",
					InputSchema: json.RawMessage(`{"type":"object"}`),
					Branches: []Branch{{
						Name: "a",
						Condition: func(_ context.Context, _ json.RawMessage) (bool, error) {
							t.Fatal("condition should not run on invalid input")
							return false, nil
						},
						Steps: []Step{
							{Definition: StepDefinition{ID: "step-a"}, Handler: echoHandler()},
						},
					}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrWorkflowInvalidBranchInput) {
		t.Fatalf("error = %v, want ErrWorkflowInvalidBranchInput", err)
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) {
		t.Fatalf("error = %v, want *WorkflowError", err)
	}
	if wfErr.Kind != WorkflowErrorInvalidBranchInput {
		t.Fatalf("kind = %q, want invalid_branch_input", wfErr.Kind)
	}
}
