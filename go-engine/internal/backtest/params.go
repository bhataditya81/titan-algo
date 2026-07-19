package backtest

import (
	"errors"

	"titan-algo/internal/strategy"
)

// ErrParamsUnsupported is returned by ResolveStrategy when the caller asked
// for parameterized construction (-params on the CLI, G-5) but
// internal/strategy doesn't yet expose a GetWithParams-style function.
//
// R2-2 (running concurrently this round) owns internal/strategy and is
// adding parameterized constructors + a registry.GetWithParams(name string,
// params map[string]float64) (strategy.Strategy, error))-shaped function
// per docs/PRODUCTION_GAPS_R2.md's R2-2 section. This file can't import a
// symbol that doesn't exist yet, so it exposes a seam instead: wire it with
//
//	backtest.StrategyWithParams = strategy.GetWithParams
//
// in cmd/backtest/main.go (or an init()) once R2-2 lands. Until then,
// ResolveStrategy fails closed with this sentinel rather than silently
// ignoring the params or guessing.
var ErrParamsUnsupported = errors.New("backtest: strategy parameterization requested but internal/strategy.GetWithParams is not wired yet (R2-2 dependency)")

// StrategyWithParams is the seam described above. nil (the default) means
// "not available yet".
var StrategyWithParams func(name string, params map[string]float64) (strategy.Strategy, error)

// ResolveStrategy looks up a strategy by name, optionally parameterized.
// Empty/nil params is exactly strategy.Get. Non-empty params requires
// StrategyWithParams to be set; otherwise returns ErrParamsUnsupported so
// the caller (cmd/backtest) can decide how to degrade -- see G-5's "don't
// block on it" instruction: the CLI falls back to the unparameterized
// strategy with a loud warning rather than refusing to run.
func ResolveStrategy(name string, params map[string]float64) (strategy.Strategy, error) {
	if len(params) == 0 {
		return strategy.Get(name)
	}
	if StrategyWithParams == nil {
		return nil, ErrParamsUnsupported
	}
	return StrategyWithParams(name, params)
}
