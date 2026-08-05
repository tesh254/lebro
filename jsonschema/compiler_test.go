package jsonschema_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/tesh254/lebro"
	lebrojsonschema "github.com/tesh254/lebro/jsonschema"
)

func TestCompilerValidatesNestedPayloadsDeterministically(t *testing.T) {
	t.Parallel()

	compiler := lebrojsonschema.NewCompiler()
	schema, err := compiler.Compile(json.RawMessage(`{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"required":["request"],
		"properties":{
			"request":{
				"type":"object",
				"required":["count","name"],
				"additionalProperties":false,
				"properties":{
					"count":{"type":"integer","minimum":1},
					"name":{"type":"string"}
				}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(json.RawMessage(`{"request":{"count":2,"name":"Ada"}}`)); err != nil {
		t.Fatalf("valid payload error = %v", err)
	}

	invalid := json.RawMessage(`{"request":{"count":0,"unexpected":true}}`)
	want := []lebro.ValidationIssue{
		{Path: "/request/count", Keyword: "minimum", Message: "minimum: got 0, want 1"},
		{Path: "/request/name", Keyword: "required", Message: "required property is missing"},
		{Path: "/request/unexpected", Keyword: "additionalProperties", Message: "additional property is not allowed"},
	}
	for i := 0; i < 50; i++ {
		validationErr := schema.Validate(invalid)
		if validationErr == nil {
			t.Fatal("invalid payload accepted")
		}
		if !reflect.DeepEqual(validationErr.Issues, want) {
			t.Fatalf("run %d issues = %#v, want %#v", i, validationErr.Issues, want)
		}
	}
}

func TestToolSchemaValidatorLabelsInputAndOutputFailures(t *testing.T) {
	t.Parallel()

	validator, err := lebro.NewToolSchemaValidator(lebrojsonschema.NewCompiler(), lebro.ToolDefinition{
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["customer"],
			"properties":{"customer":{"type":"object","required":["email"],"properties":{"email":{"type":"string"}}}}
		}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := validator.ValidateInput(json.RawMessage(`{"customer":{"email":"ada@example.com"}}`)); err != nil {
		t.Fatalf("valid input error = %v", err)
	}
	if err := validator.ValidateOutput(json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("valid output error = %v", err)
	}

	tests := []struct {
		name       string
		call       func(json.RawMessage) error
		value      json.RawMessage
		wantTarget lebro.ValidationTarget
		wantPath   string
	}{
		{name: "input", call: validator.ValidateInput, value: json.RawMessage(`{"customer":{}}`), wantTarget: lebro.ValidationTargetToolInput, wantPath: "/customer/email"},
		{name: "output", call: validator.ValidateOutput, value: json.RawMessage(`{"ok":"yes"}`), wantTarget: lebro.ValidationTargetToolOutput, wantPath: "/ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(tt.value)
			var validationErr *lebro.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %T, want *lebro.ValidationError", err)
			}
			if validationErr.Target != tt.wantTarget || len(validationErr.Issues) == 0 || validationErr.Issues[0].Path != tt.wantPath {
				t.Fatalf("validation error = %#v, want target %q path %q", validationErr, tt.wantTarget, tt.wantPath)
			}
		})
	}
}

func TestCompilerReturnsTypedSchemaErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		schema   json.RawMessage
		wantPath string
	}{
		{name: "empty", schema: nil},
		{name: "malformed JSON", schema: json.RawMessage(`{`)},
		{name: "non-string dialect", schema: json.RawMessage(`{"$schema":1}`), wantPath: "/$schema"},
		{name: "unsupported dialect", schema: json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#"}`), wantPath: "/$schema"},
		{name: "invalid keyword value", schema: json.RawMessage(`{"type":"not-a-type"}`), wantPath: "/type"},
		{name: "invalid identifier", schema: json.RawMessage(`{"$id":"%"}`)},
		{name: "external reference", schema: json.RawMessage(`{"$ref":"https://example.com/schema.json"}`)},
	}
	compiler := lebrojsonschema.NewCompiler()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compiler.Compile(tt.schema)
			var schemaErr *lebro.SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("error = %T (%v), want *lebro.SchemaError", err, err)
			}
			if schemaErr.Path != tt.wantPath {
				t.Fatalf("path = %q, want %q (error: %v)", schemaErr.Path, tt.wantPath, err)
			}
			if errors.Unwrap(schemaErr) != nil {
				t.Fatal("SchemaError exposes schema-library error")
			}
		})
	}
}

func TestCompilerSupportsBooleanSchemasAndPreciseNumbers(t *testing.T) {
	t.Parallel()

	compiler := lebrojsonschema.NewCompiler()
	allow, err := compiler.Compile(json.RawMessage(`true`))
	if err != nil {
		t.Fatal(err)
	}
	if validationErr := allow.Validate(json.RawMessage(`{"anything":9007199254740993}`)); validationErr != nil {
		t.Fatalf("true schema rejected value: %v", validationErr)
	}

	precise, err := compiler.Compile(json.RawMessage(`{"const":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if validationErr := precise.Validate(json.RawMessage(`9007199254740993`)); validationErr != nil {
		t.Fatalf("precise integer rejected: %v", validationErr)
	}
	if validationErr := precise.Validate(json.RawMessage(`9007199254740992`)); validationErr == nil {
		t.Fatal("different precise integer accepted")
	}

	reject, err := compiler.Compile(json.RawMessage(`false`))
	if err != nil {
		t.Fatal(err)
	}
	if validationErr := reject.Validate(json.RawMessage(`null`)); validationErr == nil {
		t.Fatal("false schema accepted value")
	}
}

func TestCompilerReportsMalformedValuesAndEscapesJSONPointers(t *testing.T) {
	t.Parallel()

	compiler := lebrojsonschema.NewCompiler()
	schema, err := compiler.Compile(json.RawMessage(`{
		"type":"object",
		"properties":{"a/b~c":{"type":"integer"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	malformed := schema.Validate(json.RawMessage(`{`))
	if malformed == nil || len(malformed.Issues) != 1 || malformed.Issues[0].Keyword != "json" || malformed.Issues[0].Path != "" {
		t.Fatalf("malformed JSON error = %#v", malformed)
	}

	validationErr := schema.Validate(json.RawMessage(`{"a/b~c":"wrong"}`))
	if validationErr == nil || len(validationErr.Issues) == 0 || validationErr.Issues[0].Path != "/a~1b~0c" {
		t.Fatalf("escaped pointer error = %#v", validationErr)
	}
}

func TestCompilerSupportsLocalReferencesAndArrayPaths(t *testing.T) {
	t.Parallel()

	schema, err := lebrojsonschema.NewCompiler().Compile(json.RawMessage(`{
		"$defs":{"identifier":{"type":"integer","minimum":1}},
		"type":"array",
		"items":{"$ref":"#/$defs/identifier"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if validationErr := schema.Validate(json.RawMessage(`[1,2,3]`)); validationErr != nil {
		t.Fatalf("valid local references error = %v", validationErr)
	}

	validationErr := schema.Validate(json.RawMessage(`[1,"two",0]`))
	if validationErr == nil {
		t.Fatal("invalid referenced values accepted")
	}
	want := []lebro.ValidationIssue{
		{Path: "/1", Keyword: "type", Message: "got string, want integer"},
		{Path: "/2", Keyword: "minimum", Message: "minimum: got 0, want 1"},
	}
	if !reflect.DeepEqual(validationErr.Issues, want) {
		t.Fatalf("issues = %#v, want %#v", validationErr.Issues, want)
	}
}

func TestCompilerReportsDependentAndPropertyNamePaths(t *testing.T) {
	t.Parallel()

	compiler := lebrojsonschema.NewCompiler()
	tests := []struct {
		name        string
		schema      json.RawMessage
		value       json.RawMessage
		wantPath    string
		wantKeyword string
	}{
		{
			name:        "dependent required",
			schema:      json.RawMessage(`{"dependentRequired":{"credit_card":["billing_address"]}}`),
			value:       json.RawMessage(`{"credit_card":"1234"}`),
			wantPath:    "/billing_address",
			wantKeyword: "dependentRequired",
		},
		{
			name: "nested property name",
			schema: json.RawMessage(`{
				"properties":{
					"payload":{"type":"array","items":{"type":"object","propertyNames":{"pattern":"^[a-z]+$"}}}
				}
			}`),
			value:       json.RawMessage(`{"other":{"BadName":"allowed here"},"payload":[{"BadName":1},{"BadName":2}]}`),
			wantPath:    "/payload/0/BadName",
			wantKeyword: "propertyNames",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema, err := compiler.Compile(tt.schema)
			if err != nil {
				t.Fatal(err)
			}
			validationErr := schema.Validate(tt.value)
			if validationErr == nil {
				t.Fatalf("validation error = nil, want path %q keyword %q", tt.wantPath, tt.wantKeyword)
			}
			found := false
			for _, issue := range validationErr.Issues {
				if issue.Path == tt.wantPath && issue.Keyword == tt.wantKeyword {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("validation error = %#v, want path %q keyword %q", validationErr, tt.wantPath, tt.wantKeyword)
			}
			if tt.name == "nested property name" {
				wantPaths := []string{"/payload/0/BadName", "/payload/1/BadName"}
				var gotPaths []string
				for _, issue := range validationErr.Issues {
					if issue.Keyword == "propertyNames" {
						gotPaths = append(gotPaths, issue.Path)
					}
				}
				if !reflect.DeepEqual(gotPaths, wantPaths) {
					t.Fatalf("propertyNames paths = %q, want %q", gotPaths, wantPaths)
				}
			}
		})
	}
}

func TestCompilerDeterministicallySortsIssuesWithTheSamePathAndKeyword(t *testing.T) {
	t.Parallel()

	schema, err := lebrojsonschema.NewCompiler().Compile(json.RawMessage(`{
		"anyOf":[{"type":"string"},{"type":"integer"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	validationErr := schema.Validate(json.RawMessage(`false`))
	if validationErr == nil || len(validationErr.Issues) != 2 {
		t.Fatalf("validation error = %#v, want two issues", validationErr)
	}
	if validationErr.Issues[0].Message >= validationErr.Issues[1].Message {
		t.Fatalf("issues are not sorted by message: %#v", validationErr.Issues)
	}
}

func TestCompiledSchemaSupportsConcurrentValidation(t *testing.T) {
	t.Parallel()

	schema, err := lebrojsonschema.NewCompiler().Compile(json.RawMessage(`{"type":"array","items":{"type":"integer"}}`))
	if err != nil {
		t.Fatal(err)
	}

	var waitGroup sync.WaitGroup
	for i := 0; i < 100; i++ {
		waitGroup.Add(1)
		go func(valid bool) {
			defer waitGroup.Done()
			value := json.RawMessage(`[1,2,3]`)
			if !valid {
				value = json.RawMessage(`[1,"two",3]`)
			}
			validationErr := schema.Validate(value)
			if valid && validationErr != nil {
				t.Errorf("valid payload error = %v", validationErr)
			}
			if !valid && validationErr == nil {
				t.Error("invalid payload accepted")
			}
		}(i%2 == 0)
	}
	waitGroup.Wait()
}
