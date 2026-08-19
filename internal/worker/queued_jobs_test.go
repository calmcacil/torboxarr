package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrjoiny/torboxarr/internal/config"
	"github.com/mrjoiny/torboxarr/internal/files"
	"github.com/mrjoiny/torboxarr/internal/store"
	"github.com/mrjoiny/torboxarr/internal/torbox"
)

type queuedTestEnv struct {
	orch  *Orchestrator
	store *store.Store
	mock  *torbox.MockClient
}

func newQueuedTestEnv(t *testing.T) *queuedTestEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := store.RunMigrationsFS(db, store.EmbeddedMigrations); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	dir := t.TempDir()
	layout := files.NewLayout(dir, filepath.Join(dir, "staging"), filepath.Join(dir, "completed"), filepath.Join(dir, "payloads"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Data.Root = dir
	cfg.Data.Staging = filepath.Join(dir, "staging")
	cfg.Data.Completed = filepath.Join(dir, "completed")
	cfg.Data.Payloads = filepath.Join(dir, "payloads")
	cfg.Workers.PollInterval = time.Hour
	cfg.Workers.QueuedForceStartAfter = 3 * time.Hour
	cfg.Workers.RemoteAbsenceAttempts = 3
	cfg.Workers.BatchSize = 25
	mock := &torbox.MockClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := NewOrchestrator(cfg, logger, st, layout, files.NewRangeDownloader(logger, time.Second), mock)
	return &queuedTestEnv{orch: orch, store: st, mock: mock}
}

func TestProcessPollJob_ForceStartsAgedQueuedJobOnce(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	for i := 0; i < 2; i++ {
		got, _ := env.store.GetJobByID(context.Background(), job.ID)
		if err := env.orch.processPollJob(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if calls != 1 {
		t.Fatalf("force-start calls = %d, want 1", calls)
	}
	if got.Metadata.ForceStartAcceptedAt == nil {
		t.Fatal("ForceStartAcceptedAt was not persisted")
	}
}

func TestProcessPollJob_AcceptedForceStartSurvivesReconstructedOrchestrator(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-restart", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	restarted := NewOrchestrator(env.orch.cfg, env.orch.log, env.store, env.orch.layout, env.orch.downloader, env.mock)
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if err := restarted.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("force-start calls = %d, want one accepted request across restart", calls)
	}
}

func TestProcessPollJob_DoesNotForceStartBeforeThreshold(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-time.Hour)
	job := queuedTestJob("force-start-early", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("force-start calls = %d, want 0", calls)
	}
}

func TestProcessPollJob_UsesEarlierReliableQueueCreationTime(t *testing.T) {
	env := newQueuedTestEnv(t)
	observedAt := time.Now().UTC()
	queuedAt := observedAt.Add(-4 * time.Hour)
	job := queuedTestJob("force-start-created-at", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &observedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", QueueCreatedAt: &queuedAt, State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if calls != 1 || got.Metadata.QueuedAt == nil || !got.Metadata.QueuedAt.Equal(queuedAt) {
		t.Fatalf("calls=%d QueuedAt=%v, want one request at %v", calls, got.Metadata.QueuedAt, queuedAt)
	}
}

func TestProcessPollJob_DisabledForceStartDoesNotCallTorBox(t *testing.T) {
	env := newQueuedTestEnv(t)
	env.orch.cfg.Workers.QueuedForceStartAfter = 0
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-disabled", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("force-start calls = %d, want 0", calls)
	}
}

func TestProcessPollJob_HashMatchWithoutQueueIDDoesNotForceStart(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-hash-only", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{Hash: "hash-42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("force-start calls = %d, want 0", calls)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.QueuedID == nil || *got.QueuedID != "42" {
		t.Fatalf("QueuedID = %v, want stale job ID retained for reconciliation", got.QueuedID)
	}
}

func TestProcessPollJob_FutureQueueTimestampUsesObservationTime(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("force-start-future-timestamp", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", QueueCreatedAt: &future, State: "queued"}, nil
	}
	before := time.Now().UTC()
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.Metadata.QueuedAt == nil {
		t.Fatal("QueuedAt was not initialized")
	}
	if got.Metadata.QueuedAt.Before(before.Add(-time.Second)) || got.Metadata.QueuedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("QueuedAt = %v, want local observation time", got.Metadata.QueuedAt)
	}
}

func TestProcessPollJob_ForceStartRetryIsThrottled(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-throttled", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	attemptedAt := time.Now().UTC().Add(-time.Minute)
	job.Metadata.ForceStartLastAttemptAt = &attemptedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("force-start calls = %d, want throttled", calls)
	}
}

func TestProcessPollJob_UnsupportedSourceDoesNotForceStart(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-unsupported", store.StateRemoteQueued)
	job.SourceType = store.SourceType("future-source")
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("force-start calls = %d, want 0 for unsupported source", calls)
	}
}

func TestProcessPollJob_NewQueueIDResetsForceStartLifecycle(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	acceptedAt := queuedAt.Add(time.Hour)
	job := queuedTestJob("force-start-new-lifecycle", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	job.Metadata.ForceStartAcceptedAt = &acceptedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		createdAt := time.Now().UTC().Add(-4 * time.Hour)
		return &torbox.TaskStatus{QueuedID: "43", QueueCreatedAt: &createdAt, State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if calls != 1 || got.Metadata.ForceStartAcceptedAt == nil {
		t.Fatalf("calls=%d accepted=%v", calls, got.Metadata.ForceStartAcceptedAt != nil)
	}
	if got.QueuedID == nil || *got.QueuedID != "43" {
		t.Fatalf("QueuedID = %v, want 43", got.QueuedID)
	}
}

func TestProcessPollJob_RetriesFailedForceStart(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-retry", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		if calls == 1 {
			return errors.New("temporarily unavailable")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		got, _ := env.store.GetJobByID(context.Background(), job.ID)
		got.Metadata.ForceStartLastAttemptAt = nil
		if err := env.orch.processPollJob(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if calls != 2 || got.Metadata.ForceStartAcceptedAt == nil || got.State != store.StateRemoteQueued {
		t.Fatalf("calls=%d accepted=%v state=%s", calls, got.Metadata.ForceStartAcceptedAt != nil, got.State)
	}
}

func TestProcessPollJob_ForceStartCannotPersistAfterRemoval(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	job := queuedTestJob("force-start-removal", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(ctx context.Context, _, _ string) error {
		current, err := env.store.GetJobByID(ctx, job.ID)
		if err != nil {
			return err
		}
		current.DeleteRequested = true
		return env.store.UpdateJobState(ctx, current, store.StateRemovePending, "remove requested during force start")
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemovePending || !got.DeleteRequested {
		t.Fatalf("state=%s delete_requested=%v", got.State, got.DeleteRequested)
	}
}

func queuedTestJob(id string, state store.JobState) *store.Job {
	now := time.Now().UTC()
	return &store.Job{
		ID:            id,
		PublicID:      "pub-" + id,
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         state,
		SubmissionKey: "key-" + id,
		DisplayName:   "Test " + id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestProcessSubmitJob_DuplicateIDStaysQueued(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("submit-queued", store.StateSubmitPending)
	job.SourceURI = stringPtr("magnet:?xt=urn:btih:test")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.CreateTorrentTaskFn = func(context.Context, torbox.CreateTorrentTaskRequest) (*torbox.CreateTaskResponse, error) {
		return &torbox.CreateTaskResponse{RemoteID: "42", QueuedID: "42", RemoteHash: "hash-42"}, nil
	}

	got, err := env.store.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.orch.processSubmitJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, err = env.store.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
	if got.RemoteID != nil {
		t.Fatalf("RemoteID = %v, want nil", got.RemoteID)
	}
	if got.QueuedID == nil || *got.QueuedID != "42" {
		t.Fatalf("QueuedID = %v, want 42", got.QueuedID)
	}
}

func TestProcessSubmitJob_QueuedSubmissionStartsNewForceStartLifecycle(t *testing.T) {
	env := newQueuedTestEnv(t)
	oldQueuedAt := time.Now().UTC().Add(-24 * time.Hour)
	oldAttemptAt := oldQueuedAt.Add(time.Hour)
	oldAcceptedAt := oldAttemptAt.Add(time.Second)
	job := queuedTestJob("submit-new-lifecycle", store.StateSubmitPending)
	job.Metadata.QueuedAt = &oldQueuedAt
	job.Metadata.ForceStartLastAttemptAt = &oldAttemptAt
	job.Metadata.ForceStartAcceptedAt = &oldAcceptedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.CreateTorrentTaskFn = func(context.Context, torbox.CreateTorrentTaskRequest) (*torbox.CreateTaskResponse, error) {
		return &torbox.CreateTaskResponse{QueuedID: "42", RemoteHash: "hash-42"}, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processSubmitJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
	if got.Metadata.QueuedAt == nil || !got.Metadata.QueuedAt.After(oldQueuedAt) {
		t.Fatalf("QueuedAt = %v, want new submission time after %v", got.Metadata.QueuedAt, oldQueuedAt)
	}
	if got.Metadata.ForceStartLastAttemptAt != nil || got.Metadata.ForceStartAcceptedAt != nil {
		t.Fatalf("force-start history = (%v, %v), want cleared for new lifecycle", got.Metadata.ForceStartLastAttemptAt, got.Metadata.ForceStartAcceptedAt)
	}
}

func TestProcessSubmitJob_ExplicitActiveDuplicateIDsRemainActive(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("submit-active-duplicate", store.StateSubmitPending)
	job.SourceURI = stringPtr("magnet:?xt=urn:btih:test")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.CreateTorrentTaskFn = func(context.Context, torbox.CreateTorrentTaskRequest) (*torbox.CreateTaskResponse, error) {
		return &torbox.CreateTaskResponse{
			RemoteID:         "42",
			QueuedID:         "42",
			ActiveIDExplicit: true,
			RemoteHash:       "hash-42",
		}, nil
	}

	got, err := env.store.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.orch.processSubmitJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, err = env.store.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoteActive {
		t.Fatalf("state = %s, want remote_active", got.State)
	}
	if got.RemoteID == nil || *got.RemoteID != "42" {
		t.Fatalf("RemoteID = %v, want 42", got.RemoteID)
	}
}

func TestProcessSubmitJob_DoesNotReviveConcurrentRemoval(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("submit-removal", store.StateSubmitPending)
	job.SourceURI = stringPtr("magnet:?xt=urn:btih:test")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.CreateTorrentTaskFn = func(ctx context.Context, req torbox.CreateTorrentTaskRequest) (*torbox.CreateTaskResponse, error) {
		current, err := env.store.GetJobByID(ctx, job.ID)
		if err != nil {
			return nil, err
		}
		current.DeleteRequested = true
		if err := env.store.UpdateJobState(ctx, current, store.StateRemovePending, "remove requested during submission"); err != nil {
			return nil, err
		}
		return &torbox.CreateTaskResponse{RemoteID: "42"}, nil
	}

	got, err := env.store.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.orch.processSubmitJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, err = env.store.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemovePending {
		t.Fatalf("state = %s, want remove_pending", got.State)
	}
}

func TestProcessPollJob_QueuedGenericIDDoesNotPromote(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("queued-generic", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", Hash: "hash-42", State: "queued"}, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
}

func TestProcessPollJob_ActiveMissRecoversQueued(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("active-queued", store.StateRemoteActive)
	job.RemoteID = stringPtr("42")
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	job.Metadata.PollAttempts = 2
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("torbox status 500: DATABASE_ERROR")
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", Hash: "hash-42", State: "queued"}, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
	if got.Metadata.PollAttempts != 0 {
		t.Fatalf("PollAttempts = %d, want 0", got.Metadata.PollAttempts)
	}
	if got.ErrorMessage != nil {
		t.Fatalf("ErrorMessage = %v, want nil", got.ErrorMessage)
	}
}

func TestProcessPollJob_ActiveToQueuedWithoutAcceptedForceStartStartsNewLifecycle(t *testing.T) {
	env := newQueuedTestEnv(t)
	oldQueuedAt := time.Now().UTC().Add(-24 * time.Hour)
	job := queuedTestJob("active-queued-new-lifecycle", store.StateRemoteActive)
	job.RemoteID = stringPtr("99")
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &oldQueuedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("active task unavailable")
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
	if calls != 0 {
		t.Fatalf("force-start calls = %d, want 0 for a new lifecycle", calls)
	}
	if got.Metadata.QueuedAt == nil || !got.Metadata.QueuedAt.After(oldQueuedAt) {
		t.Fatalf("QueuedAt = %v, want new observation after %v", got.Metadata.QueuedAt, oldQueuedAt)
	}
}

func TestProcessPollJob_ActiveToQueuedWithAcceptedForceStartRetainsLifecycle(t *testing.T) {
	env := newQueuedTestEnv(t)
	queuedAt := time.Now().UTC().Add(-4 * time.Hour)
	acceptedAt := queuedAt.Add(time.Hour)
	job := queuedTestJob("active-queued-same-lifecycle", store.StateRemoteActive)
	job.RemoteID = stringPtr("99")
	job.QueuedID = stringPtr("42")
	job.Metadata.QueuedAt = &queuedAt
	job.Metadata.ForceStartAcceptedAt = &acceptedAt
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls int
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("active task unavailable")
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", State: "queued"}, nil
	}
	env.mock.ForceStartQueuedTaskFn = func(context.Context, string, string) error {
		calls++
		return nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if calls != 0 || got.Metadata.ForceStartAcceptedAt == nil || !got.Metadata.QueuedAt.Equal(queuedAt) {
		t.Fatalf("calls=%d accepted=%v QueuedAt=%v, want retained lifecycle", calls, got.Metadata.ForceStartAcceptedAt != nil, got.Metadata.QueuedAt)
	}
}

func TestProcessPollJob_ActiveMissUsesQueueAuthForUsenetRecovery(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("active-queued-nzb", store.StateRemoteActive)
	job.SourceType = store.SourceTypeNZB
	job.ClientKind = store.ClientKindSAB
	job.RemoteID = stringPtr("42")
	job.QueueAuthID = stringPtr("auth-42")
	job.RemoteHash = stringPtr("hash-42")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var gotAuthID string
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("torbox status 500: DATABASE_ERROR")
	}
	env.mock.FindQueuedTaskFn = func(_ context.Context, sourceType, queuedID, queueAuthID, remoteHash string) (*torbox.TaskStatus, error) {
		if sourceType != "nzb" || queuedID != "" || remoteHash != "hash-42" {
			t.Fatalf("queue lookup identity = (%q, %q, %q)", sourceType, queuedID, remoteHash)
		}
		gotAuthID = queueAuthID
		return &torbox.TaskStatus{QueueAuthID: "auth-42", Hash: "hash-42", State: "queued"}, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if gotAuthID != "auth-42" {
		t.Fatalf("queue auth ID = %q, want auth-42", gotAuthID)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
}

func TestReconcileStartupRecoversConfirmedActiveFailure(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("startup-active", store.StateRemoteFailed)
	job.RemoteID = stringPtr("42")
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	message := "poll failed after max attempts; upstream task unrecoverable (not found or TorBox unavailable)"
	job.ErrorMessage = &message
	job.Metadata.PollAttempts = 5
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{RemoteID: "42", Hash: "hash-42", State: "downloading"}, nil
	}

	if err := env.orch.reconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteActive {
		t.Fatalf("state = %s, want remote_active", got.State)
	}
	if got.ErrorMessage != nil || got.Metadata.PollAttempts != 0 {
		t.Fatalf("recovered failure fields = (%v, %d), want cleared", got.ErrorMessage, got.Metadata.PollAttempts)
	}
}

func TestProcessPollJob_QueuedPromotesOnlyAfterActiveMatch(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("promote", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{RemoteID: "99", Hash: "hash-42", State: "downloading"}, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteActive {
		t.Fatalf("state = %s, want remote_active", got.State)
	}
	if got.RemoteID == nil || *got.RemoteID != "99" {
		t.Fatalf("RemoteID = %v, want 99", got.RemoteID)
	}
}

func TestReconcileStartupRecoversConfirmedQueuedFailure(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("startup-recover", store.StateRemoteFailed)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	message := "remote task not found in TorBox queue or active list after max attempts"
	job.ErrorMessage = &message
	job.Metadata.PollAttempts = 3
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", Hash: "hash-42", State: "queued"}, nil
	}

	if err := env.orch.reconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
	if got.ErrorMessage != nil {
		t.Fatalf("ErrorMessage = %v, want nil", got.ErrorMessage)
	}
	if got.Metadata.PollAttempts != 0 {
		t.Fatalf("PollAttempts = %d, want 0", got.Metadata.PollAttempts)
	}
}

func TestReconcileStartupInitializesRecoveredQueueAgeFromObservation(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("startup-recover-age", store.StateRemoteFailed)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	message := "remote task not found in TorBox queue or active list after max attempts"
	job.ErrorMessage = &message
	job.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", Hash: "hash-42", State: "queued"}, nil
	}

	before := time.Now().UTC()
	if err := env.orch.reconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if got.Metadata.QueuedAt == nil {
		t.Fatal("QueuedAt was not initialized")
	}
	if got.Metadata.QueuedAt.Before(before.Add(-time.Second)) || got.Metadata.QueuedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("QueuedAt = %v, want recovery-time timestamp", got.Metadata.QueuedAt)
	}
}

func TestReconcileStartupLeavesUnrelatedRemoteFailure(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("startup-unrelated", store.StateRemoteFailed)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	message := "remote task failed permanently upstream"
	job.ErrorMessage = &message
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{QueuedID: "42", Hash: "hash-42", State: "queued"}, nil
	}

	if err := env.orch.reconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteFailed {
		t.Fatalf("state = %s, want remote_failed", got.State)
	}
}

func TestReconcileStartupLeavesConfirmedActiveTerminalFailure(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("startup-terminal-active", store.StateRemoteFailed)
	job.RemoteID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	message := "poll failed after max attempts; upstream task unrecoverable (not found or TorBox unavailable)"
	job.ErrorMessage = &message
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return &torbox.TaskStatus{RemoteID: "42", Hash: "hash-42", Failed: true, State: "failed"}, nil
	}

	if err := env.orch.reconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteFailed {
		t.Fatalf("state = %s, want remote_failed", got.State)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != message {
		t.Fatalf("ErrorMessage = %v, want original failure", got.ErrorMessage)
	}
}

func TestProcessPollJob_LookupErrorsRemainNonTerminal(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("lookup-error", store.StateRemoteActive)
	job.RemoteID = stringPtr("42")
	job.QueuedID = stringPtr("42")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("torbox status 503: unavailable")
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("torbox status 503: unavailable")
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteActive {
		t.Fatalf("state = %s, want remote_active", got.State)
	}
	if got.NextRunAt == nil {
		t.Fatal("expected lookup failure to schedule another poll")
	}
}

func TestProcessPollJob_ActiveMissingCountsOnlyAfterQueueCheck(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("active-absent", store.StateRemoteActive)
	job.RemoteID = stringPtr("42")
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	job.Metadata.PollAttempts = 1
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.Metadata.PollAttempts != 2 {
		t.Fatalf("PollAttempts = %d, want 2", got.Metadata.PollAttempts)
	}
	if got.State != store.StateRemoteActive {
		t.Fatalf("state = %s, want remote_active", got.State)
	}
}

func TestProcessPollJob_ActiveMissingAfterConfirmedChecksEventuallyFails(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("active-absent-confirmed", store.StateRemoteActive)
	job.RemoteID = stringPtr("42")
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	job.Metadata.PollAttempts = env.orch.cfg.Workers.RemoteAbsenceAttempts - 1
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteFailed {
		t.Fatalf("state = %s, want remote_failed", got.State)
	}
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "queue or active list") {
		t.Fatalf("ErrorMessage = %v, want confirmed absence message", got.ErrorMessage)
	}
}

func TestProcessPollJob_ActiveOnlyMissingRemainsActive(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("active-only-missing", store.StateRemoteActive)
	job.RemoteID = stringPtr("active-42")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteActive {
		t.Fatalf("state = %s, want remote_active", got.State)
	}
	if got.Metadata.PollAttempts != 0 {
		t.Fatalf("PollAttempts = %d, want 0", got.Metadata.PollAttempts)
	}
	if got.NextRunAt == nil {
		t.Fatal("expected active-only miss to schedule another poll")
	}
}

func TestProcessPollJob_ConfirmedAbsenceEventuallyFails(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("absent", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}

	for i := 0; i < env.orch.cfg.Workers.RemoteAbsenceAttempts; i++ {
		got, _ := env.store.GetJobByID(context.Background(), job.ID)
		if err := env.orch.processPollJob(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteFailed {
		t.Fatalf("state = %s, want remote_failed", got.State)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "remote task not found in TorBox queue or active list after max attempts" {
		t.Fatalf("ErrorMessage = %v, want confirmed absence message", got.ErrorMessage)
	}
}

func TestProcessPollJob_AbsenceLookupErrorDoesNotCount(t *testing.T) {
	env := newQueuedTestEnv(t)
	job := queuedTestJob("absence-error", store.StateRemoteQueued)
	job.QueuedID = stringPtr("42")
	job.RemoteHash = stringPtr("hash-42")
	job.Metadata.PollAttempts = 2
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.mock.FindQueuedTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, errors.New("torbox status 500: unavailable")
	}
	env.mock.FindActiveTaskFn = func(context.Context, string, string, string, string) (*torbox.TaskStatus, error) {
		return nil, nil
	}

	got, _ := env.store.GetJobByID(context.Background(), job.ID)
	if err := env.orch.processPollJob(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.GetJobByID(context.Background(), job.ID)
	if got.State != store.StateRemoteQueued {
		t.Fatalf("state = %s, want remote_queued", got.State)
	}
	if got.Metadata.PollAttempts != 2 {
		t.Fatalf("PollAttempts = %d, want unchanged at 2", got.Metadata.PollAttempts)
	}
}

func stringPtr(value string) *string { return &value }
