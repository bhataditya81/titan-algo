# R2-3 — Real Data, IV, and Backtest Harness Upgrades — Report

**Scope:** `cmd/backtest/main.go`, `internal/backtest/*.go` (except `engine.go`), `cmd/fetchdata/main.go` (new), `docs/validation/*`.
**Gaps addressed:** G-3 (real data), G-4 (constant-IV), G-5 (CLI parameter sweeps/cost stress/lot size).

## 0. Mandatory reading done first

Read in full before writing anything: `docs/PRODUCTION_GAPS_R2.md` (all of §2/§3, focused on G-3/G-4/G-5 and the R2-3 section), `docs/reports/WP-7-REPORT.md`, `docs/reports/WP-10-REPORT.md`, and `internal/backtest/bs.go`, `cache.go`, `config.go`, `engine.go`, `report.go`, `types.go`, `cmd/backtest/main.go`, `internal/broker/angel_broker.go`, `historical.go`, `instruments.go`, `internal/config/config.go`, `internal/strategy/strategy.go`/`registry.go`. `docs/reports/R2-2-REPORT.md` was also read once it appeared (see §4 — R2-2 landed mid-session).

## 1. `cmd/fetchdata` (new binary)

### What it does

- Authenticates via `internal/broker.NewAngelBroker(...).Connect()` — the exact same TOTP+password login path `cmd/backtest` and the live engine use. No HTTP call is reimplemented.
- Pulls candles via the existing `AngelBroker.FetchHistory(symbol, interval, days)` (`internal/broker/historical.go`) — reused as-is, not reimplemented.
- Writes output via `internal/backtest.SaveCandlesCSV` — **exactly** the WP-7 cache format (verified by round-trip test: fetch → write → `backtest.LoadCandlesCSV` reads it back byte-for-byte equivalent).
- Rate-limits historical requests with a local `golang.org/x/time/rate.Limiter` (same package/pattern `angel_broker.go` uses for order rate-limiting; AngelBroker doesn't expose one for historical calls, so this is fetchdata's own, waited outside any lock, same idiom).
- Resumes an interrupted run: a JSON checkpoint (`<out>/.fetchdata_checkpoint.json`) marks each target "done" immediately after its CSV is written; a restart skips completed targets and only re-fetches what's left.
- Optionally fetches option-chain candles for a given underlying/expiry/strike list, resolving the exact NFO trading symbol via `broker.NewInstrumentManager().LoadInstruments()` + `Search()` (the public instrument master, independent of any broker session) instead of hand-constructing Angel's symbol-naming convention.

### READ-ONLY guard

`cmd/fetchdata/main.go` talks to the broker **only** through a locally-defined `historyFetcher` interface:
```go
type historyFetcher interface {
    Connect() error
    FetchHistory(symbol, interval string, days int) ([]backtest.Candle, error)
}
```
This interface has no `PlaceOrder`/`PlaceStopLossOrder`/`CancelOrder` method, so there is no code path in this binary that can reach an order-placement broker method, even though the concrete `*broker.AngelBroker` passed into it has them. This is a compile-time guarantee, not just a comment (though the comment is there too).

### Credential gate

`resolveCreds()` reads `ANGEL_CLIENT_CODE`/`ANGEL_PIN`/`ANGEL_API_KEY`/`ANGEL_TOTP_SECRET` (the exact names from `internal/config`'s exported `Env*` constants) from the environment **only**. If `config.yaml` parses with any non-empty Angel credential field, the tool refuses **even if the env vars are also set** — it never reads credential *values* from YAML at all, matching WP-8's live-mode gate philosophy applied unconditionally (fetching real market data always deserves that gate). If any of the four env vars is missing, it refuses.

### Usage (for the human running this later, per H-4)

```bash
cd go-engine
go build -o fetchdata.exe ./cmd/fetchdata

# Rotate credentials first (H-1), then, in the SAME shell (never in config.yaml):
export ANGEL_CLIENT_CODE=...
export ANGEL_PIN=...
export ANGEL_API_KEY=...
export ANGEL_TOTP_SECRET=...

./fetchdata.exe -underlyings NIFTY,BANKNIFTY -years 2 -interval FIVE_MINUTE \
  -out data/historical -rps 0.33

# Optional option-chain fetch for one expiry/strike range:
./fetchdata.exe -underlyings NIFTY -option-underlying NIFTY \
  -option-expiry 2026-01-29 -option-strikes 21800,21900,22000,22100,22200 \
  -option-types CE,PE -out data/historical
```
If killed (Ctrl+C, crash, reboot), rerun the exact same command — the checkpoint skips whatever already has a `.csv` on disk.

### Known limitation (discrepancy, not worked around)

`AngelBroker.FetchHistory(symbol, interval, days)` always fetches `[now-days, now]` — it has **no** parameter for an arbitrary historical `[from, to]` window. `historical.go` is outside this WP's edit scope (only `cmd/backtest/main.go`, `internal/backtest/*` except `engine.go`, `cmd/fetchdata/main.go`, and `docs/validation/*` are owned this round), so `cmd/fetchdata` cannot chunk a 2-year backfill into successive historical windows — it can only ask Angel for "N days back from today" in one call per symbol, exactly like `cmd/backtest`'s existing `fetch()` closure already does. Whether Angel's real historical endpoint accepts a single ~730-day request for 5-minute candles is unverified (untestable without real credentials, which this WP is barred from using). **Follow-up for a future WP:** extend `FetchHistory` to accept an explicit `from`/`to` and have `cmd/fetchdata` chunk requests (e.g. 60-day windows) — needs `internal/broker` file access this round didn't have.

A second, related limitation: `AngelBroker`'s `baseURL` field is unexported with no setter, and `FetchHistory` hardcodes Angel's live URL directly (doesn't even consult `a.baseURL`) — so **no code in any package can redirect the real broker client to a fake server**, including this WP's own tests. `cmd/fetchdata`'s tests instead exercise a `historyFetcher`-typed fake that itself makes real calls to a local `httptest.Server`, proving fetchdata's own logic (rate limiting, resume, credential gate, CSV format) for real; the network leg through the concrete `*AngelBroker` is untestable without editing `internal/broker` (R2-1's files this round). Flagged for R2-1/a future WP: expose a `SetBaseURL` (or accept one in `NewAngelBroker`) and route `FetchHistory` through it, matching the pattern the rest of the broker already uses (`angel_broker_test.go` sets `b.baseURL` directly because it's in-package; nothing outside `internal/broker` can).

Strike-value ambiguity: the instrument master's `strike` field is sometimes rupees, sometimes paise (×100), depending on Angel's version/segment, and `instruments.go`'s `StrikeFloat` doesn't normalize either way. `findOptionSymbol` matches against both the raw value and value÷100 so it works under either convention — documented in code rather than guessed at.

### Test evidence

`go-engine/cmd/fetchdata/main_test.go`, all against a real local `httptest.Server`:
- `TestResolveCreds_MissingEnvVarsRefuses`, `TestResolveCreds_AllEnvVarsPresentSucceeds`, `TestResolveCreds_YAMLCredentialsRefusedEvenWithEnvSet`, `TestResolveCreds_MissingYAMLFileIsFine`
- `TestRunFetch_CSVRoundTripMatchesCacheFormat` — fetch → `backtest.LoadCandlesCSV` reads it back, values match
- `TestRunFetch_RespectsRateLimit` — 3 calls at 5rps/burst1 takes ≥350ms
- `TestRunFetch_ResumesAfterInterrupt` — first run fails mid-way through target 2 of 3; asserts target 1's CSV exists, target 2's doesn't; second run re-fetches only the 2 unfinished targets and skips the completed one
- `TestStrikeMatches_RawAndPaiseConventions`

```
go build ./cmd/fetchdata/...   clean
go vet ./cmd/fetchdata/...     clean
go test -race ./cmd/fetchdata/...
ok  	titan-algo/cmd/fetchdata	4.8s
```

## 2. IV modeling (G-4)

### `internal/backtest/iv.go` (new)

```go
func ImpliedVol(marketPrice, spot, strike, timeToExpiry, rate float64, kind OptionKind) (float64, error)
func BuildIVSeries(underlying, optionCandles []Candle, strike, rate float64, expiry time.Time, kind OptionKind) map[time.Time]float64
func SummarizeIVSeries(series map[time.Time]float64) IVSeriesStats
```

`ImpliedVol` inverts `bs.go`'s `Price` via **bisection** (not Newton-Raphson, per the task's own preference — `Price` is monotonic in `Vol` and bisection needs no derivative bs.go doesn't already compute). Bracket `[1e-6, 5.0]` (500% vol upper bound). Fails closed (returns an error, never a guessed value) when the market price is below intrinsic (arbitrage violation) or above what even 500% vol produces (implausible input) — never silently clamps.

**Golden round-trip test** (`iv_test.go`, `TestImpliedVol_GoldenRoundTrip`): for 5 cases (ATM call/put, OTM, ITM, deep-OTM-high-vol) — price at a known IV via `Price()`, invert with `ImpliedVol`, assert recovered IV within `1e-4` of the original, **and** reprice at the recovered IV and assert it reproduces the original market price within `1e-3` (the actual round-trip proof, not just "close on vol"). Plus `TestImpliedVol_BelowIntrinsicIsError`, `TestImpliedVol_ImplausiblyHighPriceIsError`, `TestImpliedVol_NonPositivePriceIsError`.

`BuildIVSeries` aligns option candles to underlying candles by timestamp and inverts per bar, silently skipping bars with no matching option print (never interpolates/guesses) — tested (`TestBuildIVSeries_MatchesByTimeAndSkipsGaps`) with a synthetic gap to prove missing bars are omitted, not zero-filled.

### Wiring into the engine — blocked by file ownership, not skipped silently

The task asks to wire the per-bar IV series into "the backtest engine's option-leg repricing path... when real option-candle data with enough coverage exists." That repricing path (`priceLeg`, `openMultiLeg`, `closePosition`, `markToMarket`, `Run` — all in `internal/backtest/engine.go`) is the file this round's hard constraint explicitly excludes me from editing (**R2-2 owns `engine.go`'s EvalContext-construction section this round**). `priceLeg` calls `Price(BSParams{..., Vol: cfg.IV, ...})` — a raw `float64` field, not a per-bar lookup — so making it consult a per-bar series requires an edit inside `engine.go` itself, which I could not make.

What I built instead, right up to that boundary: the inverter, the per-bar series builder, and an **informational** coverage report (`cmd/backtest`'s new `-option-csv`/`-option-strike`/`-option-expiry`/`-option-type` flags load a fetched option-candle file, compute `BuildIVSeries`, and print mean/min/max/count) — proving the whole pipeline works end to end, without touching the one file that would make it load-bearing.

Because the constant-IV path is therefore the **only** path `Run()` exercises this round regardless of whether option data is supplied, `cmd/backtest` now prints `backtest.ConstantIVBanner(cfg.IV)` **unconditionally**, at the top of every report:
```
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
CONSTANT-IV MODE -- vega/skew risk NOT modeled. Every option leg
below is repriced all run at a single fixed IV = 12.00%. Real IV
moves with strikes/expiry/time and spikes exactly when short-vol
strategies are losing. Any short_straddle/iron_fly/nine_twenty
result here is an artifact of the assumed IV level, NOT a
demonstrated edge -- see WP-10's IV-sensitivity finding (PF
0.29->1.66 on IV alone) and docs/reports/R2-3-REPORT.md.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
```

**Exact one-line change needed once `engine.go` is open to R2-2/R2-INT again:** in `priceLeg` (engine.go), replace `Vol: cfg.IV` with a per-bar lookup (e.g. a `Config.IVAt(asOf time.Time) float64` method consulting a map built once from `BuildIVSeries`, falling back to `cfg.IV` when no entry exists for that bar) — everything that method needs (`ImpliedVol`, `BuildIVSeries`) already exists, is tested, and lives in `internal/backtest/iv.go`.

## 3. New CLI flags on `cmd/backtest` (G-5)

| Flag | Effect |
|---|---|
| `-params key=val,key2=val2` | Parsed to `map[string]float64`, passed to `strategy.GetWithParams` (see §4 — landed mid-session, wired for real via an `init()` in `cmd/backtest/main.go`). |
| `-cost-multiplier` (default 1.0) | Scales every trade's `Charges` (brokerage/STT/txn/GST/stamp — all inside `risk.EstimateCharges` — plus the modeled half-spread) by this factor and **recomputes the entire report** (win/loss counts can flip, not just the headline expectancy) via new `internal/backtest.ScaleCosts`. Replaces WP-10's "recompute 2x-cost expectancy by hand outside the tool" workaround with something that actually reruns the numbers, including win/loss reclassification the old manual approach couldn't do. |
| `-instrument-cache-dir` (default `data/instruments`) | If a cached instrument-master JSON (the same `scripmaster_YYYY-MM-DD.json` `internal/broker.InstrumentManager` writes, or one `cmd/fetchdata`'s option-resolution step downloads) contains a lot size for `-symbol`, it overrides `-lotsize`; otherwise falls back to `-lotsize` exactly as before. |
| `-option-csv` / `-option-strike` / `-option-expiry` / `-option-type` | Optional: informational implied-IV coverage summary (see §2) — never applied to repricing. |

`internal/backtest/report.go` gained `ScaleCosts(r *Report, mult float64)` and `ConstantIVBanner(iv float64) string`; `buildReport`'s aggregation loop was factored into a new `recomputeAggregates` so both the original build path and `ScaleCosts` share it (no duplicated aggregation logic).

### Test evidence

```
go build ./internal/backtest/... ./cmd/backtest/... ./cmd/fetchdata/...   clean
go vet   ./internal/backtest/... ./cmd/backtest/... ./cmd/fetchdata/...   clean
go build ./...   clean (whole module, including R2-2's landed internal/strategy changes)
go vet   ./...   clean
go test -race ./internal/backtest/...
ok  	titan-algo/internal/backtest	~2.3s
go test -race ./cmd/fetchdata/...
ok  	titan-algo/cmd/fetchdata	~4.8s
```
New tests: `report_test.go` (`TestScaleCosts_DoublesChargesAndRecomputesEverything` — proves a thin winner flips to a loss under 2x costs and the whole report, not just NetPnL, is consistent; `TestScaleCosts_NoopAtMultiplierOne`; `TestScaleCosts_NilReportIsSafe`; `TestConstantIVBanner_MentionsConstantIVAndPercentage`), `params_test.go` (`TestResolveStrategy_EmptyParamsUsesPlainGet`, `TestResolveStrategy_ParamsWithoutHookFailsClosed`, `TestResolveStrategy_ParamsWithHookDelegates`), `iv_test.go` (§2). One `go vet`/`gofmt` pass across all new/changed files; all pre-existing `internal/backtest` tests (13 functions from WP-7) still pass unchanged.

## 4. R2-2 dependency — what was needed, and what actually landed

Per the task's instruction, `internal/backtest/params.go` was built against a local seam (`ResolveStrategy`, `StrategyWithParams` package var, `ErrParamsUnsupported`) since R2-2 was running concurrently and hadn't landed when this work started. **R2-2 landed during this session** with exactly the anticipated contract:
```go
func strategy.GetWithParams(name string, params map[string]float64) (Strategy, error)
```
(confirmed by reading `docs/reports/R2-2-REPORT.md` and `internal/strategy/registry.go` directly — field names are the exact Go struct field names on each `<Strategy>Params`, e.g. `RSIReversalParams{Period, Oversold, Overbought}`, `ShortStraddleParams{RSILower, RSIUpper, StopMultiplier}`, `EMACrossoverParams{FastPeriod, SlowPeriod}` — case-sensitive, reflect-based field mapping.)

Once confirmed present, `cmd/backtest/main.go` was updated with:
```go
func init() {
	backtest.StrategyWithParams = strategy.GetWithParams
}
```
turning the seam into a real, working feature (not left as a stub) — this is the "don't block on it, don't reimplement it yourself" instruction honored in both directions: didn't block waiting for R2-2, and once it landed, wired it for real rather than leaving dead scaffolding. If `StrategyWithParams` is ever unset again (e.g. a future refactor), `ResolveStrategy` still fails closed with `ErrParamsUnsupported` and `cmd/backtest` degrades to the strategy's compiled-in defaults with a loud `WARNING` log line rather than crashing — this fallback path is still exercised by `TestResolveStrategy_ParamsWithoutHookFailsClosed`.

No discrepancy found: R2-2's actual signature matches the contract this WP was built against field-for-field.

## 5. `docs/validation/run_walkforward.py` update

`run_backtest()` now accepts `cost_multiplier` and `params` and passes `-cost-multiplier`/`-params` through to the CLI. Two new end-to-end demonstrations were added and run against the **same existing synthetic dataset** WP-10 generated (`docs/validation/data/nifty_synthetic_5min.csv`) — no real data was fetched or used, per the constraint:

- **`real_cost_multiplier_check()`**: reruns 6 (strategy, window) pairs at `-cost-multiplier=2.0` and asserts the result matches WP-10's old manual-arithmetic prediction (`NetPnL' = NetPnL - Charges`) to the penny — a real correctness check, not just "didn't crash". All 6 matched exactly:
  ```
  [cost-mult-demo] ema_crossover   2024-10-01..2024-10-31  predicted_2x=   291163.95 actual_2x=   291163.95 match=True
  [cost-mult-demo] ema_crossover   2024-11-01..2024-11-30  predicted_2x=    74419.93 actual_2x=    74419.93 match=True
  ```
  (writes `docs/validation/out/cost_multiplier_check.csv`)

- **`real_params_sweep()`**: runs `PARAM_GRID` (real `<Strategy>Params` field names) across `rsi_reversal`, `short_straddle`, `ema_crossover` on the Oct-2024 shock month, and asserts trade counts actually differ across the grid (if `-params` were silently ignored, every row would be identical):
  ```
  [params-demo] rsi_reversal    {'Period': 2}   trades= 166 net=    86277.74
  [params-demo] rsi_reversal    {'Period': 5}   trades=  22 net=    13213.75
  [params-demo] rsi_reversal    {'Period': 14}  trades=   0 net=        0.00
  [params-demo] short_straddle  {'StopMultiplier': 0.1}  trades=  71 net=  -218383.78
  [params-demo] short_straddle  {'StopMultiplier': 1.4}  trades=  61 net=  -195468.10
  [params-demo] short_straddle  {'StopMultiplier': 5.0}  trades=  61 net=  -195468.10
  [params-demo] ema_crossover   {'FastPeriod': 9, 'SlowPeriod': 21}  trades=  19 net=   295024.71
  [params-demo] ema_crossover   {'FastPeriod': 5, 'SlowPeriod': 40}  trades=   1 net=   -14834.45
  ```
  (writes `docs/validation/out/params_flag_demo.csv`) — note `StopMultiplier=1.4` and `5.0` are identical on this window (the stop simply never binds there; `0.1` — a genuinely tight stop — does differ), which is a real property of this data window, not a wiring failure (confirmed separately with a wider multiplier spread on the same window before settling the grid).

Full rerun of `gen_synthetic_data.py`'s existing dataset through `run_walkforward.py` + `summarize.py` end to end: 168 walk-forward rows + 48 sensitivity rows + 6 cost-multiplier rows + 8 params rows, all written, all consistent with WP-10's original findings (no regressions — e.g. `nine_twenty`/`iron_fly` still fail decisively, `ema_crossover` still the only one clearing PF). One incidental observation: `sniper` now produces 1039 trades across the 24 windows (WP-10 reported 0) — this is R2-2's sniper fix (G-2), landed concurrently and orthogonal to this WP; not something R2-3 changed.

`docs/validation/bin/backtest.exe` was rebuilt from the updated `cmd/backtest` source for this run.

## 6. Files changed/created (all within the R2-3 file-ownership matrix)

- `internal/backtest/report.go` — refactored `buildReport`/added `recomputeAggregates`; added `ScaleCosts`, `ConstantIVBanner`.
- `internal/backtest/iv.go` — new: `ImpliedVol`, `BuildIVSeries`, `IVSeriesStats`/`SummarizeIVSeries`.
- `internal/backtest/params.go` — new: `ResolveStrategy`, `StrategyWithParams`, `ErrParamsUnsupported`.
- `internal/backtest/iv_test.go`, `params_test.go`, `report_test.go` — new tests.
- `cmd/backtest/main.go` — new flags (`-params`, `-cost-multiplier`, `-instrument-cache-dir`, `-option-csv`/`-option-strike`/`-option-expiry`/`-option-type`); lot-size-from-instrument-cache; `ScaleCosts`/`ConstantIVBanner` wiring; `init()` wiring `strategy.GetWithParams`.
- `cmd/fetchdata/main.go`, `main_test.go` — new binary (§1).
- `docs/validation/run_walkforward.py` — `-cost-multiplier`/`-params` support, two new end-to-end demonstration functions.
- `docs/validation/out/cost_multiplier_check.csv`, `params_flag_demo.csv` — new demonstration output (plus refreshed `walkforward_windows.csv`/`sensitivity_grid.csv`/`strategy_summary.csv` from the rerun).
- `engine.go` — **not modified** (excluded this round; see §2 for the exact follow-up).

## 7. Honest gaps carried forward

- IV series is built and tested but not consumed by `Run()`/`priceLeg` — blocked on `engine.go` ownership this round (§2). Every backtest number remains constant-IV until that one-line change lands.
- `cmd/fetchdata` cannot chunk a multi-year backfill into date-windowed requests (`FetchHistory` has no `from`/`to` parameter) — untested against Angel's real day-count limits since no real fetch was performed (§1).
- No integration test exercises `*broker.AngelBroker`'s actual HTTP client end-to-end (its `baseURL` isn't overridable from outside `internal/broker`, and `FetchHistory` hardcodes the live URL) — fetchdata's own logic is fully tested via a `historyFetcher` fake backed by a real `httptest.Server`; the concrete broker's network path is not (§1).
- Option-chain fetch resolves symbols via the public instrument master search + a raw/paise strike-matching heuristic — untested against real strike-field conventions (no real instrument master was fetched).
