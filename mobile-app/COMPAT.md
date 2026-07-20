# Mobile app vs. current API server contract (G-14) — compatibility notes

Written by R2-5. Scope per the round-2 plan: assess `mobile-app/**` against
the hardened API server (`docs/reports/WP-4-REPORT.md`: token via
`X-API-Key` header or `?token=` query param, no CORS wildcard, Origin-checked
WS), fix what's small directly, park what's larger here. R2-5 does not own
`go-engine/mobile/titanmobile.go` or any other `go-engine/**` file this
round — anything below that requires touching that file is explicitly
**not** fixed here, only diagnosed with exact file:line references.

## Architecture (for context)

Per `mobile-app/README.md` / `BUILD_INSTRUCTIONS.md`, this is a
**self-sufficient** app: `gomobile bind` compiles the entire Go engine
(`go-engine/mobile/titanmobile.go` → `mobile.aar`) into the APK. There is no
PC/desktop server involved — `MainActivity.kt` starts the embedded engine
in-process, which runs its own `internal/api.Server` on `localhost:8080`
inside the phone, and a `WebView` loads `www/index.html`/`app.js` from
`file:///android_asset/www/` and talks to that same-device `localhost:8080`.

Two on-disk copies of the UI exist and were, before this fix, out of sync:
- `mobile-app/www/` — the source-of-truth copy referenced by `README.md`.
- `mobile-app/android/app/src/main/assets/www/` — a **separate, duplicated
  copy** that `MainActivity.kt:55` (`webView.loadUrl("file:///android_asset/www/index.html")`)
  actually loads into the APK. Editing only `www/app.js` would have had **no
  effect on the built app** — the asset copy is what ships.

## Fixed directly (small, applied in both copies)

**Bug:** `connect()` built the WebSocket URL as `.../ws/live` with no token,
while the server's `handleWebSocket` (`go-engine/internal/api/server.go`,
see WP-4-REPORT "Endpoint surface changes") requires `?token=` or
`X-API-Key` before it will upgrade the connection — every WS connection
attempt got a 401 and the app silently fell back to the reconnect/backoff
loop forever, never actually streaming live updates.

**Fix applied** (identical in both files, kept in sync):
- `mobile-app/www/app.js:248-256`
- `mobile-app/android/app/src/main/assets/www/app.js:248-256`

```js
const wsUrl = CONFIG.serverUrl.replace('http', 'ws') + '/ws/live?token=' + encodeURIComponent(CONFIG.apiKey);
```

`CONFIG.apiKey` already existed and was already used correctly for every
REST call (`api()` helper, `app.js:40-47`, sets `X-API-Key`) — it just wasn't
being reused for the one WS connection call. The Settings modal
(`index.html:136-153`, `#api-key` input) already lets an operator type in
whatever token the server is using; that value now reaches both REST and WS.

This is a real fix for **any** deployment where the token is knowable and
stable (e.g. `TITAN_API_TOKEN` set as an explicit env var before the engine
starts). It does **not** fully close the gap below.

## Resolved

**Issue #1: Token surface (FIXED)**

The token is now surfaced via logging. `internal/app/titan.go:156` logs the 
token using `log.Printf()` (not `fmt.Println`), which is captured by the 
`log.SetOutput(logFile)` redirect set up in `mobile/titanmobile.go:21-22`. 
The token is written to `titan_mobile.log` on app startup with a clear label: 
`🔑 API auth token (needed for mobile Settings / REST X-API-Key / WS ?token=)`. 
An operator can read this token from the log file and paste it into the 
Settings modal, where it's then used for both REST and WS connections 
(as long as the server has not been built with a pre-set token).

## Parked (larger — requires a `go-engine/mobile/titanmobile.go` change, out of R2-5's file ownership)

**Remaining issue: token regenerates on every app launch unless pre-set.**

While the token is now surfaced via the log file, it is regenerated on every 
process start when no token is supplied via `TITAN_API_TOKEN` env var 
(`go-engine/internal/api/server.go`, `NewServer`, the `if token == ""`
branch). On a phone there is no visible way to set environment variables 
(Gomobile has no console and no env-var exposure in Android glue), so every 
app launch forces a new token that must be manually transcribed from the log 
file into the Settings modal to reconnect.

2. **`MainActivity.kt:24`** passes a hardcoded literal into the wrong slot:
   ```kotlin
   val result = Mobile.start(filesDir.absolutePath, "titan-mobile-secret")
   ```
   `go-engine/mobile/titanmobile.go`'s `Start(dataDir, apiKey string)` maps
   this argument to `cfg.Brokers.Angel.APIKey` (the **broker** credential),
   not the API server's auth token — see `titanmobile.go:39-42`
   (`if apiKey != "" { cfg.Brokers.Angel.APIKey = apiKey }`). This is a
   pre-WP-4 leftover: `MainActivity.kt`'s own comment ("titan-mobile-secret
   matches the API key in app.js") is now **stale** — `app.js` no longer
   hardcodes that string anywhere (WP-4 removed it; confirmed by
   `grep -rn "titan-mobile-secret" mobile-app/` returning no matches). Since
   the mobile app always forces `PAPER` mode
   (`titanmobile.go:46`, `app.NewApp(cfg, "MODE_A", "PAPER", 1000.0)`), a
   bogus broker API key has no live-trading blast radius today, but the line
   does nothing useful and should either be deleted or repurposed once (1)
   is fixed.

3. **What a real fix needs** (sketch, not applied — touches
   `go-engine/mobile/titanmobile.go`, out of this package's ownership):
   - `titanmobile.go`'s `Start` should generate-once-and-persist a stable
     API token to a file under `dataDir` (reuse on subsequent launches
     instead of Gomobile/`api.NewServer`'s per-process random default), and
     return it in `Start`'s result string (or expose a new `Mobile.GetAPIToken()`
     accessor) instead of relying on a print that never reaches the phone.
   - `MainActivity.kt` should read that returned/queried token and inject it
     into the WebView before first use, e.g.
     `webView.evaluateJavascript("localStorage.setItem('apiKey', '<token>')", null)`
     right after `Mobile.start()` returns and before/soon after
     `webView.loadUrl(...)`, so the Settings modal is pre-filled and the
     operator never has to manually transcribe a token they have no way to
     see.
   - Once that's in place, the `?token=` fix already applied above will work
     end-to-end without any manual step.

## Not re-verified (matches, not re-checked line by line)

- REST auth header (`X-API-Key`) — already correct in `app.js`'s `api()`
  helper (`app.js:40-47`), unchanged by WP-4 (server still reads
  `X-API-Key`, confirmed against `server.go`'s `authMiddleware`).
- CORS — irrelevant here; this WebView loads `file:///android_asset/...`,
  which typically sends no `Origin` header for a same-device
  `localhost` fetch (native-app case, `checkOrigin()` in `server.go` allows
  requests with no `Origin` header through unconditionally). Not verified
  against an actual device/emulator in this session (no Android
  toolchain/emulator available here) — flagging as an assumption, not a
  confirmed fact, in case a WebView configuration change (e.g. a future
  switch to `WebViewAssetLoader` serving over `https://appassets.androidplatform.net`)
  starts sending a real `Origin` header, which would then need
  `SetAllowedOrigins` to include it.
