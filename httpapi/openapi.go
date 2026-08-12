package httpapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// openAPIVersion is the specification version of the generated document.
// OpenAPI 3.1 is used because its schema dialect is JSON Schema 2020-12, the
// same dialect lebro's tool and workflow schemas are written in — so a
// workflow's declared input schema can be embedded verbatim rather than
// translated into an older, lossy subset.
const openAPIVersion = "3.1.0"

// OpenAPI returns the OpenAPI 3.1 document describing every route this server
// serves. It is generated from the same route table that builds the router, so
// the document cannot omit a served route or describe one that is absent.
//
// The document reflects what is currently exposed: registered agents and
// workflows are enumerated in their operations' descriptions, and each
// workflow's declared input schema is embedded in a per-workflow request body
// schema, so a client can see exactly what a given workflow accepts.
func (s *Server) OpenAPI() ([]byte, error) {
	document := map[string]any{
		"openapi": openAPIVersion,
		"info":    s.openAPIInfo(),
		"paths":   s.openAPIPaths(),
		"components": map[string]any{
			"schemas": s.openAPISchemas(),
		},
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("lebro/httpapi: encode OpenAPI document: %w", err)
	}
	return encoded, nil
}

func (s *Server) openAPIInfo() map[string]any {
	info := map[string]any{
		"title":   s.config.Title,
		"version": s.config.Version,
	}
	if s.config.Description != "" {
		info["description"] = s.config.Description
	}
	return info
}

// openAPIPaths renders the route table as an OpenAPI paths object. Multiple
// methods on one path merge into a single path item, which is what the
// specification requires.
func (s *Server) openAPIPaths() map[string]any {
	paths := map[string]any{}
	for _, r := range routes() {
		item, ok := paths[r.path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[r.path] = item
		}
		item[strings.ToLower(r.method)] = s.openAPIOperation(r)
	}
	return paths
}

// openAPIOperation renders one route as an OpenAPI operation object.
func (s *Server) openAPIOperation(r route) map[string]any {
	operation := map[string]any{
		"operationId": r.operationID,
		"summary":     r.summary,
		"description": s.operationDescription(r),
		"responses":   s.openAPIResponses(r),
	}

	parameters := make([]any, 0, len(r.pathParams)+len(r.queryParams))
	for _, p := range r.pathParams {
		parameters = append(parameters, openAPIParameter(p, "path"))
	}
	for _, p := range r.queryParams {
		parameters = append(parameters, openAPIParameter(p, "query"))
	}
	if len(parameters) > 0 {
		operation["parameters"] = parameters
	}

	if r.requestBody != "" {
		operation["requestBody"] = map[string]any{
			"required": false,
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": s.requestBodySchema(r),
				},
			},
		}
	}
	return operation
}

// operationDescription augments a route's static description with what is
// currently exposed, so the generated document tells a client which identifiers
// are actually routable rather than only describing the shape of the path.
func (s *Server) operationDescription(r route) string {
	description := r.description
	switch r.operationID {
	case "createAgentRun", "streamAgentRun":
		if ids := summaryIDs(s.agentSummaries()); len(ids) > 0 {
			description += "\n\nExposed agents: " + strings.Join(ids, ", ") + "."
		} else {
			description += "\n\nNo agents are currently exposed."
		}
	case "createWorkflowRun":
		if ids := workflowIDs(s.workflowSummaries()); len(ids) > 0 {
			description += "\n\nExposed workflows: " + strings.Join(ids, ", ") + "."
		} else {
			description += "\n\nNo workflows are currently exposed."
		}
	}
	return description
}

// requestBodySchema returns the schema reference for a route's request body.
//
// The workflow run body is special-cased: each exposed workflow declares its
// own first-step input schema, so a single shared schema would document the
// route as accepting any JSON when in practice each workflow accepts one
// specific shape. A oneOf over per-workflow schemas keeps the published
// contract as precise as the runtime validation actually is.
func (s *Server) requestBodySchema(r route) any {
	if r.requestBody != schemaNameWorkflowRunRequest {
		return schemaRef(r.requestBody)
	}
	variants := s.workflowRequestVariants()
	if len(variants) == 0 {
		return schemaRef(schemaNameWorkflowRunRequest)
	}
	return map[string]any{"oneOf": variants}
}

// workflowRequestVariants builds one request-body variant per exposed workflow
// that declares an input schema. A workflow without one is covered by the
// generic body, so it contributes no variant.
func (s *Server) workflowRequestVariants() []any {
	summaries := s.workflowSummaries()
	variants := make([]any, 0, len(summaries)+1)
	generic := false
	for _, summary := range summaries {
		if len(summary.InputSchema) == 0 {
			generic = true
			continue
		}
		variants = append(variants, schemaRef(workflowSchemaName(summary.ID)))
	}
	if len(variants) == 0 {
		return nil
	}
	if generic {
		variants = append(variants, schemaRef(schemaNameWorkflowRunRequest))
	}
	return variants
}

// openAPIResponses renders the success and error responses for a route. Every
// error code the route declares becomes a documented status, so a client can
// enumerate what it must handle without reading the handler.
func (s *Server) openAPIResponses(r route) map[string]any {
	responses := map[string]any{}

	if r.streaming {
		responses["200"] = map[string]any{
			"description": "An ordered Server-Sent Events stream. Each event's data is a StreamEvent; the stream ends with exactly one terminal event.",
			"content": map[string]any{
				"text/event-stream": map[string]any{
					"schema": schemaRef(schemaNameStreamEvent),
				},
			},
		}
	} else {
		responses["200"] = map[string]any{
			"description": "Success.",
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": schemaRef(r.responseBody),
				},
			},
		}
	}

	// Collect statuses first: several codes share a status, and the
	// specification allows only one response object per status.
	statuses := map[int][]ErrorCode{}
	for _, code := range r.errorCodes {
		status := statusFor(code)
		statuses[status] = append(statuses[status], code)
	}
	for status, codes := range statuses {
		sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
		names := make([]string, 0, len(codes))
		for _, code := range codes {
			names = append(names, string(code))
		}
		responses[fmt.Sprintf("%d", status)] = map[string]any{
			"description": "Error codes: " + strings.Join(names, ", ") + ".",
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": schemaRef(schemaNameErrorResponse),
				},
			},
		}
	}
	return responses
}

// openAPISchemas renders the component schemas, including one per exposed
// workflow that declares an input schema.
func (s *Server) openAPISchemas() map[string]any {
	schemas := make(map[string]any, len(componentSchemas))
	for name, schema := range componentSchemas {
		schemas[name] = schema
	}
	for _, summary := range s.workflowSummaries() {
		if len(summary.InputSchema) == 0 {
			continue
		}
		schemas[workflowSchemaName(summary.ID)] = workflowRequestSchema(summary)
	}
	return schemas
}

// workflowRequestSchema builds the request body schema for one workflow,
// embedding its declared first-step input schema. The embedded schema is
// JSON Schema 2020-12, the dialect OpenAPI 3.1 uses, so it needs no
// translation.
func workflowRequestSchema(summary WorkflowSummary) map[string]any {
	return map[string]any{
		"type":        "object",
		"title":       "Run input for workflow " + summary.ID,
		"description": "Input for the " + summary.ID + " workflow. The input property is validated against the workflow's first step schema before the run starts.",
		"properties": map[string]any{
			"input": summary.InputSchema,
			"metadata": map[string]any{
				"type":                 "object",
				"description":          "Caller metadata carried through to step execution and run events.",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required":             []string{"input"},
		"additionalProperties": false,
	}
}

// workflowSchemaName is the component name for a workflow's request schema.
// Characters outside the OpenAPI component-name character set are replaced so a
// workflow ID containing a dot or slash cannot produce an unreferenceable name.
func workflowSchemaName(id string) string {
	var b strings.Builder
	b.WriteString("WorkflowRunRequest_")
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// openAPIParameter renders one path or query parameter. A path parameter is
// always required, which the specification mandates regardless of how the
// route table declares it.
func openAPIParameter(p param, in string) map[string]any {
	schema, ok := inlineParameterSchemas[p.schema]
	if !ok {
		schema = inlineParameterSchemas[schemaNameString]
	}
	return map[string]any{
		"name":        p.name,
		"in":          in,
		"required":    in == "path" || p.required,
		"description": p.description,
		"schema":      schema,
	}
}

// schemaRef renders a reference to a component schema.
func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// summaryIDs lists agent identifiers in order.
func summaryIDs(summaries []AgentSummary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	return ids
}

// workflowIDs lists workflow identifiers in order.
func workflowIDs(summaries []WorkflowSummary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	return ids
}
