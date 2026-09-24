//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// RED round-2 for PR#268 — reviewer entry-point probes codified as handler
// tests (octo-spec cr-convergence lesson 8/9 + 18续):
//
//   - M-P1 (B-2, Jerry-Xin 🔴; yujiawei §1(a); mocha C1): both personal-refine
//     transports must persist the NEUTRALIZED form of a model-emitted
//     "lead-in\n---\n" shape. At head fc98e22 neither path calls
//     NormalizeSetextHeadings, so the persisted PersonalResult.content keeps
//     the setext form. Identity-replacing the fix's normalize calls must turn
//     this test red again.
//   - M-P2 (B-3, yujiawei 2.1): the workspace-save branch must persist the
//     neutralized preview content; at head, agent_summary.go:622 overwrites
//     the :512 normalization with the raw locked payload.
//
// Each test must FAIL at head fc98e22 and PASS on the fix commit.

const setextRedlineLeadIn = "现将进展整理如下：\n---\n### 一、已完成事项\n"

func TestPersonalRefineNeutralizesSetextHeadingsBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					delta, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
						map[string]interface{}{"delta": map[string]string{"content": setextRedlineLeadIn}},
					}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
					map[string]interface{}{"message": map[string]string{"content": setextRedlineLeadIn}, "finish_reason": "stop"},
				}})
			}))
			defer srv.Close()
			db := setupPersonalRefineDB(t)
			task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
			h := NewPersonalHandler(db, "", nil)
			h.SetLLM(service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
			r := setupPersonalRefineRouter(h)
			r.POST("/api/v1/summaries/:id/personal-refine-stream", h.RefinePersonalSummaryStream)
			path := fmt.Sprintf("/api/v1/summaries/%d/personal-refine", task.ID)
			if stream {
				path += "-stream"
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"feedback":"adjust","base_version":1}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Token", "member_a")
			req.Header.Set("X-Space-Id", "space1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var saved model.PersonalResult
			if err := db.First(&saved, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(saved.Content, "如下：\n\n---") {
				t.Fatalf("M-P1 (stream=%t): persisted content not neutralized, setext form survived: %q (status=%d body=%s)",
					stream, saved.Content, w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateAgentSummary_WorkspaceSavePersistsNeutralizedContent(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-setext-save")
	if err := db.Create(&model.AgentMessage{
		SpaceID: "test-space", UserID: "test-user", SessionID: fixture.Session.SessionID,
		Role: "user", Content: "请生成总结",
	}).Error; err != nil {
		t.Fatalf("seed workspace user message: %v", err)
	}
	// Point the saved preview payload at the setext shape: the deliverable
	// persisted from workspace mode must be the neutralized bytes.
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "已生成预览。",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "开场说明不应保存\n\n现将进展整理如下：\n---\n### 一、已完成事项",
			Version: 3,
		},
	})
	if err != nil {
		t.Fatalf("marshal workspace payload: %v", err)
	}
	payload := string(payloadJSON)
	fixture.Message.ResponsePayload = &payload
	if err := db.Save(&fixture.Message).Error; err != nil {
		t.Fatalf("update preview payload: %v", err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	w := doAgentSave(t, r, fixture.Body, map[string]string{"Idempotency-Key": "workspace-setext-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save want 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Take(&task).Error; err != nil {
		t.Fatalf("load saved task: %v", err)
	}
	var result model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, "test-user").Take(&result).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	want := "开场说明不应保存\n\n现将进展整理如下：\n\n---\n### 一、已完成事项"
	if result.Content != want {
		t.Fatalf("M-P2: workspace-save persisted raw setext content:\n got=%q\nwant=%q", result.Content, want)
	}
}

// M6a mutation-lock (Jerry-Xin mutation battery: identity-replacing both
// edit.go normalize calls kept the full 727-test handler suite green — the
// team-refine transports had zero wiring coverage). Pins that both team
// refine transports persist the neutralized form. Passes today (edit.go
// carried the call from round 1); it exists to die under the mutation.
func TestRefineTeamNeutralizesSetextHeadingsBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					delta, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
						map[string]interface{}{"delta": map[string]string{"content": setextRedlineLeadIn}},
					}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
					map[string]interface{}{"message": map[string]string{"content": setextRedlineLeadIn}, "finish_reason": "stop"},
				}})
			}))
			defer srv.Close()
			llm := service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil)
			db := setupEditDB(t)
			id, resultID, prID := seedEditableTask(t, db)
			var base model.SummaryResult
			db.First(&base, resultID)
			base.SetTeamCitations([]model.TeamCitation{{Index: 1, UserID: "creator1"}})
			db.Save(&base)
			if err := db.Model(&model.PersonalResult{}).Where("id = ?", prID).Update("citations_json", base.CitationsJSON).Error; err != nil {
				t.Fatal(err)
			}
			h := NewEditHandler(db, llm)
			r := setupEditRouter(h)
			path := fmt.Sprintf("/api/v1/summaries/%d/refine", id)
			if stream {
				r.POST("/api/v1/summaries/:id/refine-stream", h.RefineSummaryStream)
				path += "-stream"
			}
			w := doJSONRequest(r, "POST", path, "creator1", map[string]interface{}{"feedback": "保留正文", "base_result_id": resultID})
			var saved model.SummaryResult
			db.Order("id DESC").First(&saved)
			if !strings.Contains(saved.Content, "如下：\n\n---") {
				t.Fatalf("M6a (stream=%t): team refine persisted content not neutralized: %q (status=%d body=%s)",
					stream, saved.Content, w.Code, w.Body.String())
			}
		})
	}
}
