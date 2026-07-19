package backtest

import (
	"math"
	"testing"
)

// TestBlackScholes_GoldenValues checks against the textbook reference case
// S=100, K=100, r=5%, sigma=20%, T=1yr (ATM, 1-year). This exact
// parameter set is a standard sanity-check example for Black-Scholes
// (see e.g. Hull, "Options, Futures & Other Derivatives", worked examples;
// widely reproduced by online BS calculators). Hand derivation:
//
//	d1 = (ln(100/100) + (0.05 + 0.5*0.2^2)*1) / (0.2*sqrt(1)) = 0.07/0.2 = 0.35
//	d2 = d1 - 0.2*sqrt(1) = 0.15
//	N(0.35) ~= 0.636831, N(0.15) ~= 0.559618  (standard normal CDF table)
//	Call = 100*N(d1) - 100*e^-0.05*N(d2) = 63.6831 - 95.1229*0.559618 ~= 10.4506
//	Put  = 100*e^-0.05*N(-d2) - 100*N(-d1) = 95.1229*0.440382 - 100*0.363169 ~= 5.5735
//	(Put-call parity check: Call-Put = S - K*e^-rT = 100-95.1229 = 4.8771 = 10.4506-5.5735 ✓)
func TestBlackScholes_GoldenValues(t *testing.T) {
	params := BSParams{Spot: 100, Strike: 100, TimeToExpiry: 1, Rate: 0.05, Vol: 0.2}

	callParams := params
	callParams.Kind = CallOption
	call := Price(callParams)
	wantCall := 10.4506
	if math.Abs(call-wantCall) > 0.01 {
		t.Errorf("ATM call price = %.4f, want ~%.4f", call, wantCall)
	}

	putParams := params
	putParams.Kind = PutOption
	put := Price(putParams)
	wantPut := 5.5735
	if math.Abs(put-wantPut) > 0.01 {
		t.Errorf("ATM put price = %.4f, want ~%.4f", put, wantPut)
	}

	// Put-call parity must hold exactly (both sides use the same formula
	// family): C - P = S - K*e^{-rT}.
	parityLHS := call - put
	parityRHS := params.Spot - params.Strike*math.Exp(-params.Rate*params.TimeToExpiry)
	if math.Abs(parityLHS-parityRHS) > 1e-9 {
		t.Errorf("put-call parity violated: C-P=%.6f, S-Ke^-rT=%.6f", parityLHS, parityRHS)
	}

	callDelta := Delta(callParams)
	wantCallDelta := 0.636831
	if math.Abs(callDelta-wantCallDelta) > 0.001 {
		t.Errorf("ATM call delta = %.6f, want ~%.6f", callDelta, wantCallDelta)
	}

	putDelta := Delta(putParams)
	wantPutDelta := -0.363169
	if math.Abs(putDelta-wantPutDelta) > 0.001 {
		t.Errorf("ATM put delta = %.6f, want ~%.6f", putDelta, wantPutDelta)
	}

	// This is the whole point of CR-9: unlike the old constant-delta-0.5
	// model, real BS delta is NOT 0.5 away from expiry for an ATM option
	// under these params, and a short call + short put no longer cancel.
	if math.Abs(callDelta-0.5) < 1e-6 {
		t.Fatalf("call delta suspiciously exactly 0.5 -- looks like the old broken constant model")
	}
}

// TestBlackScholes_DeepITMApproachesIntrinsic sanity-checks behavior near
// expiry: as T -> 0, price converges to intrinsic value (no more time
// value), which is what makes theta decay (and gamma risk) real in the
// engine instead of the old flat "duration*0.5" credit.
func TestBlackScholes_DeepITMApproachesIntrinsic(t *testing.T) {
	p := BSParams{Spot: 21000, Strike: 20000, TimeToExpiry: minTimeToExpiry / 2, Rate: 0.065, Vol: 0.12, Kind: CallOption}
	got := Price(p)
	want := 1000.0 // intrinsic: spot-strike
	if math.Abs(got-want) > 0.5 {
		t.Errorf("near-expiry deep ITM call = %.2f, want ~%.2f (intrinsic)", got, want)
	}
}

// TestBlackScholes_PutCallDeltaRelationship checks the identity
// callDelta - putDelta = 1 across a range of moneyness -- another
// invariant a constant-0.5 model would fail.
func TestBlackScholes_PutCallDeltaRelationship(t *testing.T) {
	spots := []float64{18000, 19000, 20000, 21000, 22000}
	for _, spot := range spots {
		base := BSParams{Spot: spot, Strike: 20000, TimeToExpiry: 0.05, Rate: 0.065, Vol: 0.12}
		cp := base
		cp.Kind = CallOption
		pp := base
		pp.Kind = PutOption
		diff := Delta(cp) - Delta(pp)
		if math.Abs(diff-1.0) > 1e-6 {
			t.Errorf("spot=%.0f: callDelta-putDelta = %.6f, want 1.0", spot, diff)
		}
	}
}
