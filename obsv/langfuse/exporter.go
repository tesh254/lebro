package langfuse

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/obsv/otlp"
)

// DefaultBaseURL is Langfuse's EU cloud endpoint.
const DefaultBaseURL = "https://cloud.langfuse.com"

// Config configures a Langfuse OTLP/HTTP v4 trace exporter.
type Config struct {
	PublicKey          string
	SecretKey          string
	BaseURL            string
	ServiceName        string
	ResourceAttributes map[string]string
	HTTPClient         *http.Client
}

// Exporter delivers spans through Langfuse's OTLP endpoint.
type Exporter struct{ *otlp.Exporter }

var _ obsv.SpanExporter = (*Exporter)(nil)

// New validates config and returns a Langfuse exporter.
func New(config Config) (*Exporter, error) {
	if strings.TrimSpace(config.PublicKey) == "" || strings.TrimSpace(config.SecretKey) == "" {
		return nil, fmt.Errorf("langfuse: public and secret keys must not be empty")
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	authorization := base64.StdEncoding.EncodeToString([]byte(config.PublicKey + ":" + config.SecretKey))
	exporter, err := otlp.New(otlp.Config{Endpoint: baseURL + "/api/public/otel/v1/traces", Headers: map[string]string{"Authorization": "Basic " + authorization, "x-langfuse-ingestion-version": "4"}, ServiceName: config.ServiceName, ResourceAttributes: config.ResourceAttributes, HTTPClient: config.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("langfuse: %w", err)
	}
	return &Exporter{Exporter: exporter}, nil
}
