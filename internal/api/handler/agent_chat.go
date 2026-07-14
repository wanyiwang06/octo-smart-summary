package handler

import (
	"log"
	"net/http"
	"regexp"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// agentChatProfile 是未显式指定 profile 时的默认场景（提示词 + 工具名单在 internal/agent/profile.go 配）。
const agentChatProfile = "chat"

// maxMessageLen 是单条用户 message 的最大字符数（rune），超长直接 400，避免超长入参打爆上游。
const maxMessageLen = 8192

// sessionIDPattern 约束前端生成的 session_id：仅字母数字下划线连字符、1..128 长。
// 既防注入/异常键，也与 DB varchar(128) 对齐。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AgentChatHandler 提供非流式一问一答对话入口，复用 internal/agent 底座。
//
// 融合两条能力：
//   - 动态 profile + uid 注入：每请求按 req.Profile 构建 runner；summary 场景把
//     鉴权得到的 uid 注入工具 handler，做频道/消息级权限隔离（工具能力线）。
//   - 多轮记忆：按 session_id 从 store 读历史、滑窗截断后喂给 RunWithHistory，
//     成功回复后落库（多轮对话线）。
//
// LLM 配置（url/key/model/...）在构造期注入并留存，供每请求动态建 runner；
// 敏感值（key）全程从环境变量经 config 传入，不出现在代码中。
type AgentChatHandler struct {
	llmApiURL    string
	llmApiKey    string
	llmModel     string
	llmTimeout   int
	llmMaxTokens int

	store  agentHistoryStore // 多轮记忆读写（生产为 gorm 实现，测试可注入 mock）
	window int               // 滑窗保留的最近轮数

	// test-only fields: when set, bypass dynamic runner construction
	testRunner *agent.Runner
	testSystem string
}

// newAgentChatHandlerWithRunner 用一个已构造好的 Runner + 系统提示词 + 记忆存储造 handler。
// 供测试注入带假 LLM 的 Runner + mock store（走 testRunner 分支，跳过动态构建）。
func newAgentChatHandlerWithRunner(r *agent.Runner, system string, store agentHistoryStore, window int) *AgentChatHandler {
	return &AgentChatHandler{
		testRunner: r,
		testSystem: system,
		store:      store,
		window:     window,
	}
}

// NewAgentChatHandler 生产构造：留存 LLM 配置供每请求动态建 runner，
// 并接入多轮记忆存储（db）与滑窗。提示词/工具/策略在 profile.go 与 prompts/*.md 配置。
func NewAgentChatHandler(db *gorm.DB, llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentChatHandler {
	return &AgentChatHandler{
		llmApiURL:    llmApiURL,
		llmApiKey:    llmApiKey,
		llmModel:     llmModel,
		llmTimeout:   llmTimeout,
		llmMaxTokens: llmMaxTokens,
		store:        newAgentMessageRepo(db),
		window:       agent.HistoryWindow(),
	}
}

// buildRunnerForProfile constructs a runner for the given profile name.
// If uid is non-empty and profile is "summary", it will be injected into tool handlers.
func (h *AgentChatHandler) buildRunnerForProfile(profileName, uid string) (*agent.Runner, string, error) {
	return buildRunner(profileName, uid, h.llmApiURL, h.llmApiKey, h.llmModel, h.llmTimeout, h.llmMaxTokens)
}

// agentChatRequest 是聊天入参。session_id 由前端生成并必传，后端据此串联多轮历史。
// profile 可选，指定使用的场景名（默认 "chat"）；总结场景传 "summary" 以挂载真实工具。
type agentChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
	Profile   string `json:"profile,omitempty"`
}

// Chat 处理 POST /api/v1/agent/chat：非流式一问一答，携带多轮历史。
//
// 流程：校验 → 取鉴权 uid → 按 profile 动态建 runner（summary 注入 uid 工具）
//   → 读 session 历史并滑窗截断 → RunWithHistory 多轮驱动 → 成功后落库。
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

	profileName := req.Profile
	if profileName == "" {
		profileName = agentChatProfile // default profile
	}

	ctx := c.Request.Context()

	// Extract uid from middleware (authenticated identity).
	// 鉴权中间件已保证到此处 uid 非空；summary profile 的工具据此做权限隔离。
	uid := middleware.GetUserID(c)

	// 按 profile 组装 runner（summary 场景注入 uid 工具）；测试可注入现成 runner。
	var runner *agent.Runner
	var system string
	var err error
	if h.testRunner != nil {
		runner = h.testRunner
		system = h.testSystem
	} else {
		runner, system, err = h.buildRunnerForProfile(profileName, uid)
		if err != nil {
			log.Printf("[agent] build runner for profile %q: %v", profileName, err)
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "failed to initialize agent"})
			return
		}
	}

	// 读多轮历史并滑窗截断。
	history, err := h.store.LoadHistory(ctx, req.SessionID)
	if err != nil {
		log.Printf("[agent] load history error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed"})
		return
	}
	history = agent.TruncateHistory(history, h.window)

	reply, newMsgs, err := runner.RunWithHistory(ctx, system, history, req.Message)
	if err != nil {
		// 真实错误只记服务端日志，避免向调用方泄漏上游 LLM 地址/网络/内部细节。
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
		"profile":    profileName,
	}})
}
