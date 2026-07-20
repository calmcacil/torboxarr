# TorBoxarr Fork — Testing & Workflow Guide (local)

Gitignored living doc for future agents/sessions working on the
`calmcacil/torboxarr` fork. Mirror of `manual-push.md` but focused on branch
structure, testing, and how to advance after upstream PRs merge.

## Canonical clone

`~/work/torboxarr` on the local workstation. Remote `origin` = the fork
(`github.com/calmcacil/torboxarr`). Remote `upstream` = MrJoiny's repo
(`github.com/MrJoiny/TorBoxarr`) — added for syncing; read-only use.

## Branch model

- **`main`** — deployed as `ghcr.io/calmcacil/torboxarr:latest` on teostra.
  Must stay clean/deployable: upstream `main` + fork-only features only
  (currently `feat/upstream-remove`). No feature work directly on `main`.
- **`dev`** — integration/testing branch, forked from `main`. Fold feature
  branches here to build and test combined changes before they graduate to
  `main`. Not deployed unless explicitly asked.
- **Feature branches** (`feat/*`, `fix/*`) — short-lived, PR-scoped. Stacked on
  upstream `main` (or the relevant base) for clean upstream PRs.
- **`feat/upstream-remove`** — the upstream-delete feature (fork-only; "keep on
  our end"). Merged into `main`. Kept as a branch for future upstream PR if the
  maintainer wants it.

### After an upstream PR merges (e.g. #2, #4)

1. `git fetch upstream`
2. `git rebase feat/<branch> upstream/main` (rebase feature branches onto the
   now-merged upstream tip).
3. `git checkout main && git reset --hard upstream/main`
4. Merge fork-only feature branches back into `main` (e.g.
   `git merge --no-ff feat/upstream-remove`).
5. Delete the merged PR branches (local + `git push origin --delete`).
6. Force-push rewritten `main`/feature branches with `--force-with-lease`.
7. Rebuild + redeploy (below).

**Always `git branch --show-current` before building** — the build uses HEAD,
not the intended target. This has bitten us before (built from the wrong
branch).

## Build & deploy (fork-only, manual push)

CI publish workflow is **disabled** on the fork (#89). Manual flow:

```sh
# 1. set version (edit VERSION file) — fork builds use a -calm suffix
# 2. ensure on the intended branch (main or dev)
git branch --show-current
# 3. build multi-arch + push to ghcr
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/calmcacil/torboxarr:latest \
  -t ghcr.io/calmcacil/torboxarr:<version> \
  --push .
# 4. on teostra:
ssh teostra "cd /opt/apps/compose && docker compose pull torboxarr && docker compose up -d torboxarr"
```

Requires `write:packages` gh token scope (#90):
`gh auth refresh -s write:packages --hostname github.com`.

### ghcr tag deletion (when rerolling versions)

```sh
# list versions + ids
gh api /user/packages/container/torboxarr/versions --paginate \
  | python3 -c "import sys,json;[print(v['id'],v['metadata']['container']['tags']) for p in json.load(sys.stdin) for v in p]"
# delete by id
gh api -X DELETE /user/packages/container/torboxarr/versions/<id>
```

## Testing on teostra

- **Logs (one-shot, do NOT use -f — it hangs):**
  ```sh
  ssh teostra "cd /opt/apps/compose && docker compose logs --since 5m torboxarr"
  ```
- **DB state (WAL-aware):** TorBoxarr uses SQLite with a `-wal` file. Reading
  only the `.db` misses today's writes. Copy all three files
  (`.db`, `-wal`, `-shm`) together into a temp dir on the host, then open with
  sqlite3 and run `PRAGMA wal_checkpoint(FULL)` before querying — this replays
  the WAL into the main db so committed state (including `removed` rows and
  metadata JSON) is visible. **Do NOT `cat` the three files into one blob** —
  concatenation does not replay the WAL and yields a stale/garbled read (shows
  pre-checkpoint state, e.g. a job still `remove_pending` after it actually
  finalized). Job states: `submit_pending`, `remote_active`, `remote_queued`,
  `completed`, `removed`, `remote_failed`.
- **Full-cycle test:** trigger a Sonarr download → finalize → Sonarr import →
  Sonarr `torrents/delete` → TorBoxarr removes local + (if
  `TORBOXARR_UPSTREAM_REMOVE=true`) deletes upstream TorBox task. Confirm via
  logs + DB row count.

## Key behavior facts (verified)

- `public_id` = infohash for torrent adds (so Sonarr can correlate + send
  delete). Set in `enqueueSubmission`.
- qBittorrent torrent info reports `ratio_limit: 0` so Sonarr's
  `HasReachedSeedLimit` is true → Sonarr removes after import.
- Multipart + URL-encoded form handling in `handleQBitAdd` (PR #2, upstream).
- Cached-download fast-path: **evaluated and dropped** — 30s poll wait is fine,
  extra API call not worth it vs 300/min limit.
- No WebUI. Manual adds: either `docker exec torboxarr torboxarr add ...`
  (subcommand, hits localhost:8085 qBittorrent API) or a future watchdir
  (see `watchdir.local.md`).

## Do NOT

- Do not enable the fork CI publish workflow (intentionally disabled).
- Do not commit `*.local.md` files (gitignored).
- Do not put fork-specific docs (README/.env.example changes) in upstream PR
  branches (#103).
- Do not build from the wrong branch — verify `git branch --show-current`.
