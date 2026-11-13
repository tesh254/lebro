package acp_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"go.uber.org/goleak"
)

// TestMain verifies the package leaves no goroutines behind after a full run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestConcurrentSessionsStreamWithoutCorruption runs prompts on several
// sessions at once and asserts every session's updates carry only its own
// session ID and only its own transcript profile. Each session runs a
// different number of turns, so the fixture's per-turn transcript echo gives
// every session a distinct expected sequence: a session that receives another
// session's stream shows an echo from a turn it never ran. It uses a SQLite
// store because the memory store's whole-state optimistic transactions
// conflict when concurrent runs persist at once.
func TestConcurrentSessionsStreamWithoutCorruption(t *testing.T) {
	t.Parallel()
	h := newHarnessWithStore(t, newSQLiteTestStore(t))

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
			// Session i runs i+1 turns, sequentially within the session.
			for turn := 0; turn <= i; turn++ {
				if _, err := h.prompt(t.Context(), id, "hello"); err != nil {
					errs[i] = err
					return
				}
			}
		}(i, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d prompt failed: %v", i, err)
		}
	}

	// Every recorded update must belong to a session this client created,
	// and each session's text must match its own turn profile: turn k of any
	// session echoes "saw {2k-1} prior messages", so session i (i+1 turns)
	// must contain echoes 1 through 2i+1 and nothing beyond.
	valid := make(map[string]bool)
	for _, id := range ids {
		valid[string(id)] = true
	}
	perSession := make(map[string]string) // session id -> concatenated text
	for _, u := range h.events.snapshot() {
		if !valid[u.SessionID] {
			t.Fatalf("update for unknown session %q leaked across sessions", u.SessionID)
		}
		if u.Update.AgentMessageChunk != nil && u.Update.AgentMessageChunk.Content.Text != nil {
			perSession[u.SessionID] += u.Update.AgentMessageChunk.Content.Text.Text
		}
	}
	for i, id := range ids {
		text := perSession[string(id)]
		for turn := 1; turn <= i+1; turn++ {
			want := fmt.Sprintf("saw %d prior messages", 2*turn-1)
			if !strings.Contains(text, want) {
				t.Fatalf("session %d missing its own turn %d echo %q; text = %q", i, turn, want, text)
			}
		}
		for turn := i + 2; turn <= sessions; turn++ {
			leaked := fmt.Sprintf("saw %d prior messages", 2*turn-1)
			if strings.Contains(text, leaked) {
				t.Fatalf("session %d received another session's update %q; text = %q", i, leaked, text)
			}
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
	// lifecycle for the same model tool-call ID, each within its own session.
	// The fixture reuses the ID across sessions, so attribution is by
	// session, not by call ID.
	type lifecycle struct {
		statuses []string
	}
	lifecycles := map[string]*lifecycle{} // session id -> updates
	for _, u := range h.events.updatesByToolCall() {
		if lifecycles[u.sessionID] == nil {
			lifecycles[u.sessionID] = &lifecycle{}
		}
		lifecycles[u.sessionID].statuses = append(lifecycles[u.sessionID].statuses, u.status)
	}
	if len(lifecycles) != 2 {
		t.Fatalf("expected 2 sessions with tool-call updates, got %v", lifecycles)
	}
	for sess, lc := range lifecycles {
		statuses := lc.statuses
		if len(statuses) < 3 || statuses[0] != "pending" || statuses[len(statuses)-1] != "completed" {
			t.Fatalf("session %s tool call lifecycle incomplete: %v", sess, statuses)
		}
		// The in_progress state must be present between pending and
		// completed; the adapter must not skip it.
		inProgress := false
		for _, status := range statuses[1 : len(statuses)-1] {
			if status == "in_progress" {
				inProgress = true
			}
		}
		if !inProgress {
			t.Fatalf("session %s tool call lifecycle missing in_progress: %v", sess, statuses)
		}
	}
}
