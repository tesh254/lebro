package lebro

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type stubSchemaCompiler struct {
	compile func(json.RawMessage) (CompiledSchema, error)
}

func (c stubSchemaCompiler) Compile(schema json.RawMessage) (CompiledSchema, error) {
	return c.compile(schema)
}

type stubCompiledSchema struct {
	validationErr *ValidationError
}

func (s stubCompiledSchema) Validate(json.RawMessage) *ValidationError {
	return s.validationErr
}

type pointerCompiledSchema struct{}

func (*pointerCompiledSchema) Validate(json.RawMessage) *ValidationError { return nil }

func TestToolSchemaValidatorUsesReplaceableCompiler(t *testing.T) {
	t.Parallel()

	var compiled []string
	compiler := stubSchemaCompiler{compile: func(schema json.RawMessage) (CompiledSchema, error) {
		compiled = append(compiled, string(schema))
		return stubCompiledSchema{validationErr: &ValidationError{Issues: []ValidationIssue{
			{Path: "/z", Keyword: "type", Message: "wrong type"},
			{Path: "/a", Keyword: "required", Message: "required property is missing"},
			{Path: "/a", Keyword: "required", Message: "required property is missing"},
		}}}, nil
	}}

	validator, err := NewToolSchemaValidator(compiler, ToolDefinition{
		InputSchema:  json.RawMessage(`{"title":"input"}`),
		OutputSchema: json.RawMessage(`{"title":"output"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"title":"input"}`, `{"title":"output"}`}; !reflect.DeepEqual(compiled, want) {
		t.Fatalf("compiled schemas = %q, want %q", compiled, want)
	}

	tests := []struct {
		name   string
		call   func(json.RawMessage) error
		target ValidationTarget
	}{
		{name: "input", call: validator.ValidateInput, target: ValidationTargetToolInput},
		{name: "output", call: validator.ValidateOutput, target: ValidationTargetToolOutput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(json.RawMessage(`null`))
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T, want *ValidationError", err)
			}
			if validationErr.Target != tt.target {
				t.Fatalf("target = %q, want %q", validationErr.Target, tt.target)
			}
			wantIssues := []ValidationIssue{
				{Path: "/a", Keyword: "required", Message: "required property is missing"},
				{Path: "/z", Keyword: "type", Message: "wrong type"},
			}
			if !reflect.DeepEqual(validationErr.Issues, wantIssues) {
				t.Fatalf("issues = %#v, want %#v", validationErr.Issues, wantIssues)
			}
		})
	}
}

func TestToolSchemaValidatorAllowsOmittedSchemas(t *testing.T) {
	t.Parallel()

	compilerCalled := false
	validator, err := NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		compilerCalled = true
		return nil, nil
	}}, ToolDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	if compilerCalled {
		t.Fatal("compiler called for omitted schemas")
	}
	if err := validator.ValidateInput(json.RawMessage(`not JSON`)); err != nil {
		t.Fatalf("ValidateInput() error = %v for omitted schema", err)
	}
	if err := validator.ValidateOutput(json.RawMessage(`not JSON`)); err != nil {
		t.Fatalf("ValidateOutput() error = %v for omitted schema", err)
	}
}

func TestNewToolSchemaValidatorReportsCompileFailures(t *testing.T) {
	t.Parallel()

	schemaErr := &SchemaError{Path: "/type", Message: "invalid type"}
	_, err := NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return nil, schemaErr
	}}, ToolDefinition{InputSchema: json.RawMessage(`{}`)})
	if !errors.Is(err, schemaErr) {
		t.Fatalf("error = %v, want wrapped schema error", err)
	}

	_, err = NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return nil, nil
	}}, ToolDefinition{InputSchema: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("nil compiled schema accepted")
	}

	var typedNilInput *pointerCompiledSchema
	if _, err := NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return typedNilInput, nil
	}}, ToolDefinition{InputSchema: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("typed nil compiled input schema accepted")
	}

	var typedNilOutput *pointerCompiledSchema
	if _, err := NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return typedNilOutput, nil
	}}, ToolDefinition{OutputSchema: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("typed nil compiled output schema accepted")
	}

	if _, err := NewToolSchemaValidator(nil, ToolDefinition{}); err == nil {
		t.Fatal("nil compiler accepted")
	}

	compileOutput := false
	_, err = NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		if !compileOutput {
			compileOutput = true
			return stubCompiledSchema{}, nil
		}
		return nil, schemaErr
	}}, ToolDefinition{InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)})
	if !errors.Is(err, schemaErr) {
		t.Fatalf("output compile error = %v, want wrapped schema error", err)
	}

	compileOutput = false
	_, err = NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		if !compileOutput {
			compileOutput = true
			return stubCompiledSchema{}, nil
		}
		return nil, nil
	}}, ToolDefinition{InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("nil compiled output schema accepted")
	}
}

func TestValidationAndSchemaErrorMessages(t *testing.T) {
	t.Parallel()

	validationErr := &ValidationError{
		Target: ValidationTargetToolInput,
		Issues: []ValidationIssue{
			{Path: "/second", Message: "second error"},
			{Path: "/first", Message: "first error"},
		},
	}
	if got, want := validationErr.Error(), "lebro: tool input validation failed at /first: first error (and 1 more)"; got != want {
		t.Fatalf("ValidationError.Error() = %q, want %q", got, want)
	}

	rootErr := &ValidationError{Issues: []ValidationIssue{{Message: "invalid"}}}
	if got, want := rootErr.Error(), "lebro: JSON value validation failed at /: invalid"; got != want {
		t.Fatalf("root ValidationError.Error() = %q, want %q", got, want)
	}

	schemaErr := &SchemaError{Path: "/properties/name/type", Message: "invalid type"}
	if got, want := schemaErr.Error(), "lebro: invalid JSON Schema at /properties/name/type: invalid type"; got != want {
		t.Fatalf("SchemaError.Error() = %q, want %q", got, want)
	}

	var nilValidationErr *ValidationError
	if got := nilValidationErr.Error(); got != "" {
		t.Fatalf("nil ValidationError.Error() = %q", got)
	}
	if got, want := (&ValidationError{Target: ValidationTargetToolOutput}).Error(), "lebro: tool output validation failed"; got != want {
		t.Fatalf("empty ValidationError.Error() = %q, want %q", got, want)
	}

	var nilSchemaErr *SchemaError
	if got := nilSchemaErr.Error(); got != "" {
		t.Fatalf("nil SchemaError.Error() = %q", got)
	}
	if got, want := (&SchemaError{Message: "invalid"}).Error(), "lebro: invalid JSON Schema: invalid"; got != want {
		t.Fatalf("root SchemaError.Error() = %q, want %q", got, want)
	}
}

func TestToolSchemaValidatorAcceptsValidValuesAndSortsIssueTies(t *testing.T) {
	t.Parallel()

	valid, err := NewToolSchemaValidator(stubSchemaCompiler{compile: func(json.RawMessage) (CompiledSchema, error) {
		return stubCompiledSchema{}, nil
	}}, ToolDefinition{InputSchema: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := valid.ValidateInput(json.RawMessage(`null`)); err != nil {
		t.Fatalf("valid input error = %v", err)
	}

	issues := sortedValidationIssues([]ValidationIssue{
		{Path: "/same", Keyword: "type", Message: "z"},
		{Path: "/same", Keyword: "required", Message: "x"},
		{Path: "/same", Keyword: "type", Message: "a"},
	})
	want := []ValidationIssue{
		{Path: "/same", Keyword: "required", Message: "x"},
		{Path: "/same", Keyword: "type", Message: "a"},
		{Path: "/same", Keyword: "type", Message: "z"},
	}
	if !reflect.DeepEqual(issues, want) {
		t.Fatalf("sorted issues = %#v, want %#v", issues, want)
	}
}

func TestNilToolSchemaValidator(t *testing.T) {
	t.Parallel()

	var validator *ToolSchemaValidator
	if err := validator.ValidateInput(json.RawMessage(`null`)); err == nil {
		t.Fatal("nil ToolSchemaValidator accepted input")
	}
	if err := validator.ValidateOutput(json.RawMessage(`null`)); err == nil {
		t.Fatal("nil ToolSchemaValidator accepted output")
	}
}
