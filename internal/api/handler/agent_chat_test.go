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
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
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
	return newAgentChatHandlerWithRunner(runner, "test-system-prompt", newFakeHistoryStore(), 10)
}

// newTestAgentChatHandlerErr 造一个其 Runner 会因 chatter 报错而返回 err 的 handler。
func newTestAgentChatHandlerErr(err error) *AgentChatHandler {
	reg := agent.NewRegistry()
	pool := agent.NewPool(2)
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 5 * time.Second}
	runner := agent.NewRunner(&fakeErrChatter{err: err}, reg, pool, policy)
	return newAgentChatHandlerWithRunner(runner, "test-system-prompt", newFakeHistoryStore(), 10)
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

// fakeHistoryStore 是内存版 agentHistoryStore：无需真 DB，模拟按 session 累积历史。
type fakeHistoryStore struct {
	byID map[string][]agent.Message
}

func newFakeHistoryStore() *fakeHistoryStore {
	return &fakeHistoryStore{byID: map[string][]agent.Message{}}
}

func (s *fakeHistoryStore) LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	return append([]agent.Message(nil), s.byID[sessionID]...), nil
}

func (s *fakeHistoryStore) AppendMessages(ctx context.Context, sessionID string, msgs []agent.Message) error {
	s.byID[sessionID] = append(s.byID[sessionID], msgs...)
	return nil
}

// recordingChatter 记录最近一次 Chat 收到的 msgs，用于断言第二轮能看到第一轮历史。
type recordingChatter struct {
	reply    string
	lastMsgs []agent.Message
}

func (c *recordingChatter) Chat(ctx context.Context, msgs []agent.Message, tools []agent.Tool) (agent.AssistantTurn, error) {
	c.lastMsgs = append([]agent.Message(nil), msgs...)
	return agent.AssistantTurn{Content: c.reply}, nil
}

func doChat(t *testing.T, r *gin.Engine, message, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"` + message + `","session_id":"` + sessionID + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestAgentChatEmptySessionID 校验：session_id 空 -> 400（前端必传）。
func TestAgentChatEmptySessionID(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatRouter(h)

	w := doChat(t, r, "你好", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty session_id, got %d, body=%s", w.Code, w.Body.String())
	}
}

// TestAgentChatInvalidSessionID 校验：session_id 含非法字符 / 超长 -> 400。
// 无校验时（fail-before）非法 session_id 会被放行到 200；加校验后应稳定 400。
func TestAgentChatInvalidSessionID(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatRouter(h)

	cases := []struct {
		name string
		sid  string
	}{
		{"illegal-char-space", "sess 42"},
		{"illegal-char-slash", "a/b"},
		{"illegal-char-cjk", "会话1"},
		{"too-long-129", strings.Repeat("a", 129)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doChat(t, r, "你好", tc.sid)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for session_id=%q, got %d, body=%s", tc.sid, w.Code, w.Body.String())
			}
		})
	}
}

// TestAgentChatMessageTooLong 校验：message 超过 maxMessageLen(8192) 字符 -> 400。
// 无校验时（fail-before）超长 message 会被放行到 200；加校验后应稳定 400。
func TestAgentChatMessageTooLong(t *testing.T) {
	h := newTestAgentChatHandler("不该出现")
	r := setupAgentChatRouter(h)

	long := strings.Repeat("x", maxMessageLen+1)
	w := doChat(t, r, long, "sess-long")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for over-long message (len=%d), got %d, body=%s", len(long), w.Code, w.Body.String())
	}
}

// TestAgentChatMessageAtLimitOK 边界：message 恰好 maxMessageLen 字符应放行(200)，证明校验只拦超长。
func TestAgentChatMessageAtLimitOK(t *testing.T) {
	h := newTestAgentChatHandler("ok-reply")
	r := setupAgentChatRouter(h)

	atLimit := strings.Repeat("x", maxMessageLen)
	w := doChat(t, r, atLimit, "sess-limit")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for message at limit (len=%d), got %d, body=%s", len(atLimit), w.Code, w.Body.String())
	}
}

// TestAgentChatMultiTurnHistory 校验：同一 session_id 两轮，第二轮 LoadHistory 能拿到
// 第一轮（user+assistant）并拼进发给 LLM 的上下文。用内存 store + recordingChatter。
func TestAgentChatMultiTurnHistory(t *testing.T) {
	reg := agent.NewRegistry()
	pool := agent.NewPool(2)
	policy := agent.Policy{MaxSteps: 8, MaxTokens: 8000, StepTimeout: 5 * time.Second}
	chatter := &recordingChatter{reply: "assistant-reply"}
	runner := agent.NewRunner(chatter, reg, pool, policy)
	store := newFakeHistoryStore()
	h := newAgentChatHandlerWithRunner(runner, "sys-prompt", store, 10)
	r := setupAgentChatRouter(h)

	const sess = "sess-multi"

	// 第一轮。
	if w := doChat(t, r, "first-msg", sess); w.Code != http.StatusOK {
		t.Fatalf("turn1 expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	// 第一轮上下文只有 system + user（无历史）。
	if len(chatter.lastMsgs) != 2 {
		t.Fatalf("turn1 ctx len = %d, want 2: %+v", len(chatter.lastMsgs), chatter.lastMsgs)
	}
	// 落库后该 session 应有 user+assistant 两条。
	if got := store.byID[sess]; len(got) != 2 {
		t.Fatalf("after turn1 store has %d msgs, want 2: %+v", len(got), got)
	}

	// 第二轮：应看到第一轮历史被拼进上下文。
	if w := doChat(t, r, "second-msg", sess); w.Code != http.StatusOK {
		t.Fatalf("turn2 expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	m := chatter.lastMsgs
	// 期望: system, user(first-msg), assistant(assistant-reply), user(second-msg)
	if len(m) != 4 {
		t.Fatalf("turn2 ctx len = %d, want 4: %+v", len(m), m)
	}
	if m[0].Role != "system" {
		t.Fatalf("turn2 msg[0] not system: %+v", m[0])
	}
	if m[1].Role != "user" || m[1].Content != "first-msg" {
		t.Fatalf("turn2 missing turn1 user: %+v", m[1])
	}
	if m[2].Role != "assistant" || m[2].Content != "assistant-reply" {
		t.Fatalf("turn2 missing turn1 assistant: %+v", m[2])
	}
	if m[3].Role != "user" || m[3].Content != "second-msg" {
		t.Fatalf("turn2 current user wrong: %+v", m[3])
	}
	// 两轮后 store 应累积 4 条。
	if got := store.byID[sess]; len(got) != 4 {
		t.Fatalf("after turn2 store has %d msgs, want 4: %+v", len(got), got)
	}
}

// TestRowsDescToMessagesAsc 锁定 LoadHistory 的“有限后缀 + 反转升序”契约的映射部分：
// DB 层按 id DESC 取回（最近在前）的行，经 rowsDescToMessagesAsc 后必须还原为 id 升序（对话时序），
// 且 tool_calls JSON 正确反序列化。无需真 DB，直接喂降序行验证升序输出。
func TestRowsDescToMessagesAsc(t *testing.T) {
	tc := `[{"id":"call_1","type":"function","function":{"name":"foo","arguments":"{}"}}]`
	// 模拟 DB Order("id DESC") 取回：最近(id=4)在前，最旧(id=1)在后。
	descRows := []model.AgentMessage{
		{ID: 4, SessionID: "s", Role: "assistant", Content: "reply2"},
		{ID: 3, SessionID: "s", Role: "user", Content: "q2"},
		{ID: 2, SessionID: "s", Role: "assistant", Content: "", ToolCalls: &tc},
		{ID: 1, SessionID: "s", Role: "user", Content: "q1"},
	}

	msgs, err := rowsDescToMessagesAsc(descRows)
	if err != nil {
		t.Fatalf("rowsDescToMessagesAsc err: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("want 4 msgs, got %d: %+v", len(msgs), msgs)
	}

	// 必须为升序（对话时序）：q1(user) -> tool_calls(assistant) -> q2(user) -> reply2(assistant)。
	wantRole := []string{"user", "assistant", "user", "assistant"}
	wantContent := []string{"q1", "", "q2", "reply2"}
	for i := range msgs {
		if msgs[i].Role != wantRole[i] {
			t.Fatalf("msg[%d] role = %q, want %q (ascending order broken): %+v", i, msgs[i].Role, wantRole[i], msgs)
		}
		if msgs[i].Content != wantContent[i] {
			t.Fatalf("msg[%d] content = %q, want %q: %+v", i, msgs[i].Content, wantContent[i], msgs)
		}
	}
	// tool_calls 应被反序列化到第 2 条 assistant（原 id=2）。
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Function.Name != "foo" {
		t.Fatalf("tool_calls not restored on ascending msg[1]: %+v", msgs[1].ToolCalls)
	}
}

// TestRowsDescToMessagesAscEmpty 边界：空输入返回空、不 panic。
func TestRowsDescToMessagesAscEmpty(t *testing.T) {
	msgs, err := rowsDescToMessagesAsc(nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("want 0 msgs for nil input, got %d", len(msgs))
	}
}
