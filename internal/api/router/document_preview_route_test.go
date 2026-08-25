package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDocumentPreviewRouteIsMountedBehindAuth pins the route's auth wiring (P2-12).
// Every handler test calls StreamDocumentPreview with user_id pre-seeded into the
// gin context, so an edit that moved
//
//	v1.POST("/summaries/document/preview", agentSummaryH.StreamDocumentPreview)
//
// out of the authenticated v1 group would leave the whole handler suite green. This
// asserts the MOUNTED route rejects an unauthenticated request with 401, the same
// seam bot_routes_test uses for the bot group.
func TestDocumentPreviewRouteIsMountedBehindAuth(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, 0, nil, "", "", "", 0, 0, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/document/preview", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/v1/summaries/document/preview: got %d, want 401 (route must sit behind auth)", w.Code)
	}
}
