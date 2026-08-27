// Package gemini adapts Gemini Developer API models to lebro.Model.
package gemini

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/internal/geminiapi"
	genai "google.golang.org/genai"
)

const providerName = "gemini"

// Config configures a Gemini Developer API adapter. APIKey is required.
type Config struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// Model implements lebro.Model and lebro.StreamingModel using the Gemini
// Developer API.
type Model struct {
	*geminiapi.Model
}

var _ lebro.Model = (*Model)(nil)
var _ lebro.StreamingModel = (*Model)(nil)

// New creates a Gemini Developer API adapter safe for concurrent use.
func New(config Config) (*Model, error) {
	if config.APIKey == "" {
		return nil, errors.New("lebro: API key is required")
	}
	options := genai.HTTPOptions{BaseURL: config.BaseURL}
	if config.Timeout > 0 {
		options.Timeout = &config.Timeout
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: config.APIKey, Backend: genai.BackendGeminiAPI, HTTPClient: config.HTTPClient, HTTPOptions: options})
	if err != nil {
		return nil, err
	}
	shared, err := geminiapi.New(geminiapi.Config{Provider: providerName, Client: client, Model: config.Model})
	if err != nil {
		return nil, err
	}
	return &Model{Model: shared}, nil
}
