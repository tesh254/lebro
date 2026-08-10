package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFanOutRunsBranchesConcurrently(t *testing.T) {
	t.Parallel()

	var active int32
	var maxActive int32
	var mu sync.Mutex

	delayHandler := func(delay time.Duration) StepHandler {
		return StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
			cur := atomic.AddInt32(&active, 1)
			mu.Lock()
			if cur > maxActive {
				maxActive = cur
			}
			mu.Unlock()
			time.Sleep(delay)
			atomic.AddInt32(&active, -1)
			return append(json.RawMessage(nil), input...), nil
		})
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-concurrent"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						MaxParallel: 2,
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: delayHandler(50 * time.Millisecond)}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: delayHandler(50 * time.Millisecond)}}},
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
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if maxActive < 2 {
		t.Fatalf("maxActive = %d, want >= 2 (branches ran concurrently)", maxActive)
	}
}

func TestFanOutJoinOrderFollowsDeclaration(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-order"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						MaxParallel: 2,
						Branches: []FanOutBranch{
							{Name: "slow", Steps: []Step{{Definition: StepDefinition{ID: "fs"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								time.Sleep(30 * time.Millisecond)
								return json.RawMessage(`{"branch":"slow"}`), nil
							})}}},
							{Name: "fast", Steps: []Step{{Definition: StepDefinition{ID: "ff"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"branch":"fast"}`), nil
							})}}},
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

	var joined []struct {
		Name   string          `json:"name"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(result.Output, &joined); err != nil {
		t.Fatalf("unmarshal joined output: %v", err)
	}
	if len(joined) != 2 {
		t.Fatalf("joined length = %d, want 2", len(joined))
	}
	if joined[0].Name != "slow" || joined[1].Name != "fast" {
		t.Fatalf("join order = [%s, %s], want [slow, fast] (declaration order)", joined[0].Name, joined[1].Name)
	}
}

func TestFanOutInputIsolation(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-isolation"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: echoHandler()}}},
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
		Input: json.RawMessage(`{"shared":true}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestFanOutFailFastCancelsSiblings(t *testing.T) {
	t.Parallel()

	var bRan atomic.Bool

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-failfast"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						MaxParallel:   2,
						FailurePolicy: FanOutFailFast,
						Branches: []FanOutBranch{
							{Name: "fail", Steps: []Step{{Definition: StepDefinition{ID: "ffail"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return nil, errors.New("branch failure")
							})}}},
							{Name: "slow", Steps: []Step{{Definition: StepDefinition{ID: "fslow"}, Handler: StepHandlerFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
								select {
								case <-time.After(5 * time.Second):
									bRan.Store(true)
									return json.RawMessage(`{}`), nil
								case <-ctx.Done():
									return nil, ctx.Err()
								}
							})}}},
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
	if !errors.Is(err, ErrWorkflowFanOutBranchFailed) {
		t.Fatalf("error = %v, want ErrWorkflowFanOutBranchFailed", err)
	}
	if bRan.Load() {
		t.Fatal("slow branch should have been cancelled, not completed")
	}
}

func TestFanOutCollectAllWaitsForAllBranches(t *testing.T) {
	t.Parallel()

	var bDone atomic.Bool

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-collectall"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						MaxParallel:   2,
						FailurePolicy: FanOutCollectAll,
						Branches: []FanOutBranch{
							{Name: "fail", Steps: []Step{{Definition: StepDefinition{ID: "ffail"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return nil, errors.New("immediate failure")
							})}}},
							{Name: "slow", Steps: []Step{{Definition: StepDefinition{ID: "fslow"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								time.Sleep(50 * time.Millisecond)
								bDone.Store(true)
								return json.RawMessage(`{"ok":true}`), nil
							})}}},
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
	if !errors.Is(err, ErrWorkflowFanOutBranchFailed) {
		t.Fatalf("error = %v, want ErrWorkflowFanOutBranchFailed", err)
	}
	if !bDone.Load() {
		t.Fatal("slow branch should have completed (collect-all policy)")
	}
}

func TestFanOutExternalCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-cancel"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
								select {
								case <-time.After(5 * time.Second):
									return json.RawMessage(`{}`), nil
								case <-ctx.Done():
									return nil, ctx.Err()
								}
							})}}},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = wf.Run(ctx, WorkflowRunInput{
		Input: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrWorkflowCancelled) {
		t.Fatalf("error = %v, want ErrWorkflowCancelled", err)
	}
}

func TestFanOutWithInputMapper(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-mapper"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{
								Name: "a",
								InputMapper: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
									return json.RawMessage(`{"mapped":"a"}`), nil
								},
								Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}},
							},
							{
								Name: "b",
								InputMapper: func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
									return json.RawMessage(`{"mapped":"b"}`), nil
								},
								Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: echoHandler()}},
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
		Input: json.RawMessage(`{"original":true}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var joined []struct {
		Name   string          `json:"name"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(result.Output, &joined); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(joined[0].Output) != `{"mapped":"a"}` {
		t.Fatalf("branch a output = %s, want {\"mapped\":\"a\"}", joined[0].Output)
	}
	if string(joined[1].Output) != `{"mapped":"b"}` {
		t.Fatalf("branch b output = %s, want {\"mapped\":\"b\"}", joined[1].Output)
	}
}

func TestFanOutWithSchemaValidation(t *testing.T) {
	t.Parallel()

	compiler := stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "fanout-schema"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID:          "fanout",
					InputSchema: json.RawMessage(`{"type":"object"}`),
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}}},
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
		Input: json.RawMessage(`{"valid":true}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestFanOutResultExposesBranchResults(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-result"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"a":1}`), nil
							})}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"b":2}`), nil
							})}}},
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
	if len(result.FanOut) != 1 {
		t.Fatalf("FanOut results = %d, want 1", len(result.FanOut))
	}
	join := result.FanOut[0]
	if join.StepID != "fanout" {
		t.Fatalf("join StepID = %q, want fanout", join.StepID)
	}
	if join.Status != RunStatusSucceeded {
		t.Fatalf("join status = %q, want succeeded", join.Status)
	}
	if len(join.Branches) != 2 {
		t.Fatalf("branches = %d, want 2", len(join.Branches))
	}
	if join.Branches[0].Name != "a" || join.Branches[0].Status != RunStatusSucceeded {
		t.Fatalf("branch 0 = %+v, want a/succeeded", join.Branches[0])
	}
	if join.Branches[1].Name != "b" || join.Branches[1].Status != RunStatusSucceeded {
		t.Fatalf("branch 1 = %+v, want b/succeeded", join.Branches[1])
	}
}

func TestFanOutFollowedBySubsequentStep(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-then-step"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`10`), nil
							})}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`20`), nil
							})}}},
						},
					},
				},
			},
			{
				Definition: StepDefinition{ID: "sum"},
				Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var joined []struct {
						Output json.RawMessage `json:"output"`
					}
					if err := json.Unmarshal(input, &joined); err != nil {
						return nil, err
					}
					total := 0
					for _, j := range joined {
						var n int
						_ = json.Unmarshal(j.Output, &n)
						total += n
					}
					return json.Marshal(total)
				}),
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
	var sum int
	if err := json.Unmarshal(result.Output, &sum); err != nil {
		t.Fatalf("unmarshal sum: %v", err)
	}
	if sum != 30 {
		t.Fatalf("sum = %d, want 30", sum)
	}
}

func TestFanOutEventsEmitted(t *testing.T) {
	t.Parallel()

	recorder := NewRunRecorder()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-events"},
		Listener:   recorder,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: echoHandler()}}},
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
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events := recorder.Events()

	hasStarted := false
	hasSucceeded := false
	childStepCount := 0
	for _, e := range events {
		if e.Type == RunEventStarted {
			hasStarted = true
		}
		if e.Type == RunEventSucceeded {
			hasSucceeded = true
		}
		if e.Type == RunEventStepStarted && e.Branch != "" {
			childStepCount++
		}
	}
	if !hasStarted {
		t.Fatal("missing run_started event")
	}
	if !hasSucceeded {
		t.Fatal("missing run_succeeded event")
	}
	if childStepCount != 2 {
		t.Fatalf("child step_started events with branch = %d, want 2", childStepCount)
	}
}

func TestFanOutPersistsSnapshotWithJoinRecords(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-persist"},
		Store:      store,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: echoHandler()}}},
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
		Input: json.RawMessage(`{"x":1}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	snapshots, err := store.WorkflowSnapshots().ListWorkflowSnapshots(context.Background(), result.ID, PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots.Records) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots.Records))
	}
	var envelope workflowSnapshotEnvelope
	if err := json.Unmarshal(snapshots.Records[0].State, &envelope); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if envelope.StepID != "fanout" {
		t.Fatalf("snapshot StepID = %q, want fanout", envelope.StepID)
	}
	if len(envelope.FanOut) != 1 {
		t.Fatalf("snapshot FanOut = %d, want 1", len(envelope.FanOut))
	}
	if len(envelope.FanOut[0].Branches) != 2 {
		t.Fatalf("snapshot fan-out branches = %d, want 2", len(envelope.FanOut[0].Branches))
	}
	if snapshots.Records[0].SchemaVersion != workflowSnapshotSchemaVersion {
		t.Fatalf("schema version = %d, want %d", snapshots.Records[0].SchemaVersion, workflowSnapshotSchemaVersion)
	}
}

func TestFanOutRetryInChildStep(t *testing.T) {
	t.Parallel()

	var attempts int32
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-retry"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{
								Name: "a",
								Steps: []Step{{
									Definition: StepDefinition{
										ID:    "fa",
										Retry: &RetryPolicy{Attempts: 3, Delay: 0},
									},
									Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
										n := atomic.AddInt32(&attempts, 1)
										if n < 2 {
											return nil, errors.New("transient")
										}
										return json.RawMessage(`{"ok":true}`), nil
									}),
								}},
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
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestFanOutRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	echo := echoHandler()
	simpleFanOut := func() *FanOut {
		return &FanOut{
			Branches: []FanOutBranch{{
				Name:  "a",
				Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echo}},
			}},
		}
	}

	tests := []struct {
		name string
		cfg  LinearWorkflowConfig
		want string
	}{
		{
			name: "fan-out step with handler",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{ID: "fo", FanOut: simpleFanOut()},
					Handler:    echo,
				}},
			},
			want: "must not declare a Handler",
		},
		{
			name: "fan-out step with output schema",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:           "fo",
						OutputSchema: json.RawMessage(`{"type":"object"}`),
						FanOut:       simpleFanOut(),
					},
				}},
			},
			want: "must not declare OutputSchema",
		},
		{
			name: "fan-out step with suspend schema",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:            "fo",
						SuspendSchema: json.RawMessage(`{"type":"object"}`),
						FanOut:        simpleFanOut(),
					},
				}},
			},
			want: "must not declare SuspendSchema",
		},
		{
			name: "fan-out step with retry",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:     "fo",
						Retry:  &RetryPolicy{Attempts: 2},
						FanOut: simpleFanOut(),
					},
				}},
			},
			want: "must not declare Retry",
		},
		{
			name: "fan-out with no branches",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:     "fo",
						FanOut: &FanOut{Branches: []FanOutBranch{}},
					},
				}},
			},
			want: "has no branches",
		},
		{
			name: "fan-out with empty branch name",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "fo",
						FanOut: &FanOut{Branches: []FanOutBranch{{
							Name:  "",
							Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echo}},
						}}},
					},
				}},
			},
			want: "empty Name",
		},
		{
			name: "fan-out with duplicate branch name",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "fo",
						FanOut: &FanOut{Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echo}}},
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa2"}, Handler: echo}}},
						}},
					},
				}},
			},
			want: "duplicate branch name",
		},
		{
			name: "fan-out branch with no steps",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID:     "fo",
						FanOut: &FanOut{Branches: []FanOutBranch{{Name: "a"}}},
					},
				}},
			},
			want: "has no steps",
		},
		{
			name: "fan-out with invalid failure policy",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "fo",
						FanOut: &FanOut{
							FailurePolicy: "bogus",
							Branches:      []FanOutBranch{{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echo}}}},
						},
					},
				}},
			},
			want: "invalid FailurePolicy",
		},
		{
			name: "fan-out with negative max parallel",
			cfg: LinearWorkflowConfig{
				Definition: WorkflowDefinition{ID: "wf"},
				Steps: []Step{{
					Definition: StepDefinition{
						ID: "fo",
						FanOut: &FanOut{
							MaxParallel: -1,
							Branches:    []FanOutBranch{{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echo}}}},
						},
					},
				}},
			},
			want: "MaxParallel",
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

func TestFanOutInputMapperErrorFailsBranch(t *testing.T) {
	t.Parallel()

	mapperErr := errors.New("mapper failed")
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-mapper-err"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{
								Name: "a",
								InputMapper: func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
									return nil, mapperErr
								},
								Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}},
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
	if !errors.Is(err, ErrWorkflowFanOutInputMapperFailed) {
		t.Fatalf("error = %v, want ErrWorkflowFanOutInputMapperFailed", err)
	}
}

func TestFanOutMultiStepBranch(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-multistep"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{
								Name: "pipeline",
								Steps: []Step{
									{Definition: StepDefinition{ID: "p1"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
										return json.RawMessage(`{"step":1}`), nil
									})},
									{Definition: StepDefinition{ID: "p2"}, Handler: StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
										var m map[string]int
										_ = json.Unmarshal(input, &m)
										m["step"] = 2
										return json.Marshal(m)
									})},
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
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var joined []struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(result.Output, &joined); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(joined) != 1 {
		t.Fatalf("joined length = %d, want 1", len(joined))
	}
	var out map[string]int
	if err := json.Unmarshal(joined[0].Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out["step"] != 2 {
		t.Fatalf("step = %d, want 2 (second step ran)", out["step"])
	}
}

func TestFanOutBoundedConcurrency(t *testing.T) {
	t.Parallel()

	var active int32
	var maxActive int32
	var mu sync.Mutex

	delayHandler := StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
		cur := atomic.AddInt32(&active, 1)
		mu.Lock()
		if cur > maxActive {
			maxActive = cur
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return append(json.RawMessage(nil), input...), nil
	})

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-bounded"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						MaxParallel: 2,
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: delayHandler}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: delayHandler}}},
							{Name: "c", Steps: []Step{{Definition: StepDefinition{ID: "fc"}, Handler: delayHandler}}},
							{Name: "d", Steps: []Step{{Definition: StepDefinition{ID: "fd"}, Handler: delayHandler}}},
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
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if maxActive > 2 {
		t.Fatalf("maxActive = %d, want <= 2 (MaxParallel bound)", maxActive)
	}
}

func TestFanOutDefaultFailFastPolicy(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-default-policy"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}}},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	compiled := wf.steps[0]
	if compiled.fanOut.failurePolicy != FanOutFailFast {
		t.Fatalf("default failurePolicy = %q, want fail_fast", compiled.fanOut.failurePolicy)
	}
	if compiled.fanOut.maxParallel != 1 {
		t.Fatalf("default maxParallel = %d, want 1", compiled.fanOut.maxParallel)
	}
}

func TestFanOutCollectAllLowestIndexFailure(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-collectall-lowest"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						MaxParallel:   2,
						FailurePolicy: FanOutCollectAll,
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								time.Sleep(20 * time.Millisecond)
								return nil, errors.New("a failed")
							})}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return nil, errors.New("b failed")
							})}}},
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
	if err == nil {
		t.Fatal("expected error")
	}
	var wfErr *WorkflowError
	if !errors.As(err, &wfErr) {
		t.Fatalf("error = %v, want *WorkflowError", err)
	}
	if !strings.Contains(wfErr.Err.Error(), "fa") {
		t.Fatalf("error should reference branch a's step fa (lowest declared index), got: %v", wfErr.Err)
	}
	if len(result.FanOut) != 1 {
		t.Fatalf("FanOut results = %d, want 1", len(result.FanOut))
	}
	join := result.FanOut[0]
	if join.Status != RunStatusFailed {
		t.Fatalf("join status = %q, want failed", join.Status)
	}
}

func TestFanOutSuspendNotSupportedInChild(t *testing.T) {
	t.Parallel()

	compiler := stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}}
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition:     WorkflowDefinition{ID: "fanout-suspend-child"},
		SchemaCompiler: compiler,
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{
								Definition: StepDefinition{ID: "fa", SuspendSchema: json.RawMessage(`{"type":"object"}`)},
								Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
									return nil, &SuspendError{Signal: SuspendSignal{
										StepID:   "fa",
										Contract: json.RawMessage(`{"ok":true}`),
									}}
								}),
							}}},
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
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrWorkflowFanOutBranchFailed) {
		t.Fatalf("error = %v, want ErrWorkflowFanOutBranchFailed", err)
	}
}

func TestFanOutWithConditionalBranchInsideChild(t *testing.T) {
	t.Parallel()

	cond := func(_ context.Context, input json.RawMessage) (bool, error) {
		var v struct {
			Route string `json:"route"`
		}
		_ = json.Unmarshal(input, &v)
		return v.Route == "x", nil
	}
	echo := echoHandler()

	branchStep := Step{
		Definition: StepDefinition{
			ID: "router",
			Branches: []Branch{
				{Name: "x", Condition: cond, Steps: []Step{{Definition: StepDefinition{ID: "fx"}, Handler: echo}}},
				{Name: "y", Condition: func(_ context.Context, _ json.RawMessage) (bool, error) { return true, nil }, Steps: []Step{{Definition: StepDefinition{ID: "fy"}, Handler: echo}}},
			},
			Default: "y",
		},
	}

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-conditional-child"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{{
							Name:  "a",
							Steps: []Step{branchStep},
						}},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := wf.Run(context.Background(), WorkflowRunInput{
		Input: json.RawMessage(`{"route":"x"}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
}

func TestFanOutPanicInChildStep(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-panic"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								panic("boom")
							})}}},
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
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrWorkflowFanOutBranchFailed) {
		t.Fatalf("error = %v, want ErrWorkflowFanOutBranchFailed", err)
	}
}

func TestFanOutJoinedOutputFormat(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-format"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"value":1}`), nil
							})}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return json.RawMessage(`{"value":2}`), nil
							})}}},
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

	expected := `[{"name":"a","output":{"value":1}},{"name":"b","output":{"value":2}}]`
	if string(result.Output) != expected {
		t.Fatalf("output = %s, want %s", result.Output, expected)
	}
}

func TestFanOutRetryOverrideInChildStep(t *testing.T) {
	t.Parallel()

	var attempts int32
	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-retry-override"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{
								Name: "a",
								Steps: []Step{{
									Definition: StepDefinition{ID: "fa", Retry: &RetryPolicy{Attempts: 1}},
									Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
										n := atomic.AddInt32(&attempts, 1)
										if n < 2 {
											return nil, errors.New("transient")
										}
										return json.RawMessage(`{}`), nil
									}),
								}},
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
		RetryOverrides: map[StepID]RetryPolicy{
			"fa": {Attempts: 3, Delay: 0},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != RunStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Status)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (override enabled retry)", attempts)
	}
}

func TestFanOutStepPositionResolvesChildStepForRetryOverride(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-position"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: echoHandler()}}},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	pos := wf.stepPosition("fa")
	if pos != 0 {
		t.Fatalf("stepPosition(fa) = %d, want 0 (not a top-level step)", pos)
	}
}

func TestFanOutCollectAllMultipleFailures(t *testing.T) {
	t.Parallel()

	wf, err := NewLinearWorkflow(LinearWorkflowConfig{
		Definition: WorkflowDefinition{ID: "fanout-multi-fail"},
		Steps: []Step{
			{
				Definition: StepDefinition{
					ID: "fanout",
					FanOut: &FanOut{
						FailurePolicy: FanOutCollectAll,
						Branches: []FanOutBranch{
							{Name: "a", Steps: []Step{{Definition: StepDefinition{ID: "fa"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return nil, fmt.Errorf("a failed")
							})}}},
							{Name: "b", Steps: []Step{{Definition: StepDefinition{ID: "fb"}, Handler: StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
								return nil, fmt.Errorf("b failed")
							})}}},
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
	if err == nil {
		t.Fatal("expected error")
	}
	if len(result.FanOut) != 1 {
		t.Fatalf("FanOut results = %d, want 1", len(result.FanOut))
	}
	join := result.FanOut[0]
	if join.Status != RunStatusFailed {
		t.Fatalf("join status = %q, want failed", join.Status)
	}
	failedCount := 0
	for _, br := range join.Branches {
		if br.Status == RunStatusFailed && br.Failure != nil {
			failedCount++
		}
	}
	if failedCount != 2 {
		t.Fatalf("failed branches = %d, want 2", failedCount)
	}
}
