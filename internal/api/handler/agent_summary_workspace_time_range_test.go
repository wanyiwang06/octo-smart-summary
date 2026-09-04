package handler

import (
	"encoding/json"
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
		{name: "relative half month", message: "请按最近半个月重新总结", label: "最近半个月", days: 15, ok: true},
		{name: "last range wins", message: "不要最近一个月，时间范围改成最近七天", label: "最近 7 天", days: 7, ok: true},
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
