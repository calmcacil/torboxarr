# Retry TorBox 521 Submission Failures

## Summary

TorBoxarr retries torrent and Usenet submissions when TorBox is temporarily
unreachable through Cloudflare and returns HTTP 521. Retries follow a bounded
outage schedule, avoid knowingly duplicating an accepted TorBox task, and end
in a clear Arr-visible failure with local submission artifacts cleaned up when
TorBox does not recover.

## Problem

Cloudflare returns HTTP 521 when it cannot connect to the TorBox origin. This
can happen during a TorBox outage or attack even though TorBoxarr, Sonarr, and
the local network remain healthy. Treating the first 521 as permanent makes
Sonarr report an immediate download error, while retrying forever leaves a dead
queue entry pending indefinitely. A create request may also have an uncertain
outcome, so retries must not knowingly create duplicate TorBox tasks.

## Goals

- Allow short and multi-hour TorBox origin outages to recover automatically.
- Bound how long an unavailable TorBox origin keeps an Arr download pending.
- Reconcile a possibly accepted submission before sending it again.
- Preserve a clear terminal failure for Sonarr and operators after exhaustion.
- Remove local submission artifacts that are no longer needed after terminal
  failure.

## Non-goals

- Applying the same fixed schedule to every TorBox, Cloudflare, network, or
  authentication error.
- Guaranteeing recovery when TorBox accepted a request but exposes no identity
  that TorBoxarr can safely match.
- Retrying authentication, malformed-input, account-policy, or other
  deterministic submission failures.
- Deleting an upstream task solely because submission retries were exhausted.
- Automatically asking Sonarr to search for a replacement release.

## Behavior

1. This behavior applies only when a torrent or Usenet create request receives
   an actual HTTP 521 response. Error text that merely contains `521` does not
   activate this policy.

2. A first HTTP 521 response does not make the job terminal. TorBoxarr keeps the
   job visible to its Arr-compatible client as pending and records that TorBox
   origin recovery is scheduled.

3. TorBoxarr permits five total create attempts for one submission lifecycle:
   the initial attempt followed by at most four retries.

4. The four retry delays are fixed and ordered as follows:
   - First retry: 15 minutes after the initial 521.
   - Second retry: 30 minutes after the preceding 521.
   - Third retry: 1 hour after the preceding 521.
   - Fourth retry: 3 hours after the preceding 521.

5. These delays are outage recovery windows, not minimums selected from the
   generic submission retry configuration. Routine worker scheduling may make
   an attempt occur slightly after its due time, but never before it.

6. A service restart or image upgrade preserves the current 521 attempt count
   and due time. Restarting TorBoxarr does not reset the sequence, skip a delay,
   or immediately consume another attempt.

7. Before every retry create request, TorBoxarr checks TorBox's applicable
   queued and active views for the same submission using available strong
   identity, including an exact torrent info hash or equivalent source-specific
   identity. Display-name similarity alone is never sufficient.

8. If reconciliation finds the matching task queued, TorBoxarr resumes tracking
   that existing queued task and does not send another create request.

9. If reconciliation finds the matching task active, TorBoxarr resumes tracking
   that existing active task and does not send another create request.

10. If every applicable reconciliation request succeeds and no matching task is
    found, TorBoxarr may send the scheduled create retry.

11. If reconciliation cannot complete reliably because TorBox is unavailable,
    rate limited, or returns an uncertain server response, TorBoxarr does not
    treat that result as confirmed absence and does not send a create request in
    the same worker run. It keeps the job pending and tries reconciliation again
    after the normal bounded worker delay without consuming a create attempt.

12. If TorBoxarr lacks sufficient strong identity to reconcile the submission,
    it may send the scheduled retry after the 521 delay. The log clearly records
    that duplicate avoidance was limited by missing upstream identity.

13. A successful retry follows the normal submission result:
    - A queued result becomes a tracked queued job.
    - An active result becomes a tracked active job.
    - Normal polling, optional force-start, local download, completion, and
      removal behavior resume.
    - The 521 failure message and retry schedule are cleared.

14. A non-521 response during a retry is handled by its normal policy. A
    successful response recovers the job; a deterministic permanent rejection
    becomes terminal; another independently retryable error uses its existing
    retry behavior and does not masquerade as another 521 attempt.

15. If the fifth total create attempt also returns HTTP 521, TorBoxarr stops
    retrying and changes the job to the existing terminal remote-failure state.
    No future submitter run retries that job automatically.

16. The terminal error states that TorBox remained unavailable through the
    complete 521 retry schedule. It includes the number of attempts and the
    final HTTP status without embedding API tokens, magnet URLs, NZB passwords,
    or raw source credentials.

17. The terminal job remains visible through the qBittorrent- or SAB-compatible
    interface as an error long enough for Sonarr to observe and process the
    failure according to its existing download-client behavior. TorBoxarr does
    not silently remove the diagnostic job record.

18. After terminal 521 exhaustion, TorBoxarr removes local artifacts created
    only to submit that job, including an uploaded torrent or NZB payload and an
    empty job-specific staging directory when present. It does not remove media
    from a completed directory and does not delete paths outside TorBoxarr's
    configured data roots.

19. Failure to remove a local submission artifact is logged as a separate local
    cleanup error. It does not hide or replace the terminal TorBox 521 failure,
    and unsafe paths are never deleted.

20. Terminal 521 exhaustion does not issue an upstream delete request. If
    reconciliation found an upstream task, the job would have resumed tracking
    it instead of exhausting; without a confirmed task, TorBoxarr does not guess
    which TorBox content to delete.

21. Removal requested by Sonarr or an operator takes precedence at every point.
    Once removal is requested, no new reconciliation-for-resubmission or create
    request begins. A late 521 response or successful create response cannot
    revive the removed job.

22. Concurrent submitter runs do not consume the same scheduled attempt twice
    or ordinarily issue duplicate create requests. A crash after TorBox accepts
    a request but before TorBoxarr persists the response remains inherently
    uncertain; subsequent retries first apply the reconciliation behavior above.

23. Logs distinguish:
    - the initial HTTP 521 outage response;
    - each scheduled retry and its due time;
    - reconciliation finding an existing queued or active task;
    - reconciliation postponing a retry because absence was not confirmed;
    - a create retry being sent;
    - recovery after a successful retry;
    - final retry exhaustion; and
    - local artifact cleanup success or failure.

24. Historical jobs already in terminal remote failure due to a recorded HTTP
    521 may be recovered once after deploying this behavior when they still
    retain a usable submission source and have not been removed. Their original
    failed attempt counts as the initial attempt, so recovery begins with the
    first 15-minute retry stage rather than granting a fresh five attempts.

25. Historical recovery does not blindly resubmit at startup. It applies the
    same identity reconciliation and scheduled timing rules as a live 521 job.
    A historical job without a usable source remains terminal and is logged as
    ineligible for automatic recovery.

26. Historical recovery is idempotent. Repeated restarts do not repeatedly
    reopen the same exhausted, removed, source-less, or already recovered job.

27. Jobs failed for reasons other than an actual HTTP 521 remain unchanged by
    this behavior, even if their message mentions Cloudflare, origin downtime,
    or a generic server error.
