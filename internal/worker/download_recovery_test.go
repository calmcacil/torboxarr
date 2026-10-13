package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrjoiny/torboxarr/internal/store"
	"github.com/mrjoiny/torboxarr/internal/torbox"
)

func TestExpiredDownloadLinkSurvivesTransientRefreshFailure(t *testing.T) {
	env := newQueuedTestEnv(t)
	ctx := context.Background()
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer stale.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer fresh.Close()
	calls := 0
	env.mock.GetDownloadLinksFn = func(context.Context, string, string) ([]torbox.DownloadAsset, error) {
		calls++
		if calls == 2 {
			return nil, torbox.MarkRetryable(context.DeadlineExceeded)
		}
		link := stale.URL
		if calls > 2 {
			link = fresh.URL
		}
		return []torbox.DownloadAsset{{FileID: "1", URL: link, RelativePath: "episode.mkv", Size: 7}}, nil
	}
	job := queuedTestJob("refresh-outage", store.StateLocalDownloadPending)
	job.RemoteID = stringPtr("123")
	if err := env.store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		job, err = env.store.GetJobByID(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.orch.processDownloadJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := env.store.GetJobByID(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 && (got.State != store.StateLocalDownloading || got.NextRunAt == nil) {
			t.Fatalf("state = %s, next run = %v, want recovery pending after refresh outage", got.State, got.NextRunAt)
		}
		if attempt == 2 && got.State != store.StateLocalVerify {
			t.Fatalf("state = %s, want verified-stage transfer after link recovery", got.State)
		}
	}
}
