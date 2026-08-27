package handler

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Refine LLM budget.
//
// The four refine entry points (personal_refine.go x2, edit.go x2) each had a
// hardcoded 90s context. That number is not independently safe: it is the
// PARENT deadline for an llmfallback.Run whose per-attempt budget is
// LLM_TIMEOUT (default 180s). A parent smaller than one attempt means the
// primary is cut off mid-request and no fallback model can ever be reached, so
// configuring LLM_FALLBACK_MODELS has no effect on refine at all.
//
// For a fallback to get one full attempt the budget must satisfy roughly:
//
//	REFINE_TIMEOUT >= 2 * LLM_TIMEOUT + backoff
//
// This file does not silently impose that: raising the default would change
// user-visible latency on a path that today returns an error at 90s, and the
// right value depends on the refine P95 that llm_call_duration_seconds now
// exposes (issue #220 explicitly defers the number to measured percentiles).
// What changes here is that the value stops being welded into four call sites
// and becomes one tunable knob, so a deployment can widen it without a build.
const defaultRefineTimeout = 90 * time.Second

// RefineTimeoutEnvVar overrides the refine budget, in seconds.
const RefineTimeoutEnvVar = "REFINE_TIMEOUT"

// refineTimeout returns the refine LLM budget. An unset, unparsable or
// non-positive value keeps the historical 90s, so a typo degrades to the
// previous behaviour instead of removing the deadline.
//
// Read from the environment rather than threaded through config.Config,
// following agentStepTimeoutOverride's precedent in agent/profile.go: the
// handlers are constructed in tests that never build a full deps container.
func refineTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv(RefineTimeoutEnvVar))
	if v == "" {
		return defaultRefineTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return defaultRefineTimeout
	}
	return time.Duration(secs) * time.Second
}
