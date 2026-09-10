package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// MediaToolOperation derives correlation from trusted application context.
// Never accept tenant/owner or operation identities chosen by the model.
type MediaToolOperation func(context.Context) (MediaOperation, error)

func MediaOperationFromToolContext(ctx context.Context) (MediaOperation, error) {
	m := ToolMetadataFromContext(ctx)
	scope, _ := RuntimeScopeFromContext(ctx)
	o := MediaOperation{ID: m["run_id"] + ":" + m["tool_call_id"], RunID: RunID(m["run_id"]), ThreadID: workflowInvocationFromContext(ctx).threadID, ToolCallID: m["tool_call_id"], Scope: scope}
	if o.RunID == "" || o.ToolCallID == "" {
		return o, mediaInvalid("media tools require run and tool-call context")
	}
	return o, o.Validate()
}

// ImageOptions are application defaults and optional model-selected image settings.
// Empty strings leave the choice to the provider; Count zero defaults to one.
type ImageOptions struct {
	Count       int    `json:"count,omitempty"`
	Size        string `json:"size,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Quality     string `json:"quality,omitempty"`
	Format      string `json:"format,omitempty"`
}

type ImageToolConfig struct {
	// Defaults apply when the model omits an option.
	Defaults ImageOptions
	// AllowModelOptions exposes generation settings in the tool input schema.
	// Provider capability validation still applies to the resulting request.
	AllowModelOptions bool
	ID                ToolID
	Generator         ImageGenerator
	Sink              MediaAssetSink
	Open              MediaAssetOpener
	Operation         MediaToolOperation
	Policy            Policy
	MaxBytes          int64
}
type imageMediaTool struct{ config ImageToolConfig }

// NewImageTool creates a schema-backed image capability for AgentConfig.Tools.
// Applications set defaults and may allow the model to override generation options.
func NewImageTool(c ImageToolConfig) (Tool, error) {
	if c.Generator == nil || c.Sink == nil {
		return nil, mediaInvalid("image tool requires generator and asset sink")
	}
	if c.ID == "" {
		c.ID = "generate_image"
	}
	if c.Operation == nil {
		c.Operation = MediaOperationFromToolContext
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 64 << 20
	}
	if c.MaxBytes < 1 {
		return nil, mediaInvalid("invalid byte limit")
	}
	if c.Defaults.Count < 0 {
		return nil, mediaInvalid("invalid image count")
	}
	return &imageMediaTool{config: c}, nil
}

var mediaPromptSchema = json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":32000}},"required":["prompt"],"additionalProperties":false}`)

var mediaImageSchema = json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":32000},"count":{"type":"integer","minimum":1},"size":{"type":"string","description":"Pixel dimensions supported by the model. Cannot be combined with resolution or aspect_ratio."},"resolution":{"type":"string"},"aspect_ratio":{"type":"string"},"quality":{"type":"string"},"format":{"type":"string"}},"required":["prompt"],"additionalProperties":false}`)

func (t *imageMediaTool) Definition() ToolDefinition {
	schema := mediaPromptSchema
	if t.config.AllowModelOptions {
		schema = mediaImageSchema
	}
	return ToolDefinition{ID: t.config.ID, Description: "Generate an image from a prompt and return durable application asset references.", InputSchema: append(json.RawMessage(nil), schema...), OutputSchema: json.RawMessage(`{"type":"object","properties":{"operation":{"type":"object"},"assets":{"type":"array","items":{"type":"object"}},"partial":{"type":"boolean"}},"required":["operation","assets","partial"],"additionalProperties":false}`)}
}

type MediaArtifactResult struct {
	Operation MediaOperation `json:"operation"`
	Assets    []MediaAsset   `json:"assets"`
	Partial   bool           `json:"partial"`
}

func mediaPrompt(args json.RawMessage) (string, error) {
	var input struct {
		Prompt string `json:"prompt"`
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil {
		return "", mediaInvalid("expected prompt object")
	}
	if err := validateMediaText(input.Prompt, 32000); err != nil {
		return "", err
	}
	return input.Prompt, nil
}
func (t *imageMediaTool) input(args json.RawMessage) (string, ImageOptions, error) {
	options := t.config.Defaults
	if options.Count == 0 {
		options.Count = 1
	}
	if !t.config.AllowModelOptions {
		prompt, err := mediaPrompt(args)
		return prompt, options, err
	}
	// Decode into defaults so omitted settings retain application values.
	input := struct {
		Prompt string `json:"prompt"`
		ImageOptions
	}{ImageOptions: options}
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return "", options, mediaInvalid("expected image options object")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return "", options, mediaInvalid("expected one image options object")
	}
	if err := validateMediaText(input.Prompt, 32000); err != nil {
		return "", options, err
	}
	if input.Count < 1 {
		return "", options, mediaInvalid("invalid image count")
	}
	if input.Size != "" && (input.Resolution != "" || input.AspectRatio != "") {
		return "", options, mediaInvalid("size conflicts with resolution/aspect ratio; clear size to override")
	}
	return input.Prompt, input.ImageOptions, nil
}

func (t *imageMediaTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	prompt, options, err := t.input(args)
	if err != nil {
		return nil, err
	}
	o, err := t.config.Operation(ctx)
	if err != nil {
		return nil, err
	}
	if err = o.Validate(); err != nil {
		return nil, err
	}
	if scope, ok := RuntimeScopeFromContext(ctx); ok && scope != o.Scope {
		return nil, &MediaError{Kind: MediaErrorAuthorization, Message: "media scope mismatch"}
	}
	if err = authorize(ctx, t.config.Policy, "media.generate", Resource{Kind: "media_asset", ID: o.ID, Tenant: o.Scope.Namespace, OwnerID: o.Scope.OwnerID}); err != nil {
		return nil, err
	}
	generated, err := t.config.Generator.GenerateImages(ctx, ImageRequest{Operation: o, Prompt: prompt, Count: options.Count, Size: options.Size, Resolution: options.Resolution, AspectRatio: options.AspectRatio, Quality: options.Quality, Format: options.Format})
	if err != nil {
		return nil, err
	}
	result := MediaArtifactResult{Operation: o, Partial: generated.Partial}
	for _, content := range generated.Images {
		asset, e := SaveMediaAsset(ctx, o, content, t.config.Sink, t.config.Open, t.config.MaxBytes)
		if e != nil {
			return nil, e
		}
		result.Assets = append(result.Assets, asset)
	}
	if len(result.Assets) == 0 {
		return nil, &MediaError{Kind: MediaErrorMalformed, Message: "image tool produced no assets"}
	}
	return json.Marshal(result)
}

type VideoToolConfig struct {
	ID        ToolID
	Service   *VideoService
	Operation MediaToolOperation
}
type videoMediaTool struct{ config VideoToolConfig }

func NewVideoTool(c VideoToolConfig) (Tool, error) {
	if c.Service == nil {
		return nil, mediaInvalid("video service is required")
	}
	if c.ID == "" {
		c.ID = "generate_video"
	}
	if c.Operation == nil {
		c.Operation = MediaOperationFromToolContext
	}
	return &videoMediaTool{config: c}, nil
}
func (t *videoMediaTool) Definition() ToolDefinition {
	return ToolDefinition{ID: t.config.ID, Description: "Submit video generation and return a durable job handle. The application polls it for completion.", InputSchema: append(json.RawMessage(nil), mediaPromptSchema...), OutputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *videoMediaTool) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	prompt, err := mediaPrompt(args)
	if err != nil {
		return nil, err
	}
	o, err := t.config.Operation(ctx)
	if err != nil {
		return nil, err
	}
	job, err := t.config.Service.Submit(ctx, VideoRequest{Operation: o, Prompt: prompt})
	if err != nil {
		return nil, err
	}
	return json.Marshal(job)
}

type mediaCopyReader struct {
	ctx     context.Context
	r       io.Reader
	left, n int64
	eof     bool
	err     error
}

func (r *mediaCopyReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, e := r.r.Read(p)
	if int64(n) > r.left {
		r.err = mediaInvalid("media exceeds byte limit")
		return 0, r.err
	}
	r.left -= int64(n)
	r.n += int64(n)
	r.eof = errors.Is(e, io.EOF)
	if e != nil && !r.eof {
		r.err = e
	}
	return n, e
}

// SaveMediaAsset consumes and closes content, bounds I/O, and verifies the sink
// returned a durable key. The sink owns atomic publication and orphan cleanup.
func SaveMediaAsset(ctx context.Context, o MediaOperation, c MediaContent, sink MediaAssetSink, open MediaAssetOpener, max int64) (MediaAsset, error) {
	if err := o.Validate(); err != nil {
		return MediaAsset{}, err
	}
	if err := c.Validate(); err != nil {
		return MediaAsset{}, err
	}
	if sink == nil || max <= 0 {
		return MediaAsset{}, mediaInvalid("asset sink and positive byte limit are required")
	}
	reader := c.Reader
	if reader == nil {
		if c.Data != nil {
			reader = io.NopCloser(bytes.NewReader(c.Data))
		} else {
			if open == nil {
				return MediaAsset{}, mediaInvalid("explicit asset opener is required")
			}
			var err error
			reader, err = open(ctx, o, c)
			if err != nil {
				return MediaAsset{}, err
			}
			if reader == nil {
				return MediaAsset{}, mediaInvalid("asset opener returned nil")
			}
		}
	}
	defer func() { _ = reader.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stop()
	hash := sha256.New()
	bounded := &mediaCopyReader{ctx: ctx, r: io.TeeReader(reader, hash), left: max}
	asset, err := sink.PutMedia(ctx, o, c.Asset, bounded)
	if err != nil {
		return MediaAsset{}, err
	}
	if bounded.err != nil {
		return MediaAsset{}, bounded.err
	}
	if !bounded.eof {
		return MediaAsset{}, mediaInvalid("asset sink did not consume to EOF")
	}
	if asset.ID == "" || asset.Locator == "" || len(asset.ID) > 256 || len(asset.Locator) > 2048 || strings.Contains(asset.Locator, "://") || strings.ContainsAny(asset.Locator, "?#\r\n") {
		return MediaAsset{}, mediaInvalid("asset sink must return an opaque durable storage key")
	}
	asset.Kind = c.Asset.Kind
	asset.MIMEType = c.Asset.MIMEType
	asset.Bytes = bounded.n
	asset.SHA256 = hex.EncodeToString(hash.Sum(nil))
	asset.ExpiresAt = nil
	return asset, nil
}
