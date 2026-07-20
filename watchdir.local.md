# TorBoxarr — Manual Add Watchdir (living design doc)

Local-only planning doc (gitignored). A **living** document — we iterate on the
design together here. The CLI subcommand (option B) is **implemented and
deployed**; the watchdir (option A) remains a design proposal (open questions
below).

## Gap

TorBoxarr has no web UI. Anything outside the *arr flow must currently be added
through the qBittorrent-compatible API directly (magnet URL / `.torrent` upload
via the fake qBittorrent client). There is no easy way to drop a manual download
in and have it land in the right *arr category (sonarr/radarr) for import.

## Proposed approach: watchdir

A directory the TorBoxarr container watches. Subfolders map to *arr categories:

```
/watch/
  sonarr/    -> category="sonarr"
  radarr/    -> category="radarr"
  (others?)  -> category=<folder name>, validated against configured categories
```

Dropping a file in a subfolder enqueues a job directly into TorBoxarr's store
(via the same path the API handler uses — NOT through the qBittorrent mock
client). It bypasses the facade entirely; manual adds are TorBoxarr-internal.

### File types to support

- **`.torrent`** — read the file, enqueue with `SourceType=torrent`,
  `PayloadRef=<path>`, `Category=<folder>`. Reuses `CreateTorrentTask(PayloadPath=...)`
  already in `internal/torbox/tasks.go`.
- **Magnet** — need a text-file convention since there's no file to read:
  - option A: drop `*.magnet` whose content is one `magnet:?xt=urn:btih:...`
    per line (or single line).
  - option B: drop `*.txt` containing the magnet(s).
  - Enqueue with `SourceURI=<magnet>`, `Category=<folder>`. Reuses the existing
    magnet handling in `handleQBitAdd` / `processSubmitJob`.

### How it enqueues (reuse existing pipeline)

`internal/api/router.go:92` `enqueueSubmission(ctx, SubmissionRequest{...})`
already creates the job (`CreateJob`) and sets `StateSubmitPending`. A new
`runWatcher` worker would, for each discovered file, build a `SubmissionRequest`
(category from folder, source from file type) and call `enqueueSubmission` — no
new store/submit logic needed.

### Post-process / cleanup (must decide)

After enqueueing, the source file must be moved out of the watchdir so it isn't
re-added next scan:
- move to `/watch/processed/<folder>/<name>` (keep for audit), or
- delete.

Need atomic claim to avoid double-submit on partial writes (a `.torrent` still
being written by the dropper). Consider: require the file to be stable
(size unchanged across two quick checks) before processing, and move-to-processing
atomically.

### Compose mount (when implementing)

`apps/torboxarr.yml` currently mounts:
```
- ${APPDATADIR}/torboxarr:/config
- ${STAGINGDIR}:/staging
```
Add a watchdir volume, e.g. `${APPDATADIR}/torboxarr/watch:/watch` (under the
existing `/config` tree, or a dedicated path). Consistent with the
"mount under shared staging / appdata" convention in the compose repo.

### Env / config

- `TORBOXARR_WATCH_DIR` (path, empty/disbled = feature off).
- `TORBOXARR_WATCH_INTERVAL` (scan period; or use fsnotify).
- Possibly a allowlist of valid category folder names (must match what
  Sonarr/Radarr expect, or imports break).

## Entry point options

Two ways to expose manual adds. Both end up creating a job the running server
picks up — the difference is *who* writes the job.

### A. Watchdir (long-running watcher)

A `runWatcher` worker scans a watched directory and enqueues discovered files.
See the rest of this doc. Pros: zero-cli, can be shared (drop a file in a folder).
Cons: polling/stability/cleanup concerns (open questions 2, 4, 5), adds a volume
mount, more code.

### B. CLI subcommand (one-shot, recommended for our setup)

Add an `add` subcommand to the `torboxarr` binary, e.g.:

```
torboxarr add --category sonarr --magnet "magnet:?xt=urn:btih:..."
torboxarr add --category radarr --torrent /path/file.torrent
```

**Recommended integration: the subcommand hits the server's own qBittorrent
add endpoint at `localhost:8085`** (the same facade *arr uses), rather than
writing the DB directly. This sidesteps SQLite concurrency with the running
server and reuses the exact submit path *arr uses. The subcommand is then a
thin HTTP client; the server owns all job state.

**Why it fits compose cleanly:**
- The container's main process is `/app/torboxarr` (the server). The subcommand
  is a separate one-shot invocation: `docker exec torboxarr torboxarr add ...`.
- All `TORBOXARR_*` env (token, etc.) is already wired in `apps/torboxarr.yml`
  and read by the same config loader — no extra env wiring.
- The container is on the `internal` network with egress to TorBox's API;
  `localhost:8085` is the server itself. No compose file changes needed.
- For `--torrent file.torrent`, mount the file in (or drop it under `/config`,
  which is already mounted via `${APPDATADIR}/torboxarr:/config`).

**Trade-off vs watchdir:** the subcommand requires CLI access to teostra (we
have it) and is fire-and-forget — no long-running loop, no polling, no
stability/cleanup concerns. Lighter than watchdir by a good margin. The WebUI
is explicitly out of scope (excessive for this use case).

#### B — IMPLEMENTED (2026-07-20)

Shipped as the `add` subcommand and folded into `dev`, built, pushed to ghcr
as `:latest` (and `:1.0.7-calm-dev`), and verified end-to-end on teostra.

**Files:**
- `cmd/torboxarr/add.go` — new. `runAddCommand` parses flags
  (`--category` required, `--name` optional, exactly one of `--magnet` /
  `--torrent`). `runAdd` loads connection config from env (see below), logs
  into `/api/v2/auth/login` to get a qBittorrent session cookie, then POSTs to
  `/api/v2/torrents/add`: `urls=` form field for magnet, multipart
  `torrents` file upload for `--torrent`. Reuses the exact server path *arr
  uses — no direct DB writes.
- `cmd/torboxarr/main.go` — dispatches `os.Args[1] == "add"` to `runAddCommand`
  before the server `run()`; the subcommand does **not** start the server.
- `deploy/Dockerfile` — symlinks `/app/torboxarr` → `/usr/local/bin/torboxarr`
  so `torboxarr` resolves on `$PATH` inside the container.

**Config the subcommand reads (env, NOT `config.Load()`):**
- `TORBOXARR_SERVER_BASE_URL` (default `http://localhost:8085`) — target.
- `TORBOXARR_QBIT_USERNAME` (default `admin`).
- `TORBOXARR_QBIT_PASSWORD` — required to authenticate; if unset the subcommand
  errors before any network call.
- We deliberately bypass `config.Load()` so the subcommand doesn't require
  `TORBOXARR_TORBOX_API_TOKEN` / `TORBOXARR_SAB_API_KEY` (which `config.Load`
  validates as required). Those are already present in the container env
  anyway, but the lighter loader keeps `add` usable standalone.

**Usage (verified):**
```
docker exec torboxarr torboxarr add --category sonarr --magnet "magnet:?xt=urn:btih:..."
docker exec torboxarr torboxarr add --category radarr --name "Movie" --torrent /config/file.torrent
```
Tested with a dummy magnet: server logged `job accepted` → `submitting job to
torbox` → `remote task created` (remote_id + remote_hash), confirming the
subcommand enqueues through the same pipeline *arr uses and lands upstream on
TorBox.

**Branch:** `feat/manual-add` (off `dev`), merged into `dev`. Both pushed to
origin.

## Open questions (iterate here)

1. **Magnet convention:** `*.magnet` (content = magnet URI) vs `*.txt` vs both?
   Single magnet per file, or multiple lines? *(Subcommand B uses a direct
   `--magnet` flag — no file convention needed for B.)*
2. **Post-process:** move to `processed/` (keep) or delete? Keep is safer for
   debugging. *(N/A for B — fire-and-forget, no source file to clean up.)*
3. **Category validation:** reject unknown folders (log + leave file in an
   `errors/` dir) or treat any folder name as a literal category? *(B takes the
   category verbatim from `--category`, same as *arr does — no validation.)*
4. **Polling vs fsnotify:** simple time-based scan (reuse worker interval model)
   is simplest and matches the existing worker pattern. fsnotify adds a dep.
5. **Stability guard:** how to avoid reading a half-written file (size-settle
   check, or require a `.done` sentinel file)?
6. **Usenet manual adds:** should `nzb/` subfolder also be supported
   (`.nzb` files / usenet links)? TorBoxarr handles both source types. *(B could
   gain a `--nzb` flag later, mirroring the SAB add path.)*
7. **Entry point:** build the watchdir (A), the CLI subcommand (B), or both?
   **B is done.** A remains a proposal if we want zero-CLI shared drops.
8. **Branch:** `feat/manual-add` created off `dev`, merged into `dev`, both
   pushed to origin. (Not stacked on upstream multipart base — folded straight
   into our `dev` since the whole fork is effectively a dev branch; per user
   direction the dev build ships as `:latest`.)

## References (in-repo)

- `cmd/torboxarr/add.go` — **the implemented `add` subcommand (option B).**
- `cmd/torboxarr/main.go` — subcommand dispatch.
- `deploy/Dockerfile` — `/usr/local/bin/torboxarr` symlink.
- `internal/api/router.go:92` — `enqueueSubmission` (the enqueue path the
  subcommand reaches via `/api/v2/torrents/add`).
- `internal/api/qbit.go:116` — `handleQBitAdd` (the endpoint the subcommand POSTs to).
- `internal/api/qbit.go:17` — `handleQBitLogin` (session cookie the subcommand obtains).
- `internal/worker/submit.go` — `processSubmitJob` (torrent payload + magnet
  handling, downstream of the add).
- `internal/torbox/tasks.go` — `CreateTorrentTask` (PayloadPath), `CreateUsenetTask`.
- `internal/worker/orchestrator.go` — worker registration (`startWorker`) to
  model `runWatcher` on (option A).
- `apps/torboxarr.yml` (compose repo) — current volume mounts.
