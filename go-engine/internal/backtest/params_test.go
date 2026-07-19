package backtest

import (
	"errors"
	"testing"

	"titan-algo/internal/strategy"
)

func TestResolveStrategy_EmptyParamsUsesPlainGet(t *testing.T) {
	strat, err := ResolveStrategy("ema_crossover", nil)
	if err != nil {
		t.Fatalf("ResolveStrategy with no params: %v", err)
	}
	if strat == nil || strat.Name() == "" {
		t.Fatalf("expected a usable strategy, got %+v", strat)
	}
}

func TestResolveStrategy_ParamsWithoutHookFailsClosed(t *testing.T) {
	StrategyWithParams = nil // simulate R2-2 not landed yet
	_, err := ResolveStrategy("rsi_reversal", map[string]float64{"rsi_period": 10})
	if !errors.Is(err, ErrParamsUnsupported) {
		t.Fatalf("expected ErrParamsUnsupported, got %v", err)
	}
}

func TestResolveStrategy_ParamsWithHookDelegates(t *testing.T) {
	called := false
	StrategyWithParams = func(name string, params map[string]float64) (strategy.Strategy, error) {
		called = true
		if name != "rsi_reversal" || params["rsi_period"] != 10 {
			t.Errorf("hook got unexpected args: name=%s params=%v", name, params)
		}
		return &fakeStrategy{name: "rsi_reversal-parameterized", fn: func(ctx EvalContext) Signal { return Signal{Action: Hold} }}, nil
	}
	t.Cleanup(func() { StrategyWithParams = nil })

	strat, err := ResolveStrategy("rsi_reversal", map[string]float64{"rsi_period": 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected StrategyWithParams hook to be invoked")
	}
	if strat.Name() != "rsi_reversal-parameterized" {
		t.Errorf("got strategy %q from hook, expected the parameterized instance", strat.Name())
	}
}
