package strategy

import "testing"

// neutralPrices produces a flat-ish price series that keeps RSI(14) inside
// the 45-55 neutral band used by short_straddle/iron_fly as their entry
// condition.
func neutralPrices(n int) []float64 {
	p := make([]float64, n)
	base := 100.0
	for i := range p {
		if i%2 == 0 {
			p[i] = base + 0.5
		} else {
			p[i] = base - 0.5
		}
	}
	return p
}

func TestShortStraddle_NoChurnWhilePositionOpen(t *testing.T) {
	s := NewShortStraddleStrategy()
	prices := neutralPrices(20)

	flat := s.Evaluate(EvalContext{Prices: prices, HasPosition: false})
	if flat.Action != Sell || len(flat.Legs) == 0 {
		t.Fatalf("expected an entry signal while flat, got %v", flat.Action)
	}

	// ST-5 fix: once HasPosition is true, the SAME neutral conditions must
	// NOT re-signal a fresh entry (the old bug re-sold a straddle every
	// evaluation while RSI stayed in-band).
	inPos := s.Evaluate(EvalContext{Prices: prices, HasPosition: true, EntryPremium: 100})
	if inPos.Action == Sell {
		t.Errorf("expected no re-entry while HasPosition=true, got Sell (churn bug)")
	}
}

func TestShortStraddle_PremiumStopExitsWhilePositionOpen(t *testing.T) {
	s := NewShortStraddleStrategy()
	prices := neutralPrices(20)

	ctx := EvalContext{
		Prices:       append(append([]float64{}, prices...), 145), // last price = current combined premium
		HasPosition:  true,
		EntryPremium: 100, // stop at 100*1.4=140; 145 breaches it
	}
	sig := s.Evaluate(ctx)
	if sig.Action != Exit {
		t.Fatalf("expected Exit on premium stop breach, got %v (%s)", sig.Action, sig.Reason)
	}
}

func TestIronFly_NoChurnWhilePositionOpen_AndHedgeFirstLegOrder(t *testing.T) {
	s := NewIronFlyStrategy()
	prices := neutralPrices(20)

	flat := s.Evaluate(EvalContext{Prices: prices, HasPosition: false})
	if flat.Action != Sell || len(flat.Legs) != 4 {
		t.Fatalf("expected a 4-leg entry signal while flat, got %v legs=%d", flat.Action, len(flat.Legs))
	}
	// CR-12: BUY (hedge) legs must precede SELL (body) legs.
	if flat.Legs[0].Direction != LegBuy || flat.Legs[1].Direction != LegBuy {
		t.Errorf("expected the first two legs to be BUY hedges, got %v, %v", flat.Legs[0].Direction, flat.Legs[1].Direction)
	}
	if flat.Legs[2].Direction != LegSell || flat.Legs[3].Direction != LegSell {
		t.Errorf("expected the last two legs to be SELL body, got %v, %v", flat.Legs[2].Direction, flat.Legs[3].Direction)
	}

	inPos := s.Evaluate(EvalContext{Prices: prices, HasPosition: true})
	if inPos.Action == Sell {
		t.Errorf("expected no re-entry while HasPosition=true, got Sell (churn bug)")
	}
}
