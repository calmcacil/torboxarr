# Manually Add Downloads From The Container

## Summary

TorBoxarr provides a container-local `add` command for operators to submit one
torrent or Usenet download without configuring an Arr application. The command
uses the running TorBoxarr server's qBittorrent- and SABnzbd-compatible APIs so
manual jobs receive the same validation, persistence, duplicate protection,
worker processing, queue visibility, and removal behavior as client-submitted
jobs.

## Problem

Operators sometimes need to send an individual magnet, torrent file, or NZB
file through TorBoxarr for testing or one-off use. Calling TorBox directly would
bypass TorBoxarr's local state and processing pipeline. Manually reproducing a
compatibility API request is error-prone, especially inside a container where
the service credentials and input files are already available.

## Goals

- Provide one documented command for manual torrent and Usenet submission.
- Route submissions through the running local TorBoxarr service.
- Require an existing category so resulting paths and client views are
  predictable.
- Validate inputs before sending them when validation can be performed safely.
- Make successful, rejected, and uncertain outcomes clear to operators.
- Avoid exposing credentials or source secrets in output and errors.

## Non-goals

- Connecting to a remote or operator-selected TorBoxarr server.
- Calling TorBox directly or writing directly to TorBoxarr's database.
- Creating categories from the command.
- Submitting multiple items in one invocation.
- Supporting HTTP NZB URLs, ordinary torrent URLs, or source types other than
  magnet URIs, torrent files, and NZB files.
- Exposing all qBittorrent or SABnzbd submission options.
- Waiting for the remote task or local download to complete.

## Interface

The supported forms are:

```text
torboxarr add --category <name> --magnet <magnet-uri>
torboxarr add --category <name> --torrent <path>
torboxarr add --category <name> --nzb <path>
```

The command is intended to run inside the TorBoxarr container, commonly through
`docker exec`. File paths are resolved in the command process's filesystem and
therefore must be visible inside the container.

## Behavior

1. `--category` is required and blank values are rejected.

2. Exactly one of `--magnet`, `--torrent`, or `--nzb` is required. The flags
   are mutually exclusive, positional arguments are rejected, and each
   invocation submits at most one item.

3. The command connects only to the running service at
   `http://127.0.0.1:8085`. It has no server-address flag or environment
   override.

4. The server must already be running and listening on the expected container
   loopback address. Failure to connect produces an actionable error and does
   not fall back to TorBox or direct database access.

5. Magnet and torrent-file submissions use the qBittorrent-compatible API.
   Authentication uses username `admin` and the value of
   `TORBOXARR_QBIT_PASSWORD` from the command environment.

6. NZB submissions use the SABnzbd-compatible API. Authentication uses
   `TORBOXARR_SAB_API_KEY` from the command environment.

7. A missing or blank credential fails before category lookup or submission.
   Credential values are never printed.

8. Before submitting a magnet or torrent file, the command authenticates to
   the qBittorrent-compatible API and queries its category list. The selected
   category must match an existing category exactly.

9. Before submitting an NZB file, the command queries the SABnzbd-compatible
   category configuration using its API key. The selected category must match
   an existing SAB category exactly.

10. An unknown category is rejected before the source is submitted. The
    command does not create or silently substitute a category.

11. A magnet must use the `magnet` scheme and contain a non-empty
    `xt=urn:btih:` value. Other magnet parameters are preserved unchanged when
    the URI is submitted.

12. Magnet validation errors and other diagnostic output do not echo the full
    URI because it may contain private tracker credentials or other secrets.

13. A torrent path must identify an existing, non-empty regular file. The file
    must contain one complete bencoded top-level dictionary with an `info`
    dictionary and must not exceed 256 MiB.

14. An NZB path must identify an existing, non-empty regular file. The command
    rejects an input that is not parseable as XML with an NZB document root. It
    applies the local SAB-compatible endpoint's 256 MiB complete-request upload
    limit before making the request.

15. Torrent and NZB files are uploaded under their base filename. The command
    does not send a rename, password, custom save path, tags, paused state,
    priority, script, or post-processing override.

16. The local compatibility endpoint remains responsible for authoritative
    acceptance, persistence, duplicate detection, and payload storage. Local
    pre-validation does not replace server validation.

17. A successful compatibility response means TorBoxarr accepted the
    submission into its normal pipeline. It does not mean TorBox accepted the
    remote task or that downloading completed.

18. On successful magnet submission, output identifies the info hash and
    category without printing the complete magnet URI.

19. On successful file submission, output identifies the base filename,
    source type, and category. It does not print file contents or credentials.

20. A definitive authentication, category, validation, or server rejection
    returns a non-zero exit status with an actionable error. A rejected input
    is not reported as accepted.

21. If interruption, cancellation, timeout, or connection loss occurs while a
    submission request may have reached the server, the command reports the
    outcome as unknown. It tells the operator to inspect the corresponding
    TorBoxarr queue before retrying because retrying may create a duplicate.

22. A failure known to occur before the submission request is sent, such as
    missing credentials, invalid input, failed authentication, failed category
    lookup, or inability to construct the request, is not described as an
    uncertain submission.

23. `SIGINT` and `SIGTERM` cancel in-flight requests and produce a non-zero exit
    status. Cancellation during submission follows the uncertain-outcome rule.

24. The command uses a bounded HTTP timeout so an unavailable local service
    does not block indefinitely.

25. Jobs accepted through the command appear in the same qBittorrent- or
    SABnzbd-compatible queue and history views as jobs submitted by other
    clients and follow the normal submission, polling, download, verification,
    completion, and removal states.

26. Duplicate handling remains the server's responsibility. The command does
    not query TorBox directly or infer deduplication from display names.

## Acceptance Criteria

- Magnet, torrent-file, and NZB-file invocations submit through the appropriate
  localhost compatibility API.
- Missing, multiple, or positional inputs fail without a network request.
- The corresponding required credential is used and never printed.
- Unknown categories fail before submission.
- Invalid magnets, torrent files, and NZB files fail locally.
- One invocation sends no more than one source item.
- A successful response prints a concise, non-secret confirmation.
- A definitive rejection and an uncertain submission are clearly
  distinguishable.
- Accepted jobs enter TorBoxarr's normal persisted processing pipeline.
