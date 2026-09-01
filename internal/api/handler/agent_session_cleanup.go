package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

// agent_message 清理策略(CHAT-REFERENCE-BASED-DESIGN 后续加固,
// 见主人 2026-07-15 决策 D1..D5):
//
//   D1: 清所有过期 Legacy session (保存成总结的 session 在 CreateAgentSummary
//       tx 里已被销毁,活着的 Legacy session = 用户没保存的临时对话残留)
//       summary workspace 由 agent_summary_session 管理生命周期,不在这里清理。
//   D2: 「过期」= 该 Legacy session 最后一条消息 created_at > 24h 前
//   D3: goroutine + time.Ticker,和 summary-api 进程同生命周期
//   D4: 日志折中 —— 只在清了 > 0 行、或耗时异常、或出错时打
//   D5: 独立分支/PR:feat/agent-session-cleanup-24h → feat/agent-framework-poc
//
// 为什么直接按 last-activity 删,不做 orphan-only 判断:
//   - 保存成总结时 CreateAgentSummary 事务里已 DELETE 该 session 全部行,
//     所以线上还活着的老 session 一定是用户没保存的
//   - 只按 "24h 未动" 判断简单可靠、SQL 快、不用关联 summary_task 表

const (
	// cleanupInterval 24h 触发一次
	cleanupInterval = 24 * time.Hour
	// cleanupAge session 最后活动超过 24h 判为过期
	cleanupAge = 24 * time.Hour
	// cleanupSlowThreshold 单次 DELETE 超过此阈值 → 打慢查询警报(D4 C)
	cleanupSlowThreshold = 1 * time.Second
	// cleanupJitter 首次执行前等一段随机时间,避免多实例撞车 & 冷启动瞬间打 DB
	cleanupInitialDelay = 30 * time.Second
	// workspaceCleanupBatchSize bounds each cleanup transaction so a large
	// backlog does not hold locks across the entire workspace history table.
	workspaceCleanupBatchSize = 200
)

// StartAgentSessionCleanup 启动 24h 定时清理 goroutine。
// ctx 取消时干净退出,不阻塞进程关停(和 http.Server graceful shutdown 一致)。
// 只应在 main() 里调用一次;重复调用会开多个 ticker 但不会崩,只是浪费。
func StartAgentSessionCleanup(ctx context.Context, db *gorm.DB) {
	go func() {
		// 冷启动等一小段,避免和 migration/其他初始化并发压 DB
		select {
		case <-time.After(cleanupInitialDelay):
		case <-ctx.Done():
			return
		}
		// 首次立即跑一次(启动几十秒后),之后每 24h 一次
		runOnce(db)

		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("[agent-cleanup] shutting down")
				return
			case <-ticker.C:
				runOnce(db)
			}
		}
	}()
}

// runOnce 执行一次清理。分成小函数便于单测直接调用不依赖 ticker。
//
// 权限模型对齐(SUM-158 blocker 6):agent_message 的所有权键是
// (user_id, session_id)——按 blocker 1 的设计,两个不同用户允许偶然共用同一
// session_id 字面值。因此清理粒度也必须是 (user_id, session_id) 而不是裸
// session_id,否则:
//  1. 一个 (user, session) tuple 的活跃会保护另一个 (other_user, same_session)
//     的老 tuple 过期不掉——过度保留。
//  2. 当两个 tuple 都空闲后,`WHERE session_id IN (...)` 会一次删掉整段,
//     连带把最后活动比另一 tuple 更晚的行也误删——跨用户误删。
//
// 用 (user_id, session_id) 复合筛选,精确到属主。
//
// #161 P2 (yujiawei): agent_message_evidence must be cleaned symmetrically.
// After PR #161 evidence is the sole citation-handle discovery source for
// both getSessionMessagePool (mid-run) and buildCitationsForSession
// (save-time). Without cleanup, evidence rows accumulate indefinitely for
// any reused session_id and inflate the citation pool of every subsequent
// summarize_chunk. Evidence expiry uses its own last-activity timestamp, while
// durable workspace references below prevent premature collection.
func runOnce(db *gorm.DB) {
	now := timezone.Now()
	cutoff := now.Add(-cleanupAge)
	start := timezone.Now()

	// 只在 Legacy 消息(space_id = '')中按 (user_id, session_id) 聚合
	// MAX(created_at),定位两键都过期的 tuple。workspace 消息由持久化 session
	// 引用,不能被 Legacy 24h 清理删除；同 tuple 下的新 workspace 消息也不能
	// 反过来阻止过期 Legacy 消息清理。
	// 组合 IN 子查询在 MySQL 和 SQLite 3.7+ 都支持 (`WHERE (a, b) IN (SELECT a, b ...)`).
	//
	// 单条 SQL 不用事务(DELETE 天然是原子写),避免长事务锁。
	result := db.Exec(`
		DELETE FROM agent_message
		WHERE space_id = ''
		  AND (user_id, session_id) IN (
			SELECT user_id, session_id FROM (
				SELECT user_id, session_id, MAX(created_at) AS last_at
				FROM agent_message
				WHERE space_id = ''
				GROUP BY user_id, session_id
				HAVING last_at <= ?
			) AS expired
		)
	`, cutoff)

	elapsed := time.Since(start)

	// D4 C: 折中日志策略
	//   1. 出错必打
	//   2. 清了 > 0 行才打(N=0 静默,不制造噪音)
	//   3. 慢查询(超过阈值)必打警报
	if result.Error != nil {
		log.Printf("[agent-cleanup] ERROR delete failed after %s: %v", elapsed, result.Error)
		return
	}
	if result.RowsAffected > 0 {
		log.Printf("[agent-cleanup] cleaned %d rows in %s (cutoff=%s)",
			result.RowsAffected, elapsed, cutoff.Format(time.RFC3339))
	}
	if elapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW delete took %s (rows=%d, cutoff=%s) — consider indexing agent_message(session_id, created_at)",
			elapsed, result.RowsAffected, cutoff.Format(time.RFC3339))
	}

	workspaceStart := timezone.Now()
	workspaceCount, workspaceErr := cleanupExpiredSummaryWorkspaces(db, now)
	workspaceElapsed := time.Since(workspaceStart)
	if workspaceErr != nil {
		log.Printf("[agent-cleanup] ERROR workspace cleanup failed after %s: %v", workspaceElapsed, workspaceErr)
	} else if workspaceCount > 0 {
		log.Printf("[agent-cleanup] cleaned %d expired workspace sessions in %s", workspaceCount, workspaceElapsed)
	}
	if workspaceElapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW workspace cleanup took %s (sessions=%d)", workspaceElapsed, workspaceCount)
	}

	// #161 P2 (yujiawei): symmetric evidence cleanup. Delete evidence rows
	// for (user_id, session_id) tuples whose evidence itself is older than
	// cleanupAge. Keying off evidence.created_at (not agent_message) is
	// simpler and self-contained — evidence is written synchronously by
	// PersistEvidence at fetch/peek/search/filter time, so its timestamps
	// reflect real user activity independent of AppendMessages ordering.
	// Evidence has no space_id, so workspace Agent runs use a space-derived
	// agent_session_id. Preserve every (user_id, agent_session_id) tuple referenced
	// by agent_summary_session; workspace retirement must remove that session
	// before this Legacy cleanup may collect its evidence.
	evStart := timezone.Now()
	evResult := db.Exec(`
		DELETE FROM agent_message_evidence
		WHERE (user_id, session_id) IN (
			SELECT user_id, session_id FROM (
				SELECT user_id, session_id, MAX(created_at) AS last_at
				FROM agent_message_evidence
				GROUP BY user_id, session_id
				HAVING last_at <= ?
			) AS expired
		)
		  AND NOT EXISTS (
			SELECT 1
			FROM agent_summary_session
			WHERE agent_summary_session.user_id = agent_message_evidence.user_id
			  AND agent_summary_session.agent_session_id = agent_message_evidence.session_id
		)
	`, cutoff)
	evElapsed := time.Since(evStart)
	if evResult.Error != nil {
		log.Printf("[agent-cleanup] ERROR evidence delete failed after %s: %v", evElapsed, evResult.Error)
		return
	}
	if evResult.RowsAffected > 0 {
		log.Printf("[agent-cleanup] cleaned %d evidence rows in %s (cutoff=%s)",
			evResult.RowsAffected, evElapsed, cutoff.Format(time.RFC3339))
	}
	if evElapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW evidence delete took %s (rows=%d, cutoff=%s) — consider indexing agent_message_evidence(session_id, created_at)",
			evElapsed, evResult.RowsAffected, cutoff.Format(time.RFC3339))
	}
}

// cleanupExpiredSummaryWorkspaces retires inactive workspace state after the
// 30-day sliding retention window maintained by AgentWorkspaceStore. Rows
// created before expires_at was populated fall back to updated_at so rollout
// does not leave permanent NULL tombstones.
func cleanupExpiredSummaryWorkspaces(db *gorm.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("workspace cleanup database is required")
	}
	legacyCutoff := now.Add(-summaryWorkspaceRetention)
	var cleaned int64
	for {
		var sessions []model.AgentSummarySession
		if err := db.Where("expires_at <= ? OR (expires_at IS NULL AND updated_at <= ?)", now, legacyCutoff).
			Order("id ASC").Limit(workspaceCleanupBatchSize).Find(&sessions).Error; err != nil {
			return cleaned, fmt.Errorf("load expired workspace sessions: %w", err)
		}
		if len(sessions) == 0 {
			break
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			for _, session := range sessions {
				if err := deleteWorkspaceSessionState(tx, session); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return cleaned, err
		}
		cleaned += int64(len(sessions))
		if len(sessions) < workspaceCleanupBatchSize {
			break
		}
	}

	// Idempotency bindings only need to outlive their live task. Retire old
	// orphan/tombstone rows after the same window while preserving bindings for
	// summaries that still exist.
	if err := db.Exec(`
		DELETE FROM summary_workflow_idempotency
		WHERE created_at <= ?
		  AND NOT EXISTS (
			SELECT 1 FROM summary_task
			WHERE summary_task.id = summary_workflow_idempotency.task_id
			  AND summary_task.deleted_at IS NULL
		  )
	`, legacyCutoff).Error; err != nil {
		return cleaned, fmt.Errorf("clean workflow idempotency tombstones: %w", err)
	}
	return cleaned, nil
}

func deleteWorkspaceSessionState(tx *gorm.DB, session model.AgentSummarySession) error {
	var runIDs []string
	if err := tx.Model(&model.AgentMessage{}).
		Where("space_id = ? AND user_id = ? AND session_id = ? AND run_id <> ''", session.SpaceID, session.UserID, session.SessionID).
		Distinct().Pluck("run_id", &runIDs).Error; err != nil {
		return fmt.Errorf("load workspace run ids for session %d: %w", session.ID, err)
	}

	evidenceSessions := make([]string, 0, len(runIDs)+1)
	if strings.TrimSpace(session.AgentSessionID) != "" {
		evidenceSessions = append(evidenceSessions, session.AgentSessionID)
	}
	if len(runIDs) > 0 {
		var runs []model.AgentSummaryRun
		if err := tx.Select("run_id", "session_id").Where("user_id = ? AND run_id IN ?", session.UserID, runIDs).Find(&runs).Error; err != nil {
			return fmt.Errorf("load workspace runs for session %d: %w", session.ID, err)
		}
		for _, run := range runs {
			if strings.TrimSpace(run.SessionID) != "" {
				evidenceSessions = append(evidenceSessions, run.SessionID)
			}
		}
		if err := tx.Where("run_id IN ?", runIDs).Delete(&model.AgentCitationManifest{}).Error; err != nil {
			return fmt.Errorf("delete workspace citation manifests for session %d: %w", session.ID, err)
		}
		if err := tx.Where("run_id IN ?", runIDs).Delete(&model.AgentEvidenceArtifact{}).Error; err != nil {
			return fmt.Errorf("delete workspace evidence artifacts for session %d: %w", session.ID, err)
		}
		if err := tx.Where("run_id IN ?", runIDs).Delete(&model.AgentSummarySpec{}).Error; err != nil {
			return fmt.Errorf("delete workspace specs for session %d: %w", session.ID, err)
		}
		if err := tx.Where("user_id = ? AND run_id IN ?", session.UserID, runIDs).Delete(&model.AgentSummaryRun{}).Error; err != nil {
			return fmt.Errorf("delete workspace runs for session %d: %w", session.ID, err)
		}
	}
	if len(evidenceSessions) > 0 {
		if err := tx.Where("user_id = ? AND session_id IN ?", session.UserID, evidenceSessions).Delete(&model.AgentMessageEvidence{}).Error; err != nil {
			return fmt.Errorf("delete workspace evidence for session %d: %w", session.ID, err)
		}
	}
	if err := tx.Where("space_id = ? AND user_id = ? AND session_id = ?", session.SpaceID, session.UserID, session.SessionID).
		Delete(&model.AgentMessage{}).Error; err != nil {
		return fmt.Errorf("delete workspace messages for session %d: %w", session.ID, err)
	}
	if err := tx.Where("space_id = ? AND user_id = ? AND session_id = ?", session.SpaceID, session.UserID, session.SessionID).
		Delete(&model.AgentSummaryTurn{}).Error; err != nil {
		return fmt.Errorf("delete workspace turns for session %d: %w", session.ID, err)
	}
	if err := tx.Where("id = ?", session.ID).Delete(&model.AgentSummarySession{}).Error; err != nil {
		return fmt.Errorf("delete workspace session %d: %w", session.ID, err)
	}
	return nil
}

// 兜底类型检查:确保 AgentMessage 表名不变时这段代码还生效
var _ = model.AgentMessage{}
var _ = model.AgentMessageEvidence{}
