//go:build cgo

package handler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestResolveAgentMessageRequestIDUsesSpaceScopedWorkspaceRun(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	ctx := context.Background()
	internalSessionID := summaryWorkspaceAgentSessionID("space-a", "public-session", 1)
	run, _, err := summaryrun.NewStore(db).CreateOrGetRun(
		ctx, "user-1", internalSessionID, "request-1", model.ScopePolicyClosed,
	)
	if err != nil {
		t.Fatalf("create workspace run: %v", err)
	}
	message := model.AgentMessage{SpaceID: "space-a", ScopeVersion: 1, RunID: run.RunID}

	got, err := resolveAgentMessageRequestID(ctx, db, "user-1", "public-session", "request-1", message)
	if err != nil || got != "request-1" {
		t.Fatalf("resolve workspace request = %q, %v; want request-1", got, err)
	}

	message.SpaceID = "space-b"
	if _, err := resolveAgentMessageRequestID(ctx, db, "user-1", "public-session", "request-1", message); !errors.Is(err, errAgentMessageRunMismatch) {
		t.Fatalf("cross-space run binding error = %v, want errAgentMessageRunMismatch", err)
	}
}

func TestBuildSnapshotV1FiltersWorkspaceToolMessagesBySpace(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	now := time.Now()
	rows := []model.AgentMessage{
		{SpaceID: "space-a", UserID: "user-1", SessionID: "public-session", Role: "tool", Name: "fetch_channel", CreatedAt: now},
		{SpaceID: "space-b", UserID: "user-1", SessionID: "public-session", Role: "tool", Name: "peek_channel", CreatedAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed workspace tool messages: %v", err)
	}

	task := &model.SummaryTask{ID: 1, Title: "总结", TimeRangeStart: now.Add(-time.Hour), TimeRangeEnd: now}
	snapshot := (&AgentSummaryHandler{}).buildSnapshotV1(db, "public-session", "user-1", task, nil, "space-a")
	if want := []string{"fetch_channel x 1"}; !reflect.DeepEqual(snapshot.ToolSummary, want) {
		t.Fatalf("tool summary = %#v, want %#v", snapshot.ToolSummary, want)
	}
}
