package testkit

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tesh254/lebro/internal/runtime"
)

// RuntimeStoreFactory builds the capability-based adapter under contract
// scrutiny. It must return a store ready for records (adapters with setup
// steps should perform them in the factory).
type RuntimeStoreFactory func(*testing.T) runtime.RuntimeStore

// RuntimeStoreContractSuite runs the capability-based contract every
// RuntimeStore implementation must satisfy: the capability advertisement must
// match the implemented interfaces exactly, every advertised capability must
// support the reads and writes its feature performs (pagination, defensive
// copies, idempotent appends, scope isolation), writes must honor context
// cancellation, and transactional adapters must commit or discard coupled
// writes atomically. Capabilities the adapter does not advertise are skipped,
// not failed: partial adapters are the point of the contract.
func RuntimeStoreContractSuite(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	t.Run("capability advertisement matches interfaces", func(t *testing.T) {
		runtimeStoreCapabilityAdvertisement(t, newRuntimeStore)
	})
	t.Run("transcript round-trip", func(t *testing.T) {
		runtimeStoreTranscriptRoundTrip(t, newRuntimeStore)
	})
	t.Run("working memory round-trip", func(t *testing.T) {
		runtimeStoreWorkingMemoryRoundTrip(t, newRuntimeStore)
	})
	t.Run("workflow state round-trip", func(t *testing.T) {
		runtimeStoreWorkflowStateRoundTrip(t, newRuntimeStore)
	})
	t.Run("schedules round-trip", func(t *testing.T) {
		runtimeStoreSchedulesRoundTrip(t, newRuntimeStore)
	})
	t.Run("observability round-trip", func(t *testing.T) {
		runtimeStoreObservabilityRoundTrip(t, newRuntimeStore)
	})
	t.Run("honors canceled context", func(t *testing.T) {
		runtimeStoreCanceledContext(t, newRuntimeStore)
	})
	t.Run("transaction commit and rollback", func(t *testing.T) {
		runtimeStoreTransactionSemantics(t, newRuntimeStore)
	})
}

// runtimeStoreCaps mirrors validateRuntimeStore through the exported API: the
// advertised set and the implemented capability interfaces must agree in both
// directions.
func runtimeStoreCaps(t *testing.T, rs runtime.RuntimeStore) runtime.StoreCapabilities {
	t.Helper()
	caps := rs.Capabilities()
	if caps == (runtime.StoreCapabilities{}) {
		t.Fatal("Capabilities() advertises no capabilities")
	}
	type check struct {
		capability  runtime.StoreCapability
		implemented bool
	}
	s, transcriptOK := rs.(runtime.TranscriptStore)
	w, workingMemoryOK := rs.(runtime.WorkingMemoryStore)
	wf, workflowStateOK := rs.(runtime.WorkflowStateStore)
	sc, schedulesOK := rs.(runtime.ScheduleStore)
	ob, observabilityOK := rs.(runtime.ObservabilityStore)
	_, transactionsOK := rs.(runtime.TransactionalStore)
	for _, c := range []check{
		{runtime.StoreCapabilityTranscript, transcriptOK && !nilInterface(s.Threads()) && !nilInterface(s.Messages())},
		{runtime.StoreCapabilityWorkingMemory, workingMemoryOK && !nilInterface(w.WorkingMemory())},
		{runtime.StoreCapabilityWorkflowState, workflowStateOK && !nilInterface(wf.WorkflowRuns()) && !nilInterface(wf.WorkflowSnapshots())},
		{runtime.StoreCapabilitySchedules, schedulesOK && !nilInterface(sc.Schedules()) && !nilInterface(sc.ScheduleExecutions())},
		{runtime.StoreCapabilityObservability, observabilityOK && !nilInterface(ob.RunEvents()) && !nilInterface(ob.ModelAttempts()) && !nilInterface(ob.ToolExecutions())},
		{runtime.StoreCapabilityTransactions, transactionsOK},
	} {
		if caps.Has(c.capability) != c.implemented {
			t.Fatalf("capability %q advertised=%v but implemented=%v", c.capability, caps.Has(c.capability), c.implemented)
		}
	}
	return caps
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func runtimeStoreCapabilityAdvertisement(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	runtimeStoreCaps(t, rs)
}

func runtimeStoreTranscriptRoundTrip(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	if !rs.Capabilities().Has(runtime.StoreCapabilityTranscript) {
		t.Skip("transcript capability not advertised")
	}
	transcript := rs.(runtime.TranscriptStore)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	thread := runtime.ThreadRecord{ID: "cap-thread-1", Namespace: "ns", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}
	if err := transcript.Threads().CreateThread(ctx, thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	got, err := transcript.Threads().GetThread(ctx, "cap-thread-1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.Namespace != "ns" || got.OwnerID != "owner-1" {
		t.Fatalf("GetThread scope = %q/%q, want ns/owner-1", got.Namespace, got.OwnerID)
	}
	thread.Metadata = []byte(`{"plan":"pro"}`)
	thread.UpdatedAt = now.Add(time.Second)
	if err := transcript.Threads().UpdateThread(ctx, thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	messages := []runtime.MessageRecord{
		{ID: "cap-msg-1", ThreadID: "cap-thread-1", Message: runtime.Message{Role: runtime.RoleUser, Content: "first"}, CreatedAt: now},
		{ID: "cap-msg-2", ThreadID: "cap-thread-1", Message: runtime.Message{Role: runtime.RoleAssistant, Content: "second"}, CreatedAt: now.Add(time.Millisecond)},
		{ID: "cap-msg-3", ThreadID: "cap-thread-1", Message: runtime.Message{Role: runtime.RoleUser, Content: "third"}, CreatedAt: now.Add(2 * time.Millisecond)},
	}
	if err := transcript.Messages().AppendMessages(ctx, messages); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	page, err := transcript.Messages().ListMessages(ctx, "cap-thread-1", runtime.PageRequest{Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(page.Records) != 2 || page.Records[0].ID != "cap-msg-1" || page.Records[1].ID != "cap-msg-2" || page.NextCursor == "" {
		t.Fatalf("first page = %+v, want msgs 1-2 with a next cursor", page)
	}
	page, err = transcript.Messages().ListMessages(ctx, "cap-thread-1", runtime.PageRequest{Cursor: page.NextCursor, Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages page 2: %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].ID != "cap-msg-3" || page.NextCursor != "" {
		t.Fatalf("second page = %+v, want msg 3 with no next cursor", page)
	}
	if _, err := transcript.Messages().ListMessages(ctx, "cap-thread-1", runtime.PageRequest{Cursor: "not-a-cursor"}); !errors.Is(err, runtime.ErrInvalidPage) {
		t.Fatalf("invalid cursor error = %v, want ErrInvalidPage", err)
	}

	messages[1].Message.Content = "second (edited)"
	if err := transcript.Messages().UpdateMessages(ctx, messages[1:2]); err != nil {
		t.Fatalf("UpdateMessages: %v", err)
	}
	page, err = transcript.Messages().ListMessages(ctx, "cap-thread-1", runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListMessages after update: %v", err)
	}
	if page.Records[1].Message.Content != "second (edited)" {
		t.Fatalf("updated content = %q", page.Records[1].Message.Content)
	}
	// Defensive copy: mutating a returned record must not corrupt stored state.
	page.Records[1].Message.Content = "mutated by caller"
	page, err = transcript.Messages().ListMessages(ctx, "cap-thread-1", runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListMessages after caller mutation: %v", err)
	}
	if page.Records[1].Message.Content != "second (edited)" {
		t.Fatalf("stored content = %q after caller mutated a returned record", page.Records[1].Message.Content)
	}

	if err := transcript.Messages().DeleteMessages(ctx, "cap-thread-1", []string{"cap-msg-1"}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	page, err = transcript.Messages().ListMessages(ctx, "cap-thread-1", runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListMessages after delete: %v", err)
	}
	if len(page.Records) != 2 || page.Records[0].ID != "cap-msg-2" {
		t.Fatalf("records after delete = %v", page.Records)
	}
}

func runtimeStoreWorkingMemoryRoundTrip(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	if !rs.Capabilities().Has(runtime.StoreCapabilityWorkingMemory) {
		t.Skip("working memory capability not advertised")
	}
	memory := rs.(runtime.WorkingMemoryStore)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	scope := runtime.WorkingMemoryScope{Namespace: "ns", OwnerID: "owner-1"}

	fact, err := memory.WorkingMemory().UpsertWorkingMemoryFact(ctx, runtime.WorkingMemoryFact{
		ID: "fact-1", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "preference", Value: []byte(`"dark"`), Version: 0, CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatalf("UpsertWorkingMemoryFact create: %v", err)
	}
	if fact.Version != 1 {
		t.Fatalf("created version = %d, want 1", fact.Version)
	}
	if _, err := memory.WorkingMemory().UpsertWorkingMemoryFact(ctx, runtime.WorkingMemoryFact{
		ID: "fact-1", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "preference", Value: []byte(`"light"`), Version: fact.Version, CreatedAt: now, UpdatedAt: now,
	}, fact.Version+1); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("stale-version upsert error = %v, want ErrConflict", err)
	}
	updated, err := memory.WorkingMemory().UpsertWorkingMemoryFact(ctx, runtime.WorkingMemoryFact{
		ID: "fact-1", Namespace: scope.Namespace, OwnerID: scope.OwnerID, Key: "preference", Value: []byte(`"light"`), Version: fact.Version, CreatedAt: now, UpdatedAt: now,
	}, fact.Version)
	if err != nil {
		t.Fatalf("UpsertWorkingMemoryFact update: %v", err)
	}
	if updated.Version != fact.Version+1 || string(updated.Value) != `"light"` {
		t.Fatalf("updated fact = %+v", updated)
	}
	// Scope isolation: another owner must not read or overwrite the fact.
	other := runtime.WorkingMemoryScope{Namespace: "ns", OwnerID: "owner-2"}
	if _, err := memory.WorkingMemory().GetWorkingMemoryFact(ctx, other, "preference"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("cross-owner read error = %v, want ErrNotFound", err)
	}
	page, err := memory.WorkingMemory().ListWorkingMemoryFacts(ctx, scope, runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListWorkingMemoryFacts: %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].Key != "preference" {
		t.Fatalf("facts = %+v", page.Records)
	}
	if err := memory.WorkingMemory().DeleteWorkingMemoryFact(ctx, scope, "preference", updated.Version); err != nil {
		t.Fatalf("DeleteWorkingMemoryFact: %v", err)
	}
	if err := memory.WorkingMemory().ClearWorkingMemory(ctx, scope); err != nil {
		t.Fatalf("ClearWorkingMemory: %v", err)
	}
}

func runtimeStoreWorkflowStateRoundTrip(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	if !rs.Capabilities().Has(runtime.StoreCapabilityWorkflowState) {
		t.Skip("workflow state capability not advertised")
	}
	state := rs.(runtime.WorkflowStateStore)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	run := runtime.WorkflowRunRecord{
		ID: "cap-run-1", WorkflowID: "cap-workflow", Status: runtime.RunStatusRunning,
		Input: []byte(`{"n":1}`), StartedAt: now, UpdatedAt: now,
	}
	if err := state.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatalf("SaveWorkflowRun: %v", err)
	}
	got, err := state.WorkflowRuns().GetWorkflowRun(ctx, "cap-run-1")
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if got.WorkflowID != "cap-workflow" || got.Status != runtime.RunStatusRunning {
		t.Fatalf("GetWorkflowRun = %+v", got)
	}
	run.Status = runtime.RunStatusSucceeded
	run.Output = []byte(`{"ok":true}`)
	run.UpdatedAt = now.Add(time.Second)
	if err := state.WorkflowRuns().SaveWorkflowRun(ctx, run); err != nil {
		t.Fatalf("SaveWorkflowRun update: %v", err)
	}
	other := runtime.WorkflowRunRecord{ID: "cap-run-2", WorkflowID: "cap-other", Status: runtime.RunStatusFailed, StartedAt: now, UpdatedAt: now}
	if err := state.WorkflowRuns().SaveWorkflowRun(ctx, other); err != nil {
		t.Fatalf("SaveWorkflowRun second: %v", err)
	}
	page, err := state.WorkflowRuns().ListWorkflowRuns(ctx, runtime.WorkflowRunFilter{WorkflowID: "cap-workflow"}, runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].ID != "cap-run-1" || page.Records[0].Status != runtime.RunStatusSucceeded {
		t.Fatalf("filtered runs = %+v", page.Records)
	}

	snapshot := runtime.WorkflowSnapshotRecord{ID: "cap-snap-1", RunID: "cap-run-1", Sequence: 1, State: []byte(`{"step":2}`), CreatedAt: now}
	if err := state.WorkflowSnapshots().SaveWorkflowSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("SaveWorkflowSnapshot: %v", err)
	}
	snapshots, err := state.WorkflowSnapshots().ListWorkflowSnapshots(ctx, "cap-run-1", runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListWorkflowSnapshots: %v", err)
	}
	if len(snapshots.Records) != 1 || snapshots.Records[0].Sequence != 1 || string(snapshots.Records[0].State) != `{"step":2}` {
		t.Fatalf("snapshots = %+v", snapshots.Records)
	}
}

func runtimeStoreSchedulesRoundTrip(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	if !rs.Capabilities().Has(runtime.StoreCapabilitySchedules) {
		t.Skip("schedules capability not advertised")
	}
	schedules := rs.(runtime.ScheduleStore)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	next := now.Add(time.Hour)

	record := runtime.ScheduleRecord{ID: "cap-schedule-1", WorkflowID: "cap-workflow", Spec: "0 * * * *", NextFireAt: &next, CreatedAt: now, UpdatedAt: now}
	if err := schedules.Schedules().SaveSchedule(ctx, record); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	got, err := schedules.Schedules().GetSchedule(ctx, "cap-schedule-1")
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if got.Spec != "0 * * * *" || got.NextFireAt == nil || !got.NextFireAt.Equal(next) {
		t.Fatalf("GetSchedule = %+v", got)
	}
	page, err := schedules.Schedules().ListSchedules(ctx, runtime.ScheduleFilter{WorkflowID: "cap-workflow"}, runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].ID != "cap-schedule-1" {
		t.Fatalf("schedules = %+v", page.Records)
	}
	execution := runtime.ScheduleExecutionRecord{
		ID: "cap-exec-1", ScheduleID: "cap-schedule-1", Status: runtime.ScheduleExecSucceeded,
		ScheduledFor: now, StartedAt: now, FinishedAt: &now,
	}
	if err := schedules.ScheduleExecutions().SaveScheduleExecution(ctx, execution); err != nil {
		t.Fatalf("SaveScheduleExecution: %v", err)
	}
	if err := schedules.ScheduleExecutions().SaveScheduleExecution(ctx, execution); err == nil {
		t.Fatal("duplicate schedule execution was accepted")
	}
	executions, err := schedules.ScheduleExecutions().ListScheduleExecutions(ctx, "cap-schedule-1", runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListScheduleExecutions: %v", err)
	}
	if len(executions.Records) != 1 || executions.Records[0].ID != "cap-exec-1" {
		t.Fatalf("executions = %+v", executions.Records)
	}
	if err := schedules.Schedules().DeleteSchedule(ctx, "cap-schedule-1"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if _, err := schedules.Schedules().GetSchedule(ctx, "cap-schedule-1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("deleted schedule read = %v, want ErrNotFound", err)
	}
}

func runtimeStoreObservabilityRoundTrip(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	if !rs.Capabilities().Has(runtime.StoreCapabilityObservability) {
		t.Skip("observability capability not advertised")
	}
	observability := rs.(runtime.ObservabilityStore)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	event := runtime.RunEventRecord{ID: "cap-event-1", RunID: "cap-run-1", Sequence: 1, Type: runtime.RunEventStarted, Timestamp: now}
	if err := observability.RunEvents().AppendRunEvents(ctx, []runtime.RunEventRecord{event}); err != nil {
		t.Fatalf("AppendRunEvents: %v", err)
	}
	// Idempotent replay: the same (run, ID) pair must not duplicate.
	if err := observability.RunEvents().AppendRunEvents(ctx, []runtime.RunEventRecord{event}); err != nil {
		t.Fatalf("AppendRunEvents replay: %v", err)
	}
	events, err := observability.RunEvents().ListRunEvents(ctx, runtime.RunEventFilter{RunID: "cap-run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	if len(events.Records) != 1 || events.Records[0].ID != "cap-event-1" {
		t.Fatalf("events = %+v, want exactly one", events.Records)
	}

	attempt := runtime.ModelAttemptRecord{
		ID: "cap-attempt-1", RunID: "cap-run-1", Index: 1, Provider: "fixture", Model: "fixture-model",
		Status: runtime.ModelAttemptSuccess, Usage: runtime.ModelUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
		StartedAt: now, FinishedAt: now.Add(time.Second), ProducedMessageIDs: []string{"cap-msg-1"},
	}
	if err := observability.ModelAttempts().SaveModelAttempts(ctx, []runtime.ModelAttemptRecord{attempt}); err != nil {
		t.Fatalf("SaveModelAttempts: %v", err)
	}
	attempts, err := observability.ModelAttempts().ListModelAttempts(ctx, runtime.ModelAttemptFilter{RunID: "cap-run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListModelAttempts: %v", err)
	}
	if len(attempts.Records) != 1 || attempts.Records[0].Usage.OutputTokens != 5 {
		t.Fatalf("attempts = %+v", attempts.Records)
	}

	execution := runtime.ToolExecutionRecord{
		ID: "cap-tool-1", RunID: "cap-run-1", ToolCallID: "call-1", ToolID: "weather",
		State: runtime.ToolExecutionSucceeded, StartedAt: now, FinishedAt: now.Add(time.Millisecond),
	}
	if err := observability.ToolExecutions().SaveToolExecutions(ctx, []runtime.ToolExecutionRecord{execution}); err != nil {
		t.Fatalf("SaveToolExecutions: %v", err)
	}
	executions, err := observability.ToolExecutions().ListToolExecutions(ctx, runtime.ToolExecutionFilter{RunID: "cap-run-1"}, runtime.PageRequest{})
	if err != nil {
		t.Fatalf("ListToolExecutions: %v", err)
	}
	if len(executions.Records) != 1 || executions.Records[0].ToolID != "weather" {
		t.Fatalf("tool executions = %+v", executions.Records)
	}
}

func runtimeStoreCanceledContext(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	caps := rs.Capabilities()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if caps.Has(runtime.StoreCapabilityTranscript) {
		transcript := rs.(runtime.TranscriptStore)
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := transcript.Threads().CreateThread(ctx, runtime.ThreadRecord{ID: "cancelled", CreatedAt: now, UpdatedAt: now}); !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateThread with canceled context = %v, want context.Canceled", err)
		}
		if _, err := transcript.Messages().ListMessages(ctx, "cancelled", runtime.PageRequest{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("ListMessages with canceled context = %v, want context.Canceled", err)
		}
	}
	if caps.Has(runtime.StoreCapabilityWorkingMemory) {
		memory := rs.(runtime.WorkingMemoryStore).WorkingMemory()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if _, err := memory.UpsertWorkingMemoryFact(ctx, runtime.WorkingMemoryFact{ID: "f", Namespace: "ns", OwnerID: "o", Key: "k", Version: 0, CreatedAt: now, UpdatedAt: now}, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("UpsertWorkingMemoryFact with canceled context = %v, want context.Canceled", err)
		}
	}
	if caps.Has(runtime.StoreCapabilityObservability) {
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		observability := rs.(runtime.ObservabilityStore)
		if err := observability.RunEvents().AppendRunEvents(ctx, []runtime.RunEventRecord{{ID: "e", RunID: "r", Sequence: 1, Type: runtime.RunEventStarted, Timestamp: now}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("AppendRunEvents with canceled context = %v, want context.Canceled", err)
		}
	}
}

func runtimeStoreTransactionSemantics(t *testing.T, newRuntimeStore RuntimeStoreFactory) {
	t.Helper()
	rs := newRuntimeStore(t)
	if !rs.Capabilities().Has(runtime.StoreCapabilityTransactions) {
		t.Skip("transactions capability not advertised")
	}
	transactional := rs.(runtime.TransactionalStore)
	if !rs.Capabilities().Has(runtime.StoreCapabilityTranscript) {
		t.Skip("transaction semantics require a writable capability")
	}
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	write := func(callCtx context.Context, view runtime.RuntimeStore, id string) error {
		transcript := view.(runtime.TranscriptStore)
		return transcript.Threads().CreateThread(callCtx, runtime.ThreadRecord{ID: runtime.ThreadID(id), CreatedAt: now, UpdatedAt: now})
	}
	read := func(callCtx context.Context, view runtime.RuntimeStore, id string) bool {
		_, err := view.(runtime.TranscriptStore).Threads().GetThread(callCtx, runtime.ThreadID(id))
		return err == nil
	}
	if err := transactional.InTransaction(ctx, func(ctx context.Context, view runtime.RuntimeStore) error {
		return write(ctx, view, "tx-commit-1")
	}); err != nil {
		t.Fatalf("InTransaction commit: %v", err)
	}
	if !read(ctx, rs, "tx-commit-1") {
		t.Fatal("committed transaction record is missing after InTransaction returns")
	}
	if err := transactional.InTransaction(ctx, func(ctx context.Context, view runtime.RuntimeStore) error {
		if err := write(ctx, view, "tx-rollback-1"); err != nil {
			return err
		}
		return errors.New("lebro: rollback requested by contract test")
	}); err == nil {
		t.Fatal("InTransaction accepted a failing callback")
	}
	if read(ctx, rs, "tx-rollback-1") {
		t.Fatal("record written before rollback is visible after the transaction failed")
	}
}
