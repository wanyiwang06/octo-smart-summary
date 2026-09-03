//go:build cgo

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

// GET /api/v1/summaries/attention is the poll-friendly badge endpoint. These
// tests pin the two properties that make it safe to add:
//
//  1. it can never disagree with the list endpoint (both must go through the
//     one attentionCounts helper), and
//  2. its 5s cache is scoped per (user, space), bypassable with ?fresh=1, and
//     genuinely expires — a badge that gets stuck is worse than no badge.

type attentionCountsResponse struct {
	Code int `json:"code"`
	Data struct {
		AttentionCount         int `json:"attention_count"`
		UnreadCount            int `json:"unread_count"`
		PendingInvitationCount int `json:"pending_invitation_count"`
		PendingSubmissionCount int `json:"pending_submission_count"`
	} `json:"data"`
}

func parseAttentionCounts(t *testing.T, w *httptest.ResponseRecorder) attentionCountsResponse {
	t.Helper()
	var resp attentionCountsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal attention response: %v; body=%s", err, w.Body.String())
	}
	return resp
}

// setupAttentionRouter mounts the attention endpoint next to the list endpoint
// AND next to the /summaries/:id wildcard. gin's radix tree gives static
// segments priority over wildcards at the same position regardless of
// registration order, so both routes resolve correctly.
func setupAttentionRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries", h.ListSummaries)
	r.GET("/api/v1/summaries/attention", h.GetAttention)
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	return r
}

// seedAttentionFixture builds one task per attention signal for userID in
// spaceID: a pending invitation, a pending submission, and unread team content.
func seedAttentionFixture(t *testing.T, db *gorm.DB, prefix, spaceID, userID string) {
	t.Helper()
	now := timezone.Now()

	// 1. Pending invitation.
	invite := model.SummaryTask{
		TaskNo: prefix + "-INVITE", SpaceID: spaceID, CreatorID: "creator1",
		SummaryMode: model.ModeByPerson, Status: model.StatusProcessing,
	}
	if err := db.Create(&invite).Error; err != nil {
		t.Fatalf("create invite task: %v", err)
	}
	if err := db.Create(&model.SummaryParticipant{
		TaskID: invite.ID, UserID: userID, UserName: userID, Status: model.ParticipantPending,
	}).Error; err != nil {
		t.Fatalf("create pending participant: %v", err)
	}

	// 2. Pending submission (needs a second participant to be a team task).
	seedPendingSubmitTaskInSpace(t, db, prefix+"-SUBMIT", spaceID, userID, []string{"participant2"})

	// 3. Unread team result.
	unread := model.SummaryTask{
		TaskNo: prefix + "-UNREAD", SpaceID: spaceID, CreatorID: userID,
		SummaryMode: model.ModeByPerson, Status: model.StatusCompleted,
	}
	if err := db.Create(&unread).Error; err != nil {
		t.Fatalf("create unread task: %v", err)
	}
	result := model.SummaryResult{
		TaskID: unread.ID, Content: "team result", Version: 1,
		GeneratedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&result).Error; err != nil {
		t.Fatalf("create team result: %v", err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", unread.ID).
		Update("current_result_id", result.ID).Error; err != nil {
		t.Fatalf("attach current result: %v", err)
	}
}

// seedPendingSubmitTaskInSpace is the space-explicit form of
// seedPendingSubmitTask; the per-space isolation test needs a space other than
// the "space1" default.
func seedPendingSubmitTaskInSpace(t *testing.T, db *gorm.DB, taskNo, spaceID, userID string, extraParticipants []string) int64 {
	t.Helper()
	now := timezone.Now()

	task := model.SummaryTask{
		TaskNo: taskNo, SpaceID: spaceID, CreatorID: "creator1",
		SummaryMode: model.ModeByPerson, Status: model.StatusProcessing,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := db.Create(&model.SummaryParticipant{
		TaskID: task.ID, UserID: userID, UserName: userID, Status: model.ParticipantAccepted,
	}).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	for _, uid := range extraParticipants {
		if err := db.Create(&model.SummaryParticipant{
			TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantAccepted,
		}).Error; err != nil {
			t.Fatalf("create participant %s: %v", uid, err)
		}
	}
	if err := db.Create(&model.PersonalResult{
		TaskID: task.ID, UserID: userID, Content: "personal draft",
		WorkerStatus: model.PersonalStatusCompleted, SubmittedAt: nil,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}
	return task.ID
}

// The drift guard. The whole reason attentionCounts exists as a shared helper
// is that a second copy of this SQL would eventually disagree with the first,
// and the user would see a badge that contradicts the cards below it. Compare
// the two endpoints on the same fixture, on every breakdown field.
func TestGetAttention_MatchesListSummaries(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedAttentionFixture(t, db, "DRIFT", "space1", "participant1")

	r := setupAttentionRouter(NewTaskHandler(db, imDB, ""))

	lw := doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1")
	if lw.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", lw.Code, lw.Body.String())
	}
	list := parseAttentionList(t, lw)

	aw := doRequest(r, http.MethodGet, "/api/v1/summaries/attention?fresh=1", "participant1")
	if aw.Code != http.StatusOK {
		t.Fatalf("attention: expected 200, got %d: %s", aw.Code, aw.Body.String())
	}
	att := parseAttentionCounts(t, aw)

	// Sanity: the fixture must actually exercise all three signals, otherwise
	// an all-zero match would pass vacuously.
	if list.Data.AttentionCount != 3 || list.Data.UnreadCount != 1 ||
		list.Data.PendingInvitationCount != 1 || list.Data.PendingSubmissionCount != 1 {
		t.Fatalf("fixture must raise all three signals, list said %+v", list.Data)
	}

	if att.Data.AttentionCount != list.Data.AttentionCount ||
		att.Data.UnreadCount != list.Data.UnreadCount ||
		att.Data.PendingInvitationCount != list.Data.PendingInvitationCount ||
		att.Data.PendingSubmissionCount != list.Data.PendingSubmissionCount {
		t.Fatalf("attention endpoint drifted from the list endpoint: attention=%+v list=%+v",
			att.Data, list.Data)
	}
}

// Same fail-closed gate as ListSummaries: SummaryTask.SpaceID defaults to '',
// so an empty X-Space-Id would MATCH those rows and leak them across spaces.
func TestGetAttention_RejectsEmptySpaceID(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedAttentionFixture(t, db, "NOSPACE", "space1", "participant1")

	r := setupAttentionRouter(NewTaskHandler(db, imDB, ""))
	w := doRequestWithSpace(r, http.MethodGet, "/api/v1/summaries/attention", "participant1", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty X-Space-Id must be rejected with 404, got %d: %s", w.Code, w.Body.String())
	}
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error body: %v; body=%s", err, w.Body.String())
	}
	if resp.Code != 40008 {
		t.Fatalf("expected code 40008, got %d: %s", resp.Code, w.Body.String())
	}
}

// Within the TTL the endpoint answers from cache. This is the point of the
// cache: a tab polling once a second must not become one full attention query
// per second per tab.
func TestGetAttention_CacheServesStaleValueWithinTTL(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedAttentionFixture(t, db, "CACHE", "space1", "participant1")

	h := NewTaskHandler(db, imDB, "")
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	h.attentionCache.now = clock.Now
	r := setupAttentionRouter(h)

	first := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if first.Data.AttentionCount != 3 {
		t.Fatalf("expected 3 attention items, got %+v", first.Data)
	}

	// Remove every signal behind the cache's back. No invalidation hook exists
	// by design, so the cached answer must survive unchanged until it expires.
	if err := db.Where("1 = 1").Delete(&model.SummaryTask{}).Error; err != nil {
		t.Fatalf("delete tasks: %v", err)
	}

	clock.advance(attentionCacheTTL - time.Millisecond)
	second := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if second.Data != first.Data {
		t.Fatalf("within the TTL the cached value must be served: got %+v want %+v", second.Data, first.Data)
	}
}

// ?fresh=1 is the escape hatch that makes the no-invalidation design work: the
// read that follows a user action asks for a recomputation, so the badge reacts
// immediately instead of lagging by up to a TTL.
func TestGetAttention_FreshBypassesAndRefreshesCache(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedAttentionFixture(t, db, "FRESH", "space1", "participant1")

	h := NewTaskHandler(db, imDB, "")
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	h.attentionCache.now = clock.Now
	r := setupAttentionRouter(h)

	first := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if first.Data.AttentionCount != 3 {
		t.Fatalf("expected 3 attention items, got %+v", first.Data)
	}

	if err := db.Where("1 = 1").Delete(&model.SummaryTask{}).Error; err != nil {
		t.Fatalf("delete tasks: %v", err)
	}

	fresh := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention?fresh=1", "participant1"))
	if fresh.Data.AttentionCount != 0 || fresh.Data.UnreadCount != 0 ||
		fresh.Data.PendingInvitationCount != 0 || fresh.Data.PendingSubmissionCount != 0 {
		t.Fatalf("fresh=1 must bypass the cache: %+v", fresh.Data)
	}

	// It must also REFRESH the cache, not merely skip it — otherwise the next
	// plain poll would resurrect the stale value the user just watched clear.
	cached := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if cached.Data != fresh.Data {
		t.Fatalf("fresh=1 must overwrite the cached entry: cached=%+v fresh=%+v", cached.Data, fresh.Data)
	}
}

// Staleness must be bounded. If an entry could outlive its TTL the badge would
// be stuck for a user who never triggers a fresh read.
func TestGetAttention_CacheExpiresAfterTTL(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedAttentionFixture(t, db, "EXPIRE", "space1", "participant1")

	h := NewTaskHandler(db, imDB, "")
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	h.attentionCache.now = clock.Now
	r := setupAttentionRouter(h)

	first := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if first.Data.AttentionCount != 3 {
		t.Fatalf("expected 3 attention items, got %+v", first.Data)
	}

	if err := db.Where("1 = 1").Delete(&model.SummaryTask{}).Error; err != nil {
		t.Fatalf("delete tasks: %v", err)
	}

	clock.advance(attentionCacheTTL)
	after := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if after.Data.AttentionCount != 0 {
		t.Fatalf("the entry must expire at the TTL boundary, got %+v", after.Data)
	}
}

// The counts are caller-specific AND space-specific, so the key must be both.
// A key missing either dimension would show one user another user's badge.
func TestGetAttention_CacheIsKeyedPerUserAndSpace(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	// Only participant1 in space1 has anything pending.
	seedAttentionFixture(t, db, "KEYED", "space1", "participant1")
	// participant1 also belongs to space2, with nothing pending there.
	otherSpaceTask := model.SummaryTask{
		TaskNo: "KEYED-OTHER-SPACE", SpaceID: "space2", CreatorID: "participant1",
		SummaryMode: model.ModeByPerson, Status: model.StatusCompleted,
	}
	if err := db.Create(&otherSpaceTask).Error; err != nil {
		t.Fatalf("create space2 task: %v", err)
	}

	h := NewTaskHandler(db, imDB, "")
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	h.attentionCache.now = clock.Now
	r := setupAttentionRouter(h)

	// Warm the cache for (participant1, space1).
	warm := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if warm.Data.AttentionCount != 3 {
		t.Fatalf("expected 3 for participant1/space1, got %+v", warm.Data)
	}

	// Same space, different user: must not inherit participant1's number.
	other := parseAttentionCounts(t, doRequestWithSpace(r, http.MethodGet, "/api/v1/summaries/attention", "participant2", "space1"))
	if other.Data.AttentionCount != 0 {
		t.Fatalf("user A's cached badge leaked to user B: %+v", other.Data)
	}

	// Same user, different space: must not inherit space1's number either.
	otherSpace := parseAttentionCounts(t, doRequestWithSpace(r, http.MethodGet, "/api/v1/summaries/attention", "participant1", "space2"))
	if otherSpace.Data.AttentionCount != 0 {
		t.Fatalf("space X's cached badge leaked to space Y: %+v", otherSpace.Data)
	}

	// And the original entry is still intact (the other lookups were misses,
	// not overwrites).
	again := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if again.Data != warm.Data {
		t.Fatalf("the (participant1, space1) entry was clobbered: got %+v want %+v", again.Data, warm.Data)
	}
}

// ListSummaries must not READ the cache — its page rows and its badge are
// computed in the same request, and serving a cached badge next to freshly
// queried cards is exactly the inconsistency this endpoint set out to avoid.
func TestListSummaries_IgnoresAttentionCacheOnRead(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedAttentionFixture(t, db, "LISTFRESH", "space1", "participant1")

	h := NewTaskHandler(db, imDB, "")
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	h.attentionCache.now = clock.Now
	r := setupAttentionRouter(h)

	// Warm the cache with the three-signal state.
	warm := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if warm.Data.AttentionCount != 3 {
		t.Fatalf("expected 3 attention items, got %+v", warm.Data)
	}

	if err := db.Where("1 = 1").Delete(&model.SummaryTask{}).Error; err != nil {
		t.Fatalf("delete tasks: %v", err)
	}

	list := parseAttentionList(t, doRequest(r, http.MethodGet, "/api/v1/summaries", "participant1"))
	if list.Data.AttentionCount != 0 {
		t.Fatalf("the list must recompute its own badge, got %+v", list.Data)
	}

	// It DOES populate the cache, so the poll that follows a list load is cheap
	// and agrees with what the list just rendered.
	after := parseAttentionCounts(t, doRequest(r, http.MethodGet, "/api/v1/summaries/attention", "participant1"))
	if after.Data.AttentionCount != 0 {
		t.Fatalf("the list must refresh the cached entry it computed, got %+v", after.Data)
	}
}

// The cache is keyed by (user, space), which is unbounded over a long uptime.
// Pin the cap so a future change cannot turn it into a slow memory leak.
func TestAttentionCache_BoundedSize(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	cache := newAttentionCache()
	cache.now = clock.Now

	for i := 0; i < maxAttentionCacheEntries*2; i++ {
		cache.put(attentionCacheKey{userID: "u" + strconv.Itoa(i), spaceID: "space1"}, attentionCounts{AttentionCount: int64(i)})
	}
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()
	if size > maxAttentionCacheEntries {
		t.Fatalf("cache grew past its cap: %d > %d", size, maxAttentionCacheEntries)
	}
}

// fakeClock drives the TTL deterministically; the TTL tests must not depend on
// wall-clock sleeps.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }
