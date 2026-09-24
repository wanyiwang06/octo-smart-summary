package service

import "fmt"

// Mixed document+chat source admission (phase 1). See
// docs/mixed-document-chat-summary-development-plan.md §1.2/§4.3.
//
// Mixed creation is admitted UNCONDITIONALLY (owner decision — no admission
// gate); validateDocumentWorkflowInput's mixed branch enforces the phase-1
// boundaries (personal-only, snapshot integrity, combined cap).

// MixedMaxTotalSources bounds the COMBINED document+chat source count of a
// mixed scope. Documents additionally respect MaxDocumentSummarySourceCount.
const MixedMaxTotalSources = 30

// MixedSourceCountExceededError returns the standard rejection for a mixed
// scope over MixedMaxTotalSources.
func MixedSourceCountExceededError() *BizError {
	return NewBizError(40001, fmt.Sprintf("混合来源总数不能超过%d个", MixedMaxTotalSources), 400)
}
