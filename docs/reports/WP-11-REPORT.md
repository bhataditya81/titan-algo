# WP-11 — Documentation Truth Pass — Report

Scope: `README.md`, `RUNNING.md`, `PAPER_TRADING.md`, `docs/MOBILE_APP_DESIGN.md`,
new `docs/RUNBOOK.md`. No code files touched. Every claim below is traced to a
specific `docs/reports/WP-*-REPORT.md`.

## Files changed

| File | Change |
|---|---|
| `README.md` | Full rewrite. Describes only what WP-1–WP-9 actually built; explicit "What it is not" section; per-subsystem capability list with WP-report citations; honest Roadmap section. |
| `RUNNING.md` | Full rewrite. Env-var credential table, config template usage, build-then-run, startup/recover/reconcile sequence, `-accept-reconcile` flag, stop procedure. |
| `PAPER_TRADING.md` | Full rewrite. Corrects the old "4 hardcoded demo trades" description (deleted by WP-9) to the real strategy-driven `Runner` loop; documents that paper mode shares state/ledger/reconciliation with live mode. |
| `docs/MOBILE_APP_DESIGN.md` | Targeted edits: security checklist, production checklist, and conclusion rewritten against WP-4's actual server implementation. Architecture/API-shape sections left as design intent (no mobile frontend has been built). |
| `docs/RUNBOOK.md` | New. Six operational procedures per the task list. |

## False/aspirational claims corrected (before → after)

### README.md
1. "High-Frequency Trading (HFT) system... Ultra-low latency" → "polls REST on a 2-second default interval (`engine.poll_interval_ms`), no market-data WebSocket feed."
2. "TimescaleDB... PostgreSQL... Redis (hot state, job queues via Asynq)" → "No TimescaleDB. No Redis/Asynq wired into the Go engine." (WP-8's compose file keeps Postgres/Redis containers present for future use, but nothing in `internal/*` reads/writes them.)
3. "Apache Arrow Flight: Zero-copy shared memory transfer" → "`internal/ipc/ipc.go` is a one-line no-op stub, left untouched per WP-9's explicit scope decision."
4. "gRPC: Inter-service communication" / py-brain server.py as a working service → "py-brain's gRPC server registers zero real services (proto imports commented out); Roadmap item."
5. "RAPIDS.ai cuDF: GPU-accelerated DataFrames... PyTorch: LSTM/Transformer models" → "WP-8 replaced the hard `cudf` import with a pandas/numpy fallback (SMA/EMA/RSI) — a basic CPU fallback, not GPU analytics; no trained model exists."
6. "Broker APIs: Zerodha Kite Connect, Angel One SmartAPI" → "Angel One only; no Zerodha code exists."
7. "Database Schema... Trade Model (GORM)" → "`models/trade.go` was rewritten (WP-3) to plain structs; no GORM, no ORM."
8. Roadmap items ("Implement Zerodha/AngelOne connectors", "Add technical indicators") that were already stale/done or abandoned → replaced with the actual current roadmap: py-brain GPU/gRPC (kept, non-functional), broker margin-API integration (WP-9 confirmed gap, fail-closed), standalone watchdog binary (not built), auto-updating NSE holiday calendar (fixed table).
9. Backtest section (didn't exist in old README) → added, including the explicit v1 constant-IV limitation documented in WP-7's report.

### RUNNING.md
1. "Edit `go-engine/config.yaml` and add your API keys" (plaintext credentials in YAML) → env-var-first credential resolution (`ANGEL_CLIENT_CODE` etc.), `config.yaml` credential fields must stay empty, live-mode hard-fails on YAML-sourced credentials (WP-8's `ValidateLiveCredentials`).
2. `go run cmd/main.go` as the documented launch method → `go build -o titan.exe ./cmd` then run the binary (WP-8 fixed the scripts for this reason).
3. No mention of state/reconciliation → full startup sequence documented (config → broker connect → state/ledger open → recover → reconcile → accept-reconcile gate → tick loop), per WP-9's report.
4. `stop.ps1`'s old force-kill-by-window-title behavior → documented as `/api/stop` then `/api/kill` via HTTP, PID-file fallback only as a degraded path (WP-8).

### PAPER_TRADING.md
1. "Execute 4 sample trades (2 buys, 2 sells)... Paper Trading Summary" (the old hardcoded demo loop) → this loop was deleted during WP-9 integration; paper mode now runs the real `Runner` tick loop against the real strategy set.
2. "MockBroker: Connected successfully... Initializes Mock Broker (₹10L)" style console-output framing implying a disposable demo → paper mode documented as using the same `state`/`ledger` persistence and reconciliation flow as live mode (WP-3, WP-5, WP-9).
3. CSV column count (was documented as 8 columns: `Timestamp,Symbol,Action,Quantity,FillPrice,Slippage,TransactionFee,VirtualBalance`) → corrected to the actual current 12-column format with `OrderID`/`Status`, and the header-driven-parsing caveat for old files (WP-5).
4. "Docker Deployment... timescaledb: Database (port 5432)" implying an active dependency → corrected to "present in compose for future use, nothing in the Go engine uses it," matching README's roadmap framing and WP-8's actual compose changes (no published ports, required passwords, healthchecks).

### docs/MOBILE_APP_DESIGN.md
1. "✅ API key header authentication (`X-API-Key`)" (checked off with nothing built) → "✅ Token-based auth... independent of any broker credential... crypto/rand-generated" — now backed by WP-4's report and grep evidence (no `titan-mobile-secret`, no wildcard CORS).
2. "✅ CORS restricted to mobile app origin" (nothing implemented at the time) → "✅ CORS allowlist... default (empty) sends no CORS headers at all" (WP-4).
3. "✅ Rate limiting (10 req/sec)" (never implemented) → "❌ Rate limiting is NOT implemented. No per-IP or per-token request-rate limiter exists anywhere in `internal/api/server.go`." Explicitly flagged as an open item now, not silently dropped.
4. "✅ Heartbeat every 5 seconds" → downgraded to "⚠️ Heartbeat exists at the engine level (per-tick, WP-9)... 'every 5 seconds' as a mobile-specific cadence is not verified against any report."
5. "✅ Connection status indicator / auto-reconnect / 'last updated' timestamps" (mobile client features, no mobile client exists) → "❌ ... No mobile app code has been built as part of WP-1–WP-9."
6. Production Checklist: all 10 items were `[x]` → corrected to 3 `[x]` (token auth, localhost-default bind, WS transport — all backend-only, WP-4) and 7 `[ ]` (rate limiting not implemented; the remaining 6 are mobile-client features and no mobile client has been built).
7. "**Ready for implementation.** ✅" conclusion → rewritten to state the backend is real and tested (WP-4/WP-9) but rate limiting and the mobile client itself remain the two biggest gaps.
8. Real control surface (new subsection, not previously documented at all): `/api/start`/`/api/stop`/`/api/kill` verified wired to real `ControlHooks` against the actual engine (WP-4 + WP-9), replacing the old cosmetic stop button (audit finding CR-4).

## docs/RUNBOOK.md — table of contents

1. Broker session/auth failure mid-day — detection (401/`AG`/`AB` codes), automatic token-refresh → TOTP re-login → unhealthy-mark sequence (WP-1), what to check, and the broker-side-SL-as-safety-net note (software SL refuses to fire on stale >15s prices, per WP-9).
2. Engine crash with open positions — `RecoverSession`/`Reconcile` behavior (WP-3, WP-9), example mismatch report format, when `-accept-reconcile` is and isn't appropriate.
3. NSE expiry day operations — instrument-master-based expiry resolution and its fallback tiers (WP-1/WP-9), the documented-incomplete `nseHolidays2026` table, no expiry-specific risk layer beyond the existing stops.
4. Using the kill switch — both `data/KILL` sentinel file and `/api/kill` (WP-9/WP-4), including `stop.ps1`'s HTTP-then-PID-fallback behavior (WP-8).
5. Credential rotation — env vars as source of truth (WP-8), `ValidateLiveCredentials` hard gate, `TITAN_API_TOKEN` rotation independent of broker credentials (WP-4).
6. Watchdog/heartbeat gone stale — explicit statement that no standalone watchdog binary exists (WP-9's own documented follow-up), manual procedure using `data/heartbeat`'s mtime and broker-side stops as the interim safety net, and the specific gap that a crashed process cannot alert on its own crash.

## Verification performed

- Grepped all five edited/new files for `timescale|redis|asynq|arrow flight|zerodha|ultra-low|hft` (case-insensitive): every remaining match is an explicit negation/correction ("No TimescaleDB", "Not Zerodha-compatible", etc.), never an affirmative current-capability claim.
- Grepped for `✅`/`[x]` across all five files: every remaining instance is backed by an inline citation to a specific WP report (WP-1, WP-3, WP-4, WP-8, or WP-9) describing the exact mechanism verified.
- Cross-checked strategy names, env var names, config field names (`engine.poll_interval_ms: 2000`, `api.bind_addr`, `state.db_path`, `ledger.db_path`), and CLI flags (`-paper`, `-live`, `-search`, `-accept-reconcile`) directly against `go-engine/config.example.yaml` and `go-engine/cmd/main.go` (read-only checks; no code files edited).
- No file outside the WP-11 ownership list (`README.md`, `RUNNING.md`, `PAPER_TRADING.md`, `docs/MOBILE_APP_DESIGN.md`, `docs/RUNBOOK.md`) was modified.

## Discrepancies / notes

- `docs/MOBILE_APP_DESIGN.md`'s architecture diagram, API endpoint examples, and screen/file-structure sections describe a mobile client that has never been built (confirmed: no mobile-app frontend code exists in the repo beyond `mobile/titanmobile.go`, which only writes a config file for a WebView app). These sections were left as forward-looking design intent rather than deleted, since the task was to correct false "done" claims, not remove the design document's purpose. Only the checklist/self-review/conclusion sections (which asserted things were already built and verified) were corrected.
- py-brain is documented as Phase 2/roadmap per the explicit decision communicated in this task's briefing (kept in the tree, not removed, honestly labeled non-functional) — no py-brain files were touched (out of WP-11's file-ownership scope regardless).
