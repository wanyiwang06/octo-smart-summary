//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

func setOptimizationPreview(t *testing.T, db *gorm.DB, f workspaceSaveFixture, content string) {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(*f.Message.ResponsePayload), &payload); err != nil {
		t.Fatal(err)
	}
	payload["preview"].(map[string]interface{})["content"] = content
	raw, _ := json.Marshal(payload)
	if err := db.Model(&f.Message).Update("response_payload_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}

func provenanceRequest(r http.Handler, method, path, body, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", user)
	req.Header.Set("X-Space-Id", "test-space")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestOptimizationPersonalRefineHistoryRestoreProvenance(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "personal")
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "provenance"})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Refined personal\"}}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"content":"Refined personal"},"finish_reason":"stop"}]}`)
				}
			}))
			defer srv.Close()
			ph := NewPersonalHandler(db, "", nil)
			ph.SetLLM(service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
			r := setupAgentSummaryRouter(h)
			r.POST("/api/v1/summaries/:id/personal-refine", ph.RefinePersonalSummary)
			r.POST("/api/v1/summaries/:id/personal-refine-stream", ph.RefinePersonalSummaryStream)
			r.GET("/api/v1/summaries/:id/personal-versions/:version_id", ph.GetPersonalVersion)
			r.POST("/api/v1/summaries/:id/personal-versions/:version_id/restore", ph.RestorePersonalVersion)
			path := fmt.Sprintf("/api/v1/summaries/%d/personal-refine", b.Task.ID)
			if stream {
				path += "-stream"
			}
			w = provenanceRequest(r, "POST", path, `{"feedback":"shorter","base_version":2}`, "test-user")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var v model.PersonalResultVersion
			if err := db.Where("task_id = ? AND version = 3", b.Task.ID).First(&v).Error; err != nil {
				t.Fatalf("%v %s", err, w.Body.String())
			}
			snap := decodeResultSnapshot(v.SnapshotJSON)
			if snap == nil || snap.ContentVersion != 3 || snap.ParentSnapshotVersion == nil || *snap.ParentSnapshotVersion != 2 || *snap.UserInstruction != "shorter" {
				t.Fatalf("bad snapshot %+v", snap)
			}
			versionPath := fmt.Sprintf("/api/v1/summaries/%d/personal-versions/%d", b.Task.ID, v.ID)
			w = provenanceRequest(r, "GET", versionPath, "", "test-user")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"content_version":3`) {
				t.Fatal(w.Body.String())
			}
			w = provenanceRequest(r, "POST", versionPath+"/restore", "{}", "test-user")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var pr model.PersonalResult
			db.First(&pr, b.Personal.ID)
			if pr.Content != v.Content || !sameSnapshotJSON(pr.SnapshotJSON, v.SnapshotJSON) {
				t.Fatal("restore lost snapshot")
			}
			// Pre-migration NULL history must not borrow V3's scope or erase its record.
			legacy := model.PersonalResultVersion{TaskID: b.Task.ID, UserID: "test-user", Content: "Legacy", Version: 4, OperationType: "generate"}
			if err := db.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			w = provenanceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/personal-versions/%d/restore", b.Task.ID, legacy.ID), "{}", "test-user")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			db.First(&pr, b.Personal.ID)
			unknown := pr.GetSnapshot()
			if unknown == nil || unknown.ProvenanceStatus != "unknown" || unknown.ContentVersion != 4 || len(unknown.Scope.ChannelIDs) != 0 {
				t.Fatalf("legacy borrowed provenance: %+v", unknown)
			}
			var preserved model.PersonalResultVersion
			db.First(&preserved, v.ID)
			if !sameSnapshotJSON(preserved.SnapshotJSON, v.SnapshotJSON) {
				t.Fatal("outgoing provenance destroyed")
			}
		})
	}
}

func TestOptimizationTeamCitationOwnershipAcrossReads(t *testing.T) {
	for _, mode := range []int{model.ModeByPerson, 1 /* legacy non-person mode */} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "team")
			db.Model(&b.Task).Update("summary_mode", mode)
			db.Create(&model.SummaryParticipant{TaskID: b.Task.ID, UserID: "other", Status: model.ParticipantAccepted})
			setOptimizationPreview(t, db, f, "Optimized evidence [1]")
			seedEvidenceRow(t, db, "test-user", f.Session.AgentSessionID, "evidence", []pipeline.Message{{ChannelID: "private-channel", MessageSeq: 10, Timestamp: 1, Content: "PRIVATE EVIDENCE", SenderUID: "test-user", ChannelType: 1}})
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "private-citations"})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			result, err := queryDisplayResult(db, b.Task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.GetCitations()) != 1 || result.CitationOwnerID != "test-user" {
				t.Fatalf("missing provenance: %+v", result)
			}
			r := setupAgentSummaryRouter(h)
			eh := NewEditHandler(db)
			r.GET("/api/v1/summaries/:id/versions/:result_id", eh.GetSummaryVersion)
			r.GET("/api/v1/summaries/:id", NewTaskHandler(db, nil, "").GetSummary)
			for _, user := range []string{"test-user", "other"} {
				for _, path := range []string{fmt.Sprintf("/api/v1/summaries/%d", b.Task.ID), fmt.Sprintf("/api/v1/summaries/%d/versions/%d", b.Task.ID, result.ID)} {
					w = provenanceRequest(r, "GET", path, "", user)
					if w.Code != 200 {
						t.Fatal(w.Body.String())
					}
					if strings.Contains(w.Body.String(), "PRIVATE EVIDENCE") != (user == "test-user") {
						t.Fatalf("citation access %s: %s", user, w.Body.String())
					}
				}
				artifact, err := resolveReferencedArtifact(context.Background(), db, b.Task.ID, "test-space", user)
				if err != nil {
					t.Fatal(err)
				}
				if (len(artifact.Citations) > 0) != (user == "test-user") {
					t.Fatal("reference visibility mismatch")
				}
			}
		})
	}
}

func TestOptimizationDerivedSourcesExcludedAtWriteAndRead(t *testing.T) {
	db, h, f, b := seedOptimization(t, "team")
	hidden := model.SummarySource{TaskID: b.Task.ID, SourceType: model.SourceGroup, SourceID: "secret-channel", SourceName: "SECRET GROUP", Derived: true}
	db.Create(&hidden)
	var scope summaryWorkspaceContext
	json.Unmarshal([]byte(f.Session.ScopeJSON), &scope)
	scope.SelectedChannels = nil
	raw, _, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	db.Model(&f.Session).Update("scope_json", string(raw))
	w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "hidden-source"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	result, err := queryDisplayResult(db, b.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.SnapshotJSON == nil || strings.Contains(*result.SnapshotJSON, "secret-channel") || strings.Contains(*result.SnapshotJSON, "SECRET GROUP") {
		t.Fatal("derived provenance persisted")
	}
	// Simulate an old snapshot persisted before this guard existed.
	contaminated := `{"snapshot_version":1,"scope":{"channel_ids":["secret-channel"],"channel_names":["SECRET GROUP"],"sources":[{"source_id":"secret-channel","source_type":1,"source_name":"SECRET GROUP"}]}}`
	db.Model(&result).Update("snapshot_json", contaminated)
	r := setupAgentSummaryRouter(h)
	r.GET("/api/v1/summaries/:id", NewTaskHandler(db, nil, "").GetSummary)
	r.GET("/api/v1/summaries/:id/versions/:result_id", NewEditHandler(db).GetSummaryVersion)
	for _, path := range []string{fmt.Sprintf("/api/v1/summaries/%d", b.Task.ID), fmt.Sprintf("/api/v1/summaries/%d/versions/%d", b.Task.ID, result.ID)} {
		w = provenanceRequest(r, "GET", path, "", "test-user")
		if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET GROUP") || strings.Contains(w.Body.String(), "secret-channel") {
			t.Fatal(w.Body.String())
		}
	}
	artifact, err := resolveReferencedArtifact(context.Background(), db, b.Task.ID, "test-space", "test-user")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Snapshot.Scope.ChannelIDs) != 0 || len(artifact.Snapshot.Scope.Sources) != 0 {
		t.Fatal("reference leaked derived provenance")
	}
}

func TestOptimizationSkipsBusyPersonalMirror(t *testing.T) {
	for _, status := range []int{model.PersonalStatusPending, model.PersonalStatusProcessing} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "team")
			pr := model.PersonalResult{TaskID: b.Task.ID, UserID: "test-user", Content: "Busy original", WorkerStatus: status}
			db.Create(&pr)
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "busy-mirror"})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			db.First(&pr, pr.ID)
			var count int64
			db.Model(&model.PersonalResultVersion{}).Where("task_id = ?", b.Task.ID).Count(&count)
			if pr.Content != "Busy original" || pr.CurrentVersionID != nil || count != 0 || pr.WorkerStatus != status {
				t.Fatal("busy personal overwritten")
			}
		})
	}
}

func TestOptimizationRevisionIgnoresLifecycleButTracksContent(t *testing.T) {
	_, _, _, b := seedOptimization(t, "personal")
	before := b.revision()
	b.Personal.RetryCount++
	b.Personal.WorkflowStage = "refresh"
	b.Personal.WorkerStatus = model.PersonalStatusPending
	if b.revision() != before {
		t.Fatal("lifecycle-only change invalidated revision")
	}
	b.Personal.Content += "edited"
	if b.revision() == before {
		t.Fatal("content edit not detected")
	}
}

func TestOptimizationSetextNormalizationBothKinds(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, kind)
			setOptimizationPreview(t, db, f, "Lead-in paragraph\n---\n### Actual section\nBody")
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "setext-save"})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			current, err := loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, kind, false)
			if err != nil {
				t.Fatal(err)
			}
			content := current.Result.Content
			if kind == "personal" {
				content = current.Personal.Content
			}
			if strings.Contains(content, "paragraph\n---") || !strings.Contains(content, "### Actual section") {
				t.Fatalf("setext not normalized: %q", content)
			}
		})
	}
}

func TestPersonalDisplaySyncReassignsRawCitationOwner(t *testing.T) {
	db, _, _, b := seedOptimization(t, "team")
	db.Model(&b.Result).Update("citation_owner_id", "old-owner")
	b.Task.CurrentResultID = &b.Result.ID
	if err := syncSinglePersonalDisplay(db, b.Task, "test-user", "New [1]", `[{"index":1}]`, b.Result.GeneratedAt); err != nil {
		t.Fatal(err)
	}
	var result model.SummaryResult
	db.First(&result, b.Result.ID)
	if result.CitationOwnerID != "test-user" || callerPlainCitationsVisible(db, &b.Task, "old-owner", &result) || !callerPlainCitationsVisible(db, &b.Task, "test-user", &result) {
		t.Fatal("personal display inherited wrong owner")
	}
}

func TestTeamRefinePreservesExplicitCitationOwner(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			db, h, _, b := seedOptimization(t, "team")
			b.Result.CitationOwnerID = "test-user"
			b.Result.Content = "Old [1]"
			b.Result.SetCitations([]model.Citation{{Index: 1, Content: "private"}})
			if err := db.Save(&b.Result).Error; err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"New [1]\"}}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"content":"New [1]"},"finish_reason":"stop"}]}`)
				}
			}))
			defer srv.Close()
			eh := NewEditHandler(db, service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
			r := setupAgentSummaryRouter(h)
			handler := eh.RefineSummary
			if stream {
				handler = eh.RefineSummaryStream
			}
			r.POST("/api/v1/summaries/:id/refine", handler)
			w := provenanceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/refine", b.Task.ID), fmt.Sprintf(`{"feedback":"shorter","base_result_id":%d}`, b.Result.ID), "test-user")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			current, err := queryDisplayResult(db, b.Task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Version != 2 || current.CitationOwnerID != "test-user" || len(current.GetCitations()) != 1 || callerPlainCitationsVisible(db, &b.Task, "other", &current) {
				t.Fatalf("lost citation restriction: %+v %s", current, w.Body.String())
			}
		})
	}
}
