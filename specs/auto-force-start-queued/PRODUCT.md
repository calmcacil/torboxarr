# Automatically Force-Start Queued TorBox Jobs

## Summary

When explicitly enabled, TorBoxarr automatically asks TorBox to start a tracked
download that has remained confirmed in TorBox's queue for a configured
duration. This gives operators an opt-in fallback for jobs that wait
indefinitely for TorBox's periodic queue processor while retaining the existing
queued-job reconciliation and duplicate-submission protections.

## Problem

TorBox may keep accepted downloads queued until a background process assigns
them to an available download slot. TorBox advises that this process may run
about every three hours and exposes a separate "Force Start" operation for
operators who need a queued download to start sooner. Without an automatic
fallback, TorBoxarr can correctly preserve a queued job but cannot move it
forward without an operator using the TorBox Web UI.

## Goals

- Automatically request force-start for TorBox jobs that remain genuinely
  queued beyond a configurable duration.
- Avoid repeated successful force-start requests for the same queued lifecycle.
- Retry requests whose outcome is failed or uncertain without failing,
  duplicating, or losing the tracked job.
- Preserve the existing queue-to-active reconciliation as the authority for
  whether TorBox has actually started the download.
- Make automatic behavior observable and controllable by operators.

## Non-goals

- Guaranteeing that TorBox starts a download after accepting the request.
- Bypassing TorBox account limits, download-slot limits, capacity checks, or
  content validation.
- Resubmitting the torrent, NZB, magnet, or URL as a new TorBox task.
- Changing TorBox queue priority or the relative order of queued downloads.
- Providing a Web UI or a manual force-start command in this feature.
- Force-starting jobs that TorBoxarr cannot currently confirm as queued.
- Replacing normal queued and active status polling.

## Behavior

1. Automatic force-start is disabled by default. TorBoxarr sends no force-start
   requests unless the operator configures a positive eligibility duration.

2. A positive configured duration enables automatic force-start. A tracked job
   becomes eligible after it has remained in TorBox's queue for that duration
   without TorBoxarr confirming that it became active. A value such as `3h`
   enables a three-hour policy.

3. An unset or zero duration disables automatic force-start. When disabled,
   TorBoxarr preserves and polls queued jobs exactly as before and never sends a
   force-start request.

4. Eligibility is based on time spent in the current tracked queued lifecycle,
   not on the age of the media release, torrent, NZB, Arr history item, or local
   database file.

5. For a job submitted by the current TorBoxarr installation, queue age begins
   when TorBoxarr first records that TorBox accepted the job as queued.

6. For a historical job recovered from `remote_failed`, queue age begins from
   the earliest reliable time TorBoxarr can establish that the existing task
   entered its current queued lifecycle. If no reliable earlier time is
   available, recovery time is used. An upgrade must not immediately
   force-start a historical job merely because its local job record is old.

7. If a job believed to be active is later confirmed back in TorBox's queue,
   that confirmation starts a new queued lifecycle unless TorBoxarr can
   determine it is the uninterrupted lifecycle for which force-start was
   already requested.

8. A job is eligible only while all of the following are true:
   - TorBoxarr currently tracks it as remotely queued.
   - A successful status check currently confirms a matching TorBox queue
     entry.
   - TorBoxarr has the queue identifier required by TorBox's force-start API.
   - The configured queue duration has elapsed.
   - Removal has not been requested.
   - TorBoxarr has not already received a successful force-start response for
     the same queued lifecycle.

9. Matching by hash or another content identity may confirm that a job remains
   queued, but TorBoxarr does not issue force-start until it has the concrete
   queue identifier required by TorBox. Missing control identity is not treated
   as failure or absence; the job remains queued and continues normal polling.

10. Eligibility applies to torrent and Usenet jobs wherever TorBox supports
    force-start through its queued-download control API. Source types for which
    TorBox does not support the operation remain queued without force-start and
    continue normal polling.

11. When an eligible job is successfully confirmed in the queue, TorBoxarr
    sends TorBox one force-start request for that specific queue entry. It does
    not send an "all" operation and does not affect other queued downloads.

12. A successful force-start response means only that TorBox accepted the
    control request. TorBoxarr keeps the job queued until a later active-view
    check confirms an active task. It does not fabricate an active identifier,
    report synthetic progress, or start local downloading based only on the
    control response.

13. After TorBox accepts a force-start request, TorBoxarr does not send another
    force-start request for the same queued lifecycle, even if:
    - the queue entry remains visible for additional polls;
    - TorBox takes time to allocate a slot;
    - TorBoxarr restarts; or
    - another TorBoxarr worker or instance observes the job concurrently.

14. Normal queue-to-active reconciliation continues after a successful request.
    When TorBox confirms the job as active, TorBoxarr promotes it and follows
    the existing progress, download-ready, local transfer, verification, and
    completion behavior.

15. If TorBox successfully reports that the queue entry disappeared but does
    not yet report an active match, TorBoxarr applies the existing transition
    window and confirmed-absence behavior. It does not resubmit the content or
    repeat an already accepted force-start request merely because the views are
    temporarily inconsistent.

16. A timeout, connection failure, rate limit, authentication failure, TorBox
    server error, or unsuccessful TorBox response does not establish whether
    force-start was applied. The job remains queued and non-terminal.

17. After a failed or uncertain force-start request, TorBoxarr retries only
    while a fresh successful queue lookup continues to confirm the same queue
    entry. It does not repeatedly send control requests while the queue view is
    unavailable or ambiguous.

18. Failed or uncertain requests are retried using bounded delay rather than a
    tight loop. Retries must respect TorBox API rate limits and must not prevent
    normal status polling for this or other jobs.

19. There is no maximum retry count that converts force-start failure into a
    terminal job failure. Force-start is an acceleration request, not a
    requirement for correctness; a job that remains confirmed queued remains a
    valid queued job.

20. A definitive TorBox response that rejects force-start because the account
    has no available slot, the item does not pass TorBox checks, or the
    operation is temporarily unavailable leaves the job queued. TorBoxarr
    records an operator-visible warning and may retry while the same queue
    entry remains eligible.

21. If a future supported source type indicates that the operation is
    unsupported, TorBoxarr stops automatic force-start attempts for that queued
    lifecycle and continues normal polling. For the currently supported torrent
    and Usenet source types, an unclassified control rejection remains a
    non-terminal retryable outcome; TorBoxarr must not infer permanent
    unsupported status from unstable error text.

22. A successful queue match before the configured duration only preserves the
    queued job and schedules normal polling. TorBoxarr must not send force-start
    early because of repeated polls, prior lookup failures, service restarts,
    or an old local job creation timestamp.

23. Queue lookup failures do not advance, reset, or independently satisfy the
    eligibility duration. They also do not trigger force-start. Once queue
    visibility returns, eligibility is evaluated using the same queued
    lifecycle timing information.

24. A confirmed active match always takes precedence over force-start. If the
    job becomes active while a force-start decision or request is in flight,
    TorBoxarr preserves the active state and does not return the job to queued.

25. Removal always takes precedence over force-start. Once removal is
    requested, TorBoxarr does not initiate a new force-start request. A late
    force-start response cannot clear the removal request, restore queued state,
    promote the job, or prevent normal removal processing.

26. If removal and force-start are initiated concurrently, TorBoxarr may be
    unable to prevent a request already delivered to TorBox, but local removal
    remains authoritative. The job is not revived, and existing upstream
    removal behavior remains responsible for remote cleanup where applicable.

27. In the supported single-service deployment, worker claims and persisted
    lifecycle metadata suppress ordinary duplicate force-start requests across
    overlapping worker runs and service restarts. Multiple TorBoxarr processes
    sharing one database are outside this guarantee.

28. If TorBoxarr crashes after sending the request but before recording the
    outcome, the result is uncertain. On restart, TorBoxarr first reconciles the
    queue and active views. If the same entry remains queued, it may retry after
    the normal retry delay; if active, removed, or absent, it does not retry.
    Exactly-once delivery across this crash window is not guaranteed.

29. Force-start tracking survives service restarts and image upgrades. A
    restart must not reset a job's queue age, forget an accepted request, or
    immediately generate duplicate requests.

30. A job that completes, fails with a confirmed terminal remote status,
    becomes confirmed absent, or is removed is no longer eligible. Automatic
    force-start must not reopen any terminal or local-processing state.

31. A job that is resubmitted as a genuinely new TorBox queue entry has a new
    queued lifecycle. Prior force-start history from an older removed or failed
    entry does not suppress force-start for the new entry.

32. The Arr-compatible queue remains stable throughout this behavior. Eligible
    and force-start-requested jobs continue to appear as pending/downloading in
    the same way as other remote-queued jobs until TorBox confirms a different
    state.

33. Automatic force-start does not alter torrent names, categories, save paths,
    tags, hashes, queue identifiers, active identifiers, or submission
    fingerprints exposed through existing Arr-compatible interfaces.

34. Operators can distinguish at least these actionable events in logs:
    - a queued job became eligible;
    - a force-start request was accepted by TorBox;
    - a force-start request failed or had an uncertain outcome;
    - a retry was scheduled;
    - an eligible force-start was skipped because the operation is unsupported,
      lacks a usable queue ID, or removal took precedence; and
    - an accepted force-start was followed by active promotion.

35. Logs identify the local job and TorBox queue entry sufficiently for
    diagnosis but never expose TorBox API tokens, Arr credentials, magnet query
    secrets, NZB passwords, or other configured secrets.

36. Force-start lifecycle timestamps are persisted in job metadata and
    actionable transitions are emitted as structured logs. Operators can use
    these existing database and log diagnostics to determine whether a job is
    waiting for the threshold, waiting for a retry, or has already had a request
    accepted. No public endpoint or new Arr-visible state is required.

37. Disabled, below-threshold, retry-throttled, and already-accepted jobs do not
    emit repetitive informational logs on every queue poll. These routine
    policy decisions may be available at debug level, but only actionable skips
    are required in normal logs.

38. The configurable duration accepts a documented duration value. Invalid,
    negative, or otherwise unusable values cause startup configuration
    validation to fail with an actionable error rather than silently selecting
    a different policy.

39. A zero duration is an explicit configuration for disabling automatic
    force-start. Disabled behavior is distinct from a very short positive
    duration and must not result in immediate requests.

40. An unset duration is also disabled. Existing installations do not begin
    sending state-changing force-start requests merely because they upgrade.

41. Changing the configured duration on restart re-evaluates currently queued
    jobs against their existing queued-lifecycle age. Shortening the duration
    may make jobs eligible at the next confirmed queue poll; lengthening it
    delays jobs that have not yet received an accepted request. It never causes
    a successful request to be repeated.
