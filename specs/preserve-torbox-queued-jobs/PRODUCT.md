# Preserve TorBox Queued Jobs

## Summary

TorBoxarr must continue tracking downloads that TorBox has accepted into its
queue but has not yet promoted to active downloads. A queued download must not
be reported as failed merely because its queue identifier cannot be used to
retrieve it from TorBox's active-download list.

## Problem

TorBox may return the same generic identifier shape for queued and active
downloads. A queued identifier can produce an error when queried through the
active-download endpoint even though the download remains visible in the
TorBox queue. Treating that error as proof that the download is missing causes
TorBoxarr to stop tracking a valid job and report a terminal failure to the
connected Arr application.

## Goals

- Keep valid TorBox queue entries pending for as long as TorBox reports them as
  queued.
- Promote a job only when TorBox reports that it is active.
- Avoid terminal failure while TorBox's available views disagree or cannot be
  checked reliably.
- Recover previously misclassified jobs when TorBox still confirms that they
  are queued.
- Preserve terminal failure for jobs that are confirmed to be unrecoverable.

## Non-goals

- Changing TorBox's queue order, priority, or scheduling behavior.
- Setting a maximum amount of time that a valid TorBox entry may remain queued.
- Automatically resubmitting a download that is absent from TorBox.
- Reopening failed jobs that TorBox does not currently confirm as queued or
  active.
- Changing local download, verification, import, or removal behavior after a
  TorBox download becomes ready.

## Behavior

1. When TorBox accepts a torrent or Usenet submission as queued, TorBoxarr
   reports the job as pending or queued through its Arr-compatible interfaces.
   The job is not reported as actively downloading until TorBox confirms that
   it has become active.

2. A generic identifier returned with a queued item identifies that queue
   entry. The mere presence of an identifier does not prove that an active
   TorBox download exists, even when the identifier resembles an active
   download ID.

3. While TorBox continues to return the matching queue entry, TorBoxarr keeps
   the job queued and schedules another status check. This remains true for an
   indefinitely queued job; elapsed queue time alone does not cause failure.

4. TorBoxarr matches a queued job using the strongest identifiers TorBox makes
   available, including its queue identifier and content identity. A match
   must not be based only on display-name similarity.

5. When TorBox no longer returns the matching queue entry, TorBoxarr checks
   whether the download has moved to TorBox's active-download list before
   deciding that it is missing.

6. When TorBox confirms a matching active download, TorBoxarr promotes the job
   from queued to active, retains the identifiers needed for subsequent status
   checks, and continues the normal progress and download-ready flow.

7. Promotion must tolerate the queue and active views changing between status
   checks. If an entry disappears from the queue immediately before appearing
   in the active list, TorBoxarr keeps the job pending and checks again rather
   than treating that short transition window as a failure.

8. If a job believed to be active cannot be retrieved from the active-download
   view but still has queue tracking information, TorBoxarr checks the queue
   view before recording an unsuccessful poll against that job.

9. If that fallback check finds the job in the queue, TorBoxarr returns the job
   to queued status, clears any stale active-lookup error, and continues
   tracking it. Connected Arr applications continue to see a pending download,
   not a failed download.

10. A failure from an active-download lookup, including TorBox's
    `DATABASE_ERROR` response for a queue-only identifier, is not by itself
    proof that the job is missing or unrecoverable.

11. A timeout, connection failure, rate limit, authentication failure, or
    TorBox server error from either status view does not establish absence. The
    job remains non-terminal and is checked again after the normal retry delay.
    The reported error may describe the temporary lookup problem, but it must
    not claim that the task is missing.

12. TorBoxarr must not create a duplicate TorBox submission while reconciling a
    queued or ambiguously transitioning job. Recovery consists only of finding
    and continuing to track the existing TorBox entry.

13. A job may become terminally failed only after TorBoxarr has successfully
    checked every applicable TorBox view and has not found a matching queued or
    active entry for the configured bounded number of checks. Failed requests
    do not count as successful absence confirmations.

14. A terminal failure caused by confirmed absence clearly states that the job
    was not found in either the queue or active-download view. It must not use
    wording that conflates confirmed absence with TorBox being unavailable.

15. Jobs without queue tracking information retain their normal active-status
    behavior. The feature does not require an inapplicable queue lookup for a
    job that was created directly as active and has no queue identity or other
    queue-matching information.

16. The behavior is the same for torrent and Usenet jobs wherever TorBox
    exposes equivalent queued and active views. Source-specific identifiers
    remain associated with the correct source and are not matched across
    torrent and Usenet queues.

17. If an operator requests removal while a job is queued or transitioning,
    removal takes precedence over further status recovery. TorBoxarr does not
    revive the job merely because it remains visible in a later queue check.

18. Concurrent worker runs or a service restart must not produce duplicate
    promotion, recovery, or terminal-failure events. After restart, the job
    resumes from the latest confirmed queued or active status.

19. On upgrade, TorBoxarr rechecks previously terminal `remote_failed` jobs
    that retain sufficient queue tracking information and whose failure may
    have resulted from the queue/active ambiguity described by this feature.

20. An upgrade recheck reopens a failed job only when TorBox currently confirms
    a matching queued or active entry. A confirmed queued match becomes queued;
    a confirmed active match becomes active. Its stale failure message and
    unsuccessful status-check count are cleared, and normal polling resumes.

21. If TorBox does not find a matching entry during the upgrade recheck, or if
    TorBox cannot be queried reliably, the historical failed job remains
    unchanged. Upgrade recovery must not turn an uncertain lookup into a new
    active job or resubmit content.

22. Upgrade recovery is idempotent. Repeated restarts or deployments leave an
    already recovered job in its current valid state and do not repeatedly
    reopen jobs that TorBox does not confirm.

23. Existing jobs that are completed, removed, locally failed, or explicitly
    deleted are not reopened by this feature, even if similarly named content
    exists in TorBox.

24. Normal queue visibility remains stable throughout recovery: queued jobs
    remain visible to connected Arr applications, active jobs continue showing
    progress, and only confirmed terminal failures disappear from the active
    queue according to existing compatibility behavior.
