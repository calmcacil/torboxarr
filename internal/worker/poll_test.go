package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrjoiny/torboxarr/internal/config"
	"github.com/mrjoiny/torboxarr/internal/files"
	"github.com/mrjoiny/torboxarr/internal/store"
	"github.com/mrjoiny/torboxarr/internal/torbox"
)

// pollTestEnv mirrors removeTestEnv but is used for poll-path tests.
type pollTestEnv struct {
	orch  *Orchestrator
	store *store.Store
	mock  *torbox.MockClient
	dir   string
}

func newPollTestEnv(t *testing.T) *pollTestEnv {
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

	return &pollTestEnv{orch: orch, store: st, mock: mock, dir: dir}
}

// TestProcessPollJob_LogicalErrorEscalatesAfterMaxAttempts verifies that a task
// whose upstream entry returns a structured TorBox error (e.g. a deleted or
// nonexistent task -> 500 with success:false) is retried a bounded number of
// times and then moved to remote_failed rather than polling forever. This is
// the exact scenario observed in production: a torrent deleted upstream keeps
// returning DATABASE_ERROR on every poll.
func TestProcessPollJob_LogicalErrorEscalatesAfterMaxAttempts(t *testing.T) {
	env := newPollTestEnv(t)

	var calls int
	env.mock.GetTaskStatusFn = func(_ context.Context, _, _ string) (*torbox.TaskStatus, error) {
		calls++
		return nil, &torbox.ErrTorboxLogical{Err: errors.New("torbox status 500: success:false DATABASE_ERROR")}
	}

	job := &store.Job{
		ID:            "ghost-001",
		PublicID:      "pub-ghost-001",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateRemoteActive,
		RemoteID:      ptr("59006036"),
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for attempt := 1; attempt <= maxPollAttempts; attempt++ {
		got, err := env.store.GetJobByID(ctx, "ghost-001")
		if err != nil {
			t.Fatal(err)
		}
		err = env.orch.processPollJob(ctx, got)
		switch {
		case attempt < maxPollAttempts:
			if err != nil {
				t.Fatalf("attempt %d: expected retry (no error), got %v", attempt, err)
			}
		default:
			if err != nil {
				t.Fatalf("attempt %d: expected escalation (no error), got %v", attempt, err)
			}
		}
	}

	if calls != maxPollAttempts {
		t.Errorf("expected %d GetTaskStatus calls, got %d", maxPollAttempts, calls)
	}

	got, err := env.store.GetJobByID(ctx, "ghost-001")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoteFailed {
		t.Errorf("after max attempts the job must become remote_failed; state=%s", got.State)
	}
	if got.Metadata.PollAttempts != maxPollAttempts {
		t.Errorf("expected PollAttempts=%d, got %d", maxPollAttempts, got.Metadata.PollAttempts)
	}
	if got.NextRunAt != nil {
		t.Error("remote_failed job must not be rescheduled")
	}
}

// TestProcessPollJob_TransientRetryableStillRetries confirms the same cap
// applies to transport-level retryable errors (e.g. a real TorBox outage), so
// a temporary blip does not immediately fail the job.
func TestProcessPollJob_TransientRetryableStillRetries(t *testing.T) {
	env := newPollTestEnv(t)

	var calls int
	env.mock.GetTaskStatusFn = func(_ context.Context, _, _ string) (*torbox.TaskStatus, error) {
		calls++
		return nil, torbox.MarkRetryable(errors.New("torbox status 503: transient"))
	}

	job := &store.Job{
		ID:            "retry-001",
		PublicID:      "pub-retry-001",
		SourceType:    store.SourceTypeTorrent,
		ClientKind:    store.ClientKindQBit,
		Category:      "movies",
		State:         store.StateRemoteActive,
		RemoteID:      ptr("59006037"),
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := env.store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for attempt := 1; attempt <= maxPollAttempts; attempt++ {
		got, err := env.store.GetJobByID(ctx, "retry-001")
		if err != nil {
			t.Fatal(err)
		}
		_ = env.orch.processPollJob(ctx, got)
	}

	if calls != maxPollAttempts {
		t.Errorf("expected %d GetTaskStatus calls, got %d", maxPollAttempts, calls)
	}
	got, err := env.store.GetJobByID(ctx, "retry-001")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateRemoteFailed {
		t.Errorf("after max attempts the job must become remote_failed; state=%s", got.State)
	}
	if got.NextRunAt != nil {
		t.Error("remote_failed job must not be rescheduled")
	}
}
