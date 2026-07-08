package handler

import (
	"log"
	"net/http"
	"regexp"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// agentChatProfile 指定本入口使用的场景（提示词 + 工具名单在 internal/agent/profile.go 配）。
const agentChatProfile = "chat"

// maxMessageLen 是单条用户 message 的最大字符数（rune），超长直接 400，避免超长入参打爆上游。
const maxMessageLen = 8192

// sessionIDPattern 约束前端生成的 session_id：仅字母数字下划线连字符、1..128 长。
// 既防注入/异常键，也与 DB varchar(128) 对齐。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AgentChatHandler 提供最基础的非流式一问一答对话入口，
// 复用 internal/agent 底座（Runner 并发安全，持有一个即可）。
type AgentChatHandler struct {
	runner *agent.Runner
	system string            // 本场景的系统提示词，构造期从 profile 加载
	store  agentHistoryStore // 多轮记忆读写（生产为 gorm 实现，测试可注入 mock）
	window int               // 滑窗保留的最近轮数
}

// newAgentChatHandlerWithRunner 用一个已构造好的 Runner + 系统提示词 + 记忆存储造 handler。
// 生产构造 NewAgentChatHandler 内部调它；测试可注入带假 LLM 的 Runner + mock store。
func newAgentChatHandlerWithRunner(r *agent.Runner, system string, store agentHistoryStore, window int) *AgentChatHandler {
	return &AgentChatHandler{runner: r, system: system, store: store, window: window}
}

// NewAgentChatHandler 按 profile 组装 agent 底座：提示词从文件加载、工具按场景名单挑。
// 提示词/工具/策略均在 internal/agent/profile.go 与 prompts/*.md 中配置，改词免重编译。
func NewAgentChatHandler(db *gorm.DB, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentChatHandler {
	profile, err := agent.GetProfile(agentChatProfile)
	if err != nil {
		log.Fatalf("[agent] load profile %q: %v", agentChatProfile, err)
	}
	system, err := agent.LoadPrompt(profile.PromptFile)
	if err != nil {
		log.Fatalf("[agent] load prompt %q: %v", profile.PromptFile, err)
	}
	reg, err := agent.BuildRegistry(profile.Tools)
	if err != nil {
		log.Fatalf("[agent] build registry: %v", err)
	}

	client := agent.NewClient(llmApiURL, llmApiKey, llmModel, llmTimeout, llmMaxTokens)
	pool := agent.NewPool(4)
	runner := agent.NewRunner(client, reg, pool, profile.Policy)
	return newAgentChatHandlerWithRunner(runner, system, newAgentMessageRepo(db), agent.HistoryWindow())
}

// agentChatRequest 是聊天入参。session_id 由前端生成并必传，后端据此串联多轮历史。
type agentChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

// Chat 处理 POST /api/v1/agent/chat：非流式一问一答，携带多轮历史。
//
// 并发约束：单 session 依赖前端单飞（同一 session_id 勿并发发送）。LoadHistory→LLM→
// AppendMessages 全程无锁，若同 session 并发进入会读到相同历史各自续写，产生分叉历史；
// 锁 / 版本号方案留后续，本轮不实现。
func (h *AgentChatHandler) Chat(c *gin.Context) {
	var req agentChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "message 不能为空"})
		return
	}
	if len([]rune(req.Message)) > maxMessageLen {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "message 过长"})
		return
	}
	// session_id 前端必传；缺失则无法串联历史，直接 400。
	if req.SessionID == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 不能为空"})
		return
	}
	if !sessionIDPattern.MatchString(req.SessionID) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "session_id 非法"})
		return
	}

	ctx := c.Request.Context()
	history, err := h.store.LoadHistory(ctx, req.SessionID)
	if err != nil {
		log.Printf("[agent] load history error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed"})
		return
	}
	history = agent.TruncateHistory(history, h.window)

	reply, newMsgs, err := h.runner.RunWithHistory(ctx, h.system, history, req.Message)
	if err != nil {
		// 真实错误只记服务端日志，避免向公开无鉴权路由泄漏上游 LLM 地址/网络/内部细节。
		log.Printf("[agent] chat runner error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed"})
		return
	}

	// 成功回复后才落库；落库失败不阻断本次回复（宁可丢本回合历史，也不只落 user 造脏历史）。
	if err := h.store.AppendMessages(ctx, req.SessionID, newMsgs); err != nil {
		log.Printf("[agent] append messages error: %v", err)
	}

	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"reply":      reply,
		"session_id": req.SessionID,
	}})
}
