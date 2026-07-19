package backtest

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Trade is one completed round trip (entry->exit), directional or multi-leg.
type Trade struct {
	Kind      string // "directional" or "multi-leg"
	OpenTime  time.Time
	CloseTime time.Time
	Legs      int
	EntrySpot float64
	ExitSpot  float64
	GrossPnL  float64
	Charges   float64
	NetPnL    float64
	Reason    string
}

// MonthStat is one row of the per-month breakdown table (ST-10/M5).
type MonthStat struct {
	Month  string // "2026-01"
	Trades int
	NetPnL float64
	Wins   int
	Losses int
}

// Report is the full backtest output (ST-10/M5): trades, win rate,
// gross/net P&L, max drawdown, profit factor, expectancy, avg win/loss,
// worst single day, per-month breakdown.
type Report struct {
	StrategyName string
	From, To     time.Time
	Trades       []Trade
	GrossPnL     float64
	TotalCharges float64
	NetPnL       float64
	WinCount     int
	LossCount    int
	MaxDrawdown  float64
	WorstDay     float64 // most negative single calendar-day net P&L (0 if none negative)
	ByMonth      []MonthStat

	grossWin, grossLoss float64 // sum of winning/losing trade NetPnL magnitudes; feeds ProfitFactor/AvgWin/AvgLoss
}

func buildReport(name string, candles []Candle, trades []Trade, maxDD float64) *Report {
	r := &Report{StrategyName: name, Trades: trades, MaxDrawdown: maxDD}
	if len(candles) > 0 {
		r.From = candles[0].Time
		r.To = candles[len(candles)-1].Time
	}
	recomputeAggregates(r)
	return r
}

// recomputeAggregates derives every field that's purely a function of
// r.Trades (P&L totals, win/loss counts, profit-factor inputs, worst day,
// per-month table). Split out of buildReport (G-5/WP-10 cost-multiplier
// blocker) so ScaleCosts can rescale charges after a run and get a fully
// consistent report back, not just a hand-adjusted headline number.
func recomputeAggregates(r *Report) {
	var gross, charges, net, grossWin, grossLoss float64
	var winCount, lossCount int
	byDay := map[string]float64{}
	byMonth := map[string]*MonthStat{}

	for _, t := range r.Trades {
		gross += t.GrossPnL
		charges += t.Charges
		net += t.NetPnL

		if t.NetPnL > 0 {
			winCount++
			grossWin += t.NetPnL
		} else {
			lossCount++
			grossLoss += -t.NetPnL
		}

		day := t.CloseTime.Format("2006-01-02")
		byDay[day] += t.NetPnL

		month := t.CloseTime.Format("2006-01")
		ms, ok := byMonth[month]
		if !ok {
			ms = &MonthStat{Month: month}
			byMonth[month] = ms
		}
		ms.Trades++
		ms.NetPnL += t.NetPnL
		if t.NetPnL > 0 {
			ms.Wins++
		} else {
			ms.Losses++
		}
	}

	r.GrossPnL, r.TotalCharges, r.NetPnL = gross, charges, net
	r.WinCount, r.LossCount = winCount, lossCount
	r.grossWin, r.grossLoss = grossWin, grossLoss

	r.WorstDay = 0
	for _, pnl := range byDay {
		if pnl < r.WorstDay {
			r.WorstDay = pnl
		}
	}

	months := make([]string, 0, len(byMonth))
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	r.ByMonth = r.ByMonth[:0]
	for _, m := range months {
		r.ByMonth = append(r.ByMonth, *byMonth[m])
	}
}

// ScaleCosts rescales every trade's Charges (and therefore NetPnL) by mult
// and recomputes all aggregate report fields -- G-5's -cost-multiplier flag,
// replacing WP-10's manual "recompute expectancy by hand outside the tool"
// workaround with a real, consistent rescale (win/loss can flip when a thin
// winner is swallowed by 2x costs; the old manual-arithmetic approach could
// not do that, only this can). mult == 1.0 or a nil report is a no-op.
func ScaleCosts(r *Report, mult float64) {
	if r == nil || mult == 1.0 {
		return
	}
	for i := range r.Trades {
		r.Trades[i].Charges *= mult
		r.Trades[i].NetPnL = r.Trades[i].GrossPnL - r.Trades[i].Charges
	}
	recomputeAggregates(r)
}

// ConstantIVBanner is the mandatory, impossible-to-miss caveat (G-4) printed
// at the top of every backtest report this round. It is unconditional: the
// per-bar implied-IV series this package can now compute (see ImpliedVol /
// BuildIVSeries in iv.go) cannot be wired into engine.go's priceLeg this
// round because engine.go's EvalContext-construction section is owned by
// R2-2 this cycle (file-ownership rule) -- so Run() ALWAYS reprices legs
// under the single constant iv, regardless of whether option-candle data is
// supplied. See docs/reports/R2-3-REPORT.md for the exact one-line change
// (swap `Vol: cfg.IV` for a per-bar lookup in priceLeg) needed once engine.go
// is open to R2-3/R2-INT again.
func ConstantIVBanner(iv float64) string {
	bar := strings.Repeat("!", 70)
	return fmt.Sprintf(
		"%s\nCONSTANT-IV MODE -- vega/skew risk NOT modeled. Every option leg\n"+
			"below is repriced all run at a single fixed IV = %.2f%%. Real IV\n"+
			"moves with strikes/expiry/time and spikes exactly when short-vol\n"+
			"strategies are losing. Any short_straddle/iron_fly/nine_twenty\n"+
			"result here is an artifact of the assumed IV level, NOT a\n"+
			"demonstrated edge -- see WP-10's IV-sensitivity finding (PF\n"+
			"0.29->1.66 on IV alone) and docs/reports/R2-3-REPORT.md.\n%s",
		bar, iv*100, bar)
}

func (r *Report) WinRate() float64 {
	n := r.WinCount + r.LossCount
	if n == 0 {
		return 0
	}
	return float64(r.WinCount) / float64(n) * 100.0
}

func (r *Report) ProfitFactor() float64 {
	win, loss := r.grossWin, r.grossLoss
	if loss == 0 {
		if win == 0 {
			return 0
		}
		return win // undefined/infinite in theory; report gross win as a large finite proxy
	}
	return win / loss
}

func (r *Report) Expectancy() float64 {
	n := len(r.Trades)
	if n == 0 {
		return 0
	}
	return r.NetPnL / float64(n)
}

func (r *Report) AvgWin() float64 {
	if r.WinCount == 0 {
		return 0
	}
	return r.grossWin / float64(r.WinCount)
}

func (r *Report) AvgLoss() float64 {
	if r.LossCount == 0 {
		return 0
	}
	return -(r.grossLoss / float64(r.LossCount))
}

func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=====================================================\n")
	fmt.Fprintf(&b, "BACKTEST REPORT: %s\n", r.StrategyName)
	fmt.Fprintf(&b, "Period: %s -> %s\n", r.From.Format("2006-01-02"), r.To.Format("2006-01-02"))
	fmt.Fprintf(&b, "=====================================================\n")
	fmt.Fprintf(&b, "Total Trades:   %d (Wins: %d | Losses: %d)\n", len(r.Trades), r.WinCount, r.LossCount)
	fmt.Fprintf(&b, "Win Rate:       %.2f%%\n", r.WinRate())
	fmt.Fprintf(&b, "Gross P&L:      Rs. %.2f\n", r.GrossPnL)
	fmt.Fprintf(&b, "Total Charges:  Rs. %.2f\n", r.TotalCharges)
	fmt.Fprintf(&b, "Net P&L:        Rs. %.2f\n", r.NetPnL)
	fmt.Fprintf(&b, "Max Drawdown:   Rs. %.2f\n", r.MaxDrawdown)
	fmt.Fprintf(&b, "Profit Factor:  %.2f\n", r.ProfitFactor())
	fmt.Fprintf(&b, "Expectancy:     Rs. %.2f / trade\n", r.Expectancy())
	fmt.Fprintf(&b, "Avg Win:        Rs. %.2f\n", r.AvgWin())
	fmt.Fprintf(&b, "Avg Loss:       Rs. %.2f\n", r.AvgLoss())
	fmt.Fprintf(&b, "Worst Day:      Rs. %.2f\n", r.WorstDay)
	fmt.Fprintf(&b, "-----------------------------------------------------\n")
	fmt.Fprintf(&b, "Per-Month Breakdown:\n")
	fmt.Fprintf(&b, "%-8s %8s %8s %8s %14s\n", "Month", "Trades", "Wins", "Losses", "Net P&L")
	for _, m := range r.ByMonth {
		fmt.Fprintf(&b, "%-8s %8d %8d %8d %14.2f\n", m.Month, m.Trades, m.Wins, m.Losses, m.NetPnL)
	}
	fmt.Fprintf(&b, "=====================================================\n")
	return b.String()
}
