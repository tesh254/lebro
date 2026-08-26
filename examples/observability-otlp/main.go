// observability-otlp sends a filtered Lebro span to an OTLP/HTTP collector.
//
// With no OTLP_PROVIDER, it sends to an in-process collector. Set OTLP_PROVIDER
// to axiom, datadog, langfuse, or langsmith to use that provider's current
// OTLP/HTTP trace intake configuration; see README.md for required variables.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/tesh254/lebro"
	"github.com/tesh254/lebro/obsv"
	"github.com/tesh254/lebro/obsv/otlp"
)

func main() {
	name, config, cleanup := destinationFromEnvironment()
	defer cleanup()

	exporter := must(otlp.New(config))
	now := time.Now()
	mustNoError(exporter.ExportSpans(context.Background(), []obsv.Span{{
		TraceID: "trace-1",
		SpanID:  "model-1",
		Kind:    obsv.SpanKindModel,
		Name:    "openai/gpt",
		RunID:   lebro.RunID("run-1"),
		Start:   now,
		End:     now.Add(120 * time.Millisecond),
		Status:  obsv.SpanStatusOK,
		Usage:   lebro.ModelUsage{InputTokens: 12, OutputTokens: 24, TotalTokens: 36},
	}}))
	fmt.Printf("%s trace export succeeded\n", name)
}

func destinationFromEnvironment() (string, otlp.Config, func()) {
	serviceName := environment("OTEL_SERVICE_NAME", "support-agent")
	switch os.Getenv("OTLP_PROVIDER") {
	case "axiom":
		return "Axiom", axiomConfig(serviceName), func() {}
	case "datadog":
		return "Datadog", datadogConfig(serviceName), func() {}
	case "langfuse":
		return "Langfuse", langfuseConfig(serviceName), func() {}
	case "langsmith":
		return "LangSmith", langsmithConfig(serviceName), func() {}
	case "":
		collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("collector received %s %s (%s)\n", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusAccepted)
		}))
		return "local collector", otlp.Config{Endpoint: collector.URL + "/v1/traces", ServiceName: serviceName}, collector.Close
	default:
		panic("OTLP_PROVIDER must be axiom, datadog, langfuse, or langsmith")
	}
}

func axiomConfig(serviceName string) otlp.Config {
	return otlp.Config{Endpoint: "https://" + requiredEnvironment("AXIOM_DOMAIN") + "/v1/traces", ServiceName: serviceName, Headers: map[string]string{"Authorization": "Bearer " + requiredEnvironment("AXIOM_TOKEN"), "X-Axiom-Dataset": requiredEnvironment("AXIOM_DATASET")}}
}

func datadogConfig(serviceName string) otlp.Config {
	return otlp.Config{Endpoint: requiredEnvironment("DATADOG_OTLP_TRACES_ENDPOINT"), ServiceName: serviceName, Headers: map[string]string{"dd-api-key": requiredEnvironment("DD_API_KEY"), "compute_stats": "true"}}
}

func langfuseConfig(serviceName string) otlp.Config {
	baseURL := environment("LANGFUSE_BASE_URL", "https://cloud.langfuse.com")
	return otlp.Config{Endpoint: trimTrailingSlash(baseURL) + "/api/public/otel/v1/traces", ServiceName: serviceName, Headers: map[string]string{"Authorization": basicAuthorization(requiredEnvironment("LANGFUSE_PUBLIC_KEY"), requiredEnvironment("LANGFUSE_SECRET_KEY")), "x-langfuse-ingestion-version": "4"}}
}

func langsmithConfig(serviceName string) otlp.Config {
	headers := map[string]string{"x-api-key": requiredEnvironment("LANGSMITH_API_KEY")}
	if project := os.Getenv("LANGSMITH_PROJECT"); project != "" {
		headers["Langsmith-Project"] = project
	}
	return otlp.Config{Endpoint: environment("LANGSMITH_OTLP_TRACES_ENDPOINT", "https://api.smith.langchain.com/otel/v1/traces"), ServiceName: serviceName, Headers: headers}
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func requiredEnvironment(name string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	panic("set " + name)
}

func trimTrailingSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}
