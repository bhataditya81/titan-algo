package strategy

import "fmt"

// StrategyFactory is a function that returns a new instance of a Strategy
type StrategyFactory func() Strategy

// registry holds the registered strategies
var registry = make(map[string]StrategyFactory)

// Register adds a strategy to the registry
func Register(name string, factory StrategyFactory) {
	registry[name] = factory
}

// Get returns a new instance of the requested strategy
func Get(name string) (Strategy, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("strategy '%s' not found. Available: %v", name, GetAvailableStrategies())
	}
	return factory(), nil
}

// GetAvailableStrategies returns a list of registered strategy names
func GetAvailableStrategies() []string {
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	return keys
}
