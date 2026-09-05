package handler

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestSummaryWorkspaceRequestedSourceUpdate(t *testing.T) {
	tests := []struct {
		name    string
		message string
		intent  service.SummaryIntent
		want    summaryWorkspaceSourceUpdateMode
	}{
		{name: "replace named group", message: "改成项目群重新总结", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceReplace},
		{name: "direct named group", message: "总结产品群最近的进展", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceReplace},
		{name: "replace all chats", message: "改成全部会话", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceReplace},
		{name: "select named chats", message: "选择项目群和产品群", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceReplace},
		{name: "bare direct chat", message: "和张三的私聊", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceReplace},
		{name: "extend group", message: "再加上运营群", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceExtend},
		{name: "extend direct chat", message: "同时包含和张三的私聊", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceExtend},
		{name: "content mention", message: "补充项目群里提到的风险", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceUnchanged},
		{name: "keep current", message: "保持当前会话，只调整结构", intent: service.SummaryIntentRevise, want: summaryWorkspaceSourceUnchanged},
		{name: "explanation never reopens", message: "为什么没有总结项目群", intent: service.SummaryIntentExplain, want: summaryWorkspaceSourceUnchanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summaryWorkspaceRequestedSourceUpdate(tt.message, summaryWorkspaceInputUser, tt.intent); got != tt.want {
				t.Fatalf("source update=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestMaterializeWorkspaceAgentContextReplacesHydratedChannels(t *testing.T) {
	preview := workspacePreviewMessage(t, []agent.SummaryResponseChannel{{ChannelID: "old-group", ChannelType: 2, ChannelName: "旧群"}})
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "ui-group", ChatType: "group", Name: "页面群"}}

	got, _, err := (&summaryWorkspaceCoordinator{}).materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{CurrentPreview: preview},
		"改成项目群重新总结", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if len(got.SelectedChannels) != 0 {
		t.Fatalf("replacement retained old channels: %#v", got.SelectedChannels)
	}
}

func TestMaterializeWorkspaceAgentContextKeepsChannelsForExtension(t *testing.T) {
	preview := workspacePreviewMessage(t, []agent.SummaryResponseChannel{{ChannelID: "old-group", ChannelType: 2, ChannelName: "旧群"}})
	contextValue := emptySummaryWorkspaceContext()

	got, _, err := (&summaryWorkspaceCoordinator{}).materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{CurrentPreview: preview},
		"再加上运营群", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "old-group" {
		t.Fatalf("extension lost hydrated channels: %#v", got.SelectedChannels)
	}
}

func workspacePreviewMessage(t *testing.T, channels []agent.SummaryResponseChannel) *model.AgentMessage {
	t.Helper()
	payload, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType: agent.SummaryResultAgentPreview,
		Preview: &agent.SummaryResponsePreview{
			Content:        "preview",
			EffectiveScope: &agent.SummaryResponseEffectiveScope{Channels: channels},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := string(payload)
	return &model.AgentMessage{ResponsePayload: &value}
}
