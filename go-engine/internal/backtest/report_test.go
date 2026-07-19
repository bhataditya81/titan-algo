package backtest

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestScaleCosts_DoublesChargesAndRecomputesEverything is the G-5
// -cost-multiplier proof: WP-10 could only hand-recompute a single
// aggregate expectancy number outside the tool. ScaleCosts must instead
// rescale every trade's charges, recompute NetPnL per trade, and get the
// whole report (including win/loss counts, since a thin winner can flip
// negative under 2x costs) consistently.
func TestScaleCosts_DoublesChargesAndRecomputesEverything(t *testing.T) {
	base := time.Date(2026, 1, 5, 15, 30, 0, 0, IST)
	trades := []Trade{
		{GrossPnL: 1000, Charges: 100, NetPnL: 900, CloseTime: base},                  // clear win even at 2x
		{GrossPnL: 50, Charges: 40, NetPnL: 10, CloseTime: base.AddDate(0, 0, 1)},     // flips to a loss at 2x (50-80=-30)
		{GrossPnL: -200, Charges: 20, NetPnL: -220, CloseTime: base.AddDate(0, 0, 2)}, // already a loss
	}
	r := buildReport("test", nil, trades, 0)

	wantNetBefore := 900.0 + 10.0 - 220.0
	if math.Abs(r.NetPnL-wantNetBefore) > 1e-9 {
		t.Fatalf("sanity: NetPnL before scaling = %.2f, want %.2f", r.NetPnL, wantNetBefore)
	}
	if r.WinCount != 2 || r.LossCount != 1 {
		t.Fatalf("sanity: before scaling wins=%d losses=%d, want 2/1", r.WinCount, r.LossCount)
	}

	ScaleCosts(r, 2.0)

	// Trade 0: 1000 - 200 = 800. Trade 1: 50 - 80 = -30 (flipped). Trade 2: -200-40=-240.
	wantCharges := 200.0 + 80.0 + 40.0
	wantNet := 800.0 - 30.0 - 240.0
	if math.Abs(r.TotalCharges-wantCharges) > 1e-9 {
		t.Errorf("TotalCharges after 2x = %.2f, want %.2f", r.TotalCharges, wantCharges)
	}
	if math.Abs(r.NetPnL-wantNet) > 1e-9 {
		t.Errorf("NetPnL after 2x = %.2f, want %.2f", r.NetPnL, wantNet)
	}
	if r.WinCount != 1 || r.LossCount != 2 {
		t.Errorf("after 2x costs wins=%d losses=%d, want 1/2 (trade 1 should flip to a loss)", r.WinCount, r.LossCount)
	}
	if len(r.ByMonth) == 0 {
		t.Errorf("expected a non-empty per-month breakdown after rescale")
	}
}

func TestScaleCosts_NoopAtMultiplierOne(t *testing.T) {
	trades := []Trade{{GrossPnL: 100, Charges: 10, NetPnL: 90, CloseTime: time.Now()}}
	r := buildReport("test", nil, trades, 0)
	before := *r
	ScaleCosts(r, 1.0)
	if r.NetPnL != before.NetPnL || r.TotalCharges != before.TotalCharges {
		t.Errorf("mult=1.0 should be a no-op, got NetPnL %.2f->%.2f Charges %.2f->%.2f",
			before.NetPnL, r.NetPnL, before.TotalCharges, r.TotalCharges)
	}
}

func TestScaleCosts_NilReportIsSafe(t *testing.T) {
	ScaleCosts(nil, 2.0) // must not panic
}

func TestConstantIVBanner_MentionsConstantIVAndPercentage(t *testing.T) {
	banner := ConstantIVBanner(0.12)
	if !strings.Contains(banner, "CONSTANT-IV MODE") || !strings.Contains(banner, "12.00%") {
		t.Errorf("banner missing expected content: %s", banner)
	}
}
