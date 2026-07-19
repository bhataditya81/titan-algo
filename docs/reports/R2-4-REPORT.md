# R2-4 — Honest Paper Fills — Report

Scope: `internal/broker/mock.go`, `internal/broker/live_paper_broker.go` only. No other files touched. (`broker.go`, `angel_broker.go`, `ws_feed.go` are R2-1's this round and were only read, never edited — R2-1's `GetRequiredMargin`/`ws_feed.go` work was already present in the tree when this ran and is untouched.)

## Gap addressed

G-6 / Known Bug B-2 (`docs/PRODUCTION_GAPS_R2.md`): `MockBroker` filled every order instantly, in full, at LTP ± a token 0.05-0.10% slippage, and credited SELL orders with full turnover as if it were free cash — "the money printer." Paper-trading results systematically flattered every strategy versus what a real broker would do, making the 1-month paper gate meaningless.

## What changed

### 1. Spread model (task 1)
`PlaceOrder` now fills at `LTP ± halfSpread` instead of a flat random band. Half-spread is a percentage of the reference price: `OptionHalfSpreadPct` (default **0.3%**) for option symbols, `EquityHalfSpreadPct` (default **0.02%**) for everything else (equity, futures). Buys fill higher, sells fill lower — always unfavorable to the trader, no more "lucky" fills.

Symbol classification (`isOptionSymbol`/`isDerivativeSymbol` in `mock.go`) uses the Angel/NSE naming convention: option symbols end in `CE`/`PE` **with a digit immediately before the suffix** (the strike price), e.g. `NIFTY25JUL26000CE`. Futures end in `FUT`.

**Bug caught and fixed during testing:** a naive `strings.HasSuffix(symbol, "CE")` misclassifies plain equity tickers that happen to end in those two letters — `RELIANCE` ends in `"CE"`. The digit-before-suffix check (`isOptionSymbol`) avoids this; a synthetic test (`TestPlaceOrder_SpreadPricing_EquityUsesSmallerHalfSpread`) caught it before it shipped.

### 2. Size-scaled slippage (task 2)
The half-spread is multiplied by `sqrt(quantity / TypicalLiquidityQty)` whenever `quantity` exceeds the reference size (default `TypicalLiquidityQty = 75`, one NIFTY lot); orders at or below the reference get the plain half-spread (scale = 1, never scaled *down*). `TypicalLiquidityQty = 0` disables scaling entirely.

### 3. Partial fills (task 3)
`PartialFillRate` (default **0.07**) is the probability an order fills for less than requested; when it does, the filled fraction is uniform in `[PartialFillMinFrac, 1.0)` (default min fraction **0.4**). This reuses WP-1's exact `FilledOrder.RequestedQty`/`Partial()` contract — `Quantity` is the actual fill, `RequestedQty` the ask, `Partial()` reports the difference. `Status` is set to `"cancelled"` on a partial (matching `broker.go`'s documented convention: `"cancelled" (partial-then-cancelled)`) and `"complete"` otherwise, mirroring how `AngelBroker` reports it.

### 4. Margin on shorts — the money-printer fix (task 4)
`MarginOnShorts` (default **true**) + `ShortMarginPct` (default **0.13**, i.e. 13% of notional) control this. When a SELL order **opens or adds to a short position** in an option or future, `MockBroker` now reserves `turnover * (1 + ShortMarginPct)` as locked margin (`marginReserved[symbol]`) and debits that plus the brokerage fee from the virtual balance — it no longer credits the premium as free cash. This is an explicit approximation (premium + a flat % of notional as a SPAN-ish proxy), **not real SPAN margin**; once R2-1's `GetRequiredMargin` is wired end-to-end by R2-INT, live/paper margin decisions for the actual risk gate should come from there (this only affects `MockBroker`'s own displayed balance/margin bookkeeping, which is informational — `internal/engine/engine.go` already treats `risk.Manager`, not `broker.GetBalance()`, as the real risk-accounting authority; `broker.GetBalance()` only feeds CSV/stat logging).

Covering a short (a BUY that reduces/closes an existing short position) releases the proportional reserved margin and realizes P&L against the short's average price — and is **never** balance-gated, matching how reduce-only orders bypass the real broker's circuit breaker (a position-reducing trade must always be allowed to proceed). Only orders that **open or increase** exposure (`existing == nil || existing.Side == order.Side`) are gated on available balance/margin.

Equity SELL and closing an existing long derivative position are unaffected (still credit turnover normally) — task 4 is scoped to option/future SELL-to-open, not equity short-selling rules.

**Before/after example** (from `TestPlaceOrder_ShortMargin_BeforeAndAfterMoneyPrinterFix`), SELL 1 lot (75) NIFTY option @ premium ₹150:
- **Before (`LegacyPaperFillConfig`)**: balance goes **up** by `150*75 - fee ≈ ₹11,230`, margin locked = **₹0**. Free money for shorting.
- **After (`DefaultPaperFillConfig`)**: balance goes **down** by `~fillPrice*75*(1+0.13) + fee ≈ ₹12,730`, margin locked ≈ **₹12,710**. No premium credited; capital is consumed, exactly like a real short.

### 5. Occasional rejections (task 5)
`RejectionRate` (default **0.03**) is the probability an order is rejected outright before any state changes, with the error `"order %s REJECTED by exchange: margin insufficient (simulated paper rejection)"` — same phrasing (`"REJECTED by exchange"`) `AngelBroker.PlaceOrder` uses for a genuine exchange rejection (`angel_broker.go:547`), so paper mode now exercises `runner.go`'s multi-leg unwind path (`unwindLegs`) the same way a real rejection would.

### 6. Configurability / backward compatibility (task 6)
New `PaperFillConfig` struct with `DefaultPaperFillConfig()` (the realistic behavior above) and `LegacyPaperFillConfig()` (pre-R2-4 behavior: flat ~0.075% slippage, no size scaling, no partials, no rejections, no margin lock — for tests that want pure, noise-free control flow).

**Constructor signatures — fully backward compatible, no breaking changes:**
- `NewMockBroker(initialBalance float64) *MockBroker` — **unchanged signature**, now internally calls `NewMockBrokerWithConfig(initialBalance, DefaultPaperFillConfig())`.
- `NewMockBrokerWithConfig(initialBalance float64, cfg PaperFillConfig) *MockBroker` — **new**, for custom/seeded/legacy configs.
- `NewLivePaperBroker(live TradeService, initialBalance float64) *LivePaperBroker` — **unchanged signature**, now backed by `NewMockBroker` (realistic defaults).
- `NewLivePaperBrokerWithConfig(live TradeService, initialBalance float64, cfg PaperFillConfig) *LivePaperBroker` — **new**.

All RNG (`math/rand`) is now an explicit `*rand.Rand` field on `MockBroker` seeded from `PaperFillConfig.Seed` (nonzero → deterministic; zero → seeded from wall-clock time, appropriate for a live paper session) — no unseeded global `math/rand` source is used anywhere in the file. `GetCurrentPrice`/`GetCurrentVolume`/`Subscribe`'s price-walk randomness was also switched to this same instance RNG (previously used the global source) for full determinism under a fixed seed. All RNG access is already inside the pre-existing `m.mu` lock, so this is `-race` clean without further synchronization.

## Call sites checked (grep across the whole repo)

```
go-engine\internal\engine\runner_smoke_test.go:48   mb := broker.NewMockBroker(10000)
go-engine\internal\app\titan.go:79                  app.TradeService = broker.NewMockBroker(app.sessionBalance)
go-engine\internal\app\titan.go:76                  app.TradeService = broker.NewLivePaperBroker(angelBroker, app.sessionBalance)
go-engine\cmd\main.go:138                           tradeService = broker.NewMockBroker(*sessionBalance)
go-engine\cmd\main.go:135                           tradeService = broker.NewLivePaperBroker(angelBroker, *sessionBalance)
```

All five compile unchanged (`go build ./...` is clean across the whole module — verified). No signature was changed, so **no call site needs an edit to keep building.**

### One behavioral note, not a build break

`internal/engine/runner_smoke_test.go` (`TestRunnerSmoke`, an engine-package test from Round 1/WP-9, not owned by R2-4) calls `broker.NewMockBroker(10000)` and places a handful of orders while asserting exact structural outcomes (`len(runner.open) == 1` after entry, etc.). Now that `NewMockBroker` defaults to `DefaultPaperFillConfig()` — nondeterministic (time-seeded) partial fills (~7%) and rejections (~3%) — that test has become intermittently flaky: verified by running it 5x in a row, it failed 3/5 times with errors like `"expected 1 open structure after entry, got 0"` and `"expected re-entry on restarted runner"` (a rejected/partial order legitimately changes what the engine does, which is the whole point of this work package — but this particular test wasn't written to expect that).

This is exactly the scenario `docs/PRODUCTION_GAPS_R2.md`'s R2-4 task 6 anticipated ("check if existing tests elsewhere in the repo construct MockBroker directly and would break... note exactly which existing tests need a one-line update"). Recommended one-line fix for whoever owns `internal/engine` (R2-INT or a follow-up):

```go
// runner_smoke_test.go:48 — was:
mb := broker.NewMockBroker(10000)
// change to:
mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
```

`TestRunnerSmoke` exercises control flow (kill-switch, pause/resume, restart/recovery, graceful shutdown), not fill realism, so `LegacyPaperFillConfig()` is the correct choice for it — it restores deterministic, always-complete fills. **I did not make this edit myself** — `internal/engine/*` is not an R2-4-owned file.

## Test evidence

New file: `internal/broker/mock_test.go` (14 new tests). Full package run:

```
$ cd go-engine && go build ./internal/broker/... && go vet ./internal/broker/... && go test -race ./internal/broker/...
ok  	titan-algo/internal/broker	18.9s
```

All pre-existing tests in the package (R2-1's `angel_broker_test.go`, `instruments_test.go`, `historical_test.go` — including R2-1's already-landed `GetRequiredMargin`/WS-feed tests) still pass, plus the 14 new ones:

```
--- PASS: TestPlaceOrder_SpreadPricing_OptionBuyAboveSellBelowLTP        [task 1]
--- PASS: TestPlaceOrder_SpreadPricing_EquityUsesSmallerHalfSpread       [task 1]
--- PASS: TestPlaceOrder_SlippageScalesWithSize                          [task 2]
--- PASS: TestPlaceOrder_PartialFillRate_StatisticalOverManyTrials       [task 3] (2000 trials, seed=42)
--- PASS: TestPlaceOrder_ShortMargin_BeforeAndAfterMoneyPrinterFix       [task 4]
--- PASS: TestPlaceOrder_ShortMargin_InsufficientBalanceBlocksOpeningAShort [task 4]
--- PASS: TestPlaceOrder_CoveringShort_ReleasesMarginAndRealizesPnL      [task 4]
--- PASS: TestPlaceOrder_RejectionRate_StatisticalOverManyTrials         [task 5] (2000 trials, seed=7)
--- PASS: TestNewMockBroker_DefaultConstructionStillCompiles             [task 6 / backward compat]
--- PASS: TestLegacyPaperFillConfig_DeterministicNoiseFreeFills          [task 6 / legacy mode]
```

(Plus `updatePosition`/crossover tests are unchanged and still pass — `updatePosition` itself was not modified.)

Whole-module sanity (not required by acceptance criteria, run anyway since paper-broker constructors are called from `cmd/main.go`/`internal/app/titan.go`):
```
$ go build ./...    → clean
$ go vet ./...       → clean
```

## Not done / explicitly out of scope

- `ExtendedTradeService` (`PlaceStopLossOrder`, `CancelOrder`, `Healthy`, `HealthError`, `RefreshBalance`, `GetCurrentPriceWithAge`) is **not** implemented on `MockBroker`/`LivePaperBroker` — not asked for by the R2-4 task list, and WP-1's report explicitly left this as a follow-up decision for whoever wires paper-mode parity for those calls.
- Real SPAN margin: `ShortMarginPct` is a flat, configurable approximation, not exchange-calculated margin. R2-1's `GetRequiredMargin` (already present in `angel_broker.go` as of this writing) is the real source once R2-INT wires it into the paper path if that's ever desired; `MockBroker`'s own balance is informational only (see task 4 discussion above), so this is a documentation note, not a functional gap for R2-4's scope.
- Equity short-selling margin (as opposed to option/future short margin) is intentionally out of scope per the task list ("SELL derivative orders").
- `internal/engine/runner_smoke_test.go` flakiness — documented above, not fixed (not an R2-4-owned file).
