package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// scheduleStores returns the durable stores to exercise the schedule
// repository contract against. SQLite runs against a shared in-memory database
// so the test stays hermetic while still traversing the SQL adapter.
func scheduleStores(t *testing.T) map[string]Store {
	t.Helper()
	stores := map[string]Store{"memory": NewMemoryStore()}
	sqliteStore, err := NewSQLiteStore("file:schedules_" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	if err := sqliteStore.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = sqliteStore.Close() })
	stores["sqlite"] = sqliteStore
	return stores
}

func TestScheduleRepositoryContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	next := now.Add(time.Hour)

	for name, store := range scheduleStores(t) {
		t.Run(name, func(t *testing.T) {
			schedule := ScheduleRecord{
				ID:          "sched-1",
				WorkflowID:  "wf-1",
				Spec:        "0 * * * *",
				Concurrency: ConcurrencySkip,
				Input:       json.RawMessage(`{"k":"v"}`),
				Metadata:    json.RawMessage(`{"tenant":"acme"}`),
				WakeRunID:   "run-1",
				WakeToken:   "token-1",
				NextFireAt:  &next,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := store.Schedules().SaveSchedule(ctx, schedule); err != nil {
				t.Fatalf("SaveSchedule: %v", err)
			}
			got, err := store.Schedules().GetSchedule(ctx, "sched-1")
			if err != nil {
				t.Fatalf("GetSchedule: %v", err)
			}
			if got.Spec != schedule.Spec || got.Concurrency != ConcurrencySkip || got.WakeRunID != schedule.WakeRunID || got.WakeToken != schedule.WakeToken {
				t.Fatalf("GetSchedule mismatch: %+v", got)
			}
			if got.NextFireAt == nil || !got.NextFireAt.Equal(next) {
				t.Fatalf("NextFireAt round-trip: got %v want %v", got.NextFireAt, next)
			}
			if string(got.Input) != `{"k":"v"}` {
				t.Fatalf("Input round-trip: %s", got.Input)
			}

			// Update (upsert): pause and clear the next fire.
			schedule.Paused = true
			schedule.NextFireAt = nil
			schedule.UpdatedAt = now.Add(time.Minute)
			if err := store.Schedules().SaveSchedule(ctx, schedule); err != nil {
				t.Fatalf("SaveSchedule update: %v", err)
			}
			got, err = store.Schedules().GetSchedule(ctx, "sched-1")
			if err != nil {
				t.Fatalf("GetSchedule after update: %v", err)
			}
			if !got.Paused || got.NextFireAt != nil {
				t.Fatalf("update not applied: %+v", got)
			}

			// DueBy filter: a paused schedule is never due.
			dueBy := next.Add(time.Hour)
			page, err := store.Schedules().ListSchedules(ctx, ScheduleFilter{DueBy: &dueBy}, PageRequest{})
			if err != nil {
				t.Fatalf("ListSchedules due: %v", err)
			}
			if len(page.Records) != 0 {
				t.Fatalf("paused schedule returned as due: %+v", page.Records)
			}

			// Unpause with a due next fire and confirm it lists as due.
			schedule.Paused = false
			schedule.NextFireAt = &now
			if err := store.Schedules().SaveSchedule(ctx, schedule); err != nil {
				t.Fatalf("SaveSchedule unpause: %v", err)
			}
			page, err = store.Schedules().ListSchedules(ctx, ScheduleFilter{DueBy: &dueBy}, PageRequest{})
			if err != nil {
				t.Fatalf("ListSchedules due after unpause: %v", err)
			}
			if len(page.Records) != 1 {
				t.Fatalf("due schedule not returned: %+v", page.Records)
			}

			if err := store.Schedules().DeleteSchedule(ctx, "sched-1"); err != nil {
				t.Fatalf("DeleteSchedule: %v", err)
			}
			if _, err := store.Schedules().GetSchedule(ctx, "sched-1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetSchedule after delete: got %v want ErrNotFound", err)
			}
			if err := store.Schedules().DeleteSchedule(ctx, "sched-1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteSchedule missing: got %v want ErrNotFound", err)
			}
		})
	}
}

func TestScheduleExecutionRepositoryContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	for name, store := range scheduleStores(t) {
		t.Run(name, func(t *testing.T) {
			// An execution for a missing schedule is rejected.
			orphan := ScheduleExecutionRecord{ID: "e-0", ScheduleID: "missing", Status: ScheduleExecMissed, ScheduledFor: now, StartedAt: now}
			if err := store.ScheduleExecutions().SaveScheduleExecution(ctx, orphan); !errors.Is(err, ErrNotFound) {
				t.Fatalf("orphan execution: got %v want ErrNotFound", err)
			}

			schedule := ScheduleRecord{ID: "sched-1", WorkflowID: "wf-1", Spec: "0 * * * *", NextFireAt: &now, CreatedAt: now, UpdatedAt: now}
			if err := store.Schedules().SaveSchedule(ctx, schedule); err != nil {
				t.Fatalf("SaveSchedule: %v", err)
			}
			finished := now.Add(time.Second)
			execs := []ScheduleExecutionRecord{
				{ID: "e-1", ScheduleID: "sched-1", RunID: "run-1", Status: ScheduleExecSucceeded, ScheduledFor: now, StartedAt: now, FinishedAt: &finished},
				{ID: "e-2", ScheduleID: "sched-1", Status: ScheduleExecFailed, ScheduledFor: now.Add(time.Hour), StartedAt: now.Add(time.Hour), FinishedAt: &finished, Error: "boom"},
			}
			for _, e := range execs {
				if err := store.ScheduleExecutions().SaveScheduleExecution(ctx, e); err != nil {
					t.Fatalf("SaveScheduleExecution %s: %v", e.ID, err)
				}
			}
			// Duplicate ID within the schedule is rejected.
			if err := store.ScheduleExecutions().SaveScheduleExecution(ctx, execs[0]); err == nil {
				t.Fatal("duplicate execution accepted")
			}

			page, err := store.ScheduleExecutions().ListScheduleExecutions(ctx, "sched-1", PageRequest{})
			if err != nil {
				t.Fatalf("ListScheduleExecutions: %v", err)
			}
			if len(page.Records) != 2 {
				t.Fatalf("execution count = %d, want 2", len(page.Records))
			}
			// Insertion order preserved.
			if page.Records[0].ID != "e-1" || page.Records[1].ID != "e-2" {
				t.Fatalf("execution order: %s, %s", page.Records[0].ID, page.Records[1].ID)
			}
			if page.Records[1].Status != ScheduleExecFailed || page.Records[1].Error != "boom" {
				t.Fatalf("failed execution round-trip: %+v", page.Records[1])
			}
			if page.Records[0].RunID != "run-1" {
				t.Fatalf("run ID round-trip: %q", page.Records[0].RunID)
			}
		})
	}
}
