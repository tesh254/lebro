package lebro

import "github.com/tesh254/lebro/internal/runtime"

// ValidateDecisionImages checks image inputs against the capabilities a
// decision provider advertises. It is the local gate that keeps an
// unsupported image request from reaching the network.
func ValidateDecisionImages(caps DecisionImageCapabilities, images []DecisionImage) error {
	return runtime.ValidateDecisionImages(caps, images)
}

// DecisionTopProbability returns the largest probability in a distribution
// and whether the distribution carries one.
//
// The value is derived from the reported distribution, not reported by the
// provider.
func DecisionTopProbability[T comparable](distribution map[T]float64) (float64, bool) {
	return runtime.DecisionTopProbability(distribution)
}

// DecisionTopTwoMargin returns the difference between the two largest
// probabilities in a distribution, and whether the distribution carries two.
//
// The value is derived from the reported distribution, not reported by the
// provider. It is the measure one provider uses for its own confidence, and
// it is not the measure another provider uses.
func DecisionTopTwoMargin[T comparable](distribution map[T]float64) (float64, bool) {
	return runtime.DecisionTopTwoMargin(distribution)
}
