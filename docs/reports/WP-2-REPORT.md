# WP-2 — Risk Engine Correctness — Report

## Owned files touched
- `go-engine/internal/risk/risk.go` (rewritten)
- `go-engine/internal/risk/risk_test.go` (new)

`go-engine/examples/risk_example.go` reviewed — still compiles/runs unchanged against the new API (no edit needed; it only uses the legacy `ValidateOrder`/`OpenPosition`/`ClosePosition`/`ResetOrderCount` surface, which is fully preserved).

No other files touched.

## Findings addressed

| Finding | Fix |
|---|---|
| EX-4 (charges wrong for FY 2025-26, FUT/OPT conflated) | New `ChargeRates` table (`DefaultChargeRates()`), FY 2025-26 rates hardcoded per the plan's spec. `TradeType` split into `FutIntraday`/`FutCarry`/`OptIntraday`/`OptCarry`; `FNO` kept as deprecated alias, normalized to options. |
| EX-3 (throttle broken — `ResetOrderCount` had no callers) | Replaced the per-session counter with a sliding 60s window (`orderTimes []time.Time`, pruned on each check). Throttle now genuinely resets as time passes, no ticker/caller dependency. `SetMaxOrdersPerMin`/`GetMaxOrdersPerMin` exported, mutex-guarded. `ResetOrderCount` kept (now correctly locked) for compat. |
| EX-9 (unsynchronized getters) | `GetCurrentBalance`, `GetRealizedPnL`, `GetRemainingBalance`, `GetSessionStatsWithPrices`, new `GetStopLossConfig` all take `m.mu.RLock()`. `GetSessionStatsWithPrices` computes unrealized P&L under the **same** lock acquisition it used to take separately (was two locks, a TOCTOU window). `ShouldTriggerStopLoss` now always takes the write lock (the trailing flag itself was previously read before the lock was chosen). |
| CR-13 (no margin model, SELL sized by premium) | New `ValidateOrderWithMargin(price, qty, tradeType, side, requiredMargin)`: for derivative (`Fut*`/`Opt*`/`FNO`) SELL, validates `requiredMargin` (caller-supplied, from broker margin API) against available balance. BUY and equity keep the premium×qty path. **Fail-closed**: `requiredMargin <= 0` on a derivative SELL → rejected, reason contains `"fail-closed"`. |
| EX-9 / locked-capital correctness | New `Position.LockedCapital` (and `Position.Margin`) — the *exact* amount locked at entry. `OpenPositionWithMargin` locks `requiredMargin + charges` for derivative SELLs (fail-closed on bad margin), vs. `OpenPosition`'s legacy `turnover + charges`. `RollbackPosition`/`ClosePosition` release `position.LockedCapital` exactly — no more recomputing turnover from `EntryPrice` (which drifted from what was actually locked once margin-based entries exist). `UpdatePositionPrice` recomputes `EntryCharges` at the corrected fill price and adjusts `LockedCapital` to match (margin component untouched — it came from the broker, not the premium). |
| CR-2 (`CheckRisk` dead code / side effects) | `CheckRisk() RiskCheckResult` (`{Breached bool; Reason string}`), pure — no logging, no mutation, one `RLock`. Safe to call every tick. |
| CR-3 (kill switch config-load-time only, unsynchronized bool) | `killSwitch atomic.Bool` + `TriggerKillSwitch()` / `KillSwitchActive()`. Legacy `Manager.KillSwitch bool` field **kept** (see Interfaces below) so existing `cmd/main.go`/`titan.go` call sites keep compiling; `KillSwitchActive()` and `CheckRisk()` honor both. |
| EX-4 (fee math duplicated/disagreeing, e.g. `angel_broker.go:811`) | `EstimateCharges(...)` exported as the single source of truth (see signature below). |

## Interfaces exported for other packages / WP-9

### `EstimateCharges` — single source of truth for fee math
```go
func EstimateCharges(price float64, quantity int, tradeType TradeType, side OrderSide) ChargeBreakdown

type ChargeBreakdown struct {
    Turnover, Brokerage, STT, ExchangeTxn, SEBIFee, GST, StampDuty, Total float64
}
```
- `price`: per-unit price (option/future premium per unit, or equity price).
- `quantity`: total units (lots × lot size already applied).
- `tradeType`: `risk.EquityDelivery | EquityIntraday | FutIntraday | FutCarry | OptIntraday | OptCarry` (or deprecated `risk.FNO`, normalized to `OptCarry`).
- Uses `DefaultChargeRates()` (FY 2025-26). For a custom rate card, use `EstimateChargesWithRates(rates ChargeRates, ...)`.
- **Cross-checked against `internal/backtest/charges.go` (WP-7's independent implementation)**: same STT/txn/stamp/SEBI/GST percentages for options, confirming both packages converged on the correct FY26 numbers independently. WP-9 should have `internal/backtest` (and `internal/broker`'s `angel_broker.go:811` fee estimate) call `risk.EstimateCharges` directly and delete their local copies, per the plan's unification goal.

### `ValidateOrderWithMargin` / `OpenPositionWithMargin` — margin-aware SELL path
```go
func (m *Manager) ValidateOrderWithMargin(price float64, quantity int, tradeType TradeType, side OrderSide, requiredMargin float64) (bool, string)
func (m *Manager) OpenPositionWithMargin(symbol string, price float64, quantity int, tradeType TradeType, side OrderSide, requiredMargin float64) error
```
- For SELL + derivative (`IsDerivative(tradeType)==true`): validates/locks `requiredMargin + charges`. `requiredMargin` must come from Angel's margin API (A-6, `internal/broker` — WP-1) fetched by WP-9 before the order. `requiredMargin <= 0` → **rejected** (`ValidateOrderWithMargin` returns `false`; `OpenPositionWithMargin` returns a non-nil `error`), fail-closed, no position recorded.
- For BUY (any trade type): behaves exactly like `ValidateOrder`/`OpenPosition` (premium×qty); `requiredMargin` is ignored.
- `IsDerivative(t TradeType) bool` is also exported, in case WP-9 needs to branch on it directly (e.g. to decide whether to call the margin API at all).
- The legacy `ValidateOrder`/`OpenPosition` (premium×qty for all sides, including derivative SELL) are **still present and functional** for paper-mode/equity flows, but carry an explicit doc-comment warning that they understate real margin for derivative SELLs — WP-9 must route derivative SELL entries through the `*WithMargin` variants for anything beyond paper trading.

### `CheckRisk` — typed, cheap, side-effect-free
```go
type RiskCheckResult struct {
    Breached bool
    Reason   string
}
func (m *Manager) CheckRisk() RiskCheckResult
```
Checks (in order): kill switch active → balance depleted (`CurrentBalance <= 0`) → max drawdown (`(InitialBalance-CurrentBalance)/InitialBalance*100 >= MaxDrawdownPercent`). No logging, one `RLock`. WP-9 calls this every tick and decides the response (halt entries / flatten / alert).

### Kill switch
```go
func (m *Manager) TriggerKillSwitch()
func (m *Manager) KillSwitchActive() bool
```
`TriggerKillSwitch` is safe from any goroutine (API handler, `data/KILL` file watcher, tick loop) — backed by `atomic.Bool`, one-way for the session. `KillSwitchActive()` (and `CheckRisk()`) also honor the legacy `Manager.KillSwitch` bool field for backward compatibility — see below.

**Legacy field kept on purpose:** `cmd/main.go:223,230,365` and `internal/app/titan.go:99` do `riskMgr.KillSwitch = config.Risk.KillSwitchEnabled` / `if riskMgr.KillSwitch`. Rather than break those (outside my ownership) by making `KillSwitch` a bare `atomic.Bool`, I kept `KillSwitch bool` as a field (now documented deprecated) alongside the new `killSwitch atomic.Bool`. Both call sites **still compile unmodified**. Recommendation for WP-9 (not required, just cleaner): replace the two assignment sites with
```go
if config.Risk.KillSwitchEnabled {
    riskMgr.TriggerKillSwitch()
}
```
and the two read sites with `riskMgr.KillSwitchActive()`, then this field can be unexported/removed in a follow-up.

### Throttle
```go
func (m *Manager) SetMaxOrdersPerMin(n int) // n<=0 ignored
func (m *Manager) GetMaxOrdersPerMin() int
```
Internally a sliding 60s window (`[]time.Time` of entry-order timestamps, pruned lazily). No ticker goroutine needed — correctness doesn't depend on anyone calling `ResetOrderCount` (kept, now locked, as a no-op-equivalent manual clear for compatibility). `Manager` also has an unexported injectable clock (`now func() time.Time`, defaults to `time.Now`) used by the throttle and `Position.EntryTime`; tests set it directly (white-box, `package risk`) to simulate elapsed time without real sleeps.

## `BrokerageConfig` — kept, now deprecated/ignored for math

I deliberately did **not** restructure `BrokerageConfig`'s field shape. `config.yaml`'s `risk.brokerage.stamp_duty` is a scalar (`0.015`); had I turned `StampDuty` into a per-trade-type struct (needed for FY26 correctness), `gopkg.in/yaml.v2` would fail to unmarshal that field and (depending on version behavior) could break `config.Load` / paper-mode startup — and `config.yaml`/`config.go` are outside my ownership. Instead:
- `BrokerageConfig`'s fields are unchanged (same names/types/yaml tags) — `cmd/main.go`'s local `Config` struct and `internal/config/config.go`'s `Config.Risk.Brokerage risk.BrokerageConfig` embed it exactly as before; YAML parsing is unaffected.
- All charge math now goes through `EstimateCharges`/`DefaultChargeRates()` and **ignores** `BrokerageConfig`'s statutory sub-fields entirely (this is the audit's own fix direction — the YAML rates were wrong; hardcoding the correct table in code, not YAML, is what "single editable struct/table" in the task means). This is called out in `BrokerageConfig`'s doc comment.
- `NewManager`'s signature is unchanged; it still accepts `brokerageConfig BrokerageConfig` for compatibility but no longer uses it for fee math.

## Full-repo build status

`go build ./internal/risk/... ./examples/...` and `go vet` both pass clean. `go build ./...` at the repo root currently fails, but **not because of anything in this package** — it fails in `internal/backtest` (`report.go:92` etc., undefined fields) and `internal/strategy` (`Evaluate` signature mismatch vs. the new `EvalContext` interface) — both mid-flight changes from WP-7/WP-6 running in parallel, confirmed via `go vet ./cmd/... ./internal/app/... ./internal/engine/...` which fails only inside `internal/strategy`, never inside anything I touched. `internal/risk` and `examples/risk_example.go` have zero compile errors, standalone or as dependencies.

Grep-confirmed: `cmd/main.go` and `internal/app/titan.go`'s direct `riskMgr.KillSwitch` field reads/writes, and their `risk.NewManager(...)`/`config.Risk.Brokerage` usage, all still type-check against the new `risk.go` (verified by inspection — the fields/signatures they touch are unchanged) — these two files were not otherwise part of the run since another WP's breakage prevented a full `go build ./...`, but there is no risk-package-side change that would break them beyond what's already documented above.

## Test evidence

`go test -race ./internal/risk/...` — **19/19 pass**, 3.1s:
- `TestEstimateCharges_GoldenValues` (9 subtests: OptIntraday/FutCarry/EquityIntraday/EquityDelivery × Buy/Sell, plus brokerage-cap case) — hand-computed expected values in test comments, asserted with a 1-paisa epsilon.
- `TestEstimateCharges_FNODeprecatedAliasMapsToOptions` — `FNO` charges == `OptCarry` charges, and materially differ from `FutCarry`.
- `TestEstimateCharges_WorkedExample_NiftyOptionRoundTrip` — the report's worked example, asserted component-by-component.
- `TestThrottle_ResetsOverSimulatedTime` — injected clock; 3 orders fill the window (max=3), 4th throttled, still throttled at +59s, valid again at +61s from the first order (sliding window, no real sleep).
- `TestSetMaxOrdersPerMin`.
- `TestValidateOrderWithMargin_FailsClosedOnUnknownMargin` (0, -1, -100 all rejected), `_AcceptsValidMargin`, `_RejectsWhenMarginExceedsBalance`, `_BuySideIgnoresMissingMargin`.
- `TestOpenPositionWithMargin_LocksMarginNotTurnover` (asserts `LockedCapital == margin+charges`, not `turnover+charges`), `_FailsClosedOnBadMargin`.
- `TestClosePosition_ReleasesExactLockedCapital`.
- `TestUpdatePositionPrice_RecomputesChargesAndLockedCapital`.
- `TestCheckRisk_NotBreachedByDefault`, `_KillSwitch`, `_MaxDrawdown`, `_BalanceDepleted`.
- `TestConcurrent_OpenCloseGettersKillSwitch_Race` — 40 goroutines × 25 iterations hammering `ValidateOrder`, `ValidateOrderWithMargin`, `OpenPosition`, all getters (`GetCurrentBalance`, `GetRealizedPnL`, `GetRemainingBalance`, `GetSessionStatsWithPrices`, `GetStopLossConfig`, `GetOpenPositions`), `CheckRisk`, `ShouldTriggerStopLoss`, `UpdatePositionPrice`, `ClosePosition`, concurrently with goroutines spamming `SetMaxOrdersPerMin`/`KillSwitchActive`/`GetSessionStats`/`GetMaxOrdersPerMin` and a `TriggerKillSwitch`. Clean under `-race`.

`gofmt -l` clean on all three files. `go vet ./internal/risk/... ./examples/...` clean.

## Worked example: 1 lot (75 qty) NIFTY option, buy + sell at ₹150 premium

Computed by `EstimateCharges(150, 75, risk.OptIntraday, side)` (identical for `OptCarry`/`FNO` — charge rates don't distinguish intraday/carry, only margin product type does):

| Component | Buy leg | Sell leg | Round trip |
|---|---:|---:|---:|
| Turnover | ₹11,250.00 | ₹11,250.00 | — |
| Brokerage | ₹20.00 | ₹20.00 | ₹40.00 |
| STT | ₹0.00 | ₹11.25 | ₹11.25 |
| Exchange txn | ₹3.940875 | ₹3.940875 | ₹7.88175 |
| SEBI fee | ₹0.01125 | ₹0.01125 | ₹0.0225 |
| GST (18% of brokerage+txn+SEBI) | ₹4.3113825 | ₹4.3113825 | ₹8.622765 |
| Stamp duty (buy only) | ₹0.3375 | ₹0.00 | ₹0.3375 |
| **Total** | **₹28.6010075** | **₹39.5135075** | **₹68.115015** |

Rounded: buy ≈ ₹28.60, sell ≈ ₹39.51, **round-trip total ≈ ₹68.12**.

**Discrepancy vs. the plan doc:** `docs/REMEDIATION_PLAN.md`'s WP-2 acceptance section headlines "Expected order of magnitude: ~₹47" but then itemizes brokerage ₹40 + STT ₹11.25 + txn ₹7.88 + GST ~8.6 + stamp ₹0.34 + SEBI ~0.02, which sums to **≈₹68**, not ₹47. Every itemized figure in the plan matches this implementation's output almost exactly (₹40, ₹11.25, ₹7.88, ₹8.62, ₹0.34, ₹0.02 above). I treat "~₹47" as a typo in the plan doc and rely on its own itemized numbers, which this implementation reproduces to the paisa.

## Discrepancies vs. audit line numbers

- Audit cites `risk.go:463-483` for dead `CheckRisk` and `risk.go:355-357` for `ResetOrderCount`/throttle and `risk.go:360-372, 446-447` for unsynchronized getters — all still accurate against the pre-change file (line numbers drifted only because the file has since been fully rewritten by this WP; the described defects were real and are what's fixed above).
- Audit's `angel_broker.go:811` (disagreeing fee estimate) is outside my ownership (WP-1); flagging for WP-9/WP-1 to switch to `risk.EstimateCharges`.
- `internal/backtest/charges.go` (WP-7, in-flight) already implements an equivalent, independently-derived FY26 options rate table as a decoupling fallback ("otherwise implement your own... reconcile later"). Rates agree with mine to the same precision — no drift to reconcile for options; WP-7's fallback has no futures/equity rates (options-only), so it can be replaced by `risk.EstimateCharges` outright once WP-9 wires the dependency.
