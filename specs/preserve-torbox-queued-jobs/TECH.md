# Technical Design: Preserve TorBox Queued Jobs

## Scope

This change reconciles each tracked remote job against TorBox's queued and
active views before changing its local state. It covers torrent and Usenet
jobs, historical `remote_failed` records created by the old poll retry path,
and protection against an in-flight poll overwriting a removal request.

The existing SQLite schema is sufficient. Queue identifiers, hashes, and the
absence-check counter are stored in the existing columns and JSON metadata, so
no database migration is required.

## Identifier Semantics

TorBox uses different identifier fields in different responses:

- Create responses with an explicit active field (`torrent_id`, `download_id`,
  `usenetdownload_id`, or `usenet_id`) provide an active candidate.
- A generic create-response `id` is treated as a queued identifier unless an
  explicit active field is present.
- Scalar create responses retain request context: an `as_queued` request
  stores the scalar as `QueuedID`; otherwise it is treated as the create
  endpoint's active result.
- Queue responses parse generic `id` as `QueuedID` and never expose it as
  `RemoteID`.
- Active responses may parse generic `id` as `RemoteID` because the response
  came from an active endpoint.

The parser records whether the active candidate came from an explicit active
field. The submitter discards a duplicated `RemoteID`/`QueuedID` pair only
when that provenance is absent. Equality of two generic-looking values is not
enough to establish that the task is active, while an explicit active field is
preserved even if TorBox repeats the same value in a queue field.

Every queued or active match may additionally use the remote content hash.
When both an expected hash and a returned hash are present, a mismatch rejects
the candidate even if its identifier matches. This prevents an ambiguous ID
from associating a job with different content.

## TorBox Client

`Client` exposes `FindQueuedTask`, in addition to the existing compatibility
method `GetQueuedStatus`. `FindQueuedTask` accepts a queue ID, a Usenet queue
auth ID, and/or remote hash.

Queued lookup sequence:

1. Request `/api/queued/getqueued` with the queue ID when applicable.
2. For Usenet, use the normalized `usenet` queue type and match the queue auth
   identifier because TorBox does not accept a queue ID query for that view.
3. If the ID-filtered request fails and an ID exists, retry the unfiltered
   queue list. A match from the fallback is accepted, but if it finds no
   match, the original lookup error is retained so the worker does not count
   the result as confirmed absence.
4. If an ID-filtered request succeeds without a match and a hash exists, retry
   the unfiltered queue list and match by hash.
5. Return a parsed queued status, or `(nil, nil)` when the view was checked
   successfully and no matching item exists.

Active lookup uses the source-specific `mylist` endpoint. It first tries the
remote ID. If that request fails and an ID is available, it retries the full
active list and matches by the ID or another identity (hash or Usenet queue auth
ID). The full active list may confirm the same ID because the response came
from the active endpoint. If that full-list fallback succeeds without a match,
the original lookup error is retained and the worker does not count the result
as proof of absence.

HTTP, transport, authentication, rate-limit, and API-envelope errors are
returned to the worker as errors. The worker treats all status-view errors as
non-confirmation; no error is interpreted as proof of absence.

## Poll State Machine

`processPollJob` dispatches by local state.

### `remote_queued`

1. Check the queued view.
2. If a matching item exists, clear the absence counter and keep the job
   queued.
3. Otherwise check the active view.
4. An active match is the only way to promote the job to `remote_active`.
5. If either lookup failed, schedule another poll without incrementing the
   absence counter.
6. If both views completed successfully without a match, increment the
   consecutive absence counter. After the configured limit, transition to
   `remote_failed` with an explicit queue-or-active absence message.

### `remote_active`

1. Check the active view using the active ID and available content identities.
2. If the lookup fails or returns no match and queue tracking exists, check the
   queue view.
3. A queue match demotes the job to `remote_queued`, clears stale active errors,
   removes the stale active ID, and resets the absence counter.
4. Any lookup error schedules another poll and does not count as absence.
5. Only successful absence from every applicable view increments the counter.

Jobs created directly as active without queue identity retain the existing
active-only behavior: a successful active no-match schedules another poll and
does not enter queue-aware absence accounting.

Successful queue or active matches reset the counter. A ready active task
continues through the existing local download pipeline. A remote task that
explicitly reports a terminal failure remains eligible for the existing
terminal failure transition; the ambiguity protection applies to lookup
failures and missing-view matches.

The default absence limit is five checks and can be overridden with
`TORBOXARR_REMOTE_ABSENCE_ATTEMPTS`. A valid queued item resets the counter, so
queue duration alone cannot fail a job.

## Startup Recovery

Startup first scans `remote_failed` jobs that:

- retain a queue ID or remote hash; and
- have one of the known old/new confirmed-absence failure messages.

This message filter prevents unrelated upstream terminal failures from being
reopened. Deleted jobs are skipped.

For each candidate, startup checks the queue first. A confirmed queued match
reopens it as `remote_queued`. If the queue view succeeds with no match, the
active view is checked; a confirmed active match reopens it as
`remote_active`. Lookup errors or no matches leave the historical failure
unchanged. No submission is attempted.

An active response already marked terminal (`Failed` or `Inactive`) is not a
recoverable active confirmation and leaves the historical failure unchanged.

Recovery uses a conditional state update from `remote_failed` and checks the
delete flag in the update predicate. Repeated startups therefore do not create
duplicate recovery events or revive a job that was concurrently marked for
removal.

## Concurrent Removal

Poll persistence uses conditional store methods:

- `UpdateJobIfState` persists non-transition poll updates only while the job is
  still in the expected state and `delete_requested = 0`.
- `UpdateJobStateIfCurrent` applies promotions, demotions, ready transitions,
  and confirmed-absence failures under the same predicate.

An Arr removal changes the job to `remove_pending` and sets the delete flag.
Any stale poll then affects zero rows and cannot revive or fail the removed
job.

Submission completion and failure persistence use the same conditional state
predicate. If removal arrives while a TorBox create request is in flight, its
response cannot move the job from `remove_pending` back into remote tracking or
schedule another submission retry.

## Validation

Regression coverage includes:

- generic queued IDs remaining queued;
- queue-to-active promotion only after an active match;
- active-to-queued recovery;
- non-terminal lookup errors;
- bounded confirmed absence;
- startup recovery and unrelated-failure preservation;
- queued and active full-list fallback after ID lookup errors;
- conditional update rejection after removal; and
- configured absence-attempt parsing.

Validation commands:

```text
git diff --check
go vet ./...
go test -count=1 ./...
go test -race ./...
```
