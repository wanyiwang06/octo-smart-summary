package handler

import (
	"log"
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/gin-gonic/gin"
)

// agentChatProfile 指定本入口使用的场景（提示词 + 工具名单在 internal/agent/profile.go 配）。
const agentChatProfile = "chat"

// AgentChatHandler 提供最基础的非流式一问一答对话入口，
// 复用 internal/agent 底座（Runner 并发安全，持有一个即可）。
type AgentChatHandler struct {
	runner *agent.Runner
	system string // 本场景的系统提示词，构造期从 profile 加载
}

// newAgentChatHandlerWithRunner 用一个已构造好的 Runner + 系统提示词造 handler。
// 生产构造 NewAgentChatHandler 内部调它；测试可注入带假 LLM 的 Runner。
func newAgentChatHandlerWithRunner(r *agent.Runner, system string) *AgentChatHandler {
	return &AgentChatHandler{runner: r, system: system}
}

// NewAgentChatHandler 按 profile 组装 agent 底座：提示词从文件加载、工具按场景名单挑。
// 提示词/工具/策略均在 internal/agent/profile.go 与 prompts/*.md 中配置，改词免重编译。
func NewAgentChatHandler(llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentChatHandler {
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
	return newAgentChatHandlerWithRunner(runner, system)
}

// agentChatRequest 是聊天入参。session_id 一期仅透传（前端本地维护聊天记录）。
type agentChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

// Chat 处理 POST /api/v1/agent/chat：非流式一问一答。
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

	reply, err := h.runner.Run(c.Request.Context(), h.system, req.Message)
	if err != nil {
		// 真实错误只记服务端日志，避免向公开无鉴权路由泄漏上游 LLM 地址/网络/内部细节。
		log.Printf("[agent] chat runner error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed"})
		return
	}

	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"reply":      reply,
		"session_id": req.SessionID,
	}})
}
