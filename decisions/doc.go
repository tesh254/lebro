// Package decisions adapts System One decision providers to
// [github.com/tesh254/lebro.DecisionModel].
//
// The package hosts the three providers whose System One contracts match
// closely enough to share one wire protocol: TypeSafe Jev, Fastino GLiDE, and
// SubQ Snap-preview. Each provider keeps its own package-level adapter type
// and configuration, because each one is configured, credentialed, and
// versioned independently:
//
//	typeSafe, err := decisions.NewTypeSafe(decisions.TypeSafeConfig{
//		APIKey: os.Getenv("TYPESAFE_API_KEY"),
//		Model:  "jev-latest",
//	})
//	fastino, err := decisions.NewFastino(decisions.FastinoConfig{
//		APIKey: os.Getenv("FASTINO_API_KEY"),
//	})
//	subq, err := decisions.NewSubQ(decisions.SubQConfig{
//		APIKey: os.Getenv("SUBQ_API_KEY"),
//	})
//
// The package also exposes the generic form of the adapter,
// [NewSystemOne], which builds one from a provider's wire contract. A
// provider this package ships no named adapter for is reachable by
// configuration alone:
//
//	model, err := decisions.NewSystemOne(decisions.SystemOneConfig{
//		APIKey:     os.Getenv("SOME_SYSTEM_ONE_API_KEY"),
//		BaseURL:    "https://api.example.com",
//		AuthHeader: "X-API-Key",
//		Model:      "some-model",
//		Score:      decisions.SystemOneScoreFields{Weighted: "score"},
//		Capabilities: lebro.DecisionCapabilities{ /* ... */ },
//	})
//
// A decision request carries state and named questions, and a decision
// response carries typed answers. The adapters translate between the two
// without losing the provider's semantics: a binary probability stays a
// probability, a selected score level stays separate from the
// probability-weighted value, and a confidence or distribution is only
// reported when the provider actually reported one.
//
// Credentials are supplied by the caller and are never logged, returned, or
// included in an error. Image bytes are encoded for the wire at the last
// moment and never appear in an error message.
//
// Image inputs are the one part of the protocol that is not shared: the
// providers encode them differently, so the wire shape is selected per
// provider and a shape the package does not implement leaves image inputs
// disabled.
//
// The package is an adapter package: it depends on provider wire formats on
// purpose, and the root lebro package does not depend on it.

package decisions
