package state

import "titan-algo/models"

// MismatchType classifies how an internally-tracked position compares
// against what the broker reports for the same symbol.
type MismatchType string

const (
	// Matched means both sides agree on symbol, side, and quantity.
	Matched MismatchType = "matched"
	// Phantom means the position exists internally but the broker has no
	// record of it for that symbol. This means our books show a position
	// that isn't real at the exchange — typically an internal position
	// that was never actually filled, or was closed at the broker without
	// our knowledge.
	Phantom MismatchType = "phantom"
	// Orphan means the broker reports a position for a symbol we have no
	// internal record of at all — this is the dangerous "unmonitored
	// option position, no stop-loss, invisible until the broker app is
	// opened" scenario from audit finding CR-7.
	Orphan MismatchType = "orphan"
	// QuantityMismatch means both sides have a position in the symbol but
	// the net signed quantity (positive for BUY/long, negative for
	// SELL/short) disagrees — e.g. a partial fill that was never
	// reconciled (CR-6/CR-7).
	QuantityMismatch MismatchType = "quantity_mismatch"
)

// NetQuantity returns a signed quantity for a position: positive for a
// BUY/long position, negative for a SELL/short position. Reconcile compares
// this signed value across internal vs broker positions so that a side
// flip (e.g. internal says long 75, broker says short 75) is caught as a
// mismatch rather than silently ignored.
func NetQuantity(p models.Position) int {
	if p.Side == models.SideSell {
		return -p.Quantity
	}
	return p.Quantity
}

// ReconcileItem describes one symbol's classification result: how it
// looked internally vs how it looked at the broker, and which bucket it
// fell into.
type ReconcileItem struct {
	Symbol   string
	Type     MismatchType
	Internal *models.Position // nil if the symbol has no internal record (Orphan)
	Broker   *models.Position // nil if the symbol has no broker record (Phantom)

	// InternalNetQty / BrokerNetQty are the signed quantities compared
	// (positive = long, negative = short); populated whenever the
	// respective side is present, 0 otherwise.
	InternalNetQty int
	BrokerNetQty   int
}

// ReconcileReport is the full result of comparing internal vs broker
// position books, bucketed by classification. It is produced entirely by
// Reconcile, a pure function with no I/O — the caller (WP-9's integration
// layer) is responsible for fetching both position slices and for acting
// on the report (alerting, refusing to trade, flattening, etc).
type ReconcileReport struct {
	Matched            []ReconcileItem
	Phantoms           []ReconcileItem // internal only, missing at broker
	Orphans            []ReconcileItem // broker only, missing internally
	QuantityMismatches []ReconcileItem
}

// Clean reports whether the reconciliation found no discrepancies at all
// (no phantoms, orphans, or quantity mismatches). A caller should refuse to
// trade (or require an explicit operator override) whenever Clean() is
// false — see Appendix B's fail-closed principle and CR-7/CR-8.
func (r ReconcileReport) Clean() bool {
	return len(r.Phantoms) == 0 && len(r.Orphans) == 0 && len(r.QuantityMismatches) == 0
}

// Reconcile is a pure, side-effect-free comparison of internally-tracked
// open positions against the broker's reported open positions, keyed by
// Symbol. It performs no I/O — callers (the WP-9 integration layer) supply
// both slices from their own sources (internal via Store.ListOpenPositions,
// broker via the broker's GetPositions/position-fetch call).
//
// Positions are compared by their NetQuantity (signed by side) per symbol.
// If multiple internal or broker entries share the same symbol, their net
// quantities are summed before comparison (this tolerates callers that
// represent a symbol's position as several partial-fill records).
//
// Classification per symbol:
//   - present both sides, net quantities equal      -> Matched
//   - present internally only                        -> Phantom
//   - present at broker only                          -> Orphan
//   - present both sides, net quantities differ       -> QuantityMismatch
func Reconcile(internalPositions []models.Position, brokerPositions []models.Position) ReconcileReport {
	type agg struct {
		netQty int
		sample models.Position
		seen   bool
	}

	internal := make(map[string]*agg)
	order := []string{} // preserve first-seen symbol order for deterministic output

	touch := func(m map[string]*agg, p models.Position) {
		a, ok := m[p.Symbol]
		if !ok {
			a = &agg{}
			m[p.Symbol] = a
			order = append(order, p.Symbol)
		}
		a.netQty += NetQuantity(p)
		a.sample = p
		a.seen = true
	}

	for _, p := range internalPositions {
		touch(internal, p)
	}

	broker := make(map[string]*agg)
	// Track broker-only symbols in their own arrival order too, but append
	// after internal ones for determinism; use a combined ordered symbol
	// list instead so no symbol is ever skipped or duplicated.
	brokerOrder := []string{}
	for _, p := range brokerPositions {
		a, ok := broker[p.Symbol]
		if !ok {
			a = &agg{}
			broker[p.Symbol] = a
			brokerOrder = append(brokerOrder, p.Symbol)
		}
		a.netQty += NetQuantity(p)
		a.sample = p
		a.seen = true
	}

	// Build a deterministic, de-duplicated symbol list: internal-first
	// order, then any broker-only symbols not already present.
	seenSymbol := make(map[string]bool, len(order)+len(brokerOrder))
	symbols := make([]string, 0, len(order)+len(brokerOrder))
	for _, sym := range order {
		if !seenSymbol[sym] {
			seenSymbol[sym] = true
			symbols = append(symbols, sym)
		}
	}
	for _, sym := range brokerOrder {
		if !seenSymbol[sym] {
			seenSymbol[sym] = true
			symbols = append(symbols, sym)
		}
	}

	var report ReconcileReport
	for _, sym := range symbols {
		iAgg, hasInternal := internal[sym]
		bAgg, hasBroker := broker[sym]

		item := ReconcileItem{Symbol: sym}
		if hasInternal {
			p := iAgg.sample
			item.Internal = &p
			item.InternalNetQty = iAgg.netQty
		}
		if hasBroker {
			p := bAgg.sample
			item.Broker = &p
			item.BrokerNetQty = bAgg.netQty
		}

		switch {
		case hasInternal && !hasBroker:
			item.Type = Phantom
			report.Phantoms = append(report.Phantoms, item)
		case !hasInternal && hasBroker:
			item.Type = Orphan
			report.Orphans = append(report.Orphans, item)
		case hasInternal && hasBroker && iAgg.netQty == bAgg.netQty:
			item.Type = Matched
			report.Matched = append(report.Matched, item)
		default: // hasInternal && hasBroker && netQty differs
			item.Type = QuantityMismatch
			report.QuantityMismatches = append(report.QuantityMismatches, item)
		}
	}

	return report
}
