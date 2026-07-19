// Command watchdog is a small, standalone process (separate binary from the
// main engine, cmd/main.go) that monitors the heartbeat file the engine
// touches every tick (internal/engine/runner.go's touchHeartbeat, default
// path "data/heartbeat", see docs/reports/WP-9-REPORT.md). It closes G-8
// (docs/PRODUCTION_GAPS_R2.md): nothing previously watched the heartbeat
// from OUTSIDE the engine process, so a hard crash or hung process went
// unnoticed.
//
// It does not import anything from internal/engine or internal/app — it
// only reads a file's mtime and (optionally) shells out to a restart
// command, so it keeps running even if the engine process/module is broken.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	heartbeatPath := flag.String("heartbeat", envOr("TITAN_HEARTBEAT_PATH", "data/heartbeat"),
		"path to the engine's heartbeat file (env TITAN_HEARTBEAT_PATH)")
	maxAge := flag.Duration("max-age", 60*time.Second,
		"heartbeat file older than this (or missing) is considered stale")
	pollInterval := flag.Duration("poll", 15*time.Second,
		"how often to check the heartbeat file")
	restartCmd := flag.String("restart-cmd", "",
		"optional command to run when the heartbeat goes stale (e.g. a restart script). Space-separated; no shell expansion.")
	restartCooldown := flag.Duration("restart-cooldown", 10*time.Minute,
		"minimum time between restart-cmd attempts (across breach episodes) — prevents restart-looping")
	flag.Parse()

	w := &watchdog{
		heartbeatPath:   *heartbeatPath,
		maxAge:          *maxAge,
		restartCmd:      *restartCmd,
		restartCooldown: *restartCooldown,
		alert:           buildAlertFunc(),
	}

	log.Printf("titan-watchdog: monitoring %q (max-age=%s poll=%s restart-cmd=%q restart-cooldown=%s)",
		w.heartbeatPath, w.maxAge, *pollInterval, w.restartCmd, w.restartCooldown)
	if w.alert == nil {
		log.Printf("titan-watchdog: TITAN_TG_TOKEN/TITAN_TG_CHAT not set — Telegram alerts are a no-op, only log lines will fire")
	}

	w.check() // check immediately at startup rather than waiting a full poll interval
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		w.check()
	}
}

// watchdog holds the breach-episode state needed to alert once per stale
// episode (not once per poll) and to avoid restart-looping.
type watchdog struct {
	heartbeatPath   string
	maxAge          time.Duration
	restartCmd      string
	restartCooldown time.Duration
	alert           func(event, detail string) // nil == no-op, matches engine's AlertFunc convention

	alerted       bool      // already alerted for the CURRENT breach episode
	lastRestartAt time.Time // zero == restart-cmd never attempted

	// runCmd is overridable in tests so restart-cmd cooldown logic can be
	// exercised without actually spawning a process.
	runCmd func(name string, args ...string) ([]byte, error)
}

// check reads the heartbeat file's mtime once and reacts to a state
// transition (healthy->stale, stale->healthy). Repeated calls while already
// stale (or already healthy) are quiet except for a low-volume status log.
func (w *watchdog) check() {
	info, statErr := os.Stat(w.heartbeatPath)
	var age time.Duration
	stale := true
	if statErr == nil {
		age = time.Since(info.ModTime())
		stale = age > w.maxAge
	}

	switch {
	case stale && !w.alerted:
		w.alerted = true
		var detail string
		if statErr != nil {
			detail = fmt.Sprintf("heartbeat file %s not found (%v) — engine may not have started, or crashed before its first tick",
				w.heartbeatPath, statErr)
		} else {
			detail = fmt.Sprintf("heartbeat file %s is %s old (max-age %s) — engine appears stuck or dead",
				w.heartbeatPath, age.Round(time.Second), w.maxAge)
		}
		log.Printf("STALE HEARTBEAT: %s", detail)
		w.notify("heartbeat_stale", detail)
		w.maybeRestart(detail)

	case stale && w.alerted:
		log.Printf("heartbeat still stale (age=%s) — already alerted for this episode, not re-alerting", age.Round(time.Second))

	case !stale && w.alerted:
		w.alerted = false
		log.Printf("heartbeat recovered (age now %s)", age.Round(time.Second))
		w.notify("heartbeat_recovered", fmt.Sprintf("heartbeat file %s is fresh again (age %s)", w.heartbeatPath, age.Round(time.Second)))

	default:
		// healthy, nothing changed — stay quiet.
	}
}

// notify sends (or, if alert is nil, logs the no-op of) an operational
// alert. Mirrors cmd/main.go's buildAlertFunc nil-check convention.
func (w *watchdog) notify(event, detail string) {
	if w.alert == nil {
		log.Printf("(alert suppressed, no TITAN_TG_TOKEN/TITAN_TG_CHAT: [%s] %s)", event, detail)
		return
	}
	w.alert(event, detail)
}

// maybeRestart runs restartCmd if configured, subject to a cooldown so a
// flapping heartbeat can't restart-loop the engine. Only ever called from
// the stale&&!alerted branch of check(), so it already fires at most once
// per breach episode; the cooldown additionally rate-limits across episodes.
func (w *watchdog) maybeRestart(detail string) {
	if w.restartCmd == "" {
		return
	}
	if !w.lastRestartAt.IsZero() && time.Since(w.lastRestartAt) < w.restartCooldown {
		log.Printf("restart-cmd skipped: cooldown active (%s remaining)", w.restartCooldown-time.Since(w.lastRestartAt))
		return
	}

	fields := strings.Fields(w.restartCmd)
	if len(fields) == 0 {
		log.Printf("restart-cmd is empty after parsing, skipping")
		return
	}

	w.lastRestartAt = time.Now()
	log.Printf("attempting restart-cmd: %s", w.restartCmd)

	run := w.runCmd
	if run == nil {
		run = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		}
	}
	out, err := run(fields[0], fields[1:]...)
	if err != nil {
		log.Printf("restart-cmd FAILED: %v | output: %s", err, strings.TrimSpace(string(out)))
		w.notify("restart_cmd_failed", fmt.Sprintf("restart-cmd %q failed: %v", w.restartCmd, err))
		return
	}
	log.Printf("restart-cmd SUCCEEDED | output: %s", strings.TrimSpace(string(out)))
	w.notify("restart_cmd_attempted", fmt.Sprintf("restart-cmd %q ran after: %s", w.restartCmd, detail))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildAlertFunc mirrors cmd/main.go's Telegram alert sink exactly: same
// TITAN_TG_TOKEN/TITAN_TG_CHAT env vars, same Bot API endpoint shape. Reusing
// the identical convention means an operator sets these two env vars once
// and both the engine and this standalone watchdog can alert. Returns nil
// (no-op) if either var is unset — callers must nil-check, same as
// engine.AlertFunc.
func buildAlertFunc() func(event, detail string) {
	token := os.Getenv("TITAN_TG_TOKEN")
	chat := os.Getenv("TITAN_TG_CHAT")
	if token == "" || chat == "" {
		return nil
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(event, detail string) {
		text := fmt.Sprintf("titan-watchdog [%s]: %s", event, detail)
		endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage?chat_id=%s&text=%s",
			token, url.QueryEscape(chat), url.QueryEscape(text))
		resp, err := client.Get(endpoint)
		if err != nil {
			log.Printf("telegram alert failed: %v", err)
			return
		}
		resp.Body.Close()
	}
}
