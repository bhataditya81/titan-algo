package state

import (
	"fmt"

	"titan-algo/models"
)

// RecoverSession loads whatever state a prior process left behind: all
// currently-open positions and the last saved risk snapshot. This is the
// first thing the integration layer (WP-9) should call on startup, before
// connecting to the broker or placing any order — the audit's CR-8 fix
// requires that persisted state be loaded and reconciled against the
// broker's live position book before trading resumes.
//
// If the store has never had a risk snapshot saved (fresh install), the
// returned RiskSnapshot is the zero value and no error is returned — this
// is a normal "first run" condition, not a failure.
//
// RecoverSession does not talk to the broker and performs no
// reconciliation itself; callers should follow it with a broker position
// fetch and a call to Reconcile.
func RecoverSession(store *Store) (openPositions []models.Position, riskSnapshot models.RiskSnapshot, err error) {
	openPositions, err = store.ListOpenPositions()
	if err != nil {
		return nil, models.RiskSnapshot{}, fmt.Errorf("state: recover session: list open positions: %w", err)
	}

	riskSnapshot, err = store.LoadRiskSnapshot()
	if err != nil {
		return nil, models.RiskSnapshot{}, fmt.Errorf("state: recover session: load risk snapshot: %w", err)
	}

	return openPositions, riskSnapshot, nil
}
