package otlp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/obsv"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestExporterSendsOTLPHTTPProtobuf(t *testing.T) {
	var received collectorpb.ExportTraceServiceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path = %q, want /v1/traces", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("content type = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		if err := proto.Unmarshal(mustRead(t, r), &received); err != nil {
			t.Errorf("unmarshal = %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	exporter, err := New(Config{Endpoint: server.URL + "/v1/traces", Headers: map[string]string{"Authorization": "Bearer token"}, ServiceName: "support-agent", ResourceAttributes: map[string]string{"deployment.environment.name": "test"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	start := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	err = exporter.ExportSpans(context.Background(), []obsv.Span{{
		TraceID: "trace-1", SpanID: "span-1", Kind: obsv.SpanKindModel, Name: "openai/gpt", RunID: lebro.RunID("run-1"),
		Start: start, End: start.Add(time.Second), Status: obsv.SpanStatusOK,
		Usage: lebro.ModelUsage{InputTokens: 4, OutputTokens: 8, TotalTokens: 12}, Attributes: map[string]string{"model.provider": "openai"},
		Events: []obsv.SpanEvent{{Name: "stream.delta", Timestamp: start.Add(time.Millisecond), Attributes: map[string]string{"sequence": "1"}}},
	}})
	if err != nil {
		t.Fatalf("ExportSpans() error = %v", err)
	}

	resources := received.GetResourceSpans()
	if len(resources) != 1 || len(resources[0].GetScopeSpans()) != 1 || len(resources[0].GetScopeSpans()[0].GetSpans()) != 1 {
		t.Fatalf("unexpected OTLP shape: %+v", resources)
	}
	span := resources[0].GetScopeSpans()[0].GetSpans()[0]
	if len(span.GetTraceId()) != 16 || len(span.GetSpanId()) != 8 {
		t.Fatalf("invalid IDs: trace=%d span=%d", len(span.GetTraceId()), len(span.GetSpanId()))
	}
	if span.GetName() != "openai/gpt" || span.GetKind().String() != "SPAN_KIND_CLIENT" {
		t.Fatalf("span = %+v", span)
	}
	if span.GetStatus().GetCode().String() != "STATUS_CODE_OK" {
		t.Fatalf("status = %v", span.GetStatus())
	}
	if got := attributes(span.GetAttributes())["gen_ai.usage.total_tokens"]; got != "12" {
		t.Errorf("total tokens = %q", got)
	}
	if got := attributes(resources[0].GetResource().GetAttributes())["service.name"]; got != "support-agent" {
		t.Errorf("service name = %q", got)
	}
}

func TestNewRejectsUnsafeEndpoint(t *testing.T) {
	for _, endpoint := range []string{"collector:4318", "ftp://collector", "https://user:secret@collector", "https://collector?token=secret"} {
		if _, err := New(Config{Endpoint: endpoint}); err == nil {
			t.Errorf("New(%q) error = nil", endpoint)
		}
	}
}

func TestExporterReportsCollectorRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := proto.Marshal(&collectorpb.ExportTraceServiceResponse{PartialSuccess: &collectorpb.ExportTracePartialSuccess{RejectedSpans: 1, ErrorMessage: "invalid attributes"}})
		if err != nil {
			t.Errorf("marshal response = %v", err)
			http.Error(w, "marshal response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(response)
	}))
	defer server.Close()
	exporter, err := New(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), []obsv.Span{{TraceID: "trace", SpanID: "span", Name: "run"}}); err == nil {
		t.Fatal("ExportSpans() error = nil")
	}
}

func mustRead(t *testing.T, r *http.Request) []byte {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body = %v", err)
	}
	return data
}

func attributes(values []*commonpb.KeyValue) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.GetKey()] = value.GetValue().GetStringValue()
	}
	return result
}
