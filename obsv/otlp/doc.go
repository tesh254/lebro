// Package otlp exports filtered lebro observability spans through OTLP/HTTP
// using protobuf encoding.
//
// It is optional: applications opt in by importing this package and attaching
// an Exporter to an obsv.Observer. The root lebro package never imports it.
//
// Observer filters run before Exporter receives a span. The default filter
// removes model and tool payloads; use a stricter filter when an endpoint must
// not receive particular identifiers or errors.
//
//	exporter, err := otlp.New(otlp.Config{
//		Endpoint:    "https://collector.example.com/v1/traces",
//		ServiceName: "support-agent",
//		Headers:     map[string]string{"Authorization": "Bearer token"},
//	})
//	observer, err := obsv.New(obsv.Config{Spans: exporter})
//	defer observer.Close()
package otlp
