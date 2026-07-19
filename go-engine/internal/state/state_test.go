package state

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"titan-algo/models"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "titan_state.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dbPath
}

const epsilon = 1e-9

func floatEq(a, b float64) bool {
	d := a - b
	return d < epsilon && d > -epsilon
}

// ---- crash simulation -------------------------------------------------------

// TestCrashRecovery writes positions, order attempts, a risk snapshot and
// strategy state, closes the store (simulating a process death after the
// last committed write), reopens a NEW Store against the same file, and
// verifies everything is intact.
func TestCrashRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "titan_state.db")

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Positions: two open, one closed.
	posA := models.Position{
		ID: "pos-a", Symbol: "NIFTY26FEB24000CE", Exchange: "NFO",
		Side: models.SideSell, Quantity: 75, EntryPrice: 152.5,
		Strategy: "nine_twenty", EntryTime: time.Date(2026, 2, 10, 3, 50, 0, 0, time.UTC),
		BrokerOrderID: "B123",
	}
	posB := models.Position{
		ID: "pos-b", Symbol: "NIFTY26FEB24000PE", Exchange: "NFO",
		Side: models.SideSell, Quantity: 75, EntryPrice: 148.0,
		Strategy: "nine_twenty", EntryTime: time.Date(2026, 2, 10, 3, 50, 5, 0, time.UTC),
	}
	posC := models.Position{
		ID: "pos-c", Symbol: "BANKNIFTY26FEB51000CE", Exchange: "NFO",
		Side: models.SideBuy, Quantity: 30, EntryPrice: 210.0,
		Strategy: "sniper", EntryTime: time.Date(2026, 2, 10, 4, 0, 0, 0, time.UTC),
	}
	for _, p := range []models.Position{posA, posB, posC} {
		if err := s1.SavePosition(p); err != nil {
			t.Fatalf("SavePosition(%s): %v", p.ID, err)
		}
	}
	if err := s1.ClosePosition("pos-c", 250.0, time.Date(2026, 2, 10, 6, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}

	// Order attempts: one resolved, one left as intent (simulates crash
	// mid-flight between SaveOrderAttempt and MarkOrderResolved).
	coid1 := s1.NextClientOrderID()
	coid2 := s1.NextClientOrderID()
	if err := s1.SaveOrderAttempt(models.OrderAttempt{
		ClientOrderID: coid1, Symbol: posA.Symbol, Side: models.SideSell,
		Quantity: 75, OrderType: "MARKET", Strategy: "nine_twenty",
	}); err != nil {
		t.Fatalf("SaveOrderAttempt: %v", err)
	}
	if err := s1.MarkOrderResolved(coid1, models.OrderFilled, "B123", 75, 152.5, "", time.Time{}); err != nil {
		t.Fatalf("MarkOrderResolved: %v", err)
	}
	if err := s1.SaveOrderAttempt(models.OrderAttempt{
		ClientOrderID: coid2, Symbol: posB.Symbol, Side: models.SideSell,
		Quantity: 75, OrderType: "MARKET", Strategy: "nine_twenty",
	}); err != nil {
		t.Fatalf("SaveOrderAttempt: %v", err)
	}

	// Risk snapshot + strategy state.
	if err := s1.SaveRiskSnapshot(9500.25, -499.75, 3200.0); err != nil {
		t.Fatalf("SaveRiskSnapshot: %v", err)
	}
	if err := s1.SaveStrategyState("nine_twenty", "entered", "true"); err != nil {
		t.Fatalf("SaveStrategyState: %v", err)
	}
	if err := s1.SaveStrategyState("nine_twenty", "entry_premium", "300.5"); err != nil {
		t.Fatalf("SaveStrategyState: %v", err)
	}

	// "Crash": close the store, reopen fresh against the same file.
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-Open after crash: %v", err)
	}
	defer s2.Close()

	// Open positions intact (closed one excluded).
	open, snap, err := RecoverSession(s2)
	if err != nil {
		t.Fatalf("RecoverSession: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("open positions after recovery = %d, want 2", len(open))
	}
	byID := map[string]models.Position{}
	for _, p := range open {
		byID[p.ID] = p
	}
	gotA, ok := byID["pos-a"]
	if !ok {
		t.Fatalf("pos-a missing after recovery")
	}
	if gotA.Symbol != posA.Symbol || gotA.Side != posA.Side || gotA.Quantity != posA.Quantity ||
		!floatEq(gotA.EntryPrice, posA.EntryPrice) || gotA.Strategy != posA.Strategy ||
		gotA.BrokerOrderID != posA.BrokerOrderID || !gotA.EntryTime.Equal(posA.EntryTime) ||
		gotA.Status != models.PositionOpen {
		t.Errorf("pos-a corrupted after recovery:\n got  %+v\n want %+v", gotA, posA)
	}
	if _, ok := byID["pos-b"]; !ok {
		t.Errorf("pos-b missing after recovery")
	}
	if _, ok := byID["pos-c"]; ok {
		t.Errorf("pos-c is closed but returned as open")
	}

	// Risk snapshot intact.
	if !floatEq(snap.Balance, 9500.25) || !floatEq(snap.RealizedPnL, -499.75) || !floatEq(snap.SessionUsed, 3200.0) {
		t.Errorf("risk snapshot corrupted: %+v", snap)
	}
	if snap.UpdatedAt.IsZero() {
		t.Errorf("risk snapshot UpdatedAt is zero")
	}

	// Order attempts: resolved one has its resolution, unresolved intent survives.
	a1, found, err := s2.GetOrderAttempt(coid1)
	if err != nil || !found {
		t.Fatalf("GetOrderAttempt(%q): found=%v err=%v", coid1, found, err)
	}
	if a1.Status != models.OrderFilled || a1.BrokerOrderID != "B123" || a1.FilledQuantity != 75 || !floatEq(a1.FilledPrice, 152.5) {
		t.Errorf("resolved attempt corrupted: %+v", a1)
	}
	if a1.ResolvedAt.IsZero() {
		t.Errorf("resolved attempt has zero ResolvedAt")
	}

	unresolved, err := s2.ListUnresolvedOrderAttempts()
	if err != nil {
		t.Fatalf("ListUnresolvedOrderAttempts: %v", err)
	}
	if len(unresolved) != 1 || unresolved[0].ClientOrderID != coid2 {
		t.Fatalf("unresolved attempts = %+v, want exactly [%s]", unresolved, coid2)
	}
	if unresolved[0].Status != models.OrderIntent {
		t.Errorf("unresolved attempt status = %q, want %q", unresolved[0].Status, models.OrderIntent)
	}

	// Strategy state intact.
	v, found, err := s2.LoadStrategyState("nine_twenty", "entered")
	if err != nil || !found || v != "true" {
		t.Errorf("LoadStrategyState(entered) = (%q, %v, %v), want (true, true, nil)", v, found, err)
	}
	all, err := s2.LoadAllStrategyState("nine_twenty")
	if err != nil {
		t.Fatalf("LoadAllStrategyState: %v", err)
	}
	if len(all) != 2 || all["entered"] != "true" || all["entry_premium"] != "300.5" {
		t.Errorf("LoadAllStrategyState = %v", all)
	}
}

// ---- store behavior ---------------------------------------------------------

func TestSavePositionUpsertAndDefaults(t *testing.T) {
	s, _ := openTestStore(t)

	// No ID -> generated; no status -> OPEN; no entry time -> now.
	p := models.Position{Symbol: "NIFTY", Side: models.SideBuy, Quantity: 75, EntryPrice: 100}
	if err := s.SavePosition(p); err != nil {
		t.Fatalf("SavePosition: %v", err)
	}
	open, err := s.ListOpenPositions()
	if err != nil {
		t.Fatalf("ListOpenPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open = %d, want 1", len(open))
	}
	if open[0].ID == "" || !strings.HasPrefix(open[0].ID, "pos-") {
		t.Errorf("generated ID = %q, want pos-<...> prefix", open[0].ID)
	}
	if open[0].Status != models.PositionOpen {
		t.Errorf("status = %q, want OPEN", open[0].Status)
	}
	if open[0].EntryTime.IsZero() {
		t.Errorf("EntryTime not defaulted")
	}

	// Upsert by same ID updates in place, does not duplicate.
	id := open[0].ID
	p2 := open[0]
	p2.Quantity = 150
	if err := s.SavePosition(p2); err != nil {
		t.Fatalf("SavePosition upsert: %v", err)
	}
	open, _ = s.ListOpenPositions()
	if len(open) != 1 || open[0].ID != id || open[0].Quantity != 150 {
		t.Errorf("after upsert: %+v", open)
	}
}

func TestClosePositionUnknownID(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.ClosePosition("does-not-exist", 100, time.Time{}); err == nil {
		t.Fatalf("ClosePosition(unknown) = nil error, want error")
	}
}

func TestMarkOrderResolvedUnknownID(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.MarkOrderResolved("nope", models.OrderFilled, "", 0, 0, "", time.Time{}); err == nil {
		t.Fatalf("MarkOrderResolved(unknown) = nil error, want error")
	}
}

func TestSaveOrderAttemptRequiresClientOrderID(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.SaveOrderAttempt(models.OrderAttempt{Symbol: "X"}); err == nil {
		t.Fatalf("SaveOrderAttempt without ClientOrderID = nil error, want error")
	}
}

func TestLoadRiskSnapshotFirstRun(t *testing.T) {
	s, _ := openTestStore(t)
	snap, err := s.LoadRiskSnapshot()
	if err != nil {
		t.Fatalf("LoadRiskSnapshot on empty store: %v", err)
	}
	if snap != (models.RiskSnapshot{}) {
		t.Errorf("first-run snapshot = %+v, want zero value", snap)
	}
}

func TestRiskSnapshotOverwrite(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.SaveRiskSnapshot(10000, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRiskSnapshot(9000, -1000, 500); err != nil {
		t.Fatal(err)
	}
	snap, err := s.LoadRiskSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !floatEq(snap.Balance, 9000) || !floatEq(snap.RealizedPnL, -1000) || !floatEq(snap.SessionUsed, 500) {
		t.Errorf("snapshot = %+v, want latest values", snap)
	}
}

func TestStrategyStateNotFound(t *testing.T) {
	s, _ := openTestStore(t)
	v, found, err := s.LoadStrategyState("nine_twenty", "entered")
	if err != nil {
		t.Fatalf("LoadStrategyState: %v", err)
	}
	if found || v != "" {
		t.Errorf("missing key: got (%q, %v), want (\"\", false)", v, found)
	}
}

func TestStrategyStateOverwrite(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.SaveStrategyState("nine_twenty", "entered", "true"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStrategyState("nine_twenty", "entered", "false"); err != nil {
		t.Fatal(err)
	}
	v, found, err := s.LoadStrategyState("nine_twenty", "entered")
	if err != nil || !found {
		t.Fatalf("LoadStrategyState: found=%v err=%v", found, err)
	}
	if v != "false" {
		t.Errorf("value = %q, want overwritten \"false\"", v)
	}
}

func TestNextClientOrderIDUnique(t *testing.T) {
	s, _ := openTestStore(t)
	const n = 2000
	seen := make(map[string]bool, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n/8; i++ {
				id := s.NextClientOrderID()
				if !strings.HasPrefix(id, "titan-") {
					t.Errorf("id %q lacks titan- prefix", id)
					return
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate client order id %q", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Errorf("generated %d unique ids, want %d", len(seen), n)
	}
}

// TestConcurrentMutations exercises concurrent writers+readers to prove
// -race cleanliness and that WAL/serialized transactions hold up.
func TestConcurrentMutations(t *testing.T) {
	s, _ := openTestStore(t)

	var wg sync.WaitGroup
	const perWorker = 25

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("pos-%d-%d", w, i)
				if err := s.SavePosition(models.Position{
					ID: id, Symbol: fmt.Sprintf("SYM%d", w), Side: models.SideBuy,
					Quantity: 75, EntryPrice: float64(100 + i),
				}); err != nil {
					t.Errorf("SavePosition: %v", err)
					return
				}
			}
		}(w)
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := s.SaveRiskSnapshot(float64(10000-i), float64(-i), float64(i)); err != nil {
					t.Errorf("SaveRiskSnapshot: %v", err)
					return
				}
				if _, err := s.ListOpenPositions(); err != nil {
					t.Errorf("ListOpenPositions: %v", err)
					return
				}
				if _, err := s.LoadRiskSnapshot(); err != nil {
					t.Errorf("LoadRiskSnapshot: %v", err)
					return
				}
				if err := s.SaveStrategyState("s", fmt.Sprintf("k%d", w), fmt.Sprintf("%d", i)); err != nil {
					t.Errorf("SaveStrategyState: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	open, err := s.ListOpenPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 4*perWorker {
		t.Errorf("open positions = %d, want %d", len(open), 4*perWorker)
	}
}

func TestOpenDefaultPathParam(t *testing.T) {
	// Just verify a nested, not-yet-existing directory is created.
	dbPath := filepath.Join(t.TempDir(), "nested", "deeper", "state.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open with nested dir: %v", err)
	}
	defer s.Close()
	if s.Path() != dbPath {
		t.Errorf("Path() = %q, want %q", s.Path(), dbPath)
	}
}

// ---- Reconcile --------------------------------------------------------------

func pos(symbol string, side models.PositionSide, qty int) models.Position {
	return models.Position{ID: "id-" + symbol, Symbol: symbol, Side: side, Quantity: qty}
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name     string
		internal []models.Position
		broker   []models.Position
		matched  []string
		phantom  []string
		orphan   []string
		qtyMism  []string
		clean    bool
	}{
		{
			name:     "both empty",
			internal: nil,
			broker:   nil,
			clean:    true,
		},
		{
			name:     "all matched",
			internal: []models.Position{pos("A", models.SideSell, 75), pos("B", models.SideBuy, 30)},
			broker:   []models.Position{pos("A", models.SideSell, 75), pos("B", models.SideBuy, 30)},
			matched:  []string{"A", "B"},
			clean:    true,
		},
		{
			name:     "phantom - internal only",
			internal: []models.Position{pos("A", models.SideSell, 75)},
			broker:   nil,
			phantom:  []string{"A"},
		},
		{
			name:     "orphan - broker only",
			internal: nil,
			broker:   []models.Position{pos("A", models.SideSell, 75)},
			orphan:   []string{"A"},
		},
		{
			name:     "quantity mismatch",
			internal: []models.Position{pos("A", models.SideBuy, 75)},
			broker:   []models.Position{pos("A", models.SideBuy, 25)},
			qtyMism:  []string{"A"},
		},
		{
			name:     "side flip is a mismatch not a match",
			internal: []models.Position{pos("A", models.SideBuy, 75)},
			broker:   []models.Position{pos("A", models.SideSell, 75)},
			qtyMism:  []string{"A"},
		},
		{
			name: "mixed all four classes",
			internal: []models.Position{
				pos("MATCH", models.SideSell, 75),
				pos("PHANTOM", models.SideBuy, 30),
				pos("QTY", models.SideSell, 150),
			},
			broker: []models.Position{
				pos("MATCH", models.SideSell, 75),
				pos("ORPHAN", models.SideSell, 75),
				pos("QTY", models.SideSell, 75),
			},
			matched: []string{"MATCH"},
			phantom: []string{"PHANTOM"},
			orphan:  []string{"ORPHAN"},
			qtyMism: []string{"QTY"},
		},
		{
			name: "multiple records same symbol are net-summed",
			internal: []models.Position{
				pos("A", models.SideBuy, 50),
				{ID: "id-A2", Symbol: "A", Side: models.SideBuy, Quantity: 25},
			},
			broker:  []models.Position{pos("A", models.SideBuy, 75)},
			matched: []string{"A"},
			clean:   true,
		},
		{
			name: "long and short records net out to broker's net",
			internal: []models.Position{
				pos("A", models.SideBuy, 100),
				{ID: "id-A2", Symbol: "A", Side: models.SideSell, Quantity: 25},
			},
			broker:  []models.Position{pos("A", models.SideBuy, 75)},
			matched: []string{"A"},
			clean:   true,
		},
	}

	symbolsOf := func(items []ReconcileItem) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.Symbol)
		}
		return out
	}
	sameSet := func(got []string, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		m := map[string]int{}
		for _, s := range got {
			m[s]++
		}
		for _, s := range want {
			m[s]--
			if m[s] < 0 {
				return false
			}
		}
		return true
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := Reconcile(tc.internal, tc.broker)
			if got := symbolsOf(rep.Matched); !sameSet(got, tc.matched) {
				t.Errorf("Matched = %v, want %v", got, tc.matched)
			}
			if got := symbolsOf(rep.Phantoms); !sameSet(got, tc.phantom) {
				t.Errorf("Phantoms = %v, want %v", got, tc.phantom)
			}
			if got := symbolsOf(rep.Orphans); !sameSet(got, tc.orphan) {
				t.Errorf("Orphans = %v, want %v", got, tc.orphan)
			}
			if got := symbolsOf(rep.QuantityMismatches); !sameSet(got, tc.qtyMism) {
				t.Errorf("QuantityMismatches = %v, want %v", got, tc.qtyMism)
			}
			wantClean := len(tc.phantom) == 0 && len(tc.orphan) == 0 && len(tc.qtyMism) == 0
			if tc.clean != wantClean {
				t.Fatalf("test case inconsistency: clean=%v but buckets say %v", tc.clean, wantClean)
			}
			if rep.Clean() != wantClean {
				t.Errorf("Clean() = %v, want %v", rep.Clean(), wantClean)
			}
		})
	}
}

func TestReconcileItemDetails(t *testing.T) {
	internal := []models.Position{pos("A", models.SideSell, 150)}
	broker := []models.Position{pos("A", models.SideSell, 75)}
	rep := Reconcile(internal, broker)
	if len(rep.QuantityMismatches) != 1 {
		t.Fatalf("QuantityMismatches = %d, want 1", len(rep.QuantityMismatches))
	}
	it := rep.QuantityMismatches[0]
	if it.Type != QuantityMismatch {
		t.Errorf("Type = %q, want %q", it.Type, QuantityMismatch)
	}
	if it.Internal == nil || it.Broker == nil {
		t.Fatalf("Internal/Broker samples missing: %+v", it)
	}
	if it.InternalNetQty != -150 || it.BrokerNetQty != -75 {
		t.Errorf("net quantities = (%d, %d), want (-150, -75) for short positions", it.InternalNetQty, it.BrokerNetQty)
	}
}

func TestNetQuantity(t *testing.T) {
	if got := NetQuantity(pos("A", models.SideBuy, 75)); got != 75 {
		t.Errorf("long NetQuantity = %d, want 75", got)
	}
	if got := NetQuantity(pos("A", models.SideSell, 75)); got != -75 {
		t.Errorf("short NetQuantity = %d, want -75", got)
	}
}
