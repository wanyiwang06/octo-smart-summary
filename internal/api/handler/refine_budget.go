package handler

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Refine LLM budget.
//
// The four refine entry points (personal_refine.go x2, edit.go x2) each had a
// hardcoded 90s context. That number is not independently meaningful: it is the
// PARENT deadline for an llmfallback.Run whose per-attempt timeout is
// LLM_TIMEOUT (default 180s). A parent smaller than a single attempt means a
// hanging primary consumes the whole budget and no fallback attempt can start,
// so LLM_FALLBACK_MODELS is effectively inert on refine.
//
// For a fallback to get one full attempt the budget must satisfy roughly:
//
//	REFINE_TIMEOUT >= 2 * LLM_TIMEOUT + backoff
//
// This file deliberately does NOT raise the default to satisfy that: 90s is the
// latency users experience today on a path that currently errors at 90s, and
// the right value depends on the refine percentiles that
// llm_run_duration_seconds now exposes (#220 defers the number to measured
// P95/P99). What changes here is only that the value stops being welded into
// four call sites.
const defaultRefineTimeout = 90 * time.Second

// Clamp bounds for REFINE_TIMEOUT.
//
// Parsing alone is not enough to make a typo safe. Two values parse cleanly and
// are still wrong in ways that are invisible in production:
//
//   - a wrong unit (REFINE_TIMEOUT=90000, assuming milliseconds) yields a
//     25-hour deadline, so a stuck refine holds its connection and goroutine
//     for effectively forever instead of failing at 90s;
//   - a huge value overflows time.Duration (seconds * time.Second wraps), which
//     produces a NEGATIVE duration and makes every refine request build an
//     already-expired context — all four handlers would fail instantly, and
//     silently.
//
// Clamping converts both into a bounded, logged deviation.
const (
	minRefineTimeout = 1 * time.Second
	maxRefineTimeout = 30 * time.Minute
)

// refineTimeoutEnvVar overrides the refine budget, in seconds.
const refineTimeoutEnvVar = "REFINE_TIMEOUT"

// refineTimeoutWarnOnce keeps a rejected or clamped value to a single log line
// per process. refineTimeout runs on every refine request, so an unguarded
// log.Printf here would reproduce the bad value on every call.
var refineTimeoutWarnOnce sync.Once

// refineTimeout returns the refine LLM budget.
//
// An unset value keeps the historical 90s. An unparsable or non-positive value
// also keeps 90s; an out-of-range value is clamped into [1s, 30m]. Every
// deviation from the configured input is logged once, so a typo is visible
// instead of silently reshaping every refine request.
//
// Read from the environment rather than threaded through config.Config,
// following agentStepTimeoutOverride's precedent in agent/profile.go: these
// handlers are constructed in tests that never build a full deps container.
func refineTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(refineTimeoutEnvVar))
	if raw == "" {
		return defaultRefineTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		warnRefineTimeoutOnce("%s=%q is not a positive integer; using default %s", refineTimeoutEnvVar, raw, defaultRefineTimeout)
		return defaultRefineTimeout
	}
	// Guard the multiplication itself: seconds beyond this bound cannot be
	// represented and would wrap to a negative duration.
	if secs > int(maxRefineTimeout/time.Second) {
		warnRefineTimeoutOnce("%s=%q exceeds the maximum %s; clamped", refineTimeoutEnvVar, raw, maxRefineTimeout)
		return maxRefineTimeout
	}
	d := time.Duration(secs) * time.Second
	if d < minRefineTimeout {
		warnRefineTimeoutOnce("%s=%q is below the minimum %s; clamped", refineTimeoutEnvVar, raw, minRefineTimeout)
		return minRefineTimeout
	}
	return d
}

func warnRefineTimeoutOnce(format string, args ...any) {
	refineTimeoutWarnOnce.Do(func() {
		log.Printf("[refine] "+format, args...)
	})
}
