package mediahttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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
	defer func() { _ = c.record(ctx, result.Info, r.Operation, "image", start, retErr) }()
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
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" || r.Format != "" && mime != "image/"+r.Format {
			return result, malformed("unsupported generated image format")
		}
		a.MIMEType = mime
		a.Bytes = int64(len(b))
		if cfg, _, e := image.DecodeConfig(bytes.NewReader(b)); e == nil {
			a.Width = cfg.Width
			a.Height = cfg.Height
		} else if mime == "image/webp" {
			if w, h, ok := webpDimensions(b); ok {
				a.Width = w
				a.Height = h
			}
		}
		result.Images = append(result.Images, lebro.MediaContent{Asset: a, Data: b})
		result.RevisedPrompts = append(result.RevisedPrompts, d.Revised)
	}
	result.Partial = len(result.Images) < r.Count
	return result, nil
}

func webpDimensions(b []byte) (w, h int, ok bool) {
	if len(b) < 20 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		if len(b) < 30 || b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff), true
	case "VP8L":
		if len(b) < 26 || b[20] != 0x2f {
			return 0, 0, false
		}
		n := binary.LittleEndian.Uint32(b[21:25])
		return int(n&0x3fff) + 1, int((n>>14)&0x3fff) + 1, true
	case "VP8X":
		if len(b) < 30 {
			return 0, 0, false
		}
		return (int(b[24]) | int(b[25])<<8 | int(b[26])<<16) + 1, (int(b[27]) | int(b[28])<<8 | int(b[29])<<16) + 1, true
	}
	return 0, 0, false
}
