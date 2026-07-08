package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/gin-gonic/gin"
)

// agentChatProfile 指定本入口使用的场景（提示词 + 工具名单在 internal/agent/profile.go 配）。
const agentChatProfile = "chat"

// AgentChatHandler 提供最基础的非流式一问一答对话入口，
// 复用 internal/agent 底座（Runner 并发安全，持有一个即可）。
type AgentChatHandler struct {
	llmApiURL    string
	llmApiKey    string
	llmModel     string
	llmTimeout   int
	llmMaxTokens int

	// test-only fields: when set, bypass dynamic runner construction
	testRunner *agent.Runner
	testSystem string
}

// newAgentChatHandlerWithRunner 用一个已构造好的 Runner + 系统提示词造 handler。
// 生产构造 NewAgentChatHandler 内部调它；测试可注入带假 LLM 的 Runner。
func newAgentChatHandlerWithRunner(r *agent.Runner, system string) *AgentChatHandler {
	return &AgentChatHandler{
		testRunner: r,
		testSystem: system,
	}
}

// NewAgentChatHandler 按 profile 组装 agent 底座：提示词从文件加载、工具按场景名单挑。
// 提示词/工具/策略均在 internal/agent/profile.go 与 prompts/*.md 中配置，改词免重编译。
func NewAgentChatHandler(llmApiURL, llmApiKey, llmModel string, llmTimeout, llmMaxTokens int) *AgentChatHandler {
	return &AgentChatHandler{
		llmApiURL:    llmApiURL,
		llmApiKey:    llmApiKey,
		llmModel:     llmModel,
		llmTimeout:   llmTimeout,
		llmMaxTokens: llmMaxTokens,
	}
}

// buildRunnerForProfile constructs a runner for the given profile name.
// If uid is non-empty and profile is "summary", it will be injected into tool handlers.
func (h *AgentChatHandler) buildRunnerForProfile(profileName, uid string) (*agent.Runner, string, error) {
	profile, err := agent.GetProfile(profileName)
	if err != nil {
		return nil, "", fmt.Errorf("load profile %q: %w", profileName, err)
	}
	system, err := agent.LoadPrompt(profile.PromptFile)
	if err != nil {
		return nil, "", fmt.Errorf("load prompt %q: %w", profile.PromptFile, err)
	}

	var reg *agent.Registry
	if profileName == "summary" && uid != "" {
		reg, err = h.buildSummaryRegistryWithUID(uid)
	} else {
		reg, err = agent.BuildRegistry(profile.Tools)
	}
	if err != nil {
		return nil, "", fmt.Errorf("build registry: %w", err)
	}

	client := agent.NewClient(h.llmApiURL, h.llmApiKey, h.llmModel, h.llmTimeout, h.llmMaxTokens)
	pool := agent.NewPool(4)
	runner := agent.NewRunner(client, reg, pool, profile.Policy)
	return runner, system, nil
}

// buildSummaryRegistryWithUID builds a summary registry with uid injected into tool handlers.
func (h *AgentChatHandler) buildSummaryRegistryWithUID(uid string) (*agent.Registry, error) {
	reg := agent.NewRegistry()

	// Non-summary tools (no uid injection needed)
	for _, name := range []string{"get_current_time", "extract_time_range"} {
		factory, ok := agent.GetToolFactory(name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		schema, handler := factory()
		reg.Register(schema, handler)
	}

	// Summary tools: wrap handlers to inject uid via context
	summaryTools := []string{
		"list_channels", "narrow_channels_by_topic", "find_shared_channels",
		"peek_channel", "fetch_channel", "search_messages",
		"filter_relevant", "summarize_chunk", "merge_summaries",
	}
	for _, name := range summaryTools {
		factory, ok := agent.GetToolFactory(name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		schema, origHandler := factory()

		// Wrap handler to inject uid into context
		wrappedHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
			ctx = context.WithValue(ctx, agent.ContextKeyUID, uid)
			return origHandler(ctx, args)
		}
		reg.Register(schema, wrappedHandler)
	}

	return reg, nil
}

// agentChatRequest 是聊天入参。session_id 一期仅透传（前端本地维护聊天记录）。
type agentChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
	Profile   string `json:"profile,omitempty"` // 可选，指定使用的 profile 名称，默认 "chat"
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

	profileName := req.Profile
	if profileName == "" {
		profileName = "chat" // default profile
	}

	var runner *agent.Runner
	var system string
	var err error

	// Extract uid from middleware (authenticated identity)
	uid := middleware.GetUserID(c)

	// Use injected test runner if available
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

	reply, err := runner.Run(c.Request.Context(), system, req.Message)
	if err != nil {
		log.Printf("[agent] chat runner error: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "agent chat failed"})
		return
	}

	c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: gin.H{
		"reply":      reply,
		"session_id": req.SessionID,
		"profile":    profileName,
	}})
}
