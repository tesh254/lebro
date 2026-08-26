package channels

import (
	"context"
	"errors"
	"fmt"
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
		if responder, ok := b.adapter.(WebhookResponder); ok {
			response, handled, err := responder.WebhookResponse(r, body)
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if handled {
				if len(response) > 0 {
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(response)
				return
			}
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
		if err := validateInbound(message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// A dispatcher owns durable acceptance. Deduplicate only when its worker
		// calls RunDispatch, so a failed handoff leaves no marker that could make
		// a platform redelivery disappear.
		if s.config.Dispatch != nil {
			job := DispatchJob{AgentID: b.agentID, Platform: b.adapter.Platform(), Message: message}
			if err := s.config.Dispatch(context.WithoutCancel(r.Context()), job); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if acknowledger, ok := b.adapter.(WebhookAcknowledger); ok {
				response, contentType, err := acknowledger.WebhookAcknowledgement(r, body)
				if err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(response)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		// Deduplicate before a synchronous run: a redelivery must not produce a second
		// reply. A message with no provider ID cannot be keyed, so it is always
		// processed. The key is scoped to this agent-platform route so an equal
		// provider ID from a different platform or agent, routed through the same
		// shared deduplicator, is not mistaken for a redelivery.
		if message.ProviderMessageID != "" {
			duplicate, err := s.deduplicator.Seen(r.Context(), b.dedupKey(message.ProviderMessageID))
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

// RunDispatch runs one previously accepted [DispatchJob]. It is intended for a
// background worker paired with Config.Dispatch. The job must come from the
// same Server's verified webhook handler; callers must not construct jobs from
// untrusted input, because the job's sender identity is used for authorization.
func (s *Server) RunDispatch(ctx context.Context, job DispatchJob) error {
	s.mu.RLock()
	b, ok := s.routes[webhookPath(job.AgentID, job.Platform)]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("lebro/channels: dispatch route for agent %q platform %q is not registered", job.AgentID, job.Platform)
	}
	if job.Message.ProviderMessageID != "" {
		duplicate, err := s.deduplicator.Seen(ctx, b.dedupKey(job.Message.ProviderMessageID))
		if err != nil {
			return err
		}
		if duplicate {
			return nil
		}
	}
	return s.run(ctx, b, job.Message)
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
	// Policy authorizes the run against the external user, scoped by platform so
	// equal user IDs on different platforms are distinct callers.
	identity := message.Sender.Identity(message.Conversation.Platform)
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
	if err == nil {
		return nil
	}
	// The create failed. It may be a benign race — a concurrent delivery created
	// the thread between the Get and the Create, reported as a plain "already
	// exists" on the memory and SQLite stores and as ErrConflict on PostgreSQL —
	// or a genuine failure such as a serialization conflict that left the thread
	// absent. Re-check existence for *every* error rather than assuming
	// ErrConflict means the thread now exists: only a present thread is success.
	// Otherwise the run would proceed and RunStream would lazily create an
	// unscoped thread, losing the namespace and owner.
	if _, getErr := s.config.Store.Threads().GetThread(ctx, id); getErr == nil {
		return nil
	}
	return err
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
