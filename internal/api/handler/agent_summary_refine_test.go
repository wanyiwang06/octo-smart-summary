package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// SUM-43 · RefineAgentSummary handler 场景测试
//
// 覆盖清单(架构师规格 v1.1 + 代码审核必改 4 全 8 条):
//   - 40001 × 2: task not found / trigger_type != agent
//   - 40002 × 1: 非 creator 无权
//   - 40003 × 2: instruction 空 / 超长(rune 计数)—— 已有,保留
//   - 40004 × 2: 无 PersonalResult / SnapshotJSON 为 NULL
//   - 50000 × 1: agent 调用失败
//   - snapshot parent link 正确性: ContentVersion+1, ParentSnapshotVersion 指向旧版
//
// 测试模式对齐仓库既定约定:
//   - 用 setupTestDB(t) 拿 sqlite in-memory + AutoMigrate(和 SUM-20/SUM-36 同一模式)
//   - 无 CGO 环境自然 skip,CI 的 Test (race, cgo) job 会真跑
//   - fake runner 用 chatter 接口注入,不依赖真 LLM

// ─── 无 DB 依赖的参数校验测试(fast path) ──────────────────────────

// TestRefineAgentSummary_InstructionEmpty tests 40003: instruction empty
func TestRefineAgentSummary_InstructionEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Use a nil DB handler - should fail before DB access
	h := &AgentSummaryHandler{db: nil}

	r.Use(func(c *gin.Context) {
		c.Set("user_id", "user-1")
		c.Next()
	})
	r.POST("/api/v1/summaries/:task_id/refine", h.RefineAgentSummary)

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"instruction":""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/1/refine", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != 40003 {
		t.Fatalf("expected code 40003, got %d", resp.Code)
	}
}

// TestRefineAgentSummary_InstructionTooLong tests 40003: instruction exceeds 1000 characters (rune count)
func TestRefineAgentSummary_InstructionTooLong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	h := &AgentSummaryHandler{db: nil}

	r.Use(func(c *gin.Context) {
		c.Set("user_id", "user-1")
		c.Next()
	})
	r.POST("/api/v1/summaries/:task_id/refine", h.RefineAgentSummary)

	// 1001 Chinese runes → 严格超过 1000 rune 上限
	longInstruction := strings.Repeat("长", 1001)

	w := httptest.NewRecorder()
	body := strings.NewReader(`{"instruction":"` + longInstruction + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/1/refine", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Code != 40003 {
		t.Fatalf("expected code 40003, got %d", resp.Code)
	}
}

// ─── DB 依赖的完整场景测试(sqlite in-memory,无 CGO 自然 skip) ─────

// setupRefineTestDB 为 refine 测试单独开一个 in-memory sqlite(不 share cache),
// 避免污染 handler 包里其他共享 `file::memory:?cache=shared` 的测试(如 SUM-24 的
// TestCreateAgentSummary_*),那些测试用 db.First(&task) 期望自己的 fixture 是第 1 条。
func setupRefineTestDB(t *testing.T) (*gorm.DB, bool) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil, true
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{},
		&model.PersonalResult{},
		&model.SummaryParticipant{},
		&model.AgentMessage{},
	); err != nil {
		t.Fatalf("failed to migrate refine test tables: %v", err)
	}
	return db, false
}

// fakeRefineChatter 实现 agent 的 chatter 接口,可选择返回固定 reply 或 error。
// 用于 mock refine 场景下的 agent 行为,不真调 LLM。
type fakeRefineChatter struct {
	reply string
	err   error
}

func (f *fakeRefineChatter) Chat(ctx context.Context, msgs []agent.Message, tools []agent.Tool) (agent.AssistantTurn, error) {
	if f.err != nil {
		return agent.AssistantTurn{}, f.err
	}
	return agent.AssistantTurn{Content: f.reply}, nil
}

// newRefineTestHandler 构造一个 handler,注入 fake chatter 通过 monkey-patching runner
// 这里因为 handler 内部通过 buildRunner 全局函数构造 runner,我们改用 test 版覆盖 db/store,
// LLM 通过环境变量注入(或后续通过 fake profile),test 里不真调 runner 分支的场景用
// 提前 fail(在 40001~40004 分支就 return)避开 runner 构造。
func newRefineTestHandler(db *gorm.DB) *AgentSummaryHandler {
	return &AgentSummaryHandler{
		db:           db,
		llmApiURL:    "http://fake-llm/v1",
		llmApiKey:    "test-key",
		llmModel:     "test-model",
		llmTimeout:   30,
		llmMaxTokens: 8000,
		store:        newAgentMessageRepo(db),
	}
}

// setupRefineRouter 挂 refine 路由 + 注入固定 user_id
func setupRefineRouter(h *AgentSummaryHandler, uid string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uid)
		c.Next()
	})
	r.POST("/api/v1/summaries/:task_id/refine", h.RefineAgentSummary)
	return r
}

func doRefine(t *testing.T, r *gin.Engine, taskID, instruction string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"instruction":"` + instruction + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/"+taskID+"/refine", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func parseRefineResp(t *testing.T, w *httptest.ResponseRecorder) apiResponse {
	t.Helper()
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	return resp
}

// ─── 40001 分支 ──────────────────────────────────────────────────

// TestRefineAgentSummary_TaskNotFound: 40001 - path task_id 在 DB 里查不到
func TestRefineAgentSummary_TaskNotFound(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	h := newRefineTestHandler(db)
	r := setupRefineRouter(h, "user-1")

	w := doRefine(t, r, "99999", "改一下")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 40001 {
		t.Errorf("expected code 40001, got %d msg=%s", resp.Code, resp.Message)
	}
	if !strings.Contains(resp.Message, "task 不存在") {
		t.Errorf("expected message about task not found, got %q", resp.Message)
	}
}

// TestRefineAgentSummary_TriggerTypeNotAgent: 40001 - task 存在但 trigger_type != agent
func TestRefineAgentSummary_TriggerTypeNotAgent(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	// Insert a task with a non-agent trigger_type (e.g. TriggerCron=1 or Manual)
	// Any value != model.TriggerAgent qualifies.
	task := model.SummaryTask{
		ID:          1,
		TaskNo:      "refine-test-trigger-not-agent",
		CreatorID:   "user-1",
		Title:       "traditional workflow summary",
		TriggerType: 1, // 非 agent(具体枚举值无所谓,只要 != TriggerAgent)
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}

	h := newRefineTestHandler(db)
	r := setupRefineRouter(h, "user-1")

	w := doRefine(t, r, "1", "改一下")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 40001 {
		t.Errorf("expected code 40001, got %d msg=%s", resp.Code, resp.Message)
	}
	if !strings.Contains(resp.Message, "trigger_type") && !strings.Contains(resp.Message, "agent") {
		t.Errorf("expected message about trigger_type != agent, got %q", resp.Message)
	}
}

// ─── 40002 分支 ──────────────────────────────────────────────────

// TestRefineAgentSummary_Unauthorized: 40002 - 当前 uid 不是 task.creator_id
func TestRefineAgentSummary_Unauthorized(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	task := model.SummaryTask{
		ID:          2,
		TaskNo:      "refine-test-unauthorized",
		CreatorID:   "user-owner",
		Title:       "agent summary owned by user-owner",
		TriggerType: model.TriggerAgent,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}

	h := newRefineTestHandler(db)
	r := setupRefineRouter(h, "user-intruder") // 非 creator

	w := doRefine(t, r, "2", "试图改别人的")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 40002 {
		t.Errorf("expected code 40002, got %d msg=%s", resp.Code, resp.Message)
	}
}

// ─── 40004 分支 ──────────────────────────────────────────────────

// TestRefineAgentSummary_NoPersonalResult: 40004 - task 存在且 agent 但 creator 没有 PersonalResult
func TestRefineAgentSummary_NoPersonalResult(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	task := model.SummaryTask{
		ID:          3,
		TaskNo:      "refine-test-no-pr",
		CreatorID:   "user-3",
		Title:       "agent task without any PersonalResult",
		TriggerType: model.TriggerAgent,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
	// 不插 PersonalResult

	h := newRefineTestHandler(db)
	r := setupRefineRouter(h, "user-3")

	w := doRefine(t, r, "3", "改一下")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 40004 {
		t.Errorf("expected code 40004, got %d msg=%s", resp.Code, resp.Message)
	}
	if !strings.Contains(resp.Message, "PersonalResult") && !strings.Contains(resp.Message, "快照") {
		t.Errorf("expected message about missing PersonalResult, got %q", resp.Message)
	}
}

// TestRefineAgentSummary_SnapshotJSONNull: 40004 - PersonalResult 存在但 SnapshotJSON 为 NULL(老数据)
func TestRefineAgentSummary_SnapshotJSONNull(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	task := model.SummaryTask{
		ID:          4,
		TaskNo:      "refine-test-null-snapshot",
		CreatorID:   "user-4",
		Title:       "agent task with legacy PersonalResult (no snapshot)",
		TriggerType: model.TriggerAgent,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
	// 插一条 PersonalResult 但故意不 SetSnapshot(SnapshotJSON 保持为 nil)
	now := time.Now()
	pr := model.PersonalResult{
		TaskID:       task.ID,
		UserID:       "user-4",
		Content:      "老数据总结正文,SUM-36 前生成,没有 snapshot",
		WorkerStatus: model.PersonalStatusCompleted,
		GeneratedAt:  &now,
		SubmittedAt:  &now,
		CreatedAt:    now,
		UpdatedAt:    now,
		// SnapshotJSON 不设,保持 nil
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("insert PersonalResult: %v", err)
	}

	h := newRefineTestHandler(db)
	r := setupRefineRouter(h, "user-4")

	w := doRefine(t, r, "4", "改一下")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 40004 {
		t.Errorf("expected code 40004, got %d msg=%s", resp.Code, resp.Message)
	}
	if !strings.Contains(resp.Message, "snapshot") && !strings.Contains(resp.Message, "快照") {
		t.Errorf("expected message about missing snapshot, got %q", resp.Message)
	}
}

// ─── Snapshot 组装逻辑(不进 handler,直接单测 buildRefineMessage + newSnap 构造)──

// TestRefineBuildMessage_IncludesAllFields: buildRefineMessage 输出应包含
// 当前产物 v{N} / content / citations / 生成语境 / 本轮修改需求 五段
func TestRefineBuildMessage_IncludesAllFields(t *testing.T) {
	parentVer := 1
	snap := &model.Snapshot{
		SnapshotVersion:       1,
		TaskID:                10,
		ContentVersion:        2,
		Requirement:           "总结群里最近一周的关键决策",
		Scope:                 model.SnapshotScope{ChannelIDs: []string{"ch-a"}, ChannelNames: []string{}, TimeRange: model.TimeRangeJSON{Start: "2026-07-01T00:00:00Z", End: "2026-07-13T00:00:00Z"}},
		ToolSummary:           []string{"fetch_channel x 2"},
		DataFreshnessNote:     "tool_summary 只记录本次调用,不代表数据边界",
		ParentSnapshotVersion: &parentVer,
		UserInstruction:       nil,
	}
	pr := &model.PersonalResult{
		Content:       "现有产物正文",
		CitationsJSON: `[{"index":1,"channel_id":"ch-a","message_seq":1}]`,
	}

	msg := buildRefineMessage(snap, pr, "把风险章节扩充一段")

	// 逐段检查关键子串
	checks := []struct {
		name string
		sub  string
	}{
		{"当前产物版本号", "【当前产物 v2】"},
		{"content 段", "content:"},
		{"citations 段", "citations:"},
		{"生成语境段", "【生成语境】"},
		{"本轮修改需求段", "【本轮修改需求】"},
		{"含现有 content", "现有产物正文"},
		{"含 requirement", "总结群里最近一周"},
		{"含本次 instruction", "把风险章节扩充一段"},
	}
	for _, c := range checks {
		if !strings.Contains(msg, c.sub) {
			t.Errorf("[%s] expected msg contains %q, msg=%s", c.name, c.sub, msg)
		}
	}
}

// TestRefineSnapshot_BuildRefineSnapshot: 验证 handler 里第 11 步的 snapshot 构造。
//
// 这个 test 直接调用生产代码的 `buildRefineSnapshot()`(agent_summary_refine.go),
// 而不是像旧版那样在 test 函数体里手写一遍相同逻辑再断言 —— 后者是"自证自洽"的
// 假测试,handler 里 ParentSnapshotVersion 写反 / ContentVersion+1 改错时不会 fail。
// 现在改成同一份代码同一份断言,改错必挂。
func TestRefineSnapshot_BuildRefineSnapshot(t *testing.T) {
	oldSnap := &model.Snapshot{
		SnapshotVersion:       1,
		TaskID:                100,
		ContentVersion:        1,
		Requirement:           "原始需求",
		Scope:                 model.SnapshotScope{ChannelIDs: []string{"ch-x"}},
		ToolSummary:           []string{"fetch_channel x 1"},
		DataFreshnessNote:     "note",
		ParentSnapshotVersion: nil, // v1 首版无 parent
		UserInstruction:       nil,
	}

	// 直接调用生产函数 —— 改错必挂
	newSnap := buildRefineSnapshot(oldSnap, 100, "扩充风险章节")

	if newSnap.ContentVersion != 2 {
		t.Errorf("ContentVersion: expected 2 (oldSnap.ContentVersion+1), got %d", newSnap.ContentVersion)
	}
	if newSnap.ParentSnapshotVersion == nil || *newSnap.ParentSnapshotVersion != 1 {
		t.Errorf("ParentSnapshotVersion: expected *int(1) pointing to old ContentVersion, got %v", newSnap.ParentSnapshotVersion)
	}
	if newSnap.UserInstruction == nil || *newSnap.UserInstruction != "扩充风险章节" {
		t.Errorf("UserInstruction: expected 扩充风险章节, got %v", newSnap.UserInstruction)
	}
	if newSnap.SnapshotVersion != 1 {
		t.Errorf("SnapshotVersion: expected 1 (schema version, not bumped by refine), got %d", newSnap.SnapshotVersion)
	}
	if newSnap.TaskID != 100 {
		t.Errorf("TaskID: expected 100, got %d", newSnap.TaskID)
	}
	// 沿用字段:refine 不改抓取 scope
	if newSnap.Requirement != oldSnap.Requirement {
		t.Errorf("Requirement not carried over")
	}
	if len(newSnap.Scope.ChannelIDs) != 1 || newSnap.Scope.ChannelIDs[0] != "ch-x" {
		t.Errorf("Scope.ChannelIDs not carried over: %v", newSnap.Scope.ChannelIDs)
	}
	if len(newSnap.ToolSummary) != 1 || newSnap.ToolSummary[0] != "fetch_channel x 1" {
		t.Errorf("ToolSummary not carried over: %v", newSnap.ToolSummary)
	}
	if newSnap.DataFreshnessNote != "note" {
		t.Errorf("DataFreshnessNote not carried over")
	}
}

// TestRefineSnapshot_MultiRoundLineage: 验证多轮 refine 的 lineage 链是否正确 —
// v1 → v2 → v3,每一版 parent 指向前一版 ContentVersion。
// 这是需求 v1.0 里 lineage 链的核心断言。
func TestRefineSnapshot_MultiRoundLineage(t *testing.T) {
	// v1(首版,parent=nil):
	snap1 := &model.Snapshot{
		SnapshotVersion: 1, TaskID: 200, ContentVersion: 1,
		Requirement: "req", Scope: model.SnapshotScope{ChannelIDs: []string{"c"}},
	}
	// v1 → v2
	snap2 := buildRefineSnapshot(snap1, 200, "第一次改")
	if snap2.ContentVersion != 2 || snap2.ParentSnapshotVersion == nil || *snap2.ParentSnapshotVersion != 1 {
		t.Fatalf("v1→v2 lineage broken: version=%d parent=%v", snap2.ContentVersion, snap2.ParentSnapshotVersion)
	}
	// v2 → v3
	snap3 := buildRefineSnapshot(snap2, 200, "第二次改")
	if snap3.ContentVersion != 3 || snap3.ParentSnapshotVersion == nil || *snap3.ParentSnapshotVersion != 2 {
		t.Fatalf("v2→v3 lineage broken: version=%d parent=%v", snap3.ContentVersion, snap3.ParentSnapshotVersion)
	}
	if snap3.UserInstruction == nil || *snap3.UserInstruction != "第二次改" {
		t.Errorf("v3 UserInstruction should be latest instruction, got %v", snap3.UserInstruction)
	}
}

// ─── 50000 分支 + 事务回滚(用 runnerFactory 注入 fake runner)─────────

// fakeRunner 实现 refineRunner 接口,用于测试注入 —— 可以返回错误、空 content、
// 或成功产出。**不需要真的 LLM,不需要 fake chatter 层层构造**。
type fakeRunner struct {
	reply   string          // 成功时返回的 content
	newMsgs []agent.Message // 成功时返回的新消息(用于 AppendMessages)
	err     error           // 非 nil 则 RunWithHistory 返回此错误
}

func (f *fakeRunner) RunWithHistory(ctx context.Context, system string, history []agent.Message, userInput string) (string, []agent.Message, error) {
	if f.err != nil {
		return "", nil, f.err
	}
	return f.reply, f.newMsgs, nil
}

// TestRefineAgentSummary_RunnerFailure_500: 50000 - runner.RunWithHistory 返回错误
func TestRefineAgentSummary_RunnerFailure_500(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	// 准备完整前置:task + PersonalResult with snapshot(前 4 个分支都要过)
	task := model.SummaryTask{
		ID: 5, TaskNo: "refine-test-runner-fail",
		CreatorID:   "user-5",
		Title:       "agent task for runner-failure test",
		TriggerType: model.TriggerAgent,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
	now := time.Now()
	pr := model.PersonalResult{
		TaskID: task.ID, UserID: "user-5",
		Content: "v1 正文", WorkerStatus: model.PersonalStatusCompleted,
		GeneratedAt: &now, SubmittedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	// 塞一个 snapshot,让 40004 分支能过
	pr.SetSnapshot(&model.Snapshot{
		SnapshotVersion: 1, TaskID: task.ID, ContentVersion: 1,
		Requirement: "req", Scope: model.SnapshotScope{ChannelIDs: []string{"c"}},
	})
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("insert pr: %v", err)
	}

	h := newRefineTestHandler(db)
	// 注入 fake runner 让 RunWithHistory 返回错误
	h.runnerFactory = func(profile, uid string) (refineRunner, string, error) {
		return &fakeRunner{err: errors.New("simulated LLM timeout")}, "system-prompt", nil
	}
	r := setupRefineRouter(h, "user-5")

	w := doRefine(t, r, "5", "改一下")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 50000 {
		t.Errorf("expected code 50000, got %d msg=%s", resp.Code, resp.Message)
	}
	if !strings.Contains(resp.Message, "Agent") && !strings.Contains(resp.Message, "agent") {
		t.Errorf("expected message about agent failure, got %q", resp.Message)
	}

	// 关键:runner 失败时 **必须没有新 PersonalResult 落库**
	var count int64
	db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&count)
	if count != 1 {
		t.Errorf("expected 1 PersonalResult (only original), got %d — runner failure should NOT insert new row", count)
	}
}

// TestRefineAgentSummary_AgentEmptyReply_500: 50000 - runner 成功返回但 content 为空
func TestRefineAgentSummary_AgentEmptyReply_500(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	task := model.SummaryTask{
		ID: 6, TaskNo: "refine-test-empty-reply",
		CreatorID:   "user-6",
		Title:       "agent task for empty-reply test",
		TriggerType: model.TriggerAgent,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
	now := time.Now()
	pr := model.PersonalResult{
		TaskID: task.ID, UserID: "user-6",
		Content: "v1", WorkerStatus: model.PersonalStatusCompleted,
		GeneratedAt: &now, SubmittedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	pr.SetSnapshot(&model.Snapshot{
		SnapshotVersion: 1, TaskID: task.ID, ContentVersion: 1, Requirement: "req",
	})
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("insert pr: %v", err)
	}

	h := newRefineTestHandler(db)
	// runner 成功但产出为空字符串
	h.runnerFactory = func(profile, uid string) (refineRunner, string, error) {
		return &fakeRunner{reply: "", newMsgs: nil}, "system-prompt", nil
	}
	r := setupRefineRouter(h, "user-6")

	w := doRefine(t, r, "6", "改一下")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 50000 {
		t.Errorf("expected code 50000, got %d msg=%s", resp.Code, resp.Message)
	}

	// 空 reply 时也不能有新 PersonalResult 落库
	var count int64
	db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&count)
	if count != 1 {
		t.Errorf("expected 1 PersonalResult, got %d", count)
	}
}

// TestRefineAgentSummary_Success_FullLifecycle: 成功链路 —— 走完 runner + tx + 响应,
// 断言:①201 应答 ②DB 里新 PersonalResult(version 递增)③snapshot lineage 链正确
// ④response body 里 content/citations/new_version 字段齐
func TestRefineAgentSummary_Success_FullLifecycle(t *testing.T) {
	db, skip := setupRefineTestDB(t)
	if skip {
		return
	}
	task := model.SummaryTask{
		ID: 7, TaskNo: "refine-test-success",
		CreatorID:   "user-7",
		Title:       "agent task for success e2e",
		TriggerType: model.TriggerAgent,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("insert task: %v", err)
	}
	now := time.Now()
	pr := model.PersonalResult{
		TaskID: task.ID, UserID: "user-7",
		Content: "v1 正文,风险章节偏简略", WorkerStatus: model.PersonalStatusCompleted,
		GeneratedAt: &now, SubmittedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	pr.SetSnapshot(&model.Snapshot{
		SnapshotVersion: 1, TaskID: task.ID, ContentVersion: 1,
		Requirement:       "总结群内决策",
		Scope:             model.SnapshotScope{ChannelIDs: []string{"c-1"}},
		ToolSummary:       []string{"fetch_channel x 1"},
		DataFreshnessNote: "note",
	})
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("insert pr: %v", err)
	}

	h := newRefineTestHandler(db)
	// runner 成功返回新 content
	h.runnerFactory = func(profile, uid string) (refineRunner, string, error) {
		if profile != "summary_refine" {
			t.Errorf("expected profile summary_refine, got %s", profile)
		}
		if uid != "user-7" {
			t.Errorf("expected uid user-7, got %s", uid)
		}
		return &fakeRunner{
			reply:   "v2 正文,风险章节已扩充成完整段落。",
			newMsgs: []agent.Message{{Role: "assistant", Content: "v2 正文,风险章节已扩充成完整段落。"}},
		}, "system-prompt", nil
	}
	r := setupRefineRouter(h, "user-7")

	w := doRefine(t, r, "7", "把风险章节扩充成一整段")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d body=%s", w.Code, w.Body.String())
	}
	resp := parseRefineResp(t, w)
	if resp.Code != 0 {
		t.Errorf("expected code 0, got %d msg=%s", resp.Code, resp.Message)
	}

	// 断言响应体字段(data 是 gin.H,需要解为 map)
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("resp.Data expected map, got %T body=%s", resp.Data, w.Body.String())
	}
	if v, _ := data["new_version"].(float64); int(v) != 2 {
		t.Errorf("new_version: expected 2, got %v", data["new_version"])
	}
	if v, _ := data["content"].(string); !strings.Contains(v, "v2") {
		t.Errorf("content: expected v2 marker, got %q", v)
	}

	// 断言 DB 里有 2 条 PersonalResult(旧 v1 + 新 v2)
	var prs []model.PersonalResult
	db.Where("task_id = ?", task.ID).Order("id ASC").Find(&prs)
	if len(prs) != 2 {
		t.Fatalf("expected 2 PersonalResults after refine, got %d", len(prs))
	}
	newestPR := prs[1]
	if newestPR.Content == "" || !strings.Contains(newestPR.Content, "v2") {
		t.Errorf("newest PR content wrong: %q", newestPR.Content)
	}
	// 断言 lineage 链
	newSnap := newestPR.GetSnapshot()
	if newSnap == nil {
		t.Fatalf("newest PR should have snapshot")
	}
	if newSnap.ContentVersion != 2 {
		t.Errorf("newSnap.ContentVersion: expected 2, got %d", newSnap.ContentVersion)
	}
	if newSnap.ParentSnapshotVersion == nil || *newSnap.ParentSnapshotVersion != 1 {
		t.Errorf("newSnap.ParentSnapshotVersion: expected *int(1), got %v", newSnap.ParentSnapshotVersion)
	}
	if newSnap.UserInstruction == nil || *newSnap.UserInstruction != "把风险章节扩充成一整段" {
		t.Errorf("newSnap.UserInstruction wrong: %v", newSnap.UserInstruction)
	}
}

// ─── Sanity: 确认 model.TriggerAgent 常量存在(SUM-43 新增)─────
func TestModelTriggerAgentConstantExists(t *testing.T) {
	if model.TriggerAgent == 0 {
		t.Error("model.TriggerAgent should not be 0 (would clash with default trigger_type value)")
	}
}

// ─── errors 包只是用来消除 unused import 提示 ─────────────────
var _ = errors.New
