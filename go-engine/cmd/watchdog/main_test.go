package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touchAt(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// TestWatchdogAlertsOncePerBreachEpisode is the acceptance scenario: a stale
// heartbeat fires exactly one alert for a burst of polls still WITHIN the
// re-alert interval (restartCooldown), resets on recovery, and fires again
// on a fresh breach. restartCooldown is set explicitly (large) so this test
// isn't about the sustained-outage re-alert behavior -- see
// TestWatchdogReAlertsOnSustainedOutage for that.
func TestWatchdogAlertsOncePerBreachEpisode(t *testing.T) {
	hb := filepath.Join(t.TempDir(), "heartbeat")
	var events []string
	w := &watchdog{
		heartbeatPath:   hb,
		maxAge:          time.Second,
		restartCooldown: time.Hour,
		alert:           func(event, _ string) { events = append(events, event) },
	}

	touchAt(t, hb, time.Now())
	w.check()
	if len(events) != 0 {
		t.Fatalf("fresh heartbeat: unexpected alerts %v", events)
	}

	stale := time.Now().Add(-5 * time.Second)
	touchAt(t, hb, stale)
	w.check()
	w.check()
	w.check()
	if len(events) != 1 || events[0] != "heartbeat_stale" {
		t.Fatalf("expected exactly one heartbeat_stale alert across repeated polls, got %v", events)
	}

	touchAt(t, hb, time.Now())
	w.check()
	if len(events) != 2 || events[1] != "heartbeat_recovered" {
		t.Fatalf("expected a recovery alert, got %v", events)
	}

	touchAt(t, hb, stale)
	w.check()
	if len(events) != 3 || events[2] != "heartbeat_stale" {
		t.Fatalf("expected a new stale alert for the new breach episode, got %v", events)
	}
}

// TestWatchdogReAlertsOnSustainedOutage reproduces the audit-R3 bug: the
// watchdog used to alert exactly once per stale episode and then go silent
// for the rest of a sustained outage, even if it never recovered -- whoever
// is on call had no way to know the outage was still ongoing hours later.
// It must now keep re-alerting at restartCooldown intervals for as long as
// the episode remains unresolved.
func TestWatchdogReAlertsOnSustainedOutage(t *testing.T) {
	hb := filepath.Join(t.TempDir(), "heartbeat")
	var events []string
	w := &watchdog{
		heartbeatPath:   hb,
		maxAge:          time.Second,
		restartCooldown: time.Minute,
		alert:           func(event, _ string) { events = append(events, event) },
	}

	stale := time.Now().Add(-5 * time.Second)
	touchAt(t, hb, stale)
	w.check() // initial alert
	if len(events) != 1 || events[0] != "heartbeat_stale" {
		t.Fatalf("expected the initial stale alert, got %v", events)
	}

	w.check() // still within the re-alert interval -- no new alert yet
	if len(events) != 1 {
		t.Fatalf("expected no re-alert before the cooldown interval elapses, got %v", events)
	}

	// Simulate the re-alert interval having elapsed without recovery.
	w.lastAlertAt = time.Now().Add(-2 * time.Minute)
	w.check()
	if len(events) != 2 || events[1] != "heartbeat_still_stale" {
		t.Fatalf("expected a follow-up alert once the re-alert interval elapsed on a still-unresolved episode, got %v", events)
	}

	// Recovering must stop the re-alerts.
	touchAt(t, hb, time.Now())
	w.check()
	if len(events) != 3 || events[2] != "heartbeat_recovered" {
		t.Fatalf("expected a recovery alert, got %v", events)
	}
}

func TestWatchdogMissingHeartbeatFileIsStale(t *testing.T) {
	var events []string
	w := &watchdog{
		heartbeatPath: filepath.Join(t.TempDir(), "does-not-exist"),
		maxAge:        time.Second,
		alert:         func(event, _ string) { events = append(events, event) },
	}
	w.check()
	if len(events) != 1 || events[0] != "heartbeat_stale" {
		t.Fatalf("expected stale alert for a missing heartbeat file, got %v", events)
	}
}

func TestWatchdogNoOpAlertWhenNil(t *testing.T) {
	// alert == nil must never panic (matches engine.AlertFunc's nil-safe
	// convention — no TITAN_TG_TOKEN/TITAN_TG_CHAT set).
	hb := filepath.Join(t.TempDir(), "heartbeat")
	touchAt(t, hb, time.Now().Add(-5*time.Second))
	w := &watchdog{heartbeatPath: hb, maxAge: time.Second}
	w.check() // must not panic
	if !w.alerted {
		t.Fatal("expected alerted=true even with alert==nil")
	}
}

// TestWatchdogRestartCmdCooldown verifies the restart-cmd fires on the first
// breach, is skipped on a second breach episode while the cooldown is still
// active, and fires again once the cooldown has expired.
func TestWatchdogRestartCmdCooldown(t *testing.T) {
	hb := filepath.Join(t.TempDir(), "heartbeat")
	stale := time.Now().Add(-5 * time.Second)

	var runs int
	w := &watchdog{
		heartbeatPath:   hb,
		maxAge:          time.Second,
		restartCmd:      "fake-restart-script.sh",
		restartCooldown: time.Hour,
		alert:           func(string, string) {},
		runCmd: func(name string, args ...string) ([]byte, error) {
			runs++
			return []byte("ok"), nil
		},
	}

	touchAt(t, hb, stale)
	w.check() // episode 1: breach -> restart attempt #1
	if runs != 1 {
		t.Fatalf("expected 1 restart attempt on first breach, got %d", runs)
	}

	w.check() // still stale, same episode: no new alert/restart call site reached at all
	if runs != 1 {
		t.Fatalf("expected restart attempts to stay at 1 while still in the same episode, got %d", runs)
	}

	// Recover, then breach again immediately (episode 2) — cooldown (1h) is
	// still active, so no restart attempt should fire.
	touchAt(t, hb, time.Now())
	w.check()
	touchAt(t, hb, stale)
	w.check()
	if runs != 1 {
		t.Fatalf("expected cooldown to block restart on second episode, got %d runs", runs)
	}

	// Expire the cooldown, then breach a third time (episode 3).
	w.lastRestartAt = time.Now().Add(-2 * time.Hour)
	touchAt(t, hb, time.Now())
	w.check()
	touchAt(t, hb, stale)
	w.check()
	if runs != 2 {
		t.Fatalf("expected a restart attempt after cooldown expiry, got %d runs", runs)
	}
}

func TestWatchdogRestartCmdNotConfiguredIsNoOp(t *testing.T) {
	hb := filepath.Join(t.TempDir(), "heartbeat")
	touchAt(t, hb, time.Now().Add(-5*time.Second))
	called := false
	w := &watchdog{
		heartbeatPath: hb,
		maxAge:        time.Second,
		alert:         func(string, string) {},
		runCmd: func(name string, args ...string) ([]byte, error) {
			called = true
			return nil, nil
		},
	}
	w.check()
	if called {
		t.Fatal("runCmd invoked despite restartCmd being empty")
	}
}
