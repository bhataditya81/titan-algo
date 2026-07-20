# TitanAlgo Web UI — Frontend Report

Location: `titan-algo/web-ui/` (new top-level directory, sibling to `go-engine/`, `py-brain/`, `mobile-app/`). No build step — open `index.html` directly or serve statically (`python -m http.server` from `web-ui/`).

## File structure

| File | Contents |
|---|---|
| `index.html` | Single-page markup: header (connection dot, mode badge, Settings button), control panel (strategy/balance/symbol-mode/Start/Pause/Kill), live status KPI tiles, chart panel (symbol/limit/Load/view-toggle + chart container + table fallback), trade history table, Settings `<dialog>`, kill-confirm `<dialog>`. |
| `styles.css` | All styling: design tokens as CSS custom properties (dark default in `:root`, light overrides via `:root[data-theme="light"]` and `prefers-color-scheme`), Fira Code/Fira Sans import, spacing scale, buttons/forms/KPI tiles/tables/modals, responsive breakpoints at 1024/768/375, `prefers-reduced-motion` handling. |
| `icons.js` | Flat `ICONS` object of ~11 hand-rolled stroke-based SVG strings (gear, close, play, pause, stop, refresh, chevrons, arrows, table, chart, warning). No icon library. |
| `api.js` | `Api` module: `localStorage`-backed token/base-URL getters/setters, a `request()` wrapper where every REST call resolves to `{ok, data, error}` (never throws — network errors and non-2xx are caught), and `connectWs()` for the `/ws/live?token=` WebSocket with defensive JSON parsing. |
| `charts.js` | `Charts` module wrapping TradingView Lightweight Charts: init/render candlesticks+volume, theme-aware colors, and a `renderTable()` fallback that reads the same candle array into a plain `<table>`. |
| `app.js` | State + all DOM wiring: theme, connection status, button busy/disabled states, config/strategies/candles/trades loading, WS message handling with defensive field lookups, generic sortable trades table, settings/kill modals. |

## Chart library: CDN, pinned version

Loaded from `https://unpkg.com/lightweight-charts@4.1.3/dist/lightweight-charts.standalone.production.js` — **CDN, explicitly version-pinned**, not vendored.

Why: no build step means no bundler to vendor a package through cleanly, and the whole page already assumes network access (Google Fonts is CDN too). Plain unpinned `unpkg.com/lightweight-charts/...` resolves to whatever is currently "latest" — **this bit during development**: it resolved to a v5 release that removed `chart.addCandlestickSeries()`/`addHistogramSeries()` in favor of a different `addSeries()` API, which threw inside chart init. Pinning to `4.1.3` (the API surface the task's guidance was written against) fixes that and — more importantly — makes future breakage impossible from an unrelated "latest" bump.

Additionally hardened `initChartPanel()` (`app.js`) with a try/catch: if the chart library is ever missing or incompatible for any other reason (offline, CDN blocked, future pin change), the panel now degrades to the accessible table view instead of throwing and aborting the rest of `init()` — this was a real bug caught during testing (see below) where one chart-init exception was silently taking down every other button/listener on the page.

## Pre-delivery checklist

- No emoji icons — hand-rolled SVG set in `icons.js`. Confirmed.
- `cursor: pointer` / native button affordance on all clickables. Confirmed.
- Hover/transition timing 150–300ms (`--transition: 200ms ease` used throughout). Confirmed.
- Light mode contrast — dark text (`#1E293B`) on white/light panels (`#FFFFFF`/`#F8FAFC`), independently defined tokens (not an inverted dark palette). Visually verified in-browser.
- Visible focus states — global `:focus-visible` rule with a 3px ring plus offset. Confirmed via inspection; native `<dialog>`/button/input focus all use it.
- `prefers-reduced-motion` respected — global media query collapses all animation/transition durations to ~0. Confirmed in CSS.
- Responsive at 375/768/1024/1440 — verified live in-browser at all four widths, zero horizontal scroll at each.
- Touch targets ≥44×44px — buttons/inputs/radios set `min-height: 44px` (radio inputs sized 20px but their `.radio-option` label wrapper is 44px min-height, so the clickable area meets the target).
- Kill switch requires typed "STOP" confirmation, styled destructively, visually separated from Start/Pause. Confirmed working end-to-end (see Testing below).
- No console errors with no backend running. Confirmed after fixing two real bugs found during testing (below).

## Real bugs found and fixed during testing (not just checklist ticking)

1. **CSS Grid blowout at narrow widths.** `.layout` (a CSS Grid) was correctly sized to the viewport, but its grid-item children (`.col-main`, `.col-side`) rendered ~2× wider than their parent and got silently clipped by `overflow-x: hidden` on `<body>` — no scrollbar appeared, but the Reconnect button, candle-limit selector, etc. were literally unreachable below ~700px width. Root cause: CSS Grid items default to `min-width: auto`, so a flex-wrap toolbar's min-content width can force the grid track wider than its column. Fix: `min-width: 0` on `.col-main`, `.col-side`, and `.panel`. Verified at 375px afterward — everything now shrinks correctly, zero overflow.
2. **Unpinned CDN version broke chart init, which silently broke the whole page.** Covered above. This was the more serious one: because `initChartPanel()` had no error handling, the `TypeError` thrown by the v5 API mismatch aborted `init()` before it reached `wireEvents()` — meaning Settings, Start/Pause/Kill, theme toggle, everything was unresponsive, with no visible sign of failure (blank click handlers, no console output visible in casual testing). Fixed by pinning the CDN version and by wrapping chart init in try/catch so a future failure only degrades the chart panel, not the entire app.

## Assumptions / contract notes for the integration step

Checked the actual Go server (`go-engine/internal/api/server.go`) alongside the spec to reduce guesswork:

- `/api/config`, `/api/trades`, `/api/start`, `/api/stop`, `/api/kill` **already exist** in the current server code with these exact field names: `ConfigResponse{strategy, session_balance, stop_loss_enabled, stop_loss_percent, discovery_enabled, indices}`, `TradeRecord{timestamp, symbol, action, quantity, fill_price, net_pnl}`. The UI's generic-table trades renderer and config pre-fill logic were built to match these field names, but still degrade gracefully (generic key/value / generic columns) if the sibling agent's final schema differs.
- `/api/strategies` and `/api/candles` **do not exist yet** in the current server code — built strictly against the spec's documented shapes (`{"strategies":[...]}`, `{"symbol","candles":[...]}` with a 404 `{"error":...}` path). Both paths already fail soft (empty-state/error message) if the response shape doesn't match once the sibling agent lands these.
- **The real `/ws/live` payload today is narrower than the spec examples**: `handleWebSocket`'s heartbeat frame is `{type:"heartbeat", timestamp, running}` and `UpdateStatus()`'s broadcast is `{type:"update", running, balance, unrealized_pnl, realized_pnl, positions}` — note `positions` is a plain count, not `positions_count`. `app.js`'s `handleWsMessage()` was written defensively to accept either spelling (`positions_count`/`positionsCount`/`positions`, `unrealized_pnl`/`unrealizedPnL`/`unrealized`, etc.) via a `firstNumber(...)` helper, so it should handle either the current real shape or the spec's example shape without changes. Worth double-checking once the sibling agent's WS payload is finalized, in case new field names are introduced that don't match any of the guessed spellings.
- Manual-symbol mode sends an extra `symbol` field in the `POST /api/config` body (not in the documented contract) alongside `discovery_enabled`. The current `handleConfigPost` ignores unknown keys safely (decodes into `map[string]interface{}` and only reads `session_balance`/`strategy`), so this is forward-compatible but inert until the sibling agent wires it up server-side.
- Kill-confirmed and pause/resume both close their dialog / reset busy state regardless of request success or failure (only a console warning + a status-line message on failure) — this was a deliberate choice to keep the control flow simple for a solo-trader tool rather than adding retry UI; flag if you want the kill dialog to stay open and let the user retry on a failed kill call.

## Testing performed

Served via `python -m http.server` from `web-ui/` and driven in the Browser pane with no backend running:
- Confirmed clean load with disconnected state everywhere (gray dot, "Disconnected", PAPER badge, "Unavailable" strategy dropdown, "Not connected" trades empty-state) and zero console errors.
- Settings modal: opens, prefills default base URL, theme radio toggle switches `data-theme` live and repaints the chart via `Charts.applyTheme()`; verified light-theme contrast visually.
- Kill switch: confirm button stays disabled until input is exactly `STOP` (tested `sto` → still disabled, `STOP` → enabled), click fires `POST /api/kill`, network failure caught as a `console.warn` (not thrown), modal closes.
- Chart view/table toggle: `aria-pressed` and hidden-class swap both verified.
- Verified zero horizontal scroll at 375/768/1024/1440px viewport widths after the grid-blowout fix.
- Escape-to-close: implemented via both the native `<dialog>` behavior and an explicit `keydown` fallback (belt-and-suspenders, since native Escape-close isn't universally reliable across engines). Automated verification of the physical Escape keypress was inconclusive in this particular sandboxed preview tool — synthetic Escape key events did not appear to reach the page's `keydown` listeners at all in this environment (zero events observed even after ruling out caching and confirming listener registration via a DOM-visible marker), which looks like a limitation of the automation harness rather than the app; mouse-driven close (X button, Cancel button) was verified working. Worth a quick manual keyboard check in a real browser as a final sanity check.
