package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/gin-gonic/gin"
)

// fakeChatter 实现 agent 的 chatter 接口：直接返回一个无 tool_calls 的
// AssistantTurn，让 Runner 回环立即收敛，不真调 LLM。
type fakeChatter struct {
	reply string
}

func (f *fakeChatter) Chat(ctx context.Context, msgs []agent.Message, tools []agent.Tool) (agent.AssistantTurn, error) {
	return agent.AssistantTurn{Content: f.reply}, nil
}

// fakeErrChatter 实现 chatter 接口，总是返回一个带敏感特征串的 error，
// 用于验证 Chat handler 错误分支不会将原始 err 写回响应体。
type fakeErrChatter struct {
	err error
}

func (f *fakeErrChatter) Chat(ctx context.Context, msgs []agent.Message, tools []agent.Tool) (agent.AssistantTurn, error) {
	return agent.AssistantTurn{}, f.err
}

func newTestAgentChatHandler(reply string) *AgentChatHandler {
	reg := agent.NewRegistry()
	pool := agent.NewPool(2)
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 5 * time.Second}
	runner := agent.NewRunner(&fakeChatter{reply: reply}, reg, pool, policy)
	return newAgentChatHandlerWithRunner(runner, "test-system-prompt")
}

// newTestAgentChatHandlerErr 造一个其 Runner 会因 chatter 报错而返回 err 的 handler。
func newTestAgentChatHandlerErr(err error) *AgentChatHandler {
	reg := agent.NewRegistry()
	pool := agent.NewPool(2)
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 5 * time.Second}
	runner := agent.NewRunner(&fakeErrChatter{err: err}, reg, pool, policy)
	return newAgentChatHandlerWithRunner(runner, "test-system-prompt")
}

func setupAgentChatRouter(h *AgentChatHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/agent/chat", h.Chat)
	return r
}

func TestAgentChatEmptyMessage(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatRouter(h)

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"","session_id":"s1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestAgentChatOK(t *testing.T) {
	const want = "你好，我是助手"
	h := newTestAgentChatHandler(want)
	r := setupAgentChatRouter(h)

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"你好","session_id":"sess-42"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Reply     string `json:"reply"`
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v, body=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("expected code 0, got %d", resp.Code)
	}
	if resp.Data.Reply != want {
		t.Fatalf("expected reply %q, got %q", want, resp.Data.Reply)
	}
	if resp.Data.SessionID != "sess-42" {
		t.Fatalf("expected session_id passthrough sess-42, got %q", resp.Data.SessionID)
	}
}

// TestAgentChatRunnerErrorNotLeaked 验证：当 Runner.Run 报错时，handler 返回 500，
// 且响应体不会包含原始错误字符串（避免向公开无鉴权路由泄漏内部细节）。
func TestAgentChatRunnerErrorNotLeaked(t *testing.T) {
	const secret = "secret-upstream-detail"
	h := newTestAgentChatHandlerErr(errors.New("dial tcp 10.0.0.5:443: " + secret))
	r := setupAgentChatRouter(h)

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"你好","session_id":"s-err"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body=%s", w.Code, w.Body.String())
	}

	// 关键断言：响应体不得包含原始错误的敏感特征串。
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("response body leaked raw error detail %q: body=%s", secret, w.Body.String())
	}
}
