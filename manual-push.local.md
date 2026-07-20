# TorBoxarr Fork — Manual Build & Push Reference

Local-only notes (gitignored). Use this when making further changes to our fork
(`github.com/calmcacil/torboxarr`) and deploying to teostra.

## Clone / working dir

- Canonical clone: `~/work/torboxarr`
- Remote: `https://github.com/calmcacil/torboxarr.git`
- Remote host: `teostra.calmcacil.dev` (tailnet only, SSH via `~/.ssh/id_ed25519.asc`)

## Branch layout (stacked for upstream PRs)

```
7813140  upstream base (Bump version to 1.0.4)
  └─ fix/multipart-content-type   (PR #2)  — multipart form-data fix
       └─ fix/sonarr-seed-removal  (PR #4)  — ratio_limit:0 so Sonarr removes torrents
            └─ feat/upstream-remove         — TORBOXARR_UPSTREAM_REMOVE feature (kept on fork for now)
main = upstream base + the 3 branches merged (deployed as 1.0.6-calm)
```

- `main` is a clean "upstream + our 3 features", NO fork CI commit (we push manually).
- PR branches are stacked so each PR diff shows only its own change.
- Rebase the stack onto upstream `main` after PR #2 merges, then `main` follows.

## Build & push (manual — CI intentionally disabled)

```sh
cd ~/work/torboxarr
git checkout main
# bump VERSION to the next release tag (e.g. 1.0.6-calm), commit it
printf '1.0.6-calm\n' > VERSION
git add VERSION && git commit -m "chore: bump version to X.Y.Z-calm"

# buildx builder (create once if missing):
# docker buildx create --name tbxb --use
docker buildx use tbxb
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --file deploy/Dockerfile \
  --tag ghcr.io/calmcacil/torboxarr:X.Y.Z-calm \
  --tag ghcr.io/calmcacil/torboxarr:latest \
  --label org.opencontainers.image.source=https://github.com/calmcacil/torboxarr \
  --label org.opencontainers.image.version=X.Y.Z-calm \
  --push .
```

Requires `write:packages` on the gh token:
`gh auth refresh -s write:packages --hostname github.com` (interactive device flow).

## Deploy on teostra

```sh
ssh -i ~/.ssh/id_ed25519.asc teostra \
  "cd /opt/apps/compose && docker compose pull torboxarr && docker compose up -d torboxarr"
```

## Deleting old ghcr tags

ghcr has no UI bulk delete; use the API (version IDs, not tag names):

```sh
gh api -X GET /user/packages/container/torboxarr/versions \
  | python3 -c "import sys,json;d=json.load(sys.stdin);[print(v['id'],[t['name'] for t in v['metadata']['container']['tags']]) for v in d]"
# then delete each unwanted version id:
gh api -X DELETE /user/packages/container/torboxarr/versions/<ID>
```

Note: deleting a tagged version (e.g. `1.0.7`) also removes its manifest; untagged
`sha256:...` entries are just layers and can be left or pruned by GitHub.

## Test the full cycle

1. Trigger a download in Sonarr (TorBoxarr as qBittorrent client).
2. Watch: `ssh -i ~/.ssh/id_ed25519.asc teostra "cd /opt/apps/compose && docker compose logs -f torboxarr"`
   Expected: `job accepted` → `remote task created` → `job finalized` →
   Sonarr imports → `job marked for removal` → `job removed locally; upstream torbox task deleted`
   (the last line only when `TORBOXARR_UPSTREAM_REMOVE=true`).

## Key behaviors

- `TORBOXARR_UPSTREAM_REMOVE=true` (set in compose `.env`): removing a job also
  deletes the matching TorBox task. Fallback: if the upstream call fails, local
  cleanup still proceeds and the log says `torbox content retained`.
- `ratio_limit:0` + `pausedUP` makes Sonarr's `HasReachedSeedLimit` true so it
  sends `torrents/delete` after import (debrid torrents never seed).
- `public_id` = torrent infohash, so Sonarr correlates the download and sends delete.

## Common gotchas

- Always `git branch --show-current` before building — buildx uses HEAD, not the
  intended target branch. Building from the wrong branch silently ships the wrong code.
- WAL reads: copy `.db`, `-wal`, `-shm` together (tar them) or run
  `PRAGMA wal_checkpoint(TRUNCATE)` before reading SQLite state.
- Don't commit `.env`, `/data`, or these local `*.local.md` notes.
