// morning-digest is the daily-brief build: a persisted cron schedule fans a
// competitive-intel job out across three sources in parallel every morning and
// joins the branch outputs into one brief. The schedule definition lives in the
// store, so the example closes the database, reopens it as if a new process had
// started, and ticks a fresh scheduler — the overdue 06:00 fire happens with no
// warm process anywhere.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tesh254/lebro"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "lebro-morning-digest-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dsn := filepath.Join(dir, "digest.db")

	// Fixed reference time so the example is deterministic: the next 06:00
	// fire was set an hour in the past, as if the digest process died overnight.
	now := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	due := now.Add(-time.Hour)

	store, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return err
	}

	// Persist everything a restart needs: the workflow's cron entry.
	if err := store.Schedules().SaveSchedule(ctx, lebro.ScheduleRecord{
		ID:          "morning-digest",
		WorkflowID:  "competitor-digest",
		Spec:        "0 6 * * *",
		Concurrency: lebro.ConcurrencySkip,
		Input:       json.RawMessage(`{"date":"2026-08-12"}`),
		NextFireAt:  &due,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		_ = store.Close()
		return err
	}
	writef(output, "schedule persisted at %s; process exits\n", now.Format(time.RFC3339))
	if err := store.Close(); err != nil {
		return err
	}

	// Restart: a fresh process opens the same database and registers its
	// workflows; the scheduler reloads due schedules from the store.
	reopened, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		return err
	}

	digest, err := newDigestWorkflow(reopened)
	if err != nil {
		return err
	}
	scheduler, err := lebro.NewScheduler(lebro.SchedulerConfig{
		Store:    reopened,
		Resolver: lebro.WorkflowMap{"competitor-digest": digest},
		Clock:    lebro.NewFixedClock(now),
	})
	if err != nil {
		return err
	}

	tick, err := scheduler.Tick(ctx, now)
	if err != nil {
		return err
	}
	writef(output, "tick fired %d schedule(s)\n", tick.Fired)
	if tick.Fired != 1 || len(tick.Executions) == 0 {
		return fmt.Errorf("expected exactly one fired execution, got %d", tick.Fired)
	}

	runID := tick.Executions[len(tick.Executions)-1].RunID
	record, err := reopened.WorkflowRuns().GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	writef(output, "digest run %s: %s\n", record.ID, record.Status)

	var brief struct {
		Brief string `json:"brief"`
	}
	if err := json.Unmarshal(record.Output, &brief); err != nil {
		return err
	}
	writef(output, "%s\n", brief.Brief)

	schedule, err := reopened.Schedules().GetSchedule(ctx, "morning-digest")
	if err != nil {
		return err
	}
	writef(output, "next fire: %s\n", schedule.NextFireAt.Format(time.RFC3339))
	return nil
}

func writef(output io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		panic(err)
	}
}

// newDigestWorkflow builds the fan-out-and-join brief. Three independent
// source branches run concurrently under MaxParallel and join in declaration
// order; the join step renders one human-readable brief.
func newDigestWorkflow(store lebro.Store) (*lebro.LinearWorkflow, error) {
	sourceStep := func(id, source string, items []string) lebro.Step {
		return lebro.Step{
			Definition: lebro.StepDefinition{ID: lebro.StepID(id)},
			Handler: lebro.StepHandlerFunc(func(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return json.Marshal(map[string]any{"source": source, "items": items})
			}),
		}
	}
	return lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "competitor-digest", Name: "Competitor Digest", Version: "v1"},
		Store:      store,
		Steps: []lebro.Step{
			{
				Definition: lebro.StepDefinition{
					ID: "collect",
					FanOut: &lebro.FanOut{
						MaxParallel:   3,
						FailurePolicy: lebro.FanOutCollectAll,
						Branches: []lebro.FanOutBranch{
							{
								Name: "news",
								Steps: []lebro.Step{sourceStep("fetch-news", "industry news", []string{
									"Rival shipped on-prem deployment option",
									"Rival raised Series C",
								})},
							},
							{
								Name: "changelog",
								Steps: []lebro.Step{sourceStep("fetch-changelog", "product changelog", []string{
									"Rival added SSO on mid-tier plans",
									"Rival deprecated legacy webhook format",
								})},
							},
							{
								Name: "pricing",
								Steps: []lebro.Step{sourceStep("fetch-pricing", "pricing page", []string{
									"Rival mid-tier price moved from $49 to $59 per seat",
								})},
							},
						},
					},
				},
			},
			{
				Definition: lebro.StepDefinition{ID: "join-brief"},
				Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
					var branches []struct {
						Name   string `json:"name"`
						Output struct {
							Source string   `json:"source"`
							Items  []string `json:"items"`
						} `json:"output"`
					}
					if err := json.Unmarshal(input, &branches); err != nil {
						return nil, err
					}
					brief := "MORNING COMPETITOR BRIEF\n"
					for _, branch := range branches {
						brief += fmt.Sprintf("\n%s (%s)\n", branch.Name, branch.Output.Source)
						for _, item := range branch.Output.Items {
							brief += fmt.Sprintf("  - %s\n", item)
						}
					}
					return json.Marshal(map[string]string{"brief": brief})
				}),
			},
		},
	})
}
