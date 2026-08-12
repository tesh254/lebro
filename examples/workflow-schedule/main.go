// The workflow-schedule example persists a recurring schedule to a file-backed
// SQLite store, then constructs a fresh Scheduler over the reopened store — a
// process restart — and ticks it so the overdue schedule fires. It reuses the
// linear workflow run machinery for the execution and prints the resulting
// schedule execution history. A fixed clock keeps the run deterministic, so no
// network, API key, or wall-clock timing is required.
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
	must(run(os.Stdout))
}

func run(output io.Writer) error {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "lebro-workflow-schedule-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dsn := filepath.Join(dir, "schedule.db")

	// A fixed reference time makes the example deterministic. The schedule's
	// next fire is set one hour in the past so the first tick is due.
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	firstFire := now.Add(-time.Hour)

	store, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return err
	}

	// Persist the schedule definition. This is all the state a restart needs.
	if err := store.Schedules().SaveSchedule(ctx, lebro.ScheduleRecord{
		ID:          "hourly-report",
		WorkflowID:  "report",
		Spec:        "0 * * * *",
		Concurrency: lebro.ConcurrencySkip,
		Input:       json.RawMessage(`{"kind":"hourly"}`),
		NextFireAt:  &firstFire,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		_ = store.Close()
		return err
	}
	// Close the store to simulate the process that created the schedule exiting.
	if err := store.Close(); err != nil {
		return err
	}

	// Restart: a brand-new store and scheduler over the same database file.
	reopened, err := lebro.NewSQLiteStore(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		return err
	}

	wf, err := lebro.NewLinearWorkflow(lebro.LinearWorkflowConfig{
		Definition: lebro.WorkflowDefinition{ID: "report", Name: "Hourly Report"},
		Steps: []lebro.Step{{
			Definition: lebro.StepDefinition{ID: "build"},
			Handler: lebro.StepHandlerFunc(func(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
				return json.Marshal(map[string]string{"report": string(input)})
			}),
		}},
		Store: reopened,
	})
	if err != nil {
		return err
	}

	scheduler, err := lebro.NewScheduler(lebro.SchedulerConfig{
		Store:    reopened,
		Resolver: lebro.WorkflowMap{"report": wf},
		Clock:    lebro.NewFixedClock(now),
	})
	if err != nil {
		return err
	}

	result, err := scheduler.Tick(ctx, now)
	if err != nil {
		return err
	}
	writef(output, "fired: %d skipped: %d missed: %d\n", result.Fired, result.Skipped, result.Missed)

	schedule, err := reopened.Schedules().GetSchedule(ctx, "hourly-report")
	if err != nil {
		return err
	}
	writef(output, "next fire after tick: %s\n", schedule.NextFireAt.Format(time.RFC3339))

	history, err := reopened.ScheduleExecutions().ListScheduleExecutions(ctx, "hourly-report", lebro.PageRequest{})
	if err != nil {
		return err
	}
	writef(output, "history entries: %d\n", len(history.Records))
	for _, entry := range history.Records {
		writef(output, "  %s scheduled_for=%s\n", entry.Status, entry.ScheduledFor.Format(time.RFC3339))
	}
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
