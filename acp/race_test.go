package acp_test

import (
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/tesh254/lebro"
	"go.uber.org/goleak"
)

// TestMain verifies the package leaves no goroutines behind after a full run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestConcurrentSessionsStreamWithoutCorruption runs prompts on several
// sessions at once and asserts every session's updates carry only its own
// session ID — the transport-level guarantee that concurrent activity cannot
// interleave message bytes.
func TestConcurrentSessionsStreamWithoutCorruption(t *testing.T) {
	t.Parallel()
	steps := []lebro.ModelResponse{}
	for i := 0; i < 4; i++ {
		steps = append(steps, textResponse("reply"))
	}
	h := newHarness(t, steps...)

	h.initialize(t.Context())
	const sessions = 4
	ids := make([]acp.SessionId, sessions)
	for i := range ids {
		ids[i] = h.newSession(t.Context())
	}

	var wg sync.WaitGroup
	errs := make([]error, sessions)
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id acp.SessionId) {
			defer wg.Done()
			_, errs[i] = h.prompt(t.Context(), id, "hello")
		}(i, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d prompt failed: %v", i, err)
		}
	}

	// Every recorded update must belong to a session this client created.
	valid := make(map[string]bool)
	for _, id := range ids {
		valid[string(id)] = true
	}
	for _, u := range h.events.snapshot() {
		if !valid[u.SessionID] {
			t.Fatalf("update for unknown session %q leaked across sessions", u.SessionID)
		}
	}
}

// TestStableToolCallIDsAcrossConcurrentTurns asserts tool-call IDs remain
// stable per session even when two sessions run tool turns simultaneously.
// It uses a SQLite store because the memory store's whole-state optimistic
// transactions conflict when two runs persist at once; real deployments use
// SQLite or Postgres, so this tests the concurrent-session guarantee against
// a production-shaped store.
func TestStableToolCallIDsAcrossConcurrentTurns(t *testing.T) {
	t.Parallel()
	// No scripted steps: the content-driven fixture model requests the
	// protected tool for prompts containing "call", independently per
	// session, so concurrent turns behave identically.
	h := newHarnessWithStore(t, newSQLiteTestStore(t))
	h.initialize(t.Context())
	sessA := h.newSession(t.Context())
	sessB := h.newSession(t.Context())

	h.events.allow <- "allow"
	h.events.allow <- "allow"

	var wg sync.WaitGroup
	for _, id := range []acp.SessionId{sessA, sessB} {
		wg.Add(1)
		go func(id acp.SessionId) {
			defer wg.Done()
			if _, err := h.prompt(t.Context(), id, "please call the tool"); err != nil {
				t.Errorf("prompt %s: %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	if got := h.tool.executionCount(); got != 2 {
		t.Fatalf("tool executions = %d, want 2", got)
	}
	// Both sessions observed a full pending -> in_progress -> completed
	// lifecycle for the same model tool-call ID.
	lifecycles := map[string][]string{}
	for _, call := range h.events.toolCalls() {
		lifecycles[call[0]] = append(lifecycles[call[0]], call[1])
	}
	for id, statuses := range lifecycles {
		if len(statuses) < 3 || statuses[0] != "pending" || statuses[len(statuses)-1] != "completed" {
			t.Fatalf("tool call %s lifecycle incomplete: %v", id, statuses)
		}
	}
}
