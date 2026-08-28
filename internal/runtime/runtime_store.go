package runtime

import (
	"context"
	"errors"
	"fmt"
)

// RuntimeStoreContractVersion is the version of the capability-based runtime
// storage contract. It bumps when the contract's record shapes or capability
// semantics change in a way adapters must react to; the version is part of the
// public contract so adapters can detect and reject mismatches explicitly.
const RuntimeStoreContractVersion = 1

// StoreCapability names one optional storage capability a RuntimeStore can
// advertise. Capabilities are the unit of feature gating: when a feature needs
// a capability the adapter does not support, setup fails with a
// *StoreCapabilityError before any run starts.
type StoreCapability string

const (
	// StoreCapabilityTranscript covers thread records and the ordered message
	// transcript: prior-message reads plus append, update, and delete writes.
	StoreCapabilityTranscript StoreCapability = "transcript"
	// StoreCapabilityWorkingMemory covers scoped working-memory fact CRUD,
	// including the reads the memory processor performs for recall.
	StoreCapabilityWorkingMemory StoreCapability = "working_memory"
	// StoreCapabilityWorkflowState covers workflow runs and resumable
	// snapshots.
	StoreCapabilityWorkflowState StoreCapability = "workflow_state"
	// StoreCapabilitySchedules covers schedule records and their execution
	// history.
	StoreCapabilitySchedules StoreCapability = "schedules"
	// StoreCapabilityObservability covers durable run events, model attempts,
	// and tool executions. It is opt-in: an adapter without it leaves those
	// records unpersisted, matching the ObservabilityRepositories semantics.
	StoreCapabilityObservability StoreCapability = "observability"
	// StoreCapabilityTransactions covers atomic, coupled writes through
	// TransactionalStore. Adapters without it get sequential writes with the
	// fallback semantics documented on TransactionalStore.
	StoreCapabilityTransactions StoreCapability = "transactions"
)

// StoreCapabilities advertises the capabilities an adapter supports. Reads and
// writes are never implied: an adapter that can only write observability
// events must not advertise StoreCapabilityObservability, because the
// capability includes the reads Lebro performs for the feature.
type StoreCapabilities struct {
	Transcript    bool
	WorkingMemory bool
	WorkflowState bool
	Schedules     bool
	Observability bool
	Transactions  bool
}

// Has reports whether the capability set includes capability.
func (c StoreCapabilities) Has(capability StoreCapability) bool {
	switch capability {
	case StoreCapabilityTranscript:
		return c.Transcript
	case StoreCapabilityWorkingMemory:
		return c.WorkingMemory
	case StoreCapabilityWorkflowState:
		return c.WorkflowState
	case StoreCapabilitySchedules:
		return c.Schedules
	case StoreCapabilityObservability:
		return c.Observability
	case StoreCapabilityTransactions:
		return c.Transactions
	}
	return false
}

// AllStoreCapabilities returns a set advertising every capability. Built-in
// stores support all of them.
func AllStoreCapabilities() StoreCapabilities {
	return StoreCapabilities{
		Transcript:    true,
		WorkingMemory: true,
		WorkflowState: true,
		Schedules:     true,
		Observability: true,
		Transactions:  true,
	}
}

// RuntimeStore is the capability-based storage contract. An adapter implements
// only the capabilities it supports by combining this interface with the
// capability interfaces below and advertising the exact set from Capabilities.
// Unlike Store, adopting it requires no Lebro-owned schema, migration, or
// repository set: the adapter maps its native storage onto the neutral
// record types (ThreadRecord, MessageRecord, and their siblings) which are
// versioned JSON contracts, and Lebro validates capabilities before use.
//
// The advertisement must exactly match the implemented capability interfaces;
// validateRuntimeStore rejects both directions of mismatch at setup time so a
// drifting adapter fails with a typed error instead of panicking or silently
// dropping records later.
type RuntimeStore interface {
	Capabilities() StoreCapabilities
}

// TranscriptStore provides thread records and ordered messages. The reads are
// explicit on purpose: a write-only event sink cannot back conversation
// history, because agent runs load prior messages before the first model call.
type TranscriptStore interface {
	Threads() ThreadRepository
	Messages() MessageRepository
}

// WorkingMemoryStore provides scoped working-memory fact CRUD.
type WorkingMemoryStore interface {
	WorkingMemory() WorkingMemoryRepository
}

// WorkflowStateStore provides durable workflow runs and resumable snapshots.
type WorkflowStateStore interface {
	WorkflowRuns() WorkflowRunRepository
	WorkflowSnapshots() WorkflowSnapshotRepository
}

// ScheduleStore provides schedule records and their execution history.
type ScheduleStore interface {
	Schedules() ScheduleRepository
	ScheduleExecutions() ScheduleExecutionRepository
}

// ObservabilityStore provides durable run events, model attempts, and tool
// executions. Advertising it is the opt-in: without it those records are
// simply not persisted, matching the ObservabilityRepositories semantics for
// custom Store implementations.
type ObservabilityStore interface {
	RunEvents() RunEventRepository
	ModelAttempts() ModelAttemptRepository
	ToolExecutions() ToolExecutionRepository
}

// TransactionalStore provides atomic coupled writes, such as committing a run's
// transcript and observability records together. The runtime store handed to
// fn is the transaction-scoped view; writes through it commit atomically when
// fn returns nil and are discarded when it returns an error.
//
// Adapters that cannot provide a transaction omit this interface and the
// StoreCapabilityTransactions capability. Their writes then run sequentially
// in the order the coupled write would have used: no rollback, no ErrConflict.
// A failure mid-sequence leaves already-written records in place and returns
// the error, which callers surface instead of retrying silently.
type TransactionalStore interface {
	InTransaction(context.Context, func(context.Context, RuntimeStore) error) error
}

// ErrCapabilityMissing is the sentinel wrapped by every *StoreCapabilityError,
// so callers can match setup failures with errors.Is regardless of message.
var ErrCapabilityMissing = errors.New("lebro: storage capability not supported")

// StoreCapabilityError reports a capability requirement the attached storage
// adapter cannot satisfy, either because it does not advertise the capability
// or because its advertisement contradicts the interfaces it implements.
type StoreCapabilityError struct {
	// Capability is the capability that was required or misadvertised.
	Capability StoreCapability
	// Feature names what required the capability, for example
	// "thread persistence" or "memory processor".
	Feature string
	// Reason explains the mismatch without exposing internals.
	Reason string
}

func (e *StoreCapabilityError) Error() string {
	if e.Feature != "" {
		return fmt.Sprintf("lebro: storage adapter does not support capability %q required by %s: %s", e.Capability, e.Feature, e.Reason)
	}
	return fmt.Sprintf("lebro: storage adapter capability %q: %s", e.Capability, e.Reason)
}

func (e *StoreCapabilityError) Unwrap() error { return ErrCapabilityMissing }

// requireCapability returns a *StoreCapabilityError when caps does not include
// capability, which feature needs.
func requireCapability(caps StoreCapabilities, capability StoreCapability, feature string) error {
	if caps.Has(capability) {
		return nil
	}
	return &StoreCapabilityError{
		Capability: capability,
		Feature:    feature,
		Reason:     "the attached storage adapter does not advertise it",
	}
}

// validateRuntimeStore checks that a RuntimeStore's capability advertisement
// exactly matches the capability interfaces it implements and that every
// advertised accessor returns a repository. It returns the validated
// capabilities or a *StoreCapabilityError naming the first inconsistency.
func validateRuntimeStore(rs RuntimeStore) (StoreCapabilities, error) {
	if rs == nil || isNilInterface(rs) {
		return StoreCapabilities{}, errors.New("lebro: runtime store is required")
	}
	caps := rs.Capabilities()
	if caps == (StoreCapabilities{}) {
		return StoreCapabilities{}, errors.New("lebro: runtime store advertises no capabilities")
	}
	type capabilityCheck struct {
		capability StoreCapability
		check      func() bool
	}
	implemented := func(probe any) bool {
		return probe != nil && !isNilInterface(probe)
	}
	for _, check := range []capabilityCheck{
		{StoreCapabilityTranscript, func() bool {
			s, ok := rs.(TranscriptStore)
			return ok && implemented(s.Threads()) && implemented(s.Messages())
		}},
		{StoreCapabilityWorkingMemory, func() bool { s, ok := rs.(WorkingMemoryStore); return ok && implemented(s.WorkingMemory()) }},
		{StoreCapabilityWorkflowState, func() bool {
			s, ok := rs.(WorkflowStateStore)
			return ok && implemented(s.WorkflowRuns()) && implemented(s.WorkflowSnapshots())
		}},
		{StoreCapabilitySchedules, func() bool {
			s, ok := rs.(ScheduleStore)
			return ok && implemented(s.Schedules()) && implemented(s.ScheduleExecutions())
		}},
		{StoreCapabilityObservability, func() bool {
			s, ok := rs.(ObservabilityStore)
			return ok && implemented(s.RunEvents()) && implemented(s.ModelAttempts()) && implemented(s.ToolExecutions())
		}},
		{StoreCapabilityTransactions, func() bool { _, ok := rs.(TransactionalStore); return ok }},
	} {
		if err := validateCapabilityAdvertisement(caps, check.capability, check.check()); err != nil {
			return StoreCapabilities{}, err
		}
	}
	return caps, nil
}

// validateCapabilityAdvertisement enforces one capability's two-way match
// between advertisement and implementation.
func validateCapabilityAdvertisement(caps StoreCapabilities, capability StoreCapability, implemented bool) error {
	if caps.Has(capability) && !implemented {
		return &StoreCapabilityError{
			Capability: capability,
			Feature:    "capability advertisement",
			Reason:     "Capabilities() advertises it but the adapter does not implement the matching capability interface",
		}
	}
	if !caps.Has(capability) && implemented {
		return &StoreCapabilityError{
			Capability: capability,
			Feature:    "capability advertisement",
			Reason:     "the adapter implements the matching capability interface but Capabilities() does not advertise it",
		}
	}
	return nil
}

// runtimeStoreView exposes transaction-scoped repositories as a RuntimeStore.
// Built-in stores hand it to TransactionalStore.InTransaction so callers see
// the capability-shaped view of the current transaction. The view bypasses
// advertisement validation: its capabilities come from the store that created
// it and its accessors are satisfied by repositories the store itself built.
type runtimeStoreView struct {
	caps  StoreCapabilities
	repos Repositories
	obs   ObservabilityRepositories
}

// newRuntimeStoreView wraps transaction-scoped repositories. The
// observability capability is satisfied only when the repositories opt in
// through ObservabilityRepositories.
func newRuntimeStoreView(caps StoreCapabilities, repos Repositories) RuntimeStore {
	view := &runtimeStoreView{caps: caps, repos: repos}
	if obs, ok := repos.(ObservabilityRepositories); ok {
		view.obs = obs
	}
	return view
}

func (v *runtimeStoreView) Capabilities() StoreCapabilities { return v.caps }
func (v *runtimeStoreView) Threads() ThreadRepository       { return v.repos.Threads() }
func (v *runtimeStoreView) Messages() MessageRepository     { return v.repos.Messages() }
func (v *runtimeStoreView) WorkingMemory() WorkingMemoryRepository {
	return v.repos.WorkingMemory()
}
func (v *runtimeStoreView) WorkflowRuns() WorkflowRunRepository {
	return v.repos.WorkflowRuns()
}
func (v *runtimeStoreView) WorkflowSnapshots() WorkflowSnapshotRepository {
	return v.repos.WorkflowSnapshots()
}
func (v *runtimeStoreView) Schedules() ScheduleRepository { return v.repos.Schedules() }
func (v *runtimeStoreView) ScheduleExecutions() ScheduleExecutionRepository {
	return v.repos.ScheduleExecutions()
}
func (v *runtimeStoreView) RunEvents() RunEventRepository {
	if v.obs == nil {
		return uncapableRunEvents{}
	}
	return v.obs.RunEvents()
}
func (v *runtimeStoreView) ModelAttempts() ModelAttemptRepository {
	if v.obs == nil {
		return uncapableModelAttempts{}
	}
	return v.obs.ModelAttempts()
}
func (v *runtimeStoreView) ToolExecutions() ToolExecutionRepository {
	if v.obs == nil {
		return uncapableToolExecutions{}
	}
	return v.obs.ToolExecutions()
}
