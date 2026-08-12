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
	if len(documented) != 9 {
		t.Errorf("documented routes = %d, want 9: %v", len(documented), documented)
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
// produce an unreferenceable schema name.
func TestOpenAPISanitizesWorkflowSchemaNames(t *testing.T) {
	server := httpapi.NewServer(httpapi.ServerConfig{})
	must(t, server.ExposeWorkflow(newEchoWorkflow(t, "billing.invoice/sync")))
	document := generateDocument(t, server)

	name := "WorkflowRunRequest_billing_invoice_sync"
	if _, ok := document.Components.Schemas[name]; !ok {
		t.Fatalf("sanitized schema %q missing; schemas = %v", name, schemaNames(document))
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

var _ = lebro.RunStatusSucceeded
