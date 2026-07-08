package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeClient 是可编程的 chatter 替身：每次 Chat 弹出一个预设 turn，
// 并记录最后一次收到的 msgs 供断言回喂内容。
type fakeClient struct {
	turns    []AssistantTurn
	idx      int
	lastMsgs []Message
	calls    int
}

func (f *fakeClient) Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error) {
	f.calls++
	f.lastMsgs = append([]Message(nil), msgs...)
	if f.idx >= len(f.turns) {
		// 用尽后一律收敛，避免测试无限循环。
		return AssistantTurn{Content: "done"}, nil
	}
	tr := f.turns[f.idx]
	f.idx++
	return tr, nil
}

func mkToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func newTestRunner(fc *fakeClient, reg *Registry, policy Policy) *Runner {
	return NewRunner(fc, reg, NewPool(4), policy)
}

func regWithEcho(names ...string) *Registry {
	reg := NewRegistry()
	for _, n := range names {
		n := n
		reg.Register(Tool{Type: "function", Function: ToolFunction{Name: n}},
			func(ctx context.Context, args json.RawMessage) (string, error) {
				return "R:" + n, nil
			})
	}
	return reg
}

func TestRunner_Run(t *testing.T) {
	policy := Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second}

	tests := []struct {
		name      string
		turns     []AssistantTurn
		reg       *Registry
		policy    Policy
		wantOut   string
		wantErr   bool
		errSubstr string
		checkMsgs func(t *testing.T, fc *fakeClient)
	}{
		{
			name:    "converge immediately",
			turns:   []AssistantTurn{{Content: "hello", Tokens: 10}},
			reg:     regWithEcho(),
			policy:  policy,
			wantOut: "hello",
		},
		{
			name: "two parallel tools then converge",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{
					mkToolCall("c1", "alpha", `{}`),
					mkToolCall("c2", "beta", `{}`),
				}, Tokens: 20},
				{Content: "final", Tokens: 5},
			},
			reg:     regWithEcho("alpha", "beta"),
			policy:  policy,
			wantOut: "final",
			checkMsgs: func(t *testing.T, fc *fakeClient) {
				// 第二轮请求的 msgs 应含: system,user,assistant(tool_calls),tool(c1),tool(c2)
				m := fc.lastMsgs
				if len(m) != 5 {
					t.Fatalf("msgs len = %d, want 5: %+v", len(m), m)
				}
				if m[2].Role != "assistant" || len(m[2].ToolCalls) != 2 {
					t.Fatalf("assistant msg wrong: %+v", m[2])
				}
				// 顺序稳定：c1 对应 alpha、c2 对应 beta。
				if m[3].ToolCallID != "c1" || m[3].Name != "alpha" || m[3].Content != "R:alpha" {
					t.Fatalf("tool msg 0 wrong: %+v", m[3])
				}
				if m[4].ToolCallID != "c2" || m[4].Name != "beta" || m[4].Content != "R:beta" {
					t.Fatalf("tool msg 1 wrong: %+v", m[4])
				}
			},
		},
		{
			name: "max steps exceeded",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 1},
				{ToolCalls: []ToolCall{mkToolCall("c2", "alpha", `{}`)}, Tokens: 1},
			},
			reg:       regWithEcho("alpha"),
			policy:    Policy{MaxSteps: 2, MaxTokens: 100000, StepTimeout: time.Second},
			wantErr:   true,
			errSubstr: "max steps exceeded",
		},
		{
			name: "token budget injects wrap-up",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{mkToolCall("c1", "alpha", `{}`)}, Tokens: 999},
				{Content: "wrapped", Tokens: 1},
			},
			reg:     regWithEcho("alpha"),
			policy:  Policy{MaxSteps: 5, MaxTokens: 500, StepTimeout: time.Second},
			wantOut: "wrapped",
			checkMsgs: func(t *testing.T, fc *fakeClient) {
				m := fc.lastMsgs
				last := m[len(m)-1]
				if last.Role != "user" || !strings.Contains(last.Content, "token预算") {
					t.Fatalf("expected wrap-up user msg, got %+v", last)
				}
			},
		},
		{
			name: "unknown tool and handler error fed back",
			turns: []AssistantTurn{
				{ToolCalls: []ToolCall{
					mkToolCall("c1", "ghost", `{}`),
					mkToolCall("c2", "explode", `{}`),
				}, Tokens: 1},
				{Content: "recovered", Tokens: 1},
			},
			reg: func() *Registry {
				reg := regWithEcho()
				reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "explode"}},
					func(ctx context.Context, args json.RawMessage) (string, error) {
						return "", context.Canceled
					})
				return reg
			}(),
			policy:  policy,
			wantOut: "recovered",
			checkMsgs: func(t *testing.T, fc *fakeClient) {
				m := fc.lastMsgs
				if len(m) != 5 {
					t.Fatalf("msgs len = %d, want 5", len(m))
				}
				if !strings.Contains(m[3].Content, "错误") || !strings.Contains(m[3].Content, "unknown tool") {
					t.Fatalf("unknown-tool result not fed back: %+v", m[3])
				}
				if !strings.Contains(m[4].Content, "错误") {
					t.Fatalf("handler error not fed back: %+v", m[4])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{turns: tt.turns}
			r := newTestRunner(fc, tt.reg, tt.policy)
			out, err := r.Run(context.Background(), "sys", "usr")

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%q)", out)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error %q missing substr %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != tt.wantOut {
				t.Fatalf("out = %q, want %q", out, tt.wantOut)
			}
			if tt.checkMsgs != nil {
				tt.checkMsgs(t, fc)
			}
		})
	}
}

// TestRunner_ParallelToolsRace 专门给 -race 用：一跳内多工具高并发无竞争、顺序稳定。
func TestRunner_ParallelToolsRace(t *testing.T) {
	const n = 12
	calls := make([]ToolCall, n)
	names := make([]string, n)
	for i := 0; i < n; i++ {
		name := "t" + string(rune('a'+i))
		names[i] = name
		calls[i] = mkToolCall("id"+name, name, `{}`)
	}
	reg := regWithEcho(names...)
	fc := &fakeClient{turns: []AssistantTurn{
		{ToolCalls: calls, Tokens: 1},
		{Content: "ok", Tokens: 1},
	}}
	r := newTestRunner(fc, reg, Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second})

	out, err := r.Run(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want ok", out)
	}
	// 校验回喂顺序与原 tool_calls 一致。
	m := fc.lastMsgs
	toolMsgs := m[3:]
	if len(toolMsgs) != n {
		t.Fatalf("tool msgs = %d, want %d", len(toolMsgs), n)
	}
	for i := 0; i < n; i++ {
		if toolMsgs[i].Content != "R:"+names[i] {
			t.Fatalf("order broken at %d: got %q want %q", i, toolMsgs[i].Content, "R:"+names[i])
		}
	}
}
