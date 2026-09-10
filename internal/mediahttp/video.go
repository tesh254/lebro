package mediahttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/tesh254/lebro"
)

type videoResponse struct {
	ID           string                     `json:"id"`
	Status       string                     `json:"status"`
	GenerationID string                     `json:"generation_id"`
	URLs         []string                   `json:"unsigned_urls"`
	Usage        map[string]json.RawMessage `json:"usage"`
}

func videoState(s string) (lebro.MediaJobState, error) {
	switch s {
	case "pending", "queued":
		return lebro.MediaJobQueued, nil
	case "in_progress":
		return lebro.MediaJobRunning, nil
	case "completed":
		return lebro.MediaJobSucceeded, nil
	case "failed":
		return lebro.MediaJobFailed, nil
	case "cancelled":
		return lebro.MediaJobCancelled, nil
	case "expired":
		return lebro.MediaJobExpired, nil
	}
	return "", malformed("unknown video state")
}
func (c *Client) SubmitVideo(ctx context.Context, r lebro.VideoRequest) (lebro.VideoJob, error) {
	var job lebro.VideoJob
	if err := c.authorize(ctx, r.Operation, "media.generate"); err != nil {
		return job, err
	}
	if !c.caps.Video {
		return job, unsupported("video generation is unsupported")
	}
	if err := c.text(r.Operation, r.Prompt); err != nil {
		return job, err
	}
	if r.DurationSeconds != 0 && !slices.Contains(c.caps.Durations, r.DurationSeconds) {
		return job, unsupported("unsupported video duration")
	}
	for _, pair := range []struct {
		v  string
		vs []string
	}{{r.Size, c.caps.Sizes}, {r.Resolution, c.caps.Resolutions}, {r.AspectRatio, c.caps.AspectRatios}} {
		if err := allowed(pair.v, pair.vs); err != nil {
			return job, err
		}
	}
	if r.Size != "" && (r.Resolution != "" || r.AspectRatio != "") {
		return job, invalid("size conflicts with resolution/aspect ratio")
	}
	if c.model == "google/veo-3.1" && r.Resolution == "1080p" && r.DurationSeconds != 8 {
		return job, invalid("1080p Veo requests require explicit 8 second duration")
	}
	payload := map[string]any{"model": c.model, "prompt": r.Prompt}
	if r.DurationSeconds != 0 {
		payload["duration"] = r.DurationSeconds
	}
	for k, v := range map[string]string{"size": r.Size, "resolution": r.Resolution, "aspect_ratio": r.AspectRatio} {
		if v != "" {
			payload[k] = v
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var out videoResponse
	h, err := c.json(ctx, http.MethodPost, "/videos", payload, &out)
	if err != nil {
		return job, err
	}
	if out.ID == "" || !safeID(out.ID) {
		return job, malformed("invalid video job identity")
	}
	state, err := videoState(out.Status)
	if err != nil {
		return job, err
	}
	now := time.Now().UTC()
	job = lebro.VideoJob{Info: c.info(r.Operation, h), ProviderJobID: out.ID, State: state, ProviderStatus: out.Status, CreatedAt: now, UpdatedAt: now, OutputCount: len(out.URLs)}
	job.Info.Usage = usage(out.Usage)
	return job, nil
}
func (c *Client) validateJob(j lebro.VideoJob) error {
	if !c.caps.Video {
		return unsupported("video generation is unsupported")
	}
	if j.ProviderJobID == "" || !safeID(j.ProviderJobID) || j.Info.Model != c.model || j.Info.Provider != c.provider {
		return invalid("job does not belong to this configured adapter")
	}
	return j.Info.Operation.Validate()
}
func (c *Client) GetVideo(ctx context.Context, j lebro.VideoJob) (lebro.VideoJob, error) {
	if err := c.authorize(ctx, j.Info.Operation, "media.read"); err != nil {
		return j, err
	}
	if err := c.validateJob(j); err != nil {
		return j, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var out videoResponse
	_, err := c.json(ctx, http.MethodGet, "/videos/"+url.PathEscape(j.ProviderJobID), nil, &out)
	if err != nil {
		return j, err
	}
	if out.ID != j.ProviderJobID {
		return j, malformed("provider changed job identity")
	}
	state, err := videoState(out.Status)
	if err != nil {
		return j, err
	}
	j.State = state
	j.ProviderStatus = out.Status
	j.Info.Usage = usage(out.Usage)
	if safeID(out.GenerationID) {
		j.Info.ProviderRequestID = out.GenerationID
	}
	j.OutputCount = len(out.URLs)
	if j.State == lebro.MediaJobSucceeded && j.OutputCount == 0 {
		return j, malformed("completed video has no outputs")
	}
	if j.OutputCount > 10 {
		return j, malformed("too many video outputs")
	}
	return j, nil
}
func (c *Client) OpenVideo(ctx context.Context, j lebro.VideoJob, index int) (lebro.MediaContent, error) {
	if err := c.authorize(ctx, j.Info.Operation, "media.read"); err != nil {
		return lebro.MediaContent{}, err
	}
	if err := c.validateJob(j); err != nil {
		return lebro.MediaContent{}, err
	}
	if j.State != lebro.MediaJobSucceeded || index < 0 || index >= j.OutputCount {
		return lebro.MediaContent{}, invalid("video output unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	resp, err := c.request(ctx, http.MethodGet, "/videos/"+url.PathEscape(j.ProviderJobID)+"/content?index="+strconv.Itoa(index), "", nil)
	if err != nil {
		cancel()
		return lebro.MediaContent{}, err
	}
	return c.content(resp, cancel, lebro.MediaVideo)
}
