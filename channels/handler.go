package channels

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tesh254/lebro"
)

// webhookHandler builds the HTTP handler for one agent-adapter binding. The
// returned handler verifies the request, decodes it, drops duplicates, maps the
// conversation to a thread, runs the agent, and streams the reply back through
// the adapter.
//
// Response codes are chosen so a platform's retry logic behaves: a rejected
// signature is 401, a decode failure is 400, an already-processed delivery is
// 200 (acknowledged, not retried), and a run or delivery failure is 500 (so the
// platform may redeliver, which deduplication then makes safe).
func (s *Server) webhookHandler(b binding) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := b.adapter.Verify(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		message, ok, err := b.adapter.Decode(r, body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !ok {
			// A handshake or receipt with no actionable message. Acknowledge so
			// the platform does not redeliver it.
			w.WriteHeader(http.StatusOK)
			return
		}
		if message.Conversation.ID == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Deduplicate before running: a redelivery must not produce a second
		// reply. A message with no provider ID cannot be keyed, so it is always
		// processed.
		if message.ProviderMessageID != "" {
			duplicate, err := s.deduplicator.Seen(r.Context(), message.ProviderMessageID)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if duplicate {
				w.WriteHeader(http.StatusOK)
				return
			}
		}

		if err := s.run(r.Context(), b, message); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// run maps the message to a thread, runs the agent as the sender, and relays
// the streamed reply to the adapter. It returns an error when the run fails or
// a chunk cannot be delivered, so the caller can respond 500 and let the
// platform redeliver.
func (s *Server) run(ctx context.Context, b binding, message InboundMessage) error {
	threadID, namespace, ownerID := s.mapper.Map(message)

	// Pre-create the thread with the mapper's namespace and owner. The runtime
	// also lazily creates a thread on persist, but with empty scoping fields;
	// creating it here first records the external principal and namespace on the
	// thread. Creation is idempotent: an existing thread is left untouched.
	if err := s.ensureThread(ctx, threadID, namespace, ownerID); err != nil {
		return err
	}

	// Carry the platform sender as the authenticated caller so a configured
	// Policy authorizes the run against the external user.
	identity := message.Sender.Identity()
	if namespace != "" {
		if identity.Attributes == nil {
			identity.Attributes = map[string]string{}
		}
		identity.Attributes["namespace"] = namespace
	}
	ctx = lebro.WithIdentity(ctx, identity)

	input := lebro.RunInput{
		// The role is fixed to user so a platform payload cannot forge a system
		// or assistant turn the model would treat as its own prior output,
		// matching the httpapi and mcp adapters.
		Messages: []lebro.Message{{Role: lebro.RoleUser, Content: message.Text}},
		ThreadID: threadID,
		Metadata: message.Metadata,
	}

	run, err := b.agent.RunStream(ctx, input)
	if err != nil {
		return err
	}
	// Cancel unconditionally: an early return from a delivery failure must not
	// leave the run goroutine parked writing to a channel nobody reads.
	defer run.Cancel()

	format := s.config.TextFormat
	var deliverErr error
	for delta := range run.Deltas {
		if delta.Text == "" {
			continue
		}
		if err := b.adapter.Send(ctx, message.Conversation, OutboundMessage{Text: delta.Text, Format: format}); err != nil {
			// Stop delivering, but keep draining so the run goroutine can
			// finish; the deferred Cancel unblocks the provider stream.
			deliverErr = err
			run.Cancel()
			for range run.Deltas {
			}
			break
		}
	}

	result, runErr := run.Wait()
	if deliverErr != nil {
		return deliverErr
	}
	if runErr != nil {
		return runErr
	}

	// Deliver the terminal chunk carrying the run's complete assistant message,
	// so an adapter that ignores incremental chunks still posts the full reply
	// and an adapter that rendered them can finalize.
	return b.adapter.Send(ctx, message.Conversation, OutboundMessage{
		Text:   assistantText(result),
		Final:  true,
		Format: format,
	})
}

// ensureThread creates the run's thread with the given scoping when it does not
// already exist. It is a no-op without a configured Store, since there is then
// no thread to scope, and idempotent when the thread is already present: a
// concurrent creator winning the race leaves the thread as-is rather than
// erroring, so a second inbound message on the same conversation does not fail.
func (s *Server) ensureThread(ctx context.Context, id lebro.ThreadID, namespace, ownerID string) error {
	if s.config.Store == nil {
		return nil
	}
	if _, err := s.config.Store.Threads().GetThread(ctx, id); err == nil {
		return nil
	} else if !errors.Is(err, lebro.ErrNotFound) {
		return err
	}
	now := time.Now().UTC()
	err := s.config.Store.Threads().CreateThread(ctx, lebro.ThreadRecord{
		ID:        id,
		Namespace: namespace,
		OwnerID:   ownerID,
		CreatedAt: now,
		UpdatedAt: now,
	})
	// A concurrent delivery may have created the thread between the Get and the
	// Create; treat that as success rather than surfacing a spurious conflict.
	if err != nil && !errors.Is(err, lebro.ErrConflict) {
		// CreateThread reports a duplicate as a plain error rather than
		// ErrConflict, so a second creator's "already exists" also lands here;
		// re-check existence to distinguish it from a real failure.
		if _, getErr := s.config.Store.Threads().GetThread(ctx, id); getErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// assistantText returns the last assistant turn in a completed run, or empty
// when the transcript has none, mirroring the httpapi projection. It never
// leaks a tool or system turn.
func assistantText(result lebro.RunResult) string {
	for i := len(result.Messages) - 1; i >= 0; i-- {
		if result.Messages[i].Role == lebro.RoleAssistant {
			return result.Messages[i].Content
		}
	}
	return ""
}
