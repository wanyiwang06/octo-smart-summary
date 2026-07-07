package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// chatter 是 Runner 对 LLM 的最小依赖抽象，*Client 实现它；测试可注入 fake。
type chatter interface {
	Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error)
}

type Policy struct {
	MaxSteps    int
	MaxTokens   int
	StepTimeout time.Duration
}

type Runner struct {
	client chatter
	reg    *Registry
	pool   *Pool
	policy Policy
}

func NewRunner(client chatter, reg *Registry, pool *Pool, policy Policy) *Runner {
	return &Runner{client: client, reg: reg, pool: pool, policy: policy}
}

// Run 驱动多轮"思考→调工具→回喂"回环，直到模型不再调工具（收敛）或触顶。
func (r *Runner) Run(ctx context.Context, system, userInput string) (string, error) {
	msgs := []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: userInput},
	}
	totalTokens := 0

	for step := 0; step < r.policy.MaxSteps; step++ {
		stepCtx, cancel := context.WithTimeout(ctx, r.policy.StepTimeout)
		turn, err := r.client.Chat(stepCtx, msgs, r.reg.Schemas())
		cancel()
		if err != nil {
			return "", err
		}
		totalTokens += turn.Tokens

		// 无工具调用 = 模型给出最终答案，正常出口。
		if len(turn.ToolCalls) == 0 {
			return turn.Content, nil
		}

		// 回喂 assistant 轮次（必须携带原始 tool_calls，否则下游 tool 消息无处挂靠）。
		msgs = append(msgs, Message{
			Role:      "assistant",
			Content:   turn.Content,
			ToolCalls: turn.ToolCalls,
		})

		// 单跳内多工具并发执行；结果按原索引回填以保证顺序稳定、无数据竞争。
		results := r.runTools(ctx, turn.ToolCalls)
		for i, tc := range turn.ToolCalls {
			msgs = append(msgs, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    results[i],
			})
		}

		// 预算触顶：注入收尾指令，逼模型下一轮直接给答案。
		if totalTokens >= r.policy.MaxTokens {
			msgs = append(msgs, Message{
				Role:    "user",
				Content: "已达token预算，请基于现有信息直接给出最终答案，不要再调用工具。",
			})
		}
	}
	return "", errors.New("max steps exceeded")
}

// runTools 并发分发一跳内的全部 tool_calls，各自独立 ctx，错误转结果字符串（不中断）。
// 结果写入预分配 slice 的固定索引，天然无写冲突；WaitGroup 收齐。
func (r *Runner) runTools(ctx context.Context, calls []ToolCall) []string {
	results := make([]string, len(calls))
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		i, tc := i, tc
		r.pool.Submit(func() {
			defer wg.Done()
			out, err := r.reg.Dispatch(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if err != nil {
				results[i] = "错误: " + err.Error()
				return
			}
			results[i] = out
		})
	}
	wg.Wait()
	return results
}
