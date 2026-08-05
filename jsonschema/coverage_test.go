package jsonschema

import (
	"errors"
	"testing"

	engine "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/tesh254/lebro"
)

func TestNormalizeSchemaErrorFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "metaschema error without validation details",
			err:  &engine.SchemaValidationError{Err: errors.New("no details")},
			want: "schema does not satisfy the Draft 2020-12 metaschema",
		},
		{
			name: "unknown compiler error",
			err:  errors.New("unknown"),
			want: "schema could not be compiled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := normalizeSchemaError(tt.err)
			var schemaErr *lebro.SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("error = %T, want *lebro.SchemaError", err)
			}
			if schemaErr.Message != tt.want {
				t.Fatalf("message = %q, want %q", schemaErr.Message, tt.want)
			}
		})
	}
}

func TestPropertyPathFallbacks(t *testing.T) {
	t.Parallel()

	collector := validationIssueCollector{
		instance:        nil,
		propertyPaths:   map[string][][]string{},
		propertyOffsets: map[string]int{},
	}
	fallback := []string{"BadName"}
	if got := collector.nextPropertyPath("schema-without-fragment", "BadName", fallback); len(got) != 1 || got[0] != "BadName" {
		t.Fatalf("fallback path = %q, want %q", got, fallback)
	}

	tests := []struct {
		name      string
		instance  any
		schemaURL string
	}{
		{name: "properties on scalar", instance: 1, schemaURL: "schema#/properties/child/propertyNames"},
		{name: "missing property", instance: map[string]any{}, schemaURL: "schema#/properties/child/propertyNames"},
		{name: "items on scalar", instance: map[string]any{"child": 1}, schemaURL: "schema#/properties/child/items/propertyNames"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if paths := findPropertyPaths(tt.instance, tt.schemaURL, "BadName"); len(paths) != 0 {
				t.Fatalf("paths = %q, want none", paths)
			}
		})
	}

	var issues []lebro.ValidationIssue
	collector.collect(&engine.ValidationError{
		ErrorKind: &kind.PropertyNames{Property: "BadName"},
	}, []string{"outer"}, &issues)
	if len(issues) != 1 || issues[0].Path != "/outer/BadName" {
		t.Fatalf("issues = %#v, want inherited property path", issues)
	}
}
