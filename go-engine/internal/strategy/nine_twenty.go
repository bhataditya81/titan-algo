package strategy

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NineTwentyStrategy implements the famous time-based Short Straddle:
// sell an ATM straddle shortly after the open, square off before close.
//
// Fixes applied (WP-6):
//   - ST-3: all clock logic converts through EvalContext.Now.In(IST); no
//     more server-local wall-clock comparisons.
//   - ST-4: the "entered" flag (believed-live-position) now flips true ONLY
//     via the explicit ConfirmEntry() method, which the integration layer
//     calls after a fill is actually confirmed — never merely on signal
//     generation. This also fixes the day-reset bug: the old code compared
//     only currentTime.Day() (1-31), which incorrectly treats the 1st of
//     every month as "same day" as any other day-of-month 1..31 seen
//     before; it now compares full IST calendar dates.
//   - CR-14: added a combined-premium stop-loss — exit when current combined
//     premium >= entry premium * StopMultiplier (default 1.4). Previously
//     this strategy had NO stop-loss of any kind.
//   - Exposes Snapshot()/Restore() so the integration layer can persist and
//     recover the entered/pending state across restarts (WP-3).
//
// All per-position fields are keyed by ctx.Symbol / the ConfirmEntry(symbol)
// / ConfirmExit(symbol) argument (audit R3): registry.Get caches one shared
// singleton per strategy name, and runner.go's tick loop calls Evaluate for
// every configured symbol against that same instance (a real,
// discovery-driven multi-symbol feature, e.g. trading NIFTY and BANKNIFTY
// straddles together). Un-keyed scalar fields let one symbol's confirmed
// entry silently block every other symbol from ever entering for the rest
// of the day.
type NineTwentyStrategy struct {
	EntryHour       int
	EntryMinute     int
	SquareOffHour   int
	SquareOffMinute int
	StopMultiplier  float64 // Combined-premium stop multiplier (default 1.4)

	mu sync.Mutex

	entered       map[string]bool      // symbol -> true only after ConfirmEntry(symbol); "we believe we hold a live position"
	pendingExit   map[string]bool      // symbol -> true after an Exit signal was emitted but not yet confirmed via ConfirmExit(symbol)
	signaledEntry map[string]bool      // symbol -> true after an entry signal was emitted but not yet confirmed via ConfirmEntry(symbol)
	lastDate      map[string]time.Time // symbol -> IST calendar date of the last Evaluate call, for day-reset
	entryPremium  map[string]float64   // symbol -> cached entry premium (kept in sync from ctx.EntryPremium)
}

// NineTwentyParams configures NewNineTwenty. The zero value produces today's
// hardcoded defaults (EntryHour=9, EntryMinute=20, SquareOffHour=15,
// SquareOffMinute=15, StopMultiplier=1.4) — this is what
// registry.Get("nine_twenty") constructs internally, so existing callers are
// unaffected by this parameterization (G-5). Note: a zero value in any
// field always means "use the default", so e.g. EntryMinute=0 or
// SquareOffMinute=0 cannot be selected explicitly — an accepted limitation
// of the zero-value-means-default options pattern.
type NineTwentyParams struct {
	EntryHour       int
	EntryMinute     int
	SquareOffHour   int
	SquareOffMinute int
	StopMultiplier  float64
}

// NewNineTwenty builds a NineTwentyStrategy from params, substituting the
// hardcoded default for any zero-valued field.
func NewNineTwenty(p NineTwentyParams) *NineTwentyStrategy {
	if p.EntryHour == 0 {
		p.EntryHour = 9
	}
	if p.EntryMinute == 0 {
		p.EntryMinute = 20
	}
	if p.SquareOffHour == 0 {
		p.SquareOffHour = 15
	}
	if p.SquareOffMinute == 0 {
		p.SquareOffMinute = 15
	}
	if p.StopMultiplier == 0 {
		p.StopMultiplier = 1.4
	}
	return &NineTwentyStrategy{
		EntryHour:       p.EntryHour,
		EntryMinute:     p.EntryMinute,
		SquareOffHour:   p.SquareOffHour,
		SquareOffMinute: p.SquareOffMinute,
		StopMultiplier:  p.StopMultiplier,
		entered:         make(map[string]bool),
		pendingExit:     make(map[string]bool),
		signaledEntry:   make(map[string]bool),
		lastDate:        make(map[string]time.Time),
		entryPremium:    make(map[string]float64),
	}
}

// NewNineTwentyStrategy creates a new instance. Preserved for existing
// callers; equivalent to NewNineTwenty(NineTwentyParams{}).
func NewNineTwentyStrategy() *NineTwentyStrategy {
	return NewNineTwenty(NineTwentyParams{})
}

func (s *NineTwentyStrategy) Name() string {
	return "9:20 Straddle"
}

func (s *NineTwentyStrategy) Evaluate(ctx EvalContext) Signal {
	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	nowIST := now.In(IST)

	sym := ctx.Symbol

	s.mu.Lock()
	defer s.mu.Unlock()

	// Full-date day reset (fixes the Day()-only bug: comparing just the
	// day-of-month wrongly treats e.g. Jan 1 and Feb 1 as "same day").
	if !sameISTDate(s.lastDate[sym], nowIST) {
		s.entered[sym] = false
		s.pendingExit[sym] = false
		s.signaledEntry[sym] = false
		s.entryPremium[sym] = 0
		s.lastDate[sym] = nowIST
	}

	h, m := nowIST.Hour(), nowIST.Minute()

	// The engine's position book is authoritative over our own latch: if it
	// says we have a position (e.g. after a restart + reconcile), treat
	// ourselves as "in position" even if our local `entered` flag hasn't
	// been confirmed yet.
	inPosition := ctx.HasPosition || s.entered[sym]

	// 1. Entry window: only while flat and no entry signal is already
	// awaiting confirmation (avoids re-signaling every tick inside the
	// window before ConfirmEntry is called).
	if !inPosition {
		if !s.signaledEntry[sym] && h == s.EntryHour && m >= s.EntryMinute && m < s.EntryMinute+5 {
			s.signaledEntry[sym] = true
			return Signal{
				Action:   Sell, // Short Strategy
				Strength: 1.0,
				Reason:   "9:20 Straddle Trigger",
				Legs: []OrderLeg{
					{Direction: LegSell, StrikeOffset: 0, OptionType: "CE", Quantity: 1},
					{Direction: LegSell, StrikeOffset: 0, OptionType: "PE", Quantity: 1},
				},
			}
		}
		return Signal{Action: Hold, Reason: fmt.Sprintf("Waiting for %02d:%02d IST (Current: %02d:%02d IST)", s.EntryHour, s.EntryMinute, h, m)}
	}

	// 2. In position: monitor for exit. Don't re-signal Exit every tick
	// while a prior exit is awaiting confirmation.
	if s.pendingExit[sym] {
		return Signal{Action: Hold, Reason: "Exit pending confirmation"}
	}

	// Keep the cached entry premium in sync with whatever the caller feeds.
	if ctx.EntryPremium > 0 {
		s.entryPremium[sym] = ctx.EntryPremium
	}

	// Combined-premium stop-loss (CR-14): current combined premium is read
	// from the last element of ctx.Prices/ctx.Candles — the caller is
	// responsible for feeding the live combined-premium series into this
	// symbol's context once the straddle is open (see EvalContext doc).
	if s.entryPremium[sym] > 0 {
		if cur, ok := ctx.LastPrice(); ok {
			stopLevel := s.entryPremium[sym] * s.effectiveMultiplier()
			if cur >= stopLevel {
				s.pendingExit[sym] = true
				return Signal{
					Action:   Exit,
					Strength: 1.0,
					Reason:   fmt.Sprintf("Premium stop hit: combined %.2f >= entry %.2f x %.2f", cur, s.entryPremium[sym], s.effectiveMultiplier()),
				}
			}
		}
	}

	// Time-based square-off.
	if h > s.SquareOffHour || (h == s.SquareOffHour && m >= s.SquareOffMinute) {
		s.pendingExit[sym] = true
		return Signal{
			Action:   Exit,
			Reason:   "Intraday Squareoff",
			Strength: 1.0,
		}
	}

	return Signal{Action: Hold, Reason: fmt.Sprintf("Holding straddle (Current: %02d:%02d IST)", h, m)}
}

func (s *NineTwentyStrategy) effectiveMultiplier() float64 {
	if s.StopMultiplier <= 0 {
		return 1.4
	}
	return s.StopMultiplier
}

// ConfirmEntry flips the "entered" (believed-live-position) flag to true for
// symbol. The integration layer (WP-9) MUST call this only after the entry
// order's fill has actually been confirmed by the broker — never merely
// because an entry Signal was returned by Evaluate. This fixes ST-4: the old
// code flipped `entered=true` at signal-generation time, so a rejected order
// still left the strategy believing it held a position.
func (s *NineTwentyStrategy) ConfirmEntry(symbol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered[symbol] = true
	s.signaledEntry[symbol] = false
}

// ConfirmExit flips the "entered" flag back to false for symbol after the
// exit order's fill has been confirmed. Symmetric with ConfirmEntry, for the
// same fill-confirmation-not-signal-generation reasoning; not explicitly
// named in the WP-6 spec but added because leaving `entered` cleared at
// Exit-signal time has the identical class of bug ST-4 called out for
// entries (a rejected exit order would otherwise silently mark the strategy
// flat while the broker still holds the position).
func (s *NineTwentyStrategy) ConfirmExit(symbol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered[symbol] = false
	s.pendingExit[symbol] = false
	s.entryPremium[symbol] = 0
}

// snapshotKeySep separates the symbol from the field name in Snapshot/Restore
// keys (e.g. "NIFTY|entered") -- state.Store treats these as opaque strings
// under its "strat:" prefix, so encoding the symbol here needs no store change.
const snapshotKeySep = "|"

// Snapshot returns the strategy's persistable state as a flat string map,
// suitable for WP-3's state.Store (SaveStrategyState/LoadStrategyState). One
// entry per (symbol, field) pair, across every symbol this instance has ever
// evaluated.
func (s *NineTwentyStrategy) Snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]string, len(s.entered)*5)
	for sym := range s.entered {
		out[sym+snapshotKeySep+"entered"] = strconv.FormatBool(s.entered[sym])
		out[sym+snapshotKeySep+"pending_exit"] = strconv.FormatBool(s.pendingExit[sym])
		out[sym+snapshotKeySep+"signaled_entry"] = strconv.FormatBool(s.signaledEntry[sym])
		lastDate := ""
		if !s.lastDate[sym].IsZero() {
			lastDate = s.lastDate[sym].Format("2006-01-02")
		}
		out[sym+snapshotKeySep+"last_date"] = lastDate
		out[sym+snapshotKeySep+"entry_premium"] = strconv.FormatFloat(s.entryPremium[sym], 'f', -1, 64)
	}
	return out
}

// Restore loads previously-saved state (from Snapshot) back into the
// strategy, e.g. at engine startup after WP-3's RecoverSession. Unknown or
// malformed keys are ignored (best-effort restore).
func (s *NineTwentyStrategy) Restore(state map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, v := range state {
		sym, field, found := strings.Cut(key, snapshotKeySep)
		if !found {
			continue // pre-multi-symbol snapshot format; nothing safe to restore per-symbol
		}
		switch field {
		case "entered":
			if b, err := strconv.ParseBool(v); err == nil {
				s.entered[sym] = b
			}
		case "pending_exit":
			if b, err := strconv.ParseBool(v); err == nil {
				s.pendingExit[sym] = b
			}
		case "signaled_entry":
			if b, err := strconv.ParseBool(v); err == nil {
				s.signaledEntry[sym] = b
			}
		case "last_date":
			if v != "" {
				if t, err := time.ParseInLocation("2006-01-02", v, IST); err == nil {
					s.lastDate[sym] = t
				}
			}
		case "entry_premium":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				s.entryPremium[sym] = f
			}
		}
	}
}

// sameISTDate reports whether a and b fall on the same IST calendar date.
// Returns false if a is the zero value (forces a reset on first use).
func sameISTDate(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func init() {
	Register("nine_twenty", func() Strategy {
		return NewNineTwenty(NineTwentyParams{})
	})
	RegisterParams("nine_twenty", func(params map[string]float64) (Strategy, error) {
		p := NineTwentyParams{}
		if err := applyParams(&p, params); err != nil {
			return nil, fmt.Errorf("nine_twenty: %w", err)
		}
		return NewNineTwenty(p), nil
	})
}
