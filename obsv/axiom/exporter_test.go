package axiom

import (
	"testing"
)

func TestNewRequiresConfig(t *testing.T) {
	for _, config := range []Config{{}, {Domain: "api.axiom.co"}, {Domain: "api.axiom.co", Token: "token"}} {
		if _, err := New(config); err == nil {
			t.Errorf("New(%+v) error = nil", config)
		}
	}
}

func TestNewBuildsTraceEndpoint(t *testing.T) {
	exporter, err := New(Config{Domain: "https://api.axiom.co/", Token: "token", Dataset: "traces"})
	if err != nil {
		t.Fatal(err)
	}
	if exporter == nil {
		t.Fatal("New() exporter = nil")
	}
}
