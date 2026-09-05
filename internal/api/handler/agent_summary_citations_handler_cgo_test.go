//go:build cgo

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// History hydration does not carry the generation request_id. A strict workspace
// save must therefore use the request id derived from the selected preview's
// persisted run binding when it resolves the frozen citation manifest.
func TestCreateAgentSummary_WorkspaceSaveDerivesRequestIDForFrozenCitations(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(
		&model.AgentSummaryRun{},
		&model.AgentSummarySpec{},
		&model.AgentEvidenceArtifact{},
		&model.AgentCitationManifest{},
		&model.AgentSummaryTurn{},
	); err != nil {
		t.Fatalf("migrate V2 tables: %v", err)
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	fixture := seedWorkspaceSaveFixture(t, db, "session-1")
	internalSessionID := summaryWorkspaceAgentSessionID(
		fixture.Session.SpaceID,
		fixture.Session.SessionID,
		fixture.Session.ScopeVersion,
	)
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", "run-v2-1").
		Update("session_id", internalSessionID).Error; err != nil {
		t.Fatalf("scope workspace run: %v", err)
	}
	if err := db.Model(&model.AgentMessageEvidence{}).
		Where("user_id = ? AND session_id = ?", "test-user", "session-1").
		Update("session_id", internalSessionID).Error; err != nil {
		t.Fatalf("scope workspace evidence: %v", err)
	}
	if err := db.Model(&model.AgentEvidenceArtifact{}).Where("run_id = ?", "run-v2-1").
		Update("session_id", internalSessionID).Error; err != nil {
		t.Fatalf("scope workspace artifact: %v", err)
	}
	turn := model.AgentSummaryTurn{
		SpaceID: fixture.Session.SpaceID, UserID: fixture.Session.UserID, SessionID: fixture.Session.SessionID,
		RequestID: "req-1", RequestHash: "workspace-history-save", ScopeVersion: fixture.Session.ScopeVersion,
		Status: "completed", Attempt: 1, RunID: "run-v2-1",
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("seed workspace turn: %v", err)
	}
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "preview ready",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "Charlie said [3]",
			Version: fixture.Message.ArtifactVersion,
		},
	})
	if err != nil {
		t.Fatalf("marshal preview payload: %v", err)
	}
	if err := db.Model(&model.AgentMessage{}).Where("id = ?", fixture.Message.ID).Updates(map[string]interface{}{
		"run_id":                "run-v2-1",
		"turn_id":               turn.ID,
		"response_payload_json": string(payloadJSON),
	}).Error; err != nil {
		t.Fatalf("bind workspace preview to run: %v", err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{
		"Idempotency-Key": "workspace-history-save",
	})
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
	if len(citations) != 0 {
		t.Fatalf("saved citations = %+v, want frozen manifest to drop post-freeze Charlie [3]", citations)
	}
}
