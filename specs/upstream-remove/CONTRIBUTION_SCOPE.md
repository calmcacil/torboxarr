# Contribution scope and deployment fix inventory

Compared on 2026-10-13:

- MrJoiny/TorBoxarr upstream main: `72343d0`.
- Personal deployment/testing branch dev: `642595d`.
- Rebuilt upstream-remove contribution: `f8277bf` on
  `codex/feat/upstream-remove`.

The user reports that the connectivity and recovery fixes resolved genuine
failures on the personal deployment. That operational evidence is the reason
to preserve these changes, not a reason to combine unrelated features into
one upstream PR. No live deployment reproduction was performed in this review.

## Contribution decision

Use two contribution topics: download reliability and opt-in upstream removal.
Keep feature-specific CLI and force-start work with those features. A fix can
be necessary for the personal deployment without being a dependency of the
upstream removal operation itself.

The rebuilt upstream-remove branch is not a complete replacement for the dev
container. Its automated deletion tests passing does not prove that all of the
personal deployment's queue, submission and transfer failures are covered.
Keep dev as the integrated testing branch until the selected contributions are
combined and validated together.

## Fix ownership and current coverage

| Work and source | Problem / behavior | Contribution | Rebuilt removal branch |
| --- | --- | --- | --- |
| `01c19aa`, preserve queued jobs, and later queue corrections | Accepted queue IDs must not be used as proof of active status; queue/active gaps and failed lookups must not permanently lose accepted jobs. Follow confirmed queues indefinitely and recover previously misclassified jobs on startup. | Reliability PR; reuse shared lookup/state guards in removal. | Lookup helpers exist, but normal submit/poll classification and historical queue recovery are NOT ported. |
| `8c546c5`, preserve inconclusive lookup errors | A failed ID lookup followed by an inconclusive empty fallback must not become confirmed absence. | Shared correctness required by removal; also relevant to normal tracking. | Included, with regression tests. |
| Removal reconciliation and race work: `d923df6`, `cbecb31`, `d218cf0`, `31bfd72`, `8ff30c8` | Match active/queued identities safely, retain independent cleanup progress, bound failures, preserve late task IDs/paths and prevent stale workers from reviving removal. | Upstream-remove PR for issue #3. | Included selectively; unrelated feature history excluded. |
| Review fixes in `f8277bf` | Queue promotion during lookup/delete; conflicting stale ID fallback; full-list pagination; typed absence; expired remover claims; cancel stalled transfers on client removal; preserve ordinary structured-5xx retries. | Required removal/shared correctness safeguards. | Included and tested. |
| `642595d`, stalled download and link recovery | Cancel after two minutes without bytes, including stalled headers/body; preserve partials; refresh links for HTTP 400/403/404, 5xx, and idle timeout; reset failure budget on actual progress. | Reliability PR; applies even when upstream removal is disabled. | NOT included. Removal-triggered cancellation is not automatic idle recovery. |
| `22b0b4a`, HTTP 521 recovery | Persist five create attempts with 15m, 30m, 1h, 3h delays; reconcile uncertain queued/active acceptance before retry; recover eligible historical 521 failures at startup; retain Arr-visible failure on exhaustion and clean input artifacts. | Reliability PR with explicit submission policy review. | Normal transient-5xx retry behavior is preserved; special 521 schedule, persistence and startup recovery are NOT included. |
| Older poll caps and reset-on-recovery fixes: `11a65eb`, `f51e0d8`, duplicated `b7e7491`/`dceff2f` | Consecutive failure accounting must reset after recovery. Later dev queue work counts confirmed absence rather than treating lookup errors as missing tasks. | Consolidate into the final reliability behavior and tests; do not cherry-pick duplicate or obsolete policies blindly. | Normal polling remains upstream's implementation; removal has separate bounded budgets. |
| `b185f68`, permanently rejected submission cleanup | Removes stranded local input artifacts, but also removes the Arr-visible failed job. | Separate behavior decision. Prefer retaining failure visibility while safely cleaning artifacts if Sonarr validation supports it. | NOT included; upstream failure visibility retained. |
| CLI redirect/credential fixes, including `3f2a2b1` | Protect manual input fetching and credential handling. | Manual-add feature PR. | NOT included; no manual-add CLI introduced. |
| Force-start work and `9d8ec4d`/`1cbdda7` requeue guards | Optional request to start aged confirmed queue entries, with lifecycle/race protection. Provider queue delay is distinct from lost tracking or a disconnected transfer. | Force-start feature PR; depends on correct queue tracking. | NOT included; no automatic upstream start operation introduced. |

Upstream PR #7's name handling must survive all contributions: keep magnet
`dn`/Arr titles and only replace placeholders with decoded TorBox names.
The old dev branch predates this fix; use targeted ports rather than copying
dev's complete submit/poll/API files over current upstream.

## Proposed reliability PR

Suggested branch: `codex/fix/torbox-download-reliability`. Its reviewable outcome
is that accepted downloads recover through temporary upstream/HTTP failures
without losing valid queued tasks, duplicating uncertain submissions, or
hanging local transfers indefinitely.

Keep separate commits inside that PR for:

1. Queue-to-active tracking, confirmed-absence accounting, and historical queue
   recovery; share matching/pagination/state guards with removal.
2. Byte-idle detection, link refresh, partial preservation, and progress-based
   retry-budget reset.
3. Submission-outage recovery, including the tested HTTP 521 behavior.

This consolidates genuine reliability fixes while leaving manual-add,
force-start, deletion policy, toolchain upgrades and version bumps outside the
fix PR. The reliability branch has not been created or implemented by this
inventory task.

### Behavior decisions that must survive review

- HTTP status errors are not proof of task absence. Preserve normal structured
  HTTP 5xx retry behavior; do not reintroduce the initial removal-port regression.
- Confirmed queue presence resets absence accounting and remains pending, even
  when the queue has waited a long time. Force-start is an optional feature.
- Actual byte progress resets download failure accounting; startup byte counts
  or repeated attempts alone do not constitute progress.
- Dev stops after three consecutive HTTP/status/idle failures. Decide whether
  that terminal budget is appropriate upstream instead of presenting the
  entire policy as just timeout plumbing.
- Dev treats HTTP 400/404 with no fresh link as permanent. Distinguish provider
  refusal from a temporary failure obtaining a replacement URL before keeping
  this rule in the contribution.
- The five-attempt HTTP 521 policy differs from generic retry behavior and
  ordinary permanent rejection. Retain its separate accounting, visibility and
  artifact cleanup when porting; do not silently combine it with `b185f68`.
- Apply identity reconciliation to all uncertain create outcomes where possible,
  not just HTTP 521. The current dev implementation gates that recovery on the
  521 counter, leaving other lost responses/timeouts as a known gap. Unknown
  identity must not justify blind duplication or deleting unrelated tasks.
- Cancellation must retain late submission discoveries and prevent active
  transfer/promotion results from reviving removal. Reuse the new removal
  safeguards rather than overwrite them with older dev implementations.

## Shared code and merge order

Shared prerequisites include typed lookup outcomes, queue/active identity
matching, stale-ID fallback, complete pagination, and removal-authoritative
state updates. Assign each final shared implementation to one contribution,
then have the other PR reuse it. Do not submit conflicting copies of the same
helper as independently complete implementations.

Prepare the reliability port against current upstream and compare its shared
parts with `f8277bf`. Prefer merging reliability first when it establishes the
common queue/submission foundation; rebase/rebuild upstream-remove to contain
only the deletion feature and its remaining safeguards afterward. If deletion
is reviewed first, rebase reliability on the merged result and retain the
normal queue/transfer fixes. Neither route requires rewriting the published
personal dev history. Use an explicit stacked PR if a dependency is needed
before the first PR merges.

## Evidence to carry with the contributions

Existing regression sources on dev:

- `internal/worker/queued_jobs_test.go` and specs/preserve-torbox-queued-jobs:
  accepted queues, generic IDs, handover gaps, failed views, startup recovery,
  removal races, and confirmed absence.
- `internal/files/downloader_idle_test.go`: stalled headers and bodies and
  small progressing reads that continually reset the idle timer.
- `internal/worker/orchestrator_test.go`: HTTP 403 link replacement and
  exhaustion, HTTP 400 unrecoverable links, 521 retry/reconciliation/exhaustion,
  and historical recovery. Keep these with the behavior they actually exercise.
- `internal/worker/download_test.go`: retry-budget reset on actual progress.
- specs/retry-torbox-521/PRODUCT.md: explicit retry delays and terminal policy.

Evidence on rebuilt upstream-remove:

- `internal/torbox/removal_lookup_test.go`: pagination, stale conflicting IDs,
  repeated-page rejection, and inconclusive fallback errors.
- `internal/worker/removal_regression_test.go`: disabled-mode structured outage
  retries; late create cleanup; stalled-transfer cancellation on removal;
  queue promotion; typed absence; promotion budget retention.
- Existing upstream PR #7 naming tests remain intact.

Validate the combined deployment artifact, not just each isolated branch:
connection/authentication -> add -> confirmed queue -> active -> local download
with stall or expired link -> verified completion -> Sonarr import -> client
removal -> local and upstream cleanup. Include container restart, an upstream
outage, concurrent client removal, and disabled upstream cleanup. Record exact
commit/image identity and sanitized logs for the deployed version. The current
review has automated tests and a Linux build for `f8277bf`, not live integration
or race-detector evidence for the combined future reliability branch.
