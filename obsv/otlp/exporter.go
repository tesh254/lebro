package otlp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tesh254/lebro/obsv"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultEndpoint is the standard OTLP/HTTP traces endpoint.
	DefaultEndpoint      = "http://localhost:4318/v1/traces"
	instrumentationScope = "github.com/tesh254/lebro/obsv/otlp"
	maxExportAttempts    = 3
)

// Config configures an OTLP/HTTP exporter.
type Config struct {
	// Endpoint receives protobuf-encoded OTLP trace batches. An empty value uses
	// DefaultEndpoint.
	Endpoint string
	// Headers are sent with every export request. Use this for collector or
	// vendor authentication; do not put credentials in ResourceAttributes.
	Headers map[string]string
	// ServiceName identifies this application in the receiving backend. Empty
	// selects "lebro".
	ServiceName string
	// ResourceAttributes attach stable application-wide attributes to each OTLP
	// resource. A supplied service.name overrides ServiceName.
	ResourceAttributes map[string]string
	// HTTPClient sends export requests. Nil uses http.DefaultClient.
	HTTPClient *http.Client
}

// Exporter converts obsv spans to OTLP trace batches.
type Exporter struct {
	endpoint *url.URL
	headers  http.Header
	resource []*commonpb.KeyValue
	client   *http.Client
}

// New validates config and returns an OTLP/HTTP exporter.
func New(config Config) (*Exporter, error) {
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("otlp: endpoint must be an absolute HTTP URL: %q", endpoint)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("otlp: endpoint scheme must be http or https: %q", parsed.Scheme)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("otlp: endpoint must not contain credentials, query, or fragment")
	}

	headers := make(http.Header, len(config.Headers))
	for key, value := range config.Headers {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("otlp: header name must not be empty")
		}
		headers.Set(key, value)
	}
	resourceAttributes := map[string]string{"service.name": config.ServiceName}
	if resourceAttributes["service.name"] == "" {
		resourceAttributes["service.name"] = "lebro"
	}
	for key, value := range config.ResourceAttributes {
		if key != "" {
			resourceAttributes[key] = value
		}
	}

	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Exporter{
		endpoint: parsed,
		headers:  headers,
		resource: stringAttributes(resourceAttributes),
		client:   client,
	}, nil
}

var _ obsv.SpanExporter = (*Exporter)(nil)

// ExportSpans sends spans as one OTLP/HTTP protobuf request. A non-success
// response is returned to Observer, where it remains non-fatal to agent runs.
func (e *Exporter) ExportSpans(ctx context.Context, spans []obsv.Span) error {
	if len(spans) == 0 {
		return nil
	}
	if e == nil || e.endpoint == nil || e.client == nil {
		return fmt.Errorf("otlp: nil exporter")
	}
	requestBody, err := proto.Marshal(&collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   &resourcepb.Resource{Attributes: cloneAttributes(e.resource)},
			ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: instrumentationScope}, Spans: convertSpans(spans)}},
		}},
	})
	if err != nil {
		return fmt.Errorf("otlp: marshal trace request: %w", err)
	}

	var body []byte
	var response *http.Response
	for attempt := 0; attempt < maxExportAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint.String(), bytes.NewReader(requestBody))
		if err != nil {
			return fmt.Errorf("otlp: create export request: %w", err)
		}
		req.Header = e.headers.Clone()
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-protobuf")
		}
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/x-protobuf")
		}
		response, err = e.client.Do(req)
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(response.Body, 4<<10))
			closeErr := response.Body.Close()
			if err != nil {
				return fmt.Errorf("otlp: read export response: %w", err)
			}
			if closeErr != nil {
				return fmt.Errorf("otlp: close export response: %w", closeErr)
			}
			if response.StatusCode/100 == 2 {
				break
			}
			if !retryableStatus(response.StatusCode) || attempt == maxExportAttempts-1 {
				return fmt.Errorf("otlp: export traces: server returned %s: %s", response.Status, strings.TrimSpace(string(body)))
			}
			if err := waitRetry(ctx, response.Header.Get("Retry-After"), attempt); err != nil {
				return err
			}
			continue
		}
		if attempt == maxExportAttempts-1 {
			return fmt.Errorf("otlp: export traces: %w", err)
		}
		if err := waitRetry(ctx, "", attempt); err != nil {
			return err
		}
	}
	if len(body) == 0 {
		return nil
	}
	var exportResponse collectorpb.ExportTraceServiceResponse
	if err := proto.Unmarshal(body, &exportResponse); err != nil {
		return fmt.Errorf("otlp: decode export response: %w", err)
	}
	if partial := exportResponse.GetPartialSuccess(); partial != nil && partial.GetRejectedSpans() > 0 {
		return fmt.Errorf("otlp: collector rejected %d span(s): %s", partial.GetRejectedSpans(), partial.GetErrorMessage())
	}
	return nil
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}
func waitRetry(ctx context.Context, retryAfter string, attempt int) error {
	delay := time.Duration(attempt+1) * 100 * time.Millisecond
	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func convertSpans(spans []obsv.Span) []*tracepb.Span {
	converted := make([]*tracepb.Span, 0, len(spans))
	for _, span := range spans {
		converted = append(converted, convertSpan(span))
	}
	return converted
}

func convertSpan(span obsv.Span) *tracepb.Span {
	start, end := spanTimes(span)
	attributes := make(map[string]string, len(span.Attributes)+8)
	for key, value := range span.Attributes {
		attributes[key] = value
	}
	attributes["lebro.trace_id"] = string(span.TraceID)
	attributes["lebro.span_id"] = string(span.SpanID)
	attributes["lebro.span.kind"] = string(span.Kind)
	attributes["lebro.status"] = string(span.Status)
	if span.RunID != "" {
		attributes["lebro.run_id"] = string(span.RunID)
	}
	if span.RunSpanID != "" {
		attributes["lebro.run_span_id"] = string(span.RunSpanID)
	}
	if span.StepID != "" {
		attributes["lebro.step_id"] = string(span.StepID)
	}
	if span.Step != 0 {
		attributes["lebro.step"] = fmt.Sprint(span.Step)
	}
	if span.Usage.InputTokens != 0 {
		attributes["gen_ai.usage.input_tokens"] = fmt.Sprint(span.Usage.InputTokens)
	}
	if span.Usage.OutputTokens != 0 {
		attributes["gen_ai.usage.output_tokens"] = fmt.Sprint(span.Usage.OutputTokens)
	}
	if span.Usage.ReasoningTokens != 0 {
		attributes["lebro.gen_ai.usage.reasoning_tokens"] = fmt.Sprint(span.Usage.ReasoningTokens)
	}
	if span.Usage.TotalTokens != 0 {
		attributes["gen_ai.usage.total_tokens"] = fmt.Sprint(span.Usage.TotalTokens)
	}

	return &tracepb.Span{
		TraceId:           traceID(span.TraceID),
		SpanId:            spanID(span.SpanID),
		ParentSpanId:      parentSpanID(span.ParentSpanID),
		Name:              span.Name,
		Kind:              spanKind(span.Kind),
		StartTimeUnixNano: unixNano(start),
		EndTimeUnixNano:   unixNano(end),
		Attributes:        stringAttributes(attributes),
		Events:            convertEvents(span.Events),
		Status:            spanStatus(span),
	}
}

func spanTimes(span obsv.Span) (time.Time, time.Time) {
	start, end := span.Start, span.End
	if end.IsZero() {
		end = start.Add(span.Duration)
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

func unixNano(value time.Time) uint64 {
	if value.UnixNano() <= 0 {
		return 0
	}
	return uint64(value.UnixNano())
}

func traceID(id obsv.TraceID) []byte { return identifierBytes(string(id), 16) }
func spanID(id obsv.SpanID) []byte   { return identifierBytes(string(id), 8) }
func parentSpanID(id obsv.SpanID) []byte {
	if id == "" {
		return nil
	}
	return spanID(id)
}

func identifierBytes(id string, size int) []byte {
	sum := sha256.Sum256([]byte(id))
	return append([]byte(nil), sum[:size]...)
}

func spanKind(kind obsv.SpanKind) tracepb.Span_SpanKind {
	if kind == obsv.SpanKindModel {
		return tracepb.Span_SPAN_KIND_CLIENT
	}
	return tracepb.Span_SPAN_KIND_INTERNAL
}

func spanStatus(span obsv.Span) *tracepb.Status {
	status := &tracepb.Status{}
	switch span.Status {
	case obsv.SpanStatusOK:
		status.Code = tracepb.Status_STATUS_CODE_OK
	case obsv.SpanStatusError:
		status.Code, status.Message = tracepb.Status_STATUS_CODE_ERROR, span.Error
	}
	return status
}

func convertEvents(events []obsv.SpanEvent) []*tracepb.Span_Event {
	if len(events) == 0 {
		return nil
	}
	converted := make([]*tracepb.Span_Event, 0, len(events))
	for _, event := range events {
		converted = append(converted, &tracepb.Span_Event{Name: event.Name, TimeUnixNano: unixNano(event.Timestamp), Attributes: stringAttributes(event.Attributes)})
	}
	return converted
}

func stringAttributes(attributes map[string]string) []*commonpb.KeyValue {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]*commonpb.KeyValue, 0, len(keys))
	for _, key := range keys {
		values = append(values, &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: attributes[key]}}})
	}
	return values
}

func cloneAttributes(attributes []*commonpb.KeyValue) []*commonpb.KeyValue {
	return proto.Clone(&resourcepb.Resource{Attributes: attributes}).(*resourcepb.Resource).Attributes
}
