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

	var grossWin, grossLoss float64
	byDay := map[string]float64{}
	byMonth := map[string]*MonthStat{}

	for _, t := range trades {
		r.GrossPnL += t.GrossPnL
		r.TotalCharges += t.Charges
		r.NetPnL += t.NetPnL

		if t.NetPnL > 0 {
			r.WinCount++
			grossWin += t.NetPnL
		} else {
			r.LossCount++
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

	r.grossWin, r.grossLoss = grossWin, grossLoss

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
	for _, m := range months {
		r.ByMonth = append(r.ByMonth, *byMonth[m])
	}

	return r
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
