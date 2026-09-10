// Package openrouter implements OpenRouter's dedicated media endpoints.
// Chat completions continue to use openai.New with an OpenRouter BaseURL.
package openrouter

import "github.com/tesh254/lebro/internal/mediahttp"

type MediaConfig = mediahttp.Config

// Media implements image/video generation, batch transcription and streaming
// speech according to the configured model's Capabilities. Live STT and remote
// video cancellation are unsupported by this baseline adapter.
type Media struct{ *mediahttp.Client }

func NewMedia(config MediaConfig) (*Media, error) {
	c, err := mediahttp.New(config, "openrouter")
	if err != nil {
		return nil, err
	}
	return &Media{Client: c}, nil
}
