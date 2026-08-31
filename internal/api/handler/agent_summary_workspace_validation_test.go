//go:build cgo

package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newSummaryWorkspaceIMValidationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	statements := []string{
		`CREATE TABLE "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at INTEGER)`,
		`CREATE TABLE group_member (group_no TEXT, uid TEXT, is_deleted INTEGER, role INTEGER)`,
		`CREATE TABLE conversation_extra (channel_id TEXT, uid TEXT, channel_type INTEGER, updated_at INTEGER)`,
		`CREATE TABLE thread (id INTEGER PRIMARY KEY, short_id TEXT, name TEXT, group_no TEXT, status INTEGER, message_count INTEGER, creator_uid TEXT, updated_at INTEGER)`,
		`CREATE TABLE space_member (space_id TEXT, uid TEXT, status INTEGER)`,
		`CREATE TABLE message (message_seq INTEGER, from_uid TEXT, channel_id TEXT, channel_type INTEGER, timestamp INTEGER, payload BLOB, is_deleted INTEGER DEFAULT 0)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("create IM schema: %v", err)
		}
	}
	if err := db.Exec(`INSERT INTO "group" VALUES ('group-a','A群','space-a',1,1),('group-b','B群','space-b',1,1)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO group_member VALUES ('group-a','actor',0,0),('group-a','member',0,0),('group-b','actor',0,0),('group-b','member',0,0)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO conversation_extra VALUES ('peer-in@actor','actor',1,2),('peer-out@actor','actor',1,1)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO space_member VALUES ('space-a','actor',1),('space-a','member',1),('space-a','peer-in',1),('space-b','peer-out',1)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSummaryWorkspaceRecentChannelUsesNewestAuthorisedMessageInSpace(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	canonicalDM := pipeline.NormalizeDMChannelID("peer-in", "actor", model.ChannelTypeDM)
	rows := []struct {
		id      string
		kind    int
		when    time.Time
		content string
	}{
		{id: "group-a", kind: model.ChannelTypeGroup, when: now.Add(-24 * time.Hour), content: "group"},
		{id: canonicalDM, kind: model.ChannelTypeDM, when: now.Add(-2 * time.Hour), content: "dm"},
		{id: "group-b", kind: model.ChannelTypeGroup, when: now.Add(-time.Hour), content: "cross-space"},
	}
	for i, row := range rows {
		if err := imDB.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, 'actor', ?, ?, ?, ?, 0)`, i+1, row.id, row.kind, row.when.Unix(), []byte(`{"content":"`+row.content+`"}`)).Error; err != nil {
			t.Fatal(err)
		}
	}
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB, messageTableCount: 1, now: func() time.Time { return now }}
	got, err := coordinator.findMostRecentAuthorizedChannel(context.Background(), "space-a", "actor", now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("find recent channel: %v", err)
	}
	if got.ChatType != "direct" || got.ChatID != canonicalDM {
		t.Fatalf("recent channel = %#v, want authorised in-space DM %q", got, canonicalDM)
	}
}

func TestSummaryWorkspaceRecentChannelDoesNotExpandPastEffectiveRange(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := imDB.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (1, 'actor', 'group-a', 2, ?, X'01', 0)`, now.Add(-8*24*time.Hour).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB, messageTableCount: 1}
	_, err := coordinator.findMostRecentAuthorizedChannel(context.Background(), "space-a", "actor", now.Add(-7*24*time.Hour), now)
	if !errors.Is(err, errSummaryWorkspaceNoRecentChannel) {
		t.Fatalf("find recent channel error = %v, want no recent channel", err)
	}
}

func TestMaterializeTemplateOnlyContextPinsRecentChannelAndSevenDays(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := imDB.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (1, 'actor', 'group-a', 2, ?, X'01', 0)`, now.Add(-time.Hour).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB, messageTableCount: 1, now: func() time.Time { return now }}
	contextValue := summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{},
		Participants:      []summaryWorkspaceParticipant{},
		Template:          &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "总结进展"},
		ReferencedTaskIDs: []int64{},
	}
	got, inferred, err := coordinator.materializeWorkspaceAgentContext(context.Background(), "space-a", "actor", contextValue, WorkspaceSnapshot{}, service.SummaryIntentGenerate, summaryWorkspaceInputSystemIntent)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if !inferred || len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "group-a" {
		t.Fatalf("materialized source = %#v inferred=%t", got.SelectedChannels, inferred)
	}
	start, end, err := parseSummaryWorkspaceTimeRange(got.TimeRange)
	if err != nil || end.Sub(start) != 7*24*time.Hour {
		t.Fatalf("materialized range = %#v duration=%s err=%v", got.TimeRange, end.Sub(start), err)
	}
}

func TestMaterializeOpenScopeAgentContextPinsSevenDays(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}

	got, inferred, err := coordinator.materializeWorkspaceAgentContext(
		context.Background(), "space-a", "actor", emptySummaryWorkspaceContext(), WorkspaceSnapshot{},
		service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize open-scope context: %v", err)
	}
	if inferred || len(got.SelectedChannels) != 0 {
		t.Fatalf("open-scope context unexpectedly inferred a channel: %#v inferred=%t", got.SelectedChannels, inferred)
	}
	start, end, err := parseSummaryWorkspaceTimeRange(got.TimeRange)
	if err != nil || end.Sub(start) != 7*24*time.Hour {
		t.Fatalf("materialized range = %#v duration=%s err=%v", got.TimeRange, end.Sub(start), err)
	}
}

func TestSummaryWorkspaceValidateSourcesEnforcesSpaceAndDMMembership(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB}
	ctx := context.Background()

	valid, err := coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}})
	if err != nil || !valid {
		t.Fatalf("same-space group valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "group-b", ChatType: "group", Name: "B群"}})
	if err != nil || valid {
		t.Fatalf("cross-space group valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "peer-in", ChatType: "direct", Name: "私聊"}})
	if err != nil || !valid {
		t.Fatalf("same-space DM peer valid=%t err=%v", valid, err)
	}
	canonical := pipeline.NormalizeDMChannelID("peer-in", "actor", model.ChannelTypeDM)
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: canonical, ChatType: "direct", Name: "私聊"}})
	if err != nil || !valid {
		t.Fatalf("canonical DM valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateSources(ctx, "space-a", "actor", []summaryWorkspaceChannel{{ChatID: "peer-out", ChatType: "direct", Name: "外部私聊"}})
	if err != nil || valid {
		t.Fatalf("out-of-space DM peer valid=%t err=%v", valid, err)
	}
}

func TestSummaryWorkspaceValidateTeamScopeRequiresOneSharedGroup(t *testing.T) {
	imDB := newSummaryWorkspaceIMValidationDB(t)
	coordinator := &summaryWorkspaceCoordinator{imDB: imDB}
	ctx := context.Background()
	participants := []summaryWorkspaceParticipant{{UserID: "member", UserName: "成员"}}

	valid, err := coordinator.validateTeamScope(ctx, "actor", []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}, participants)
	if err != nil || !valid {
		t.Fatalf("shared group valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateTeamScope(ctx, "actor", []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}, []summaryWorkspaceParticipant{{UserID: "outsider"}})
	if err != nil || valid {
		t.Fatalf("non-member valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateTeamScope(ctx, "actor", []summaryWorkspaceChannel{{ChatID: "peer-in", ChatType: "direct", Name: "私聊"}}, participants)
	if err != nil || valid {
		t.Fatalf("DM team scope valid=%t err=%v", valid, err)
	}
	valid, err = coordinator.validateTeamScope(ctx, "actor", []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}, {ChatID: "group-b", ChatType: "group", Name: "B群"}}, participants)
	if err != nil || valid {
		t.Fatalf("multi-source team scope valid=%t err=%v", valid, err)
	}
}
