# Live Trading Gate Checklist

**Do not run any strategy with `-live` until every box below is checked, in order, by a
human operator.** This checklist assumes all of WP-1 through WP-9 are merged and
`go build ./... && go vet ./... && go test -race ./...` are clean at HEAD. It does not
replace the go/no-go verdicts in `docs/validation/RESULTS.md` (which found **zero
strategies with a demonstrated edge**, on synthetic data, as of this WP) — that is a
separate, prior gate. Passing this checklist makes live trading *operationally safe*; it
does not make a strategy *profitable*. Both gates must clear before real money moves.

## 0. Prerequisite — strategy validation gate

- [ ] The strategy being deployed has run through the `docs/validation/RESULTS.md`
      protocol (or its successor once real data is available) and either (a) cleared all
      three gates (PF > 1.3 OOS, max DD < 15% of deployed margin, positive expectancy at
      2x modeled costs) on **real historical data**, or (b) the operator has explicitly
      decided to proceed anyway and documented why (e.g. "small discretionary allocation,
      accepted as an experiment") — silent deployment of a strategy that failed
      validation is not acceptable.
- [ ] If (b), the position size in section 3 below is reduced further than the 1-lot
      floor would otherwise imply.

## 1. Account & broker-side controls

- [ ] Angel One credentials rotated post-remediation (TOTP re-enrolled, new API key, new
      PIN) — Phase 0 of `docs/REMEDIATION_PLAN.md`, confirmed not still using the burned
      values from `config.yaml:6-11` referenced in the audit.
- [ ] Credentials are environment variables only (`ANGEL_CLIENT_CODE`, `ANGEL_PIN`,
      `ANGEL_API_KEY`, `ANGEL_API_SECRET`, `ANGEL_TOTP_SECRET`), never committed to
      `config.yaml` or any file tracked by git.
- [ ] **Broker-side daily loss cap configured** (not just an internal risk-engine check —
      an actual RMS/broker-enforced limit that survives a process crash). Confirm via
      Angel One's RMS settings, not just `internal/risk`'s in-process `CheckRisk`.
- [ ] **Minimum lot size: 1 lot, no exceptions**, for the first live deployment of any
      strategy or any code change to the live path. Confirm `-lotsize` / the equivalent
      live-mode config is set to the exchange minimum (75 for NIFTY post-Apr-2025 — verify
      current value, lot sizes change; see `internal/broker.GetLotSize` if wired, or the
      instrument master directly).
- [ ] Broker-side stop-loss orders (`PlaceStopLossOrder`, WP-1) are placed with every
      entry, not relying solely on the in-process software stop-loss checker.

## 2. System health / operational safety

- [ ] **Watchdog / dead-man's-switch verified working**: kill the process (or block its
      network) mid-session in a paper run and confirm the watchdog fires an alert
      (Telegram/SMS per the remediation plan) within the expected window, and that open
      positions get flattened or the operator is paged in time to act manually.
- [ ] **Kill-switch tested**: manually trigger `TriggerKillSwitch()` (or the equivalent
      operator control) during a paper session and confirm (a) no new orders are placed
      after the trigger, (b) existing positions are flattened or clearly surfaced as
      needing manual action, (c) the kill-switch state survives a process restart (i.e.
      it doesn't silently reset to "safe" on reboot).
- [ ] Crash-and-restart drill performed with an open paper position: kill the process,
      restart it, confirm `internal/state`'s reconciliation (`RecoverSession`/`Reconcile`)
      correctly recovers the open position and risk snapshot with no orphan or phantom
      position.
- [ ] Session-expiry handling verified: force an auth failure mid-session (e.g. revoke
      the token) in paper mode and confirm the broker health check + refresh/re-login
      flow behaves as documented, not silently trading against stale/failed auth.
- [ ] Market-hours / expiry-day edge cases exercised at least once in paper mode: a
      normal squareoff time, and one actual weekly-expiry Tuesday if the strategy trades
      options.

## 3. Paper trading track record (the actual gate, not a formality)

- [ ] **≥1 calendar month of continuous paper trading completed on the CURRENT
      post-remediation code** (i.e. after WP-1 through WP-9 landed — a paper run on
      pre-remediation code does not count, the whole point of this remediation was that
      the old code's P&L math and signal logic were wrong).
- [ ] Paper trading ran through the **same code path** live trading will use (per CR-11 —
      confirm whatever unification WP-9 actually delivered; if backtest/live still don't
      fully share an execution engine, note that explicitly as a residual gap rather than
      assuming otherwise).
- [ ] Every paper trade reconciled daily against `internal/ledger`'s append-only trade
      ledger: fills, charges, and running P&L in the ledger match what the paper broker
      simulation reported, with discrepancies investigated and explained (not just
      totals eyeballed at month-end).
- [ ] Paper P&L for the month is **net positive after modeled charges**, or if not, the
      operator has reviewed *why* (bad luck in a real down-month vs. a real problem) and
      made an explicit decision to proceed or wait another month.
- [ ] At least one paper-trading day included a real adverse move (a genuine down day or
      spike, not just calm conditions) with a documented outcome — don't let the whole
      paper month be an unusually quiet period that never tested the stop-loss/risk paths.

## 4. Position sizing / scale-up rule

- [ ] First live deployment: **1 lot, one strategy at a time.** Do not deploy multiple
      strategies simultaneously on first go-live.
- [ ] **Scale-up rule, written down and followed mechanically, not by feel**: increase
      position size only after **N consecutive profitable weeks** (operator sets N based
      on the strategy's own backtest trade frequency — e.g. N=4 for a strategy trading
      several times a day, N=8+ for a low-frequency one, so N weeks represents a
      statistically meaningful sample of trades, not just 1-2 lucky trades) **AND**
      live-vs-paper tracking error stays within a stated tolerance for that entire period
      (suggested starting tolerance: live P&L within ±20% of what the same signals would
      have produced in paper/backtest terms for the same period, per trade — tighten this
      once real data on live/paper divergence exists).
- [ ] A single week where live-vs-paper tracking error breaches the tolerance **resets the
      consecutive-week counter to zero** and pauses further scale-up (does not
      necessarily require flattening existing size) until the operator has investigated
      why paper and live diverged.
- [ ] Scale-up increments are small and pre-committed (e.g. 1 lot → 2 lots → 3 lots), not
      an unbounded "double it" jump.

## 5. Credential & secret hygiene (ongoing, not one-time)

- [ ] Credential rotation confirmed on a defined cadence (not just once at Phase 0) —
      write down the actual cadence the operator commits to (e.g. quarterly TOTP
      re-enrollment, API key rotation on any suspected exposure).
- [ ] No secrets appear in logs, CSV exports, or the ledger (grep the actual log output
      from a paper session for the API key / TOTP secret / PIN before going live, don't
      just trust that the code path removed the logging — verify).
- [ ] `TITAN_API_TOKEN` (or equivalent API-server auth token, WP-4) is a freshly generated
      random value, not a default or previously-committed one, before the API server is
      exposed to anything beyond localhost.

## 6. Sign-off

- [ ] All boxes above checked by a human, dated, and kept somewhere durable (this file's
      checked state in version control is fine — commit it as evidence, don't just tick
      boxes in a chat window and forget).
- [ ] The specific strategy name, parameter set, and lot size being deployed live are
      written down here at sign-off time, so "what did we actually approve" is never a
      question later:

  ```
  Strategy:        ______________________
  Parameters:      ______________________ (must match what was paper-traded, not a
                                            fresh untested variant)
  Lot size:        ______________________
  Daily loss cap:  ______________________
  Sign-off date:   ______________________
  Signed off by:   ______________________
  ```
