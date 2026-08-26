package service

import "testing"

func TestDeriveSummaryRoute(t *testing.T) {
	tests := []struct {
		name string
		in   SummaryRouteInput
		want SummaryRoute
	}{
		{
			name: "personal workflow requires source template and generate intent",
			in: SummaryRouteInput{
				Intent:              SummaryIntentGenerate,
				HasValidSource:      true,
				HasSelectedTemplate: true,
			},
			want: SummaryRoutePersonalWorkflow,
		},
		{
			name: "participants take the team confirmation route without template",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasValidSource:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamConfirmation,
		},
		{
			name: "invalid participant blocks side effects",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasValidSource:       true,
				HasOtherParticipants: true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "free form request generates preview",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasEnoughContextForPreview: true,
				ParticipantsValid:          true,
			},
			want: SummaryRouteAgentPreview,
		},
		{
			name: "revision requires an existing preview",
			in: SummaryRouteInput{
				Intent:              SummaryIntentRevise,
				HasCurrentPreview:   true,
				PreviewScopeMatches: true,
			},
			want: SummaryRouteAgentRevision,
		},
		{
			name: "revision without preview clarifies",
			in: SummaryRouteInput{
				Intent: SummaryIntentRevise,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "participant scope invalidates personal preview revision",
			in: SummaryRouteInput{
				Intent:               SummaryIntentRevise,
				HasCurrentPreview:    true,
				PreviewScopeMatches:  true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "explanation remains available with incomplete execution scope",
			in: SummaryRouteInput{
				Intent:               SummaryIntentExplain,
				HasValidSource:       true,
				HasSelectedTemplate:  true,
				HasOtherParticipants: true,
				HasHardMissingData:   true,
			},
			want: SummaryRouteExplanation,
		},
		{
			name: "confirmed current team proposal creates workflow",
			in: SummaryRouteInput{
				Action:                   SummaryActionConfirmWorkflow,
				HasValidSource:           true,
				HasOtherParticipants:     true,
				ParticipantsValid:        true,
				HasTeamProposal:          true,
				TeamProposalScopeMatches: true,
			},
			want: SummaryRouteTeamWorkflow,
		},
		{
			name: "stale team proposal cannot create workflow",
			in: SummaryRouteInput{
				Action:               SummaryActionConfirmWorkflow,
				HasValidSource:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
				HasTeamProposal:      true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "current preview can be saved",
			in: SummaryRouteInput{
				Action:              SummaryActionSavePreview,
				HasCurrentPreview:   true,
				PreviewScopeMatches: true,
			},
			want: SummaryRouteAgentPreviewSave,
		},
		{
			name: "stale preview cannot be saved",
			in: SummaryRouteInput{
				Action:            SummaryActionSavePreview,
				HasCurrentPreview: true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "hard missing data overrides all routes",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasValidSource:             true,
				HasSelectedTemplate:        true,
				ParticipantsValid:          true,
				HasEnoughContextForPreview: true,
				HasHardMissingData:         true,
			},
			want: SummaryRouteClarification,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveSummaryRoute(tt.in); got != tt.want {
				t.Fatalf("DeriveSummaryRoute() = %q, want %q", got, tt.want)
			}
		})
	}
}
