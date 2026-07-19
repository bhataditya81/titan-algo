package backtest

import (
	"fmt"
	"math"
	"time"
)

// openLeg is one live option leg of an open position, priced via Black-Scholes.
type openLeg struct {
	Direction    LegDirection
	OptionType   string // "CE" / "PE"
	Strike       float64
	Expiry       time.Time
	EntryTime    time.Time
	EntryPremium float64 // per-share, post-slippage
	Quantity     int     // shares = lots * lot size
	EntryCharge  float64
	EntrySpread  float64
}

// openPosition is either a single synthetic directional leg (CR-10 fix) or
// a real multi-leg option strategy (short straddle, iron fly, ...).
type openPosition struct {
	Kind      string // "directional" or "multi-leg"
	DirSide   SignalAction
	Legs      []openLeg
	OpenedAt  time.Time
	EntrySpot float64
}

func combinedEntryPremium(p *openPosition) float64 {
	if p == nil {
		return 0
	}
	var sum float64
	for _, l := range p.Legs {
		sum += l.EntryPremium
	}
	return sum
}

// resolveExpiry parses OrderLeg.Expiry ("2006-01-02", "02Jan06" or
// "02-Jan-2006"); falls back to asOf + Config.DefaultDTEDays at 15:30 (NSE
// close) when empty or unparseable. Real expiry-calendar lookup (weekly
// Tuesday/monthly, holidays) is instrument-master territory — out of scope
// here, see WP-1's GetLotSize/GetExpiries follow-up note in the report.
func resolveExpiry(raw string, asOf time.Time, cfg Config) time.Time {
	layouts := []string{"2006-01-02", "02Jan06", "02-Jan-2006", "2Jan2006"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, asOf.Location()); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 15, 30, 0, 0, asOf.Location())
		}
	}
	fallback := asOf.AddDate(0, 0, cfg.DefaultDTEDays)
	return time.Date(fallback.Year(), fallback.Month(), fallback.Day(), 15, 30, 0, 0, asOf.Location())
}

func roundStrike(spot, step, offset float64) float64 {
	if step <= 0 {
		step = 1
	}
	atm := math.Round(spot/step) * step
	return atm + offset
}

func timeToExpiryYears(asOf, expiry time.Time) float64 {
	d := expiry.Sub(asOf).Hours() / 24.0
	if d < 0 {
		d = 0
	}
	return d / 365.0
}

// fillSlippage returns the price bump applied on execution: the larger of
// the percentage slippage and one tick (ST-7 default: 0.05% or min 1 tick).
func fillSlippage(theoretical float64, cfg Config) float64 {
	pct := theoretical * cfg.OptionSlippagePct
	if pct < cfg.OptionTickSize {
		return cfg.OptionTickSize
	}
	return pct
}

// priceLeg reprices one leg from scratch at a given spot/time (CR-9: full
// Black-Scholes repricing every candle, not a constant delta).
func priceLeg(kind string, strike float64, expiry, asOf time.Time, spot float64, cfg Config) float64 {
	k := CallOption
	if kind == "PE" {
		k = PutOption
	}
	return Price(BSParams{
		Spot:         spot,
		Strike:       strike,
		TimeToExpiry: timeToExpiryYears(asOf, expiry),
		Rate:         cfg.RiskFreeRate,
		Vol:          cfg.IV,
		Kind:         k,
	})
}

// openMultiLeg opens every declared leg, pricing/filling each at fillCandle.Open.
func openMultiLeg(legs []OrderLeg, fillCandle Candle, cfg Config) openPosition {
	pos := openPosition{Kind: "multi-leg", OpenedAt: fillCandle.Time, EntrySpot: fillCandle.Open}
	for _, leg := range legs {
		qtyLots := leg.Quantity
		if qtyLots <= 0 {
			qtyLots = 1
		}
		qty := qtyLots * cfg.LotSize
		strike := roundStrike(fillCandle.Open, cfg.StrikeStep, float64(leg.StrikeOffset))
		expiry := resolveExpiry(leg.Expiry, fillCandle.Time, cfg)
		theo := priceLeg(leg.OptionType, strike, expiry, fillCandle.Time, fillCandle.Open, cfg)
		slip := fillSlippage(theo, cfg)

		fillPrem := theo
		if leg.Direction == LegBuy {
			fillPrem = theo + slip
		} else {
			fillPrem = theo - slip
			if fillPrem < 0 {
				fillPrem = 0
			}
		}

		pos.Legs = append(pos.Legs, openLeg{
			Direction:    leg.Direction,
			OptionType:   leg.OptionType,
			Strike:       strike,
			Expiry:       expiry,
			EntryTime:    fillCandle.Time,
			EntryPremium: fillPrem,
			Quantity:     qty,
			EntryCharge:  LegCharge(fillPrem, qty, leg.Direction),
			EntrySpread:  SpreadCost(cfg.HalfSpreadPct, theo, qty),
		})
	}
	return pos
}

// openDirectional opens the single synthetic leg used for leg-less
// directional signals (CR-10): Buy -> long ATM CE, Sell -> long ATM PE.
// This keeps directional strategies flowing through the same Black-Scholes
// P&L model as multi-leg strategies rather than a second, separate model.
func openDirectional(action SignalAction, fillCandle Candle, cfg Config) openPosition {
	optType := "CE"
	if action == Sell {
		optType = "PE"
	}
	leg := OrderLeg{Direction: LegBuy, StrikeOffset: 0, OptionType: optType}
	pos := openMultiLeg([]OrderLeg{leg}, fillCandle, cfg)
	pos.Kind = "directional"
	pos.DirSide = action
	return pos
}

// closePosition reprices every leg at fillCandle.Open (ST-7 next-open
// fill), charges the closing side, and returns the completed Trade.
func closePosition(pos *openPosition, fillCandle Candle, cfg Config, reason string) Trade {
	var gross, charges float64
	for _, l := range pos.Legs {
		theo := priceLeg(l.OptionType, l.Strike, l.Expiry, fillCandle.Time, fillCandle.Open, cfg)
		slip := fillSlippage(theo, cfg)

		closeSide := LegSell
		if l.Direction == LegSell {
			closeSide = LegBuy
		}

		exitPrem := theo
		if closeSide == LegBuy { // buying back a short leg: pay more
			exitPrem = theo + slip
		} else { // selling a long leg: receive less
			exitPrem = theo - slip
			if exitPrem < 0 {
				exitPrem = 0
			}
		}

		var legPnL float64
		if l.Direction == LegBuy {
			legPnL = (exitPrem - l.EntryPremium) * float64(l.Quantity)
		} else {
			legPnL = (l.EntryPremium - exitPrem) * float64(l.Quantity)
		}
		gross += legPnL

		exitCharge := LegCharge(exitPrem, l.Quantity, closeSide)
		exitSpread := SpreadCost(cfg.HalfSpreadPct, theo, l.Quantity)
		charges += l.EntryCharge + l.EntrySpread + exitCharge + exitSpread
	}

	return Trade{
		Kind:      pos.Kind,
		OpenTime:  pos.OpenedAt,
		CloseTime: fillCandle.Time,
		Legs:      len(pos.Legs),
		EntrySpot: pos.EntrySpot,
		ExitSpot:  fillCandle.Open,
		GrossPnL:  gross,
		Charges:   charges,
		NetPnL:    gross - charges,
		Reason:    reason,
	}
}

// markToMarket reprices every open leg at the given spot/time for equity
// curve / drawdown purposes (unrealized only, no charges).
func markToMarket(pos *openPosition, spot float64, asOf time.Time, cfg Config) float64 {
	if pos == nil {
		return 0
	}
	var unrealized float64
	for _, l := range pos.Legs {
		cur := priceLeg(l.OptionType, l.Strike, l.Expiry, asOf, spot, cfg)
		if l.Direction == LegBuy {
			unrealized += (cur - l.EntryPremium) * float64(l.Quantity)
		} else {
			unrealized += (l.EntryPremium - cur) * float64(l.Quantity)
		}
	}
	return unrealized
}

// combinedCurrentPremium sums each leg's current (not P&L, raw) premium
// level. Per the WP-6 EvalContext contract ("the caller is responsible for
// feeding the live combined-premium series into Prices... for a symbol
// tracking an open option structure"), the engine feeds this series into
// EvalContext.Prices while a position is open, so premium-based stops
// (CR-14: nine_twenty / short_straddle) compare against the actual option
// structure's premium, not the underlying's spot price.
func combinedCurrentPremium(pos *openPosition, spot float64, asOf time.Time, cfg Config) float64 {
	var sum float64
	for _, l := range pos.Legs {
		sum += priceLeg(l.OptionType, l.Strike, l.Expiry, asOf, spot, cfg)
	}
	return sum
}

// Run drives the full simulation and returns the report.
//
// CR-10 fix: leg-less Buy/Sell signals now actually open a position when
// flat, and close on Exit or an opposite-direction signal — the old code's
// entry branch was only reachable for Legs-bearing signals.
// ST-7 fix: every fill happens at the NEXT candle's open (never the
// signal candle's close).
func Run(candles []Candle, strat Strategy, cfg Config) (*Report, error) {
	if cfg.MinHistory < 1 {
		cfg.MinHistory = 1
	}
	if len(candles) <= cfg.MinHistory+1 {
		return nil, fmt.Errorf("backtest: need more than %d candles (MinHistory+1), got %d", cfg.MinHistory+1, len(candles))
	}
	var pos *openPosition
	var trades []Trade
	var realized float64
	var peakEquity, maxDD float64
	var premiumHistory []float64 // combined premium series of the CURRENT open position only; reset on every open/close

	for i := cfg.MinHistory; i < len(candles)-1; i++ {
		evalCandle := candles[i]
		fillCandle := candles[i+1]

		// Mark-to-market using this candle's close, reflecting the
		// position as it stood after the fill applied at this candle's
		// open (i.e. the previous iteration's fillCandle == this candle).
		equity := realized + markToMarket(pos, evalCandle.Close, evalCandle.Time, cfg)
		if equity > peakEquity {
			peakEquity = equity
		}
		if dd := peakEquity - equity; dd > maxDD {
			maxDD = dd
		}

		history := candles[:i+1]
		ctx := EvalContext{
			Symbol:       cfg.Symbol,
			Candles:      history,
			Now:          evalCandle.Time,
			HasPosition:  pos != nil,
			EntryPremium: combinedEntryPremium(pos),
		}
		if pos != nil {
			ctx.PositionAge = evalCandle.Time.Sub(pos.OpenedAt)
			premiumHistory = append(premiumHistory, combinedCurrentPremium(pos, evalCandle.Close, evalCandle.Time, cfg))
			ctx.Prices = premiumHistory // see combinedCurrentPremium doc: premium series, not spot
		} else {
			ctx.Prices = closesOf(history)
			ctx.Volumes = volumesOf(history)
		}

		signal := strat.Evaluate(ctx)

		switch {
		case len(signal.Legs) > 0:
			// Multi-leg entries only taken while flat; a position-aware
			// strategy (WP-6 contract) shouldn't emit Legs while
			// HasPosition is true, so we don't churn on repeats.
			if pos == nil {
				opened := openMultiLeg(signal.Legs, fillCandle, cfg)
				pos = &opened
			}

		case signal.Action == Exit:
			if pos != nil {
				t := closePosition(pos, fillCandle, cfg, signal.Reason)
				trades = append(trades, t)
				realized += t.NetPnL
				pos = nil
				premiumHistory = nil
			}

		case signal.Action == Buy, signal.Action == Sell:
			switch {
			case pos == nil:
				opened := openDirectional(signal.Action, fillCandle, cfg)
				pos = &opened
			case pos.Kind == "directional" && pos.DirSide != signal.Action:
				// Opposite-direction signal: exit only (no same-candle
				// reverse — keeps behavior deterministic and matches
				// the literal CR-10 fix wording).
				t := closePosition(pos, fillCandle, cfg, "opposite-direction signal: "+signal.Reason)
				trades = append(trades, t)
				realized += t.NetPnL
				pos = nil
				premiumHistory = nil
			}
			// same-direction repeat while already in position: Hold (ST-5).

		default: // Hold
		}
	}

	// Force-close any still-open position at the last candle's close —
	// end of data, not a signal-driven exit.
	if pos != nil {
		last := candles[len(candles)-1]
		closingCandle := Candle{Time: last.Time, Open: last.Close, Close: last.Close}
		t := closePosition(pos, closingCandle, cfg, "end of data: force close")
		trades = append(trades, t)
		realized += t.NetPnL
		equity := realized
		if equity > peakEquity {
			peakEquity = equity
		}
		if dd := peakEquity - equity; dd > maxDD {
			maxDD = dd
		}
	}

	return buildReport(strat.Name(), candles, trades, maxDD), nil
}

func closesOf(cs []Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Close
	}
	return out
}

func volumesOf(cs []Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = float64(c.Volume)
	}
	return out
}
