// multitenant-platform is the B2B build: one shared agent sold to many
// customers, with isolation enforced by a lebro Policy at four points — agent
// run start, each tool call, workflow runs, and every storage read/write on a
// PolicyStore — plus wire-level redaction of tool-call arguments on streams.
//
// The guarantees shown here are library-enforced, not glue code:
//
//   - a denied tenant never reaches the model (ActionAgentRun),
//   - a tool call without the required capability never executes
//     (ActionToolCall),
//   - a denied storage read never reaches the database (PolicyStore),
//   - streamed tool-call arguments are stripped by the default redactor.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

const callTools = lebro.Capability("tools:call")

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	ctx := context.Background()
	policy := platformPolicy{allowedTenant: "acme"}

	store := lebro.NewMemoryStore()
	guarded, err := lebro.NewPolicyStore(store, policy)
	if err != nil {
		return err
	}

	model := newFixtureModel(
		// Consumed by the capable run: request the tool, then close out.
		fixtureStep{toolCall: &lebro.ModelToolCall{
			ID:        "call-1",
			ToolID:    "weather.lookup",
			Arguments: json.RawMessage(`{"city":"Nairobi"}`),
		}},
		fixtureStep{content: "Nairobi is 24.5C."},
	)

	agent, err := newWeatherAgent("support", "Support", "Help the caller.", model, guarded, policy, 4)
	if err != nil {
		return err
	}

	// 1. A denied tenant is refused at run start; the model fixture is still
	// unconsumed because nothing reached the provider.
	denied := lebro.WithIdentity(ctx, lebro.Identity{Subject: "sam", Tenant: "other"})
	_, err = agent.Run(denied, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if !errors.Is(err, lebro.ErrPolicyDenied) {
		return fmt.Errorf("expected ErrPolicyDenied for other tenant, got %v", err)
	}
	if err := writef(output, "other tenant run: denied before any model call (fixtures left: %d)\n", model.remaining()); err != nil {
		return err
	}

	// 2. An acme caller without tools:call gets past the run gate, but the
	// model's tool request never executes: a separate fixture model drives this
	// run, and it fails with a typed, auditable denial naming the subject,
	// action, and resource.
	blockedModel := newFixtureModel(fixtureStep{toolCall: &lebro.ModelToolCall{
		ID:        "call-1",
		ToolID:    "weather.lookup",
		Arguments: json.RawMessage(`{"city":"Nairobi"}`),
	}})
	blockedAgent, err := newWeatherAgent("support", "Support", "Help the caller.", blockedModel, nil, policy, 0)
	if err != nil {
		return err
	}
	noCaps := lebro.WithIdentity(ctx, lebro.Identity{Subject: "kim", Tenant: "acme"})
	_, err = blockedAgent.Run(noCaps, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather?"}}})
	var denial *lebro.PolicyDenial
	if !errors.As(err, &denial) || denial.Action != lebro.ActionToolCall {
		return fmt.Errorf("expected a typed ActionToolCall denial, got %v", err)
	}
	if err := writef(output, "%s without %s: tool call denied (%s on %q)\n", "kim", callTools, denial.Action, denial.Resource.ID); err != nil {
		return err
	}

	// 3. The same agent with the capability executes the tool.
	capable := lebro.WithIdentity(ctx, lebro.Identity{
		Subject:      "ava",
		Tenant:       "acme",
		Capabilities: []lebro.Capability{callTools},
	})
	allowed, err := agent.Run(capable, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather?"}}})
	if err != nil {
		return err
	}
	if err := writef(output, "%s with %s: %s\n", "ava", callTools, allowed.Messages[len(allowed.Messages)-1].Content); err != nil {
		return err
	}

	// 4. Storage is guarded too: another tenant reading a thread it does not
	// own hits the policy before the repository.
	thief := lebro.WithIdentity(ctx, lebro.Identity{Subject: "sam", Tenant: "other"})
	_, readErr := guarded.Messages().ListMessages(thief, "any-thread", lebro.PageRequest{})
	if !errors.Is(readErr, lebro.ErrPolicyDenied) {
		return fmt.Errorf("expected storage read denial, got %v", readErr)
	}
	if err := writef(output, "cross-tenant thread read: denied before reaching the store\n"); err != nil {
		return err
	}

	// 5. On the wire, streamed tool-call arguments are redacted by default.
	return streamRedaction(output, policy)
}

// streamRedaction serves the same shape of agent over HTTP and streams a run
// whose model requests a tool call: the client sees the tool-call identity but
// never its arguments.
func streamRedaction(output io.Writer, policy lebro.Policy) error {
	ctx := context.Background()

	streamModel := newFixtureModel(
		fixtureStep{stream: true, toolCall: &lebro.ModelToolCall{
			ID:        "call-1",
			ToolID:    "weather.lookup",
			Arguments: json.RawMessage(`{"city":"Nairobi"}`),
		}},
		fixtureStep{content: "Done."},
	)
	streamed, err := newWeatherAgent("streamer", "Streamer", "", streamModel, nil, policy, 0)
	if err != nil {
		return err
	}

	server := httpapi.NewServer(httpapi.ServerConfig{
		Title:      "tenant-stream-example",
		Middleware: []func(http.Handler) http.Handler{identityMiddleware},
	})
	if err := server.ExposeAgent(streamed); err != nil {
		return err
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client, err := httpapi.NewClient(httpapi.ClientConfig{
		BaseURL: httpServer.URL,
		Header: func(r *http.Request) {
			// A real deployment derives these from verified credentials; the
			// server-side middleware below trusts them exactly that way.
			r.Header.Set("X-Tenant", "acme")
			r.Header.Set("X-Subject", "ava")
			r.Header.Set("X-Capabilities", string(callTools))
		},
	})
	if err != nil {
		return err
	}
	acmeCtx := lebro.WithIdentity(ctx, lebro.Identity{Subject: "ava", Tenant: "acme", Capabilities: []lebro.Capability{callTools}})

	stream, err := client.RunStream(acmeCtx, "streamer", httpapi.RunRequest{
		Messages: []httpapi.MessageInput{{Content: "Weather?"}},
	})
	if err != nil {
		return err
	}
	defer stream.Cancel()

	sawToolCall := false
	for event := range stream.Events {
		if event.ToolCall == nil {
			continue
		}
		sawToolCall = true
		if err := writef(output, "streamed tool call %s: arguments visible=%t\n", event.ToolCall.ToolID, len(event.ToolCall.Arguments) != 0); err != nil {
			return err
		}
	}
	if _, err := stream.Drain(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if !sawToolCall {
		return fmt.Errorf("stream never surfaced a tool call")
	}
	return nil
}

func newWeatherAgent(id, name, instructions string, model lebro.Model, store lebro.Store, policy lebro.Policy, maxSteps int) (*lebro.Agent, error) {
	registry, err := lebro.NewToolRegistry(lebrojsonschema.NewCompiler())
	if err != nil {
		return nil, err
	}
	if err := registry.Register(weatherTool{}); err != nil {
		return nil, err
	}
	return lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           lebro.AgentID(id),
			Name:         name,
			Instructions: instructions,
			Tools:        []lebro.ToolID{"weather.lookup"},
		},
		Model:    model,
		Tools:    registry,
		Store:    store,
		Policy:   policy,
		MaxSteps: maxSteps,
	})
}

// fixtureStep scripts one model call: either a tool-call request or a final
// text turn. stream=true marks a step the streaming protocol must consume —
// Generate refuses such a step, so a test that expected the streaming path
// but got the synchronous one fails instead of passing by accident.
type fixtureStep struct {
	content  string
	toolCall *lebro.ModelToolCall
	stream   bool
}

// fixtureModel is a deterministic stand-in for a provider adapter supporting
// both the generate and streaming protocols. A real deployment supplies
// openai.New, anthropic.New, or any other lebro.Model instead.
type fixtureModel struct {
	steps []fixtureStep
	next  int
}

func newFixtureModel(steps ...fixtureStep) *fixtureModel {
	return &fixtureModel{steps: steps}
}

func (m *fixtureModel) remaining() int { return len(m.steps) - m.next }

func (m *fixtureModel) take() (fixtureStep, error) {
	if m.next >= len(m.steps) {
		return fixtureStep{}, errors.New("fixture model script exhausted")
	}
	step := m.steps[m.next]
	m.next++
	return step, nil
}

func (m *fixtureModel) response(step fixtureStep) (lebro.ModelResponse, error) {
	message := lebro.Message{Role: lebro.RoleAssistant, Content: step.content}
	finish := lebro.FinishReasonStop
	if step.toolCall != nil {
		calls, err := lebro.NewModelToolCalls(*step.toolCall)
		if err != nil {
			return lebro.ModelResponse{}, err
		}
		message.Content = ""
		message.ToolCalls = calls
		finish = lebro.FinishReasonToolCalls
	}
	return lebro.ModelResponse{Message: message, FinishReason: finish}, nil
}

// Generate consumes the next scripted step synchronously; it refuses a step
// marked stream=true so the streaming path cannot silently pass for it.
func (m *fixtureModel) Generate(_ context.Context, _ lebro.ModelRequest) (lebro.ModelResponse, error) {
	step, err := m.take()
	if err != nil {
		return lebro.ModelResponse{}, err
	}
	if step.stream {
		return lebro.ModelResponse{}, errors.New("fixture model: stream-only step consumed through Generate")
	}
	return m.response(step)
}

// Stream consumes the next scripted step as ordered deltas.
func (m *fixtureModel) Stream(ctx context.Context, _ lebro.ModelRequest) (lebro.StreamReader, error) {
	step, err := m.take()
	if err != nil {
		return nil, err
	}
	deltas := make(chan lebro.StreamDelta, 2)
	if step.toolCall != nil {
		call := *step.toolCall
		deltas <- lebro.StreamDelta{ToolCall: &call}
	} else {
		deltas <- lebro.StreamDelta{Text: step.content}
	}
	finish := lebro.FinishReasonStop
	if step.toolCall != nil {
		finish = lebro.FinishReasonToolCalls
	}
	deltas <- lebro.StreamDelta{FinishReason: finish}
	close(deltas)

	return &lebro.StreamReaderFunc{
		NextFn: func() (lebro.StreamDelta, error) {
			select {
			case delta, ok := <-deltas:
				if !ok {
					return lebro.StreamDelta{}, io.EOF
				}
				return delta, nil
			case <-ctx.Done():
				return lebro.StreamDelta{}, ctx.Err()
			}
		},
		CloseFn: func() error { return nil },
	}, nil
}

// identityMiddleware is the service boundary: it turns verified request
// credentials into a lebro.Identity so the policy authorizes the real caller.
func identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capabilities := []lebro.Capability{}
		for _, c := range strings.Split(r.Header.Get("X-Capabilities"), ",") {
			if c = strings.TrimSpace(c); c != "" {
				capabilities = append(capabilities, lebro.Capability(c))
			}
		}
		identity := lebro.Identity{
			Subject:      r.Header.Get("X-Subject"),
			Tenant:       r.Header.Get("X-Tenant"),
			Capabilities: capabilities,
		}
		next.ServeHTTP(w, r.WithContext(lebro.WithIdentity(r.Context(), identity)))
	})
}

// platformPolicy enforces the tenant boundary at every point lebro consults a
// Policy. Denials are typed and auditable via *lebro.PolicyDenial.
type platformPolicy struct {
	allowedTenant string
}

func (p platformPolicy) Authorize(_ context.Context, identity lebro.Identity, action lebro.Action, resource lebro.Resource) lebro.Decision {
	switch action {
	case lebro.ActionAgentRun, lebro.ActionWorkflowRun, lebro.ActionNetworkRun:
		if identity.Tenant != p.allowedTenant {
			return lebro.Deny(fmt.Sprintf("tenant %q is not licensed for this agent", identity.Tenant))
		}
	case lebro.ActionToolCall:
		if identity.Tenant != p.allowedTenant || !identity.HasCapability(callTools) {
			return lebro.Deny(fmt.Sprintf("missing %s capability", callTools))
		}
	case lebro.ActionStorageRead, lebro.ActionStorageWrite:
		if identity.Tenant != p.allowedTenant {
			return lebro.Deny(fmt.Sprintf("tenant %q may not touch this store", identity.Tenant))
		}
	}
	return lebro.Allow()
}

// weatherTool is a schema-backed stand-in for any billable internal API.
type weatherTool struct{}

func (weatherTool) Definition() lebro.ToolDefinition {
	return lebro.ToolDefinition{
		ID:          "weather.lookup",
		Description: "Look up the current weather for a city",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["city"],
			"properties":{"city":{"type":"string"}},
			"additionalProperties":false
		}`),
		OutputSchema: json.RawMessage(`{
			"type":"object",
			"required":["temperature_c"],
			"properties":{"temperature_c":{"type":"number"}},
			"additionalProperties":false
		}`),
	}
}

func (weatherTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(map[string]float64{"temperature_c": 24.5})
}

func writef(output io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(output, format, args...)
	return err
}
