package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewRuleRouter(rules []RouteRule, defaultID ToolID) (*RuleRouter, error) {
	return runtime.NewRuleRouter(rules, defaultID)
}

func NewModelSpecialistRouter(config ModelSpecialistRouterConfig) (*ModelSpecialistRouter, error) {
	return runtime.NewModelSpecialistRouter(config)
}

func NewRoutedSubagent(config RoutedSubagentConfig) (*RoutedSubagent, error) {
	return runtime.NewRoutedSubagent(config)
}

// NewNetwork builds a router-led, bounded network of named workflows.
func NewNetwork(config NetworkConfig) (*Network, error) { return runtime.NewNetwork(config) }
