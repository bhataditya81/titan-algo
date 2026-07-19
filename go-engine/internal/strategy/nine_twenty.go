package strategy

import (
	"fmt"
	"strconv"
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
type NineTwentyStrategy struct {
	EntryHour       int
	EntryMinute     int
	SquareOffHour   int
	SquareOffMinute int
	StopMultiplier  float64 // Combined-premium stop multiplier (default 1.4)

	mu sync.Mutex

	entered       bool      // true only after ConfirmEntry(); "we believe we hold a live position"
	pendingExit   bool      // true after an Exit signal was emitted but not yet confirmed via ConfirmExit()
	signaledEntry bool      // true after an entry signal was emitted but not yet confirmed via ConfirmEntry()
	lastDate      time.Time // IST calendar date of the last Evaluate call, for day-reset
	entryPremium  float64   // cached entry premium (kept in sync from ctx.EntryPremium)
}

// NewNineTwentyStrategy creates a new instance
func NewNineTwentyStrategy() *NineTwentyStrategy {
	return &NineTwentyStrategy{
		EntryHour:       9,
		EntryMinute:     20,
		SquareOffHour:   15,
		SquareOffMinute: 15,
		StopMultiplier:  1.4,
	}
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

	s.mu.Lock()
	defer s.mu.Unlock()

	// Full-date day reset (fixes the Day()-only bug: comparing just the
	// day-of-month wrongly treats e.g. Jan 1 and Feb 1 as "same day").
	if !sameISTDate(s.lastDate, nowIST) {
		s.entered = false
		s.pendingExit = false
		s.signaledEntry = false
		s.entryPremium = 0
		s.lastDate = nowIST
	}

	h, m := nowIST.Hour(), nowIST.Minute()

	// The engine's position book is authoritative over our own latch: if it
	// says we have a position (e.g. after a restart + reconcile), treat
	// ourselves as "in position" even if our local `entered` flag hasn't
	// been confirmed yet.
	inPosition := ctx.HasPosition || s.entered

	// 1. Entry window: only while flat and no entry signal is already
	// awaiting confirmation (avoids re-signaling every tick inside the
	// window before ConfirmEntry is called).
	if !inPosition {
		if !s.signaledEntry && h == s.EntryHour && m >= s.EntryMinute && m < s.EntryMinute+5 {
			s.signaledEntry = true
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
	if s.pendingExit {
		return Signal{Action: Hold, Reason: "Exit pending confirmation"}
	}

	// Keep the cached entry premium in sync with whatever the caller feeds.
	if ctx.EntryPremium > 0 {
		s.entryPremium = ctx.EntryPremium
	}

	// Combined-premium stop-loss (CR-14): current combined premium is read
	// from the last element of ctx.Prices/ctx.Candles — the caller is
	// responsible for feeding the live combined-premium series into this
	// symbol's context once the straddle is open (see EvalContext doc).
	if s.entryPremium > 0 {
		if cur, ok := ctx.LastPrice(); ok {
			stopLevel := s.entryPremium * s.effectiveMultiplier()
			if cur >= stopLevel {
				s.pendingExit = true
				return Signal{
					Action:   Exit,
					Strength: 1.0,
					Reason:   fmt.Sprintf("Premium stop hit: combined %.2f >= entry %.2f x %.2f", cur, s.entryPremium, s.effectiveMultiplier()),
				}
			}
		}
	}

	// Time-based square-off.
	if h > s.SquareOffHour || (h == s.SquareOffHour && m >= s.SquareOffMinute) {
		s.pendingExit = true
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

// ConfirmEntry flips the "entered" (believed-live-position) flag to true.
// The integration layer (WP-9) MUST call this only after the entry order's
// fill has actually been confirmed by the broker — never merely because an
// entry Signal was returned by Evaluate. This fixes ST-4: the old code
// flipped `entered=true` at signal-generation time, so a rejected order
// still left the strategy believing it held a position.
func (s *NineTwentyStrategy) ConfirmEntry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = true
	s.signaledEntry = false
}

// ConfirmExit flips the "entered" flag back to false after the exit order's
// fill has been confirmed. Symmetric with ConfirmEntry, for the same
// fill-confirmation-not-signal-generation reasoning; not explicitly named in
// the WP-6 spec but added because leaving `entered` cleared at Exit-signal
// time has the identical class of bug ST-4 called out for entries (a
// rejected exit order would otherwise silently mark the strategy flat while
// the broker still holds the position).
func (s *NineTwentyStrategy) ConfirmExit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entered = false
	s.pendingExit = false
	s.entryPremium = 0
}

// Snapshot returns the strategy's persistable state as a flat string map,
// suitable for WP-3's state.Store (SaveStrategyState/LoadStrategyState).
func (s *NineTwentyStrategy) Snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	lastDate := ""
	if !s.lastDate.IsZero() {
		lastDate = s.lastDate.Format("2006-01-02")
	}

	return map[string]string{
		"entered":        strconv.FormatBool(s.entered),
		"pending_exit":   strconv.FormatBool(s.pendingExit),
		"signaled_entry": strconv.FormatBool(s.signaledEntry),
		"last_date":      lastDate,
		"entry_premium":  strconv.FormatFloat(s.entryPremium, 'f', -1, 64),
	}
}

// Restore loads previously-saved state (from Snapshot) back into the
// strategy, e.g. at engine startup after WP-3's RecoverSession. Unknown or
// malformed keys are ignored (best-effort restore).
func (s *NineTwentyStrategy) Restore(state map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if v, ok := state["entered"]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			s.entered = b
		}
	}
	if v, ok := state["pending_exit"]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			s.pendingExit = b
		}
	}
	if v, ok := state["signaled_entry"]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			s.signaledEntry = b
		}
	}
	if v, ok := state["last_date"]; ok && v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, IST); err == nil {
			s.lastDate = t
		}
	}
	if v, ok := state["entry_premium"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.entryPremium = f
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
		return NewNineTwentyStrategy()
	})
}
