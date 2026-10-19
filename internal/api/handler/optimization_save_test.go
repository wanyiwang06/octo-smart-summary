//go:build cgo

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func seedOptimization(t *testing.T, kind string) (*gorm.DB, *AgentSummaryHandler, workspaceSaveFixture, optimizationBase) {
	t.Helper()
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(&model.PersonalResultVersion{}); err != nil {
		t.Fatal(err)
	}
	task := model.SummaryTask{TaskNo: "OPT-1", SpaceID: "test-space", CreatorID: "test-user", Title: "Original", Status: model.StatusCompleted, SummaryMode: model.ModeByPerson, TriggerType: model.TriggerAgent}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	p := model.SummaryParticipant{TaskID: task.ID, UserID: "test-user", Status: model.ParticipantAccepted}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if kind == "team" {
		if err := db.Create(&model.SummaryResult{TaskID: task.ID, Content: "Original content", Version: 1}).Error; err != nil {
			t.Fatal(err)
		}
	} else {
		if err := db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: p.ID, UserID: "test-user", Content: "Original content", WorkerStatus: model.PersonalStatusCompleted}).Error; err != nil {
			t.Fatal(err)
		}
	}
	f := seedWorkspaceSaveFixture(t, db, "optimize-session")
	var scope summaryWorkspaceContext
	if err := json.Unmarshal([]byte(f.Session.ScopeJSON), &scope); err != nil {
		t.Fatal(err)
	}
	scope.ReferencedTaskIDs = []int64{task.ID}
	scopeJSON, _, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&f.Session).Update("scope_json", string(scopeJSON)).Error; err != nil {
		t.Fatal(err)
	}
	b, err := loadOptimizationBase(db, "test-space", "test-user", task.ID, kind, false)
	if err != nil {
		t.Fatal(err)
	}
	f.Body["save_mode"] = "new_version"
	f.Body["target"] = optimizationTarget{task.ID, kind, b.revision()}
	return db, NewAgentSummaryHandler(db, nil, "", "", "", 0, 0), f, b
}

func TestOptimizationSaveVersionAndReplay(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, kind)
			r := setupAgentSummaryRouter(h)
			headers := map[string]string{"Idempotency-Key": "optimization-save"}
			first := doAgentSave(t, r, f.Body, headers)
			if first.Code != http.StatusOK {
				t.Fatalf("save: %d %s", first.Code, first.Body.String())
			}
			second := doAgentSave(t, r, f.Body, headers)
			if second.Code != http.StatusOK {
				t.Fatalf("replay: %d %s", second.Code, second.Body.String())
			}
			var count int64
			db.Model(&model.SummaryTask{}).Count(&count)
			if count != 1 {
				t.Fatalf("created extra tasks: %d", count)
			}
			current, err := loadOptimizationBase(db, "test-space", "test-user", b.Task.ID, kind, false)
			if err != nil {
				t.Fatal(err)
			}
			content := current.Result.Content
			if kind == "personal" {
				content = current.Personal.Content
			}
			var snapshot *model.Snapshot
			if kind == "personal" {
				snapshot = current.Personal.GetSnapshot()
			} else {
				snapshot = decodeResultSnapshot(current.Result.SnapshotJSON)
			}
			if snapshot == nil || snapshot.ContentVersion != 2 {
				t.Fatalf("missing version-specific generation snapshot: %#v", snapshot)
			}
			if content != "开场说明不应保存\n\n# 服务端可信总结正文" {
				t.Fatalf("wrong content: %q", content)
			}
			if kind == "team" {
				db.Model(&model.SummaryResult{}).Where("task_id = ?", b.Task.ID).Count(&count)
			} else {
				db.Model(&model.PersonalResultVersion{}).Where("task_id = ?", b.Task.ID).Count(&count)
			}
			if count != 2 {
				t.Fatalf("history count=%d", count)
			}
			f.Body["title"] = "different request"
			if response := doAgentSave(t, r, f.Body, headers); response.Code != 409 {
				t.Fatalf("changed replay: %d", response.Code)
			}
		})
	}
}

func TestOptimizationSaveProtectsEditsAndDraft(t *testing.T) {
	for _, kind := range []string{"team", "personal"} {
		t.Run(kind, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, kind)
			if kind == "team" {
				db.Model(&b.Result).Update("content", "Concurrent edit")
			} else {
				db.Model(&b.Personal).Update("content", "Concurrent edit")
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "conflict-save"})
			if w.Code != 409 {
				t.Fatalf("expected conflict: %d %s", w.Code, w.Body.String())
			}
			var session model.AgentSummarySession
			db.First(&session, f.Session.ID)
			if session.State != "preview_ready" {
				t.Fatalf("draft lost: %s", session.State)
			}
		})
	}
}

func TestOptimizationSaveAuthorization(t *testing.T) {
	for _, change := range []string{"space", "creator", "deleted", "reference", "status"} {
		t.Run(change, func(t *testing.T) {
			db, h, f, b := seedOptimization(t, "team")
			switch change {
			case "space":
				db.Model(&b.Task).Update("space_id", "other")
			case "creator":
				db.Model(&b.Task).Update("creator_id", "other")
			case "deleted":
				db.Delete(&b.Task)
			case "reference":
				db.Model(&f.Session).Update("scope_json", `{"selected_channels":[],"participants":[],"referenced_task_ids":[]}`)
			case "status":
				db.Model(&b.Task).Update("status", model.StatusPending)
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), f.Body, map[string]string{"Idempotency-Key": "denied-save"})
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("expected rejection: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
