package strategy

import "testing"

func TestBullishEngulfing_StrictInequalityRequired(t *testing.T) {
	// Old >=/<= behavior: a candle that opens EXACTLY at prior close and
	// closes EXACTLY at prior open (same-size body, just mirrored) used to
	// register as "engulfing" even though it doesn't actually engulf a
	// LARGER range than the prior candle. This must NOT fire under the fix.
	prev := Candle{Open: 100, Close: 98} // bearish, body 2
	curr := Candle{Open: 98, Close: 100} // bullish, body 2 (same size, exact mirror)

	if IsBullishEngulfing(prev, curr) {
		t.Errorf("expected same-size mirrored body to NOT count as engulfing (old >=/<= bug), but it fired")
	}
}

func TestBullishEngulfing_TrueEngulfingFires(t *testing.T) {
	prev := Candle{Open: 100, Close: 98} // bearish, body 2
	curr := Candle{Open: 97, Close: 102} // bullish, body 5, strictly engulfs both sides

	if !IsBullishEngulfing(prev, curr) {
		t.Errorf("expected a genuinely larger engulfing candle to fire")
	}
}

func TestBullishEngulfing_RejectsNonBearishPrev(t *testing.T) {
	prev := Candle{Open: 98, Close: 100} // bullish, not bearish
	curr := Candle{Open: 97, Close: 102}
	if IsBullishEngulfing(prev, curr) {
		t.Errorf("expected false when prev candle isn't bearish")
	}
}

func TestBearishEngulfing_StrictInequalityRequired(t *testing.T) {
	prev := Candle{Open: 98, Close: 100} // bullish, body 2
	curr := Candle{Open: 100, Close: 98} // bearish, body 2 (exact mirror)

	if IsBearishEngulfing(prev, curr) {
		t.Errorf("expected same-size mirrored body to NOT count as engulfing (old >=/<= bug), but it fired")
	}
}

func TestBearishEngulfing_TrueEngulfingFires(t *testing.T) {
	prev := Candle{Open: 98, Close: 100} // bullish, body 2
	curr := Candle{Open: 102, Close: 97} // bearish, body 5

	if !IsBearishEngulfing(prev, curr) {
		t.Errorf("expected a genuinely larger engulfing candle to fire")
	}
}
