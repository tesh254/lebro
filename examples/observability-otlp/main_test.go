package main

import (
	"testing"

	"github.com/tesh254/lebro/obsv/otlp"
)

func TestExample(t *testing.T) { main() }

func TestProviderConfigurations(t *testing.T) {
	t.Setenv("AXIOM_DOMAIN", "api.axiom.co")
	t.Setenv("AXIOM_TOKEN", "token")
	t.Setenv("AXIOM_DATASET", "traces")
	t.Setenv("DATADOG_OTLP_TRACES_ENDPOINT", "https://otlp.datadoghq.com/v1/traces")
	t.Setenv("DD_API_KEY", "key")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-lf")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-lf")
	t.Setenv("LANGSMITH_API_KEY", "key")
	t.Setenv("LANGSMITH_PROJECT", "support")

	tests := []struct {
		name     string
		config   func(string) otlp.Config
		endpoint string
		header   string
		value    string
	}{
		{"axiom", axiomConfig, "https://api.axiom.co/v1/traces", "X-Axiom-Dataset", "traces"},
		{"datadog", datadogConfig, "https://otlp.datadoghq.com/v1/traces", "compute_stats", "true"},
		{"langfuse", langfuseConfig, "https://cloud.langfuse.com/api/public/otel/v1/traces", "x-langfuse-ingestion-version", "4"},
		{"langsmith", langsmithConfig, "https://api.smith.langchain.com/otel/v1/traces", "Langsmith-Project", "support"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := test.config("support-agent")
			if config.Endpoint != test.endpoint {
				t.Errorf("endpoint = %q, want %q", config.Endpoint, test.endpoint)
			}
			if config.Headers[test.header] != test.value {
				t.Errorf("header %q = %q, want %q", test.header, config.Headers[test.header], test.value)
			}
		})
	}
}
