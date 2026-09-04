package pipeline

import (
	"context"
	"reflect"
	"testing"
)

func TestGetPeerUIDRequiresActorParticipation(t *testing.T) {
	tests := []struct {
		name      string
		channelID string
		selfUID   string
		want      string
	}{
		{name: "actor first", channelID: "actor@peer", selfUID: "actor", want: "peer"},
		{name: "actor second", channelID: "peer@actor", selfUID: "actor", want: "peer"},
		{name: "malformed", channelID: "peer", selfUID: "actor", want: ""},
		{name: "actor absent", channelID: "peer-a@peer-b", selfUID: "actor", want: ""},
		{name: "self dm", channelID: "actor@actor", selfUID: "actor", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getPeerUID(tt.channelID, tt.selfUID); got != tt.want {
				t.Fatalf("getPeerUID(%q, %q) = %q, want %q", tt.channelID, tt.selfUID, got, tt.want)
			}
		})
	}
}

func TestApplyParticipantChannelScopePreservesWorkspaceUnionSources(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-a"}, {ChannelID: "group-b"}}
	got, err := applyParticipantChannelScope(
		context.Background(), channels, []string{"member-a", "member-b"}, nil,
		&ChannelScopeOptions{ParticipantSourceUnion: true},
	)
	if err != nil {
		t.Fatalf("applyParticipantChannelScope() error = %v", err)
	}
	if !reflect.DeepEqual(got, channels) {
		t.Fatalf("channels = %#v, want %#v", got, channels)
	}
}

func TestValidateExplicitSourceCoverageRejectsFilteredSource(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-visible"}}
	sources := []map[string]interface{}{
		{"source_id": "group-visible", "source_type": 1},
		{"source_id": "group-filtered", "source_type": 1},
	}

	if err := validateExplicitSourceCoverage(channels, sources, "user-1"); err == nil {
		t.Fatal("expected explicitly selected filtered source to be rejected")
	}
}

func TestValidateExplicitSourceCoverageAcceptsAllSources(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-a"}, {ChannelID: "group-b"}}
	sources := []map[string]interface{}{
		{"source_id": "group-a", "source_type": 1},
		{"source_id": "group-b", "source_type": 1},
	}

	if err := validateExplicitSourceCoverage(channels, sources, "user-1"); err != nil {
		t.Fatalf("validateExplicitSourceCoverage() error = %v", err)
	}
}
