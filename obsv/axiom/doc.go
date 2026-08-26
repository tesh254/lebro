// Package axiom exports filtered Lebro observability spans to Axiom.
//
// It is optional and configures Axiom's OTLP/HTTP trace endpoint. Attach an
// Exporter to obsv.Observer; the observer's filter controls exported data.
package axiom
