package runtime

import (
	"context"
	"encoding/json"
	"time"
)

// ConcurrencyPolicy governs what a Scheduler does when a schedule becomes due
// while a prior run of the same schedule is still in flight.
type ConcurrencyPolicy string

const (
	// ConcurrencyAllow starts the due run regardless of whether a prior run of
	// the same schedule is still running. It is the default when a schedule
	// leaves the policy empty.
	ConcurrencyAllow ConcurrencyPolicy = "allow"
	// ConcurrencySkip drops the due fire when a prior run of the same schedule
	// is still running, recording a skipped execution so the gap is visible in
	// history. The schedule still advances to its next fire time.
	ConcurrencySkip ConcurrencyPolicy = "skip"
)

// normalized returns the policy with the empty value resolved to the default.
func (p ConcurrencyPolicy) normalized() ConcurrencyPolicy {
	if p == "" {
		return ConcurrencyAllow
	}
	return p
}

// ScheduleExecStatus is the recorded outcome of a single schedule fire.
type ScheduleExecStatus string

const (
	// ScheduleExecSucceeded marks a fire whose workflow run completed
	// successfully.
	ScheduleExecSucceeded ScheduleExecStatus = "succeeded"
	// ScheduleExecFailed marks a fire whose workflow run returned an error;
	// the Error field carries the cause.
	ScheduleExecFailed ScheduleExecStatus = "failed"
	// ScheduleExecSkipped marks a fire dropped by the concurrency policy
	// because a prior run of the same schedule was still running.
	ScheduleExecSkipped ScheduleExecStatus = "skipped"
	// ScheduleExecMissed marks a fire whose scheduled time elapsed without the
	// scheduler ticking (for example while the process was down). The catch-up
	// records one missed execution per skipped occurrence so the gap is visible.
	ScheduleExecMissed ScheduleExecStatus = "missed"
)

// ScheduleRecord is the durable definition of a workflow trigger. Spec is the
// cron or "@every" expression (see ParseCronSpec); "@once" is reserved for
// internal durable workflow wakeups. WorkflowID names the workflow to run; the
// Scheduler resolves it to a bound LinearWorkflow.
// Input and Metadata are the raw JSON payload and metadata passed to each run;
// both may be nil. NextFireAt is the next instant at which the schedule is due;
// it is nil for a schedule that has no future fire (an exhausted or
// unsatisfiable spec). LastFireAt is the scheduled time of the most recent fire
// the scheduler processed, or nil before the first. A paused schedule is
// retained but never fires.
type ScheduleRecord struct {
	ID          ScheduleID        `json:"id"`
	WorkflowID  WorkflowID        `json:"workflow_id"`
	Namespace   string            `json:"namespace,omitempty"`
	OwnerID     string            `json:"owner_id,omitempty"`
	Spec        string            `json:"spec"`
	Paused      bool              `json:"paused,omitempty"`
	Concurrency ConcurrencyPolicy `json:"concurrency,omitempty"`
	Input       json.RawMessage   `json:"input,omitempty"`
	Metadata    json.RawMessage   `json:"metadata,omitempty"`
	NextFireAt  *time.Time        `json:"next_fire_at,omitempty"`
	LastFireAt  *time.Time        `json:"last_fire_at,omitempty"`
	// WakeRunID marks this as an internal one-shot workflow wakeup. Scheduler
	// resumes that exact run instead of starting a new one. WakeToken fences a
	// stale wakeup after the run has suspended again.
	WakeRunID RunID     `json:"wake_run_id,omitempty"`
	WakeToken string    `json:"wake_token,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ScheduleExecutionRecord is one entry in a schedule's execution history. It
// records the scheduled instant (ScheduledFor), the resulting workflow run ID
// (empty for skipped or missed fires that never started a run), the outcome
// Status, and, for failures, the error string. StartedAt and FinishedAt bracket
// the run; FinishedAt is nil for a record persisted before the run returned.
type ScheduleExecutionRecord struct {
	ID           ScheduleExecutionID `json:"id"`
	ScheduleID   ScheduleID          `json:"schedule_id"`
	RunID        RunID               `json:"run_id,omitempty"`
	Namespace    string              `json:"namespace,omitempty"`
	OwnerID      string              `json:"owner_id,omitempty"`
	Status       ScheduleExecStatus  `json:"status"`
	ScheduledFor time.Time           `json:"scheduled_for"`
	StartedAt    time.Time           `json:"started_at"`
	FinishedAt   *time.Time          `json:"finished_at,omitempty"`
	Error        string              `json:"error,omitempty"`
}

// ScheduleFilter narrows a ListSchedules query. A zero value returns every
// schedule. WorkflowID matches exactly when non-zero. When DueBy is non-nil,
// only non-paused schedules whose NextFireAt is at or before it are returned;
// this is how the Scheduler loads the work due at a tick.
type ScheduleFilter struct {
	WorkflowID WorkflowID
	DueBy      *time.Time
	Namespace  string
	OwnerID    string
}

// ScheduleRepository owns durable schedule definitions.
type ScheduleRepository interface {
	SaveSchedule(context.Context, ScheduleRecord) error
	GetSchedule(context.Context, ScheduleID) (ScheduleRecord, error)
	ListSchedules(context.Context, ScheduleFilter, PageRequest) (Page[ScheduleRecord], error)
	DeleteSchedule(context.Context, ScheduleID) error
}

// ScheduleExecutionRepository owns ordered schedule execution history.
type ScheduleExecutionRepository interface {
	SaveScheduleExecution(context.Context, ScheduleExecutionRecord) error
	ListScheduleExecutions(context.Context, ScheduleID, PageRequest) (Page[ScheduleExecutionRecord], error)
}
