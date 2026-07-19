package broker

import (
	"math"
	"strings"
	"testing"
)

func newConnectedMockBroker(t *testing.T, cfg PaperFillConfig, balance float64) *MockBroker {
	t.Helper()
	mb := NewMockBrokerWithConfig(balance, cfg)
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return mb
}

// --- task 1: spread-based fill pricing ---------------------------------

func TestPlaceOrder_SpreadPricing_OptionBuyAboveSellBelowLTP(t *testing.T) {
	cfg := PaperFillConfig{
		OptionHalfSpreadPct: 0.3,
		EquityHalfSpreadPct: 0.02,
		TypicalLiquidityQty: 75,
		Seed:                1,
	}
	mb := newConnectedMockBroker(t, cfg, 1_000_000)
	const symbol = "NIFTY25JUL26000CE"
	const ltp = 200.0
	mb.marketPrices[symbol] = ltp
	wantHalf := ltp * cfg.OptionHalfSpreadPct / 100.0

	buy, err := mb.PlaceOrder(Order{Symbol: symbol, Quantity: 75, Side: Buy})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if buy.FillPrice <= ltp {
		t.Fatalf("buy should fill above LTP: got %.4f, LTP %.4f", buy.FillPrice, ltp)
	}
	if math.Abs(buy.FillPrice-(ltp+wantHalf)) > 1e-9 {
		t.Fatalf("buy fill price = %.4f, want %.4f (LTP + configured half-spread)", buy.FillPrice, ltp+wantHalf)
	}

	// A fresh symbol so this SELL isn't closing the long just opened above.
	const symbol2 = "NIFTY25JUL26000PE"
	mb.marketPrices[symbol2] = ltp
	sell, err := mb.PlaceOrder(Order{Symbol: symbol2, Quantity: 75, Side: Sell})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if sell.FillPrice >= ltp {
		t.Fatalf("sell should fill below LTP: got %.4f, LTP %.4f", sell.FillPrice, ltp)
	}
	if math.Abs(sell.FillPrice-(ltp-wantHalf)) > 1e-9 {
		t.Fatalf("sell fill price = %.4f, want %.4f (LTP - configured half-spread)", sell.FillPrice, ltp-wantHalf)
	}
}

func TestPlaceOrder_SpreadPricing_EquityUsesSmallerHalfSpread(t *testing.T) {
	cfg := PaperFillConfig{
		OptionHalfSpreadPct: 0.3,
		EquityHalfSpreadPct: 0.02,
		TypicalLiquidityQty: 75,
		Seed:                1,
	}
	mb := newConnectedMockBroker(t, cfg, 1_000_000)
	const symbol = "RELIANCE"
	const ltp = 2500.0
	mb.marketPrices[symbol] = ltp

	buy, err := mb.PlaceOrder(Order{Symbol: symbol, Quantity: 10, Side: Buy})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	wantHalf := ltp * cfg.EquityHalfSpreadPct / 100.0
	if math.Abs(buy.FillPrice-(ltp+wantHalf)) > 1e-9 {
		t.Fatalf("equity buy fill price = %.4f, want %.4f (equity half-spread, not option's)", buy.FillPrice, ltp+wantHalf)
	}
}

// --- task 2: size-scaled slippage ---------------------------------------

func TestPlaceOrder_SlippageScalesWithSize(t *testing.T) {
	cfg := PaperFillConfig{
		OptionHalfSpreadPct: 0.3,
		EquityHalfSpreadPct: 0.02,
		TypicalLiquidityQty: 75,
		Seed:                1,
	}
	mb := newConnectedMockBroker(t, cfg, 10_000_000)
	const ltp = 200.0
	mb.marketPrices["SMALL"] = ltp
	mb.marketPrices["BIG"] = ltp

	small, err := mb.PlaceOrder(Order{Symbol: "SMALL", Quantity: 75, Side: Buy}) // at reference size: scale=1
	if err != nil {
		t.Fatalf("small order: %v", err)
	}
	big, err := mb.PlaceOrder(Order{Symbol: "BIG", Quantity: 300, Side: Buy}) // 4x reference: scale=sqrt(4)=2
	if err != nil {
		t.Fatalf("big order: %v", err)
	}

	if big.Slippage <= small.Slippage {
		t.Fatalf("expected larger order to slip more: small=%.4f big=%.4f", small.Slippage, big.Slippage)
	}
	wantRatio := 2.0 // sqrt(300/75)
	gotRatio := big.Slippage / small.Slippage
	if math.Abs(gotRatio-wantRatio) > 1e-6 {
		t.Fatalf("slippage ratio = %.4f, want %.4f (sqrt(qty/typicalLiquidity) scaling)", gotRatio, wantRatio)
	}
}

// --- task 3: partial fills ------------------------------------------------

func TestPlaceOrder_PartialFillRate_StatisticalOverManyTrials(t *testing.T) {
	cfg := PaperFillConfig{
		OptionHalfSpreadPct: 0.3,
		EquityHalfSpreadPct: 0.02,
		TypicalLiquidityQty: 75,
		PartialFillRate:     0.07,
		PartialFillMinFrac:  0.4,
		RejectionRate:       0, // isolate: only measuring partial-fill behavior here
		Seed:                42,
	}
	mb := newConnectedMockBroker(t, cfg, 1e9)
	mb.marketPrices["RELIANCE"] = 200

	const trials = 2000
	partials := 0
	for i := 0; i < trials; i++ {
		filled, err := mb.PlaceOrder(Order{Symbol: "RELIANCE", Quantity: 100, Side: Buy})
		if err != nil {
			t.Fatalf("trial %d: unexpected error: %v", i, err)
		}
		if filled.Partial() {
			partials++
			if filled.Quantity >= filled.RequestedQty || filled.Quantity < 1 {
				t.Fatalf("trial %d: partial fill qty %d out of range (requested %d)", i, filled.Quantity, filled.RequestedQty)
			}
		}
		mb.openPositions = map[string]*Position{} // reset so repeated buys don't change opening/closing branch
	}

	rate := float64(partials) / float64(trials)
	if rate < 0.04 || rate > 0.11 {
		t.Fatalf("partial-fill rate = %.4f (%d/%d), want ~%.2f (configured PartialFillRate)", rate, partials, trials, cfg.PartialFillRate)
	}
}

// --- task 4: margin on shorts (the "money printer" fix) -------------------

func TestPlaceOrder_ShortMargin_BeforeAndAfterMoneyPrinterFix(t *testing.T) {
	const symbol = "NIFTY25JUL26000CE"
	const premium = 150.0
	const qty = 75

	// BEFORE (legacy config): SELL credited full turnover as free cash, no margin locked.
	legacy := newConnectedMockBroker(t, LegacyPaperFillConfig(), 100000)
	legacy.marketPrices[symbol] = premium
	beforeLegacy := legacy.GetBalance()
	legacyFill, err := legacy.PlaceOrder(Order{Symbol: symbol, Quantity: qty, Side: Sell})
	if err != nil {
		t.Fatalf("legacy sell: %v", err)
	}
	if legacy.marginReserved[symbol] != 0 {
		t.Fatalf("legacy config must not reserve margin, got %.2f", legacy.marginReserved[symbol])
	}
	if legacy.GetBalance() <= beforeLegacy {
		t.Fatalf("legacy (money-printer) SELL should credit turnover as free cash: before=%.2f after=%.2f", beforeLegacy, legacy.GetBalance())
	}
	t.Logf("BEFORE fix: SELL %d lot @ %.2f -> balance %.2f -> %.2f (margin locked: 0)",
		qty, legacyFill.FillPrice, beforeLegacy, legacy.GetBalance())

	// AFTER (realistic config): SELL reserves margin, does not credit turnover as profit.
	cfg := DefaultPaperFillConfig()
	cfg.RejectionRate = 0
	cfg.PartialFillRate = 0
	cfg.Seed = 5
	realistic := newConnectedMockBroker(t, cfg, 100000)
	realistic.marketPrices[symbol] = premium
	beforeReal := realistic.GetBalance()
	realFill, err := realistic.PlaceOrder(Order{Symbol: symbol, Quantity: qty, Side: Sell})
	if err != nil {
		t.Fatalf("realistic sell: %v", err)
	}
	margin := realistic.marginReserved[symbol]
	if margin <= 0 {
		t.Fatalf("expected margin reserved > 0 after the fix, got %.2f", margin)
	}
	if realistic.GetBalance() >= beforeReal {
		t.Fatalf("fixed SELL must NOT credit free turnover: before=%.2f after=%.2f", beforeReal, realistic.GetBalance())
	}
	wantMargin := realFill.FillPrice * float64(realFill.Quantity) * (1 + cfg.ShortMarginPct)
	if math.Abs(margin-wantMargin) > 1.0 {
		t.Fatalf("margin reserved = %.2f, want ~%.2f (premium*qty*(1+%.2f))", margin, wantMargin, cfg.ShortMarginPct)
	}
	t.Logf("AFTER fix: SELL %d lot @ %.2f -> balance %.2f -> %.2f (margin locked: ~%.2f)",
		qty, realFill.FillPrice, beforeReal, realistic.GetBalance(), margin)
}

func TestPlaceOrder_ShortMargin_InsufficientBalanceBlocksOpeningAShort(t *testing.T) {
	cfg := DefaultPaperFillConfig()
	cfg.RejectionRate = 0
	cfg.PartialFillRate = 0
	cfg.Seed = 3
	mb := newConnectedMockBroker(t, cfg, 100) // far too little to margin a 75-lot short
	mb.marketPrices["NIFTY25JUL26000CE"] = 150

	_, err := mb.PlaceOrder(Order{Symbol: "NIFTY25JUL26000CE", Quantity: 75, Side: Sell})
	if err == nil {
		t.Fatalf("expected insufficient balance/margin error (money-printer bug would let this through for free)")
	}
	if !strings.Contains(err.Error(), "insufficient balance/margin") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlaceOrder_CoveringShort_ReleasesMarginAndRealizesPnL(t *testing.T) {
	cfg := DefaultPaperFillConfig()
	cfg.RejectionRate = 0
	cfg.PartialFillRate = 0
	cfg.Seed = 9
	mb := newConnectedMockBroker(t, cfg, 100000)
	const symbol = "NIFTY25JUL26000CE"
	mb.marketPrices[symbol] = 150

	sellFill, err := mb.PlaceOrder(Order{Symbol: symbol, Quantity: 75, Side: Sell})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	balAfterSell := mb.GetBalance()
	if mb.marginReserved[symbol] <= 0 {
		t.Fatalf("expected margin reserved after opening the short")
	}

	// Price drops before covering: favorable for the short seller.
	mb.marketPrices[symbol] = 100
	buyFill, err := mb.PlaceOrder(Order{Symbol: symbol, Quantity: 75, Side: Buy})
	if err != nil {
		t.Fatalf("buy to cover: %v", err)
	}

	if mb.marginReserved[symbol] != 0 {
		t.Fatalf("expected margin fully released after covering the whole quantity, got %.2f", mb.marginReserved[symbol])
	}
	if _, open := mb.GetPositions()[symbol]; open {
		t.Fatalf("expected position fully closed after covering the whole quantity")
	}
	if mb.GetBalance() <= balAfterSell {
		t.Fatalf("expected a profit on a favorable cover: balAfterSell=%.2f balAfterCover=%.2f", balAfterSell, mb.GetBalance())
	}
	t.Logf("sell @ %.2f (margin locked) -> cover @ %.2f -> balance %.2f -> %.2f",
		sellFill.FillPrice, buyFill.FillPrice, balAfterSell, mb.GetBalance())
}

// --- task 5: occasional rejections -----------------------------------------

func TestPlaceOrder_RejectionRate_StatisticalOverManyTrials(t *testing.T) {
	cfg := PaperFillConfig{
		OptionHalfSpreadPct: 0.3,
		EquityHalfSpreadPct: 0.02,
		TypicalLiquidityQty: 75,
		PartialFillRate:     0, // isolate: only measuring rejection behavior here
		RejectionRate:       0.03,
		Seed:                7,
	}
	mb := newConnectedMockBroker(t, cfg, 1e9)
	mb.marketPrices["RELIANCE"] = 200

	const trials = 2000
	rejects := 0
	for i := 0; i < trials; i++ {
		_, err := mb.PlaceOrder(Order{Symbol: "RELIANCE", Quantity: 100, Side: Buy})
		if err != nil {
			if !strings.Contains(err.Error(), "REJECTED by exchange") {
				t.Fatalf("trial %d: unexpected error: %v", i, err)
			}
			rejects++
			continue
		}
		mb.openPositions = map[string]*Position{}
	}

	rate := float64(rejects) / float64(trials)
	if rate < 0.015 || rate > 0.05 {
		t.Fatalf("rejection rate = %.4f (%d/%d), want ~%.2f (configured RejectionRate)", rate, rejects, trials, cfg.RejectionRate)
	}
}

// --- task 6: backward compatibility / legacy mode --------------------------

func TestNewMockBroker_DefaultConstructionStillCompiles(t *testing.T) {
	mb := NewMockBroker(10000) // pre-R2-4 call sites (cmd/main.go, titan.go, engine tests) use this signature unchanged
	if err := mb.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := mb.Subscribe([]string{"TESTSYM"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if mb.GetBalance() != 10000 {
		t.Fatalf("balance = %.2f, want 10000", mb.GetBalance())
	}
}

func TestLegacyPaperFillConfig_DeterministicNoiseFreeFills(t *testing.T) {
	mb := newConnectedMockBroker(t, LegacyPaperFillConfig(), 100_000_000)
	mb.marketPrices["TESTSYM"] = 1000

	// Legacy config: no partials, no rejections, tiny flat spread, run many
	// orders and confirm every single one fills completely (deterministic).
	for i := 0; i < 200; i++ {
		filled, err := mb.PlaceOrder(Order{Symbol: "TESTSYM", Quantity: 10, Side: Buy})
		if err != nil {
			t.Fatalf("trial %d: unexpected error under legacy config: %v", i, err)
		}
		if filled.Partial() {
			t.Fatalf("trial %d: legacy config must never partially fill", i)
		}
	}
}
