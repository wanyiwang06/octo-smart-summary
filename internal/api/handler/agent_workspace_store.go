package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrWorkspaceScopeConflict   = errors.New("summary workspace scope conflict")
	ErrWorkspaceRequestMismatch = errors.New("summary workspace request id reused with a different request")
	ErrWorkspaceProposalStale   = errors.New("summary workspace proposal is stale")
	ErrWorkspacePreviewStale    = errors.New("summary workspace preview is stale")
	ErrWorkspaceTurnLeaseLost   = errors.New("summary workspace turn lease was lost")
)

type WorkspaceSessionKey struct {
	SpaceID   string
	UserID    string
	SessionID string
}

type WorkspaceTurnDisposition string

const (
	WorkspaceTurnAcquired   WorkspaceTurnDisposition = "acquired"
	WorkspaceTurnReplay     WorkspaceTurnDisposition = "replay"
	WorkspaceTurnInProgress WorkspaceTurnDisposition = "in_progress"
)

type WorkspaceBeginTurnInput struct {
	Key           WorkspaceSessionKey
	RequestID     string
	RequestHash   string
	ScopeVersion  int
	ScopeJSON     []byte
	ScopeHash     string
	RunID         string
	LeaseDuration time.Duration
}

type WorkspaceBeginTurnResult struct {
	Disposition WorkspaceTurnDisposition
	Session     model.AgentSummarySession
	Turn        model.AgentSummaryTurn
	Snapshot    WorkspaceSnapshot
}

type WorkspacePersistMessage struct {
	Message         agent.Message
	ResultType      string
	ResponsePayload json.RawMessage
	ScopeVersion    int
	SnapshotVersion int
	ParentMessageID int64
}

type WorkspaceProposalMutation struct {
	JSON  json.RawMessage
	Token string
}

type WorkspaceWorkflowMutation struct {
	TaskID   int64
	Scope    string
	Terminal bool
}

type WorkspaceTurnCompletion struct {
	Key             WorkspaceSessionKey
	TurnID          int64
	Attempt         int
	RunID           string
	Messages        []WorkspacePersistMessage
	ResultType      string
	ResponsePayload json.RawMessage
	ScopeVersion    int
	SnapshotVersion int
	ParentMessageID int64
	Proposal        *WorkspaceProposalMutation
	Workflow        *WorkspaceWorkflowMutation
}

type WorkspaceTurnFailure struct {
	Key       WorkspaceSessionKey
	TurnID    int64
	Attempt   int
	ErrorCode string
}

type WorkspaceSnapshot struct {
	Session        model.AgentSummarySession
	Messages       []model.AgentMessage
	CurrentPreview *model.AgentMessage
}

type WorkspaceProposalValidation struct {
	Key             WorkspaceSessionKey
	ProposalVersion int
	ProposalToken   string
	ScopeVersion    int
}

type WorkspaceProposalConfirmationInput struct {
	Begin           WorkspaceBeginTurnInput
	ProposalVersion int
	ProposalToken   string
}

type WorkspaceWorkflowReconcile struct {
	Key           WorkspaceSessionKey
	TaskID        int64
	ScopeVersion  int
	ResultType    string
	MessageID     int64
	Reply         string
	ClearWorkflow bool
}

type WorkspacePreviewSave struct {
	Key                     WorkspaceSessionKey
	MessageID               int64
	ScopeVersion            int
	ExpectedArtifactVersion int
	SnapshotVersion         int
	TaskID                  int64
}

type AgentWorkspaceStore struct {
	db *gorm.DB
}

func NewAgentWorkspaceStore(db *gorm.DB) *AgentWorkspaceStore {
	return &AgentWorkspaceStore{db: db}
}

func (s *AgentWorkspaceStore) BeginTurn(ctx context.Context, in WorkspaceBeginTurnInput) (WorkspaceBeginTurnResult, error) {
	if s == nil || s.db == nil {
		return WorkspaceBeginTurnResult{}, errors.New("workspace database is required")
	}
	if err := validateWorkspaceBegin(in); err != nil {
		return WorkspaceBeginTurnResult{}, err
	}
	lease := in.LeaseDuration
	if lease <= 0 {
		lease = summaryWorkspaceTurnLease
	}
	now := time.Now()
	leaseUntil := now.Add(lease)
	result := WorkspaceBeginTurnResult{}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		session, err := lockOrCreateWorkspaceSession(tx, in, now)
		if err != nil {
			return err
		}
		if in.ScopeVersion < session.ScopeVersion || (in.ScopeVersion == session.ScopeVersion && session.ScopeHash != "" && session.ScopeHash != in.ScopeHash) {
			return ErrWorkspaceScopeConflict
		}

		var existing model.AgentSummaryTurn
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ? AND request_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID, in.RequestID).
			Take(&existing).Error
		hasExisting := err == nil
		if err == nil {
			if existing.RequestHash != in.RequestHash {
				return ErrWorkspaceRequestMismatch
			}
			if existing.Status == "completed" {
				snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Key)
				if snapErr != nil {
					return snapErr
				}
				result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnReplay, Session: snapshot.Session, Turn: existing, Snapshot: snapshot}
				return nil
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if session.ActiveTurnID > 0 {
			var active model.AgentSummaryTurn
			activeErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", session.ActiveTurnID).Take(&active).Error
			if activeErr == nil && active.Status == "running" && active.LeaseExpiresAt != nil && active.LeaseExpiresAt.After(now) {
				snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Key)
				if snapErr != nil {
					return snapErr
				}
				result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnInProgress, Session: snapshot.Session, Turn: active, Snapshot: snapshot}
				return nil
			}
			if activeErr != nil && !errors.Is(activeErr, gorm.ErrRecordNotFound) {
				return activeErr
			}
		}
		if hasExisting && existing.Status == "running" && existing.LeaseExpiresAt != nil && existing.LeaseExpiresAt.After(now) {
			snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Key)
			if snapErr != nil {
				return snapErr
			}
			result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnInProgress, Session: snapshot.Session, Turn: existing, Snapshot: snapshot}
			return nil
		}

		if in.ScopeVersion > session.ScopeVersion {
			updates := map[string]interface{}{
				"agent_session_id":               summaryWorkspaceAgentSessionID(in.Key.SpaceID, in.Key.SessionID, in.ScopeVersion),
				"scope_version":                  in.ScopeVersion,
				"scope_json":                     string(in.ScopeJSON),
				"scope_hash":                     in.ScopeHash,
				"latest_preview_message_id":      0,
				"latest_preview_saved_task_id":   0,
				"pending_proposal_status":        "",
				"pending_proposal_token":         "",
				"pending_proposal_json":          nil,
				"pending_proposal_message_id":    0,
				"pending_proposal_scope_version": 0,
				"pending_proposal_task_id":       0,
				"workflow_task_id":               0,
				"workflow_scope":                 "",
				"workflow_scope_version":         0,
				"workflow_started_message_id":    0,
				"workflow_terminal_message_id":   0,
				"updated_at":                     now,
			}
			if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).Updates(updates).Error; err != nil {
				return err
			}
			if err := tx.Where("id = ?", session.ID).Take(&session).Error; err != nil {
				return err
			}
		}

		if hasExisting {
			existing.Status = "running"
			existing.Attempt++
			existing.LeaseExpiresAt = &leaseUntil
			existing.ErrorCode = ""
			existing.RunID = in.RunID
			existing.UpdatedAt = now
			if err := tx.Save(&existing).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).
				Updates(map[string]interface{}{"active_turn_id": existing.ID, "state": "running", "updated_at": now}).Error; err != nil {
				return err
			}
			snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Key)
			if snapErr != nil {
				return snapErr
			}
			result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnAcquired, Session: snapshot.Session, Turn: existing, Snapshot: snapshot}
			return nil
		}

		turn := model.AgentSummaryTurn{
			SpaceID:        in.Key.SpaceID,
			UserID:         in.Key.UserID,
			SessionID:      in.Key.SessionID,
			RequestID:      in.RequestID,
			RequestHash:    in.RequestHash,
			ScopeVersion:   in.ScopeVersion,
			Status:         "running",
			Attempt:        1,
			LeaseExpiresAt: &leaseUntil,
			RunID:          in.RunID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).
			Updates(map[string]interface{}{"active_turn_id": turn.ID, "state": "running", "updated_at": now}).Error; err != nil {
			return err
		}
		snapshot, err := loadWorkspaceSnapshotTx(tx, in.Key)
		if err != nil {
			return err
		}
		result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnAcquired, Session: snapshot.Session, Turn: turn, Snapshot: snapshot}
		return nil
	})
	return result, err
}

func validateWorkspaceBegin(in WorkspaceBeginTurnInput) error {
	if strings.TrimSpace(in.Key.SpaceID) == "" || strings.TrimSpace(in.Key.UserID) == "" || strings.TrimSpace(in.Key.SessionID) == "" {
		return errors.New("workspace session key is incomplete")
	}
	if strings.TrimSpace(in.RequestID) == "" || strings.TrimSpace(in.RequestHash) == "" || in.ScopeVersion <= 0 || len(in.ScopeJSON) == 0 || strings.TrimSpace(in.ScopeHash) == "" {
		return errors.New("workspace turn input is incomplete")
	}
	if !json.Valid(in.ScopeJSON) {
		return errors.New("workspace scope is not valid JSON")
	}
	return nil
}

func lockOrCreateWorkspaceSession(tx *gorm.DB, in WorkspaceBeginTurnInput, now time.Time) (model.AgentSummarySession, error) {
	var session model.AgentSummarySession
	query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID)
	if err := query.Take(&session).Error; err == nil {
		return session, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return session, err
	}
	session = model.AgentSummarySession{
		SpaceID:         in.Key.SpaceID,
		UserID:          in.Key.UserID,
		SessionID:       in.Key.SessionID,
		AgentSessionID:  summaryWorkspaceAgentSessionID(in.Key.SpaceID, in.Key.SessionID, in.ScopeVersion),
		ContractVersion: summaryWorkspaceContractVersion,
		State:           "idle",
		StateVersion:    1,
		ScopeVersion:    in.ScopeVersion,
		ScopeJSON:       string(in.ScopeJSON),
		ScopeHash:       in.ScopeHash,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&session).Error; err != nil {
		return session, err
	}
	// Always re-read the unique row under lock. MySQL CLIENT_FOUND_ROWS may
	// report a conflict/no-op insert as affected, so RowsAffected cannot tell
	// whether session contains the persisted row (including its primary key).
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
		Take(&session).Error; err != nil {
		return session, err
	}
	return session, nil
}

func (s *AgentWorkspaceStore) CompleteTurn(ctx context.Context, in WorkspaceTurnCompletion) (WorkspaceSnapshot, error) {
	if s == nil || s.db == nil {
		return WorkspaceSnapshot{}, errors.New("workspace database is required")
	}
	var snapshot WorkspaceSnapshot
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.AgentSummarySession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&session).Error; err != nil {
			return err
		}
		var turn model.AgentSummaryTurn
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND space_id = ? AND user_id = ? AND session_id = ?", in.TurnID, in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&turn).Error; err != nil {
			return err
		}
		if in.Attempt <= 0 || turn.Attempt != in.Attempt {
			return ErrWorkspaceTurnLeaseLost
		}
		if turn.Status == "completed" {
			var err error
			snapshot, err = loadWorkspaceSnapshotTx(tx, in.Key)
			return err
		}
		if turn.Status != "running" || session.ActiveTurnID != turn.ID || in.ScopeVersion != session.ScopeVersion {
			return ErrWorkspaceScopeConflict
		}

		rows := make([]model.AgentMessage, 0, len(in.Messages))
		finalIndex := -1
		for i, item := range in.Messages {
			row, err := workspaceMessageRow(in.Key, turn.ID, item)
			if err != nil {
				return err
			}
			if item.ResultType != "" {
				finalIndex = i
			}
			rows = append(rows, row)
		}
		if finalIndex < 0 {
			return errors.New("workspace completion has no final assistant message")
		}

		artifactVersion := session.ArtifactVersion
		if in.ResultType == workspaceResultAgentPreview || in.ResultType == workspaceResultAgentRevision {
			artifactVersion++
			if artifactVersion <= 0 {
				artifactVersion = 1
			}
			rows[finalIndex].ArtifactVersion = artifactVersion
			rows[finalIndex].SnapshotVersion = in.SnapshotVersion
			rows[finalIndex].ParentMessageID = in.ParentMessageID
		}
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		responseMessageID := rows[finalIndex].ID
		now := time.Now()
		updates := map[string]interface{}{
			"state":          in.ResultType,
			"state_version":  gorm.Expr("state_version + 1"),
			"active_turn_id": 0,
			"updated_at":     now,
		}
		if in.ResultType == workspaceResultAgentPreview || in.ResultType == workspaceResultAgentRevision {
			updates["artifact_version"] = artifactVersion
			updates["latest_preview_message_id"] = responseMessageID
			updates["latest_preview_saved_task_id"] = 0
			updates["pending_proposal_status"] = ""
			updates["pending_proposal_token"] = ""
			updates["pending_proposal_json"] = nil
			updates["pending_proposal_message_id"] = 0
			updates["pending_proposal_scope_version"] = 0
			updates["pending_proposal_task_id"] = 0
			updates["workflow_task_id"] = 0
			updates["workflow_scope"] = ""
			updates["workflow_scope_version"] = 0
			updates["workflow_started_message_id"] = 0
			updates["workflow_terminal_message_id"] = 0
		}
		if in.Proposal != nil {
			updates["latest_preview_message_id"] = 0
			updates["latest_preview_saved_task_id"] = 0
			updates["workflow_task_id"] = 0
			updates["workflow_scope"] = ""
			updates["workflow_scope_version"] = 0
			updates["workflow_started_message_id"] = 0
			updates["workflow_terminal_message_id"] = 0
			token := strings.TrimSpace(in.Proposal.Token)
			if token == "" {
				var tokenErr error
				token, tokenErr = workspaceStoreToken()
				if tokenErr != nil {
					return tokenErr
				}
			}
			updates["pending_proposal_version"] = session.PendingProposalVersion + 1
			updates["pending_proposal_status"] = "pending"
			updates["pending_proposal_token"] = token
			updates["pending_proposal_json"] = string(in.Proposal.JSON)
			updates["pending_proposal_message_id"] = responseMessageID
			updates["pending_proposal_scope_version"] = in.ScopeVersion
			updates["pending_proposal_task_id"] = 0
		}
		if in.Workflow != nil {
			updates["latest_preview_message_id"] = 0
			updates["latest_preview_saved_task_id"] = 0
			updates["workflow_task_id"] = in.Workflow.TaskID
			updates["workflow_scope"] = in.Workflow.Scope
			updates["workflow_scope_version"] = in.ScopeVersion
			if in.Workflow.Scope == "team" {
				updates["pending_proposal_status"] = "confirmed"
				updates["pending_proposal_task_id"] = in.Workflow.TaskID
			} else {
				updates["pending_proposal_status"] = ""
				updates["pending_proposal_token"] = ""
				updates["pending_proposal_json"] = nil
				updates["pending_proposal_message_id"] = 0
				updates["pending_proposal_scope_version"] = 0
				updates["pending_proposal_task_id"] = 0
			}
			if in.Workflow.Terminal {
				updates["workflow_started_message_id"] = 0
				updates["workflow_terminal_message_id"] = responseMessageID
			} else {
				updates["workflow_started_message_id"] = responseMessageID
				updates["workflow_terminal_message_id"] = 0
			}
		}
		if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.AgentSummaryTurn{}).Where("id = ?", turn.ID).Updates(map[string]interface{}{
			"status":              "completed",
			"lease_expires_at":    nil,
			"run_id":              in.RunID,
			"response_message_id": responseMessageID,
			"result_type":         in.ResultType,
			"response_json":       string(in.ResponsePayload),
			"completed_at":        now,
			"updated_at":          now,
		}).Error; err != nil {
			return err
		}
		var err error
		snapshot, err = loadWorkspaceSnapshotTx(tx, in.Key)
		return err
	})
	return snapshot, err
}

func workspaceMessageRow(key WorkspaceSessionKey, turnID int64, item WorkspacePersistMessage) (model.AgentMessage, error) {
	row := model.AgentMessage{
		SpaceID:         key.SpaceID,
		SessionID:       key.SessionID,
		UserID:          key.UserID,
		TurnID:          turnID,
		Role:            item.Message.Role,
		Content:         item.Message.Content,
		ToolCallID:      item.Message.ToolCallID,
		Name:            item.Message.Name,
		RunID:           item.Message.RunID,
		OutputTruncated: item.Message.OutputTruncated,
		ResultType:      item.ResultType,
		ScopeVersion:    item.ScopeVersion,
		SnapshotVersion: item.SnapshotVersion,
		ParentMessageID: item.ParentMessageID,
		CreatedAt:       time.Now(),
	}
	if len(item.Message.ToolCalls) > 0 {
		data, err := json.Marshal(item.Message.ToolCalls)
		if err != nil {
			return row, err
		}
		value := string(data)
		row.ToolCalls = &value
	}
	if len(item.ResponsePayload) > 0 {
		value := string(item.ResponsePayload)
		row.ResponsePayload = &value
	}
	return row, nil
}

func (s *AgentWorkspaceStore) FailTurn(ctx context.Context, in WorkspaceTurnFailure) error {
	if s == nil || s.db == nil {
		return errors.New("workspace database is required")
	}
	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.AgentSummarySession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&session).Error; err != nil {
			return err
		}
		var turn model.AgentSummaryTurn
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND space_id = ? AND user_id = ? AND session_id = ?", in.TurnID, in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&turn).Error; err != nil {
			return err
		}
		if in.Attempt <= 0 || turn.Attempt != in.Attempt || turn.Status != "running" || session.ActiveTurnID != turn.ID {
			return ErrWorkspaceTurnLeaseLost
		}
		if err := tx.Model(&model.AgentSummaryTurn{}).Where("id = ? AND attempt = ? AND status = 'running'", turn.ID, in.Attempt).
			Updates(map[string]interface{}{"status": "failed", "error_code": in.ErrorCode, "lease_expires_at": nil, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&model.AgentSummarySession{}).Where("id = ? AND active_turn_id = ?", session.ID, turn.ID).
			Updates(map[string]interface{}{"active_turn_id": 0, "state": "error", "updated_at": now}).Error
	})
}

func (s *AgentWorkspaceStore) LoadSnapshot(ctx context.Context, key WorkspaceSessionKey) (WorkspaceSnapshot, error) {
	if s == nil || s.db == nil {
		return WorkspaceSnapshot{}, errors.New("workspace database is required")
	}
	return loadWorkspaceSnapshotTx(s.db.WithContext(ctx), key)
}

func loadWorkspaceSnapshotTx(db *gorm.DB, key WorkspaceSessionKey) (WorkspaceSnapshot, error) {
	var snapshot WorkspaceSnapshot
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", key.SpaceID, key.UserID, key.SessionID).
		Take(&snapshot.Session).Error; err != nil {
		return snapshot, err
	}
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", key.SpaceID, key.UserID, key.SessionID).
		Order("id ASC").Find(&snapshot.Messages).Error; err != nil {
		return snapshot, err
	}
	if snapshot.Session.LatestPreviewMessageID > 0 {
		var preview model.AgentMessage
		if err := db.Where("id = ? AND space_id = ? AND user_id = ? AND session_id = ?", snapshot.Session.LatestPreviewMessageID, key.SpaceID, key.UserID, key.SessionID).
			Take(&preview).Error; err != nil {
			return snapshot, err
		}
		snapshot.CurrentPreview = &preview
	}
	return snapshot, nil
}

func (s *AgentWorkspaceStore) LoadHistory(ctx context.Context, key WorkspaceSessionKey) ([]agent.Message, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("workspace database is required")
	}
	var session model.AgentSummarySession
	if err := s.db.WithContext(ctx).
		Select("scope_version").
		Where("space_id = ? AND user_id = ? AND session_id = ?", key.SpaceID, key.UserID, key.SessionID).
		Take(&session).Error; err != nil {
		return nil, err
	}
	var rows []model.AgentMessage
	if err := s.db.WithContext(ctx).
		Where("space_id = ? AND user_id = ? AND session_id = ? AND scope_version = ?", key.SpaceID, key.UserID, key.SessionID, session.ScopeVersion).
		Order("id DESC").Limit(maxHistoryRows).Find(&rows).Error; err != nil {
		return nil, err
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	messages := make([]agent.Message, 0, len(rows))
	for _, row := range rows {
		message := agent.Message{
			Role:            row.Role,
			Content:         row.Content,
			ToolCallID:      row.ToolCallID,
			Name:            row.Name,
			RunID:           row.RunID,
			OutputTruncated: row.OutputTruncated,
		}
		if row.ToolCalls != nil && *row.ToolCalls != "" {
			if err := json.Unmarshal([]byte(*row.ToolCalls), &message.ToolCalls); err != nil {
				return nil, err
			}
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// BeginProposalConfirmation atomically validates a pending proposal and
// acquires the idempotent confirmation turn. A completed turn is replayed
// without requiring the proposal to remain pending, because the first
// successful completion transitions it to confirmed.
func (s *AgentWorkspaceStore) BeginProposalConfirmation(ctx context.Context, in WorkspaceProposalConfirmationInput) (WorkspaceBeginTurnResult, error) {
	if s == nil || s.db == nil {
		return WorkspaceBeginTurnResult{}, errors.New("workspace database is required")
	}
	if err := validateWorkspaceBegin(in.Begin); err != nil {
		return WorkspaceBeginTurnResult{}, err
	}
	if in.ProposalVersion <= 0 || strings.TrimSpace(in.ProposalToken) == "" {
		return WorkspaceBeginTurnResult{}, errors.New("workspace proposal confirmation is incomplete")
	}
	lease := in.Begin.LeaseDuration
	if lease <= 0 {
		lease = summaryWorkspaceTurnLease
	}
	now := time.Now()
	leaseUntil := now.Add(lease)
	result := WorkspaceBeginTurnResult{}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.AgentSummarySession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ?", in.Begin.Key.SpaceID, in.Begin.Key.UserID, in.Begin.Key.SessionID).
			Take(&session).Error; err != nil {
			return err
		}

		var existing model.AgentSummaryTurn
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ? AND request_id = ?", in.Begin.Key.SpaceID, in.Begin.Key.UserID, in.Begin.Key.SessionID, in.Begin.RequestID).
			Take(&existing).Error
		hasExisting := err == nil
		if err == nil {
			if existing.RequestHash != in.Begin.RequestHash {
				return ErrWorkspaceRequestMismatch
			}
			if existing.Status == "completed" {
				if session.ScopeVersion != in.Begin.ScopeVersion || session.ScopeHash != in.Begin.ScopeHash {
					return ErrWorkspaceScopeConflict
				}
				snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Begin.Key)
				if snapErr != nil {
					return snapErr
				}
				result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnReplay, Session: snapshot.Session, Turn: existing, Snapshot: snapshot}
				return nil
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if session.ScopeVersion != in.Begin.ScopeVersion || session.ScopeHash != in.Begin.ScopeHash ||
			session.PendingProposalStatus != "pending" ||
			session.PendingProposalVersion != in.ProposalVersion ||
			session.PendingProposalToken != strings.TrimSpace(in.ProposalToken) ||
			session.PendingProposalScopeVersion != in.Begin.ScopeVersion {
			return ErrWorkspaceProposalStale
		}

		if session.ActiveTurnID > 0 {
			var active model.AgentSummaryTurn
			activeErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", session.ActiveTurnID).Take(&active).Error
			if activeErr == nil && active.Status == "running" && active.LeaseExpiresAt != nil && active.LeaseExpiresAt.After(now) {
				snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Begin.Key)
				if snapErr != nil {
					return snapErr
				}
				result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnInProgress, Session: snapshot.Session, Turn: active, Snapshot: snapshot}
				return nil
			}
			if activeErr != nil && !errors.Is(activeErr, gorm.ErrRecordNotFound) {
				return activeErr
			}
		}
		if hasExisting && existing.Status == "running" && existing.LeaseExpiresAt != nil && existing.LeaseExpiresAt.After(now) {
			snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Begin.Key)
			if snapErr != nil {
				return snapErr
			}
			result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnInProgress, Session: snapshot.Session, Turn: existing, Snapshot: snapshot}
			return nil
		}

		if hasExisting {
			existing.Status = "running"
			existing.Attempt++
			existing.LeaseExpiresAt = &leaseUntil
			existing.ErrorCode = ""
			existing.RunID = in.Begin.RunID
			existing.UpdatedAt = now
			if err := tx.Save(&existing).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).
				Updates(map[string]interface{}{"active_turn_id": existing.ID, "state": "running", "updated_at": now}).Error; err != nil {
				return err
			}
			snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Begin.Key)
			if snapErr != nil {
				return snapErr
			}
			result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnAcquired, Session: snapshot.Session, Turn: existing, Snapshot: snapshot}
			return nil
		}

		turn := model.AgentSummaryTurn{
			SpaceID:        in.Begin.Key.SpaceID,
			UserID:         in.Begin.Key.UserID,
			SessionID:      in.Begin.Key.SessionID,
			RequestID:      in.Begin.RequestID,
			RequestHash:    in.Begin.RequestHash,
			ScopeVersion:   in.Begin.ScopeVersion,
			Status:         "running",
			Attempt:        1,
			LeaseExpiresAt: &leaseUntil,
			RunID:          in.Begin.RunID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).
			Updates(map[string]interface{}{"active_turn_id": turn.ID, "state": "running", "updated_at": now}).Error; err != nil {
			return err
		}
		snapshot, snapErr := loadWorkspaceSnapshotTx(tx, in.Begin.Key)
		if snapErr != nil {
			return snapErr
		}
		result = WorkspaceBeginTurnResult{Disposition: WorkspaceTurnAcquired, Session: snapshot.Session, Turn: turn, Snapshot: snapshot}
		return nil
	})
	return result, err
}

func (s *AgentWorkspaceStore) ValidateProposal(ctx context.Context, in WorkspaceProposalValidation) (model.AgentSummarySession, error) {
	if s == nil || s.db == nil {
		return model.AgentSummarySession{}, errors.New("workspace database is required")
	}
	var session model.AgentSummarySession
	if err := s.db.WithContext(ctx).
		Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
		Take(&session).Error; err != nil {
		return session, err
	}
	if session.PendingProposalStatus != "pending" || session.PendingProposalVersion != in.ProposalVersion ||
		session.PendingProposalToken != strings.TrimSpace(in.ProposalToken) ||
		session.PendingProposalScopeVersion != in.ScopeVersion || session.ScopeVersion != in.ScopeVersion {
		return session, ErrWorkspaceProposalStale
	}
	return session, nil
}

func (s *AgentWorkspaceStore) ReconcileWorkflow(ctx context.Context, in WorkspaceWorkflowReconcile) (WorkspaceSnapshot, error) {
	if s == nil || s.db == nil {
		return WorkspaceSnapshot{}, errors.New("workspace database is required")
	}
	var snapshot WorkspaceSnapshot
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.AgentSummarySession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&session).Error; err != nil {
			return err
		}
		if session.WorkflowTerminalMessageID > 0 {
			var err error
			snapshot, err = loadWorkspaceSnapshotTx(tx, in.Key)
			return err
		}
		if session.WorkflowTaskID != in.TaskID || session.WorkflowScopeVersion != in.ScopeVersion {
			return ErrWorkspaceScopeConflict
		}
		messageID := in.MessageID
		if messageID == 0 {
			reply := strings.TrimSpace(in.Reply)
			if reply == "" {
				reply = "总结已生成并自动保存。"
				if session.WorkflowScope == "team" {
					reply = "团队总结已完成并自动保存。"
				}
			}
			payloadValue := agent.SummaryResponsePayload{
				ResultType: in.ResultType,
				Reply:      reply,
				ExecutionTarget: func() string {
					if session.WorkflowScope == "team" {
						return "team_workflow"
					}
					return "personal_workflow"
				}(),
			}
			if !in.ClearWorkflow {
				payloadValue.Workflow = &agent.SummaryResponseWorkflow{TaskID: in.TaskID, Status: "completed", Saved: true}
			}
			payload, _ := json.Marshal(payloadValue)
			payloadString := string(payload)
			message := model.AgentMessage{
				SpaceID:         in.Key.SpaceID,
				SessionID:       in.Key.SessionID,
				UserID:          in.Key.UserID,
				Role:            "assistant",
				Content:         reply,
				ResultType:      in.ResultType,
				ResponsePayload: &payloadString,
				ScopeVersion:    in.ScopeVersion,
				CreatedAt:       time.Now(),
			}
			if err := tx.Create(&message).Error; err != nil {
				return err
			}
			messageID = message.ID
		}
		updates := map[string]interface{}{
			"state":                        in.ResultType,
			"state_version":                gorm.Expr("state_version + 1"),
			"workflow_terminal_message_id": messageID,
			"updated_at":                   time.Now(),
		}
		if in.ClearWorkflow {
			updates["workflow_task_id"] = 0
			updates["workflow_scope"] = ""
			updates["workflow_scope_version"] = 0
			updates["workflow_started_message_id"] = 0
		}
		if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).Updates(updates).Error; err != nil {
			return err
		}
		var err error
		snapshot, err = loadWorkspaceSnapshotTx(tx, in.Key)
		return err
	})
	return snapshot, err
}

func (s *AgentWorkspaceStore) MarkPreviewSaved(ctx context.Context, in WorkspacePreviewSave) (WorkspaceSnapshot, error) {
	if s == nil || s.db == nil {
		return WorkspaceSnapshot{}, errors.New("workspace database is required")
	}
	var snapshot WorkspaceSnapshot
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session model.AgentSummarySession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("space_id = ? AND user_id = ? AND session_id = ?", in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&session).Error; err != nil {
			return err
		}
		if session.ScopeVersion != in.ScopeVersion || session.LatestPreviewMessageID != in.MessageID || session.ArtifactVersion != in.ExpectedArtifactVersion {
			return ErrWorkspacePreviewStale
		}
		var message model.AgentMessage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND space_id = ? AND user_id = ? AND session_id = ?", in.MessageID, in.Key.SpaceID, in.Key.UserID, in.Key.SessionID).
			Take(&message).Error; err != nil {
			return err
		}
		if (message.ResultType != workspaceResultAgentPreview && message.ResultType != workspaceResultAgentRevision) ||
			message.ScopeVersion != in.ScopeVersion || message.ArtifactVersion != in.ExpectedArtifactVersion ||
			message.SnapshotVersion != in.SnapshotVersion {
			return ErrWorkspacePreviewStale
		}
		if message.SavedTaskID > 0 && message.SavedTaskID != in.TaskID {
			return ErrWorkspacePreviewStale
		}
		if err := tx.Model(&model.AgentMessage{}).Where("id = ?", message.ID).Update("saved_task_id", in.TaskID).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.AgentSummarySession{}).Where("id = ?", session.ID).Updates(map[string]interface{}{
			"latest_preview_saved_task_id": in.TaskID,
			"state_version":                gorm.Expr("state_version + 1"),
			"updated_at":                   time.Now(),
		}).Error; err != nil {
			return err
		}
		var err error
		snapshot, err = loadWorkspaceSnapshotTx(tx, in.Key)
		return err
	})
	return snapshot, err
}

func workspaceStoreToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate proposal token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
