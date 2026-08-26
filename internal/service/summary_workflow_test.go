//go:build cgo

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newSummaryWorkflowTestService(t *testing.T) (*SummaryWorkflowService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummaryWorkflowIdempotency{},
	); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return NewSummaryWorkflowService(db, nil, 31), db
}

func baseSummaryWorkflowInput() LegacyCreateSummaryWorkflowInput {
	return LegacyCreateSummaryWorkflowInput{
		ActorID: "creator",
		SpaceID: "space-1",
		Title:   "Weekly summary",
		Topic:   "Summarize delivery status",
		Sources: []SummaryWorkflowSource{{SourceType: model.SourceGroup, SourceID: "group-1"}},
	}
}

func TestSummaryWorkflowCreatePersonal(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)

	got, err := svc.CreateFromLegacyHTTP(context.Background(), baseSummaryWorkflowInput())
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if got.Replayed {
		t.Fatal("first create must not be marked replayed")
	}
	if got.Target != SummaryWorkflowPersonal {
		t.Fatalf("target = %q, want %q", got.Target, SummaryWorkflowPersonal)
	}
	if got.WorkerTrigger == nil || got.WorkerTrigger.TaskID != got.Task.ID || got.WorkerTrigger.ParticipantRefID == 0 {
		t.Fatalf("worker trigger = %#v, want committed task and creator participant", got.WorkerTrigger)
	}

	var taskCount, sourceCount, participantCount, personalCount int64
	db.Model(&model.SummaryTask{}).Count(&taskCount)
	db.Model(&model.SummarySource{}).Count(&sourceCount)
	db.Model(&model.SummaryParticipant{}).Count(&participantCount)
	db.Model(&model.PersonalResult{}).Count(&personalCount)
	if taskCount != 1 || sourceCount != 1 || participantCount != 1 || personalCount != 1 {
		t.Fatalf("row counts task/source/participant/personal = %d/%d/%d/%d, want 1/1/1/1", taskCount, sourceCount, participantCount, personalCount)
	}
}

func TestSummaryWorkflowCreateTeamDeduplicatesParticipants(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.Participants = []SummaryWorkflowParticipant{
		{UserID: "p1", UserName: "P1"},
		{UserID: "p1", UserName: "duplicate"},
		{UserID: "p2", UserName: "P2"},
		{UserID: "creator", UserName: "creator duplicate"},
	}

	got, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if got.Target != SummaryWorkflowTeam {
		t.Fatalf("target = %q, want %q", got.Target, SummaryWorkflowTeam)
	}
	var participants []model.SummaryParticipant
	if err := db.Order("user_id").Find(&participants).Error; err != nil {
		t.Fatal(err)
	}
	if len(participants) != 3 {
		t.Fatalf("participants = %d, want creator + 2 unique collaborators", len(participants))
	}
	if participants[0].UserID != "creator" || participants[0].Status != model.ParticipantAccepted {
		t.Fatalf("creator participant = %#v, want accepted creator", participants[0])
	}
}

func TestSummaryWorkflowOriginAutofillsSource(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.Sources = nil
	in.OriginChannelID = "group-origin"
	in.OriginChannelType = model.OriginChannelGroup

	got, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if got.Inferred {
		t.Fatal("origin channel is a concrete source, not an inferred scope")
	}
	var source model.SummarySource
	if err := db.First(&source).Error; err != nil {
		t.Fatal(err)
	}
	if source.SourceID != "group-origin" || source.SourceType != model.SourceGroup {
		t.Fatalf("source = %#v, want origin group", source)
	}
}

func TestSummaryWorkflowValidationFailureWritesNothing(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.ActorID = ""

	_, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	var bizErr *BizError
	if !errors.As(err, &bizErr) || bizErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error = %v, want unauthorized BizError", err)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 0 {
		t.Fatalf("task count = %d, want 0", count)
	}
}

func TestSummaryWorkflowIdempotencyReplayAndMismatch(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.IdempotencyKey = "summary-create-001"

	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first Create() error: %v", err)
	}
	second, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("replay Create() error: %v", err)
	}
	if !second.Replayed || second.Task.ID != first.Task.ID || second.WorkerTrigger != nil {
		t.Fatalf("replay = %#v, want same task and no worker trigger", second)
	}

	var taskCount int64
	db.Model(&model.SummaryTask{}).Count(&taskCount)
	if taskCount != 1 {
		t.Fatalf("task count after replay = %d, want 1", taskCount)
	}

	in.Topic = "different request"
	_, err = svc.CreateFromLegacyHTTP(context.Background(), in)
	var mismatch *SummaryWorkflowIdempotencyError
	if !errors.As(err, &mismatch) {
		t.Fatalf("mismatch error = %v, want SummaryWorkflowIdempotencyError", err)
	}
	if mismatch.ExistingTaskID != first.Task.ID || mismatch.BizError.Code != WorkflowIdempotencyMismatchCode || mismatch.Reason != "request_mismatch" {
		t.Fatalf("mismatch = %#v, want existing task %d and code %d", mismatch, first.Task.ID, WorkflowIdempotencyMismatchCode)
	}
}

func TestSummaryWorkflowIdempotencyCreatorChangeIsRequestMismatch(t *testing.T) {
	svc, _ := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.IdempotencyKey = "summary-create-creator-change"

	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first Create() error: %v", err)
	}
	in.CreatorID = "legacy-delegated-creator"
	_, err = svc.CreateFromLegacyHTTP(context.Background(), in)
	var mismatch *SummaryWorkflowIdempotencyError
	if !errors.As(err, &mismatch) {
		t.Fatalf("creator-change error = %v, want SummaryWorkflowIdempotencyError", err)
	}
	if mismatch.ExistingTaskID != first.Task.ID || mismatch.Reason != "request_mismatch" || mismatch.RecoveryAction != "open_existing_summary" {
		t.Fatalf("creator-change mismatch = %#v, want request_mismatch for task %d", mismatch, first.Task.ID)
	}
}

func TestSummaryWorkflowIdempotencyBindingReadsBackWinner(t *testing.T) {
	_, db := newSummaryWorkflowTestService(t)
	winner := model.SummaryWorkflowIdempotency{
		SpaceID: "space-1", UserID: "creator", IdempotencyKey: "race-key", RequestHash: "same", TaskID: 11,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return createSummaryWorkflowIdempotencyBinding(tx, &winner)
	}); err != nil {
		t.Fatalf("create winner binding: %v", err)
	}
	loser := model.SummaryWorkflowIdempotency{
		SpaceID: "space-1", UserID: "creator", IdempotencyKey: "race-key", RequestHash: "same", TaskID: 12,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return createSummaryWorkflowIdempotencyBinding(tx, &loser)
	}); !errors.Is(err, errSummaryWorkflowIdempotencyRace) {
		t.Fatalf("loser error = %v, want idempotency race", err)
	}
}

func TestSummaryWorkflowDeletedTaskKeepsIdempotencyTombstone(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.IdempotencyKey = "deleted-task-key"
	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", first.Task.ID).Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	_, err = svc.CreateFromLegacyHTTP(context.Background(), in)
	var stale *SummaryWorkflowIdempotencyError
	if !errors.As(err, &stale) || stale.Reason != "deleted_summary" || stale.ExistingTaskID != first.Task.ID {
		t.Fatalf("stale error = %#v (%v), want deleted task conflict", stale, err)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count after deleted replay = %d, want 1", count)
	}
}

func TestSummaryWorkflowIdempotencyIsScopedByUserAndSpace(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	base := baseSummaryWorkflowInput()
	base.IdempotencyKey = "shared-client-key"
	first, err := svc.CreateFromLegacyHTTP(context.Background(), base)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	otherUser := base
	otherUser.ActorID = "creator-2"
	otherUser.CreatorID = "creator-2"
	second, err := svc.CreateFromLegacyHTTP(context.Background(), otherUser)
	if err != nil {
		t.Fatalf("other-user create: %v", err)
	}
	otherSpace := base
	otherSpace.SpaceID = "space-2"
	third, err := svc.CreateFromLegacyHTTP(context.Background(), otherSpace)
	if err != nil {
		t.Fatalf("other-space create: %v", err)
	}
	if first.Task.ID == second.Task.ID || first.Task.ID == third.Task.ID || second.Task.ID == third.Task.ID {
		t.Fatalf("scoped idempotency returned duplicate ids: %d, %d, %d", first.Task.ID, second.Task.ID, third.Task.ID)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 3 {
		t.Fatalf("task count = %d, want 3 independently scoped creates", count)
	}
}

func TestValidSummaryWorkflowIdempotencyKey(t *testing.T) {
	for _, tt := range []struct {
		key  string
		want bool
	}{
		{"request-1", true},
		{"", false},
		{" leading-space", false},
		{"bad/key", false},
	} {
		if got := ValidSummaryWorkflowIdempotencyKey(tt.key); got != tt.want {
			t.Errorf("ValidSummaryWorkflowIdempotencyKey(%q) = %t, want %t", tt.key, got, tt.want)
		}
	}
}
