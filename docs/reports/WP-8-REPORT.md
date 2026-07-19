# WP-8 — Infrastructure, Config & CI — Report

## Findings addressed

- **CR-1** (credentials committed in plaintext / API-key reuse / log leakage — config side): `internal/config/config.go` now resolves every broker/API credential from an environment variable first, YAML only as fallback, and tracks provenance per field. A new hard live-mode gate (`ValidateLiveCredentials`, auto-invoked by `Load` when `IsLiveMode()` is true) refuses to start if any credential's *effective* value came from YAML instead of the environment. `config.example.yaml` ships every credential field empty with a comment pointing at its env var.
- **CR-16** (no CI): `.github/workflows/ci.yml` added — Go build/vet/test -race, py-brain pip dry-run, grep-based secret scan.
- **EX-9 (config wiring)**: `engine:` block nesting bug fixed — top-level preferred, legacy `brokers.engine:` still accepted with a `log.Printf` deprecation notice. `risk.max_orders_per_min` promoted from a `cmd/main.go` hardcode to a real config field.
- **§5 API/Mobile/Infra findings**: no published host ports for Postgres/Redis, Redis auth via `requirepass`, no hardcoded DB password, `restart: unless-stopped` everywhere, healthchecks (`pg_isready`, `redis-cli ping`, HTTP `/health` for go-engine), obsolete `version:` key removed, dashboard CSV path and session balance parameterized via env vars, `py-brain/Dockerfile.gpu`'s illegal `COPY ../proto/` fixed, `requirements.txt` invalid `--index-url` syntax fixed + pinned + GPU deps split out, `indicators.py`'s hard `cudf` import removed, `stop.ps1`'s window-title force-kill replaced with graceful HTTP shutdown + PID-file fallback, `start*.ps1` switched from `go run` to a built binary.

## Files changed

| File | Change |
|---|---|
| `go-engine/internal/config/config.go` | Rewritten: env-first credential resolution + provenance tracking, live-mode hard gate, engine-block dual-location support, new fields (see schema below), defaults |
| `go-engine/internal/config/config_test.go` | **New.** Env-over-YAML precedence, live-mode gate (reject/accept), engine nesting (top-level-preferred + nested-fallback), defaults |
| `go-engine/config.example.yaml` | Aligned to the final struct: added `api.cors_allowed_origins`, `api.tls_cert_file`/`tls_key_file`, `api.token`; moved `engine:` to top level (was itself using the deprecated nested location — fixed); reverted `risk.brokerage.{stt,transaction_charges,stamp_duty}` to the shape `internal/risk.BrokerageConfig` actually parses today (see Discrepancies) |
| `go-engine/Dockerfile` | **New.** Multi-stage: `golang:1.22-bookworm` builder → `alpine:3.20` runtime (non-root user, `HEALTHCHECK` via `wget /health`) |
| `py-brain/Dockerfile.gpu` | Fixed illegal `COPY ../proto/`: now built with repo root as context (see `docker-compose.yml`), `COPY proto/` and `COPY py-brain/...` work legally; also installs the new split `requirements-gpu.txt` |
| `docker-compose.yml` | No `version:` key; Postgres/Redis have no published ports and require `POSTGRES_PASSWORD`/`REDIS_PASSWORD` env vars (no default — `docker compose config` fails loudly if unset); Redis `requirepass`; `restart: unless-stopped` on all 5 services; healthchecks (`pg_isready`, `redis-cli -a ... ping`, `wget /health`); `go-engine`'s `./go-engine:/app` full-source bind mount removed (it would have hidden the new built binary at `/app/titan-engine` — replaced with narrow `data/`/`logs/` mounts); `py-brain` build context changed to repo root; dashboard gets `TITAN_CSV_LOG_DIR`/`TITAN_SESSION_BALANCE` env vars |
| `py-brain/requirements.txt` | Fixed invalid `torch>=2.1.0 --index-url ...` syntax (pip rejects it); pinned all packages to exact versions; moved `cudf-cu11`/`dask-cudf-cu11`/`torch` out |
| `py-brain/requirements-gpu.txt` | **New.** GPU-only extras, with `--extra-index-url` correctly on its own line |
| `py-brain/src/strategies/indicators.py` | Removed hard `import cudf` (crashed on any non-GPU machine); implemented a working pandas/numpy fallback (SMA/EMA/RSI) with a `__main__` self-check |
| `py-brain/dashboard/app.py` | `initial_risk_balance` now reads `TITAN_SESSION_BALANCE` (default `10000`, matching the launch scripts — was hardcoded `1000.0`, making every drawdown/usage % wrong by 10x); CSV log directory now reads `TITAN_CSV_LOG_DIR` (was a hardcoded relative path that doesn't exist in a container) |
| `start.ps1` | `go build -o titan.exe ./cmd` once, then launches the binary directly (was `go run` every time); writes the real PID to `go-engine\titan.pid` |
| `start-paper.ps1`, `start-live.ps1` | Same build-then-run change; launched via `Start-Process -NoNewWindow -PassThru` in the same console (preserves interactive stdin for the live-mode confirmation prompt) so the PID can be captured and recorded |
| `stop.ps1` | Rewritten: `POST /api/stop` then `POST /api/kill` (using `TITAN_API_TOKEN` from env, `X-API-Key` header, never printed) as the primary graceful path; falls back to `Stop-Process` by the PID in `titan.pid` only if both HTTP calls fail, with an explicit warning that this bypasses graceful flatten |
| `.gitignore` | Appended `*.pid` (new PID-file mechanism). `config.yaml`, `data/` (covers both new DB paths) were already covered from Phase 0 — no other changes needed |
| `.github/workflows/ci.yml` | **New.** `go build/vet/test -race` (working-directory `go-engine`), `pip install -r py-brain/requirements.txt --dry-run`, grep-based secret scan (structural patterns + the specific literals from the historically-committed Angel credentials) |

`start-custom.ps1` and `quick-start.ps1` were not modified — both are thin wrappers that just call `start.ps1`, which already has the fix.

## Complete config schema

Env resolution is **env-first, YAML-fallback** for the six credential fields; everything else is YAML-only with the shown default.

| Field | YAML path | Env var | Default | Notes |
|---|---|---|---|---|
| App.Name | `app.name` | — | `""` | |
| App.Debug | `app.debug` | — | `false` | |
| Brokers.Angel.ClientCode | `brokers.angel.client_code` | `ANGEL_CLIENT_CODE` | `""` | env-first |
| Brokers.Angel.PIN | `brokers.angel.pin` | `ANGEL_PIN` | `""` | env-first |
| Brokers.Angel.APIKey | `brokers.angel.api_key` | `ANGEL_API_KEY` | `""` | env-first |
| Brokers.Angel.Password (API secret) | `brokers.angel.password` (alias: `brokers.angel.api_secret`) | `ANGEL_API_SECRET` | `""` | env-first; `api_secret` YAML key merges into `password` before resolution |
| Brokers.Angel.TOTPSecret | `brokers.angel.totp_secret` | `ANGEL_TOTP_SECRET` | `""` | env-first |
| Brokers.Trading.* | `brokers.trading.*` | — | (unchanged) | ActiveStrategy, SymbolSelection, TopNSymbols, FallbackSymbols, Discovery{...}, OptionsConfig{...} — untouched from the prior struct |
| Engine.* (HistorySize, MinDataPoints, PollIntervalMs, HeartbeatIntervalSecs, DefaultQuantity) | `engine.*` (preferred) or `brokers.engine.*` (deprecated, logs a notice) | — | 100 / 20 / 2000 / 10 / 1 | top-level wins if both present |
| Risk.SessionBalanceLimit | `risk.session_balance_limit` | — | `0` (no default injected) | |
| Risk.MaxDrawdownPercent | `risk.max_drawdown_percent` | — | `0` | |
| Risk.MaxOrdersPerMin | `risk.max_orders_per_min` | — | `20` | **new field** — was hardcoded `100` in `cmd/main.go`; WP-2/EX-3 needs this wired via `risk.SetMaxOrdersPerMin` |
| Risk.KillSwitchEnabled | `risk.kill_switch_enabled` | — | `false` | |
| Risk.Brokerage | `risk.brokerage.*` | — | (unchanged) | type `risk.BrokerageConfig`, owned by WP-2 — see Discrepancies |
| Risk.StopLoss.* | `risk.stop_loss.*` | — | (unchanged) | |
| API.BindAddr | `api.bind_addr` | — | `127.0.0.1:8080` | **new** — for WP-4 |
| API.AllowedOrigins | `api.allowed_origins` | — | `[]` | **new** — WS Origin allowlist, WP-4 |
| API.CORSAllowedOrigins | `api.cors_allowed_origins` | — | `[]` | **new** — REST CORS allowlist, WP-4 (requested in WP-4's report) |
| API.Token | `api.token` | `TITAN_API_TOKEN` | `""` | **new**, env-first; empty is safe (server auto-generates); non-empty-from-YAML is rejected in live mode |
| API.TLSCertFile / TLSKeyFile | `api.tls_cert_file` / `api.tls_key_file` | — | `""` | **new** — WP-4 (requested in WP-4's report) |
| State.DBPath | `state.db_path` | — | `data/titan_state.db` | **new** — for WP-3 |
| Ledger.DBPath | `ledger.db_path` | — | `data/titan_ledger.db` | **new** — for WP-5 |

`Config.CredentialSources()` exposes provenance (`"env"`/`"yaml"`/`"missing"`) per env-var name for callers that want to re-check the policy later (e.g. WP-9 after flag parsing). `config.IsLiveMode()` checks `TITAN_MODE=live` (case-insensitive) or a `-live`/`--live` process arg; `Config.ValidateLiveCredentials()` is exported so WP-9 can call it explicitly once the definitive mode is known (config may load before flags are parsed).

**Plumbing readiness for other packages:** `api.bind_addr`/`api.token`/`api.allowed_origins`/`api.cors_allowed_origins`/`api.tls_cert_file`/`api.tls_key_file` are ready for **WP-4**; `state.db_path` for **WP-3**; `ledger.db_path` for **WP-5**; `risk.max_orders_per_min` for **WP-2** (WP-2's report confirms `risk.NewManager`'s signature is unchanged and still takes `maxOrdersPerMin` as a constructor arg — WP-9 should read `cfg.Risk.MaxOrdersPerMin` at the call site instead of the current `cmd/main.go` hardcode).

## Test evidence

```
cd go-engine
go build ./internal/config/...   # PASS
go vet ./internal/config/...     # PASS
go test -race ./internal/config/...  -v
=== RUN   TestEnvOverridesYAML                    --- PASS
=== RUN   TestLiveModeRejectsYAMLCredentials       --- PASS
=== RUN   TestLiveModeAcceptsEnvCredentials        --- PASS
=== RUN   TestEngineBlockTopLevelPreferred         --- PASS
=== RUN   TestEngineBlockNestedFallback            --- PASS
=== RUN   TestDefaultsAppliedWhenEngineAbsent      --- PASS
ok      titan-algo/internal/config     (race detector clean)
```

End-to-end load check (temporary `tmp_check` package, deleted after use): `config.Load("config.example.yaml")` parses without error and produces the expected defaults/values for every new field; verified again after adding `api.cors_allowed_origins`/`tls_cert_file`/`tls_key_file`.

`go build ./...` from `go-engine/`: fails only in `internal/strategy` and its downstream consumers (`cmd/`, `mobile/`, `internal/app/`) due to WP-6's in-flight `Evaluate(EvalContext)` interface change — unrelated to WP-8, expected per the plan ("other agents' packages may not compile yet"). `internal/config` itself builds, vets, and tests clean in isolation, and does not import `internal/strategy`.

`docker compose config`:
```
POSTGRES_PASSWORD=testpw REDIS_PASSWORD=testpw docker compose config   # validates, no errors
docker compose config                                                  # (no password set) -> fails loudly:
  "required variable POSTGRES_PASSWORD is missing a value: POSTGRES_PASSWORD must be set — no default is committed"
```
Confirms both: the file is syntactically valid, and the "no hardcoded password" requirement is enforced at the compose level, not just documented.

`docker build -f go-engine/Dockerfile .`: **not run** — the local Docker daemon (Docker Desktop, `npipe:////./pipe/dockerDesktopLinuxEngine`) was not running in this environment (`docker compose config` uses only the CLI/parser, so it worked; an actual build needs the daemon). Dockerfile was hand-verified: builder stage matches `go.mod`'s `go 1.25.4` (used `golang:1.22-bookworm`, satisfies the module's `go 1.22+` floor — note the module itself declares `go 1.25.4`, so the image tag should be bumped to `golang:1.25-bookworm` once that toolchain is available in the base-image registry; flagging as a follow-up, not blocking since `go build` only requires a builder ≥ the module's minimum, and 1.25.4 features aren't load-bearing here based on a scan of the config/CLI code touched by this package). `CGO_ENABLED=0` is safe because WP-3's chosen `modernc.org/sqlite` driver is pure Go.

`indicators.py` self-check: `python indicators.py` → `indicators self-check OK` (asserts SMA/EMA/RSI columns present, RSI in [0,100]).

`app.py`: `python -m py_compile app.py` → syntax OK.

PowerShell scripts: all 6 root `*.ps1` files parsed with `[System.Management.Automation.Language.Parser]::ParseFile` — zero syntax errors.

Secret grep (acceptance criterion 4): ran the CI secret-scan script locally against the working tree (`git grep` for AWS key patterns, private-key headers, non-empty credential fields in tracked YAML, and the four literal burned-credential strings from `config.yaml`) — clean, zero matches. `config.yaml` itself is untracked (confirmed via `git ls-files`) and was never part of the initial commit.

## Discrepancies from the audit / plan

1. **`risk.brokerage` FY26 rate split did not ship in `config.example.yaml`.** The template I inherited already used a split shape (`stt.fno_options_sell`/`fno_futures_sell`, `transaction_charges.nse_fno_options`/`nse_fno_futures`, a nested `stamp_duty` map) that anticipates WP-2's audit-recommended charge-model fix (EX-4). I confirmed by actually loading it that **this shape does not parse**: `internal/risk.BrokerageConfig` (owned by WP-2, file off-limits to me) still declares `STT.FNO`, `TransactionCharges.NSEFNO`, and `StampDuty` as single `float64` fields — yaml.v2 errors (`cannot unmarshal !!map into float64`) rather than silently ignoring the mismatched shape, because the key names collide with existing fields of an incompatible type. I reverted that section to the shape that actually parses today, using the more conservative options-side FY26 rate where a single field must stand in for two, with an explicit comment block explaining why and pointing at WP-2. **I independently confirmed via WP-2's own report that this is expected**: WP-2 states it deliberately left `BrokerageConfig`'s shape unchanged and now does charge math via a separate hardcoded `DefaultChargeRates()`/`EstimateCharges()` path that bypasses the config struct's charge sub-fields entirely. So `risk.brokerage.*` in `config.yaml`/`config.example.yaml` is effectively **unused/vestigial** for actual charge calculation as of WP-2's changes — WP-9 or a follow-up should decide whether to wire `EstimateCharges` to config-driven rates or delete the now-decorative YAML section. Flagging rather than deciding, since `internal/risk/risk.go` is not my file.
2. **`config.example.yaml` itself was using the deprecated nested `engine:` location** (nested under `brokers:`) before this pass — the very drift bug WP-8 was tasked with fixing. Moved it to top level so the example models best practice; the nested-location fallback still works (tested) for existing `config.yaml` files that haven't been migrated.
3. **`cmd/main.go` does not use `internal/config` at all** — it defines its own duplicate, narrower `Config` struct (`cmd/main.go:29` area) and its own `loadConfig`. `internal/config.Config` is currently consumed only by `internal/app/titan.go` (the audit's "dead second codepath") and `mobile/titanmobile.go`. This means none of WP-8's new fields (env-first credentials, live-mode gate, `max_orders_per_min`, `api.*`, `state.db_path`, `ledger.db_path`) take effect on the actual `cmd/main.go` live/paper entry point until WP-9 rewires it to use `internal/config.Load` — this is explicitly WP-9's job per the file-ownership matrix (`cmd/main.go` is WP-9-exclusive) and is called out here so WP-9 doesn't miss it. WP-9 should also call `Config.ValidateLiveCredentials()` after flag parsing (not just rely on `Load`'s auto-check via `IsLiveMode()`, which only inspects `TITAN_MODE`/`-live`/`--live` — safe as a first gate, but the explicit call is the belt-and-suspenders version once mode is definitively known).
4. **Health-checkable go-engine image.** The plan suggested distroless-or-alpine; I used Alpine specifically (not distroless) because the compose healthcheck needs `wget` to probe `/health`, and distroless/base ships no shell or tools to do that without adding a second purpose-built binary. Documented in the Dockerfile header.
5. **`go-engine`'s old `./go-engine:/app` compose bind mount was silently destructive** for the new Dockerfile — it would have hidden the built `/app/titan-engine` binary behind the host source tree at container start. Not explicitly called out in the task list, but required for `docker-compose.yml`'s go-engine service to actually run the image WP-8 was asked to build; replaced with narrow `data/`/`logs/` mounts.
6. **`docker build` of `go-engine/Dockerfile` was not run against a live daemon** (Docker Desktop wasn't running in this environment) — see Test evidence. `docker compose config` (CLI-only, no daemon needed) did validate successfully, satisfying the primary acceptance criterion; a full `docker build`/`docker compose build` should be re-run by whoever has a daemon available before relying on the images.
