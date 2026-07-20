package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"titan-algo/internal/broker"
	"titan-algo/internal/risk"
	"titan-algo/internal/strategy"
	"titan-algo/models"

	"gopkg.in/yaml.v2"
)

// AlertFunc sends an operational alert (e.g. Telegram). No-op when nil.
// cmd/main.go wires this to a Telegram-bot-API poster (TITAN_TG_TOKEN /
// TITAN_TG_CHAT); tests and mobile can pass nil.
type AlertFunc func(event, detail string)

// RunnerConfig configures the single, shared tick loop (WP-9 task 1: exactly
// one engine construction, one loop implementation — used by both
// cmd/main.go and internal/app/titan.go).
type RunnerConfig struct {
	Symbols              []string
	StrategyName         string
	HistorySize          int
	MinDataPoints        int
	PollInterval         time.Duration
	HeartbeatInterval    time.Duration
	DefaultQuantity      int
	LotSizeFallback      int            // last-resort fallback; instrument master is tried first (task 9)
	LotSizes             map[string]int // config-provided per-underlying override, tried before the fallback
	IndexSymbol          string
	OptionExpiryOverride string // optional manual override; instrument master is tried first
	StrikeStep           int
	ModeLabel            string // "PAPER" or "LIVE"
	KillSentinelPath     string // e.g. data/KILL
	HeartbeatFilePath    string // e.g. data/heartbeat
	SquareOffHour        int    // default 15
	SquareOffMinute      int    // default 20
	MarketOpenBufferMin  int    // minutes after 09:15 IST before entries are allowed
	StaleAge             time.Duration
	Alert                AlertFunc
	Instruments          *broker.InstrumentManager
	HolidayFile          string // e.g. "nse_holidays.yaml" (R2-5/G-12); "" = use the built-in hardcoded table
}

func (c *RunnerConfig) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
	if c.HistorySize <= 0 {
		c.HistorySize = 100
	}
	if c.MinDataPoints <= 0 {
		c.MinDataPoints = 20
	}
	if c.SquareOffHour == 0 && c.SquareOffMinute == 0 {
		c.SquareOffHour, c.SquareOffMinute = 15, 20
	}
	if c.StaleAge <= 0 {
		c.StaleAge = 15 * time.Second
	}
	// No auto-default here: LotSizeFallback must only ever hold a value the
	// operator explicitly configured (options_config.lot_size in
	// config.yaml). Silently substituting a guessed constant (e.g. 75) here
	// masked a stale/missing config the same way as any other silent
	// fallback and is exactly what the fail-closed policy forbids — see
	// lotSize() below, which now errors instead of using an unset value.
	if c.DefaultQuantity <= 0 {
		c.DefaultQuantity = 1
	}
}

// legRecord tracks one actually-traded leg of an open structure.
type legRecord struct {
	Symbol     string
	Side       broker.OrderSide
	PositionID string // state store position id, for closing later
	SLOrderID  string // broker-side resting stop order, if placed
}

// openStructure tracks one signal-symbol's currently open position (single
// leg for directional entries, multiple legs for straddle/fly structures).
type openStructure struct {
	Legs           []legRecord
	EntryPremium   float64
	EntryTime      time.Time
	PremiumHistory []float64 // combined-premium series fed into EvalContext.Prices while open (issue 5)
	MultiLeg       bool
}

// entryConfirmer / exitConfirmer / snapshotter are optional interfaces a
// Strategy may implement (currently only NineTwentyStrategy) so the runner
// can flip fill-confirmed state and persist/restore it (WP-6 ST-4 contract).
type entryConfirmer interface{ ConfirmEntry(symbol string) }
type exitConfirmer interface{ ConfirmExit(symbol string) }
type snapshotter interface {
	Snapshot() map[string]string
	Restore(map[string]string)
}

// RunnerStatus is a transport-agnostic snapshot of engine state (cmd/main.go
// converts this to api.EngineStatus when wiring ControlHooks — engine
// deliberately does not import internal/api).
type RunnerStatus struct {
	Running         bool
	Mode            string
	Strategy        string
	Balance         float64
	UnrealizedPnL   float64
	RealizedPnL     float64
	PositionsCount  int
	StopLossEnabled bool
	StopLossPercent float64
}

// Runner drives the tick loop: market-hours gating, strategy evaluation,
// order flow, software stop-loss, heartbeat/kill-sentinel checks, and
// graceful shutdown. There is exactly one Runner per process (WP-9 task 1).
type Runner struct {
	te    *TradingEngine
	cfg   RunnerConfig
	strat strategy.Strategy

	priceHistory *strategy.PriceHistory
	candleAgg    *strategy.CandleAggregator // R2-2/R2-INT wiring (G-2): real OHLC candles for candle-primary strategies (sniper)
	holidays     map[string]bool            // R2-5/R2-INT wiring (G-12): loaded from cfg.HolidayFile, falls back to nseHolidays2026

	mu     sync.Mutex
	open   map[string]*openStructure
	paused atomic.Bool
	ticks  int64
}

// NewRunner constructs a Runner. The strategy is resolved from the registry
// singleton (strategy.Get), so repeated NewRunner calls for the same name
// within one process share strategy state — the intended behavior once the
// old duplicate-loop bug (task 1) is gone.
func NewRunner(te *TradingEngine, cfg RunnerConfig) (*Runner, error) {
	cfg.applyDefaults()
	strat, err := strategy.Get(cfg.StrategyName)
	if err != nil {
		return nil, fmt.Errorf("engine: cannot start runner: %w", err)
	}
	holidays, err := loadHolidays(cfg.HolidayFile)
	if err != nil {
		return nil, fmt.Errorf("engine: cannot start runner: %w", err)
	}
	return &Runner{
		te:           te,
		cfg:          cfg,
		strat:        strat,
		priceHistory: strategy.NewPriceHistory(cfg.HistorySize),
		candleAgg:    strategy.NewCandleAggregator(5*time.Minute, cfg.HistorySize),
		holidays:     holidays,
		open:         make(map[string]*openStructure),
	}, nil
}

// holidayFile is the on-disk shape of the R2-5 holiday calendar
// (go-engine/nse_holidays.yaml): a flat list of {date, description}.
type holidayFile struct {
	Holidays []struct {
		Date        string `yaml:"date"`
		Description string `yaml:"description"`
	} `yaml:"holidays"`
}

// loadHolidays loads path (R2-5's nse_holidays.yaml format) into a
// "2006-01-02" -> true set.
//
// Fails CLOSED, not open: an empty path is the documented default (use the
// built-in nseHolidays2026 table — that is not a failure, it's the intended
// behavior when no file is configured). But once an operator has explicitly
// pointed HolidayFile at a path, a missing/malformed/empty file is a
// configuration error and must stop startup rather than silently trade
// against a possibly-stale hardcoded table without the operator knowing
// their real calendar never loaded (an unnoticed fail-open here could mean
// entries on what is actually an NSE holiday).
func loadHolidays(path string) (map[string]bool, error) {
	if path == "" {
		return nseHolidays2026, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("holiday file %q: %w", path, err)
	}
	var hf holidayFile
	if err := yaml.Unmarshal(data, &hf); err != nil {
		return nil, fmt.Errorf("holiday file %q: parse: %w", path, err)
	}
	out := make(map[string]bool, len(hf.Holidays))
	for _, h := range hf.Holidays {
		if h.Date != "" {
			out[h.Date] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("holiday file %q loaded but contained no dates", path)
	}
	log.Printf("📅 loaded %d holiday date(s) from %s (replaces the built-in hardcoded table)", len(out), path)
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────
// State persistence (WP-9 task 10 / issue in WP-3's report: strategy state
// is an opaque string k/v store; we encode our own runner-level bookkeeping
// under the "legs:<symbol>" / "premium:<symbol>" / "entrytime:<symbol>" keys
// and the strategy's own Snapshot() under "strat:<key>".
// ─────────────────────────────────────────────────────────────────────────

// RestoreState reloads persisted strategy/runner/risk state at startup (the
// CR-8 recovery flow — call after state.RecoverSession/Reconcile). It
// restores three things, in order:
//  1. The risk manager's live position book and balance/P&L figures from
//     the durable store (state.Store.ListOpenPositions / LoadRiskSnapshot) —
//     without this, a restarted process would enforce drawdown/stop-loss
//     checks against an empty position book even though the broker (and the
//     store) still show open risk. This is engine-owned bookkeeping, not a
//     risk.Manager redesign: OpenPositions is already an exported map, so
//     recovery writes into it directly rather than adding a new risk.Manager
//     method.
//  2. This strategy's own Snapshot()/Restore() state, if it implements
//     snapshotter.
//  3. The runner-level "which legs make up each open structure" bookkeeping
//     (keyed "legs:<underlying>"), cross-referenced against the store's
//     position records so each restored leg keeps its PositionID (needed so
//     a later PlaceExitOrder can mark the durable position CLOSED instead of
//     leaving a stale OPEN row forever).
func (r *Runner) RestoreState() {
	if r.te.store == nil {
		return
	}

	positions, err := r.te.store.ListOpenPositions()
	if err != nil {
		log.Printf("⚠️ could not list open positions for recovery: %v", err)
	}
	byID := make(map[string]string, len(positions)) // symbol -> store position ID
	for _, p := range positions {
		byID[p.Symbol] = p.ID
		side := risk.Buy
		if p.Side == models.SideSell {
			side = risk.Sell
		}
		tt := risk.EquityIntraday
		if looksLikeOptionSymbol(p.Symbol) {
			tt = risk.OptCarry
		}
		r.te.riskManager.RestorePosition(p.Symbol, p.EntryPrice, p.Quantity, tt, side, p.EntryTime.Unix())
	}
	if snap, err := r.te.store.LoadRiskSnapshot(); err == nil && !snap.UpdatedAt.IsZero() {
		r.te.riskManager.RestoreSnapshot(snap.Balance, snap.RealizedPnL, snap.SessionUsed)
		log.Printf("♻️  restored risk snapshot: balance=₹%.2f realizedPnL=₹%.2f sessionUsed=₹%.2f",
			snap.Balance, snap.RealizedPnL, snap.SessionUsed)
	}

	all, err := r.te.store.LoadAllStrategyState(r.cfg.StrategyName)
	if err != nil {
		log.Printf("⚠️ could not load strategy state for %s: %v", r.cfg.StrategyName, err)
		return
	}
	if len(all) == 0 {
		return
	}

	if sn, ok := r.strat.(snapshotter); ok {
		strat := make(map[string]string)
		for k, v := range all {
			if rest, found := strings.CutPrefix(k, "strat:"); found {
				strat[rest] = v
			}
		}
		if len(strat) > 0 {
			sn.Restore(strat)
			log.Printf("♻️  restored strategy state for %s: %v", r.cfg.StrategyName, strat)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range all {
		underlying, found := strings.CutPrefix(k, "legs:")
		if !found || v == "" {
			continue
		}
		st := &openStructure{MultiLeg: true}
		for _, sym := range strings.Split(v, ",") {
			if sym != "" {
				st.Legs = append(st.Legs, legRecord{Symbol: sym, PositionID: byID[sym]})
			}
		}
		if p, ok := all["premium:"+underlying]; ok {
			st.EntryPremium, _ = strconv.ParseFloat(p, 64)
		}
		if t, ok := all["entrytime:"+underlying]; ok {
			st.EntryTime, _ = time.Parse(time.RFC3339, t)
		}
		r.open[underlying] = st
		log.Printf("♻️  restored open structure %s: %d leg(s)", underlying, len(st.Legs))
	}
}

func (r *Runner) persistStructure(underlying string, st *openStructure) {
	if r.te.store == nil {
		return
	}
	syms := make([]string, len(st.Legs))
	for i, l := range st.Legs {
		syms[i] = l.Symbol
	}
	_ = r.te.store.SaveStrategyState(r.cfg.StrategyName, "legs:"+underlying, strings.Join(syms, ","))
	_ = r.te.store.SaveStrategyState(r.cfg.StrategyName, "premium:"+underlying, strconv.FormatFloat(st.EntryPremium, 'f', -1, 64))
	_ = r.te.store.SaveStrategyState(r.cfg.StrategyName, "entrytime:"+underlying, st.EntryTime.Format(time.RFC3339))
}

func (r *Runner) clearStructure(underlying string) {
	if r.te.store == nil {
		return
	}
	_ = r.te.store.SaveStrategyState(r.cfg.StrategyName, "legs:"+underlying, "")
}

func (r *Runner) persistStrategySnapshot() {
	if r.te.store == nil {
		return
	}
	if sn, ok := r.strat.(snapshotter); ok {
		for k, v := range sn.Snapshot() {
			_ = r.te.store.SaveStrategyState(r.cfg.StrategyName, "strat:"+k, v)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Control hooks (CR-4) — wired into api.ControlHooks by cmd/main.go.
// ─────────────────────────────────────────────────────────────────────────

// Pause blocks new entries; exits/stop-losses/square-off continue running.
func (r *Runner) Pause() error {
	r.paused.Store(true)
	log.Println("⏸️  Runner PAUSED via control hook (new entries blocked, exits still allowed)")
	return nil
}

// Resume undoes Pause.
func (r *Runner) Resume() error {
	r.paused.Store(false)
	log.Println("▶️  Runner RESUMED via control hook")
	return nil
}

// KillAndFlatten triggers the kill switch and flattens every open position.
func (r *Runner) KillAndFlatten() error {
	r.te.riskManager.TriggerKillSwitch()
	log.Println("🔴 KILL SWITCH triggered via control hook — flattening all positions")
	if r.cfg.Alert != nil {
		r.cfg.Alert("kill_switch", "triggered via /api/kill control hook")
	}
	r.flattenAll("manual kill switch")
	return nil
}

// Status reports real, current engine state.
func (r *Runner) Status() RunnerStatus {
	stats := r.te.GetSessionStats()
	bal, _ := stats["risk_balance"].(float64)
	unreal, _ := stats["unrealized_pnl"].(float64)
	real, _ := stats["realized_pnl"].(float64)
	posCount, _ := stats["open_positions"].(int)
	return RunnerStatus{
		Running:         !r.paused.Load(),
		Mode:            r.cfg.ModeLabel,
		Strategy:        r.cfg.StrategyName,
		Balance:         bal,
		UnrealizedPnL:   unreal,
		RealizedPnL:     real,
		PositionsCount:  posCount,
		StopLossEnabled: r.te.riskManager.StopLossConfig.Enabled,
		StopLossPercent: r.te.riskManager.StopLossConfig.Value,
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Main loop
// ─────────────────────────────────────────────────────────────────────────

// Run starts the tick loop and blocks until ctx is cancelled, then performs
// graceful shutdown (task 12) and returns. A non-nil error means shutdown
// could NOT verify a flat position book — the caller (cmd/main.go) must
// exit(1), not exit(0), in that case.
func (r *Runner) Run(ctx context.Context) error {
	r.RestoreState()
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	log.Printf("🚀 Runner started [%s mode | strategy=%s | symbols=%v | poll=%s]",
		r.cfg.ModeLabel, r.cfg.StrategyName, r.cfg.Symbols, r.cfg.PollInterval)

	for {
		select {
		case <-ctx.Done():
			return r.Shutdown()
		case <-ticker.C:
			r.tick()
		}
	}
}

func (r *Runner) tick() {
	r.ticks++
	r.touchHeartbeat()
	r.checkKillSentinel()

	rc := r.te.riskManager.CheckRisk()
	if rc.Breached {
		if !r.paused.Load() {
			log.Printf("🚨 RISK BREACH: %s — halting new entries and flattening all positions", rc.Reason)
			if r.cfg.Alert != nil {
				r.cfg.Alert("drawdown_breach", rc.Reason)
			}
		}
		r.paused.Store(true)
		r.flattenAll("risk breach: " + rc.Reason)
	}

	if healthy, herr := r.te.BrokerHealthy(); !healthy {
		if !r.paused.Load() {
			log.Printf("🚨 BROKER UNHEALTHY: %v — halting new entries (exits still attempted)", herr)
			if r.cfg.Alert != nil {
				r.cfg.Alert("auth_failure", fmt.Sprintf("%v", herr))
			}
		}
	}

	nowIST := time.Now().In(strategy.IST)
	ms := r.marketState(nowIST)
	if ms == marketSquareOff {
		r.flattenAll("intraday square-off")
	}
	entriesAllowed := ms == marketOpenState && !r.paused.Load() && !rc.Breached

	if r.ticks%r.heartbeatTicks() == 0 {
		r.logHeartbeat(ms)
	}

	r.softStopLossCheck()

	for _, symbol := range r.cfg.Symbols {
		r.evaluateSymbol(symbol, entriesAllowed, nowIST)
	}
}

func (r *Runner) heartbeatTicks() int64 {
	n := int64(r.cfg.HeartbeatInterval / r.cfg.PollInterval)
	if n < 1 {
		n = 1
	}
	return n
}

func (r *Runner) logHeartbeat(ms marketState) {
	stats := r.te.GetSessionStats()
	log.Printf("💓 Heartbeat | Market: %s | Balance: ₹%.2f | Positions: %v | Unrealized: ₹%.2f | Realized: ₹%.2f | Total P&L: ₹%.2f | Paused: %v",
		ms, stats["risk_balance"], stats["open_positions"], stats["unrealized_pnl"], stats["realized_pnl"], stats["total_pnl"], r.paused.Load())
}

func (r *Runner) touchHeartbeat() {
	if r.cfg.HeartbeatFilePath == "" {
		return
	}
	if dir := filepath.Dir(r.cfg.HeartbeatFilePath); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	_ = os.WriteFile(r.cfg.HeartbeatFilePath, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)
}

// checkKillSentinel implements task 3: a data/KILL file present on disk (of
// any content) triggers the kill switch, same as a runtime API call.
func (r *Runner) checkKillSentinel() {
	if r.cfg.KillSentinelPath == "" {
		return
	}
	if _, err := os.Stat(r.cfg.KillSentinelPath); err == nil {
		if !r.te.riskManager.KillSwitchActive() {
			log.Printf("🔴 KILL sentinel file detected (%s) — triggering kill switch", r.cfg.KillSentinelPath)
			if r.cfg.Alert != nil {
				r.cfg.Alert("kill_switch", "sentinel file "+r.cfg.KillSentinelPath)
			}
		}
		r.te.riskManager.TriggerKillSwitch()
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Market hours (task 8)
// ─────────────────────────────────────────────────────────────────────────

type marketState int

const (
	marketOpenState marketState = iota
	marketClosedWeekend
	marketClosedHoliday
	marketClosedPreOpen
	marketSquareOff // reached square-off time; entries blocked, flatten triggered once
	marketClosedPostClose
)

func (s marketState) String() string {
	switch s {
	case marketOpenState:
		return "OPEN"
	case marketClosedWeekend:
		return "CLOSED(weekend)"
	case marketClosedHoliday:
		return "CLOSED(holiday)"
	case marketClosedPreOpen:
		return "PRE-OPEN"
	case marketSquareOff:
		return "SQUARE-OFF"
	case marketClosedPostClose:
		return "CLOSED(post-close)"
	default:
		return "UNKNOWN"
	}
}

// nseHolidays2026 is a small, DELIBERATELY INCOMPLETE table of fixed-date
// NSE holidays for 2026 — as-of 2026-07-18 (audit date). Movable
// festival-based holidays (Holi, Diwali/Laxmi Puja, Eid, Dussehra, Good
// Friday, etc.) are NOT included: this is a documented gap (see
// docs/reports/WP-9-REPORT.md), not a silent omission. Verify against NSE's
// official trading-holiday circular before relying on this for live trading.
var nseHolidays2026 = map[string]bool{
	"2026-01-26": true, // Republic Day
	"2026-08-15": true, // Independence Day
	"2026-10-02": true, // Gandhi Jayanti
	"2026-12-25": true, // Christmas
}

func (r *Runner) marketState(nowIST time.Time) marketState {
	if nowIST.Weekday() == time.Saturday || nowIST.Weekday() == time.Sunday {
		return marketClosedWeekend
	}
	if r.holidays[nowIST.Format("2006-01-02")] {
		return marketClosedHoliday
	}
	y, m, d := nowIST.Date()
	openTime := time.Date(y, m, d, 9, 15, 0, 0, strategy.IST).Add(time.Duration(r.cfg.MarketOpenBufferMin) * time.Minute)
	squareOff := time.Date(y, m, d, r.cfg.SquareOffHour, r.cfg.SquareOffMinute, 0, 0, strategy.IST)
	hardClose := time.Date(y, m, d, 15, 30, 0, 0, strategy.IST)

	switch {
	case nowIST.Before(openTime):
		return marketClosedPreOpen
	case nowIST.Before(squareOff):
		return marketOpenState
	case nowIST.Before(hardClose):
		return marketSquareOff
	default:
		return marketClosedPostClose
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Strategy evaluation / order flow
// ─────────────────────────────────────────────────────────────────────────

func (r *Runner) evaluateSymbol(symbol string, entriesAllowed bool, nowIST time.Time) {
	price := r.te.broker.GetCurrentPrice(symbol)
	if price <= 0 {
		return
	}
	volume := r.te.broker.GetCurrentVolume(symbol)

	r.mu.Lock()
	st, hasPos := r.open[symbol]
	r.mu.Unlock()

	ctx := strategy.EvalContext{Symbol: symbol, Now: nowIST, HasPosition: hasPos}

	if hasPos {
		ctx.EntryPremium = st.EntryPremium
		ctx.PositionAge = time.Since(st.EntryTime)
		if st.MultiLeg {
			// issue 5 / WP-7 precedent: while a multi-leg structure is open,
			// feed the COMBINED CURRENT PREMIUM series into Prices, not the
			// underlying spot — this is what nine_twenty/short_straddle's
			// premium-based stop (and any RSI computed while in-position)
			// actually reads.
			combined := r.combinedLegPremium(st.Legs)
			r.mu.Lock()
			st.PremiumHistory = append(st.PremiumHistory, combined)
			if len(st.PremiumHistory) > r.cfg.HistorySize {
				st.PremiumHistory = st.PremiumHistory[len(st.PremiumHistory)-r.cfg.HistorySize:]
			}
			ctx.Prices = append([]float64(nil), st.PremiumHistory...)
			r.mu.Unlock()
		} else {
			r.priceHistory.Add(symbol, price, volume)
			r.candleAgg.Add(symbol, price, volume, nowIST)
			ctx.Prices = r.priceHistory.GetPrices(symbol)
			ctx.Volumes = r.priceHistory.GetVolumes(symbol)
		}
	} else {
		r.priceHistory.Add(symbol, price, volume)
		r.candleAgg.Add(symbol, price, volume, nowIST)
		if r.priceHistory.GetDataPoints(symbol) < r.cfg.MinDataPoints {
			return
		}
		ctx.Prices = r.priceHistory.GetPrices(symbol)
		ctx.Volumes = r.priceHistory.GetVolumes(symbol)
	}

	// R2-2/G-2 wiring: give candle-primary strategies (sniper) a real OHLC
	// buffer alongside the existing Prices/Volumes tick series — purely
	// additive, does not change nine_twenty/short_straddle's premium-based
	// stop contract (LastPrice() still prefers Prices over Candles).
	ctx.Candles = r.candleAgg.Completed(symbol)

	sig := r.strat.Evaluate(ctx)

	switch sig.Action {
	case strategy.Exit:
		if hasPos {
			r.exitStructure(symbol, sig.Reason)
		}
	case strategy.Hold:
		// nothing to do
	case strategy.Buy, strategy.Sell:
		if hasPos || !entriesAllowed {
			return
		}
		if len(sig.Legs) > 0 {
			r.enterMultiLeg(symbol, sig)
		} else {
			r.enterDirectional(symbol, sig, price)
		}
	}
}

func (r *Runner) combinedLegPremium(legs []legRecord) float64 {
	var sum float64
	for _, l := range legs {
		sum += r.te.broker.GetCurrentPrice(l.Symbol)
	}
	return sum
}

// basketMargin implements R2-INT's wiring of R2-1's G-1 margin API: it calls
// the broker's real margin-calculator batch endpoint (ExtendedTradeService.
// GetRequiredMargin) for a basket of one or more orders. Fail-closed (CR-13):
// if the broker doesn't implement ExtendedTradeService (MockBroker/
// LivePaperBroker don't, per R2-1's report) or the call errors, an error is
// returned — never a premium-based fallback estimate.
func (r *Runner) basketMargin(orders []broker.MarginOrderInput) (float64, error) {
	ext, ok := r.te.broker.(broker.ExtendedTradeService)
	if !ok {
		return 0, fmt.Errorf("margin unavailable: broker does not implement ExtendedTradeService.GetRequiredMargin; SELL derivative entries are blocked fail-closed (CR-13)")
	}
	return ext.GetRequiredMargin(orders)
}

// requiredMargin queries broker-supplied margin for a single-leg SELL
// derivative order.
func (r *Runner) requiredMargin(symbol string, qty int) (float64, error) {
	return r.basketMargin([]broker.MarginOrderInput{{
		Symbol:          symbol,
		TransactionType: broker.Sell,
		Quantity:        qty,
	}})
}

func (r *Runner) enterDirectional(underlying string, sig strategy.Signal, price float64) {
	if isKnownIndex(underlying) || underlying == r.cfg.IndexSymbol {
		optType := "CE"
		if sig.Action == strategy.Sell {
			optType = "PE" // directional bearish view = long PUT (mirrors pre-remediation behavior)
		}
		expiry, err := r.resolveExpiry(underlying)
		if err != nil {
			log.Printf("❌ entry skipped [%s]: %v", underlying, err)
			return
		}
		step := r.strikeStep(underlying)
		probe := buildOptionSymbol(underlying, expiry, price, step, 0, optType)
		lotSize, err := r.lotSize(underlying, probe)
		if err != nil {
			log.Printf("❌ entry skipped [%s]: %v", underlying, err)
			return
		}
		qty := lotSize // 1 lot; sizing beyond 1 lot is a strategy/config concern, not wired here
		r.placeSingleLeg(underlying, probe, broker.Buy, qty, risk.OptCarry)
		return
	}

	// Direct equity/derivative symbol (no index → option translation).
	side := broker.Buy
	tradeType := risk.EquityIntraday
	if sig.Action == strategy.Sell {
		side = broker.Sell
	}
	r.placeSingleLeg(underlying, underlying, side, r.cfg.DefaultQuantity, tradeType)
}

func (r *Runner) placeSingleLeg(underlying, tradeSymbol string, side broker.OrderSide, qty int, tradeType risk.TradeType) {
	var requiredMargin float64
	if side == broker.Sell && risk.IsDerivative(tradeType) {
		m, err := r.requiredMargin(tradeSymbol, qty)
		if err != nil {
			log.Printf("❌ Entry REJECTED [%s]: %v", tradeSymbol, err)
			if r.cfg.Alert != nil {
				r.cfg.Alert("margin_unavailable", tradeSymbol)
			}
			return
		}
		requiredMargin = m
	}

	res, err := r.te.PlaceEntryOrder(tradeSymbol, qty, side, tradeType, r.cfg.StrategyName, requiredMargin)
	if err != nil {
		if errors.Is(err, broker.ErrOrderIndeterminate) && r.cfg.Alert != nil {
			r.cfg.Alert("indeterminate_order", tradeSymbol)
		}
		return
	}

	slID := r.placeBrokerStop(tradeSymbol, res.Filled, side)
	st := &openStructure{
		Legs:         []legRecord{{Symbol: tradeSymbol, Side: side, PositionID: res.PositionID, SLOrderID: slID}},
		EntryPremium: res.Filled.FillPrice,
		EntryTime:    time.Now(),
		MultiLeg:     false,
	}
	r.mu.Lock()
	r.open[underlying] = st
	r.mu.Unlock()
	r.persistStructure(underlying, st)
}

// enterMultiLeg implements WP-9 task 5's multi-leg order flow: place legs in
// the order given (hedges first per CR-12, enforced by the strategy's own
// Legs ordering), wait for each fill before placing the next, and on any
// rejection immediately unwind the legs already known to be filled.
func (r *Runner) enterMultiLeg(underlying string, sig strategy.Signal) {
	effectiveIndex, effectiveExpiry, effectiveSpot, err := r.effectiveLegContext(underlying)
	if err != nil {
		log.Printf("❌ multi-leg entry skipped [%s]: %v", underlying, err)
		return
	}
	step := r.strikeStep(effectiveIndex)
	probe := buildOptionSymbol(effectiveIndex, effectiveExpiry, effectiveSpot, step, 0, "CE")
	lotSize, err := r.lotSize(effectiveIndex, probe)
	if err != nil {
		log.Printf("❌ multi-leg entry skipped [%s]: %v", underlying, err)
		return
	}

	// R2-1/R2-INT wiring (G-1): resolve every leg's target symbol/side/qty
	// FIRST, then price the WHOLE basket together in ONE GetRequiredMargin
	// call — this is what lets Angel's margin calculator apply the
	// hedged-margin benefit across e.g. iron_fly's 4 legs, instead of
	// pricing (and over-stating) each leg in isolation. A basket-level
	// margin rejection happens before any leg is placed, so there is
	// nothing to unwind in that case.
	type plannedLeg struct {
		symbol string
		side   broker.OrderSide
		qty    int
	}
	planned := make([]plannedLeg, len(sig.Legs))
	basketOrders := make([]broker.MarginOrderInput, len(sig.Legs))
	sellCount := 0
	for i, leg := range sig.Legs {
		targetSymbol := buildOptionSymbol(effectiveIndex, effectiveExpiry, effectiveSpot, step, leg.StrikeOffset, leg.OptionType)
		side := broker.Buy
		if leg.Direction == strategy.LegSell {
			side = broker.Sell
			sellCount++
		}
		qtyLots := leg.Quantity
		if qtyLots <= 0 {
			qtyLots = 1
		}
		qty := qtyLots * lotSize
		planned[i] = plannedLeg{symbol: targetSymbol, side: side, qty: qty}
		basketOrders[i] = broker.MarginOrderInput{Symbol: targetSymbol, TransactionType: side, Quantity: qty}
	}

	// requiredMargin.go's per-symbol locking (risk.Manager) has no concept of
	// a shared basket-level lock; Angel's own margin breakdown per leg isn't
	// parsed (R2-1's report, §6 "Known limitations"). Split the basket total
	// evenly across the SELL legs — a documented approximation, not exact
	// per-leg accounting, but it never under-locks the combined total.
	// ponytail: even split, not proportional to notional; revisit if a
	// strategy ever mixes very differently-sized SELL legs.
	var perSellLegMargin float64
	if sellCount > 0 {
		total, err := r.basketMargin(basketOrders)
		if err != nil {
			log.Printf("❌ multi-leg entry REJECTED [%s]: %v — no legs placed", underlying, err)
			if r.cfg.Alert != nil {
				r.cfg.Alert("margin_unavailable", underlying)
			}
			return
		}
		perSellLegMargin = total / float64(sellCount)
	}

	var legs []legRecord
	var premiumSum float64
	for i := range sig.Legs {
		p := planned[i]
		targetSymbol, side, qty := p.symbol, p.side, p.qty

		var requiredMargin float64
		if side == broker.Sell {
			requiredMargin = perSellLegMargin
		}

		res, err := r.te.PlaceEntryOrder(targetSymbol, qty, side, risk.OptCarry, r.cfg.StrategyName, requiredMargin)
		if err != nil {
			if errors.Is(err, broker.ErrOrderIndeterminate) {
				log.Printf("🟡 CRITICAL: leg %d/%d [%s] INDETERMINATE — NOT unwinding this leg (fill state unknown); unwinding the %d leg(s) known filled; structure needs MANUAL reconciliation", i+1, len(sig.Legs), targetSymbol, len(legs))
				if r.cfg.Alert != nil {
					r.cfg.Alert("indeterminate_order", targetSymbol)
				}
				r.unwindLegs(legs)
				return
			}
			log.Printf("❌ leg %d/%d REJECTED [%s]: %v — unwinding %d already-filled leg(s)", i+1, len(sig.Legs), targetSymbol, err, len(legs))
			r.unwindLegs(legs)
			if r.cfg.Alert != nil {
				r.cfg.Alert("leg_rejected", targetSymbol)
			}
			return
		}

		slID := r.placeBrokerStop(targetSymbol, res.Filled, side)
		legs = append(legs, legRecord{Symbol: targetSymbol, Side: side, PositionID: res.PositionID, SLOrderID: slID})
		premiumSum += res.Filled.FillPrice
	}

	st := &openStructure{Legs: legs, EntryPremium: premiumSum, EntryTime: time.Now(), MultiLeg: true}
	r.mu.Lock()
	r.open[underlying] = st
	r.mu.Unlock()
	r.persistStructure(underlying, st)
	if ec, ok := r.strat.(entryConfirmer); ok {
		ec.ConfirmEntry(underlying)
	}
	r.persistStrategySnapshot()
	log.Printf("✅ Multi-leg structure OPENED [%s]: %d leg(s), combined entry premium ₹%.2f", underlying, len(legs), premiumSum)
}

// unwindLegs closes every leg known to have filled, using reduce-only exit
// orders (bypasses the circuit breaker per EX-2). Used both for multi-leg
// entry failures and generic flatten/shutdown.
func (r *Runner) unwindLegs(legs []legRecord) {
	for _, l := range legs {
		if ext, ok := r.te.broker.(broker.ExtendedTradeService); ok && l.SLOrderID != "" {
			if err := ext.CancelOrder(l.SLOrderID); err != nil {
				log.Printf("⚠️ could not cancel resting SL order %s for %s: %v", l.SLOrderID, l.Symbol, err)
			}
		}
		if _, _, err := r.te.PlaceExitOrder(l.Symbol, r.cfg.StrategyName, l.PositionID); err != nil {
			log.Printf("❌ CRITICAL: failed to unwind leg %s: %v — position may be naked, MANUAL INTERVENTION REQUIRED", l.Symbol, err)
			if r.cfg.Alert != nil {
				r.cfg.Alert("flatten_failure", l.Symbol)
			}
		}
	}
}

// exitStructure closes every leg of underlying's open structure.
func (r *Runner) exitStructure(underlying, reason string) {
	r.mu.Lock()
	st, ok := r.open[underlying]
	r.mu.Unlock()
	if !ok {
		return
	}
	log.Printf("🛑 Closing structure [%s]: %s", underlying, reason)
	allOK := true
	for _, leg := range st.Legs {
		if ext, ok2 := r.te.broker.(broker.ExtendedTradeService); ok2 && leg.SLOrderID != "" {
			if err := ext.CancelOrder(leg.SLOrderID); err != nil {
				log.Printf("⚠️ could not cancel resting SL order %s for %s: %v", leg.SLOrderID, leg.Symbol, err)
			}
		}
		if _, _, err := r.te.PlaceExitOrder(leg.Symbol, r.cfg.StrategyName, leg.PositionID); err != nil {
			allOK = false
			log.Printf("❌ failed to close leg %s: %v", leg.Symbol, err)
			if r.cfg.Alert != nil {
				r.cfg.Alert("flatten_failure", leg.Symbol)
			}
		}
	}
	r.mu.Lock()
	delete(r.open, underlying)
	r.mu.Unlock()
	r.clearStructure(underlying)
	if allOK {
		if ec, ok2 := r.strat.(exitConfirmer); ok2 {
			ec.ConfirmExit(underlying)
		}
	}
	r.persistStrategySnapshot()
}

// flattenAll closes every tracked structure AND, fail-closed, anything the
// risk manager still knows about that isn't tracked by this Runner's own
// bookkeeping (e.g. recovered from a startup reconcile).
func (r *Runner) flattenAll(reason string) {
	r.mu.Lock()
	underlyings := make([]string, 0, len(r.open))
	for u := range r.open {
		underlyings = append(underlyings, u)
	}
	r.mu.Unlock()

	for _, u := range underlyings {
		r.exitStructure(u, reason)
	}

	for symbol := range r.te.riskManager.GetOpenPositions() {
		if _, _, err := r.te.PlaceExitOrder(symbol, r.cfg.StrategyName, ""); err != nil {
			log.Printf("❌ flatten: failed to close leftover position %s: %v", symbol, err)
			if r.cfg.Alert != nil {
				r.cfg.Alert("flatten_failure", symbol)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Stop-loss (task 7)
// ─────────────────────────────────────────────────────────────────────────

func (r *Runner) placeBrokerStop(symbol string, filled *broker.FilledOrder, entrySide broker.OrderSide) string {
	if !r.te.riskManager.StopLossConfig.Enabled {
		return ""
	}
	ext, ok := r.te.broker.(broker.ExtendedTradeService)
	if !ok {
		log.Printf("ℹ️  broker-side stop-loss unavailable for %s (paper/mock broker has no ExtendedTradeService) — relying on the software stop-loss loop only", symbol)
		return ""
	}
	slSide := broker.Sell
	trigger := filled.FillPrice * (1 - r.te.riskManager.StopLossConfig.Value/100)
	if entrySide == broker.Sell {
		slSide = broker.Buy
		trigger = filled.FillPrice * (1 + r.te.riskManager.StopLossConfig.Value/100)
	}
	orderID, err := ext.PlaceStopLossOrder(symbol, filled.Quantity, trigger, slSide)
	if err != nil {
		log.Printf("⚠️ broker-side SL placement failed for %s: %v (software SL loop still active)", symbol, err)
		return ""
	}
	log.Printf("🛡️  Broker-side SL-M placed for %s @ trigger ₹%.2f (OrderID=%s)", symbol, trigger, orderID)
	return orderID
}

// softStopLossCheck is the supplementary software SL loop (kept per task 7
// even though broker-side SL-M is also placed). Uses GetCurrentPriceWithAge
// when the broker supports it; a price older than cfg.StaleAge is treated as
// invalid for SL decisions — never fires on stale data, alerts instead.
func (r *Runner) softStopLossCheck() {
	if !r.te.riskManager.StopLossConfig.Enabled {
		return
	}
	r.mu.Lock()
	underlyings := make([]string, 0, len(r.open))
	for u := range r.open {
		underlyings = append(underlyings, u)
	}
	r.mu.Unlock()

	covered := make(map[string]bool)
	for _, u := range underlyings {
		r.mu.Lock()
		st, ok := r.open[u]
		r.mu.Unlock()
		if !ok {
			continue
		}
		for _, leg := range st.Legs {
			covered[leg.Symbol] = true
			price, age, fresh := r.priceWithAge(leg.Symbol)
			if !fresh || price <= 0 {
				continue
			}
			if age > r.cfg.StaleAge {
				log.Printf("⚠️ %s: price is %s stale (> %s threshold) — skipping software SL decision on stale data", leg.Symbol, age.Round(time.Second), r.cfg.StaleAge)
				if r.cfg.Alert != nil {
					r.cfg.Alert("stale_price", leg.Symbol)
				}
				continue
			}
			if r.te.riskManager.ShouldTriggerStopLoss(leg.Symbol, price) {
				log.Printf("🛑 SOFTWARE STOP-LOSS TRIGGERED [%s]", leg.Symbol)
				r.exitStructure(u, "software stop-loss: "+leg.Symbol)
				break
			}
		}
	}

	// R3 fix: the risk manager can hold a position with NO corresponding
	// r.open leg -- specifically, an entry order that came back
	// ErrOrderIndeterminate (engine.go's PlaceEntryOrder deliberately does
	// NOT roll back risk state there, since the fill may have happened at
	// the broker) is added to risk.Manager but never reaches r.open, since
	// the multi-leg builder returns before appending it. Without this, such
	// a position got zero software-SL coverage until the next restart or an
	// explicit flatten -- fail-closed here the same way flattenAll already
	// treats the risk manager as the authoritative source of truth for
	// untracked positions.
	for symbol := range r.te.riskManager.GetOpenPositions() {
		if covered[symbol] {
			continue
		}
		price, age, fresh := r.priceWithAge(symbol)
		if !fresh || price <= 0 {
			continue
		}
		if age > r.cfg.StaleAge {
			log.Printf("⚠️ %s: price is %s stale (> %s threshold) — skipping software SL decision on stale data", symbol, age.Round(time.Second), r.cfg.StaleAge)
			if r.cfg.Alert != nil {
				r.cfg.Alert("stale_price", symbol)
			}
			continue
		}
		if r.te.riskManager.ShouldTriggerStopLoss(symbol, price) {
			log.Printf("🛑 SOFTWARE STOP-LOSS TRIGGERED [%s] (untracked/indeterminate-origin position)", symbol)
			if _, _, err := r.te.PlaceExitOrder(symbol, r.cfg.StrategyName, ""); err != nil {
				log.Printf("❌ failed to close untracked position %s on stop-loss: %v", symbol, err)
				if r.cfg.Alert != nil {
					r.cfg.Alert("flatten_failure", symbol)
				}
			}
		}
	}
}

func (r *Runner) priceWithAge(symbol string) (float64, time.Duration, bool) {
	if ext, ok := r.te.broker.(broker.ExtendedTradeService); ok {
		p, age, err := ext.GetCurrentPriceWithAge(symbol)
		if err != nil {
			return 0, 0, false
		}
		return p, age, true
	}
	p := r.te.broker.GetCurrentPrice(symbol)
	if p <= 0 {
		return 0, 0, false
	}
	return p, 0, true // paper/mock broker: no staleness concept, always "fresh"
}

// ─────────────────────────────────────────────────────────────────────────
// Symbol / expiry / lot-size resolution (task 9)
// ─────────────────────────────────────────────────────────────────────────

func (r *Runner) effectiveLegContext(symbol string) (index, expiry string, spot float64, err error) {
	index = symbol
	if looksLikeOptionSymbol(symbol) {
		index = extractIndexPrefix(symbol)
	}
	spot = r.te.broker.GetCurrentPrice(index)
	if spot <= 0 {
		prices, _ := r.te.broker.FetchMarketDataBatch([]string{index})
		spot = prices[index]
	}
	if spot <= 0 {
		return "", "", 0, fmt.Errorf("no spot price available for %s", index)
	}
	expiry, err = r.resolveExpiry(index)
	if err != nil {
		return "", "", 0, err
	}
	return index, expiry, spot, nil
}

// resolveExpiry replaces the old hardcoded/weekday-arithmetic expiry logic
// with WP-1's instrument-master-backed GetExpiries: nearest expiry >= today.
// cfg.OptionExpiryOverride, if set, takes precedence (operator override).
func (r *Runner) resolveExpiry(underlying string) (string, error) {
	if r.cfg.OptionExpiryOverride != "" {
		return r.cfg.OptionExpiryOverride, nil
	}
	if r.cfg.Instruments == nil {
		return "", fmt.Errorf("no instrument master loaded — cannot resolve expiry for %s (set options_config.expiry as an override)", underlying)
	}
	expiries, err := r.cfg.Instruments.GetExpiries(underlying)
	if err != nil {
		return "", err
	}
	nowIST := time.Now().In(strategy.IST)
	today := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 0, 0, 0, 0, strategy.IST)
	for _, e := range expiries {
		if !e.Before(today) {
			return strings.ToUpper(e.Format("02Jan06")), nil
		}
	}
	return "", fmt.Errorf("no expiry >= today found for %s in instrument master", underlying)
}

// lotSize resolves via WP-1's InstrumentManager.GetLotSize against a
// concrete probe symbol first (the authoritative source), then an explicit
// per-underlying config override (LotSizes), then an explicit single-value
// config override (LotSizeFallback, from options_config.lot_size). Every
// tier here is either the real instrument master or a value the operator
// deliberately set — never a guessed constant. If none resolve, this
// returns an error and the caller must refuse the trade, not size a real
// order off a made-up number.
func (r *Runner) lotSize(underlying, probeSymbol string) (int, error) {
	if r.cfg.Instruments != nil {
		if ls, err := r.cfg.Instruments.GetLotSize(probeSymbol); err == nil && ls > 0 {
			return ls, nil
		}
	}
	if size, ok := r.cfg.LotSizes[underlying]; ok && size > 0 {
		log.Printf("⚠️ instrument-master lot size lookup failed for %s; using per-underlying config override %d", probeSymbol, size)
		return size, nil
	}
	if r.cfg.LotSizeFallback > 0 {
		log.Printf("⚠️ no lot size for %s in instrument master or per-underlying config; using configured options_config.lot_size override %d", underlying, r.cfg.LotSizeFallback)
		return r.cfg.LotSizeFallback, nil
	}
	return 0, fmt.Errorf("no lot size available for %s", underlying)
}

func (r *Runner) strikeStep(underlying string) int {
	switch underlying {
	case "BANKNIFTY", "SENSEX":
		return 100
	case "MIDCPNIFTY":
		return 25
	case "NIFTY", "FINNIFTY":
		return 50
	}
	if r.cfg.StrikeStep > 0 {
		return r.cfg.StrikeStep
	}
	return 50
}

var knownIndices = []string{"NIFTY", "BANKNIFTY", "FINNIFTY", "SENSEX", "MIDCPNIFTY"}

func isKnownIndex(symbol string) bool {
	for _, idx := range knownIndices {
		if symbol == idx {
			return true
		}
	}
	return false
}

func looksLikeOptionSymbol(symbol string) bool {
	return strings.ContainsAny(symbol, "0123456789")
}

func extractIndexPrefix(symbol string) string {
	for _, idx := range knownIndices {
		if strings.HasPrefix(symbol, idx) {
			return idx
		}
	}
	return symbol
}

// buildOptionSymbol constructs an Angel One NFO option symbol:
// {INDEX}{DDMMMYY}{STRIKE}{CE/PE}, e.g. NIFTY16JAN2624500CE.
func buildOptionSymbol(index, expiry string, ltp float64, step int, offset int, optionType string) string {
	index = extractIndexPrefix(index)
	strike := math.Round(ltp/float64(step))*float64(step) + float64(offset)
	return fmt.Sprintf("%s%s%.0f%s", index, expiry, strike, optionType)
}

// ─────────────────────────────────────────────────────────────────────────
// Shutdown (task 12)
// ─────────────────────────────────────────────────────────────────────────

// Shutdown stops new entries, flattens all positions with retries
// (escalating backoff), and verifies the broker's position book is actually
// empty before returning nil. A non-nil return means the caller MUST exit
// non-zero — positions may still be open.
func (r *Runner) Shutdown() error {
	log.Println("🛑 SHUTDOWN — stopping new entries")
	r.paused.Store(true)

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		r.flattenAll("graceful shutdown")
		if r.brokerBookEmpty() {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("broker position book not empty after flatten attempt %d/%d", attempt, maxAttempts)
		if attempt < maxAttempts {
			backoff := time.Duration(attempt) * 2 * time.Second
			log.Printf("⚠️ %v — retrying in %s", lastErr, backoff)
			time.Sleep(backoff)
		}
	}

	if lastErr != nil {
		log.Printf("🔴 CRITICAL: shutdown could not verify a flat position book: %v", lastErr)
		if r.cfg.Alert != nil {
			r.cfg.Alert("flatten_failure", lastErr.Error())
		}
		return lastErr
	}

	stats := r.te.GetSessionStats()
	log.Printf("📊 FINAL SESSION STATS | Realized P&L: ₹%v | Balance: ₹%v", stats["realized_pnl"], stats["risk_balance"])
	log.Println("✅ Graceful shutdown complete — position book verified empty")
	return nil
}

func (r *Runner) brokerBookEmpty() bool {
	for _, p := range r.te.broker.GetPositions() {
		if p.Quantity != 0 {
			return false
		}
	}
	return len(r.te.riskManager.GetOpenPositions()) == 0
}
