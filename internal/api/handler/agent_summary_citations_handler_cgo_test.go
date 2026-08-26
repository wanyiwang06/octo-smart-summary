//go:build cgo

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// A save may omit request_id even when its selected assistant message is
// bound to a V2 run. The derived request id is for validating/finalizing that
// message only; it must not silently switch citation building from the legacy
// recompute to the run's frozen manifest.
func TestCreateAgentSummary_OmittedRequestIDKeepsLegacyCitationRecompute(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(
		&model.AgentSummaryRun{},
		&model.AgentSummarySpec{},
		&model.AgentEvidenceArtifact{},
		&model.AgentCitationManifest{},
	); err != nil {
		t.Fatalf("migrate V2 tables: %v", err)
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	draft := model.AgentMessage{
		UserID:    "test-user",
		SessionID: "session-1",
		Role:      "assistant",
		Content:   "Charlie said [3]",
		RunID:     "run-v2-1",
	}
	if err := db.Create(&draft).Error; err != nil {
		t.Fatalf("seed assistant draft: %v", err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), map[string]interface{}{
		"session_id":          draft.SessionID,
		"origin_channel_id":   "channel-1",
		"origin_channel_type": 1,
		"agent_message_id":    draft.ID,
		"snapshot_version":    1,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			FinishStatus string `json:"finish_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if response.Data.FinishStatus != string(finishgate.Partial) {
		t.Fatalf("finish_status = %q, want derived request_id to finalize as PARTIAL", response.Data.FinishStatus)
	}

	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatalf("load saved deliverable: %v", err)
	}
	citations := saved.GetCitations()
	if len(citations) != 1 {
		t.Fatalf("saved citations = %d, want recomputed Charlie citation", len(citations))
	}
	if citations[0].Index != 3 || citations[0].Sender != "Charlie" {
		t.Fatalf("saved citation = %+v, want recomputed [3]=Charlie", citations[0])
	}
}
