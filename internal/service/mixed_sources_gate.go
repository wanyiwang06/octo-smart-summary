package service

import (
	"fmt"
	"sync/atomic"
)

// Mixed document+chat source admission (phase 1). See
// docs/mixed-document-chat-summary-development-plan.md §1.2/§4.3.
//
// Mixed creation is behind an environment gate (default OFF) until the worker
// executor half lands. Turning the gate on without the worker would admit a
// task that every pipeline entry point hard-rejects, so the admission surface
// and the executor must flip together. When the gate is OFF, mixed scopes are
// rejected with the same clear contract error the phase-1 boundary already
// uses.

// MixedMaxTotalSources bounds the COMBINED document+chat source count of a
// mixed scope. Documents additionally respect MaxDocumentSummarySourceCount.
const MixedMaxTotalSources = 30

// mixedSourcesAdmission is the process-wide rollout switch. It is written once
// at startup (router assembly) and read on every admission decision.
var mixedSourcesAdmission atomic.Bool

// SetMixedSourcesAdmission records the environment-level rollout decision.
// false (the default) rejects mixed document+chat admission so the contract
// and source-merge work can ship and be tested without exposing a flow whose
// executor does not exist yet.
func SetMixedSourcesAdmission(enabled bool) {
	mixedSourcesAdmission.Store(enabled)
}

// MixedSourcesAdmissionEnabled reports whether mixed document+chat creation is
// admitted. The worker follow-up PR flips this on at the same time it lands
// the mixed executor.
func MixedSourcesAdmissionEnabled() bool {
	return mixedSourcesAdmission.Load()
}

// MixedSourceCountExceededError returns the standard rejection for a mixed
// scope over MixedMaxTotalSources.
func MixedSourceCountExceededError() *BizError {
	return NewBizError(40001, fmt.Sprintf("混合来源总数不能超过%d个", MixedMaxTotalSources), 400)
}
