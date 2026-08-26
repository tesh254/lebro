// Package langsmith exports filtered Lebro observability spans to LangSmith.
//
// It is optional and configures LangSmith's OTLP/HTTP trace endpoint. Attach an
// Exporter to obsv.Observer; the observer's filter controls which data leaves
// the application.
package langsmith
