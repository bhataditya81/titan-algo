package strategy

import (
	"fmt"
	"sync"
)

// StrategyFactory is a function that returns a new instance of a Strategy
type StrategyFactory func() Strategy

var (
	regMu     sync.Mutex
	registry  = make(map[string]StrategyFactory)
	instances = make(map[string]Strategy) // cached singleton per name (fixes L4: Get used to lose state every call)
)

// Register adds a strategy to the registry
func Register(name string, factory StrategyFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[name] = factory
}

// Get returns the shared singleton instance of the requested strategy,
// creating it on first request. Stateful strategies (nine_twenty, sniper)
// depend on this: fetching the same name twice must return the SAME
// instance, or in-memory state (entered flag, candle buffers) is silently
// lost (ST-10/L4).
func Get(name string) (Strategy, error) {
	regMu.Lock()
	defer regMu.Unlock()

	if inst, ok := instances[name]; ok {
		return inst, nil
	}

	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("strategy '%s' not found. Available: %v", name, availableStrategiesLocked())
	}

	inst := factory()
	instances[name] = inst
	return inst, nil
}

// Reset discards the cached singleton instance for name, if any, so the next
// Get call constructs a fresh one. Useful for tests that need isolated state
// between cases.
func Reset(name string) {
	regMu.Lock()
	defer regMu.Unlock()
	delete(instances, name)
}

// GetAvailableStrategies returns a list of registered strategy names
func GetAvailableStrategies() []string {
	regMu.Lock()
	defer regMu.Unlock()
	return availableStrategiesLocked()
}

func availableStrategiesLocked() []string {
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	return keys
}
