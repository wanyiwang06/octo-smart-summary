//go:build cgo
// +build cgo

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

type handlerTruncatedPlanner struct{}

func (*handlerTruncatedPlanner) Chat(context.Context, []agent.Message, []agent.Tool) (agent.AssistantTurn, error) {
	return agent.AssistantTurn{Content: "truncated final answer", Truncated: true}, nil
}

// This pins the production wiring the lower-level agent tests cannot prove:
// both HTTP handlers must put the authenticated uid and persisted run id on the
// exact context handed to Runner.RunWithHistory.
func TestAgentChatHandlersRecordPlannerOutputTruncation(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	agent.SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { agent.SetSummaryDeps(nil, nil, nil, config.Config{}) })

	store := summaryrun.NewStore(db)
	reg := agent.NewRegistry()
	runner := agent.NewRunner(&handlerTruncatedPlanner{}, reg, agent.NewPool(1), agent.Policy{
		MaxSteps: 1, MaxTokens: 1000, StepTimeout: time.Second,
	})
	handler := newAgentChatHandlerWithRunner(runner, "test-system", newFakeHistoryStore(), 10)
	handler.runStore = store
	router := setupAgentChatRouter(handler)

	tests := []struct {
		name      string
		path      string
		sessionID string
		requestID string
	}{
		{name: "chat", path: "/api/v1/agent/chat", sessionID: "sess-handler-chat", requestID: "req-handler-chat"},
		{name: "stream", path: "/api/v1/agent/chat/stream", sessionID: "sess-handler-stream", requestID: "req-handler-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			body := strings.NewReader(`{"message":"summarize","session_id":"` + tc.sessionID + `","request_id":"` + tc.requestID + `","profile":"summary"}`)
			req := httptest.NewRequest(http.MethodPost, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}

			run, err := store.GetByRequest(context.Background(), testAgentChatUID, tc.sessionID, tc.requestID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if !run.OutputTruncated {
				t.Fatal("planner truncation was not recorded through the real handler context")
			}
		})
	}
}

// THE LOAD-BEARING TEST: the truncation disclosure must not be defeatable by
// the model.
//
// The failure this guards against is specific. When a Reduce is length-
// truncated, merge_summaries appends service.TruncationNotice to the
// `merged_summary` field of a TOOL RESULT. That tool result is not the
// deliverable — it is context handed back to the planner, which then writes the
// final user-facing answer in its own words. Nothing forces the notice through.
// It reads like meta-commentary rather than content, so a model producing a
// polished final answer will plausibly drop it, and the user receives a summary
// that is silently unfinished while looking complete.
//
// So: simulate exactly that. The run is marked output-truncated (as the
// producing paths do), and the content passed to finalizeRun is a clean final
// answer containing NO notice — the model rewrote it away. The verdict must
// still be PARTIAL with a truncation gap, because the gate assembles its
// disclosure from the persisted run row, outside the model's control.
func TestFinalizeRunDisclosesOutputTruncationDespiteCleanAnswer(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-trunc", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	// Full, clean coverage: every channel in scope was fetched successfully and
	// nothing was dropped. The ONLY defect in this run is that the model's own
	// output was cut off, so any gap reported here must come from that fact.
	if err := store.RecordChannelFetch(ctx, "u1", run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record fetch: %v", err)
	}

	// The producing side (merge_summaries / the planner loop) latches the fact.
	if err := store.MarkOutputTruncated(ctx, "u1", run.RunID); err != nil {
		t.Fatalf("mark output truncated: %v", err)
	}

	// The model's final answer. Note what is NOT here: no TruncationNotice, no
	// hint of degradation, no ellipsis. This is a confident, well-formed summary
	// — precisely what a planner emits after quietly discarding the notice.
	cleanAnswer := "本周项目进展顺利，核心功能已全部交付 [1]。"
	if strings.Contains(cleanAnswer, strings.TrimSpace(service.TruncationNotice)) {
		t.Fatal("test setup is wrong: the answer must contain NO prose notice")
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u1", "sess1", "req-trunc", cleanAnswer, []model.Citation{{Index: 1}})

	if verdict != finishgate.Partial {
		t.Fatalf("verdict = %s, want PARTIAL: the model dropped the prose notice, so the gate is the only thing standing between the user and a silently unfinished summary (gaps=%v)", verdict, gaps)
	}
	if !hasGapKind(gaps, finishgate.GapOutputTruncation) {
		t.Fatalf("gaps = %v, want a %s gap assembled from the run row rather than from model-authored text", gaps, finishgate.GapOutputTruncation)
	}
	// Coverage was perfect; the disclosure must name the right failure and not
	// send the user off to narrow a time range that was never the problem.
	if hasGapKind(gaps, finishgate.GapTruncation) {
		t.Fatalf("gaps = %v: an OUTPUT truncation must not be reported as a fetch-pool truncation", gaps)
	}
}

// NO FALSE POSITIVES (requirement 3). The same run shape with nothing truncated
// must stay COMPLETE. gate.go documents a prior bug where a new disclosure path
// reported correct behaviour as a defect; this is the guard against repeating it
// and it is the exact control for the test above — identical setup minus the
// MarkOutputTruncated call.
func TestFinalizeRunCleanRunStaysCompleteWithoutOutputTruncation(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-clean", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	if err := store.RecordChannelFetch(ctx, "u1", run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record fetch: %v", err)
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u1", "sess1", "req-clean", "本周项目进展顺利 [1]。", []model.Citation{{Index: 1}})
	if verdict != finishgate.Complete {
		t.Fatalf("verdict = %s, want COMPLETE for a run with no truncation anywhere (gaps=%v)", verdict, gaps)
	}
	if len(gaps) != 0 {
		t.Fatalf("a clean run must disclose nothing, got %v", gaps)
	}
}

// MarkOutputTruncated is idempotent and owner-scoped. The within-attempt latch
// semantics are covered by the rejected-premature and repeated-Reduce tests;
// the replay-only reset boundary is covered separately below.
func TestMarkOutputTruncatedIsIdempotentAndOwnerScoped(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-latch", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if got, err := store.GetByID(ctx, "u1", run.RunID); err != nil {
		t.Fatalf("get run: %v", err)
	} else if got.OutputTruncated {
		t.Fatal("a fresh run must not be marked output-truncated")
	}

	// Mark twice: the second call is a no-op, not an error.
	for i := 0; i < 2; i++ {
		if err := store.MarkOutputTruncated(ctx, "u1", run.RunID); err != nil {
			t.Fatalf("mark #%d: %v", i+1, err)
		}
	}
	got, err := store.GetByID(ctx, "u1", run.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !got.OutputTruncated {
		t.Fatal("output_truncated must stay latched after repeated marks")
	}

	// Owner scoping: another user's mark must not touch this run, and must not
	// error (it simply matches no row).
	if err := store.MarkOutputTruncated(ctx, "u2", run.RunID); err != nil {
		t.Fatalf("cross-owner mark should be a silent no-op, got: %v", err)
	}
}

// A repeated request_id starts a NEW generation attempt on the SAME run row.
// Attempt A's output is discarded on the SSE fallback/retry path, so its
// output_truncated latch must not poison attempt B's clean deliverable. The
// reset belongs exactly at maybePersistSummaryRun's created=false branch; the
// latch remains one-way everywhere inside one attempt.
func TestReplayClearsStaleOutputTruncationAndCanRelatch(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	h := &AgentChatHandler{runStore: store}
	req := agentChatRequest{
		Message:   "总结项目进展",
		SessionID: "sess-trunc-replay",
		RequestID: "req-trunc-replay",
		SelectedChannels: []selectedChannel{{
			ChannelID: "ch-1", ChannelType: "group", Name: "项目群",
		}},
	}

	// Attempt A creates the run and delivers a truncated result.
	runID := h.maybePersistSummaryRun(ctx, "u1", req, true)
	if runID == "" {
		t.Fatal("first attempt did not create a run")
	}
	if err := store.RecordChannelFetch(ctx, "u1", runID, "ch-1", true, false); err != nil {
		t.Fatalf("record coverage: %v", err)
	}
	if err := store.MarkOutputTruncated(ctx, "u1", runID); err != nil {
		t.Fatalf("mark attempt A truncated: %v", err)
	}

	// The reset is owner-scoped: an unrelated user cannot clear the latch.
	if err := store.ClearOutputTruncatedForReplay(ctx, "someone-else", runID); err != nil {
		t.Fatalf("foreign replay reset: %v", err)
	}
	if got, err := store.GetByID(ctx, "u1", runID); err != nil {
		t.Fatalf("get after foreign reset: %v", err)
	} else if !got.OutputTruncated {
		t.Fatal("a foreign user cleared the owner's truncation latch")
	}

	// Attempt B reuses request_id. The handler recognises created=false and
	// clears only the previous attempt's latch before generating fresh output.
	if replayRunID := h.maybePersistSummaryRun(ctx, "u1", req, true); replayRunID != runID {
		t.Fatalf("replay run_id = %q, want %q", replayRunID, runID)
	}
	got, err := store.GetByID(ctx, "u1", runID)
	if err != nil {
		t.Fatalf("get replay run: %v", err)
	}
	if got.OutputTruncated {
		t.Fatal("attempt B inherited attempt A's output_truncated latch")
	}

	// A complete attempt B must now be judged COMPLETE rather than inheriting a
	// false output_truncation gap from attempt A.
	verdict, gaps := (&AgentSummaryHandler{db: db}).finalizeRun(
		ctx, "u1", req.SessionID, req.RequestID, "项目进展完整 [1]。", []model.Citation{{Index: 1}},
	)
	if verdict != finishgate.Complete {
		t.Fatalf("clean replay verdict = %s, want COMPLETE (gaps=%v)", verdict, gaps)
	}
	if hasGapKind(gaps, finishgate.GapOutputTruncation) {
		t.Fatalf("clean replay inherited stale output_truncation gap: %v", gaps)
	}

	// The reset is not permanent: attempt B still latches its own truncation.
	if err := store.MarkOutputTruncated(ctx, "u1", runID); err != nil {
		t.Fatalf("relatch attempt B: %v", err)
	}
	if got, err := store.GetByID(ctx, "u1", runID); err != nil {
		t.Fatalf("get after relatch: %v", err)
	} else if !got.OutputTruncated {
		t.Fatal("attempt B could not latch its own output truncation")
	}
}
