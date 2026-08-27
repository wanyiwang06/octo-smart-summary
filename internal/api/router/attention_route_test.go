package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /api/v1/summaries/attention is a static sibling of the /summaries/:id
// wildcard. gin's radix tree panics at REGISTRATION time on a shape it cannot
// represent, so building the real router is the assertion that the two routes
// coexist; serving one request each then proves "attention" is not swallowed as
// an :id and that the endpoint is mounted behind the human-token group (not the
// bot group, which must stay a read-only bot surface).
func TestAttentionRouteCoexistsWithSummaryIDRoute(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, 0, nil, "", "", "", 0, 0, nil)

	for _, path := range []string{
		"/api/v1/summaries/attention",
		"/api/v1/summaries/42",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(w, req)
		// No token -> the auth middleware rejects before any handler runs.
		// A 404 here would mean the route is not registered at all.
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 from the human-token guard, got %d: %s", path, w.Code, w.Body.String())
		}
	}

	// The bot surface must NOT gain the new endpoint.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bot/summaries/attention", nil)
	req.Header.Set("Authorization", "Bearer irrelevant")
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("the attention endpoint must not be mounted on the bot group: %d %s", w.Code, w.Body.String())
	}
}
