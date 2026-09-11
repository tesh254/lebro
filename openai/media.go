package openai

import "github.com/tesh254/lebro/internal/mediahttp"

// MediaConfig selects one media model and its transport. Configure separate
// instances for image, file/live transcription, and speech models.
type MediaConfig = mediahttp.Config

// Media implements optional media interfaces independently of the chat Model.
// Consult Capabilities before selecting an operation. Sora video is deprecated;
// use a supported video adapter such as openrouter.Media.
type Media struct{ *mediahttp.Client }

func NewMedia(config MediaConfig) (*Media, error) {
	c, err := mediahttp.New(config, "openai")
	if err != nil {
		return nil, err
	}
	return &Media{Client: c}, nil
}
