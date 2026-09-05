package handler

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestSummaryWorkspaceRequestedPresetTimeRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	cases := []struct {
		name    string
		message string
		label   string
		days    int
		ok      bool
	}{
		{name: "explicit month", message: "把时间范围扩大到一个月", label: "最近一个月", days: 30, ok: true},
		{name: "month with expansion verb", message: "扩大到一个月再生成", label: "最近一个月", days: 30, ok: true},
		{name: "month with shrink verb", message: "把时间范围缩小到一个月", label: "最近一个月", days: 30, ok: true},
		{name: "relative half month", message: "请按最近半个月重新总结", label: "最近半个月", days: 15, ok: true},
		{name: "last range wins", message: "不要最近一个月，时间范围改成最近七天", label: "最近 7 天", days: 7, ok: true},
		{name: "negated trailing range is ignored", message: "时间范围改成最近七天，不要最近一个月", label: "最近 7 天", days: 7, ok: true},
		{name: "informal selection ignores negated range", message: "最近七天就好，别用最近一个月", label: "最近 7 天", days: 7, ok: true},
		{name: "negation may precede the range command", message: "请不要把时间范围改成最近一个月", ok: false},
		{name: "negative expansion is ignored", message: "无需把时间范围扩大到一个月", ok: false},
		{name: "natural affirmative before range", message: "总结最近一个月的进展", label: "最近一个月", days: 30, ok: true},
		{name: "bare range is an explicit follow up", message: "最近一个月", label: "最近一个月", days: 30, ok: true},
		{name: "reference mention is not a range change", message: "参考最近一个月的总结，按选择器范围生成", ok: false},
		{name: "complaint keeps picker", message: "最近一个月的数据太多了，就总结我选的这三天", ok: false},
		{name: "past tense keeps picker", message: "上次总结用的是最近一个月", ok: false},
		{name: "question keeps picker", message: "最近一个月团队都在忙什么", ok: false},
		{name: "title keeps picker", message: "把标题改成最近一个月总结", ok: false},
		{name: "explicit correction uses affirmative range", message: "时间范围不是最近一个月，而是最近七天", label: "最近 7 天", days: 7, ok: true},
		{name: "unsupported three day range is not guessed", message: "最近三天的总结", ok: false},
		{name: "unsupported two week range is not guessed", message: "最近两周的总结", ok: false},
		{name: "unsupported ninety day range is not guessed", message: "最近90天的总结", ok: false},
		{name: "historical mention is not a range", message: "补充一个月前确定的结论", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := summaryWorkspaceRequestedPresetTimeRange(tc.message, now)
			if ok != tc.ok {
				t.Fatalf("matched=%t, want %t; range=%#v", ok, tc.ok, got)
			}
			if !tc.ok {
				return
			}
			if got.Label != tc.label {
				t.Fatalf("label=%q, want %q", got.Label, tc.label)
			}
			start, end, err := parseSummaryWorkspaceTimeRange(got)
			if err != nil {
				t.Fatalf("parse range: %v", err)
			}
			wantDuration := time.Duration(tc.days)*24*time.Hour - time.Nanosecond
			if end.Sub(start) != wantDuration {
				t.Fatalf("duration=%s, want %s", end.Sub(start), wantDuration)
			}
		})
	}
}

func TestMaterializeWorkspaceAgentContextKeepsExplicitPickerRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start:  now.AddDate(0, 0, -2).Format(time.RFC3339),
		End:    now.Format(time.RFC3339),
		Label:  "2026-09-02 至 2026-09-04",
		Source: summaryWorkspaceTimeRangeSourcePicker,
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"最近一个月的背景我知道，但只总结我选的这三天", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != contextValue.TimeRange.Label {
		t.Fatalf("time range=%#v, want explicit picker range %#v", got.TimeRange, contextValue.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextLetsExplicitCommandReplacePickerRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.AddDate(0, 0, -2).Format(time.RFC3339), End: now.Format(time.RFC3339),
		Label: "2026-09-02 至 2026-09-04", Source: summaryWorkspaceTimeRangeSourcePicker,
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"把时间范围扩大到最近一个月", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近一个月" || got.TimeRange.Source != summaryWorkspaceTimeRangeSourceConversation {
		t.Fatalf("time range=%#v, want conversational month", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextLetsDirectRequestReplacePickerRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.AddDate(0, 0, -2).Format(time.RFC3339), End: now.Format(time.RFC3339),
		Label: "2026-09-02 至 2026-09-04", Source: summaryWorkspaceTimeRangeSourcePicker,
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"总结最近一个月的进展", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近一个月" || got.TimeRange.Source != summaryWorkspaceTimeRangeSourceConversation {
		t.Fatalf("time range=%#v, want direct conversational month", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextLetsExplicitFollowUpReplacePersistedRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.AddDate(0, 0, -29).Format(time.RFC3339),
		End:   now.Format(time.RFC3339),
		Label: "最近一个月",
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"时间范围改成最近七天", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近 7 天" {
		t.Fatalf("time range=%#v, want 最近 7 天", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextMaterializesTeamDefaultRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.Participants = []summaryWorkspaceParticipant{{UserID: "member", UserName: "成员"}}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"总结本周进展", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || !strings.HasSuffix(got.TimeRange.Label, "（默认）") {
		t.Fatalf("time range=%#v, want materialized team default", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextAppliesFollowUpTimeRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		End:   now.Format(time.RFC3339),
		Label: "最近 7 天（默认）",
	}

	got, inferred, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"把时间范围扩大到一个月", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if inferred {
		t.Fatal("explicit source must not be inferred")
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近一个月" {
		t.Fatalf("time range=%#v, want 最近一个月", got.TimeRange)
	}
}

func TestHydrateSummaryWorkspaceContextKeepsNaturalLanguageRangeForNextTurn(t *testing.T) {
	oldRange := &summaryWorkspaceTimeRange{Start: "2026-08-29T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近 7 天"}
	payload := agent.SummaryResponsePayload{
		ResultType: agent.SummaryResultAgentRevision,
		Preview: &agent.SummaryResponsePreview{
			Content: "preview",
			EffectiveScope: &agent.SummaryResponseEffectiveScope{
				TimeRange: &agent.SummaryResponseTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = oldRange

	got, err := hydrateSummaryWorkspaceContextFromPreview(contextValue, &model.AgentMessage{ResponsePayload: &value}, "actor")
	if err != nil {
		t.Fatalf("hydrate context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近一个月" {
		t.Fatalf("time range=%#v, want persisted effective month", got.TimeRange)
	}
}

func TestApplySummaryWorkspaceProposalScopeUsesConfirmedRange(t *testing.T) {
	persisted := emptySummaryWorkspaceContext()
	persisted.Participants = []summaryWorkspaceParticipant{{UserID: "user-2", UserName: "成员"}}
	persisted.TimeRange = &summaryWorkspaceTimeRange{
		Start: "2026-08-29T00:00:00+08:00",
		End:   "2026-09-04T23:59:59+08:00",
		Label: "最近 7 天",
	}
	confirmed := &summaryWorkspaceTimeRange{
		Start:  "2026-08-06T00:00:00+08:00",
		End:    "2026-09-04T23:59:59+08:00",
		Label:  "最近一个月",
		Source: summaryWorkspaceTimeRangeSourceConversation,
	}

	got, err := applySummaryWorkspaceProposalScope(persisted, summaryWorkspaceProposal{
		Participants: persisted.Participants,
		TimeRange:    confirmed,
	}, "actor")
	if err != nil {
		t.Fatalf("apply proposal scope: %v", err)
	}
	if got.TimeRange == nil || !reflect.DeepEqual(*got.TimeRange, *confirmed) {
		t.Fatalf("time range=%#v, want confirmed range %#v", got.TimeRange, confirmed)
	}
}

func TestSummaryWorkspaceAssumptionsUseEffectiveRangeLabel(t *testing.T) {
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = &summaryWorkspaceTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"}

	got := summaryWorkspaceAssumptions(contextValue)
	if len(got) == 0 || got[0] != "时间范围使用最近一个月" {
		t.Fatalf("assumptions=%#v, want effective month first", got)
	}
}

func TestMergeSummaryWorkspaceAssumptionsReplacesStaleTimeRange(t *testing.T) {
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = &summaryWorkspaceTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"}

	got := mergeSummaryWorkspaceAssumptions([]string{"时间范围使用最近 7 天", "保留重点项目"}, contextValue)
	if len(got) != 4 {
		t.Fatalf("assumptions=%#v, want four canonical assumptions", got)
	}
	if got[0] != "保留重点项目" || got[1] != "时间范围使用最近一个月" {
		t.Fatalf("assumptions=%#v, stale time range was not replaced", got)
	}
}

func TestMergeSummaryWorkspaceAssumptionsKeepsOtherTimeRangeNotes(t *testing.T) {
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = &summaryWorkspaceTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"}

	got := mergeSummaryWorkspaceAssumptions([]string{
		"时间范围使用最近 7 天",
		"时间范围内未发现阻塞风险",
		"时间范围较长，结论以最新为准",
	}, contextValue)
	if !reflect.DeepEqual(got, []string{
		"时间范围内未发现阻塞风险",
		"时间范围较长，结论以最新为准",
		"时间范围使用最近一个月",
		"采用通用总结结构",
		"重点覆盖结论、进展、风险和行动项",
	}) {
		t.Fatalf("assumptions=%#v", got)
	}
}
