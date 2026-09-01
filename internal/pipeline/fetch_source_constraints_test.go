package pipeline

import "testing"

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
