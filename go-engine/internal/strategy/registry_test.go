package strategy

import "testing"

func TestRegistryGetReturnsSingleton(t *testing.T) {
	Reset("nine_twenty")
	a, err := Get("nine_twenty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := Get("nine_twenty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a != b {
		t.Errorf("expected Get to return the same cached instance twice")
	}

	// Mutate state on the shared instance and verify it's visible via the
	// second handle — proves it's the same object, not just an equal value.
	nt, ok := a.(*NineTwentyStrategy)
	if !ok {
		t.Fatalf("expected *NineTwentyStrategy")
	}
	nt.ConfirmEntry()
	nt2 := b.(*NineTwentyStrategy)
	snap := nt2.Snapshot()
	if snap["entered"] != "true" {
		t.Errorf("expected shared state to reflect ConfirmEntry() call via the other handle, got %v", snap)
	}

	Reset("nine_twenty")
}

func TestRegistryReset(t *testing.T) {
	Reset("sniper")
	a, _ := Get("sniper")
	Reset("sniper")
	b, _ := Get("sniper")
	if a == b {
		t.Errorf("expected Reset to force a fresh instance on next Get")
	}
	Reset("sniper")
}

func TestRegistryUnknownStrategy(t *testing.T) {
	if _, err := Get("does_not_exist"); err == nil {
		t.Errorf("expected error for unknown strategy name")
	}
}
