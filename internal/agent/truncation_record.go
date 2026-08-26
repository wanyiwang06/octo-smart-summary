package agent

import (
	"context"
	"log"
	"sync/atomic"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
)

type outputTruncationTracker struct {
	truncated atomic.Bool
}

type contextKeyOutputTruncationTracker struct{}

func (t *outputTruncationTracker) mark() {
	if t != nil {
		t.truncated.Store(true)
	}
}

func (t *outputTruncationTracker) value() bool {
	return t != nil && t.truncated.Load()
}

func withOutputTruncationTracker(ctx context.Context) (context.Context, *outputTruncationTracker) {
	tracker := &outputTruncationTracker{}
	return context.WithValue(ctx, contextKeyOutputTruncationTracker{}, tracker), tracker
}

func markAttemptOutputTruncated(ctx context.Context) {
	if tracker, ok := ctx.Value(contextKeyOutputTruncationTracker{}).(*outputTruncationTracker); ok && tracker != nil {
		tracker.mark()
	}
}

// recordOutputTruncated latches "a model completion on this run's answer path
// was cut off at its token limit" onto the conservative run aggregate. The
// attempt-local form is copied to the final assistant message for the SS-07
// finish gate (finishgate.RunState.OutputTruncated -> GapOutputTruncation).
//
// WHY THIS EXISTS AT ALL — the disclosure must not be defeatable by the model.
//
// Before this, an LLM-output truncation was disclosed only as PROSE: the client
// appends service.TruncationNotice, and merge_summaries puts it in the
// `merged_summary` field of a TOOL RESULT. But a tool result is an INTERMEDIATE
// artifact. It is fed back to the planner as context, and the planner — not the
// tool — writes the final user-facing answer. Nothing forces the notice through:
// it reads like meta-commentary rather than content, so a model polishing a
// final answer will plausibly drop it. The user then receives a deliverable that
// is silently incomplete and LOOKS complete, which is the precise failure class
// this area of the codebase exists to prevent.
//
// Routing the fact through persistence instead makes the disclosure structural:
// an attempt-local tracker is copied onto the final assistant row (the exact
// deliverable selected at save time), while the run row keeps a conservative
// aggregate for legacy rows. finishgate.Evaluate consumes that persisted fact
// outside the generating model's control, so no rewrite can suppress it.
//
// This does NOT replace the inline notice — the two are complementary. The gate
// is reliable and machine-readable; the inline notice is what tells a reader
// WHERE the text stops. Both are kept deliberately.
//
// The run write mirrors the other coverage facts (recordFetch in
// tool_fetch_channel.go, recordDroppedMessages in tool_summarize_chunk.go). The
// message binding is intentionally more precise: it travels with the final
// assistant row and overrides the shared aggregate when that row is saved.
//
// Best-effort and never fatal. Failing a usable-but-degraded deliverable because
// its bookkeeping write failed would trade a partial answer for no answer, which
// the surrounding design explicitly refuses. A failed write is logged loudly.
func recordOutputTruncated(ctx context.Context, uid, runID string) {
	if !SummaryV2Enabled() || runID == "" {
		return
	}
	if uid == "" {
		log.Printf("[truncation] skip output truncation record: missing uid for run=%s", runID)
		return
	}
	summaryDB, _, _, _ := GetSummaryDeps()
	if summaryDB == nil {
		return
	}
	// WithoutCancel, mirroring recordFetch: this records a DEGRADATION, and one
	// live cause of degradation is a client that disconnected mid-answer, which
	// cancels this ctx. The record must not fail for the same reason the run did.
	recordCtx := context.WithoutCancel(ctx)
	if err := summaryrun.NewStore(summaryDB).MarkOutputTruncated(recordCtx, uid, runID); err != nil {
		log.Printf("[truncation] record output truncation failed run=%s: %v", runID, err)
	}
}

// recordOutputTruncatedFromContext is the convenience form shared by the
// planner loop and tool handlers. Runner-level bookkeeping uses
// ContextKeyRunOwnerID; tool handlers retain the narrower ContextKeyUID injected
// by buildRegistryWithUID.
func recordOutputTruncatedFromContext(ctx context.Context) {
	// Mark the current Runner attempt before the best-effort DB write. Even if
	// the run-row write fails, a successfully persisted final assistant message
	// still carries the degradation and the save gate can disclose it.
	markAttemptOutputTruncated(ctx)
	uid, _ := ctx.Value(ContextKeyRunOwnerID).(string)
	if uid == "" {
		uid, _ = ctx.Value(ContextKeyUID).(string)
	}
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	recordOutputTruncated(ctx, uid, runID)
}
