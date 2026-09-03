//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

// Pending-submission attention (multi-person "your turn to submit" red dot).
//
// has_pending_submission was already computed by ListSummaries but never fed
// needs_attention / attention_count, so the dot never rendered. These tests pin
// the three rules that make it a usable signal:
//
//  1. it only fires for MULTI-person tasks (single-person owners have nobody to
//     submit to),
//  2. it clears on submitted_at, not on read — MarkSummaryRead must leave it
//     standing (owner decision 2026-08-26),
//  3. it participates in the space-level attention_count that drives the
//     sidebar badge, with its own pending_submission_count breakdown.

type attentionListResponse struct {
	Code int `json:"code"`
	Data struct {
		Total                  int `json:"total"`
		AttentionCount         int `json:"attention_count"`
		UnreadCount            int `json:"unread_count"`
		PendingInvitationCount int `json:"pending_invitation_count"`
		PendingSubmissionCount int `json:"pending_submission_count"`
		Items                  []struct {
			TaskID               int64 `json:"task_id"`
			IsUnread             bool  `json:"is_unread"`
			HasPendingInvitation bool  `json:"has_pending_invitation"`
			HasPendingSubmission bool  `json:"has_pending_submission"`
			NeedsAttention       bool  `json:"needs_attention"`
		} `json:"items"`
	} `json:"data"`
}

func parseAttentionList(t *testing.T, w *httptest.ResponseRecorder) attentionListResponse {
	t.Helper()
	var resp attentionListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal list response: %v; body=%s", err, w.Body.String())
	}
	return resp
}

// seedPendingSubmitTask builds a task whose personal result for userID has
// finished generating but has not been submitted. extraParticipants controls
// whether the task is multi-person (the gate for the submission signal).
func seedPendingSubmitTask(t *testing.T, db *gorm.DB, taskNo, userID string, extraParticipants []string) int64 {
	t.Helper()
	now := timezone.Now()

	task := model.SummaryTask{
		TaskNo:      taskNo,
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusProcessing,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := db.Create(&model.SummaryParticipant{
		TaskID: task.ID, UserID: userID, UserName: userID, Status: model.ParticipantAccepted,
	}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	for _, uid := range extraParticipants {
		if err := db.Create(&model.SummaryParticipant{
			TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantAccepted,
		}).Error; err != nil {
			t.Fatalf("create participant %s: %v", uid, err)
		}
	}

	// Personal summary generated (worker_status=Completed) but not submitted.
	// No current_version_id / current_result_id is set, so is_unread stays false
	// and the assertions below isolate the submission signal.
	if err := db.Create(&model.PersonalResult{
		TaskID: task.ID, UserID: userID, Content: "personal draft",
		WorkerStatus: model.PersonalStatusCompleted, SubmittedAt: nil,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}

	return task.ID
}

func TestListSummaries_PendingSubmissionDrivesAttention(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	taskID := seedPendingSubmitTask(t, db, "SUBMIT-MULTI", "participant1", []string{"participant2"})

	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	w := doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseAttentionList(t, w)
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].TaskID != taskID {
		t.Fatalf("expected the seeded task in the list, got %+v", resp.Data)
	}
	item := resp.Data.Items[0]
	if !item.HasPendingSubmission {
		t.Fatalf("generated-but-unsubmitted personal result must flag pending submission: %+v", item)
	}
	if item.IsUnread || item.HasPendingInvitation {
		t.Fatalf("test isolates the submission signal; unread/invitation must be off: %+v", item)
	}
	if !item.NeedsAttention {
		t.Fatalf("pending submission must raise needs_attention (the card red dot): %+v", item)
	}
	if resp.Data.AttentionCount != 1 || resp.Data.PendingSubmissionCount != 1 {
		t.Fatalf("pending submission must feed the sidebar counts: attention=%d submission=%d",
			resp.Data.AttentionCount, resp.Data.PendingSubmissionCount)
	}
	if resp.Data.UnreadCount != 0 || resp.Data.PendingInvitationCount != 0 {
		t.Fatalf("submission must not leak into the unread/invitation breakdown: %+v", resp.Data)
	}
}

func TestListSummaries_SubmittedPersonalResultClearsAttention(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	taskID := seedPendingSubmitTask(t, db, "SUBMIT-DONE", "participant1", []string{"participant2"})
	now := timezone.Now()
	if err := db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, "participant1").
		Updates(map[string]interface{}{"submitted_at": now, "submit_source": model.SubmitSourceManual}).Error; err != nil {
		t.Fatalf("mark submitted: %v", err)
	}

	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	w := doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1")
	resp := parseAttentionList(t, w)
	if len(resp.Data.Items) != 1 {
		t.Fatalf("expected the seeded task, got %+v", resp.Data)
	}
	if resp.Data.Items[0].HasPendingSubmission || resp.Data.Items[0].NeedsAttention {
		t.Fatalf("submitting must clear the dot: %+v", resp.Data.Items[0])
	}
	if resp.Data.AttentionCount != 0 || resp.Data.PendingSubmissionCount != 0 {
		t.Fatalf("counts must drop after submit: attention=%d submission=%d",
			resp.Data.AttentionCount, resp.Data.PendingSubmissionCount)
	}
}

func TestListSummaries_SinglePersonTaskNeverPendsSubmission(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	// Sole participant: there is no team to submit into, so an unsubmitted
	// personal result is just the owner's own summary.
	seedPendingSubmitTask(t, db, "SUBMIT-SOLO", "participant1", nil)

	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	w := doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1")
	resp := parseAttentionList(t, w)
	if len(resp.Data.Items) != 1 {
		t.Fatalf("expected the seeded task, got %+v", resp.Data)
	}
	if resp.Data.Items[0].HasPendingSubmission || resp.Data.Items[0].NeedsAttention {
		t.Fatalf("single-person task must not pend submission: %+v", resp.Data.Items[0])
	}
	if resp.Data.AttentionCount != 0 || resp.Data.PendingSubmissionCount != 0 {
		t.Fatalf("single-person task must not raise counts: %+v", resp.Data)
	}
}

// A dead task must not keep nagging. The stuck-task reaper flips Processing ->
// Failed once retries are exhausted (worker/scheduler.go) and CancelSummary
// writes Cancelled; neither touches summary_personal_result, so without the
// status guard the member who DID generate their summary keeps a red dot and a
// non-zero space-level attention_count for a task nobody can revive
// (Regenerate and Delete are creator-gated). A badge that cannot return to zero
// is worse than no badge.
func TestListSummaries_TerminalTaskDropsPendingSubmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		taskNo string
		status int
	}{
		{"reaper marked it failed", "SUBMIT-FAILED", model.StatusFailed},
		{"creator cancelled it", "SUBMIT-CANCELLED", model.StatusCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, imDB := setupListTestDBs(t)
			taskID := seedPendingSubmitTask(t, db, tc.taskNo, "participant1", []string{"participant2"})
			if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
				Update("status", tc.status).Error; err != nil {
				t.Fatalf("set terminal status: %v", err)
			}

			r := setupListRouter(NewTaskHandler(db, imDB, ""))
			w := doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1")
			resp := parseAttentionList(t, w)
			if len(resp.Data.Items) != 1 {
				t.Fatalf("expected the seeded task, got %+v", resp.Data)
			}
			if resp.Data.Items[0].HasPendingSubmission || resp.Data.Items[0].NeedsAttention {
				t.Fatalf("a terminal task must not pend submission: %+v", resp.Data.Items[0])
			}
			if resp.Data.AttentionCount != 0 || resp.Data.PendingSubmissionCount != 0 {
				t.Fatalf("terminal task must not hold the badge above zero: %+v", resp.Data)
			}

			// MarkSummaryRead must agree — it is the other needs_attention producer.
			rr := setupReadRouter(NewTaskHandler(db, imDB, ""))
			now := timezone.Now()
			version := model.PersonalResultVersion{
				TaskID: taskID, UserID: "participant1", Content: "personal", Version: 1,
				GeneratedAt: now, CreatedAt: now, UpdatedAt: now,
			}
			if err := db.Create(&version).Error; err != nil {
				t.Fatalf("create personal version: %v", err)
			}
			if err := db.Model(&model.PersonalResult{}).
				Where("task_id = ? AND user_id = ?", taskID, "participant1").
				Update("current_version_id", version.ID).Error; err != nil {
				t.Fatalf("attach current version: %v", err)
			}
			rw := markReadRequest(rr, taskID, "participant1", fmt.Sprintf(`{"personal_version_id":%d}`, version.ID))
			var readResp struct {
				Data struct {
					HasPendingSubmission bool `json:"has_pending_submission"`
					NeedsAttention       bool `json:"needs_attention"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rw.Body.Bytes(), &readResp); err != nil {
				t.Fatalf("unmarshal mark-read response: %v; body=%s", err, rw.Body.String())
			}
			if readResp.Data.HasPendingSubmission || readResp.Data.NeedsAttention {
				t.Fatalf("mark-read must agree that a terminal task pends nothing: %+v", readResp.Data)
			}
		})
	}
}

// Completed is deliberately NOT guarded. A personal-only regenerate on a
// finished task leaves submitted_at NULL on purpose and waits for an explicit
// submit (worker/personal_processor.go), so the dot is correct there. Pinning
// this stops a future "just exclude every terminal status" simplification from
// silently dropping the signal on the one terminal status that still needs it.
func TestListSummaries_CompletedTaskStillPendsSubmission(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	taskID := seedPendingSubmitTask(t, db, "SUBMIT-REGEN", "participant1", []string{"participant2"})
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
		Update("status", model.StatusCompleted).Error; err != nil {
		t.Fatalf("set completed status: %v", err)
	}

	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	w := doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1")
	resp := parseAttentionList(t, w)
	if len(resp.Data.Items) != 1 {
		t.Fatalf("expected the seeded task, got %+v", resp.Data)
	}
	if !resp.Data.Items[0].HasPendingSubmission || !resp.Data.Items[0].NeedsAttention {
		t.Fatalf("a regenerated personal result on a completed task still owes a submit: %+v", resp.Data.Items[0])
	}
	if resp.Data.AttentionCount != 1 || resp.Data.PendingSubmissionCount != 1 {
		t.Fatalf("completed task must still hold the counts: %+v", resp.Data)
	}
}

// The status=waiting list filter carries the same predicate as the badge. If
// the two disagree, a card can be counted by the badge but hidden by the
// filter the user clicks to find it (or vice versa).
func TestListSummaries_WaitingFilterAgreesWithTerminalGuard(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	liveID := seedPendingSubmitTask(t, db, "SUBMIT-WAIT-LIVE", "participant1", []string{"participant2"})
	deadID := seedPendingSubmitTask(t, db, "SUBMIT-WAIT-DEAD", "participant1", []string{"participant2"})
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", deadID).
		Update("status", model.StatusFailed).Error; err != nil {
		t.Fatalf("set terminal status: %v", err)
	}

	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	w := doRequest(r, http.MethodGet, fmt.Sprintf("/api/v1/summaries?status=%d", model.StatusPending), "participant1")
	resp := parseAttentionList(t, w)
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].TaskID != liveID {
		t.Fatalf("waiting filter must keep the live task and drop the terminal one: %+v", resp.Data)
	}
}

// Owner decision 2026-08-26: reading is not submitting. Opening the detail page
// records the read cursor, but the participant still owes the team a /submit,
// so the dot must survive MarkSummaryRead.
func TestMarkSummaryRead_KeepsPendingSubmissionAttention(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	taskID := seedPendingSubmitTask(t, db, "SUBMIT-READ", "participant1", []string{"participant2"})

	now := timezone.Now()
	version := model.PersonalResultVersion{
		TaskID: taskID, UserID: "participant1", Content: "personal", Version: 1,
		GeneratedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&version).Error; err != nil {
		t.Fatalf("create personal version: %v", err)
	}
	if err := db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, "participant1").
		Update("current_version_id", version.ID).Error; err != nil {
		t.Fatalf("attach current version: %v", err)
	}

	r := setupReadRouter(NewTaskHandler(db, imDB, ""))
	w := markReadRequest(r, taskID, "participant1", fmt.Sprintf(`{"personal_version_id":%d}`, version.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			IsUnread             bool `json:"is_unread"`
			HasPendingInvitation bool `json:"has_pending_invitation"`
			HasPendingSubmission bool `json:"has_pending_submission"`
			NeedsAttention       bool `json:"needs_attention"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal mark-read response: %v; body=%s", err, w.Body.String())
	}
	if resp.Data.IsUnread {
		t.Fatalf("the rendered version was recorded, so content is read: %+v", resp.Data)
	}
	if !resp.Data.HasPendingSubmission || !resp.Data.NeedsAttention {
		t.Fatalf("reading must not clear the pending-submission dot: %+v", resp.Data)
	}

	// The list projection must agree with the read response — a client that
	// refreshes instead of trusting the optimistic update sees the same dot.
	lr := setupListRouter(NewTaskHandler(db, imDB, ""))
	lw := doRequest(lr, http.MethodGet, "/api/v1/summaries", "participant1")
	list := parseAttentionList(t, lw)
	if len(list.Data.Items) != 1 || !list.Data.Items[0].NeedsAttention || !list.Data.Items[0].HasPendingSubmission {
		t.Fatalf("list must keep the dot after read: %+v", list.Data)
	}
	if list.Data.AttentionCount != 1 || list.Data.PendingSubmissionCount != 1 {
		t.Fatalf("counts must survive read: attention=%d submission=%d",
			list.Data.AttentionCount, list.Data.PendingSubmissionCount)
	}
}
