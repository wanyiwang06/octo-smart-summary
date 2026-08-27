package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestAllowedChannelScopeIsOptInAndExact(t *testing.T) {
	if restricted, allowed := ChannelAllowedByScope(context.Background(), "group-1", 2); restricted || allowed {
		t.Fatalf("unscoped legacy context = (%v, %v), want (false, false)", restricted, allowed)
	}

	ctx := WithAllowedChannelScope(context.Background(), []ChannelScope{
		{ChannelID: "group-1", ChannelType: 2},
	})
	if restricted, allowed := ChannelAllowedByScope(ctx, "group-1", 2); !restricted || !allowed {
		t.Fatalf("selected channel = (%v, %v), want (true, true)", restricted, allowed)
	}
	for _, candidate := range []ChannelScope{
		{ChannelID: "group-2", ChannelType: 2},
		{ChannelID: "group-1", ChannelType: 5},
	} {
		if restricted, allowed := ChannelAllowedByScope(ctx, candidate.ChannelID, candidate.ChannelType); !restricted || allowed {
			t.Fatalf("unselected channel %+v = (%v, %v), want (true, false)", candidate, restricted, allowed)
		}
	}

	empty := WithAllowedChannelScope(context.Background(), nil)
	if restricted, allowed := ChannelAllowedByScope(empty, "group-1", 2); !restricted || allowed {
		t.Fatalf("empty explicit scope = (%v, %v), want (true, false)", restricted, allowed)
	}
}

func TestAllowedChannelScopeCanonicalizesLogicalDMID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "self-user")
	canonical := pipeline.NormalizeDMChannelID("peer-user", "self-user", model.ChannelTypeDM)

	ctx = WithAllowedChannelScope(ctx, []ChannelScope{{ChannelID: canonical, ChannelType: model.ChannelTypeDM}})
	if restricted, allowed := ChannelAllowedByScope(ctx, "peer-user", model.ChannelTypeDM); !restricted || !allowed {
		t.Fatalf("logical DM id did not match selected canonical id: (%v, %v), canonical=%q", restricted, allowed, canonical)
	}
}

func TestChannelReadToolsRejectOutsideSelectedScopeBeforeLookup(t *testing.T) {
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-1")
	ctx = WithAllowedChannelScope(ctx, []ChannelScope{{ChannelID: "group-1", ChannelType: 2}})

	_, fetch := FetchChannelTool()
	if _, err := fetch(ctx, []byte(`{"channel_id":"group-2","time_start":"2026-08-20T00:00:00Z","time_end":"2026-08-27T00:00:00Z"}`)); err == nil || !strings.Contains(err.Error(), "channel_type is required") {
		t.Fatalf("missing channel type must remain a repairable argument error, got %v", err)
	}
	if _, err := fetch(ctx, []byte(`{"channel_id":"group-2","channel_type":2,"time_start":"2026-08-20T00:00:00Z","time_end":"2026-08-27T00:00:00Z"}`)); err == nil || !strings.Contains(err.Error(), "outside the selected summary scope") {
		t.Fatalf("fetch outside scope error = %v", err)
	} else {
		var outside *ErrChannelOutsideSelectedScope
		if !errors.As(err, &outside) || outside.ChannelID != "group-2" || outside.ChannelType != 2 {
			t.Fatalf("fetch error type = %T %+v", err, err)
		}
	}

	_, peek := PeekChannelTool()
	if _, err := peek(ctx, []byte(`{"channel_id":"group-1","channel_type":5}`)); err == nil || !strings.Contains(err.Error(), "outside the selected summary scope") {
		t.Fatalf("peek with mismatched type error = %v", err)
	}
}
