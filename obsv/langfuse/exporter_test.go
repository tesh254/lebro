package langfuse

import "testing"

func TestNewRequiresKeys(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil")
	}
	if _, err := New(Config{PublicKey: "pk"}); err == nil {
		t.Fatal("New() error = nil")
	}
}

func TestNewBuildsExporter(t *testing.T) {
	exporter, err := New(Config{PublicKey: "pk", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	if exporter == nil {
		t.Fatal("New() exporter = nil")
	}
}
