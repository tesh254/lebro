package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/httpapi"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

// openAPIDocument is the subset of the generated document these tests assert
// on. Decoding into a typed shape rather than map[string]any keeps the
// assertions readable and fails loudly if the top-level structure changes.
type openAPIDocument struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description"`
	} `json:"info"`
	Paths      map[string]map[string]openAPIOperation `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type openAPIOperation struct {
	OperationID string `json:"operationId"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Parameters  []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
	RequestBody *struct {
		Content map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Description string `json:"description"`
		Content     map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

func generateDocument(t *testing.T, server *httpapi.Server) openAPIDocument {
	t.Helper()
	encoded, err := server.OpenAPI()
	must(t, err)
	var document openAPIDocument
	must(t, json.Unmarshal(encoded, &document))
	return document
}

// This is the ticket's acceptance criterion asserted directly: every route the
// router serves is described in the document, and every described route is
// actually served. Checking both directions is what makes it meaningful — a
// one-way check passes for a document that documents routes that do not exist.
func TestOpenAPICoversEveryServedRoute(t *testing.T) {
	// A Store is configured and a thread created so the thread routes resolve
	// to a real resource. Without it their 404 would be indistinguishable from
	// the catch-all 404 that signals an undocumented route.
	store := lebro.NewMemoryStore()
	must(t, store.CreateThread(context.Background(), lebro.ThreadRecord{ID: "t-1"}))

	server := httpapi.NewServer(httpapi.ServerConfig{Store: store})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))
	handler := server.Handler()
	document := generateDocument(t, server)

	documented := map[string]bool{}
	for path, item := range document.Paths {
		for method := range item {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}

	// Every documented operation must resolve to a real handler. A request
	// against a documented path that falls through to the catch-all 404 proves
	// the document describes a route the router does not serve.
	for path, item := range document.Paths {
		for method, operation := range item {
			target := concreteTarget(path)
			recorder := doJSON(t, handler, strings.ToUpper(method), target, nil)
			if recorder.Code == http.StatusNotFound {
				body := decodeBody[httpapi.ErrorResponse](t, recorder)
				// A path-level 404 from an unmatched route is
				// indistinguishable in status from a matched route reporting a
				// missing resource, so the concrete target uses identifiers
				// that do exist.
				if body.Error.Code == httpapi.ErrorCodeNotFound {
					t.Errorf("documented operation %s %s (%s) is not served", strings.ToUpper(method), path, operation.OperationID)
				}
			}
			if operation.OperationID == "" {
				t.Errorf("operation %s %s has no operationId", strings.ToUpper(method), path)
			}
			if operation.Summary == "" {
				t.Errorf("operation %s has no summary", operation.OperationID)
			}
			if _, ok := operation.Responses["200"]; !ok {
				t.Errorf("operation %s documents no 200 response", operation.OperationID)
			}
		}
	}

	// Every route the package serves must be documented. The expected set is
	// restated here rather than read from the route table, so a route added to
	// the table without a documentation entry is caught instead of both sides
	// moving together silently.
	for _, want := range []string{
		"GET /health",
		"GET /agents",
		"POST /agents/{id}/runs",
		"POST /agents/{id}/runs/stream",
		"POST /agents/{id}/runs/ai-sdk/stream",
		"GET /workflows",
		"POST /workflows/{id}/runs",
		"GET /threads/{id}",
		"GET /threads/{id}/messages",
		"GET /openapi.json",
	} {
		if !documented[want] {
			t.Errorf("route %s is served but absent from the OpenAPI document", want)
		}
	}
	if len(documented) != 10 {
		t.Errorf("documented routes = %d, want 10: %v", len(documented), documented)
	}
}

// concreteTarget substitutes identifiers that the test server actually exposes,
// so a 404 means "route not served" rather than "resource absent".
func concreteTarget(path string) string {
	path = strings.ReplaceAll(path, "/agents/{id}", "/agents/assistant")
	path = strings.ReplaceAll(path, "/workflows/{id}", "/workflows/echo")
	path = strings.ReplaceAll(path, "/threads/{id}", "/threads/t-1")
	return path
}

// Every $ref in the document must resolve to a defined component. A dangling
// reference makes the document unusable by a code generator while still
// parsing as JSON, so it cannot be caught by decoding alone.
func TestOpenAPIReferencesResolve(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))

	encoded, err := server.OpenAPI()
	must(t, err)
	document := generateDocument(t, server)

	var walk func(value any)
	refs := map[string]bool{}
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "$ref" {
					if ref, ok := child.(string); ok {
						refs[ref] = true
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	var raw any
	must(t, json.Unmarshal(encoded, &raw))
	walk(raw)

	if len(refs) == 0 {
		t.Fatal("document contains no schema references")
	}
	for ref := range refs {
		name, ok := strings.CutPrefix(ref, "#/components/schemas/")
		if !ok {
			t.Errorf("reference %q is not a component schema reference", ref)
			continue
		}
		if _, defined := document.Components.Schemas[name]; !defined {
			t.Errorf("reference %q does not resolve to a defined schema", ref)
		}
	}
}

func TestAISDKStreamOpenAPIContract(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	document := generateDocument(t, server)
	operation := document.Paths["/agents/{id}/runs/ai-sdk/stream"]["post"]
	var versionFound bool
	for _, parameter := range operation.Parameters {
		if parameter.Name == "version" && parameter.In == "query" && parameter.Required {
			versionFound = true
		}
	}
	if !versionFound {
		t.Fatal("AI SDK stream does not document required version query parameter")
	}
	for _, contentType := range []string{"text/plain", "text/event-stream"} {
		if _, ok := operation.Responses["200"].Content[contentType]; !ok {
			t.Errorf("AI SDK stream does not document %s response", contentType)
		}
	}
}

// Component schemas must themselves be valid JSON Schema, otherwise the
// published contract is decorative.
func TestOpenAPISchemasCompile(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))
	document := generateDocument(t, server)

	compiler := lebrojsonschema.NewCompiler()
	for name, schema := range document.Components.Schemas {
		// A schema carrying a $ref points outside itself; compiling it alone
		// would fail on the unresolvable reference rather than on a defect.
		if strings.Contains(string(schema), `"$ref"`) {
			continue
		}
		if _, err := compiler.Compile(schema); err != nil {
			t.Errorf("component schema %q does not compile: %v", name, err)
		}
	}
}

// A workflow's declared input schema must appear in the published contract, so
// a client sees the shape the run will actually validate against rather than
// "any JSON".
func TestOpenAPIEmbedsWorkflowInputSchema(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))
	document := generateDocument(t, server)

	schema, ok := document.Components.Schemas["WorkflowRunRequest_echo"]
	if !ok {
		t.Fatalf("no per-workflow request schema; schemas = %v", schemaNames(document))
	}
	var decoded struct {
		Properties struct {
			Input json.RawMessage `json:"input"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	must(t, json.Unmarshal(schema, &decoded))
	if !strings.Contains(string(decoded.Properties.Input), `"value"`) {
		t.Fatalf("embedded input schema does not carry the workflow's declared shape: %s", decoded.Properties.Input)
	}
	if len(decoded.Required) != 1 || decoded.Required[0] != "input" {
		t.Fatalf("required = %v, want [input]", decoded.Required)
	}
}

// A workflow ID containing characters outside the component-name set must not
// produce an unreferenceable schema name, and the escaping must keep distinct
// IDs distinct.
func TestOpenAPISanitizesWorkflowSchemaNames(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "billing.invoice/sync")))
	document := generateDocument(t, server)

	name := "WorkflowRunRequest_billing_x00002einvoice_x00002fsync"
	if _, ok := document.Components.Schemas[name]; !ok {
		t.Fatalf("sanitized schema %q missing; schemas = %v", name, schemaNames(document))
	}
	for _, schema := range schemaNames(document) {
		if strings.ContainsAny(schema, "./ ") {
			t.Errorf("schema name %q contains a character outside the component-name set", schema)
		}
	}
}

// IDs that differ only in characters the sanitizer escapes must not collide.
// A lossy sanitizer maps "billing.sync" and "billing_sync" onto one component
// name, so the second workflow registered silently overwrites the first's
// schema and one of them is published with the wrong validation contract.
func TestOpenAPIWorkflowSchemaNamesDoNotCollide(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "billing.sync")))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "billing_sync")))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "billing-sync")))
	document := generateDocument(t, server)

	found := 0
	for _, name := range schemaNames(document) {
		if strings.HasPrefix(name, "WorkflowRunRequest_billing") {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("per-workflow schemas = %d, want 3 distinct: %v", found, schemaNames(document))
	}
}

// The escape must be fixed-width, not just present. A variable-width "_x%x" is
// ambiguous whenever an escape is followed by a hex digit: "a.b" ('.' is 0x2e,
// then a literal 'b') and "a\u2eb" both render as "a_x2eb", so one workflow's
// schema silently overwrites the other's.
func TestOpenAPIWorkflowSchemaNamesSurviveHexAdjacency(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "a.b")))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "a˫")))
	document := generateDocument(t, server)

	found := 0
	for _, name := range schemaNames(document) {
		if strings.HasPrefix(name, "WorkflowRunRequest_a") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("per-workflow schemas = %d, want 2 distinct for IDs that differ only across an escape boundary: %v",
			found, schemaNames(document))
	}
}

// The workflow body is one operation covering every exposed workflow ID, so
// "required" must hold for all of them. A server mixing a schema-ful workflow
// with a schema-less one must not tell clients running the schema-less one that
// a body is mandatory, because the handler accepts an omitted body there.
func TestOpenAPIWorkflowBodyRequiredAccountsForMixedRegistrations(t *testing.T) {
	requiredFor := func(t *testing.T, expose func(*httpapi.Server)) bool {
		t.Helper()
		server := httpapi.NewServer(httpapi.ServerConfig{})
		expose(server)
		encoded, err := server.OpenAPI()
		must(t, err)
		var raw struct {
			Paths map[string]map[string]struct {
				RequestBody *struct {
					Required bool `json:"required"`
				} `json:"requestBody"`
			} `json:"paths"`
		}
		must(t, json.Unmarshal(encoded, &raw))
		body := raw.Paths["/workflows/{id}/runs"]["post"].RequestBody
		if body == nil {
			t.Fatal("workflow run documents no request body")
		}
		return body.Required
	}

	t.Run("every workflow requires input", func(t *testing.T) {
		got := requiredFor(t, func(s *httpapi.Server) {
			must(t, s.ExposeWorkflow(newEchoWorkflow(t, "alpha")))
			must(t, s.ExposeWorkflow(newEchoWorkflow(t, "beta")))
		})
		if !got {
			t.Fatal("body documented optional though every workflow rejects an omitted body")
		}
	})

	t.Run("mixed registrations", func(t *testing.T) {
		got := requiredFor(t, func(s *httpapi.Server) {
			must(t, s.ExposeWorkflow(newEchoWorkflow(t, "alpha")))
			must(t, s.ExposeWorkflow(newPermissiveWorkflow(t, "loose")))
		})
		if got {
			t.Fatal("body documented required though the schema-less workflow accepts an omitted body")
		}
	})

	t.Run("no workflow requires input", func(t *testing.T) {
		got := requiredFor(t, func(s *httpapi.Server) {
			must(t, s.ExposeWorkflow(newPermissiveWorkflow(t, "loose")))
		})
		if got {
			t.Fatal("body documented required though no workflow declares an input schema")
		}
	})
}

// The workflow request body must combine per-workflow schemas with anyOf.
// oneOf requires an instance to match exactly one branch, so two workflows that
// accept the same shape — or a schema-ful workflow alongside the permissive
// generic body — would make every valid request match more than one branch and
// be rejected by the contract that documents it. The path's {id} already
// selects the workflow, so exclusivity is not the property to enforce.
func TestOpenAPIWorkflowBodyUsesAnyOf(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "alpha")))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "beta")))
	document := generateDocument(t, server)

	operation := document.Paths["/workflows/{id}/runs"]["post"]
	if operation.RequestBody == nil {
		t.Fatal("workflow run documents no request body")
	}
	schema := string(operation.RequestBody.Content["application/json"].Schema)
	if strings.Contains(schema, `"oneOf"`) {
		t.Fatalf("workflow body uses oneOf, which rejects requests matching two identical workflow schemas: %s", schema)
	}
	if !strings.Contains(schema, `"anyOf"`) {
		t.Fatalf("workflow body schema = %s, want an anyOf over per-workflow variants", schema)
	}

	// The canonical request for one of these workflows must validate. Two
	// workflows share a shape here, so this is exactly the case oneOf broke.
	assertValidatesAgainstAnyBranch(t, document, schema, `{"input":{"value":"x"}}`)
}

// assertValidatesAgainstAnyBranch compiles each anyOf branch and requires the
// instance to satisfy at least one, which is what an anyOf means.
func assertValidatesAgainstAnyBranch(t *testing.T, document openAPIDocument, schema, instance string) {
	t.Helper()
	var combinator struct {
		AnyOf []struct {
			Ref string `json:"$ref"`
		} `json:"anyOf"`
	}
	must(t, json.Unmarshal([]byte(schema), &combinator))
	if len(combinator.AnyOf) == 0 {
		t.Fatal("schema has no anyOf branches")
	}

	compiler := lebrojsonschema.NewCompiler()
	for _, branch := range combinator.AnyOf {
		name, ok := strings.CutPrefix(branch.Ref, "#/components/schemas/")
		if !ok {
			continue
		}
		compiled, err := compiler.Compile(document.Components.Schemas[name])
		if err != nil {
			continue
		}
		if compiled.Validate([]byte(instance)) == nil {
			return
		}
	}
	t.Fatalf("instance %s validates against no branch of %s", instance, schema)
}

// A workflow whose first step requires input rejects an omitted body with 400,
// so documenting the body as optional would publish a contract the server does
// not honor.
func TestOpenAPIMarksWorkflowBodyRequired(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "echo")))

	encoded, err := server.OpenAPI()
	must(t, err)
	var raw struct {
		Paths map[string]map[string]struct {
			RequestBody *struct {
				Required bool `json:"required"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	must(t, json.Unmarshal(encoded, &raw))

	workflow := raw.Paths["/workflows/{id}/runs"]["post"]
	if workflow.RequestBody == nil || !workflow.RequestBody.Required {
		t.Fatal("workflow run body is documented as optional, but a required input rejects an omitted body")
	}
	// An agent run genuinely accepts an empty body, so it must stay optional.
	agent := raw.Paths["/agents/{id}/runs"]["post"]
	if agent.RequestBody == nil || agent.RequestBody.Required {
		t.Fatal("agent run body is documented as required, but an empty body is accepted")
	}
}

// The generated RunRequest schema must accept exactly what the handler accepts.
// A schema that requires `content` or forbids null while the handler allows
// both leaves a client's local validation rejecting requests the server serves.
func TestOpenAPIRunRequestSchemaMatchesHandler(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	document := generateDocument(t, server)

	compiled, err := lebrojsonschema.NewCompiler().Compile(document.Components.Schemas["RunRequest"])
	must(t, err)

	// Every one of these is accepted by the handler, asserted by
	// TestNullJSONFieldsAreHandled and TestAgentRunAcceptsEmptyBody.
	for _, instance := range []string{
		`{}`,
		`{"messages":[]}`,
		`{"messages":null}`,
		`{"messages":[{}]}`,
		`{"messages":[{"content":"hi"}]}`,
		`{"messages":[{"content":"hi"}],"metadata":null}`,
		`{"messages":[{"content":"hi"}],"metadata":{"k":"v"}}`,
	} {
		if err := compiled.Validate([]byte(instance)); err != nil {
			t.Errorf("schema rejects %s, which the handler accepts: %v", instance, err)
		}
	}

	// A role the client tried to set is still rejected, matching
	// DisallowUnknownFields in the handler.
	if compiled.Validate([]byte(`{"messages":[{"role":"system","content":"x"}]}`)) == nil {
		t.Error("schema accepts a client-supplied role, which the handler rejects")
	}
}

func TestOpenAPIListsExposedPrimitivesInDescriptions(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "researcher", &scriptedModel{})))
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "billing")))
	document := generateDocument(t, server)

	run := document.Paths["/agents/{id}/runs"]["post"]
	if !strings.Contains(run.Description, "researcher") {
		t.Fatalf("agent run description does not name the exposed agent: %q", run.Description)
	}
	workflow := document.Paths["/workflows/{id}/runs"]["post"]
	if !strings.Contains(workflow.Description, "billing") {
		t.Fatalf("workflow run description does not name the exposed workflow: %q", workflow.Description)
	}
}

func TestOpenAPIReportsNoExposedPrimitives(t *testing.T) {
	document := generateDocument(t, httpapi.NewServer(httpapi.ServerConfig{}))
	run := document.Paths["/agents/{id}/runs"]["post"]
	if !strings.Contains(run.Description, "No agents are currently exposed") {
		t.Fatalf("description does not report an empty server: %q", run.Description)
	}
}

func TestOpenAPIDocumentsErrorResponses(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	document := generateDocument(t, server)

	operation := document.Paths["/agents/{id}/runs"]["post"]
	for _, status := range []string{"400", "404", "500", "502", "499"} {
		if _, ok := operation.Responses[status]; !ok {
			t.Errorf("agent run does not document a %s response; documented = %v", status, responseStatuses(operation))
		}
	}
}

func TestOpenAPIStreamRouteDocumentsEventStream(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))
	document := generateDocument(t, server)

	operation := document.Paths["/agents/{id}/runs/stream"]["post"]
	success, ok := operation.Responses["200"]
	if !ok {
		t.Fatal("stream route documents no 200 response")
	}
	if _, ok := success.Content["text/event-stream"]; !ok {
		t.Fatalf("stream 200 content types = %v, want text/event-stream", contentTypes(success.Content))
	}
}

func TestOpenAPIInfoUsesConfiguredIdentity(t *testing.T) {
	document := generateDocument(t, httpapi.NewServer(httpapi.ServerConfig{
		Title:       "billing-api",
		Version:     "2.1.0",
		Description: "Billing agents.",
	}))
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", document.OpenAPI)
	}
	if document.Info.Title != "billing-api" || document.Info.Version != "2.1.0" {
		t.Fatalf("info = %+v", document.Info)
	}
	if document.Info.Description != "Billing agents." {
		t.Fatalf("info description = %q", document.Info.Description)
	}
}

func TestOpenAPIDefaultsIdentity(t *testing.T) {
	document := generateDocument(t, httpapi.NewServer(httpapi.ServerConfig{}))
	if document.Info.Title != "lebro" || document.Info.Version != "0.0.0" {
		t.Fatalf("info = %+v, want lebro/0.0.0", document.Info)
	}
}

func TestOpenAPIRouteServesTheSameDocument(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeAgent(newAgent(t, "assistant", &scriptedModel{})))

	generated, err := server.OpenAPI()
	must(t, err)

	recorder := doJSON(t, server.Handler(), http.MethodGet, "/openapi.json", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != string(generated) {
		t.Fatal("served document differs from OpenAPI()")
	}
}

// The package must stay absent from the core dependency graph: an application
// that imports only the root façade must not compile an HTTP server into its
// binary. This is the ticket's first acceptance criterion.
func TestRootModuleDoesNotDependOnHTTPAPI(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", "github.com/tesh254/lebro").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "github.com/tesh254/lebro/httpapi" {
			t.Fatal("root lebro package depends on httpapi; the HTTP server must stay optional")
		}
	}
}

// The package itself must import nothing beyond the standard library and the
// lebro façade; the OpenAPI document is generated by hand for exactly this
// reason. Transitive dependencies are not asserted here because importing the
// façade necessarily pulls in the storage adapters it re-exports — that is a
// property of the root package, not of this one.
func TestHTTPAPIImportsOnlyStandardLibraryAndLebro(t *testing.T) {
	output, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", "github.com/tesh254/lebro/httpapi").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		imported := strings.TrimSpace(line)
		if imported == "" {
			continue
		}
		// A standard library import path's first element has no dot; anything
		// else names a module.
		first, _, _ := strings.Cut(imported, "/")
		if !strings.Contains(first, ".") {
			continue
		}
		if imported == "github.com/tesh254/lebro" {
			continue
		}
		t.Errorf("httpapi imports %q; it must depend only on the standard library and the lebro façade", imported)
	}
}

func schemaNames(document openAPIDocument) []string {
	names := make([]string, 0, len(document.Components.Schemas))
	for name := range document.Components.Schemas {
		names = append(names, name)
	}
	return names
}

func responseStatuses(operation openAPIOperation) []string {
	statuses := make([]string, 0, len(operation.Responses))
	for status := range operation.Responses {
		statuses = append(statuses, status)
	}
	return statuses
}

func contentTypes[T any](content map[string]T) []string {
	types := make([]string, 0, len(content))
	for contentType := range content {
		types = append(types, contentType)
	}
	return types
}
