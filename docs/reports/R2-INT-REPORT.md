# R2-INT — Wave B Integration — Report

Scope owned: `cmd/main.go`, `internal/engine/*`, `internal/app/titan.go`,
`internal/config/config.go`, plus one wiring-only addition outside that set
(`internal/api/server.go`: a `Token()` getter — see task 8). No other file
was touched. `config.yaml` was **not** edited (confirmed by `git status`
below).

Mandatory reading completed first, in full: `docs/PRODUCTION_GAPS_R2.md`,
`docs/reports/R2-1-REPORT.md` through `R2-5-REPORT.md`, and
`internal/engine/runner.go` as it existed before any change.

---

## 1. Sniper candle feed — DONE

**Files:** `internal/engine/runner.go`.

Applied R2-2's exact ~4-line prescription (R2-2-REPORT.md §3):
- `Runner` gained a `candleAgg *strategy.CandleAggregator` field.
- `NewRunner` constructs it: `strategy.NewCandleAggregator(5*time.Minute, cfg.HistorySize)`.
- `evaluateSymbol` now calls `r.candleAgg.Add(symbol, price, volume, nowIST)`
  at both existing `r.priceHistory.Add` call sites (has-position and
  no-position branches).
- `ctx.Candles = r.candleAgg.Completed(symbol)` is set right before
  `r.strat.Evaluate(ctx)`, additively — `ctx.Prices`/`ctx.Volumes` population
  is unchanged, so `nine_twenty`/`short_straddle`'s premium-based-stop
  contract (which reads `Prices` via `LastPrice()`) is untouched.

**Test evidence:** new `internal/engine/runner_candlefeed_test.go`,
`TestRunner_EvaluateSymbol_FeedsNonEmptyCandles` — registers a
context-capturing test strategy, drives the REAL `evaluateSymbol` call path
(not a synthetic harness) across 8 timestamps spanning distinct 5-minute
wall-clock buckets, and asserts `ctx.Candles` is non-empty (`ctx.Prices` also
stays populated). Output:
```
ctx.Candles has 7 completed candle(s) after 8 ticks; ctx.Prices has 8 point(s)
--- PASS: TestRunner_EvaluateSymbol_FeedsNonEmptyCandles (0.22s)
```
This is the direct regression proof that sniper's "zero trades ever" root
cause (G-2 — `ctx.Candles` never populated by the live runner) is closed at
the runner layer, on top of R2-2's own fix to sniper's mode-priority gate.

---

## 2. Margin-aware SELL entries — DONE

**Files:** `internal/engine/runner.go`.

- `requiredMargin` (previously a permanent stub returning "not implemented")
  now calls the broker's real margin API via a new `basketMargin` helper:
  type-asserts `r.te.broker.(broker.ExtendedTradeService)` and calls
  `GetRequiredMargin`. If the broker doesn't implement the interface
  (`MockBroker`/`LivePaperBroker` don't, per R2-1/R2-4's reports) or the call
  errors, the entry is rejected — fail-closed, no premium-based fallback.
- `placeSingleLeg` passes the single leg's own qty into `requiredMargin`.
- `enterMultiLeg` was restructured per R2-1's hedged-margin design: it now
  resolves every leg's target symbol/side/qty **first**, builds the whole
  basket (`[]broker.MarginOrderInput`), and prices it in **one**
  `GetRequiredMargin` call before placing any leg — a basket-level margin
  rejection now happens with zero legs placed (nothing to unwind), instead
  of the old per-leg-during-the-loop margin check.
  - The combined margin figure is split evenly across the basket's SELL legs
    (`risk.Manager` locks margin per-symbol, not per-basket, and Angel's
    per-leg margin breakdown isn't parsed per R2-1's report §6) — documented
    as an approximation with a `ponytail:` comment; it never under-locks the
    true combined total, only potentially over-locks if leg sizes are very
    uneven.

**Test evidence:** new `internal/engine/runner_margin_test.go`, 5 tests, all
against a `fakeMarginBroker` (wraps `MockBroker`, adds a controllable
`GetRequiredMargin`) since `MockBroker` itself deliberately doesn't implement
`ExtendedTradeService`:
```
--- PASS: TestPlaceSingleLeg_SellDerivative_FailsClosedWithoutExtendedTradeService
--- PASS: TestPlaceSingleLeg_SellDerivative_UsesRealMargin
--- PASS: TestPlaceSingleLeg_SellDerivative_MarginErrorFailsClosed
--- PASS: TestEnterMultiLeg_PricesWholeBasketInOneCall
--- PASS: TestEnterMultiLeg_BasketMarginError_RejectsBeforePlacingAnyLeg
```
`TestEnterMultiLeg_PricesWholeBasketInOneCall` specifically asserts exactly
one `GetRequiredMargin` call carrying **both** legs of a short-straddle-shaped
signal (not two separate calls) — the direct proof of the hedged-margin
design. `TestPlaceSingleLeg_SellDerivative_FailsClosedWithoutExtendedTradeService`
proves a plain `MockBroker` (today's actual paper-mode broker) still rejects
SELL derivative entries fail-closed exactly as before — this wiring does not
change paper-mode's own safety posture, it only makes the path do something
real once a broker that implements `GetRequiredMargin` (`AngelBroker`, R2-1)
is in play.

---

## 3. WebSocket feed subscription — DONE

**Files:** `internal/app/titan.go`, `cmd/main.go`.

Both entry points now call, right after the existing `Subscribe(symbols)`
REST-polling subscription:
```go
if ext, ok := tradeService.(broker.ExtendedTradeService); ok {
    if err := ext.SubscribeLive(symbols); err != nil {
        log.Printf("⚠️ WS live feed subscribe failed for %v: %v (falling back to REST polling)", symbols, err)
    } else {
        log.Printf("📡 WS live feed subscribed for %d symbol(s)", len(symbols))
    }
}
```
Best-effort, matching R2-1's design: `MockBroker`/`LivePaperBroker` don't
implement `ExtendedTradeService` so paper mode is unaffected (the `ok`
branch simply doesn't run); a real `AngelBroker` whose feed never connects
still serves prices via REST exactly as before.

**No runner-side code change was needed** for the software SL loop to
benefit — confirmed by reading `priceWithAge`/`softStopLossCheck` again:
both already consult `GetCurrentPriceWithAge` uniformly regardless of what
last wrote the cache (WS tick or REST poll), exactly as R2-1's report states.

**Test evidence:** new `internal/engine/runner_wsfeed_test.go`,
`TestPriceWithAge_SameLoopLogicRegardlessOfSource` — three sub-tests prove
(a) a fresh/low-age price is treated as usable, (b) a stale/high-age price is
correctly flagged stale (so `softStopLossCheck` skips it), and (c) a broker
with no `ExtendedTradeService` (no staleness concept) is always "fresh" —
all through the exact same `priceWithAge` code path, with only the
underlying age value differing, proving the loop genuinely doesn't
source-branch.
```
--- PASS: TestPriceWithAge_SameLoopLogicRegardlessOfSource (0.38s)
    --- PASS: .../fresh_tick_(as_if_just_delivered_over_WS) (0.15s)
    --- PASS: .../stale_price_(as_if_REST_polling_fell_behind) (0.10s)
    --- PASS: .../plain_MockBroker_(no_ExtendedTradeService,...) (0.11s)
```
A real end-to-end "price age drops after `SubscribeLive` against a live WS
feed" comparison was **not** performed — that requires a real Angel
connection, which is BLOCKED this round (see §10). R2-1's own
`ws_feed_test.go` (`TestSubscribeLive_TickUpdatesPriceAge`) already proves
the underlying mechanism against a fake WS server; this task only needed to
confirm the runner-level wiring and the source-agnostic staleness logic,
both done above.

---

## 4. Paper-fill test fix — DONE

**File:** `internal/engine/runner_smoke_test.go`.

Applied R2-4's exact documented one-line fix:
```go
mb := broker.NewMockBrokerWithConfig(10000, broker.LegacyPaperFillConfig())
```
(previously `broker.NewMockBroker(10000)`, which now defaults to R2-4's
realistic-but-nondeterministic fill model).

**Test evidence:** ran `go test -race -count=1 ./internal/engine/ -run
TestRunnerSmoke` **10 times in a row** in a loop — all 10 passed:
```
ok  	titan-algo/internal/engine	7.530s
ok  	titan-algo/internal/engine	5.950s
ok  	titan-algo/internal/engine	5.156s
ok  	titan-algo/internal/engine	5.287s
ok  	titan-algo/internal/engine	5.410s
ok  	titan-algo/internal/engine	5.159s
ok  	titan-algo/internal/engine	5.184s
ok  	titan-algo/internal/engine	5.222s
ok  	titan-algo/internal/engine	5.060s
ok  	titan-algo/internal/engine	6.599s
```
(Before the fix, R2-4's own report documented 3/5 failures on repeated runs
with the default config — this is the confirmed resolution.)

---

## 5. Rate-limit config wiring — DONE

**Files:** `internal/config/config.go`, `cmd/main.go`, `internal/app/titan.go`.

Added to `APIConfig` exactly the fields R2-5's report specified:
```go
RateLimitRPS   float64 `yaml:"rate_limit_rps"`
RateLimitBurst int     `yaml:"rate_limit_burst"`
WSMaxConns     int     `yaml:"ws_max_conns"`
```
No new defaults needed in `config.go` — `api.Server.SetRateLimit`/
`SetWSMaxConns` already treat `<= 0` as "keep the built-in default"
(`DefaultRateLimitRPS`/`DefaultRateLimitBurst`/`DefaultWSMaxConns`), so a
config.yaml that doesn't set these fields (today's, unedited) behaves
identically to before this change.

Wired at both call sites that construct `api.Server`, immediately after
`api.NewServer(...)`:
```go
apiServer.SetRateLimit(cfg.API.RateLimitRPS, cfg.API.RateLimitBurst)
apiServer.SetWSMaxConns(cfg.API.WSMaxConns)
```
Also wired `SetTLS` (previously a dead knob per known-bug B-5 / G-13(e)) at
the same two call sites:
```go
apiServer.SetTLS(cfg.API.TLSCertFile, cfg.API.TLSKeyFile)
```
Decision made and documented in-code: wire it (not delete it) — the fields
already existed in `APIConfig`, `SetTLS` already existed on `Server`, and
empty/empty is a documented no-op, so wiring it closes B-5 at zero behavioral
risk to the default (unedited, localhost, plaintext) deployment.

**Test evidence:** whole-module `go build`/`go vet`/`go test -race`
(§9) green, including `internal/api`'s existing `TestRateLimitReturns429WithRetryAfter`
etc. (R2-5's tests, unaffected — this task only adds the config→setter
plumbing, `internal/api/server.go`'s own logic wasn't touched except the
`Token()` getter in task 8).

---

## 6. Holiday file wiring — DONE

**Files:** `internal/config/config.go`, `internal/engine/runner.go`.

- `Config` gained `HolidayFile string \`yaml:"holiday_file"\`` with a
  code-side default of `"nse_holidays.yaml"` (set in `parse()`'s defaults
  section) — `config.yaml` itself needs no edit (and wasn't edited) for this
  default to take effect.
- `RunnerConfig` gained a matching `HolidayFile string` field; both
  `cmd/main.go` and `internal/app/titan.go` now pass `cfg.HolidayFile`
  through when constructing `RunnerConfig`.
- `runner.go` gained `loadHolidays(path string) map[string]bool`, called once
  in `NewRunner`. It parses the real shape R2-5 shipped in
  `go-engine/nse_holidays.yaml` (a top-level `holidays:` list of
  `{date, description}`). Per R2-5's explicit recommendation, it **fails
  open**: a missing file, malformed YAML, or an empty holiday list all fall
  back to the existing hardcoded `nseHolidays2026` table with a loud log
  line — never blocks startup. On success, the loaded set **replaces** (not
  merges with) the hardcoded table, exactly as R2-5 recommended ("simplest —
  the YAML file becomes the one source of truth going forward").
- `marketState()` now reads `r.holidays` (a `Runner` field) instead of the
  package-level `nseHolidays2026` map directly.

**Test evidence:** new `internal/engine/runner_holidays_test.go`, 3 tests:
```
--- PASS: TestLoadHolidays_ValidFile_ReplacesHardcodedTable
--- PASS: TestLoadHolidays_MissingOrMalformedFile_FailsOpenToHardcodedTable
    --- PASS: .../missing_file
    --- PASS: .../malformed_YAML
    --- PASS: .../empty_path_uses_hardcoded_table
--- PASS: TestLoadHolidays_RealShippedFile
```
`TestLoadHolidays_ValidFile_ReplacesHardcodedTable` uses a date that exists
ONLY in a synthetic file (not in the hardcoded table) and confirms
`marketState()` correctly reports `marketClosedHoliday` for it — proving the
file is genuinely authoritative, not just parsed-and-ignored.
`TestLoadHolidays_RealShippedFile` loads the actual
`go-engine/nse_holidays.yaml` R2-5 committed and confirms all 5 of its dates
parse correctly.

---

## 7. IV series consumption — NOT APPLIED (follow-up, not guessed)

**Gap:** G-4 (`internal/backtest/engine.go`'s `priceLeg`, `Vol: cfg.IV`).

R2-3's report (§2) states the "exact one-line change" is replacing
`Vol: cfg.IV` with a per-bar lookup via *a new method*,
e.g. `Config.IVAt(asOf time.Time) float64`, backed by a map built once from
`BuildIVSeries` — but that method does not exist yet, and R2-3's report is
explicit that wiring it requires **adding** that method plus a way to get a
populated IV series into `Config` in the first place (which candle/expiry/
strike/option-type combination, sourced from where). None of that is
specified precisely enough to apply without inventing the missing pieces
myself — which the task instructions explicitly said not to do ("apply...
IF R2-3's report makes it unambiguous; otherwise note it as a follow-up
rather than guessing").

Concretely, doing this properly would require: (1) a new `Config.IVSeries
map[time.Time]float64` (or equivalent) field, (2) an `IVAt` method with a
fallback-to-constant-IV policy for bars with no series entry, and (3) CLI
wiring in `cmd/backtest/main.go` (R2-3's owned file, not touched this round)
to actually populate that field from a fetched option-candle file — none of
which existed as a call site to hook into. Guessing the CLI-side wiring
risks silently diverging from whatever R2-3's own `-option-csv`/`-option-strike`
et al. flags were designed to produce.

**Left as an explicit follow-up for Round 3**, not silently dropped:
`internal/backtest/iv.go`'s `ImpliedVol`/`BuildIVSeries` are fully built and
tested (R2-3's work); `cmd/backtest` still prints R2-3's loud
`ConstantIVBanner` on every run, so nobody can mistake a backtest result for
IV-aware. No behavior changed, no code guessed.

---

## 8. Mobile token surfacing — PARTIALLY DONE (server-side wired; Android-side parked, as anticipated)

**Files:** `internal/api/server.go` (new `Token()` getter — the one
wiring-only edit outside my owned-file set), `internal/app/titan.go`.

R2-5's finding: `api.NewServer` prints a freshly-generated random token via
`fmt.Println` only; the mobile build (`mobile/titanmobile.go`) redirects the
`log` package's output to `titan_mobile.log` but never touches stdout, so
the token never reaches anywhere the mobile operator can read it.

Applied the small, well-specified half of the fix that's actually mine to
make (task 8's own framing: "wire the server side and note what's left"):
- Added `Server.Token() string` (`internal/api/server.go`) — a plain getter,
  no behavior change, needed because nothing outside the `api` package could
  previously read the actual (possibly auto-generated) token back out.
- `internal/app/titan.go`'s `Initialize()` now logs it through the `log`
  package (not `fmt.Println`) right after constructing `ApiServer`:
  ```go
  log.Printf("🔑 API auth token (needed for mobile Settings / REST X-API-Key / WS ?token=): %s", app.ApiServer.Token())
  ```
  Since `mobile/titanmobile.go`'s `Start()` calls `log.SetOutput(logFile)`
  **before** calling `Initialize()`, this line now lands in
  `titan_mobile.log` on Android — the exact channel R2-5's report identified
  as the one Android build actually captures. The desktop path
  (`cmd/main.go`) is unaffected (it never called `titan.go`'s `Initialize`;
  it already saw the token via `NewServer`'s own stdout banner).

**Not done (parked, per R2-5's own COMPAT.md and the task's explicit
allowance for an Android-side follow-up):** persisting the token across
launches and auto-filling it into the WebView (`localStorage`) still needs a
`MainActivity.kt` (Kotlin, Android-side) change — `Mobile.start(dataDir,
apiKey)`'s `apiKey` argument also still maps to the **broker** API key
(`cfg.Brokers.Angel.APIKey`), not the API-server token; that mismatch is
`titanmobile.go`'s own pre-existing wiring (a Go file R2-5 read but marked
off-limits, and which I read but is not in my owned-file set for a redesign
this round — only titan.go's log line was the well-specified, small piece).
Full detail already lives in `mobile-app/COMPAT.md` (R2-5).

**Verification:** confirmed by code reading (mobile/titanmobile.go's
`log.SetOutput` ordering) — no Android emulator/device is available in this
environment to observe `titan_mobile.log` directly; this is a logging change
with no runtime branching, so `go build`/`go vet`/`go test` passing is the
available proof of correctness for the Go-side half.

---

## 9. General integration verification — DONE

```
$ cd go-engine && go build ./... && go vet ./...
(clean, no output)

$ go test -race -count=1 ./...
ok  	titan-algo/cmd/fetchdata	9.490s
ok  	titan-algo/cmd/watchdog	3.041s
ok  	titan-algo/internal/api	8.492s
ok  	titan-algo/internal/backtest	2.970s
ok  	titan-algo/internal/broker	27.259s
ok  	titan-algo/internal/config	4.059s
ok  	titan-algo/internal/engine	12.072s
ok  	titan-algo/internal/ledger	7.820s
ok  	titan-algo/internal/logger	2.854s
ok  	titan-algo/internal/risk	3.001s
ok  	titan-algo/internal/state	9.558s
ok  	titan-algo/internal/strategy	1.912s
(cmd, cmd/backtest, examples, internal/app, internal/cli, internal/discovery,
mobile, models: no test files, as before)
```

This is the first point all of Round 1 (11 packages) + all 5 Round 2 Wave A
packages + this integration work compile and test together, green, under
`-race`, across the entire module.

`internal/engine`'s own suite grew from 1 test file (`runner_smoke_test.go`)
to 5, all passing:
```
--- PASS: TestRunner_EvaluateSymbol_FeedsNonEmptyCandles (0.21s)
--- PASS: TestLoadHolidays_ValidFile_ReplacesHardcodedTable (0.01s)
--- PASS: TestLoadHolidays_MissingOrMalformedFile_FailsOpenToHardcodedTable (0.01s)
--- PASS: TestLoadHolidays_RealShippedFile (0.00s)
--- PASS: TestPlaceSingleLeg_SellDerivative_FailsClosedWithoutExtendedTradeService (0.18s)
--- PASS: TestPlaceSingleLeg_SellDerivative_UsesRealMargin (0.22s)
--- PASS: TestPlaceSingleLeg_SellDerivative_MarginErrorFailsClosed (0.19s)
--- PASS: TestEnterMultiLeg_PricesWholeBasketInOneCall (0.24s)
--- PASS: TestEnterMultiLeg_BasketMarginError_RejectsBeforePlacingAnyLeg (0.20s)
--- PASS: TestRunnerSmoke (0.32s)
--- PASS: TestPriceWithAge_SameLoopLogicRegardlessOfSource (0.38s)
```

`docs/RUNBOOK.md` was **not** further edited — R2-5 already appended
sections 7 (watchdog/Task Scheduler), 8 (rate limiting), 9 (holiday
maintenance) in its own pass; nothing in this round's wiring needed a new
RUNBOOK section per the task's own gating ("only if a report explicitly
asked for a RUNBOOK append you haven't yet applied").

**File-ownership check:** `git status` after all changes shows modifications
only inside this package's owned set
(`cmd/main.go`, `internal/engine/*`, `internal/app/titan.go`,
`internal/config/config.go`) plus the one justified exception
(`internal/api/server.go`'s `Token()` getter, task 8) — no Wave A file was
altered by this work. `config.yaml` does not appear in the modified list.

**Credential/network safety check:**
```
$ git diff -- go-engine/internal/engine/runner.go go-engine/internal/app/titan.go \
             go-engine/cmd/main.go go-engine/internal/config/config.go \
             go-engine/internal/api/server.go \
  | grep -iE "client_code|totp|password|api_key|A647965|http://|https://"
(no output)
```
No credentials, credential-like strings, or new outbound network calls were
introduced by this round's wiring.

---

## 10. Real-endpoint verification — BLOCKED (expected)

BLOCKED — `config.yaml` still holds the original unrotated credentials
(client code `A647965`, TOTP secret present — reverified at the start of
this session) and the project remains on OneDrive. Per the standing safety
constraint, no real-network Angel One connection was attempted. All wiring
above (margin, WS feed, candle feed, holidays, rate limiting, TLS, mobile
token) was verified via `MockBroker`/a hand-built `fakeMarginBroker`/direct
unit calls only — never against `AngelBroker`'s real HTTP/WS endpoints.

---

## Final status — what Round 2 leaves for Round 3

**Engineering (this round's scope) is done and green:** all Wave A work
(R2-1..R2-5) is now wired into the live trading loop; whole-module
`build`/`vet`/`test -race` passes; every wiring task above has a specific,
runnable test (not just "it compiles") except item 7, which was correctly
left unguessed, and item 10, which is correctly blocked rather than faked.

**What is NOT done, and why, honestly:**

1. **IV-driven backtest pricing (G-4)** is still not load-bearing — the
   inverter/series-builder exist and are tested (R2-3), but nothing feeds a
   per-bar IV series into `priceLeg`. Every backtest number remains
   constant-IV (the loud banner says so on every run). This needs a small
   but real design decision (what `Config.IVAt` looks like, how
   `cmd/backtest` populates it) that belongs with whoever next owns
   `internal/backtest/engine.go` + `cmd/backtest/main.go` together, not
   guessed piecemeal across two rounds.
2. **Real historical/option data (G-3)** still doesn't exist in-repo — this
   was never in scope for any Wave A/B package; it needs H-1 (credential
   rotation) and H-4 (supervised `fetchdata` run) first, then R2-7's
   walk-forward rerun on real data. Nothing in this round changes that
   timeline.
3. **Mobile app (G-14)** has its WS-token bug fixed (R2-5) and now has a
   server-side channel for the operator to actually read the auth token
   (this round), but the deeper fix — persisting the token and injecting it
   into the WebView automatically, and un-conflating `apiKey`/broker-key in
   `titanmobile.go`'s `Start` signature — still needs a `MainActivity.kt`
   (Kotlin/Android) change plus a `titanmobile.go` redesign, both explicitly
   parked in `mobile-app/COMPAT.md`.
4. **The margin-splitting approximation** (task 2, §2 above): dividing a
   basket's combined margin evenly across its SELL legs is a documented
   `ponytail:`-flagged simplification, not exact per-leg accounting. It never
   under-locks the true combined total, but if `risk.Manager` is ever
   redesigned to support one shared basket-level lock instead of per-symbol
   locks (an R2-6-shaped cleanup), this approximation should be revisited.
5. **The one open item from the plan doc that nobody has touched yet and
   this round correctly left alone:** real-endpoint E2E verification (G-11)
   — genuinely blocked on H-1 (credential rotation) and H-2 (move off
   OneDrive), both human tasks, both still outstanding as of this report.

**Bottom line:** the system is now structurally wired end-to-end in
paper/mock mode — margin-aware sizing, a live-feed hook, real candle data
for sniper, config-driven ops knobs, and a fixed flaky test all verified with
targeted tests. It is still not evidence-based production-ready, because (per
`PRODUCTION_GAPS_R2.md` §6) that requires real data and a strategy that
clears the go-live gates, which remains Round 3's job, gated on the human
tasks (H-1/H-2/H-4) that have been outstanding since before Round 2 started.
