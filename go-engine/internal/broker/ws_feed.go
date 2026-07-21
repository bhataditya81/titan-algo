package broker

// ---------------------------------------------------------------------------
// Angel One SmartAPI WebSocket 2.0 market feed (G-7).
//
// ADDITIVE, not a replacement: REST polling (Subscribe / FetchMarketDataBatch
// in angel_broker.go) is completely untouched by this file. If the WS feed
// never connects, or is mid-backoff after a drop, GetCurrentPrice /
// GetCurrentPriceWithAge keep working exactly as they do today because
// nothing here changes their code path. Every WS tick updates the SAME
// a.marketPrices / a.priceUpdated maps that REST polling updates, via
// AngelBroker.applyTick (angel_broker.go) — so GetCurrentPriceWithAge
// reflects a live tick's age just like a REST refresh (WP-1's staleness
// mechanism, CR-14/EX-6).
//
// VERIFICATION FLAG (see R2-1-REPORT.md for the full writeup): the WS URL,
// auth headers, and JSON subscribe/unsubscribe message shape below match
// public SmartAPI WebSocket 2.0 documentation (confirmed via two independent
// lookups during this work package). The BINARY TICK PAYLOAD's byte offsets
// are reconstructed from the well-known public reference-client packet
// layout (LTP is a byte-prefix of Quote, which is a byte-prefix of
// SnapQuote) and have NOT been verified against a live connection. Tick
// parsing is defensive: a too-short or unrecognized packet is logged and
// skipped, never crashes the feed or corrupts the price cache. Verify the
// offsets against a real WS2 session before trusting parsed tick prices in
// production.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// angelWSURL is a var (not const) so tests can point it at a local server.
var angelWSURL = "wss://smartapisocket.angelone.in/smart-stream"

// Subscription modes and actions (SmartAPI WS2 JSON control messages).
const (
	wsModeLTP   = 1
	wsModeQuote = 2 // LTP + volume + OHLC; used for SubscribeLive (needs volume)

	wsActionUnsubscribe = 0
	wsActionSubscribe   = 1
)

// wsExchangeType maps an instrument's exchange segment to the WS2
// exchangeType code.
func wsExchangeType(exchSeg string) (int, error) {
	switch strings.ToUpper(strings.TrimSpace(exchSeg)) {
	case "NSE":
		return 1, nil
	case "NFO":
		return 2, nil
	case "BSE":
		return 3, nil
	case "BFO":
		return 4, nil
	case "MCX":
		return 5, nil
	case "NCDEX":
		return 7, nil
	case "CDS":
		return 13, nil
	default:
		return 0, fmt.Errorf("unknown exchange segment %q for WS subscription", exchSeg)
	}
}

type wsTokenGroup struct {
	ExchangeType int      `json:"exchangeType"`
	Tokens       []string `json:"tokens"`
}

type wsSubscribeParams struct {
	Mode      int            `json:"mode"`
	TokenList []wsTokenGroup `json:"tokenList"`
}

type wsSubscribeRequest struct {
	CorrelationID string            `json:"correlationID"`
	Action        int               `json:"action"`
	Params        wsSubscribeParams `json:"params"`
}

// Reconnect / heartbeat tuning — vars (not consts) so tests can shrink them,
// same pattern as fillPollAttempts et al. in angel_broker.go.
var (
	wsReconnectInitialDelay = 1 * time.Second
	wsReconnectMaxDelay     = 30 * time.Second
	wsHeartbeatInterval     = 10 * time.Second
	wsHandshakeTimeout      = 15 * time.Second

	// wsReadTimeout (F3): read deadline refreshed on every successful read.
	// A silently-stalled-but-open connection (TCP alive, peer gone quiet)
	// stops satisfying reads, ReadMessage returns a timeout error, and the
	// existing run()/connectAndServe() reconnect loop takes over — no new
	// liveness mechanism needed. 3x the heartbeat interval gives the server
	// two missed ping/pong round-trips of slack before we give up on it.
	wsReadTimeout = 3 * wsHeartbeatInterval
)

// Binary tick layout (see verification flag above). Mode-1 (LTP) packets are
// tickMinLenLTP bytes; mode-2/3 packets are longer, strictly prefixed by the
// same first tickMinLenLTP bytes.
const (
	tickOffMode      = 0
	tickOffExchange  = 1
	tickOffToken     = 2
	tickTokenLen     = 25
	tickOffSeq       = 27
	tickOffTimestamp = 35
	tickOffLTP       = 43
	tickMinLenLTP    = tickOffLTP + 8 // 51

	tickOffVolume   = 67
	tickMinLenQuote = tickOffVolume + 8 // 75
)

// wsSub is one desired live subscription: resolved WS identity for a symbol.
type wsSub struct {
	exchangeType int
	token        string
}

// wsFeed manages the WebSocket 2.0 connection lifecycle for one AngelBroker.
//
// Lock discipline: mu guards ONLY in-memory state (desired symbol set,
// current *websocket.Conn pointer, current write channel, start/cancel).
// It is never held across a dial, a WriteMessage/ReadMessage call, or a
// sleep — writes to the connection are handed off via writeCh to a single
// per-connection writer goroutine so no lock is ever held across network I/O.
type wsFeed struct {
	broker *AngelBroker

	mu      sync.Mutex
	symbols map[string]wsSub // broker symbol -> resolved WS identity
	conn    *websocket.Conn
	writeCh chan []byte // outbound frames for the CURRENT connection only
	cancel  context.CancelFunc
	started bool
	done    chan struct{} // closed when run() returns; stop() waits on this
}

func newWSFeed(a *AngelBroker) *wsFeed {
	return &wsFeed{broker: a, symbols: make(map[string]wsSub)}
}

// ---------------------------------------------------------------------------
// Public API (ExtendedTradeService / interface addition per R2-1)
// ---------------------------------------------------------------------------

// SubscribeLive starts (if not already running) the WS live feed and adds
// symbols to its desired subscription set. Additive: REST polling is
// unaffected and keeps serving prices regardless of whether the feed ever
// connects (G-7 fallback requirement). Returns an error only if NONE of the
// supplied symbols could be resolved via the instrument master; a mix of
// resolvable/unresolvable symbols proceeds with the resolvable subset and
// logs a warning for the rest.
func (a *AngelBroker) SubscribeLive(symbols []string) error {
	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()
	if !connected {
		return fmt.Errorf("not connected to broker")
	}
	if len(symbols) == 0 {
		return fmt.Errorf("no symbols supplied")
	}

	a.mu.Lock()
	if a.liveFeed == nil {
		a.liveFeed = newWSFeed(a)
	}
	feed := a.liveFeed
	a.mu.Unlock()

	added, err := feed.addSymbols(symbols)
	if err != nil {
		return err
	}

	feed.ensureStarted()
	// Best-effort: if already connected, subscribe the new symbols right now
	// instead of waiting for the next reconnect cycle to resend the full set.
	feed.sendSubscribeIfConnected(added)
	return nil
}

// UnsubscribeLive stops streaming the given symbols. No-op if the feed was
// never started.
func (a *AngelBroker) UnsubscribeLive(symbols []string) error {
	a.mu.RLock()
	feed := a.liveFeed
	a.mu.RUnlock()
	if feed == nil {
		return nil
	}

	var removed []wsSub
	feed.mu.Lock()
	for _, sym := range symbols {
		if sub, ok := feed.symbols[sym]; ok {
			removed = append(removed, sub)
			delete(feed.symbols, sym)
		}
	}
	ch := feed.writeCh
	feed.mu.Unlock()

	if len(removed) == 0 || ch == nil {
		return nil // not connected right now; nothing to unsubscribe on the wire
	}

	payload, err := buildSubscribePayload(wsActionUnsubscribe, removed)
	if err != nil {
		return fmt.Errorf("UnsubscribeLive: %w", err)
	}
	select {
	case ch <- payload:
	default:
		log.Printf("⚠️ WS live feed: unsubscribe queue full, dropping (best effort)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// wsFeed internals
// ---------------------------------------------------------------------------

func (f *wsFeed) addSymbols(symbols []string) ([]wsSub, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var added []wsSub
	var errs []string
	for _, sym := range symbols {
		inst, err := f.broker.instruments.GetInstrument(sym)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", sym, err))
			continue
		}
		exType, err := wsExchangeType(inst.ExchSeg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", sym, err))
			continue
		}
		sub := wsSub{exchangeType: exType, token: inst.Token}
		f.symbols[sym] = sub
		added = append(added, sub)
	}

	if len(added) == 0 {
		return nil, fmt.Errorf("SubscribeLive: could not resolve any symbol: %s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		log.Printf("⚠️ SubscribeLive: %d symbol(s) unresolved (proceeding with the rest): %s", len(errs), strings.Join(errs, "; "))
	}
	return added, nil
}

func (f *wsFeed) ensureStarted() {
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return
	}
	f.started = true
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	done := make(chan struct{})
	f.done = done
	f.mu.Unlock()

	go func() {
		defer close(done)
		f.run(ctx)
	}()
}

// stop shuts the feed down and waits for its goroutine to fully exit (called
// from AngelBroker.Close()). Cancel itself is non-blocking; the wait on
// f.done is bounded because run()/connectAndServe() all select on ctx.Done()
// at every blocking point (dial, read-loop teardown, backoff sleep).
func (f *wsFeed) stop() {
	f.mu.Lock()
	cancel := f.cancel
	done := f.done
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (f *wsFeed) sendSubscribeIfConnected(subs []wsSub) {
	if len(subs) == 0 {
		return
	}
	f.mu.Lock()
	ch := f.writeCh
	f.mu.Unlock()
	if ch == nil {
		return // not connected yet; the next connect will subscribe the full desired set
	}

	payload, err := buildSubscribePayload(wsActionSubscribe, subs)
	if err != nil {
		log.Printf("⚠️ WS live feed: failed to build subscribe payload: %v", err)
		return
	}
	select {
	case ch <- payload:
	default:
		log.Printf("⚠️ WS live feed: subscribe queue full, will retry on next reconnect")
	}
}

func (f *wsFeed) snapshotSubsLocked() []wsSub {
	out := make([]wsSub, 0, len(f.symbols))
	for _, s := range f.symbols {
		out = append(out, s)
	}
	return out
}

func buildSubscribePayload(action int, subs []wsSub) ([]byte, error) {
	byExch := make(map[int][]string)
	for _, s := range subs {
		byExch[s.exchangeType] = append(byExch[s.exchangeType], s.token)
	}
	groups := make([]wsTokenGroup, 0, len(byExch))
	for ex, tokens := range byExch {
		groups = append(groups, wsTokenGroup{ExchangeType: ex, Tokens: tokens})
	}
	req := wsSubscribeRequest{
		CorrelationID: fmt.Sprintf("titan-%d", time.Now().UnixNano()),
		Action:        action,
		Params:        wsSubscribeParams{Mode: wsModeQuote, TokenList: groups},
	}
	return json.Marshal(req)
}

// run is the reconnect loop: connect, serve until disconnect/error, back off
// exponentially, repeat, until ctx is cancelled (AngelBroker.Close()).
func (f *wsFeed) run(ctx context.Context) {
	delay := wsReconnectInitialDelay
	for {
		if ctx.Err() != nil {
			return
		}

		start := time.Now()
		err := f.connectAndServe(ctx)
		if ctx.Err() != nil {
			return
		}

		// ponytail: reset backoff if the connection stayed up a while before
		// dropping, so a brief live blip doesn't inherit a long-dead-feed
		// delay. A jittered/smarter backoff curve is the upgrade path if this
		// heuristic proves too coarse in practice.
		if time.Since(start) > wsReconnectInitialDelay*4 {
			delay = wsReconnectInitialDelay
		}

		log.Printf("⚠️ WS live feed disconnected, reconnecting in %s: %v", delay, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > wsReconnectMaxDelay {
			delay = wsReconnectMaxDelay
		}
	}
}

// connectAndServe dials once, subscribes the desired symbol set, then blocks
// reading frames until the connection errors/closes. Returns the error that
// ended the connection (never nil on return, except via ctx cancellation).
func (f *wsFeed) connectAndServe(ctx context.Context) error {
	a := f.broker
	a.mu.RLock()
	apiKey := a.apiKey
	clientCode := a.clientCode
	accessToken := a.accessToken
	feedToken := a.feedToken
	a.mu.RUnlock()

	if accessToken == "" || feedToken == "" {
		return fmt.Errorf("WS live feed: no session token yet (login not complete)")
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+accessToken)
	header.Set("x-api-key", apiKey)
	header.Set("x-client-code", clientCode)
	header.Set("x-feed-token", feedToken)

	dialer := websocket.Dialer{HandshakeTimeout: wsHandshakeTimeout}
	conn, _, err := dialer.DialContext(ctx, angelWSURL, header)
	if err != nil {
		return fmt.Errorf("WS dial failed: %w", err)
	}
	defer conn.Close()

	writeCh := make(chan []byte, 16)
	f.mu.Lock()
	f.conn = conn
	f.writeCh = writeCh
	desired := f.snapshotSubsLocked()
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		if f.conn == conn {
			f.conn = nil
			f.writeCh = nil
		}
		f.mu.Unlock()
	}()

	connCtx, connCancel := context.WithCancel(ctx)

	// conn.ReadMessage() below blocks on the raw socket and does not itself
	// observe ctx cancellation. Watch connCtx and force-close the connection
	// when it fires (either the outer ctx was cancelled via stop(), or this
	// function is returning for its own reasons) so the read loop — and
	// therefore this whole function, and therefore wsFeed.stop()'s wait on
	// f.done — actually unblocks instead of hanging until the peer closes.
	closerDone := make(chan struct{})
	go func() {
		defer close(closerDone)
		<-connCtx.Done()
		_ = conn.Close()
	}()
	defer func() { connCancel(); <-closerDone }()

	// Single writer goroutine per connection: the ONLY goroutine that calls
	// conn.WriteMessage, so concurrent writers (SubscribeLive, heartbeat)
	// never race on the wire. No mutex is held while it blocks on I/O.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case msg, ok := <-writeCh:
				if !ok {
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					return
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	if len(desired) > 0 {
		if payload, perr := buildSubscribePayload(wsActionSubscribe, desired); perr == nil {
			select {
			case writeCh <- payload:
			default:
			}
		}
	}

	// Client-initiated heartbeat: send "ping" periodically per the protocol.
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(wsHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case writeCh <- []byte("ping"):
				default:
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	log.Printf("🔌 WS live feed connected (%d symbols subscribed)", len(desired))

	// F3 fix: without a read deadline, a connection whose peer has gone
	// silent (TCP still up, no more data ever arrives) never unblocks
	// ReadMessage — the reconnect logic below never runs, and this feed
	// silently stops delivering ticks forever. Refreshed on every
	// successful read so an active connection never times out.
	_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))

	var readErr error
	for {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		switch msgType {
		case websocket.BinaryMessage:
			f.handleTick(msg)
		case websocket.TextMessage:
			f.handleText(msg, writeCh)
		}
	}

	connCancel()
	<-writerDone
	<-heartbeatDone
	return fmt.Errorf("WS connection closed: %w", readErr)
}

// handleText responds to the server's app-level heartbeat/ping per the
// protocol (a server-sent "ping" gets a "pong" reply); anything else
// (including our own "pong" being echoed, or an unrecognized control string)
// is ignored — never fatal.
func (f *wsFeed) handleText(msg []byte, writeCh chan []byte) {
	if strings.TrimSpace(string(msg)) == "ping" {
		select {
		case writeCh <- []byte("pong"):
		default:
		}
	}
}

// handleTick parses one binary market-data frame and, on success, updates
// the shared price cache via AngelBroker.applyTick. Malformed or
// unrecognized packets are logged and skipped — never fatal to the feed.
func (f *wsFeed) handleTick(data []byte) {
	if len(data) < tickMinLenLTP {
		log.Printf("⚠️ WS live feed: tick packet too short (%d bytes), skipping", len(data))
		return
	}

	tokenRaw := data[tickOffToken : tickOffToken+tickTokenLen]
	token := strings.TrimRight(string(tokenRaw), "\x00")
	if token == "" {
		return
	}

	ltpFixed := int64(binary.LittleEndian.Uint64(data[tickOffLTP : tickOffLTP+8]))
	ltp := float64(ltpFixed) / 100.0 // Angel WS2 prices are fixed-point paise
	if ltp <= 0 {
		return
	}

	var volume float64
	hasVolume := false
	if len(data) >= tickMinLenQuote {
		volFixed := int64(binary.LittleEndian.Uint64(data[tickOffVolume : tickOffVolume+8]))
		if volFixed > 0 {
			volume = float64(volFixed)
			hasVolume = true
		}
	}

	symbol, err := f.broker.instruments.GetSymbol(token)
	if err != nil {
		// Token isn't in our loaded instrument master; nothing to key the
		// price cache by. Not an error worth logging on every tick.
		return
	}
	f.broker.applyTick(symbol, ltp, volume, hasVolume)
}
