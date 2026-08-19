package worker

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mrjoiny/torboxarr/internal/store"
	"github.com/mrjoiny/torboxarr/internal/torbox"
)

func (o *Orchestrator) runSubmitter(ctx context.Context) error {
	jobs, err := o.store.ClaimJobsDue(ctx, "submitter", []store.JobState{store.StateSubmitPending, store.StateSubmitRetry}, time.Now().UTC(), o.cfg.Workers.BatchSize)
	if err != nil {
		return err
	}
	if len(jobs) > 0 {
		o.log.Debug("submitter fetched jobs", "count", len(jobs))
	}
	for _, job := range jobs {
		if err := o.processSubmitJob(ctx, job); err != nil {
			o.log.Error("submit job failed", "job_id", job.ID, "error", err)
		}
		o.releaseJobClaim(ctx, "submitter", job.ID)
	}
	return nil
}

func (o *Orchestrator) processSubmitJob(ctx context.Context, job *store.Job) error {
	expectedState := job.State
	o.log.Info("submitting job to torbox",
		"job_id", job.ID,
		"public_id", job.PublicID,
		"source_type", job.SourceType,
		"state", job.State,
		"display_name", job.DisplayName,
		"has_payload", job.PayloadRef != nil,
		"retry_count", job.RetryCount,
	)
	var (
		resp *torbox.CreateTaskResponse
		err  error
	)

	switch job.SourceType {
	case store.SourceTypeTorrent:
		resp, err = o.torbox.CreateTorrentTask(ctx, torbox.CreateTorrentTaskRequest{
			Magnet:          deref(job.SourceURI),
			PayloadPath:     deref(job.PayloadRef),
			Name:            job.DisplayName,
			AsQueued:        job.Metadata.AsQueued,
			AddOnlyIfCached: job.Metadata.AddOnlyIfCached,
		})
	case store.SourceTypeNZB:
		postProcessing := job.Metadata.PostProcessing
		resp, err = o.torbox.CreateUsenetTask(ctx, torbox.CreateUsenetTaskRequest{
			Link:            deref(job.SourceURI),
			PayloadPath:     deref(job.PayloadRef),
			Name:            job.DisplayName,
			Password:        job.Metadata.Password,
			PostProcessing:  &postProcessing,
			AsQueued:        job.Metadata.AsQueued,
			AddOnlyIfCached: job.Metadata.AddOnlyIfCached,
		})
	default:
		err = fmt.Errorf("unsupported source type %q", job.SourceType)
	}

	if err != nil {
		return o.handleSubmitFailure(ctx, job, err)
	}
	if resp == nil {
		return o.handleSubmitFailure(ctx, job, torbox.MarkRetryable(fmt.Errorf("torbox create returned empty response")))
	}

	remoteID := strings.TrimSpace(resp.RemoteID)
	queuedID := strings.TrimSpace(resp.QueuedID)
	// A duplicated generic ID is queue tracking, not active confirmation.
	if remoteID != "" && queuedID != "" && remoteID == queuedID && !resp.ActiveIDExplicit {
		remoteID = ""
	}
	job.RemoteID = ptr(remoteID)
	job.QueuedID = ptr(queuedID)
	job.QueueAuthID = ptr(strings.TrimSpace(resp.QueueAuthID))
	remoteHash := strings.TrimSpace(resp.RemoteHash)
	if remoteHash == "" && job.InfoHash != nil {
		remoteHash = strings.TrimSpace(*job.InfoHash)
	}
	job.RemoteHash = ptr(remoteHash)
	if strings.TrimSpace(resp.DisplayName) != "" {
		job.DisplayName = strings.TrimSpace(resp.DisplayName)
	}
	if job.RemoteID == nil && job.QueuedID == nil && job.QueueAuthID == nil && job.RemoteHash == nil {
		return o.handleSubmitFailure(ctx, job, torbox.MarkRetryable(fmt.Errorf("torbox create returned no remote id or queue tracking identifiers")))
	}
	job.ErrorMessage = nil
	job.RetryCount = 0
	if remoteID == "" && job.Metadata.QueuedAt == nil {
		now := time.Now().UTC()
		job.Metadata.QueuedAt = &now
	}
	nextRun := time.Now().UTC().Add(o.cfg.Workers.PollInterval)
	job.NextRunAt = &nextRun
	job.UpdatedAt = time.Now().UTC()
	if job.RemoteID != nil {
		o.log.Info("remote task created",
			"job_id", job.ID,
			"public_id", job.PublicID,
			"remote_id", deref(job.RemoteID),
			"queued_id", deref(job.QueuedID),
			"remote_hash", deref(job.RemoteHash),
			"display_name", job.DisplayName,
			"next_run_at", nextRun.Format(time.RFC3339Nano),
		)
		_, err := o.store.UpdateJobStateIfCurrent(ctx, job, expectedState, store.StateRemoteActive, "remote task created")
		return err
	}

	o.log.Info("remote task queued",
		"job_id", job.ID,
		"public_id", job.PublicID,
		"queued_id", deref(job.QueuedID),
		"queue_auth_id", deref(job.QueueAuthID),
		"remote_hash", deref(job.RemoteHash),
		"display_name", job.DisplayName,
		"next_run_at", nextRun.Format(time.RFC3339Nano),
	)
	_, err = o.store.UpdateJobStateIfCurrent(ctx, job, expectedState, store.StateRemoteQueued, "remote task queued awaiting active id")
	return err
}

func (o *Orchestrator) handleSubmitFailure(ctx context.Context, job *store.Job, err error) error {
	expectedState := job.State
	message := err.Error()
	job.ErrorMessage = &message
	job.UpdatedAt = time.Now().UTC()
	if torbox.IsRetryable(err) {
		job.RetryCount++
		nextRun := time.Now().UTC().Add(o.submitBackoff(job.RetryCount))
		job.NextRunAt = &nextRun
		o.log.Warn("submit failed, scheduling retry",
			"job_id", job.ID,
			"public_id", job.PublicID,
			"retry_count", job.RetryCount,
			"next_run_at", nextRun.Format(time.RFC3339Nano),
			"error", message,
		)
		_, updateErr := o.store.UpdateJobStateIfCurrent(ctx, job, expectedState, store.StateSubmitRetry, message)
		return updateErr
	}
	job.NextRunAt = nil
	o.log.Error("submit failed permanently", "job_id", job.ID, "public_id", job.PublicID, "error", message)
	_, updateErr := o.store.UpdateJobStateIfCurrent(ctx, job, expectedState, store.StateRemoteFailed, message)
	return updateErr
}

func (o *Orchestrator) submitBackoff(retryCount int) time.Duration {
	base := float64(o.cfg.Workers.SubmitRetryMin)
	maxDelay := float64(o.cfg.Workers.SubmitRetryMax)
	delay := base * math.Pow(2, float64(max(retryCount-1, 0)))
	if delay > maxDelay {
		delay = maxDelay
	}
	return withJitter(time.Duration(delay))
}
