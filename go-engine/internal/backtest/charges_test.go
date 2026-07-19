package backtest

import (
	"math"
	"testing"
)

// TestLegCharge_WorkedExample hand-computes the charge for a 1-lot (75 qty)
// NIFTY option at Rs.150 premium, buy side and sell side, per the FY
// 2025-26 rate card (EX-4) now owned by internal/risk (risk.EstimateCharges,
// which LegCharge delegates to per WP-2 task 7's "unified fee model").
//
// turnover = 150 * 75 = 11250
//
// BUY side:
//
//	brokerage = 20
//	stt       = 0 (buy side exempt)
//	txn       = 11250 * 0.03503% = 3.940875
//	stamp     = 11250 * 0.003%   = 0.3375
//	sebi      = 11250 * 0.0001%  = 0.01125
//	gst       = (20 + 3.940875 + 0.01125) * 18% ~= 4.311383
//	total     = 20 + 3.940875 + 0.3375 + 0.01125 + 4.311383 ~= 28.601
//
// SELL side:
//
//	brokerage = 20
//	stt       = 11250 * 0.1%     = 11.25
//	txn       = 3.940875
//	stamp     = 0 (sell side exempt)
//	sebi      = 0.01125
//	gst       = (20 + 3.940875 + 0.01125) * 18% ~= 4.311383
//	total     = 20 + 11.25 + 3.940875 + 0.01125 + 4.311383 ~= 39.513
func TestLegCharge_WorkedExample(t *testing.T) {
	premium := 150.0
	qty := 75

	buy := LegCharge(premium, qty, LegBuy)
	wantBuy := 28.601
	if math.Abs(buy-wantBuy) > 0.01 {
		t.Errorf("buy-side charge = %.4f, want ~%.4f", buy, wantBuy)
	}

	sell := LegCharge(premium, qty, LegSell)
	wantSell := 39.513
	if math.Abs(sell-wantSell) > 0.01 {
		t.Errorf("sell-side charge = %.4f, want ~%.4f", sell, wantSell)
	}

	// The old backtest charged buy-side charges twice and never charged
	// sell-side STT at all (ST-6). Prove sell > buy here specifically
	// because of the STT asymmetry this fixes.
	if sell <= buy {
		t.Errorf("sell-side charge (%.4f) should exceed buy-side (%.4f): options STT is sell-side only", sell, buy)
	}
}

func TestLegCharge_PerLegPerSide(t *testing.T) {
	// A 4-leg iron fly round trip should be charged as 8 separate orders
	// (4 legs x entry+exit), not "one flat brokerage" (ST-6 bug).
	legPremiums := []float64{40, 40, 120, 120} // wings cheap, body expensive
	qty := 75

	var total float64
	for _, prem := range legPremiums {
		total += LegCharge(prem, qty, LegBuy)  // entry
		total += LegCharge(prem, qty, LegSell) // exit
	}

	// Sanity: 8 orders at Rs 20 flat brokerage alone is already Rs 160,
	// well above any single "one flat fee" the old model charged.
	if total < 160 {
		t.Errorf("4-leg round trip total charges = %.2f, expected at least the Rs 160 brokerage floor (8 orders x Rs 20)", total)
	}
}

func TestSpreadCost(t *testing.T) {
	got := SpreadCost(0.003, 150, 75)
	want := 150 * 0.003 * 75 // 33.75
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("SpreadCost = %.4f, want %.4f", got, want)
	}
}
