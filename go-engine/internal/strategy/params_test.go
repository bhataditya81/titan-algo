package strategy

import "testing"

// TestParamsZeroValueMatchesOldDefaults proves that
// New<Strategy>(<Strategy>Params{}) reproduces exactly the pre-R2-2
// hardcoded constants each strategy's plain New<Strategy>Strategy()
// constructor used to build inline — i.e. parameterization (G-5) changed
// nothing for existing callers going through registry.Get.
func TestParamsZeroValueMatchesOldDefaults(t *testing.T) {
	ema := NewEMACrossover(EMACrossoverParams{})
	if ema.FastPeriod != 9 || ema.SlowPeriod != 21 {
		t.Errorf("ema_crossover defaults changed: got FastPeriod=%d SlowPeriod=%d", ema.FastPeriod, ema.SlowPeriod)
	}

	rsi := NewRSIReversal(RSIReversalParams{})
	if rsi.Period != 2 || rsi.Oversold != 10 || rsi.Overbought != 90 || rsi.ExitSMA != 5 {
		t.Errorf("rsi_reversal defaults changed: got %+v", rsi)
	}

	mom := NewMomentum(MomentumParams{})
	if mom.RSIPeriod != 14 || mom.RSIOversold != 35 || mom.RSIOverbought != 65 ||
		mom.MACDFast != 12 || mom.MACDSlow != 26 || mom.MACDSignal != 9 ||
		mom.BollingerPeriod != 20 || mom.BollingerStdDev != 2.0 || mom.MinSignalStrength != 0.6 {
		t.Errorf("momentum defaults changed: got %+v", mom)
	}

	nt := NewNineTwenty(NineTwentyParams{})
	if nt.EntryHour != 9 || nt.EntryMinute != 20 || nt.SquareOffHour != 15 ||
		nt.SquareOffMinute != 15 || nt.StopMultiplier != 1.4 {
		t.Errorf("nine_twenty defaults changed: got %+v", nt)
	}

	sniper := NewSniper(SniperParams{})
	if sniper.StopLossPct != 1.0 || sniper.TargetPct != 2.0 || sniper.TrailingSL != 0.5 ||
		sniper.EMAPeriod != 50 || sniper.RSIPeriod != 14 || sniper.CandleMinutes != 5 {
		t.Errorf("sniper defaults changed: got %+v", sniper)
	}

	fly := NewIronFly(IronFlyParams{})
	if fly.WingWidth != 200 || fly.RSILower != 45 || fly.RSIUpper != 55 {
		t.Errorf("iron_fly defaults changed: got %+v", fly)
	}

	straddle := NewShortStraddle(ShortStraddleParams{})
	if straddle.RSILower != 45 || straddle.RSIUpper != 55 || straddle.StopMultiplier != 1.4 {
		t.Errorf("short_straddle defaults changed: got %+v", straddle)
	}
}

func TestGetWithParams_HappyPath_OverridesOnlyGivenFields(t *testing.T) {
	s, err := GetWithParams("ema_crossover", map[string]float64{"FastPeriod": 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ema, ok := s.(*EMACrossoverStrategy)
	if !ok {
		t.Fatalf("expected *EMACrossoverStrategy, got %T", s)
	}
	if ema.FastPeriod != 5 {
		t.Errorf("expected overridden FastPeriod=5, got %d", ema.FastPeriod)
	}
	if ema.SlowPeriod != 21 {
		t.Errorf("expected untouched SlowPeriod to keep its default 21, got %d", ema.SlowPeriod)
	}
}

func TestGetWithParams_ReturnsIndependentInstances_NotTheSingleton(t *testing.T) {
	a, err := GetWithParams("iron_fly", map[string]float64{"WingWidth": 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := GetWithParams("iron_fly", map[string]float64{"WingWidth": 300})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Fatalf("expected GetWithParams to build independent instances, not share one")
	}
	fa := a.(*IronFlyStrategy)
	fb := b.(*IronFlyStrategy)
	if fa.WingWidth != 100 || fb.WingWidth != 300 {
		t.Errorf("expected independently-parameterized instances, got %d and %d", fa.WingWidth, fb.WingWidth)
	}

	// The plain Get singleton must be unaffected by either call above.
	singleton, err := Get("iron_fly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if singleton.(*IronFlyStrategy).WingWidth != 200 {
		t.Errorf("expected registry.Get's singleton to keep the default WingWidth=200, got %d", singleton.(*IronFlyStrategy).WingWidth)
	}
}

func TestGetWithParams_UnknownStrategyName_Errors(t *testing.T) {
	if _, err := GetWithParams("does_not_exist", nil); err == nil {
		t.Errorf("expected an error for an unregistered strategy name")
	}
}

func TestGetWithParams_UnknownParameterKey_Errors(t *testing.T) {
	if _, err := GetWithParams("ema_crossover", map[string]float64{"NotARealField": 1}); err == nil {
		t.Errorf("expected an error for a typo'd/unknown parameter key, got nil (silent typo, not allowed)")
	}
}
