package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// refineAgentSummaryReq is the request body for POST /api/v1/summaries/{task_id}/refine.
type refineAgentSummaryReq struct {
	Instruction string `json:"instruction"`
}

// RefineAgentSummary handles POST /api/v1/summaries/:task_id/refine.
//
// Error codes:
//   - 40001: task_id not found / task.trigger_type != agent
//   - 40002: current user is not the creator (unauthorized)
//   - 40003: instruction is empty or exceeds 1000 characters
//   - 40004: no available snapshot (old data or PersonalResult.SnapshotJSON is NULL)
//   - 50000: agent invocation failed / DB transaction failed
func (h *AgentSummaryHandler) RefineAgentSummary(c *gin.Context) {
	taskID := c.Param("task_id")
	userID := middleware.GetUserID(c)

	var req refineAgentSummaryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40003, Message: err.Error()})
		return
	}

	// Validate instruction
	if req.Instruction == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40003, Message: "instruction 不能为空"})
		return
	}
	if utf8.RuneCountInString(req.Instruction) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40003, Message: "instruction 超过 1000 字符"})
		return
	}

	// 1. Query SummaryTask
	var task model.SummaryTask
	if err := h.db.Where("id = ?", taskID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "task 不存在"})
			return
		}
		log.Printf("[handler] RefineAgentSummary query task failed task_id=%s: %v", taskID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "查询 task 失败: " + err.Error()})
		return
	}

	// 2. Validate: trigger_type == agent
	if task.TriggerType != model.TriggerAgent {
		c.JSON(http.StatusBadRequest, apiResponse{
			Code:    40001,
			Message: "只有 agent 生成的总结支持修改 (trigger_type != agent)",
		})
		return
	}

	// 3. Validate: current user is creator
	if task.CreatorID != userID {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40002, Message: "无权修改他人创建的总结"})
		return
	}

	// 4. Query the latest PersonalResult for this creator
	var pr model.PersonalResult
	err := h.db.Where("task_id = ? AND user_id = ?", task.ID, userID).
		Order("id DESC").
		First(&pr).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "该任务无可用快照(无 PersonalResult)"})
			return
		}
		log.Printf("[handler] RefineAgentSummary query PersonalResult failed task_id=%s user_id=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "查询 PersonalResult 失败: " + err.Error()})
		return
	}

	// 5. Validate: SnapshotJSON is not NULL
	snap := pr.GetSnapshot()
	if snap == nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "该任务无可用快照(snapshot_json 为空)"})
		return
	}

	// 6. Build refine message
	refineMessage := buildRefineMessage(snap, &pr, req.Instruction)

	// 7. Invoke agent (summary_refine profile)
	newSessionID := uuid.New().String()
	aReq := agent.Message{
		Profile:   "summary_refine",
		SessionID: newSessionID,
		Messages: []agent.Message{
			{Role: "user", Content: refineMessage},
		},
		CreatorUID: userID,
		SpaceID:    task.SpaceID,
	}
	aResp, err := buildRunnerForProfile(c.Request.Context(), aReq)
	if err != nil {
		log.Printf("[handler] RefineAgentSummary buildRunnerForProfile failed session=%s: %v", newSessionID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "Agent 调用失败: " + err.Error()})
		return
	}

	// Extract content from agent response (last assistant message)
	newContent := ""
	for i := len(aResp.Messages) - 1; i >= 0; i-- {
		if aResp.Messages[i].Role == "assistant" && aResp.Messages[i].Content != "" {
			newContent = aResp.Messages[i].Content
			break
		}
	}
	if newContent == "" {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "Agent 未产出内容"})
		return
	}

	// 8. Build citations for the refined content
	cits, cerr := h.buildCitationsForSession(c.Request.Context(), newSessionID, newContent, userID)
	if cerr != nil {
		log.Printf("[handler] RefineAgentSummary buildCitationsForSession failed session=%s: %v (fallback to empty)", newSessionID, cerr)
		cits = nil
	}

	// 9. Build new snapshot (increment version, link to parent)
	newVersion := 1
	oldVersion := 1
	instructionCopy := req.Instruction
	newSnap := &model.Snapshot{
		SnapshotVersion:       1,
		TaskID:                task.ID,
		Requirement:           snap.Requirement,
		Scope:                 snap.Scope,
		ToolSummary:           snap.ToolSummary,
		DataFreshnessNote:     snap.DataFreshnessNote,
		ParentSnapshotVersion: &oldVersion,
		UserInstruction:       &instructionCopy,
	}

	// 10. Insert new PersonalResult in a transaction
	now := time.Now()
	var newPRID int64
	txErr := h.db.Transaction(func(tx *gorm.DB) error {
		newPR := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: pr.ParticipantRefID,
			UserID:           userID,
			
			Content:          newContent,
			WorkerStatus:     model.PersonalStatusCompleted,
			GeneratedAt:      &now,
			SubmittedAt:      &now,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		newPR.SetCitations(cits)
		newPR.SetSnapshot(newSnap)

		if err := tx.Create(&newPR).Error; err != nil {
			return fmt.Errorf("create new PersonalResult: %w", err)
		}
		newPRID = newPR.ID

		// Update task updated_at (optional)
		if err := tx.Model(&task).Update("updated_at", now).Error; err != nil {
			return fmt.Errorf("update task updated_at: %w", err)
		}
		return nil
	})
	if txErr != nil {
		log.Printf("[handler] RefineAgentSummary tx failed task_id=%s user_id=%s: %v", taskID, userID, txErr)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "落库失败: " + txErr.Error()})
		return
	}

	log.Printf("[handler] RefineAgentSummary ok task_id=%s user_id=%s new_version=%d new_pr_id=%d session=%s",
		taskID, userID, newVersion, newPRID, newSessionID)

	// Response
	c.JSON(http.StatusOK, apiResponse{
		Code:    0,
		Message: "ok",
		Data: gin.H{
			"task_id":     task.ID,
			"new_version": newVersion,
			"content":     newContent,
			"citations":   cits,
		},
	})
}

// buildRefineMessage constructs the user message for summary_refine profile.
func buildRefineMessage(snap *model.Snapshot, pr *model.PersonalResult, instruction string) string {
	// Serialize snapshot metadata (exclude content/citations to avoid duplication)
	snapMeta := map[string]interface{}{
		"snapshot_version":        snap.SnapshotVersion,
		"task_id":                 snap.TaskID,
		"content_version":         snap.ContentVersion,
		"requirement":             snap.Requirement,
		"scope":                   snap.Scope,
		"tool_summary":            snap.ToolSummary,
		"data_freshness_note":     snap.DataFreshnessNote,
		"parent_snapshot_version": snap.ParentSnapshotVersion,
		"user_instruction":        snap.UserInstruction,
	}
	snapJSON, _ := json.MarshalIndent(snapMeta, "", "  ")

	msg := fmt.Sprintf(`【当前产物 v%d】
content:
%s

citations:
%s

【生成语境】(snapshot 元数据)
%s

【本轮修改需求】
%s`,
		snap.ContentVersion,
		pr.Content,
		pr.CitationsJSON,
		string(snapJSON),
		instruction,
	)
	return msg
}
