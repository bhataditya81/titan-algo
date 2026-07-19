# WP-4 — API Server & Mobile Security — Report

## Owned files touched
- `go-engine/internal/api/server.go` (rewritten)
- `go-engine/internal/api/server_test.go` (new)
- `go-engine/mobile/titanmobile.go` (targeted edits)

No other files touched.

## Findings addressed

| Finding | Fix |
|---|---|
| CR-1 (API-key reuse) | `NewServer` no longer accepts/assumes a broker key; token is a dedicated credential. Constructor doc comment tells WP-9 explicitly never to pass `cfg.Brokers.Angel.APIKey`. |
| CR-1 (partial key logging) | Deleted `log.Printf("🔑 API Key: %s", s.apiKey[:8]+"...")`. Token is never passed to `log.*` anywhere in the file. |
| CR-1 (`titan-mobile-secret` fallback) | Removed from `mobile/titanmobile.go` (was the default `brokers.angel.api_key` in the generated config, not a server-token fallback — see Discrepancies). Also confirmed absent from `server.go`. |
| CR-4 (cosmetic stop) | `/api/start`, `/api/stop`, `/api/kill` now call `ControlHooks.Resume/Pause/KillAndFlatten`; return `503 {"error":"not wired"}` when hooks unset. No fake success. |
| §5 HIGH: bind `:8080` all interfaces, plaintext | Default bind is `127.0.0.1:<port>`. `SetBindAddr` for override. `SetTLS(certFile, keyFile)` → `ListenAndServeTLS` when both set. |
| §5 HIGH: `/ws/live` no auth, `CheckOrigin` allow-all | Token required (`?token=` query param or `X-API-Key` header) before upgrade. `CheckOrigin` validates against an allowlist (`SetAllowedOrigins`); browser-originated (Origin header present) cross-origin requests are rejected unless allowlisted; non-browser clients (no Origin header) pass through (still need the token). |
| §5 MEDIUM: WS concurrent-write race | One writer goroutine per connection (`wsClient.writePump`) reading a buffered channel (`wsClient.send`, size 32). Heartbeat and `broadcast()` both call `enqueue()`, never `conn.WriteJSON`/`WriteMessage` directly. Full channel → connection dropped (`enqueue` returns false → caller removes client), never blocks. |
| §5 MEDIUM: `/api/config` unvalidated, hardcoded GET | POST validates `session_balance` (>0, ≤ `ConfigHooks.MaxSessionBalance` or `DefaultMaxSessionBalance`=10,00,000) and `strategy` (must be in `ConfigHooks.AllowedStrategies`; empty allowlist = all strategy changes rejected, fail-closed). GET sources from `ControlHooks.Status()` when wired, else last-validated cached values. |
| §5 LOW: mobile world-readable config, hardcoded key | `os.WriteFile(path, ..., 0600)`. Placeholder `api_key: "titan-mobile-secret"` → `api_key: ""` (mobile always forces PAPER mode, so a blank key is safe; see comment in file). |
| Wildcard CORS (`Access-Control-Allow-Origin: *`) | Replaced with `SetCORSAllowedOrigins([]string)` allowlist; empty (default) = no CORS headers sent at all. |

## Interfaces for WP-9 (critical — read before wiring)

### `NewServer` token contract
```go
func NewServer(port int, token string) *Server
```
- `token` is the server's own credential — **independent of any broker credential**. WP-9 must source it from a dedicated config value (recommend `TITAN_API_TOKEN` env var per the plan's Phase-0 list), never from `cfg.Brokers.Angel.APIKey`.
- If `token == ""`, a random 32-byte `crypto/rand` token (hex-encoded, 64 chars) is generated and printed **once** to stdout via `fmt.Println` (not `log.*`) with a clear banner. It is never logged again — do not add logging of `s.token` anywhere, and don't log the constructor's `token` argument either.
- Existing call site to fix in `internal/app/titan.go:104-110` (WP-9 owned):
  ```go
  apiKey := app.Config.Brokers.Angel.APIKey
  if apiKey == "" {
      apiKey = "titan-mobile-secret"
  }
  app.ApiServer = api.NewServer(8080, apiKey)
  ```
  must become something like:
  ```go
  app.ApiServer = api.NewServer(8080, app.Config.API.Token) // new dedicated config field, NOT Brokers.Angel.APIKey
  ```
  (report the new `api.token` / `TITAN_API_TOKEN` config field to WP-8, same as the plan's own note for `api.bind_addr`).

### `ControlHooks` — exact signature
```go
type ControlHooks struct {
    Pause          func() error
    Resume         func() error
    KillAndFlatten func() error
    Status         func() EngineStatus
}

type EngineStatus struct {
    Running          bool
    Mode             string
    Strategy         string
    Balance          float64
    UnrealizedPnL    float64
    RealizedPnL      float64
    PositionsCount   int
    StopLossEnabled  bool
    StopLossPercent  float64
    DiscoveryEnabled bool
    Indices          []string
}

func (s *Server) SetControlHooks(hooks ControlHooks)
```
- `Pause` → wired to `/api/stop` (soft stop: block new entries, exits still allowed per plan §4 task 4).
- `Resume` → wired to `/api/start`.
- `KillAndFlatten` → wired to new `/api/kill` (hard stop + flatten).
- `Status` → sourced by `/api/status` (preferred over the server's internal cached fields when set) and by `/api/config` GET (`Strategy`, `Balance`, `StopLossEnabled`, `StopLossPercent`, `DiscoveryEnabled`, `Indices`).
- **Until `SetControlHooks` is called, `/api/start`, `/api/stop`, `/api/kill` all return `503` with body `{"error":"not wired"}`.** No fake `{"success":true}`.
- If a hook function returns `error`, the endpoint returns `500` with `{"error": "<message>"}`.

### `ConfigHooks` (separate from `ControlHooks`, optional)
```go
type ConfigHooks struct {
    AllowedStrategies []string
    MaxSessionBalance float64 // <=0 → DefaultMaxSessionBalance (1,000,000)
    Apply             func(sessionBalance float64, strategy string) error
}

func (s *Server) SetConfigHooks(hooks ConfigHooks)
```
- `AllowedStrategies` empty (default, i.e. `SetConfigHooks` never called) → **any POST that includes `"strategy"` is rejected with 400** (fail-closed, per Appendix B). WP-9 must call `SetConfigHooks` with the real registered strategy list before a strategy-change UI is usable.
- `Apply(sessionBalance, strategy)` is called with the already-validated values after both are merged with any not-updated existing value; wire it to actually push the change into the risk manager / engine. If unset, POST still validates and updates the server's own cached copy (visible via GET) but has no effect on the real engine — **WP-9 must set `Apply` before this endpoint does anything real.**

### Other setters (call before `Start()`)
```go
func (s *Server) SetBindAddr(addr string)                  // default "127.0.0.1:<port>"
func (s *Server) SetTLS(certFile, keyFile string)           // both non-empty → ListenAndServeTLS
func (s *Server) SetAllowedOrigins(origins []string)        // WS Origin allowlist; empty = reject all browser-Origin WS
func (s *Server) SetCORSAllowedOrigins(origins []string)    // REST CORS allowlist; empty = no CORS headers (native-app default)
```

### New config fields to report to WP-8 (`internal/config/config.go`)
- `api.token` (string, resolved from env `TITAN_API_TOKEN` first) — the dedicated server token.
- `api.bind_addr` (string, default `127.0.0.1:8080`) — already anticipated by the plan.
- `api.tls_cert_file`, `api.tls_key_file` (string, optional).
- `api.allowed_origins` ([]string) — WS Origin allowlist.
- `api.cors_allowed_origins` ([]string) — REST CORS allowlist (leave empty for the mobile-only deployment).

### Endpoint surface changes
- New: `POST /api/kill` → `KillAndFlatten`.
- `/api/status`, `/api/config` (GET) prefer `ControlHooks.Status()` when set, else fall back to the last value pushed via the existing `UpdateStatus(...)` method (kept, unchanged signature, for any code that still pushes status rather than polling via `Status`).
- `/ws/live` now requires `?token=<token>` (or `X-API-Key` header) and enforces the Origin allowlist during upgrade.

## Test evidence

`go build ./internal/api/... && go vet ./internal/api/...` — clean.
`gofmt -l internal/api/server.go internal/api/server_test.go mobile/titanmobile.go` — clean (no output) after formatting.

`go test -race ./internal/api/...` — **PASS**, 20 tests, 4.9s:
- Token: generation randomness/length, provided-token passthrough, default bind = `127.0.0.1:8080`.
- REST: all 7 authenticated endpoints reject missing/wrong token (401); valid token accepted.
- Control hooks: all three endpoints return 503 `{"error":"not wired"}` when unset; correct hook invoked exactly once each when set; hook error → 500.
- Config: POST validation table (zero/negative/over-max balance, wrong type, unknown/empty strategy, invalid JSON) all rejected 400; valid values accepted 200; strategy change with no allowlist configured rejected 400; GET reflects hook-sourced `EngineStatus` values.
- WebSocket: missing token → 401 on upgrade; bad token → 401; valid token via query param and via header both succeed; disallowed Origin rejected; allowlisted Origin accepted.
- **Concurrent-write stress** (`TestWebSocketConcurrentBroadcastAndHeartbeat`): 4 real WS clients, 8 goroutines hammering `broadcast()`/`UpdateStatus()` (200 iterations each) concurrently with the server's own per-connection heartbeat ticker, run under `-race` — no panic, no data race.
- **Slow consumer**: a client that never reads gets dropped once its 32-message buffer fills; `broadcast()` never blocks (bounded by a 10s test timeout that is never hit).
- CORS: no headers by default; allowlisted origin gets header echoed; non-allowlisted gets nothing; wildcard never appears.

```
ok  	titan-algo/internal/api	4.945s
```

Grep verification (from `go-engine/`):
```
grep -rn "titan-mobile-secret" internal/api/ mobile/     → no matches
grep -n "apiKey\[" internal/api/server.go mobile/titanmobile.go   → no matches
grep -n '"\*"' internal/api/server.go                     → no matches (no wildcard CORS)
```
The only remaining `log.*`/`fmt.*` calls touching the token are the one-time `fmt.Println(token)` banner in `NewServer` (stdout, not the `log` package, fires only when a token was generated) and `log.Fatalf` on `crypto/rand` failure (does not print the token).

## Discrepancies vs. the audit/plan

1. **`titan-mobile-secret` location.** The plan/audit describe this fallback as living in `server.go:120` "here and titanmobile.go:78". Actual code: `server.go` has no such fallback string at all — the credential-reuse bug there is `log.Printf("🔑 API Key: %s", s.apiKey[:8]+"...")` plus `NewServer` accepting whatever key the caller passes (which happened to be the broker key). The literal string `"titan-mobile-secret"` exists in two places, **neither of which is `server.go`**:
   - `mobile/titanmobile.go:78` (owned by me — fixed: was the default `brokers.angel.api_key` placeholder in the mobile app's generated config, not a server-token fallback).
   - `internal/app/titan.go:108` (owned by WP-9, **not fixed here** — file ownership forbids editing it):
     ```go
     apiKey := app.Config.Brokers.Angel.APIKey
     if apiKey == "" {
         apiKey = "titan-mobile-secret"
     }
     app.ApiServer = api.NewServer(8080, apiKey)
     ```
     This is the actual CR-1 credential-reuse call site (broker API key passed directly as the server token, with a guessable string as its own fallback). **WP-9 must delete this fallback and stop reading `Config.Brokers.Angel.APIKey` for the server token** — see "Interfaces for WP-9" above for the required replacement. Flagging this clearly since acceptance criteria says "grep confirms no occurrence of titan-mobile-secret" — that now holds for all files in WP-4's ownership, but the string still exists in `internal/app/titan.go` until WP-9 acts on it.

2. **Angel access-token stdout print (`angel_broker.go:202`).** Audit CR-1 also flags this line printing an access-token prefix to stdout. That file is owned by WP-1, out of scope here — noting it for completeness since it's part of the same CR-1 finding cluster.

3. **`go build ./...` at repo root does not currently pass**, but not due to WP-4 changes. Root cause is unrelated, concurrent Wave-1 work in packages this package doesn't own:
   - `internal/strategy/*.go` — mid-refactor to the new `Evaluate(EvalContext) Signal` signature (WP-6's task); several strategies still implement the old `Evaluate(string, []float64, []float64, time.Time) Signal` and fail to satisfy the `Strategy` interface.
   - `internal/broker/angel_broker.go` — imports `golang.org/x/time/rate`, not yet present in `go.mod` (WP-1's allowed new dependency, not yet run through `go get`).
   `go-engine/internal/api` and its test package build, vet, and test clean in complete isolation (`go build ./internal/api/...`, `go vet ./internal/api/...`, `go test -race ./internal/api/...` all green — see Test evidence). `mobile/titanmobile.go` also builds standalone-syntax-clean but transitively fails to build only because it imports `internal/app` → `internal/broker`, which currently fails for the `x/time/rate` reason above (pre-existing/parallel WP-1 state, not a WP-4 defect). Re-run `go build ./...` after WP-1 and WP-6 land.

4. **CORS "same-origin-only" wording.** Task 3 says the WS Origin allowlist should "treat empty as same-origin-only or reject cross-origin" — a bare API server has no natural browser "same origin" concept, so empty allowlist rejects every WS upgrade that carries an `Origin` header at all (i.e., any browser-based client), while non-browser clients (no `Origin` header — the expected native mobile app case) pass the Origin check unconditionally and still need the token. Documented in the `SetAllowedOrigins` doc comment.

5. **Extra `EngineStatus` fields beyond the plan's literal example.** Plan's task 6 shows `Status func() EngineStatus` without defining `EngineStatus`'s fields. I extended `EngineStatus` with `StopLossEnabled/StopLossPercent/DiscoveryEnabled/Indices` (beyond `Running/Mode/Strategy/Balance/UnrealizedPnL/RealizedPnL/PositionsCount`) so a single `Status()` hook can also satisfy task 7's "GET /api/config returns real current values sourced from hooks" requirement without inventing a second, overlapping hook type. `ControlHooks`'s four function fields themselves are unchanged from the plan's literal spec.
