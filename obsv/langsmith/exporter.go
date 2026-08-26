package langsmith

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/obsv/otlp"
)

// DefaultEndpoint is LangSmith's US SaaS OTLP/HTTP traces endpoint.
const DefaultEndpoint = "https://api.smith.langchain.com/otel/v1/traces"

// Config configures a LangSmith OTLP/HTTP trace exporter.
type Config struct {
	// APIKey authenticates requests to LangSmith.
	APIKey string
	// Project selects a LangSmith project. Empty selects LangSmith's default.
	Project string
	// Endpoint overrides DefaultEndpoint for a regional or self-hosted instance.
	Endpoint           string
	ServiceName        string
	ResourceAttributes map[string]string
	HTTPClient         *http.Client
}

// Exporter delivers spans through LangSmith's OTLP endpoint.
type Exporter struct{ *otlp.Exporter }

var _ obsv.SpanExporter = (*Exporter)(nil)

// New validates config and returns a LangSmith exporter.
func New(config Config) (*Exporter, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("langsmith: API key must not be empty")
	}
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	headers := map[string]string{"x-api-key": config.APIKey}
	if config.Project != "" {
		headers["Langsmith-Project"] = config.Project
	}
	exporter, err := otlp.New(otlp.Config{Endpoint: endpoint, Headers: headers, ServiceName: config.ServiceName, ResourceAttributes: config.ResourceAttributes, HTTPClient: config.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("langsmith: %w", err)
	}
	return &Exporter{Exporter: exporter}, nil
}
