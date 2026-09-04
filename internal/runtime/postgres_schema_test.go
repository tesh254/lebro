package runtime

import "testing"

func TestValidatePostgresSchema(t *testing.T) {
	for _, schema := range []string{"", "lebro", "control_plane_2"} {
		if err := validatePostgresSchema(schema); err != nil {
			t.Fatalf("%q: %v", schema, err)
		}
	}
	for _, schema := range []string{"public;drop", "two.names", "9starts", "UPPER"} {
		if err := validatePostgresSchema(schema); err == nil {
			t.Fatalf("%q accepted", schema)
		}
	}
}
