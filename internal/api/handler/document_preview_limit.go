package handler

// Per-user in-flight admission control for the ephemeral document preview.
//
// Why this exists, stated plainly because the alternative was shipping on an
// unresolved 「待人确认」:
//
// The inline path (documentPreviewReq.Content) deliberately removed the
// document-service fetch as an admission gate — that was the point of the change,
// since a caller that already holds the text should not need a round trip to have
// it summarized. But document_id is validated for shape only and, on the inline
// path, resolved against nothing. So any authenticated principal can POST an
// invented id plus up to 1 MiB of arbitrary text and get a full ~80k-rune
// completion, as fast as the gateway will serve them.
//
// The mitigations named in the PR description — manual click, docId+version
// caching, AbortController — are all CLIENT-side, and the endpoint is directly
// callable. "The LLM gateway probably has a per-user quota" is not a control this
// repository can point at, and this is the change that removed the one gate that
// did exist here.
//
// This is deliberately NOT a rate limiter:
//
//   - It caps CONCURRENCY, not rate. A user may run as many previews as they like
//     sequentially; what they cannot do is fan out. Concurrency is what converts one
//     account into an amplifier, and it is the dimension a single-process counter
//     can enforce honestly.
//   - It is per-process, and this service runs multiple replicas, so the effective
//     cluster limit is documentPreviewMaxInFlightPerUser × replicas. That is a real
//     limitation, not a rounding error, and it is why this is described as an
//     admission gate rather than a quota. A cluster-wide quota belongs in the
//     gateway or a shared store; this closes the unbounded case without pretending
//     to be that.
//   - It holds no lock during the LLM call. The slot is taken before generation and
//     released by defer, including on panic.
//
// The counter is keyed by user id, which StrictAuthMiddleware resolved from the
// Token — never from anything the caller supplied.

import "sync"

// documentPreviewMaxInFlightPerUser bounds concurrent previews for one user in one
// process. 2 rather than 1: a user legitimately reopening a document while the
// previous stream is still draining should not see a spurious rejection, and the
// front end cancels via AbortController rather than waiting for the server.
const documentPreviewMaxInFlightPerUser = 2

// documentPreviewLimiter tracks in-flight previews per user.
type documentPreviewLimiter struct {
	mu      sync.Mutex
	max     int
	inFlght map[string]int
}

func newDocumentPreviewLimiter(max int) *documentPreviewLimiter {
	return &documentPreviewLimiter{max: max, inFlght: make(map[string]int)}
}

// acquire reserves a slot for userID. The release func is safe to call exactly
// once; it is a no-op when ok is false, so callers can defer unconditionally.
func (l *documentPreviewLimiter) acquire(userID string) (release func(), ok bool) {
	if l == nil || l.max <= 0 {
		return func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlght[userID] >= l.max {
		return func() {}, false
	}
	l.inFlght[userID]++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if n := l.inFlght[userID] - 1; n > 0 {
				l.inFlght[userID] = n
			} else {
				// Delete rather than store 0: the map is keyed by user id and would
				// otherwise grow without bound for the lifetime of the process.
				delete(l.inFlght, userID)
			}
		})
	}, true
}

// documentPreviewLimiterInstance is process-global because the handler is
// constructed per router and the limit is a property of the process's capacity,
// not of any one handler instance.
var documentPreviewLimiterInstance = newDocumentPreviewLimiter(documentPreviewMaxInFlightPerUser)
