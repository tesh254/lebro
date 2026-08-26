// Package vertexai adapts Google Vertex AI (Gemini Enterprise Agent
// Platform) models to lebro.Model.
package vertexai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/geminiapi"
	genai "google.golang.org/genai"
)

const providerName = "vertexai"

// Config configures a Vertex AI adapter. Authentication always uses Google
// Application Default Credentials (ADC); there is deliberately no API key
// field.
type Config struct {
	// Project is the Google Cloud project ID. Required.
	Project string
	// Location is the Vertex AI location, defaulting to "global" when empty.
	Location string
	// Model is the Vertex-hosted Gemini model, for example
	// "gemini-2.5-flash". Required.
	Model string
	// BaseURL overrides the Vertex AI endpoint. Intended for tests and
	// proxies; production callers leave it empty.
	BaseURL string
	// HTTPClient overrides the transport used for Vertex AI requests. When
	// set, ADC is not applied and the caller is responsible for
	// authentication.
	HTTPClient *http.Client
	// Timeout bounds each request when greater than zero.
	Timeout time.Duration
}

// Model implements lebro.Model and lebro.StreamingModel using Google Vertex
// AI with Application Default Credentials.
type Model struct {
	*geminiapi.Model
}

var _ lebro.Model = (*Model)(nil)
var _ lebro.StreamingModel = (*Model)(nil)

// New creates a Vertex AI adapter safe for concurrent use. It authenticates
// with Application Default Credentials; run `gcloud auth application-default
// login` (or attach a service account) before calling it. The Vertex AI API
// must be enabled and the principal needs roles/aiplatform.user.
func New(config Config) (*Model, error) {
	if config.Project == "" {
		return nil, errors.New("lebro: project is required")
	}
	if config.Model == "" {
		return nil, errors.New("lebro: model is required")
	}
	location := config.Location
	if location == "" {
		location = "global"
	}
	options := genai.HTTPOptions{BaseURL: config.BaseURL, APIVersion: "v1"}
	if config.Timeout > 0 {
		options.Timeout = &config.Timeout
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{Backend: genai.BackendVertexAI, Project: config.Project, Location: location, HTTPClient: config.HTTPClient, HTTPOptions: options})
	if err != nil {
		return nil, fmt.Errorf("lebro: vertexai: %w", err)
	}
	return &Model{Model: geminiapi.New(geminiapi.Config{Provider: providerName, Client: client, Model: config.Model})}, nil
}
