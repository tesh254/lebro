package mediahttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"time"

	"github.com/tesh254/lebro"
)

func (c *Client) GenerateImages(ctx context.Context, r lebro.ImageRequest) (result lebro.ImageResult, retErr error) {
	if err := c.authorize(ctx, r.Operation, "media.generate"); err != nil {
		return result, err
	}
	start := time.Now().UTC()
	defer func() { retErr = errors.Join(retErr, c.record(ctx, result.Info, r.Operation, "image", start, retErr)) }()
	if !c.caps.Image {
		return result, unsupported("image generation is unsupported")
	}
	if err := c.text(r.Operation, r.Prompt); err != nil {
		return result, err
	}
	if r.Count == 0 {
		r.Count = 1
	}
	if r.Count < 1 || r.Count > c.caps.MaxCount {
		return result, invalid("invalid image count")
	}
	for _, pair := range []struct {
		v      string
		values []string
	}{{r.Size, c.caps.Sizes}, {r.Resolution, c.caps.Resolutions}, {r.AspectRatio, c.caps.AspectRatios}, {r.Quality, c.caps.Qualities}, {r.Format, c.caps.OutputFormats}} {
		if err := allowed(pair.v, pair.values); err != nil {
			return result, err
		}
	}
	if r.Size != "" && (r.Resolution != "" || r.AspectRatio != "") {
		return result, invalid("size conflicts with resolution/aspect ratio")
	}
	payload := map[string]any{"model": c.model, "prompt": r.Prompt, "n": r.Count}
	for k, v := range map[string]string{"size": r.Size, "resolution": r.Resolution, "aspect_ratio": r.AspectRatio, "quality": r.Quality, "output_format": r.Format} {
		if v != "" {
			payload[k] = v
		}
	}
	path := "/images/generations"
	if c.provider == "openrouter" {
		path = "/images"
		payload["provider"] = map[string]any{"allow_fallbacks": false}
	}
	var response struct {
		Data []struct {
			B64     string `json:"b64_json"`
			URL     string `json:"url"`
			MIME    string `json:"media_type"`
			Revised string `json:"revised_prompt"`
		} `json:"data"`
		Usage map[string]json.RawMessage `json:"usage"`
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	h, err := c.json(ctx, http.MethodPost, path, payload, &response)
	if err != nil {
		return result, err
	}
	result.Info = c.info(r.Operation, h)
	result.Info.Usage = usage(response.Usage)
	if len(response.Data) == 0 {
		return result, malformed("image response contains no artifacts")
	}
	if len(response.Data) > r.Count {
		return result, malformed("too many image artifacts")
	}
	for _, d := range response.Data {
		a := lebro.MediaAsset{Kind: lebro.MediaImage, Provider: c.provider, Model: c.model, ProviderRequestID: result.Info.ProviderRequestID}
		if d.B64 == "" || d.URL != "" {
			return result, malformed("expected inline generated image")
		}
		b, e := base64.StdEncoding.DecodeString(d.B64)
		if e != nil || len(b) == 0 {
			return result, malformed("invalid image encoding")
		}
		mime := http.DetectContentType(b)
		if d.MIME != "" && d.MIME != mime {
			return result, malformed("image MIME mismatch")
		}
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
			return result, malformed("unsupported generated image format")
		}
		a.MIMEType = mime
		a.Bytes = int64(len(b))
		if cfg, _, e := image.DecodeConfig(bytes.NewReader(b)); e == nil {
			a.Width = cfg.Width
			a.Height = cfg.Height
		}
		result.Images = append(result.Images, lebro.MediaContent{Asset: a, Data: b})
		result.RevisedPrompts = append(result.RevisedPrompts, d.Revised)
	}
	result.Partial = len(result.Images) < r.Count
	return result, nil
}
