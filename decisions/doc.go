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
// The package is an adapter package: it depends on provider wire formats on
// purpose, and the root lebro package does not depend on it.

package decisions
