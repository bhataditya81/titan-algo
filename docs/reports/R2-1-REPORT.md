# R2-1 — Broker: Margin API + WebSocket Market Feed — Report

Scope: `internal/broker/angel_broker.go`, `internal/broker/broker.go`, `internal/broker/ws_feed.go` (new) only. Companion test files `angel_broker_test.go` (extended, pre-existing) and `ws_feed_test.go` (new) — same convention WP-1 used for its own test files.

Gaps closed: **G-1** (margin API) and **G-7** (WebSocket market feed).

---

## 1. G-1 — Margin API

### New exported API

```go
// broker.go
type MarginOrderInput struct {
	Symbol          string
	Token           string  // optional; resolved from Symbol via the instrument master if empty
	Exchange        string  // optional; resolved from Symbol via the instrument master if empty
	TransactionType OrderSide
	Quantity        int
	ProductType     string  // optional; defaults to CARRYFORWARD (NFO/MCX) or INTRADAY otherwise
	Price           float64 // 0 for market-priced legs
}

// angel_broker.go
func (a *AngelBroker) GetRequiredMargin(orders []MarginOrderInput) (float64, error)
```

Added to `ExtendedTradeService` in `broker.go` alongside `SubscribeLive`/`UnsubscribeLive` (see §2).

### Behavior

- Every leg in `orders` is sent in **one** POST to `/rest/secure/angelbroking/margin/v1/batch` (A-6) — this is what lets iron_fly's 4 legs get Angel's hedged-margin benefit instead of being priced leg-by-leg (which would over-state the true requirement).
- Fail-closed, no exceptions:
  - Empty basket → error.
  - >50 legs (Angel's documented per-request cap) → error.
  - Any leg with non-positive quantity, an invalid `TransactionType`, or an unresolvable symbol/token/exchange → error, whole call fails (no partial guess).
  - Not connected → error.
  - HTTP transport error, non-200, WAF block, or auth failure → error (all funneled through the existing `doAPIRequest`, so 401-refresh-and-retry, WAF detection, and HTTP-status checking are inherited for free — same choke point every other Angel call in this file uses).
  - Malformed JSON → error.
  - `status:false` in the envelope → error (message + errorcode surfaced).
  - `"data":null` (Angel's own documented error-response shape) → error, distinguished from a present `data` via a pointer field (`*struct{...}`).
  - `data.totalMarginRequired <= 0` → error ("ambiguous margin response"), since a real multi-leg derivatives basket never has zero-or-negative margin — never returned as if it were a valid answer.
- On success, returns `data.totalMarginRequired` and nothing else is guessed.

### Endpoint/schema verification — FLAG

Per the task's explicit instruction, I do **not** have access to a live SmartAPI account or the paywalled/rendered docs pages directly. I used two independent web searches (a broad search-engine query, and a targeted fetch of the SmartAPI forum thread titled "Calculate Margin Requirements with SmartAPI's New Margin Calculator API") that **agreed** on:

- Endpoint: `POST https://apiconnect.angelbroking.com/rest/secure/angelbroking/margin/v1/batch`, up to 50 positions/request, 10 req/s rate limit.
- Request: `{"positions":[{"exchange","qty","price","productType","token","tradeType"}]}`.
- Response: the standard `{status, message, errorcode, data}` envelope; `data.totalMarginRequired`, with a documented error example of `data:null` + `errorcode:"AB4022"` ("Null or Empty Margin Data").

I implemented exactly against this shape and match Appendix A's `angelbroking.com` host convention already used elsewhere in the file. **This has not been exercised against a live sandbox call.** Before trusting this in production:
1. Fire one real `GetRequiredMargin` call against a live (rotated) account with a known simple basket and diff the response against `data.marginComponents`/`data.marginBreakup` (fields the forum thread also documented but this implementation doesn't currently read — see "Not done" below) to confirm `totalMarginRequired` is the right top-level number to key off.
2. Confirm `tradeType` really wants `"BUY"`/`"SELL"` (matches `OrderSide` 1:1 in this codebase, convenient if true) rather than a numeric code.
3. Confirm `productType` string values accepted (`CARRYFORWARD`/`INTRADAY`) match this broker's existing `PlaceOrder` convention, which I reused rather than inventing a new one.

### Tests (`angel_broker_test.go`)

All via `httptest`, following the existing `newTestBroker` harness:

| Test | Proves |
|---|---|
| `TestGetRequiredMargin_HappyPath_MultiLegBasket` | 4-leg iron_fly-shaped basket sent as ONE request (server asserts `len(positions)==4`); returns the server's `totalMarginRequired`. |
| `TestGetRequiredMargin_HTTPError_FailsClosed` | HTTP 500 → error, not a guess. |
| `TestGetRequiredMargin_MalformedResponse_FailsClosed` | Non-JSON 200 body → error. |
| `TestGetRequiredMargin_NullData_FailsClosed` | Angel's own documented `data:null` error example → error. |
| `TestGetRequiredMargin_NonPositiveMargin_FailsClosed` | `totalMarginRequired:0` → error (ambiguous, rejected). |
| `TestGetRequiredMargin_NoOrders_ReturnsError` | Empty basket → error. |
| `TestGetRequiredMargin_UnresolvableSymbol_ReturnsError` | Unknown symbol, no token/exchange override → error. |

---

## 2. G-7 — WebSocket Market Feed (`ws_feed.go`, new)

### New exported API

```go
// Added to ExtendedTradeService (broker.go); implemented on *AngelBroker (ws_feed.go).
SubscribeLive(symbols []string) error
UnsubscribeLive(symbols []string) error
```

**Design choice:** exposed via `ExtendedTradeService` (not a new standalone interface) — same decision WP-1 made for `PlaceStopLossOrder`/`CancelOrder`/etc. R2-INT should type-assert the same way: `if ext, ok := svc.(broker.ExtendedTradeService); ok { ext.SubscribeLive(symbols) }`. `MockBroker`/`LivePaperBroker` don't implement `ExtendedTradeService` (confirmed unchanged from WP-1) so this addition doesn't break their compilation — every call site in `internal/engine` already reaches broker methods through a type-assertion (`.(broker.ExtendedTradeService)`), never a direct interface-satisfaction requirement, so this is a safe additive change (verified: `go build ./...` and `go vet ./...` are clean across the whole repo after this change).

### Behavior

- `SubscribeLive` resolves each symbol to a WS `(exchangeType, token)` pair via the existing `InstrumentManager`, lazily creates one `wsFeed` per `AngelBroker`, and starts one long-lived reconnect-loop goroutine the first time it's called. Calling it again just adds symbols to the desired set (and pushes an incremental subscribe if already connected).
- `UnsubscribeLive` removes symbols from the desired set and — if currently connected — sends an unsubscribe message. No-op (`nil` error) if the feed was never started.
- **Additive/fallback (the actual G-7 requirement):** nothing in `ws_feed.go` touches the existing REST path (`Subscribe`, `FetchMarketDataBatch`, `GetCurrentPrice`, `GetCurrentPriceWithAge`). If the feed never connects, or is mid-backoff after a drop, REST polling keeps serving prices exactly as it does today — proven by `TestWSFeed_NeverConnects_RESTFallbackStillWorks`.
- **Same staleness path as WP-1:** every tick goes through a new `AngelBroker.applyTick(symbol, ltp, volume, hasVolume)` (in `angel_broker.go`) that writes into the SAME `a.marketPrices`/`a.priceUpdated` maps `FetchMarketDataBatch` writes into. `GetCurrentPriceWithAge` therefore reports a fresh age immediately after a WS tick, with no separate code path — proven by `TestSubscribeLive_TickUpdatesPriceAge`.
- **Reconnect with exponential backoff:** `wsReconnectInitialDelay` (1s) doubling to `wsReconnectMaxDelay` (30s cap); resets to initial if a connection stayed up more than 4x the initial delay before dropping (ponytail-flagged heuristic — a jittered/smarter curve is the upgrade path if this proves too coarse). Proven by `TestWSFeed_ForcedDisconnect_Reconnects`.
- **Heartbeat:** client sends a text `"ping"` every `wsHeartbeatInterval` (10s); if the server sends a text `"ping"`, the client replies `"pong"` (handles the protocol's "simplified heartbeat message and response" in both directions).
- **Clean shutdown:** `AngelBroker.Close()` now calls `wsFeed.stop()`, which cancels the feed's context AND **waits** for the feed goroutine to fully exit (via a `done` channel) before returning — this was a real bug caught by `-race` during development: cancelling the context alone doesn't unblock a goroutine parked in `conn.ReadMessage()`, so a dedicated watcher goroutine force-closes the connection on context-cancellation to unblock the read loop, and `stop()` blocks on that goroutine's exit too. Without this, `Close()` would return while the feed goroutine was still touching shared state.

### Lock hygiene (grep-verified, matching WP-1's discipline)

`a.mu` (the broker's central state mutex) is taken and released immediately around each state snapshot/write in `ws_feed.go` — never across `dialer.DialContext`, `conn.ReadMessage`, or `conn.WriteMessage`:

```
$ awk '/func \(a \*AngelBroker\) GetRequiredMargin/,/^}/' internal/broker/angel_broker.go | grep -n "a.mu\|doAPIRequest"
9:  a.mu.RLock()
11: a.mu.RUnlock()
66: body, err := a.doAPIRequest(...)   # network call — no lock held

$ grep -n "a\.mu\|f\.mu" internal/broker/ws_feed.go   # every Lock has a paired Unlock before any DialContext/ReadMessage/WriteMessage line
$ grep -n "DialContext\|ReadMessage\|WriteMessage" internal/broker/ws_feed.go
405: conn, _, err := dialer.DialContext(...)   # no lock held (last unlock at line 392)
455: conn.WriteMessage(...)                    # called only from the dedicated writer goroutine, no lock held
496: msgType, msg, err := conn.ReadMessage()    # no lock held
```

All actual network writes are funneled through a single per-connection writer goroutine reading off a channel (`writeCh`) — `SubscribeLive`/`UnsubscribeLive`/the heartbeat ticker never call `conn.WriteMessage` directly and never hold a lock across the send (channel sends are non-blocking `select`/`default`). This avoids needing a mutex around the connection write path at all, which is stronger than "lock released before I/O" — there simply is no lock in that path.

`wsFeed.mu` (a feed-local lock, separate from `a.mu`) guards only the desired-symbol map and the current `*websocket.Conn`/`writeCh` pointers — same pattern, never held across I/O.

### Endpoint/schema verification — FLAG (important)

Confirmed via two independent web lookups during this work package:
- WS URL: `wss://smartapisocket.angelone.in/smart-stream`.
- Auth: `Authorization: Bearer <JWT>`, `x-api-key`, `x-client-code`, `x-feed-token` headers (all four values already exist on `*AngelBroker` post-login: `accessToken`/`apiKey`/`clientCode`/`feedToken`).
- Control-plane JSON: `{"correlationID","action","params":{"mode","tokenList":[{"exchangeType","tokens"}]}}`, `action` 0/1 = unsubscribe/subscribe.
- "Simplified heartbeat message and response" (confirmed the existence of an app-level ping/pong, not just RFC6455 control frames).

**NOT independently verified — the binary tick payload's byte offsets** (`tickOffMode/Exchange/Token/Seq/Timestamp/LTP/Volume` in `ws_feed.go`). These are reconstructed from the well-known structure of Angel's public reference WebSocket client (LTP-mode packets are documented as a byte-prefix of Quote-mode packets, which are a byte-prefix of SnapQuote): mode(1) + exchangeType(1) + token(25, null-padded ASCII) + sequence(int64 LE) + exchangeTimestamp(int64 LE) + LTP(int64 LE, fixed-point paise ÷100) as the 51-byte LTP-mode prefix, then lastTradedQty/avgTradedPrice/volume as int64 LE fields appended for Quote mode (volume read at offset 67). **I could not fetch the raw byte-format table from the actual docs page** (the doc site is JS-rendered; `WebFetch` returned only a page title, not the underlying spec table) **and did not have a live WS2 session to capture and diff against. Do not trust parsed tick prices in production until this is verified against a real connection** — capture one real tick frame from a rotated-credential session and diff its bytes against these offsets before wiring `SubscribeLive` into the live trading loop.

Defensive design given this uncertainty: `handleTick` bounds-checks packet length before indexing, skips (logs, doesn't crash) anything shorter than the LTP-mode minimum, and treats volume as optional (best-effort, only read if the packet is long enough) — a wrong offset degrades to "missed ticks, logged" rather than a panic or a corrupted price.

### Tests (`ws_feed_test.go`, new)

Uses a local `httptest.Server` + `gorilla/websocket`'s `Upgrader` as the fake WS server (per task instruction), with `angelWSURL` swapped to point at it (same override-a-package-var pattern `instrumentURL` already uses for the instrument-master tests).

| Test | Proves |
|---|---|
| `TestSubscribeLive_TickUpdatesPriceAge` | Fake server receives a well-formed subscribe JSON (asserts action/mode/token list), sends one binary tick; `GetCurrentPriceWithAge` reflects the new price with a sub-second age. |
| `TestWSFeed_ForcedDisconnect_Reconnects` | Fake server closes the first connection immediately; asserts a second connection attempt happens (reconnect-with-backoff working). |
| `TestWSFeed_NeverConnects_RESTFallbackStillWorks` | WS URL points at a plain HTTP 404 server (handshake always fails); feed retries in the background (visible in logs) while `FetchMarketDataBatch` against a separate REST fake server still returns prices normally — proves WS is additive, not a hard dependency. |
| `TestUnsubscribeLive_NeverStarted_NoOp` | Calling `UnsubscribeLive` before any `SubscribeLive` is a safe no-op. |
| `TestSubscribeLive_NotConnected_ReturnsError` | Fails closed if the broker isn't connected yet. |

---

## 3. Files changed

- `internal/broker/broker.go` — added `MarginOrderInput`; added `GetRequiredMargin`/`SubscribeLive`/`UnsubscribeLive` to `ExtendedTradeService`.
- `internal/broker/angel_broker.go` — added `angelMarginPath` (A-6) + request/response structs + `GetRequiredMargin`; added `applyTick` (shared staleness-cache writer for WS ticks); replaced the unused `wsConn *websocket.Conn` field with `liveFeed *wsFeed`; `Close()` now stops the live feed and waits for it to exit; dropped the now-unused `gorilla/websocket` import (the only prior use was the dead `wsConn` field).
- `internal/broker/ws_feed.go` — new. WS 2.0 feed: connect/auth, subscribe/unsubscribe control messages, binary tick parsing, heartbeat, exponential-backoff reconnect, `SubscribeLive`/`UnsubscribeLive`.
- `internal/broker/angel_broker_test.go` — extended (existing file): added margin tests (§1); added `encoding/json`/`io` imports.
- `internal/broker/ws_feed_test.go` — new: WS feed tests (§2).

No new dependencies — used the already-permitted `gorilla/websocket` and `golang.org/x/time/rate` was not additionally needed for the WS feed (Angel's own rate limit is on the margin endpoint's HTTP path, already covered by `doAPIRequest`'s existing choke point; the WS feed's own pacing is the heartbeat/backoff timers, not a token bucket).

## 4. Test evidence

```
$ cd go-engine && go build ./internal/broker/... && go vet ./internal/broker/...
(clean, no output)

$ go clean -testcache && go test -race ./internal/broker/...
ok  	titan-algo/internal/broker	20.125s

$ go build ./... && go vet ./...
(clean, no output — whole repo currently builds/vets clean, no conflicts with in-flight parallel R2 packages at time of writing)
```

Re-ran the full `internal/broker` suite 3x non-cached to rule out flakiness introduced by the new WS tests' timing — consistently green. (One unrelated one-off flake was observed in `mock.go`'s `TestPlaceOrder_SpreadPricing_EquityUsesSmallerHalfSpread`, a file outside this package's ownership — R2-4's file — during the same session; it passed on every other run including the final non-cached run. Not touched, not caused by anything in `ws_feed.go`/`angel_broker.go`/`broker.go` — no shared global state between that test and my new code.)

## 5. For R2-INT (wiring)

- Margin: call `ext.GetRequiredMargin([]broker.MarginOrderInput{...})` from the SELL-entry path before `risk.ValidateOrderWithMargin`; treat any error as "unknown margin" (reject the entry, alert) — this satisfies fail-closed end to end.
- WS feed: call `ext.SubscribeLive(symbols)` once at startup/on new-symbol-discovery. No further wiring is required for `GetCurrentPrice(WithAge)` to benefit — it already reads the same cache the feed writes into. To prove the "software SL loop gets faster" benefit end-to-end, R2-INT's E2E check should compare price age before/after calling `SubscribeLive` against a real feed.
- `AngelBroker.Close()` now blocks briefly (until the WS goroutine exits) — this is intentional and bounded (see §2 "Clean shutdown"), but flagging it in case any shutdown-path timeout assumption elsewhere is tight.

## 6. Known limitations / explicitly out of scope

- Margin response's `marginComponents`/`marginBreakup`/`optionsBuy` fields (present per the forum thread) are not parsed/exposed — only `totalMarginRequired` is used, per the task's minimal ask. If R2-7/strategy work wants per-leg margin breakdown for reporting, that's an additive follow-up to the same struct.
- WS binary tick byte offsets are the one piece of this work package that needs live verification before production trust (see the FLAG in §2) — everything else (endpoint, headers, control-message JSON, margin endpoint/schema) was corroborated by two independent sources.
- No new persistent "is the WS feed currently connected" accessor was added (not asked for); `SubscribeLive` returning `nil` only means symbol resolution succeeded, not that the socket is up yet — by design, since G-7 explicitly wants the caller to not have to care (REST fallback is automatic).
