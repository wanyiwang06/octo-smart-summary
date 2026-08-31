package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// ListChannelsTool lists all channels visible to a user.
func ListChannelsTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "list_channels",
			Description: "列出指定用户可见的所有频道（群组 + 私聊 + 子区）。用于探索阶段了解可用频道范围。默认不含已归档子区。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"include_archived": map[string]interface{}{
						"type":        "boolean",
						"description": "是否包含已归档的子区（thread）。默认 false（只列活跃频道）。仅当用户明确要「已归档/历史/已关闭的子区」时才置 true。返回结果中归档子区带 is_archived=true 标记。",
					},
					"commit_scope": map[string]interface{}{
						"type":        "boolean",
						"description": "是否把本次返回的全部可见频道确认为当前总结范围。仅当用户明确要求总结所有可见会话时置 true；主题筛选或探索阶段保持 false。",
					},
				},
				"required": []string{},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			IncludeArchived bool `json:"include_archived,omitempty"`
			CommitScope     bool `json:"commit_scope,omitempty"`
		}
		// An omitted argument payload is equivalent to {}; malformed JSON is
		// surfaced so the model can self-correct instead of silently changing
		// the request to include_archived=false.
		if len(bytes.TrimSpace(args)) > 0 {
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
		}

		// Extract uid from context (injected by handler middleware)
		uidVal := ctx.Value(ContextKeyUID)
		uid, ok := uidVal.(string)
		if !ok || uid == "" {
			return "", fmt.Errorf("missing user identity in context")
		}

		// Listing is exploratory by default and does not expand the read scope.
		// commit_scope is the explicit all-visible-channels decision: only an open
		// request scope accepts it, while a closed UI scope remains unexpandable.
		summaryDB, imDB, _, _ := GetSummaryDeps()

		options := []pipeline.ChannelQueryOption{pipeline.WithIncludeArchived(req.IncludeArchived)}
		if !req.IncludeArchived {
			options = append(options, pipeline.WithSelectedThreads(SelectedArchivedChannelIDs(ctx)))
		}
		channels, err := pipeline.GetUserChannels(ctx, uid, imDB, options...)
		if err != nil {
			return "", fmt.Errorf("get user channels: %w", err)
		}
		channels, err = FilterChannelsForWorkspace(ctx, uid, imDB, channels)
		if err != nil {
			return "", fmt.Errorf("filter channels for workspace: %w", err)
		}
		channels = RestrictDiscoveredChannels(ctx, channels)
		scopeCommitted := false
		if req.CommitScope && AuthorizeDiscoveredChannels(ctx, channels) {
			recordDiscoveredChannels(ctx, summaryDB, uid, channelIDsOf(channels))
			scopeCommitted = true
		}

		result := map[string]interface{}{
			"total":           len(channels),
			"channels":        channels,
			"scope_committed": scopeCommitted,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
