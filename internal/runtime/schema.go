package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// JSONSchemaDraft202012 is the JSON Schema dialect supported by the bundled
// validator adapter. Schemas that omit $schema are interpreted as this draft.
const JSONSchemaDraft202012 = "https://json-schema.org/draft/2020-12/schema"

// SchemaCompiler separates the Tool API from a concrete JSON Schema engine.
// Implementations should return SchemaError for invalid or unsupported schemas
// and must be safe for concurrent calls.
type SchemaCompiler interface {
	Compile(json.RawMessage) (CompiledSchema, error)
}

// CompiledSchema validates JSON-compatible values against a reusable schema.
// Implementations return only library-neutral ValidationError values and must
// be safe for concurrent calls.
type CompiledSchema interface {
	Validate(json.RawMessage) *ValidationError
}

// ValidationTarget identifies which side of a tool boundary failed validation.
type ValidationTarget string

const (
	ValidationTargetToolInput        ValidationTarget = "tool_input"
	ValidationTargetToolOutput       ValidationTarget = "tool_output"
	ValidationTargetStructuredOutput ValidationTarget = "structured_output"
)

// ValidationIssue describes one JSON Schema violation. Path is an RFC 6901
// JSON Pointer; an empty path identifies the root value.
type ValidationIssue struct {
	Path    string
	Keyword string
	Message string
}

// ValidationError is returned when a JSON value does not satisfy its schema.
// Issues are sorted deterministically by path, keyword, and message.
type ValidationError struct {
	Target ValidationTarget
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}

	target := strings.ReplaceAll(string(e.Target), "_", " ")
	if target == "" {
		target = "JSON value"
	}
	if len(e.Issues) == 0 {
		return fmt.Sprintf("lebro: %s validation failed", target)
	}

	issues := sortedValidationIssues(e.Issues)
	issue := issues[0]
	path := issue.Path
	if path == "" {
		path = "/"
	}
	if len(issues) == 1 {
		return fmt.Sprintf("lebro: %s validation failed at %s: %s", target, path, issue.Message)
	}
	return fmt.Sprintf("lebro: %s validation failed at %s: %s (and %d more)", target, path, issue.Message, len(issues)-1)
}

// SchemaError reports a malformed or unsupported schema without exposing the
// concrete schema engine's error types. Path is an RFC 6901 JSON Pointer.
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return "lebro: invalid JSON Schema: " + e.Message
	}
	return fmt.Sprintf("lebro: invalid JSON Schema at %s: %s", e.Path, e.Message)
}

// ToolSchemaValidator compiles and validates the schema boundary described by
// a ToolDefinition. It does not execute a tool; execution policy is layered on
// top so invalid input can be rejected before a handler is invoked and invalid
// output can be rejected before it is returned to a model.
type ToolSchemaValidator struct {
	input  CompiledSchema
	output CompiledSchema
}

// NewToolSchemaValidator compiles the non-empty schemas in definition once.
// An omitted schema disables validation for that side of the boundary.
func NewToolSchemaValidator(compiler SchemaCompiler, definition ToolDefinition) (*ToolSchemaValidator, error) {
	if compiler == nil {
		return nil, errors.New("lebro: schema compiler is required")
	}

	validator := &ToolSchemaValidator{}
	var err error
	if len(definition.InputSchema) != 0 {
		validator.input, err = compiler.Compile(definition.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("lebro: compile tool input schema: %w", err)
		}
		if validator.input == nil || isNilInterface(validator.input) {
			return nil, errors.New("lebro: schema compiler returned a nil input schema")
		}
	}
	if len(definition.OutputSchema) != 0 {
		validator.output, err = compiler.Compile(definition.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("lebro: compile tool output schema: %w", err)
		}
		if validator.output == nil || isNilInterface(validator.output) {
			return nil, errors.New("lebro: schema compiler returned a nil output schema")
		}
	}

	return validator, nil
}

// ValidateInput validates a value before tool execution.
func (v *ToolSchemaValidator) ValidateInput(value json.RawMessage) error {
	return validateToolBoundary(v, ValidationTargetToolInput, value)
}

// ValidateOutput validates a value before it is returned to the model.
func (v *ToolSchemaValidator) ValidateOutput(value json.RawMessage) error {
	return validateToolBoundary(v, ValidationTargetToolOutput, value)
}

func validateToolBoundary(v *ToolSchemaValidator, target ValidationTarget, value json.RawMessage) error {
	if v == nil {
		return errors.New("lebro: tool schema validator is nil")
	}

	schema := v.input
	if target == ValidationTargetToolOutput {
		schema = v.output
	}
	if schema == nil {
		return nil
	}

	validationErr := schema.Validate(value)
	if validationErr == nil {
		return nil
	}
	return &ValidationError{
		Target: target,
		Issues: sortedValidationIssues(validationErr.Issues),
	}
}

func sortedValidationIssues(issues []ValidationIssue) []ValidationIssue {
	sorted := append([]ValidationIssue(nil), issues...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		if sorted[i].Keyword != sorted[j].Keyword {
			return sorted[i].Keyword < sorted[j].Keyword
		}
		return sorted[i].Message < sorted[j].Message
	})

	if len(sorted) < 2 {
		return sorted
	}
	deduplicated := sorted[:1]
	for _, issue := range sorted[1:] {
		if issue != deduplicated[len(deduplicated)-1] {
			deduplicated = append(deduplicated, issue)
		}
	}
	return deduplicated
}
