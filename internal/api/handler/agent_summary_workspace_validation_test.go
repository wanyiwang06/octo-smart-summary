//go:build cgo

package handler

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
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
