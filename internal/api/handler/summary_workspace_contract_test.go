package handler

import (
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestNormalizeSummaryWorkspaceContext(t *testing.T) {
	got, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{
			{ChatID: " group-1 ", ChatType: " GROUP ", Name: " 项目群 "},
			{ChatID: "group-1", ChatType: "group", Name: "重复"},
		},
		Participants: []summaryWorkspaceParticipant{
			{UserID: " u2 ", UserName: " 张三 "},
			{UserID: "u2", UserName: "重复"},
		},
		ReferencedTaskIDs: []int64{9, 9, 10},
	})
	if err != nil {
		t.Fatalf("normalize context: %v", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "group-1" || got.SelectedChannels[0].ChatType != "group" {
		t.Fatalf("unexpected channels: %#v", got.SelectedChannels)
	}
	if len(got.Participants) != 1 || got.Participants[0].UserID != "u2" || got.Participants[0].UserName != "张三" {
		t.Fatalf("unexpected participants: %#v", got.Participants)
	}
	if !reflect.DeepEqual(got.ReferencedTaskIDs, []int64{9, 10}) {
		t.Fatalf("unexpected references: %#v", got.ReferencedTaskIDs)
	}
	if got.Template != nil || got.TimeRange != nil {
		t.Fatalf("nil optional values changed: %#v", got)
	}
}

func TestNormalizeSummaryWorkspaceContextRejectsInvalidRange(t *testing.T) {
	_, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		TimeRange: &summaryWorkspaceTimeRange{
			Start: "2026-08-27T10:00:00Z",
			End:   "2026-08-26T10:00:00Z",
			Label: "昨天",
		},
	})
	if err == nil {
		t.Fatal("expected invalid time range")
	}
}

func TestWorkspaceActionsOnlyPreviewCanSave(t *testing.T) {
	for _, resultType := range []string{
		workspaceResultClarification,
		workspaceResultExplanation,
		workspaceResultWorkflowConfirm,
		workspaceResultWorkflowStarted,
		workspaceResultWorkflowCompleted,
		workspaceResultError,
	} {
		for _, action := range workspaceActionsForResult(resultType, false) {
			if action == workspaceActionSavePreview {
				t.Fatalf("result %s unexpectedly saveable", resultType)
			}
		}
	}
	for _, resultType := range []string{workspaceResultAgentPreview, workspaceResultAgentRevision} {
		if !reflect.DeepEqual(workspaceActionsForResult(resultType, false), []string{workspaceActionSavePreview, workspaceActionContinueChat}) {
			t.Fatalf("result %s must be saveable", resultType)
		}
		if reflect.DeepEqual(workspaceActionsForResult(resultType, true), []string{workspaceActionSavePreview, workspaceActionContinueChat}) {
			t.Fatalf("saved result %s must not stay saveable", resultType)
		}
	}
}

func TestClassifySummaryWorkspaceIntentKeepsQuestionsReadOnly(t *testing.T) {
	tests := []struct {
		name       string
		message    string
		hasPreview bool
		want       service.SummaryIntent
	}{
		{name: "template question before preview", message: "这个模板包含什么内容？", want: service.SummaryIntentExplain},
		{name: "explicit generation", message: "请根据当前选择生成总结", want: service.SummaryIntentGenerate},
		{name: "polite generation question", message: "能帮我总结一下吗？", want: service.SummaryIntentGenerate},
		{name: "negated generation", message: "先不要生成总结，我还要补充要求", want: service.SummaryIntentExplain},
		{name: "explicit preview overrides formal negation", message: "不要生成正式总结，先给预览", want: service.SummaryIntentGenerate},
		{name: "free form requirement", message: "重点关注风险和行动项", want: service.SummaryIntentGenerate},
		{name: "preview follow-up", message: "把行动项放到最前面", hasPreview: true, want: service.SummaryIntentRevise},
		{name: "explicit regeneration updates preview", message: "请重新生成总结", hasPreview: true, want: service.SummaryIntentRevise},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySummaryWorkspaceIntent(tt.message, tt.hasPreview); got != tt.want {
				t.Fatalf("classifySummaryWorkspaceIntent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasExplicitSummaryRunIntent(t *testing.T) {
	for _, message := range []string{
		"请根据当前选择生成总结",
		"请根据当前选择准备多人总结任务",
		"Generate a summary from the current selection",
		"Prepare a team summary task from the current selection",
		"Summarize the selected conversation",
	} {
		if !hasExplicitSummaryRunIntent(message) {
			t.Errorf("message %q must authorize an execution route", message)
		}
	}

	for _, message := range []string{
		"这个模板包含什么内容？",
		"重点关注风险和行动项",
		"为什么会得到这个结论",
		"先不要生成总结，我还要补充要求",
		"不要生成正式总结，先给预览",
	} {
		if hasExplicitSummaryRunIntent(message) {
			t.Errorf("message %q must remain free of workflow side effects", message)
		}
	}
}

func TestExplicitExecutionCommandOverridesExistingPreviewWithoutHijackingRevision(t *testing.T) {
	if !hasExplicitSummaryExecutionCommand("开始总结") ||
		!hasExplicitSummaryExecutionCommand("发起多人总结：按风险排序") ||
		!hasExplicitSummaryExecutionCommand("请根据当前选择生成总结") ||
		!hasExplicitSummaryExecutionCommand("请根据当前选择准备多人总结任务") ||
		!hasExplicitSummaryExecutionCommand("Generate a summary from the current selection") ||
		!hasExplicitSummaryExecutionCommand("Prepare a team summary task from the current selection") {
		t.Fatal("fixed execution commands must be recognized")
	}
	if hasExplicitSummaryExecutionCommand("请总结得更简洁") {
		t.Fatal("revision wording must not be treated as an execution command")
	}
	if got := classifySummaryWorkspaceIntent("请总结得更简洁", true); got != service.SummaryIntentRevise {
		t.Fatalf("revision intent=%q", got)
	}
}

func TestSummaryWorkspaceRequirementOmitsGeneratedExecutionMessage(t *testing.T) {
	template := &summaryWorkspaceTemplate{
		TemplateID:  "weekly",
		Label:       "周报",
		Requirement: "按进展、风险和下一步输出",
	}
	if got := summaryWorkspaceRequirement(summaryWorkspaceContext{Template: template}, "请根据当前选择生成总结"); got != template.Requirement {
		t.Fatalf("template requirement = %q, want %q", got, template.Requirement)
	}
	if got := summaryWorkspaceRequirement(summaryWorkspaceContext{}, "Prepare a team summary task from the current selection"); got != "请总结关键结论、进展、风险和行动项" {
		t.Fatalf("default team requirement = %q", got)
	}
	if got := summaryWorkspaceRequirement(summaryWorkspaceContext{Template: template}, "重点关注延期风险"); got != template.Requirement+"\n\n重点关注延期风险" {
		t.Fatalf("custom requirement = %q", got)
	}
}

func TestCanonicalizeSummaryWorkspaceContextForActor(t *testing.T) {
	got := canonicalizeSummaryWorkspaceContextForActor(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{
			{ChatID: "peer", ChatType: "direct", Name: "私聊"},
			{ChatID: "peer@actor", ChatType: "direct", Name: "重复私聊"},
		},
		Participants: []summaryWorkspaceParticipant{
			{UserID: "actor", UserName: "自己"},
			{UserID: "other", UserName: "成员"},
		},
		ReferencedTaskIDs: []int64{},
	}, "actor")
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != pipeline.NormalizeDMChannelID("peer", "actor", model.ChannelTypeDM) {
		t.Fatalf("direct channels were not canonicalized and deduplicated: %#v", got.SelectedChannels)
	}
	if len(got.Participants) != 1 || got.Participants[0].UserID != "other" {
		t.Fatalf("actor was not removed from participants: %#v", got.Participants)
	}
}

func TestEmptySummaryWorkspaceHistoryUsesV1Shape(t *testing.T) {
	history := emptySummaryWorkspaceHistory("session-1")
	if history.ContractVersion != summaryWorkspaceContractVersion || history.SessionID != "session-1" || history.State.ScopeVersion != 1 {
		t.Fatalf("unexpected empty history: %#v", history)
	}
	if history.Messages == nil || history.State.SummaryContext.SelectedChannels == nil || history.State.SummaryContext.Participants == nil || history.State.SummaryContext.ReferencedTaskIDs == nil {
		t.Fatalf("empty workspace collections must encode as arrays: %#v", history)
	}
	if history.State.CurrentPreview != nil || history.State.PendingProposal != nil || history.State.Workflow != nil {
		t.Fatalf("empty workspace state contains artifacts: %#v", history.State)
	}
}

func TestSummaryWorkspaceRequestHashIncludesScopeVersion(t *testing.T) {
	v1 := summaryWorkspaceRequestHash("chat", "请总结", 1, "same-scope")
	v2 := summaryWorkspaceRequestHash("chat", "请总结", 2, "same-scope")
	if v1 == v2 {
		t.Fatal("request hash must change when scope_version changes")
	}
}
