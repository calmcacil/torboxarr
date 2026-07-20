# TorBoxarr — Upstream-Delete Hardening (living design doc)

Local-only planning doc (gitignored, `*.local.md`). Tracks the upstream-delete
feature work and its follow-up hardening. Companion to `watchdir.local.md`.

## Feature: delete upstream TorBox task on *arr removal

`feat/upstream-remove` branch (fork-only, "keep on our end" per user). When an
*arr sends `torrents/delete`, TorBoxarr deletes the upstream TorBox task before
local cleanup, gated by `TORBOXARR_UPSTREAM_REMOVE` (default false). The only
`DeleteTask` call site is `processRemoveJob` (`internal/worker/remove.go`),
reached only via `markRemovePending` (an *arr delete). No delete at finalize.
Numeric task IDs sent as JSON numbers (`DeleteTask` parses via `strconv.ParseInt`).
Tests in `internal/worker/remove_test.go` cover torrent + usenet, disabled,
queued-only, concurrent claims, and non-retryable failure.

## Follow-up: cap retryable upstream-delete failures (DONE on dev, 1.0.8-calm)

### Problem observed (2026-07-20, teostra)
During a TorBox API `DATABASE_ERROR` 500 outage, jobs stuck in `remove_pending`
forever: each `processRemoveJob` call hit the 500, which `http_client.go`
classifies as retryable (any `>= 500`), so `remove.go` returned the error and
the job retried every 5s indefinitely. The local files were removed each
attempt but the DB row never reached `StateRemoved`, and the *arr-side entry
lingered.

User insight: the 500 may also mean **the task was already deleted** (by a
prior successful call, or manually) — `controltorrent` delete on a gone task
returns the same `DATABASE_ERROR`. In that case the goal (task gone) is already
met, so retrying is wasted effort. BUT TorBox's 500 `DATABASE_ERROR` does NOT
distinguish "not found" from a real DB error, so we cannot reliably detect
already-deleted and treat it as success. The 5-attempt cap is the pragmatic
guard either way.

### Fix
- Added `UpstreamDeleteAttempts int` to `SubmissionMetadata` (persisted via the
  existing `metadata_json` column — no schema migration).
- `processRemoveJob` (`internal/worker/remove.go`): on a retryable
  `DeleteTask` error, increment the counter; if `< 5` → log "temporarily
  failed, will retry" and return the error (retry). If `>= 5` → log
  "failed after max attempts, proceeding with local cleanup" and fall through
  to local cleanup (job finalizes as `StateRemoved`, "torbox content retained").
  The counter is persisted via `store.UpdateJob` before returning the retry, so
  it survives across remover ticks.
- `maxUpstreamDeleteAttempts = 5` constant in `remove.go`.
- Test: `TestProcessRemoveJob_UpstreamDeleteRetriesThenEscalates` — 5 retryable
  calls → attempts 1-4 return error, attempt 5 escalates to `StateRemoved`.

### Deployed
`1.0.8-calm` (+ `:latest`) on teostra. Verified: wedged job `e77601b4`
(remote_id 58970510) escalated after 5 attempts and finalized as
`StateRemoved` ("torbox content retained"). TorBox still returned 500 the whole
time, so the task may or may not still exist upstream — acceptable; local state
is consistent and the job is no longer stuck.

## TODO / open questions

1. **Port to `feat/upstream-remove`.** The max-attempts hardening currently
   lives only on `dev` (commit `682cab5`, 1.0.8-calm). When we next sync the
   feature branch, cherry-pick it onto `feat/upstream-remove` so the upstream
   PR candidate includes the resilience fix. (The feature branch is the
   upstream-PR candidate; `dev` is the integration/testing branch.)
2. **Already-deleted detection.** If TorBox ever returns a distinguishable
   code for "task not found" (e.g. 404 or a specific error string), treat it as
   success instead of counting toward the 5 attempts. Monitor API behavior.
3. **Backoff.** Currently the remover retries every `RemoveInterval` (5s) with
   no extra backoff. A longer backoff for upstream-delete retries could reduce
   API load during a sustained outage. Optional.
4. **Visibility.** After escalation, the job is `StateRemoved` with the
   upstream task possibly still present. A future reconciliation scan (compare
   local `removed` jobs' remote_ids against TorBox `mylist`) could clean
   stragglers. Out of scope for now.

## References (in-repo)

- `internal/worker/remove.go` — `processRemoveJob`, `maxUpstreamDeleteAttempts`.
- `internal/store/model.go` — `SubmissionMetadata.UpstreamDeleteAttempts`.
- `internal/torbox/client.go` / `http_client.go` — `RetryableError`, the
  `>= 500 → retryable` classification.
- `internal/worker/remove_test.go` — escalation test.
- `apps/torboxarr.yml` (compose repo) — `TORBOXARR_UPSTREAM_REMOVE` env.
