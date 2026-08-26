package axiom

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/obsv/otlp"
)

// Config configures an Axiom OTLP/HTTP trace exporter.
type Config struct {
	Domain             string
	Token              string
	Dataset            string
	ServiceName        string
	ResourceAttributes map[string]string
	HTTPClient         *http.Client
}

// Exporter delivers spans through Axiom's OTLP endpoint.
type Exporter struct{ *otlp.Exporter }

var _ obsv.SpanExporter = (*Exporter)(nil)

// New validates config and returns an Axiom exporter.
func New(config Config) (*Exporter, error) {
	if strings.TrimSpace(config.Domain) == "" {
		return nil, fmt.Errorf("axiom: domain must not be empty")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, fmt.Errorf("axiom: token must not be empty")
	}
	if strings.TrimSpace(config.Dataset) == "" {
		return nil, fmt.Errorf("axiom: dataset must not be empty")
	}
	domain := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(config.Domain), "/"), "https://")
	domain = strings.TrimPrefix(domain, "http://")
	exporter, err := otlp.New(otlp.Config{Endpoint: "https://" + domain + "/v1/traces", Headers: map[string]string{"Authorization": "Bearer " + config.Token, "X-Axiom-Dataset": config.Dataset}, ServiceName: config.ServiceName, ResourceAttributes: config.ResourceAttributes, HTTPClient: config.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("axiom: %w", err)
	}
	return &Exporter{Exporter: exporter}, nil
}
