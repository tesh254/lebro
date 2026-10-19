package lebro

import "github.com/tesh254/lebro/internal/runtime"

// ValidateDecisionImages checks image inputs against the capabilities a
// decision provider advertises. It is the local gate that keeps an
// unsupported image request from reaching the network.
func ValidateDecisionImages(caps DecisionImageCapabilities, images []DecisionImage) error {
	return runtime.ValidateDecisionImages(caps, images)
}
