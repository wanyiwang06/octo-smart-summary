package service

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// PR1: service-layer mixed validation (validateDocumentWorkflowInput runs on
// BOTH create paths — legacy HTTP and agent workspace — so mixed admission
// must be recognisable here). Admission is gated behind
// MixedSourcesAdmissionEnabled (default OFF until the worker executor lands).

// enableMixedAdmission turns the gate on for a test and restores the prior
// state (default OFF) when the test finishes.
func enableMixedAdmission(t *testing.T) {
	t.Helper()
	SetMixedSourcesAdmission(true)
	t.Cleanup(func() { SetMixedSourcesAdmission(false) })
}

func mixedDocSource(id string) SummaryWorkflowSource {
	content := "文档内容 " + id
	hash := sha256.Sum256([]byte(content))
	return SummaryWorkflowSource{
		SourceType:      model.SourceDocument,
		SourceID:        id,
		SnapshotContent: content,
		SourceHash:      hex.EncodeToString(hash[:]),
	}
}

func mixedWorkflowInput() LegacyCreateSummaryWorkflowInput {
	return LegacyCreateSummaryWorkflowInput{
		ActorID:   "user-1",
		CreatorID: "user-1",
		SpaceID:   "space-1",
	}
}

func TestValidateDocumentWorkflowInputMixedAccepted(t *testing.T) {
	enableMixedAdmission(t)
	in := mixedWorkflowInput()
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		mixedDocSource("doc-1"),
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr != nil {
		t.Fatalf("mixed sources must validate: %v", bizErr)
	}
}

func TestValidateDocumentWorkflowInputMixedRejectedWhenGateOff(t *testing.T) {
	// The admission gate defaults OFF; a mixed scope must be rejected with the
	// same clear contract error the pure-document path uses BEFORE any snapshot
	// work runs (kills the "capabilities/admission hardcodes true" mutant).
	// No enableMixedAdmission call here.
	in := mixedWorkflowInput()
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		mixedDocSource("doc-1"),
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("mixed scope must be rejected when the admission gate is OFF")
	}
}

func TestValidateDocumentWorkflowInputMixedRejectsParticipants(t *testing.T) {
	enableMixedAdmission(t)
	in := mixedWorkflowInput()
	in.Participants = []SummaryWorkflowParticipant{{UserID: "user-2", UserName: "同事"}}
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		mixedDocSource("doc-1"),
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("mixed scope with participants must be rejected (phase-1 personal-only)")
	}
}

func TestValidateDocumentWorkflowInputMixedRejectsBadSnapshot(t *testing.T) {
	enableMixedAdmission(t)
	in := mixedWorkflowInput()
	sources := []SummaryWorkflowSource{
		{SourceType: model.SourceGroup, SourceID: "group-1"},
		{SourceType: model.SourceDocument, SourceID: "doc-1"}, // no snapshot
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("document source without snapshot must be rejected even in mixed scope")
	}
}

func TestValidateDocumentWorkflowInputMixedCountCap(t *testing.T) {
	enableMixedAdmission(t)
	in := mixedWorkflowInput()
	sources := make([]SummaryWorkflowSource, 0, MixedMaxTotalSources+1)
	sources = append(sources, SummaryWorkflowSource{SourceType: model.SourceGroup, SourceID: "group-1"})
	for i := 0; i < MixedMaxTotalSources; i++ {
		sources = append(sources, mixedDocSource("doc-"+string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("mixed scope over the combined cap must be rejected")
	}
}

func TestValidateDocumentWorkflowInputPureDocumentsUnchanged(t *testing.T) {
	// Mixed admission must not change the pure-document path.
	in := mixedWorkflowInput()
	in.TimeRange = &SummaryWorkflowTimeRange{}
	sources := []SummaryWorkflowSource{mixedDocSource("doc-1")}
	if bizErr := validateDocumentWorkflowInput(in, sources); bizErr == nil {
		t.Fatal("pure-document scope with explicit time range must stay rejected")
	}
}
