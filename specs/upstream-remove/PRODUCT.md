# Remove Jobs From TorBox

## Summary

TorBoxarr can remove a job's corresponding active or queued task from TorBox
when an operator removes the job through an Arr-compatible interface. Upstream
removal is opt-in. When enabled, TorBoxarr reconciles all strongly identified
upstream representations before deleting local payloads, while bounding
retries so TorBox outages cannot leave local removal pending indefinitely.

## Problem

Removing a job locally without removing it from TorBox can leave unwanted
active, queued, or cached tasks in the operator's TorBox account. Conversely,
making local removal depend indefinitely on TorBox availability can leave stale
files and jobs in TorBoxarr. Queue-to-active transitions also mean a job can
retain identifiers for more than one TorBox view, so deleting only one stored
identifier can leave the same task visible elsewhere.

## Goals

- Make upstream TorBox cleanup an explicit operator-controlled policy.
- Remove every active or queued representation that can be identified safely.
- Treat already-absent tasks as successful cleanup.
- Retry transient and uncertain failures without repeating successful work.
- Ensure TorBox failures cannot block local cleanup indefinitely.
- Preserve enough outcome information for operators to distinguish confirmed
  upstream cleanup from local-only cleanup.

## Non-goals

- Deleting TorBox tasks when upstream removal is disabled.
- Deleting tasks using display-name similarity or another ambiguous match.
- Guaranteeing cleanup when TorBox is unavailable or definitively rejects it.
- Retaining removal records beyond the existing removed-job retention period.
- Changing TorBox account-wide retention or cache policy.
- Adding a separate Arr-visible removal-failure state.

## Behavior

1. `TORBOXARR_UPSTREAM_REMOVE` controls upstream cleanup and defaults to
   disabled.

2. When upstream cleanup is disabled, removal never calls a TorBox lookup,
   active-task control, or queued-task control endpoint. TorBoxarr removes local
   payloads and records that TorBox content was retained.

3. When upstream cleanup is enabled, TorBoxarr changes the job to
   `remove_pending`. Removal takes precedence over submission, polling,
   force-start, downloading, verification, and promotion. No later worker
   result may revive the job.

4. Before its first delete request, the remover reconciles the applicable
   active and queued TorBox views using the job's source-specific identifiers
   and content identity.

5. A match may use a stored active ID, queue ID, Usenet queue authentication
   ID, or exact content hash. A candidate with a conflicting hash is rejected.
   Display-name similarity alone is never sufficient.

6. A fresh lookup may supply a current active or queue ID that was not stored
   on the job. TorBoxarr may delete that representation only when strong,
   source-specific identity confirms that it belongs to the removed job.

7. TorBoxarr reconciles active and queued views independently. If both contain
   a strongly matched representation, both are cleanup targets. A stale local
   identifier must not cause TorBoxarr to ignore a currently matched
   representation in the other view.

8. A confirmed active torrent is deleted through TorBox's torrent control
   operation. A confirmed active Usenet download is deleted through TorBox's
   Usenet control operation.

9. A confirmed queued torrent or Usenet item with a concrete queue ID is
   deleted through TorBox's single-item queued control operation. TorBoxarr
   never uses an account-wide `all` operation.

10. If every applicable upstream lookup succeeds and finds no matching active
    or queued representation, upstream cleanup is complete. Already-absent
    content is an idempotent success and does not generate a warning.

11. If the job has content identity but no safe control ID, and reconciliation
    cannot obtain one, TorBoxarr does not delete by fuzzy matching. It proceeds
    with local cleanup and records a warning that upstream cleanup could not be
    attempted safely.

12. Successful cleanup of one upstream representation is persisted. If another
    representation fails, later remover runs retry only the unfinished work and
    do not repeat a successful delete.

13. Active and queued cleanup have separate attempt budgets. Each unfinished
    representation receives at most five delete attempts.

14. A timeout, connection failure, rate limit, TorBox server error, or other
    uncertain/retryable result consumes one attempt because the request may
    have reached TorBox.

15. After an uncertain result, the next remover run reconciles that view before
    issuing another delete. If the representation is now absent, cleanup is
    successful. A repeated delete is sent only if the same representation is
    still confirmed present.

16. Retryable cleanup uses the existing remover schedule and does not spin in a
    tight loop. Retry progress and completed representations survive service
    restarts.

17. After the fifth unsuccessful attempt for a representation, TorBoxarr stops
    retrying it and proceeds with local cleanup. The job does not remain
    `remove_pending` indefinitely.

18. A definitive non-retryable rejection is not retried. TorBoxarr proceeds
    with local cleanup immediately and records the rejected upstream outcome.

19. Local payload deletion begins only after every applicable upstream
    representation is successful, already absent, definitively rejected,
    unsafe to identify, or has exhausted its attempt budget.

20. Safe-path validation remains authoritative. TorBoxarr never removes a local
    path outside its configured completed, staging, or payload roots. An unsafe
    local path fails removal even if upstream cleanup succeeded.

21. Successful local cleanup ends in the existing `removed` state. No separate
    removal-failure state is introduced for incomplete upstream cleanup.

22. A fully successful or already-absent upstream outcome records that both
    local and TorBox cleanup completed.

23. Retry exhaustion, definitive rejection, or missing safe identity records
    the job as removed with an operator-visible warning that identifies which
    upstream representation was not confirmed removed and why.

24. The upstream outcome and warning remain on the removed job for the normal
    removed-job retention period and are pruned with that job under the existing
    retention policy.

25. Concurrent remover workers must not consume duplicate attempt budgets or
    issue ordinary duplicate deletes for the same job. A crash after an API
    request but before persistence may leave the result uncertain; subsequent
    reconciliation prevents another delete when TorBox shows the item absent.

26. Logs distinguish disabled/local-only cleanup, reconciliation results,
    active and queued delete attempts, successful or already-absent cleanup,
    retry scheduling, retry exhaustion, definitive rejection, missing safe
    identity, local cleanup, and the final removal outcome.

27. Logs identify the local job, source type, upstream view, and relevant
    control ID without exposing API tokens, source URLs, NZB passwords, magnet
    query secrets, or other credentials.

## Acceptance Criteria

- Disabled upstream removal performs no TorBox requests.
- Enabled removal deletes matched active torrent and Usenet tasks.
- Enabled removal deletes matched queued torrent and Usenet entries.
- A job represented in both views cleans both views independently.
- Already-absent content completes without a warning.
- Successful partial cleanup is not repeated while the remaining view retries.
- Each view stops after five retryable or uncertain delete attempts.
- An uncertain result is reconciled before another delete.
- A definitive rejection or missing safe ID proceeds to local cleanup with a
  persisted warning.
- Local payloads are retained while an upstream retry remains available.
- Completed removal uses `removed`, and normal retention applies to its outcome.
