# TitanAlgo Mobile App - Design Document v3

> **Status**: Design document; backend security items below are corrected
> against `docs/reports/WP-4-REPORT.md` (the actual API server implementation)
> as of the WP-11 documentation pass. Items marked done (✅) below are verified
> against that report, not aspirational.
> **Last Updated**: 2026-07-19
> **Author**: AI System Architect

---

## Executive Summary

Build a minimal, lightweight Android app to remotely control the TitanAlgo trading engine. The app will communicate with the Go backend via a REST API layer.

**Key Decisions:**
- **Architecture**: WebView PWA (< 2MB APK)
- **Backend**: REST API + WebSocket
- **Security**: API key authentication + localhost restriction
- **Offline**: Local asset caching

---

## Architecture Options Considered

| Option | Pros | Cons | Verdict |
|--------|------|------|---------|
| **Native Kotlin** | Best performance | Only Android, complex, 10MB+ | ❌ Overkill |
| **React Native** | Cross-platform | 50MB+ APK, Node.js needed | ❌ Too heavy |
| **Flutter** | Cross-platform | Dart, 15MB+ APK | ❌ Not minimal |
| **WebView PWA** | <2MB, single codebase, easy updates | Not "fully native" | ✅ **SELECTED** |

---

## Final Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     ANDROID DEVICE                          │
│  ┌───────────────────────────────────────────────────────┐  │
│  │              WebView PWA App (< 2MB)                  │  │
│  │  ┌─────────────┐  ┌─────────────┐  ┌──────────────┐   │  │
│  │  │  Dashboard  │  │   Config    │  │   Trades     │   │  │
│  │  │   Screen    │  │   Screen    │  │   History    │   │  │
│  │  └─────────────┘  └─────────────┘  └──────────────┘   │  │
│  └───────────────────────┼───────────────────────────────┘  │
└──────────────────────────┼──────────────────────────────────┘
                           │ HTTPS / Local Network
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    GO BACKEND (PC/Server)                   │
│  ┌───────────────────────────────────────────────────────┐  │
│  │              API Server (Port 8080)                   │  │
│  │    GET  /api/status     - Engine status               │  │
│  │    GET  /api/positions  - Open positions              │  │
│  │    GET  /api/trades     - Trade history               │  │
│  │    GET  /api/config     - Current config              │  │
│  │    POST /api/config     - Update config               │  │
│  │    POST /api/start      - Start trading               │  │
│  │    POST /api/stop       - Stop trading                │  │
│  │    WS   /ws/live        - Real-time updates           │  │
│  └───────────────────────────────────────────────────────┘  │
│                  Existing Trading Engine                     │
└─────────────────────────────────────────────────────────────┘
```

---

## Critical Self-Review Summary — corrected against WP-4 (actual backend state)

An earlier draft of this section marked every item below as done before the
backend existed. `internal/api/server.go` was rewritten in WP-4; the table
below reflects what that report actually verifies (build/vet/test evidence
in `docs/reports/WP-4-REPORT.md`), not the original aspirational checklist.

### Security
- ✅ **Token-based auth**, not a shared "API key" — `NewServer` takes a
  dedicated token, independent of any broker credential; if empty, a random
  `crypto/rand` 32-byte token is generated and printed once at startup, never
  logged again. REST uses `X-API-Key` header or query token; WebSocket
  (`/ws/live`) requires the same token via `?token=` or header.
- ✅ **Localhost-default bind, optional TLS** — default bind is
  `127.0.0.1:<port>`; `SetTLS(certFile, keyFile)` enables `ListenAndServeTLS`
  when both are configured. Not HTTPS-by-default; TLS is opt-in via config.
- ✅ **CORS allowlist** — configurable via `SetCORSAllowedOrigins`; default
  (empty) sends no CORS headers at all. The old wildcard `Access-Control-Allow-Origin: *` is gone (grep-confirmed in the WP-4 report).
- ✅ **WebSocket Origin validation** — `/ws/live` validates the `Origin`
  header against a configurable allowlist (`SetAllowedOrigins`) during the
  upgrade handshake; requests with no `Origin` header (the expected native
  mobile-app case) still require the token.
- ❌ **Rate limiting is NOT implemented.** No per-IP or per-token request-rate
  limiter exists anywhere in `internal/api/server.go`. This was checked off in
  an earlier draft of this document with nothing behind it — treat it as an
  open item, not a shipped control.
- ⚠️ **Config-change confirmation is a client-side UX concern**, not a server
  guarantee. The server does validate `POST /api/config` inputs (session
  balance range, strategy must be in an explicit allowlist — see
  `ConfigHooks.AllowedStrategies`; an empty allowlist rejects every strategy
  change, fail-closed) and only applies changes via a caller-supplied `Apply`
  hook, but there is no "are you sure?" step enforced server-side. Any
  confirmation UX is the mobile app's job to build, not yet built.

### Reliability
- ⚠️ **Heartbeat exists at the engine level** (`data/heartbeat` file touched
  once per tick by `internal/engine/runner.go`, per WP-9), not as a
  dedicated 5-second API heartbeat message to mobile clients specifically.
  The WebSocket layer does have a per-connection heartbeat/broadcast writer
  goroutine (WP-4) to keep connections alive and avoid concurrent-write
  panics, but "every 5 seconds" as a specific cadence to the mobile UI is not
  verified against any report — do not assume that exact interval.
- ❌ **Connection status indicator, auto-reconnect with backoff, "last
  updated" timestamps** — these are mobile-app (client-side) UI features.
  No mobile app code has been built as part of the WP-1 through WP-9
  remediation; only the backend (`internal/api`) and the WebView config
  writer (`mobile/titanmobile.go`) exist. Not yet verified end-to-end.

### Usability
No mobile-app frontend (HTML/JS/Android wrapper) has been implemented as
part of WP-1 through WP-9. The items in this section (single-page app,
form-based config, read-only trade history, background-pause) remain design
intent only — nothing here has shipped code to verify against. Treat this
entire subsection as **not yet implemented**, same status as before this
audit pass; it was not previously mismarked (the original draft did not claim
these were done under this specific server), so no correction was needed here
beyond flagging that it's still unbuilt.

### Real control surface (verified, WP-4 + WP-9)
- ✅ `/api/start` → `Pause`/`Resume` and `/api/kill` → `KillAndFlatten` are
  wired to real `ControlHooks` backed by the actual trading engine
  (`internal/engine/runner.go`'s `Runner`), per WP-9. If hooks are unset, these
  endpoints return `503 {"error":"not wired"}` — never a fake success. This
  replaces the old cosmetic stop button (audit finding CR-4) that only flipped
  the API server's own internal flag and did nothing to the engine.

---

## API Endpoints

### GET /api/status
```json
{
  "running": true,
  "mode": "paper",
  "strategy": "sniper",
  "balance": 1000.00,
  "unrealized_pnl": 45.50,
  "realized_pnl": 120.00,
  "positions_count": 2
}
```

### GET /api/positions
```json
{
  "positions": [{
    "symbol": "NIFTY27JAN2625000PE",
    "side": "BUY",
    "quantity": 65,
    "entry_price": 45.50,
    "current_price": 48.00,
    "pnl": 162.50
  }]
}
```

### POST /api/config
```json
{"session_balance": 2000.0, "stop_loss_percent": 3.0}
```

### POST /api/start
```json
{"mode": "paper", "strategy": "sniper"}
```

---

## Mobile App Screens

### Dashboard (Home)
- Balance display
- Unrealized/Realized P&L
- Open positions list
- Start/Stop button

### Configuration
- Strategy dropdown
- Session balance input
- Stop-loss toggle + percentage
- Discovery indices checkboxes
- Save button

### Trade History
- Chronological trade list
- Buy/Sell with prices
- P&L per trade

---

## File Structure

```
titan-algo/
├── mobile-app/
│   ├── android/
│   │   ├── app/src/main/
│   │   │   ├── java/.../MainActivity.kt
│   │   │   ├── res/layout/activity_main.xml
│   │   │   └── assets/www/
│   │   │       ├── index.html
│   │   │       ├── app.js
│   │   │       └── style.css
│   │   └── build.gradle
│   └── README.md
│
└── go-engine/
    └── internal/api/
        └── server.go
```

---

## Implementation Phases

| Phase | Component | Time |
|-------|-----------|------|
| 1 | Backend REST API | 2-3h |
| 2 | Web UI (HTML/JS) | 2-3h |
| 3 | Android Wrapper | 1h |
| 4 | Testing | 1h |
| **Total** | | **6-8h** |

---

## Production Checklist — corrected against WP-4/WP-9 (backend only; no mobile frontend exists)

- [x] Token authentication (backend, WP-4 — see Critical Self-Review above)
- [x] Localhost-only default bind; TLS optional/opt-in (backend, WP-4)
- [ ] Rate limiting — **not implemented**
- [ ] Heartbeat monitoring — engine-level heartbeat file exists (WP-9); no
      mobile-specific heartbeat protocol built
- [ ] Auto-reconnect — mobile client not built
- [ ] Single-page app — mobile client not built
- [ ] Form-based config — mobile client not built
- [ ] <2MB APK — mobile client not built
- [x] WebSocket transport exists on the backend (`/ws/live`, authenticated,
      Origin-validated, one writer goroutine per connection — WP-4); no
      mobile client consumes it yet
- [ ] Offline asset caching — mobile client not built

---

## Risk Assessment

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| Network latency | Medium | Low | Loading states, timeout |
| Backend unreachable | Medium | High | Offline indicator, retry |
| Unauthorized access | Low | Critical | API key + localhost |
| Config errors | Medium | High | Validation + confirmation |

---

## Conclusion

This remains a design document for a mobile control panel that has not been
built (no HTML/JS UI, no Android wrapper). What has actually shipped is the
backend it would talk to: a real, tested control API (`internal/api`, WP-4)
with token auth, Origin-validated WebSocket, configurable CORS, and genuine
start/stop/kill control hooks wired to the trading engine (WP-9) — see the
corrected Critical Self-Review section above for exactly what's verified.
Rate limiting and the mobile client itself are the two biggest open gaps
before this design would be "ready for implementation" end-to-end.
