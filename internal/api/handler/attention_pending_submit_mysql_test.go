//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// MySQL placeholder-ordering coverage for the pending-submission argument.
//
// The SQLite harness cannot exercise schedulePendingInvitationExpr: it needs
// JSON_TABLE, so on SQLite the predicate collapses to the constant "0" and
// contributes no placeholder. On MySQL it contributes one `?` that sits
// immediately BEFORE the new PersonalStatusCompleted argument in every
// hand-assembled statement. A mutation that swapped those two would pass the
// whole SQLite suite and only break in production.
//
// schedule_attention_mysql_test.go already pins the list query's ordering. This
// covers the two statements it does not touch: the attention-count query (whose
// new has_submit term is a third placeholder position) and MarkSummaryRead's
// stateSQL (which had no MySQL coverage at all).
//
// Opt-in like its sibling: point SUMMARY_MYSQL_TEST_DSN at an isolated,
// migrated database.
func TestPendingSubmission_MySQL_PlaceholderOrderingAndMarkRead(t *testing.T) {
	dsn := os.Getenv("SUMMARY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_MYSQL_TEST_DSN is not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	for _, table := range []string{
		"summary_task", "summary_source", "summary_participant", "summary_result",
		"summary_personal_result", "summary_personal_result_version", "summary_user_read", "summary_schedule",
	} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("mysql integration database is not migrated: missing %s", table)
		}
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	const spaceID = "pending-submit-mysql-space"
	const me = "submit-owner"
	now := timezone.Now()

	// A schedule whose roster still has an unconfirmed member makes
	// schedulePendingInvitationExpr live (confirm_policy=1), so its placeholder
	// really is bound during this test rather than short-circuiting.
	schedule := model.SummarySchedule{
		SpaceID: spaceID, CreatorID: me, Title: "scheduled", SummaryMode: model.ModeByPerson,
		CronExpr: "0 0 * * *", TimeRangeType: 1,
		ParticipantConfig: model.JSON(`{"participants":[{"user_id":"other-invitee","confirmed":false}],"confirm_gate_passed":false}`),
		ConfirmPolicy:     model.SchedConfirmRequire, IsActive: 1,
	}
	if err := tx.Create(&schedule).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	task := model.SummaryTask{
		TaskNo: "MYSQL-PENDING-SUBMIT", SpaceID: spaceID, CreatorID: me,
		SummaryMode: model.ModeByPerson, Status: model.StatusProcessing,
		TriggerType: model.TriggerScheduled, ScheduleID: &schedule.ID,
		TimeRangeStart: time.Now().Add(-time.Hour), TimeRangeEnd: time.Now(),
	}
	if err := tx.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	for _, uid := range []string{me, "teammate"} {
		if err := tx.Create(&model.SummaryParticipant{
			TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantAccepted,
		}).Error; err != nil {
			t.Fatalf("create participant %s: %v", uid, err)
		}
	}
	version := model.PersonalResultVersion{
		TaskID: task.ID, UserID: me, Content: "personal", Version: 1,
		GeneratedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&version).Error; err != nil {
		t.Fatalf("create personal version: %v", err)
	}
	if err := tx.Create(&model.PersonalResult{
		TaskID: task.ID, UserID: me, Content: "personal draft",
		WorkerStatus: model.PersonalStatusCompleted, SubmittedAt: nil,
		CurrentVersionID: &version.ID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}

	// --- list projection + attention-count query ---
	r := setupListRouter(NewTaskHandler(tx, nil, ""))
	w := doRequestWithSpace(r, http.MethodGet, "/api/v1/summaries", me, spaceID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseAttentionList(t, w)
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].TaskID != task.ID {
		t.Fatalf("expected the seeded task: %+v", resp.Data)
	}
	if !resp.Data.Items[0].HasPendingSubmission || !resp.Data.Items[0].NeedsAttention {
		t.Fatalf("MySQL list projection lost the pending submission: %+v", resp.Data.Items[0])
	}
	// A wrong argument position would not necessarily error — it would silently
	// compare worker_status against a uid, yielding 0. Assert the value.
	if resp.Data.PendingSubmissionCount != 1 || resp.Data.AttentionCount != 1 {
		t.Fatalf("MySQL attention-count placeholders are misaligned: attention=%d submission=%d",
			resp.Data.AttentionCount, resp.Data.PendingSubmissionCount)
	}

	// --- MarkSummaryRead stateSQL ---
	rr := setupReadRouter(NewTaskHandler(tx, nil, ""))
	rw := markReadRequestWithSpace(rr, task.ID, me, spaceID, fmt.Sprintf(`{"personal_version_id":%d}`, version.ID))
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 from mark-read, got %d: %s", rw.Code, rw.Body.String())
	}
	var readResp struct {
		Data struct {
			IsUnread             bool `json:"is_unread"`
			HasPendingSubmission bool `json:"has_pending_submission"`
			NeedsAttention       bool `json:"needs_attention"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &readResp); err != nil {
		t.Fatalf("unmarshal mark-read response: %v; body=%s", err, rw.Body.String())
	}
	if readResp.Data.IsUnread {
		t.Fatalf("the rendered version was recorded: %+v", readResp.Data)
	}
	if !readResp.Data.HasPendingSubmission || !readResp.Data.NeedsAttention {
		t.Fatalf("MySQL stateSQL lost the pending submission on read: %+v", readResp.Data)
	}
}
