package strategy

import (
	"fmt"
	"reflect"
	"sync"
)

// StrategyFactory is a function that returns a new instance of a Strategy
type StrategyFactory func() Strategy

// ParamFactory builds a fresh Strategy instance from a flat parameter map
// (see GetWithParams). Each strategy file registers its own ParamFactory
// alongside its plain StrategyFactory.
type ParamFactory func(params map[string]float64) (Strategy, error)

var (
	regMu          sync.Mutex
	registry       = make(map[string]StrategyFactory)
	paramFactories = make(map[string]ParamFactory)
	instances      = make(map[string]Strategy) // cached singleton per name (fixes L4: Get used to lose state every call)
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

// RegisterParams adds a parameterized constructor for name, used by
// GetWithParams. Each strategy's init() calls this alongside Register.
func RegisterParams(name string, factory ParamFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	paramFactories[name] = factory
}

// GetWithParams builds a FRESH strategy instance for name (G-5:
// parameter-sweep tooling, e.g. the backtest CLI, needs an independent
// instance per call — never the Get singleton cache, which is shared and
// mutates). params keys are the exported field names of that strategy's own
// <Strategy>Params struct (e.g. "FastPeriod"/"SlowPeriod" for
// ema_crossover; see docs/reports/R2-2-REPORT.md for the full per-strategy
// key list); values are float64, truncated to int for int-typed fields. A
// zero-valued/absent key falls back to that field's hardcoded default,
// exactly like New<Strategy>(<Strategy>Params{}).
//
// Fails clearly rather than silently ignoring a mistake: an unregistered
// strategy name or a params key that isn't a field on that strategy's Params
// struct (a typo) both return an error, never a partially-applied result.
func GetWithParams(name string, params map[string]float64) (Strategy, error) {
	regMu.Lock()
	factory, ok := paramFactories[name]
	names := availableStrategiesLocked()
	regMu.Unlock()

	if !ok {
		return nil, fmt.Errorf("strategy '%s' not found. Available: %v", name, names)
	}
	return factory(params)
}

// applyParams copies params (flat name->float64 overrides) onto the
// exported fields of dst, which must be a pointer to a Params struct with
// only int/float64 fields. int-typed fields are set via int(v) (truncation,
// not rounding — matches Go's normal float->int conversion semantics).
// Returns an error naming the first unknown key, so a typo'd flag fails
// loudly instead of being silently dropped (G-5 requirement).
func applyParams(dst interface{}, params map[string]float64) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("applyParams: dst must be a pointer to a struct")
	}
	v = v.Elem()
	for key, val := range params {
		f := v.FieldByName(key)
		if !f.IsValid() || !f.CanSet() {
			return fmt.Errorf("unknown parameter %q", key)
		}
		switch f.Kind() {
		case reflect.Float64:
			f.SetFloat(val)
		case reflect.Int:
			f.SetInt(int64(val))
		default:
			return fmt.Errorf("parameter %q has unsupported type %s", key, f.Kind())
		}
	}
	return nil
}
