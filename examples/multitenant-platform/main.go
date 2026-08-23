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
	"github.com/tesh254/lebro/internal/testkit"
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
	guarded := mustValue(lebro.NewPolicyStore(store, policy))

	model := testkit.NewModel(
		// Consumed by the capable run.
		testkit.ToolCallResponse(lebro.ModelToolCall{
			ID:        "call-1",
			ToolID:    "weather.lookup",
			Arguments: json.RawMessage(`{"city":"Nairobi"}`),
		}),
		testkit.Text("Nairobi is 24.5C."),
	)

	registry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(registry.Register(weatherTool{}))

	agent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support",
			Name:         "Support",
			Instructions: "Help the caller.",
			Tools:        []lebro.ToolID{"weather.lookup"},
		},
		Model:    model,
		Tools:    registry,
		Store:    guarded,
		Policy:   policy,
		MaxSteps: 4,
	}))

	// 1. A denied tenant is refused at run start; the model fixture is still
	// unconsumed because nothing reached the provider.
	denied := lebro.WithIdentity(ctx, lebro.Identity{Subject: "sam", Tenant: "other"})
	_, err := agent.Run(denied, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "hi"}}})
	if !errors.Is(err, lebro.ErrPolicyDenied) {
		return fmt.Errorf("expected ErrPolicyDenied for other tenant, got %v", err)
	}
	writef(output, "other tenant run: denied before any model call (fixtures left: %d)\n", model.Remaining())

	// 2. An acme caller without tools:call gets past the run gate, but the
	// model's tool request never executes: a separate fixture model drives this
	// run, and it fails with a typed, auditable denial naming the subject,
	// action, and resource.
	blockedModel := testkit.NewModel(testkit.ToolCallResponse(lebro.ModelToolCall{
		ID:        "call-1",
		ToolID:    "weather.lookup",
		Arguments: json.RawMessage(`{"city":"Nairobi"}`),
	}))
	blockedAgent := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:           "support",
			Name:         "Support",
			Instructions: "Help the caller.",
			Tools:        []lebro.ToolID{"weather.lookup"},
		},
		Model:  blockedModel,
		Tools:  registry,
		Policy: policy,
	}))
	noCaps := lebro.WithIdentity(ctx, lebro.Identity{Subject: "kim", Tenant: "acme"})
	_, err = blockedAgent.Run(noCaps, lebro.RunInput{Messages: []lebro.Message{{Role: lebro.RoleUser, Content: "Weather?"}}})
	var denial *lebro.PolicyDenial
	if !errors.As(err, &denial) || denial.Action != lebro.ActionToolCall {
		return fmt.Errorf("expected a typed ActionToolCall denial, got %v", err)
	}
	writef(output, "%s without %s: tool call denied (%s on %q)\n", "kim", callTools, denial.Action, denial.Resource.ID)

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
	writef(output, "%s with %s: %s\n", "ava", callTools, allowed.Messages[len(allowed.Messages)-1].Content)

	// 4. Storage is guarded too: another tenant reading a thread it does not
	// own hits the policy before the repository.
	thief := lebro.WithIdentity(ctx, lebro.Identity{Subject: "sam", Tenant: "other"})
	_, readErr := guarded.Messages().ListMessages(thief, "any-thread", lebro.PageRequest{})
	if !errors.Is(readErr, lebro.ErrPolicyDenied) {
		return fmt.Errorf("expected storage read denial, got %v", readErr)
	}
	writef(output, "cross-tenant thread read: denied before reaching the store\n")

	// 5. On the wire, streamed tool-call arguments are redacted by default.
	return streamRedaction(output, policy)
}

// streamRedaction serves the same shape of agent over HTTP and streams a run
// whose model requests a tool call: the client sees the tool-call identity but
// never its arguments.
func streamRedaction(output io.Writer, policy lebro.Policy) error {
	ctx := context.Background()

	streamModel := testkit.NewModel(
		testkit.Stream(testkit.ToolCallChunk(lebro.ModelToolCall{
			ID:        "call-1",
			ToolID:    "weather.lookup",
			Arguments: json.RawMessage(`{"city":"Nairobi"}`),
		})),
		testkit.Text("Done."),
	)
	streamRegistry := mustValue(lebro.NewToolRegistry(lebrojsonschema.NewCompiler()))
	must(streamRegistry.Register(weatherTool{}))
	streamed := mustValue(lebro.NewAgent(lebro.AgentConfig{
		Definition: lebro.AgentDefinition{
			ID:    "streamer",
			Name:  "Streamer",
			Tools: []lebro.ToolID{"weather.lookup"},
		},
		Model:  streamModel,
		Tools:  streamRegistry,
		Policy: policy,
	}))

	server := httpapi.NewServer(httpapi.ServerConfig{
		Title:      "tenant-stream-example",
		Middleware: []func(http.Handler) http.Handler{identityMiddleware},
	})
	if err := server.ExposeAgent(streamed); err != nil {
		return err
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	client := mustValue(httpapi.NewClient(httpapi.ClientConfig{
		BaseURL: httpServer.URL,
		Header: func(r *http.Request) {
			// A real deployment derives these from verified credentials; the
			// server-side middleware below trusts them exactly that way.
			r.Header.Set("X-Tenant", "acme")
			r.Header.Set("X-Subject", "ava")
			r.Header.Set("X-Capabilities", string(callTools))
		},
	}))
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
		writef(output, "streamed tool call %s: arguments visible=%t\n", event.ToolCall.ToolID, len(event.ToolCall.Arguments) != 0)
	}
	if _, err := stream.Drain(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if !sawToolCall {
		return fmt.Errorf("stream never surfaced a tool call")
	}
	return nil
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

func mustValue[T any](value T, err error) T {
	must(err)
	return value
}
