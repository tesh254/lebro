package jsonschema

import (
	"errors"
	"testing"

	engine "github.com/santhosh-tekuri/jsonschema/v6"
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
