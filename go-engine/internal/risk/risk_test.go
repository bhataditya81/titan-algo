package risk

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const eps = 0.01 // half-paisa tolerance (Appendix B: no float equality without epsilon)

func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

func assertClose(t *testing.T, label string, got, want float64) {
	t.Helper()
	if !almostEqual(got, want) {
		t.Errorf("%s = %.6f, want %.6f (diff %.6f)", label, got, want, got-want)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Golden-value charge tests — expected values computed by hand from the
// FY 2025-26 rate card (DefaultChargeRates), matching the rates given in
// docs/REMEDIATION_PLAN.md WP-2 task 1.
// ─────────────────────────────────────────────────────────────────────────

func TestEstimateCharges_GoldenValues(t *testing.T) {
	cases := []struct {
		name      string
		price     float64
		qty       int
		tradeType TradeType
		side      OrderSide
		wantTotal float64
	}{
		// OptIntraday BUY 150 x 75: turnover 11250
		//   brokerage 20 (flat F&O)
		//   STT 0 (buy side, options STT is sell-only)
		//   txn 11250*0.03503% = 3.940875
		//   SEBI 11250*0.0001% = 0.01125
		//   GST 18% of (20+3.940875+0.01125=23.952125) = 4.3113825
		//   stamp 11250*0.003% (buy) = 0.3375
		//   total = 28.6010075
		{"OptIntraday BUY 150x75", 150, 75, OptIntraday, Buy, 28.6010075},

		// OptIntraday SELL 150 x 75: turnover 11250
		//   brokerage 20, STT 11250*0.1%=11.25, txn 3.940875, SEBI 0.01125
		//   GST 4.3113825 (same base as above), stamp 0 (sell)
		//   total = 39.5135075
		{"OptIntraday SELL 150x75", 150, 75, OptIntraday, Sell, 39.5135075},

		// FutCarry BUY 200 x 75: turnover 15000
		//   brokerage 20, STT 0 (buy), txn 15000*0.00173%=0.2595
		//   SEBI 15000*0.0001%=0.015, GST 18% of (20+0.2595+0.015=20.2745)=3.64941
		//   stamp 15000*0.002%(buy)=0.3
		//   total = 24.22391
		{"FutCarry BUY 200x75", 200, 75, FutCarry, Buy, 24.22391},

		// FutCarry SELL 200 x 75: turnover 15000
		//   brokerage 20, STT 15000*0.02%=3.0, txn 0.2595, SEBI 0.015
		//   GST 3.64941 (same base), stamp 0 (sell)
		//   total = 26.92391
		{"FutCarry SELL 200x75", 200, 75, FutCarry, Sell, 26.92391},

		// EquityIntraday BUY 100 x 10: turnover 1000
		//   brokerage min(20, 1000*0.03%=0.3)=0.3, STT 0 (buy)
		//   txn 1000*0.00297%=0.0297, SEBI 1000*0.0001%=0.001
		//   GST 18% of (0.3+0.0297+0.001=0.3307)=0.059526
		//   stamp 1000*0.003%(buy, intraday)=0.03
		//   total = 0.420226
		{"EquityIntraday BUY 100x10", 100, 10, EquityIntraday, Buy, 0.420226},

		// EquityIntraday SELL 100 x 10: turnover 1000
		//   brokerage 0.3, STT 1000*0.025%=0.25, txn 0.0297, SEBI 0.001
		//   GST 0.059526, stamp 0 (sell)
		//   total = 0.640226
		{"EquityIntraday SELL 100x10", 100, 10, EquityIntraday, Sell, 0.640226},

		// EquityDelivery BUY 100 x 10: turnover 1000
		//   brokerage 0.3, STT 1000*0.1%=1.0 (both sides), txn 0.0297, SEBI 0.001
		//   GST 0.059526, stamp 1000*0.015%(buy,delivery)=0.15
		//   total = 1.540226
		{"EquityDelivery BUY 100x10", 100, 10, EquityDelivery, Buy, 1.540226},

		// EquityDelivery SELL 100 x 10: turnover 1000
		//   brokerage 0.3, STT 1.0 (both sides), txn 0.0297, SEBI 0.001
		//   GST 0.059526, stamp 0 (delivery stamp is buy-only)
		//   total = 1.390226
		{"EquityDelivery SELL 100x10", 100, 10, EquityDelivery, Sell, 1.390226},

		// High-turnover equity: brokerage caps at flat 20, not 0.03% of turnover.
		// price=5000 qty=100 -> turnover 500000; 0.03% = 150, min(20,150) = 20.
		{"EquityIntraday BUY brokerage cap", 5000, 100, EquityIntraday, Buy, 0}, // wantTotal computed below
	}

	for _, c := range cases {
		if c.name == "EquityIntraday BUY brokerage cap" {
			continue // handled separately below (checks brokerage component, not total)
		}
		t.Run(c.name, func(t *testing.T) {
			got := EstimateCharges(c.price, c.qty, c.tradeType, c.side).Total
			assertClose(t, c.name, got, c.wantTotal)
		})
	}

	t.Run("EquityIntraday brokerage caps at flat fee", func(t *testing.T) {
		b := EstimateCharges(5000, 100, EquityIntraday, Buy)
		assertClose(t, "brokerage", b.Brokerage, 20.0)
	})
}

// FNO is a deprecated alias that must behave exactly like OptCarry.
func TestEstimateCharges_FNODeprecatedAliasMapsToOptions(t *testing.T) {
	got := EstimateCharges(150, 75, FNO, Sell)
	want := EstimateCharges(150, 75, OptCarry, Sell)
	if got != want {
		t.Errorf("FNO alias diverged from OptCarry: got %+v want %+v", got, want)
	}
	// And FNO must NOT be priced as a future (rates differ materially).
	fut := EstimateCharges(150, 75, FutCarry, Sell)
	if almostEqual(got.Total, fut.Total) {
		t.Errorf("FNO priced identically to FutCarry (%.4f) — deprecated alias must map to OPTIONS, not futures", fut.Total)
	}
}

// Worked example from the WP-2 report: 1 lot (75 qty) NIFTY option bought
// and sold at Rs 150 premium. Round-trip total should be ~Rs 68.
func TestEstimateCharges_WorkedExample_NiftyOptionRoundTrip(t *testing.T) {
	buy := EstimateCharges(150, 75, OptIntraday, Buy)
	sell := EstimateCharges(150, 75, OptIntraday, Sell)
	roundTrip := buy.Total + sell.Total

	assertClose(t, "buy leg total", buy.Total, 28.6010075)
	assertClose(t, "sell leg total", sell.Total, 39.5135075)
	assertClose(t, "round trip total", roundTrip, 68.115015)

	// Sanity-check itemized components against the plan doc's own numbers:
	// brokerage 40, STT 11.25 (sell only), txn ~7.88, GST ~8.6, stamp 0.34, SEBI ~0.02.
	assertClose(t, "brokerage x2", buy.Brokerage+sell.Brokerage, 40.0)
	assertClose(t, "STT (sell only)", buy.STT+sell.STT, 11.25)
	assertClose(t, "txn x2", buy.ExchangeTxn+sell.ExchangeTxn, 7.88175)
	assertClose(t, "GST x2", buy.GST+sell.GST, 8.622765)
	assertClose(t, "stamp (buy only)", buy.StampDuty+sell.StampDuty, 0.3375)
	assertClose(t, "SEBI x2", buy.SEBIFee+sell.SEBIFee, 0.0225)
}

// ─────────────────────────────────────────────────────────────────────────
// Throttle: sliding 60s window, reset via elapsed simulated time (EX-3).
// ─────────────────────────────────────────────────────────────────────────

func TestThrottle_ResetsOverSimulatedTime(t *testing.T) {
	rm := NewManager(100, 1_000_000, BrokerageConfig{}, StopLossConfig{}, 3)

	simNow := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	rm.now = func() time.Time { return simNow }

	rm.OpenPosition("A", 10, 1, EquityIntraday, Buy)
	rm.OpenPosition("B", 10, 1, EquityIntraday, Buy)
	rm.OpenPosition("C", 10, 1, EquityIntraday, Buy)

	if valid, reason := rm.ValidateOrder(10, 1, EquityIntraday, Buy); valid {
		t.Fatalf("expected 4th order within the same minute to be throttled, got valid=true")
	} else if !strings.Contains(reason, "rate limit") {
		t.Errorf("expected rate-limit reason, got %q", reason)
	}

	// Advance simulated time by just under a minute: still throttled.
	simNow = simNow.Add(59 * time.Second)
	if valid, _ := rm.ValidateOrder(10, 1, EquityIntraday, Buy); valid {
		t.Fatalf("expected order at +59s to still be throttled")
	}

	// Advance past 60s from the FIRST recorded order: window slides, capacity frees up.
	simNow = simNow.Add(2 * time.Second) // total +61s from first open
	if valid, reason := rm.ValidateOrder(10, 1, EquityIntraday, Buy); !valid {
		t.Fatalf("expected order after window slide to be valid, got reason=%q", reason)
	}
}

func TestSetMaxOrdersPerMin(t *testing.T) {
	rm := NewManager(100, 1_000_000, BrokerageConfig{}, StopLossConfig{}, 3)
	if got := rm.GetMaxOrdersPerMin(); got != 3 {
		t.Fatalf("initial MaxOrdersPerMin = %d, want 3", got)
	}
	rm.SetMaxOrdersPerMin(10)
	if got := rm.GetMaxOrdersPerMin(); got != 10 {
		t.Fatalf("after SetMaxOrdersPerMin(10) = %d, want 10", got)
	}
	rm.SetMaxOrdersPerMin(0)  // ignored
	rm.SetMaxOrdersPerMin(-5) // ignored
	if got := rm.GetMaxOrdersPerMin(); got != 10 {
		t.Fatalf("non-positive SetMaxOrdersPerMin should be ignored, got %d", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Margin-aware SELL validation (CR-13) — fail-closed.
// ─────────────────────────────────────────────────────────────────────────

func TestValidateOrderWithMargin_FailsClosedOnUnknownMargin(t *testing.T) {
	rm := NewManager(100, 1_000_000, BrokerageConfig{}, StopLossConfig{}, 100)

	for _, badMargin := range []float64{0, -1, -100} {
		valid, reason := rm.ValidateOrderWithMargin(150, 75, OptCarry, Sell, badMargin)
		if valid {
			t.Fatalf("SELL derivative with requiredMargin=%.2f must be REJECTED (fail-closed)", badMargin)
		}
		if !strings.Contains(reason, "fail-closed") {
			t.Errorf("expected fail-closed reason, got %q", reason)
		}
	}
}

func TestValidateOrderWithMargin_AcceptsValidMargin(t *testing.T) {
	rm := NewManager(100, 200_000, BrokerageConfig{}, StopLossConfig{}, 100)
	valid, reason := rm.ValidateOrderWithMargin(150, 75, OptCarry, Sell, 150_000)
	if !valid {
		t.Fatalf("expected valid margin-backed SELL to pass, got reason=%q", reason)
	}
}

func TestValidateOrderWithMargin_RejectsWhenMarginExceedsBalance(t *testing.T) {
	rm := NewManager(100, 50_000, BrokerageConfig{}, StopLossConfig{}, 100)
	valid, reason := rm.ValidateOrderWithMargin(150, 75, OptCarry, Sell, 150_000)
	if valid {
		t.Fatalf("expected margin exceeding balance to be rejected")
	}
	if !strings.Contains(reason, "Insufficient balance") {
		t.Errorf("expected insufficient-balance reason, got %q", reason)
	}
}

func TestValidateOrderWithMargin_BuySideIgnoresMissingMargin(t *testing.T) {
	rm := NewManager(100, 1_000_000, BrokerageConfig{}, StopLossConfig{}, 100)
	// BUY side must use premium*qty path regardless of requiredMargin.
	valid, reason := rm.ValidateOrderWithMargin(150, 75, OptCarry, Buy, 0)
	if !valid {
		t.Fatalf("expected BUY order to validate on premium path, got reason=%q", reason)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Locked-capital correctness (EX-9): margin locked for SELL derivatives,
// turnover+charges otherwise; released exactly on close/rollback; recomputed
// on fill-price correction.
// ─────────────────────────────────────────────────────────────────────────

func TestOpenPositionWithMargin_LocksMarginNotTurnover(t *testing.T) {
	rm := NewManager(100, 500_000, BrokerageConfig{}, StopLossConfig{}, 100)

	err := rm.OpenPositionWithMargin("NIFTY25JULCE", 150, 75, OptCarry, Sell, 120_000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pos := rm.GetOpenPositions()["NIFTY25JULCE"]
	if pos == nil {
		t.Fatalf("position not recorded")
	}
	wantCharges := EstimateCharges(150, 75, OptCarry, Sell).Total
	wantLocked := 120_000 + wantCharges
	assertClose(t, "LockedCapital", pos.LockedCapital, wantLocked)

	gotAvailable := rm.GetRemainingBalance()
	wantAvailable := 500_000 - wantLocked
	assertClose(t, "available balance", gotAvailable, wantAvailable)

	// Turnover would have been 150*75=11250 — locked capital must NOT equal that.
	if almostEqual(pos.LockedCapital, 11250+wantCharges) {
		t.Fatalf("locked capital used turnover instead of margin")
	}
}

func TestOpenPositionWithMargin_FailsClosedOnBadMargin(t *testing.T) {
	rm := NewManager(100, 500_000, BrokerageConfig{}, StopLossConfig{}, 100)
	err := rm.OpenPositionWithMargin("NIFTY25JULCE", 150, 75, OptCarry, Sell, 0)
	if err == nil {
		t.Fatalf("expected fail-closed error for SELL derivative with margin<=0")
	}
	if len(rm.GetOpenPositions()) != 0 {
		t.Fatalf("no position should have been recorded on fail-closed rejection")
	}
}

func TestClosePosition_ReleasesExactLockedCapital(t *testing.T) {
	rm := NewManager(100, 500_000, BrokerageConfig{}, StopLossConfig{}, 100)
	if err := rm.OpenPositionWithMargin("SYM", 150, 75, OptCarry, Sell, 120_000); err != nil {
		t.Fatalf("open failed: %v", err)
	}
	before := rm.GetRemainingBalance()
	if _, err := rm.ClosePosition("SYM", 140); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	after := rm.GetRemainingBalance()
	// LockedCapital (120000+charges) must have been fully released; only P&L
	// moves CurrentBalance from here on.
	if after <= before {
		t.Fatalf("expected available balance to increase after releasing locked margin: before=%.2f after=%.2f", before, after)
	}
	if len(rm.GetOpenPositions()) != 0 {
		t.Fatalf("position should be removed after close")
	}
}

func TestUpdatePositionPrice_RecomputesChargesAndLockedCapital(t *testing.T) {
	rm := NewManager(100, 500_000, BrokerageConfig{}, StopLossConfig{}, 100)
	rm.OpenPosition("SYM", 150, 75, OptIntraday, Buy)

	posBefore := rm.GetOpenPositions()["SYM"]
	chargesAt150 := EstimateCharges(150, 75, OptIntraday, Buy).Total
	assertClose(t, "initial EntryCharges", posBefore.EntryCharges, chargesAt150)

	rm.UpdatePositionPrice("SYM", 160)

	posAfter := rm.GetOpenPositions()["SYM"]
	chargesAt160 := EstimateCharges(160, 75, OptIntraday, Buy).Total
	assertClose(t, "recomputed EntryCharges", posAfter.EntryCharges, chargesAt160)
	if almostEqual(posAfter.EntryCharges, chargesAt150) {
		t.Fatalf("EntryCharges did not change after fill-price correction")
	}

	wantLocked := 160*75.0 + chargesAt160
	assertClose(t, "recomputed LockedCapital", posAfter.LockedCapital, wantLocked)
}

// ─────────────────────────────────────────────────────────────────────────
// CheckRisk: typed result, cheap, side-effect-free.
// ─────────────────────────────────────────────────────────────────────────

func TestCheckRisk_NotBreachedByDefault(t *testing.T) {
	rm := NewManager(5, 100_000, BrokerageConfig{}, StopLossConfig{}, 100)
	res := rm.CheckRisk()
	if res.Breached {
		t.Fatalf("fresh manager should not be breached, got reason=%q", res.Reason)
	}
}

func TestCheckRisk_KillSwitch(t *testing.T) {
	rm := NewManager(5, 100_000, BrokerageConfig{}, StopLossConfig{}, 100)
	rm.TriggerKillSwitch()
	res := rm.CheckRisk()
	if !res.Breached || !strings.Contains(res.Reason, "kill switch") {
		t.Fatalf("expected kill-switch breach, got %+v", res)
	}
	if !rm.KillSwitchActive() {
		t.Fatalf("KillSwitchActive() should report true after TriggerKillSwitch()")
	}
}

func TestCheckRisk_MaxDrawdown(t *testing.T) {
	rm := NewManager(5, 100_000, BrokerageConfig{}, StopLossConfig{}, 100) // 5% max drawdown
	rm.CurrentBalance = 94_000                                             // 6% down
	res := rm.CheckRisk()
	if !res.Breached || !strings.Contains(res.Reason, "drawdown") {
		t.Fatalf("expected drawdown breach, got %+v", res)
	}
}

func TestCheckRisk_BalanceDepleted(t *testing.T) {
	rm := NewManager(90, 100_000, BrokerageConfig{}, StopLossConfig{}, 100)
	rm.CurrentBalance = 0
	res := rm.CheckRisk()
	if !res.Breached || !strings.Contains(res.Reason, "depleted") {
		t.Fatalf("expected balance-depleted breach, got %+v", res)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Getter thread-safety (EX-9) + full race hammer test.
// Run with: go test -race ./internal/risk/...
// ─────────────────────────────────────────────────────────────────────────

func TestConcurrent_OpenCloseGettersKillSwitch_Race(t *testing.T) {
	rm := NewManager(50, 10_000_000, BrokerageConfig{}, StopLossConfig{}, 1000)
	priceGetter := func(string) float64 { return 155.0 }

	const goroutines = 40
	const iterations = 25

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				symbol := fmt.Sprintf("SYM-%d-%d", id, i)

				rm.ValidateOrder(150, 75, OptIntraday, Buy)
				rm.ValidateOrderWithMargin(150, 75, OptCarry, Sell, 120_000)
				rm.OpenPosition(symbol, 150, 75, OptIntraday, Buy)

				_ = rm.GetCurrentBalance()
				_ = rm.GetRealizedPnL()
				_ = rm.GetRemainingBalance()
				_ = rm.GetSessionStatsWithPrices(priceGetter)
				_ = rm.GetStopLossConfig()
				_ = rm.GetOpenPositions()
				_ = rm.CheckRisk()
				_ = rm.ShouldTriggerStopLoss(symbol, 149)

				rm.UpdatePositionPrice(symbol, 151)
				rm.ClosePosition(symbol, 152)
			}
		}(g)
	}

	// Concurrent kill-switch / throttle-limit mutators racing the workers above.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			rm.SetMaxOrdersPerMin(1000 + i)
			_ = rm.KillSwitchActive()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_ = rm.GetSessionStats()
			_ = rm.GetMaxOrdersPerMin()
		}
		rm.TriggerKillSwitch()
	}()

	wg.Wait()
	// If we get here under -race without the race detector firing, and
	// without a panic, thread-safety holds.
}
