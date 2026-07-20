package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrjoiny/torboxarr/internal/config"
	"github.com/mrjoiny/torboxarr/internal/files"
	"github.com/mrjoiny/torboxarr/internal/store"
	"github.com/mrjoiny/torboxarr/internal/torbox"
)

// removeTestEnv bundles a configured Orchestrator with a controllable mock client
// and an in-memory store for exercising processRemoveJob / runRemover.
type removeTestEnv struct {
	orch  *Orchestrator
	store *store.Store
	mock  *torbox.MockClient
	dir   string
}

func newRemoveTestEnv(t *testing.T) *removeTestEnv {
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
	t.Cleanup(func() { db.Close() })
	st := store.New(db)

	dir := t.TempDir()
	layout := files.NewLayout(dir, filepath.Join(dir, "staging"), filepath.Join(dir, "completed"), filepath.Join(dir, "payloads"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Logging.Level = "ERROR"
	cfg.Database.Path = ":memory:"
	cfg.Data.Root = dir
	cfg.Data.Staging = filepath.Join(dir, "staging")
	cfg.Data.Completed = filepath.Join(dir, "completed")
	cfg.Data.Payloads = filepath.Join(dir, "payloads")
	cfg.TorBox.BaseURL = "https://api.torbox.app/v1"
	cfg.TorBox.APIToken = "test-token"
	cfg.Auth.QBitUsername = "admin"
	cfg.Auth.QBitPassword = "password"
	cfg.Auth.SABAPIKey = "sabapikey"
	cfg.Auth.SABNZBKey = "sabnzbkey"
	cfg.Workers.SubmitInterval = 5 * time.Second
	cfg.Workers.PollInterval = 30 * time.Second
	cfg.Workers.DownloadInterval = 5 * time.Second
	cfg.Workers.FinalizeInterval = 3 * time.Second
	cfg.Workers.RemoveInterval = 5 * time.Second
	cfg.Workers.PruneInterval = 12 * time.Hour
	cfg.Workers.SubmitRetryMin = 100 * time.Millisecond
	cfg.Workers.SubmitRetryMax = 1 * time.Second
	cfg.Workers.RemovedRetention = 30 * 24 * time.Hour
	cfg.Workers.BatchSize = 25

	mock := &torbox.MockClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	downloader := files.NewRangeDownloader(logger, 30*time.Second)
	orch := NewOrchestrator(cfg, logger, st, layout, downloader, mock)

	return &removeTestEnv{orch: orch, store: st, mock: mock, dir: dir}
}

// insertRemovePendingJob creates a remove_pending job with local files on disk
// and (optionally) an upstream remote id. Returns the created job.
func (e *removeTestEnv) insertRemovePendingJob(t *testing.T, id, remoteID string, sourceType store.SourceType) *store.Job {
	t.Helper()
	ctx := context.Background()

	completedDir := filepath.Join(e.dir, "completed", id)
	if err := os.MkdirAll(completedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	completedPath := filepath.Join(completedDir, "file.mkv")
	if err := os.WriteFile(completedPath, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &store.Job{
		ID:            id,
		PublicID:      "pub-" + id,
		SourceType:    sourceType,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateRemovePending,
		SubmissionKey: "key-" + id,
		DisplayName:   "Test " + id,
		CompletedPath: &completedPath,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if remoteID != "" {
		job.RemoteID = ptr(remoteID)
	}
	if err := e.store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

// insertRemovePendingJobAt is like insertRemovePendingJob but uses an explicit
// completed file path so tests can assert local deletion afterwards.
func (e *removeTestEnv) insertRemovePendingJobAt(t *testing.T, id, remoteID string, sourceType store.SourceType, completedPath string) *store.Job {
	t.Helper()
	ctx := context.Background()

	completedDir := filepath.Dir(completedPath)
	if err := os.MkdirAll(completedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(completedPath, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &store.Job{
		ID:            id,
		PublicID:      "pub-" + id,
		SourceType:    sourceType,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateRemovePending,
		SubmissionKey: "key-" + id,
		DisplayName:   "Test " + id,
		CompletedPath: &completedPath,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if remoteID != "" {
		job.RemoteID = ptr(remoteID)
	}
	if err := e.store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestProcessRemoveJob_UpstreamDeleteTorrent(t *testing.T) {
	env := newRemoveTestEnv(t)
	env.orch.cfg.UpstreamRemove = true

	var mu sync.Mutex
	var calls []string
	env.mock.DeleteTaskFn = func(_ context.Context, sourceType, remoteID string) error {
		mu.Lock()
		calls = append(calls, sourceType+":"+remoteID)
		mu.Unlock()
		return nil
	}

	completedPath := filepath.Join(env.dir, "completed", "t1", "file.mkv")
	env.insertRemovePendingJobAt(t, "t1", "57356712", store.SourceTypeTorrent, completedPath)

	ctx := context.Background()
	jobs, err := env.store.ClaimJobsDue(ctx, "remover", []store.JobState{store.StateRemovePending}, time.Now().UTC(), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := env.orch.processRemoveJob(ctx, job); err != nil {
			t.Fatalf("processRemoveJob: %v", err)
		}
		env.orch.releaseJobClaim(ctx, "remover", job.ID)
	}

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "torrent:57356712" {
		t.Fatalf("expected single torrent upstream delete, got %v", got)
	}

	got2, err := env.store.GetJobByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got2.State != store.StateRemoved {
		t.Errorf("expected StateRemoved, got %s", got2.State)
	}
	if _, err := os.Stat(completedPath); !os.IsNotExist(err) {
		t.Error("completed file should have been removed locally")
	}
}

func TestProcessRemoveJob_UpstreamDeleteUsenet(t *testing.T) {
	env := newRemoveTestEnv(t)
	env.orch.cfg.UpstreamRemove = true

	var mu sync.Mutex
	var calls []string
	env.mock.DeleteTaskFn = func(_ context.Context, sourceType, remoteID string) error {
		mu.Lock()
		calls = append(calls, sourceType+":"+remoteID)
		mu.Unlock()
		return nil
	}

	env.insertRemovePendingJob(t, "u1", "88123455", store.SourceTypeNZB)

	ctx := context.Background()
	jobs, err := env.store.ClaimJobsDue(ctx, "remover", []store.JobState{store.StateRemovePending}, time.Now().UTC(), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := env.orch.processRemoveJob(ctx, job); err != nil {
			t.Fatalf("processRemoveJob: %v", err)
		}
		env.orch.releaseJobClaim(ctx, "remover", job.ID)
	}

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "nzb:88123455" {
		t.Fatalf("expected single usenet upstream delete, got %v", got)
	}
}

func TestProcessRemoveJob_DisabledSkipsUpstream(t *testing.T) {
	env := newRemoveTestEnv(t)
	env.orch.cfg.UpstreamRemove = false

	called := false
	env.mock.DeleteTaskFn = func(_ context.Context, _, _ string) error {
		called = true
		return nil
	}

	env.insertRemovePendingJob(t, "d1", "57356712", store.SourceTypeTorrent)

	ctx := context.Background()
	jobs, err := env.store.ClaimJobsDue(ctx, "remover", []store.JobState{store.StateRemovePending}, time.Now().UTC(), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := env.orch.processRemoveJob(ctx, job); err != nil {
			t.Fatalf("processRemoveJob: %v", err)
		}
		env.orch.releaseJobClaim(ctx, "remover", job.ID)
	}

	if called {
		t.Error("upstream delete must not be called when TORBOXARR_UPSTREAM_REMOVE is false")
	}
	got, err := env.store.GetJobByID(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoved {
		t.Errorf("expected StateRemoved, got %s", got.State)
	}
}

func TestProcessRemoveJob_UpstreamFailureStillCleansLocally(t *testing.T) {
	env := newRemoveTestEnv(t)
	env.orch.cfg.UpstreamRemove = true

	env.mock.DeleteTaskFn = func(_ context.Context, _, _ string) error {
		return errors.New("torbox 500")
	}

	env.insertRemovePendingJob(t, "f1", "57356712", store.SourceTypeTorrent)

	ctx := context.Background()
	jobs, err := env.store.ClaimJobsDue(ctx, "remover", []store.JobState{store.StateRemovePending}, time.Now().UTC(), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if err := env.orch.processRemoveJob(ctx, job); err != nil {
			t.Fatalf("processRemoveJob: %v", err)
		}
		env.orch.releaseJobClaim(ctx, "remover", job.ID)
	}

	got, err := env.store.GetJobByID(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoved {
		t.Errorf("local cleanup must still proceed on upstream failure; state=%s", got.State)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "completed", "f1", "file.mkv")); !os.IsNotExist(err) {
		t.Error("completed file should have been removed locally despite upstream failure")
	}
}

func TestProcessRemoveJob_QueuedJobNoRemoteIDSkipsUpstream(t *testing.T) {
	env := newRemoveTestEnv(t)
	env.orch.cfg.UpstreamRemove = true

	called := false
	env.mock.DeleteTaskFn = func(_ context.Context, _, _ string) error {
		called = true
		return nil
	}

	// Queued job: only a queued_id, no remote_id yet.
	job := env.insertRemovePendingJob(t, "q1", "", store.SourceTypeTorrent)
	job.QueuedID = ptr("some-queue-auth-id")

	ctx := context.Background()
	if err := env.store.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	jobs, err := env.store.ClaimJobsDue(ctx, "remover", []store.JobState{store.StateRemovePending}, time.Now().UTC(), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if err := env.orch.processRemoveJob(ctx, j); err != nil {
			t.Fatalf("processRemoveJob: %v", err)
		}
		env.orch.releaseJobClaim(ctx, "remover", j.ID)
	}

	if called {
		t.Error("upstream delete must be skipped when there is no remote_id (queued job)")
	}
	got, err := env.store.GetJobByID(ctx, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoved {
		t.Errorf("expected StateRemoved for queued job, got %s", got.State)
	}
}

// TestRunRemover_ConcurrentClaims verifies that when two remover workers run
// concurrently against the same pending job, the job is claimed and the upstream
// delete is invoked exactly once (ClaimJobsDue serializes via claimed_by).
func TestRunRemover_ConcurrentClaims(t *testing.T) {
	env := newRemoveTestEnv(t)
	env.orch.cfg.UpstreamRemove = true

	var mu sync.Mutex
	var calls int
	env.mock.DeleteTaskFn = func(_ context.Context, _, _ string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}

	env.insertRemovePendingJob(t, "c1", "57356712", store.SourceTypeTorrent)

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = env.orch.runRemover(ctx)
		}()
	}
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected upstream delete exactly once across concurrent workers, got %d", got)
	}
	job, err := env.store.GetJobByID(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.StateRemoved {
		t.Errorf("expected StateRemoved, got %s", job.State)
	}
}
