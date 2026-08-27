package service

// SummaryIntent is the natural-language intent already classified by the
// Agent. The policy combines it with trusted structured context; it does not
// let the model decide whether a side-effecting workflow is allowed.
type SummaryIntent string

const (
	SummaryIntentGenerate SummaryIntent = "generate"
	SummaryIntentRevise   SummaryIntent = "revise"
	SummaryIntentExplain  SummaryIntent = "explain"
	SummaryIntentUnknown  SummaryIntent = "unknown"
)

// SummaryAction is the trusted UI action attached to a request. Chat asks the
// policy to derive a route; confirm/save actions must satisfy persisted-state
// guards before a side effect is allowed.
type SummaryAction string

const (
	SummaryActionChat            SummaryAction = "chat"
	SummaryActionConfirmWorkflow SummaryAction = "confirm_workflow"
	SummaryActionSavePreview     SummaryAction = "save_preview"
)

// SummaryRoute is the next operation allowed by the deterministic policy.
type SummaryRoute string

const (
	SummaryRouteClarification    SummaryRoute = "clarification"
	SummaryRoutePersonalWorkflow SummaryRoute = "personal_workflow"
	SummaryRouteTeamConfirmation SummaryRoute = "team_workflow_confirmation"
	SummaryRouteTeamWorkflow     SummaryRoute = "team_workflow"
	SummaryRouteAgentPreview     SummaryRoute = "agent_preview"
	SummaryRouteAgentRevision    SummaryRoute = "agent_revision"
	SummaryRouteAgentPreviewSave SummaryRoute = "agent_preview_save"
	SummaryRouteExplanation      SummaryRoute = "explanation"
)

// SummaryRouteInput contains only facts needed to select an execution path.
// Permission checks happen before setting HasValidSource/ParticipantsValid.
type SummaryRouteInput struct {
	Action                     SummaryAction
	Intent                     SummaryIntent
	HasExplicitRunIntent       bool
	HasValidSource             bool
	HasSelectedTemplate        bool
	HasOtherParticipants       bool
	ParticipantsValid          bool
	HasCurrentPreview          bool
	PreviewScopeMatches        bool
	HasTeamProposal            bool
	TeamProposalScopeMatches   bool
	HasEnoughContextForPreview bool
	HasHardMissingData         bool
}

// DeriveSummaryRoute applies the server-side routing boundary for the unified
// entry. Team impact has priority over personal execution; only a generate
// intent can start or propose a workflow.
func DeriveSummaryRoute(in SummaryRouteInput) SummaryRoute {
	switch in.Action {
	case SummaryActionConfirmWorkflow:
		if in.HasHardMissingData || !in.HasOtherParticipants || !in.ParticipantsValid ||
			!in.HasValidSource || !in.HasTeamProposal || !in.TeamProposalScopeMatches {
			return SummaryRouteClarification
		}
		return SummaryRouteTeamWorkflow
	case SummaryActionSavePreview:
		if in.HasCurrentPreview && in.PreviewScopeMatches {
			return SummaryRouteAgentPreviewSave
		}
		return SummaryRouteClarification
	case "", SummaryActionChat:
		// Continue below. Empty keeps zero-value callers backward compatible.
	default:
		return SummaryRouteClarification
	}

	// Explanation is read-only and must remain available even if the current
	// execution scope is incomplete or stale.
	if in.Intent == SummaryIntentExplain {
		return SummaryRouteExplanation
	}
	if in.HasHardMissingData || (in.HasOtherParticipants && !in.ParticipantsValid) {
		return SummaryRouteClarification
	}

	switch in.Intent {
	case SummaryIntentRevise:
		// Adding collaborators changes the execution scope. An older personal
		// preview must not be revised or saved as if that scope were unchanged;
		// the Agent needs to establish a new team proposal first.
		if in.HasOtherParticipants {
			return SummaryRouteClarification
		}
		if in.HasCurrentPreview && in.PreviewScopeMatches {
			return SummaryRouteAgentRevision
		}
		return SummaryRouteClarification
	case SummaryIntentGenerate:
		if in.HasOtherParticipants {
			if !in.HasValidSource {
				return SummaryRouteClarification
			}
			return SummaryRouteTeamConfirmation
		}
		if in.HasExplicitRunIntent && in.HasValidSource && in.HasSelectedTemplate {
			return SummaryRoutePersonalWorkflow
		}
		if in.HasEnoughContextForPreview {
			return SummaryRouteAgentPreview
		}
	}

	return SummaryRouteClarification
}
