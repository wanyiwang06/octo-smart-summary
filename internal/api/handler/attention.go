package handler

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// attentionJoinsSQL is the caller-specific join block shared by the ListSummaries
// page query and the attention-count query. It is a single constant on purpose:
// the count and the per-card dots must be derived from exactly the same rows, or
// the sidebar badge and the list disagree and neither can be trusted.
//
// It contributes FOUR positional placeholders, all userID, in textual order.
const attentionJoinsSQL = `
 LEFT JOIN summary_user_read sur ON sur.task_id = sub.id AND sur.user_id = ?
 LEFT JOIN summary_result cr ON cr.id = sub.current_result_id AND cr.task_id = sub.id
 LEFT JOIN summary_personal_result pr ON pr.task_id = sub.id AND pr.user_id = ?
 LEFT JOIN summary_personal_result_version pv ON pv.id = pr.current_version_id AND pv.task_id = sub.id AND pv.user_id = ?
 LEFT JOIN summary_participant me ON me.task_id = sub.id AND me.user_id = ?`

// attentionCounts is the space-level, caller-specific attention breakdown that
// drives the navigation badge.
type attentionCounts struct {
	AttentionCount         int64 `gorm:"column:attention_count" json:"attention_count"`
	UnreadCount            int64 `gorm:"column:unread_count" json:"unread_count"`
	PendingInvitationCount int64 `gorm:"column:pending_invitation_count" json:"pending_invitation_count"`
	PendingSubmissionCount int64 `gorm:"column:pending_submission_count" json:"pending_submission_count"`
}

// attentionCounts computes the attention breakdown for one (space, user) pair.
//
// This is the ONLY place the attention statistics SQL exists. Both the list
// endpoint (which returns the counts alongside its page) and the dedicated
// polling endpoint call it, so the badge shown by a poll can never drift from
// the badge shown by a list refresh — a second hand-maintained copy of this
// query would drift the first time either signal's definition changed.
//
// Counts deliberately ignore the list filters: the navigation badge represents
// all cards that require this user's attention in the space.
func (h *TaskHandler) attentionCounts(spaceID, userID string) (attentionCounts, error) {
	var counts attentionCounts

	scheduleVisibilityExpr := h.schedulePendingInvitationExpr("summary_task")
	schedulePendingExpr := h.schedulePendingInvitationExpr("sub")

	countInner := "SELECT *, ROW_NUMBER() OVER (PARTITION BY (CASE WHEN schedule_id IS NULL THEN CONCAT('t', id) ELSE CONCAT('s', schedule_id) END) ORDER BY id DESC) AS rn FROM summary_task WHERE space_id = ? AND deleted_at IS NULL AND (creator_id = ? OR id IN (SELECT task_id FROM summary_participant WHERE user_id = ?) OR " + scheduleVisibilityExpr + ")"
	//
	// ⚠️ The placeholders below are positional and hand-assembled; countArgs must
	// be appended in exactly the textual order the `?`s appear:
	//   1. has_invite      -> ParticipantPending [+ userID when schedulePendingExpr is live (MySQL only)]
	//   2. has_submit      -> PersonalStatusCompleted
	//   3. countInner      -> spaceID, userID, userID [+ userID when scheduleVisibilityExpr is live]
	//   4. attentionJoins  -> userID x4
	attentionCountSQL := "SELECT COALESCE(SUM(has_invite),0) pending_invitation_count, COALESCE(SUM(unread),0) unread_count, COALESCE(SUM(has_submit),0) pending_submission_count, COALESCE(SUM(CASE WHEN has_invite=1 OR unread=1 OR has_submit=1 THEN 1 ELSE 0 END),0) attention_count FROM (SELECT CASE WHEN me.status=? OR " + schedulePendingExpr + " THEN 1 ELSE 0 END has_invite, CASE WHEN pr.worker_status=? AND pr.submitted_at IS NULL AND (SELECT COUNT(*) FROM summary_participant wait_sp WHERE wait_sp.task_id=sub.id) > 1" + pendingSubmitStatusGuard("sub") + " THEN 1 ELSE 0 END has_submit, CASE WHEN (sub.current_result_id IS NOT NULL AND (sur.last_read_team_result_id IS NULL OR sur.last_read_team_result_id<>sub.current_result_id)) OR (pr.current_version_id IS NOT NULL AND (sur.last_read_personal_version_id IS NULL OR sur.last_read_personal_version_id<>pr.current_version_id)) THEN 1 ELSE 0 END unread FROM (" + countInner + ") sub" + attentionJoinsSQL + " WHERE sub.rn=1) attention"

	countArgs := []interface{}{model.ParticipantPending}
	if h.db.Dialector.Name() == "mysql" {
		countArgs = append(countArgs, userID)
	}
	countArgs = append(countArgs, model.PersonalStatusCompleted)
	countArgs = append(countArgs, spaceID, userID, userID)
	if h.db.Dialector.Name() == "mysql" {
		countArgs = append(countArgs, userID)
	}
	countArgs = append(countArgs, userID, userID, userID, userID)

	if err := h.db.Raw(attentionCountSQL, countArgs...).Scan(&counts).Error; err != nil {
		return attentionCounts{}, err
	}
	return counts, nil
}

// ---------------------------------------------------------------------------
// Polling cache
// ---------------------------------------------------------------------------

// attentionCacheTTL bounds how stale a polled badge may be. Five seconds is
// short enough that a user who never triggers an explicit refresh still
// converges quickly, and long enough to collapse the fan-out of a browser tab
// polling every second (plus every duplicated tab of the same user).
const attentionCacheTTL = 5 * time.Second

// maxAttentionCacheEntries caps memory for the whole process. The key space is
// (user, space), which grows with the active population and is unbounded over a
// long uptime, so the map MUST NOT be allowed to grow forever. Overflow is
// handled by dropping entries, never by refusing to answer: a dropped entry only
// costs one extra query.
const maxAttentionCacheEntries = 4096

type attentionCacheKey struct {
	userID  string
	spaceID string
}

type attentionCacheEntry struct {
	counts    attentionCounts
	expiresAt time.Time
}

// attentionCache is a plain in-process TTL cache.
//
// Why in-process and not a shared cache: the API deployment runs a single
// replica by default, and the value being cached is a cheap, self-healing
// derived number with a 5s lifetime. A shared cache would add an infrastructure
// dependency and a new failure mode to protect a query the database can already
// answer; if the process count ever grows, each replica simply keeps its own
// copy, which is still correct because every entry expires on its own.
//
// Why there is NO write-path invalidation:
//
//	Correctness here comes from bounded staleness plus an explicit refresh
//	(`?fresh=1`) on the reads that follow a user action, NOT from remembering to
//	poke this cache from every code path that can change an attention signal.
//	Invalidation hooks would have to be added to read-marking, invite accept and
//	decline, member add and remove, submit, personal edit, regenerate, delete,
//	cancel, the scheduler, and the worker — and every future path that touches
//	those tables. A single missed hook produces a badge that is stuck FOREVER
//	(until an unrelated request happens to evict the entry): the user clears
//	their tasks and the red dot never goes away, with nothing they can do about
//	it. That failure is silent, user-visible, and unbounded in time. Five seconds
//	of staleness is bounded, self-correcting, and invisible in practice. Trading
//	a bounded, self-healing error for an unbounded, silent one is never worth it,
//	so this cache intentionally has no invalidation surface at all.
type attentionCache struct {
	mu      sync.Mutex
	entries map[attentionCacheKey]attentionCacheEntry
	ttl     time.Duration
	// now is injectable so TTL behaviour can be tested deterministically
	// instead of with real sleeps.
	now func() time.Time
}

func newAttentionCache() *attentionCache {
	return &attentionCache{
		entries: make(map[attentionCacheKey]attentionCacheEntry),
		ttl:     attentionCacheTTL,
		now:     time.Now,
	}
}

// get returns the cached counts when the entry exists and has not expired.
func (c *attentionCache) get(key attentionCacheKey) (attentionCounts, bool) {
	if c == nil {
		return attentionCounts{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, found := c.entries[key]
	if !found {
		return attentionCounts{}, false
	}
	if !c.now().Before(entry.expiresAt) {
		// Expired entries are dropped on sight so a key that stops being
		// polled cannot pin memory until the next overflow sweep.
		delete(c.entries, key)
		return attentionCounts{}, false
	}
	return entry.counts, true
}

// put stores freshly computed counts, evicting as needed to stay under the cap.
func (c *attentionCache) put(key attentionCacheKey, counts attentionCounts) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= maxAttentionCacheEntries {
		for k, e := range c.entries {
			if !now.Before(e.expiresAt) {
				delete(c.entries, k)
			}
		}
		// Still full: every entry is live, so there is no "least valuable"
		// one to pick without tracking extra metadata. Reset wholesale. The
		// only cost is that the next poll of each dropped key re-queries,
		// and the cap is high enough that this is a pathological case.
		if len(c.entries) >= maxAttentionCacheEntries {
			c.entries = make(map[attentionCacheKey]attentionCacheEntry)
		}
	}
	c.entries[key] = attentionCacheEntry{counts: counts, expiresAt: now.Add(c.ttl)}
}

// GetAttention handles GET /api/v1/summaries/attention.
//
// It is the poll-friendly sibling of ListSummaries: same numbers, none of the
// page rendering (participants, sources, per-task result lookups). Clients that
// only need the badge should poll this instead of re-listing.
//
// `?fresh=1` bypasses and refreshes the cache. Callers must use it on the read
// that immediately follows a user action which can change a signal (marking a
// summary read, accepting or declining an invitation, submitting) so the badge
// reacts instantly; plain polls take the cached path.
func (h *TaskHandler) GetAttention(c *gin.Context) {
	spaceID := middleware.GetSpaceID(c)
	// Same fail-closed hard gate as ListSummaries: SummaryTask.SpaceID is
	// `not null default ''`, so querying `space_id=''` would MATCH rows with an
	// empty space and leak them across spaces. Reject before any query.
	if spaceID == "" {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	userID := middleware.GetUserID(c)

	// The key is (user, space) because the counts are caller-specific AND
	// space-specific: the same user has a different badge per space, and two
	// users in one space must never see each other's numbers.
	key := attentionCacheKey{userID: userID, spaceID: spaceID}
	fresh := c.Query("fresh") == "1"
	if !fresh {
		if counts, hit := h.attentionCacheGet(key); hit {
			writeAttentionCounts(c, counts)
			return
		}
	}

	counts, err := h.attentionCounts(spaceID, userID)
	if err != nil {
		log.Printf("attention count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "failed to load attention counts"})
		return
	}
	h.attentionCachePut(key, counts)
	writeAttentionCounts(c, counts)
}

func writeAttentionCounts(c *gin.Context, counts attentionCounts) {
	ok(c, gin.H{
		"attention_count":          counts.AttentionCount,
		"unread_count":             counts.UnreadCount,
		"pending_invitation_count": counts.PendingInvitationCount,
		"pending_submission_count": counts.PendingSubmissionCount,
	})
}

// attentionCacheGet / attentionCachePut are the handler-level accessors. They
// tolerate a nil cache so a TaskHandler built by any other construction path
// still works (uncached, always fresh) instead of panicking.
func (h *TaskHandler) attentionCacheGet(key attentionCacheKey) (attentionCounts, bool) {
	return h.attentionCache.get(key)
}

func (h *TaskHandler) attentionCachePut(key attentionCacheKey, counts attentionCounts) {
	h.attentionCache.put(key, counts)
}
