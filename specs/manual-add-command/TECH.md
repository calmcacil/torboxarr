# Technical Design: Manually Add Downloads From The Container

## Context

The product behavior is defined in [`PRODUCT.md`](./PRODUCT.md).

The current `cmd/torboxarr/add.go` implements container-local magnet, torrent
file, and NZB file submission through the qBittorrent- and SABnzbd-compatible
APIs. It validates command arguments and files, verifies the corresponding
category, submits one item, and distinguishes definitive and uncertain
outcomes.

## Command Dispatch

Keep `add` as an early CLI dispatch in `cmd/torboxarr/main.go`. Running an add
command must not load the server configuration, open SQLite, initialize the
filesystem layout, or start workers.

Use a `flag.FlagSet` with:

```text
--category string
--magnet string
--torrent string
--nzb string
```

Trim values and require exactly one non-empty source flag. Reject positional
arguments. Continue using the signal-derived context from `main` so `SIGINT`
and `SIGTERM` cancel HTTP requests.

Keep the production base URL constant:

```go
const addServerAddress = "http://127.0.0.1:8085"
```

An internal base-URL parameter remains useful for HTTP tests but is not exposed
through flags or environment variables.

The container image exposes `/app/torboxarr` through
`/usr/local/bin/torboxarr` so the documented `docker exec ... torboxarr add`
form resolves through the standard container `PATH`.

## Input Model

Represent the selected source explicitly rather than extending a function with
several unrelated string parameters:

```go
type addInput struct {
    Category    string
    Magnet      string
    TorrentPath string
    NZBPath     string
}
```

The command can dispatch to focused torrent and NZB client methods after common
argument validation. Avoid creating a general plugin or source abstraction for
three fixed input forms.

## Torrent Path

Preserve the existing qBittorrent flow:

1. Validate the magnet or torrent file.
2. Require `TORBOXARR_QBIT_PASSWORD`.
3. Create a cookie jar and `http.Client` with the bounded add timeout.
   Reject redirects so credentials and source data cannot leave the fixed local
   server origin.
4. `POST /api/v2/auth/login` with username `admin` and the configured password.
5. `GET /api/v2/torrents/categories` and require an exact category key.
6. Submit a magnet as URL-encoded `urls` and `category` fields to
   `POST /api/v2/torrents/add`.
7. Submit a torrent file as multipart fields `torrents` and `category` to the
   same endpoint.
8. Require HTTP 200 and body `Ok.` for accepted submission.

Keep the existing magnet redaction behavior and 256 MiB torrent limit. Preserve
the bounded bencode parser rather than adding a dependency solely for command
validation. The server remains authoritative and will compute the submission
fingerprint and persist the payload.

## NZB Path

Add an SAB-focused path using the same bounded HTTP client but without a cookie
jar or qBittorrent login:

1. Validate the NZB file locally.
2. Require `TORBOXARR_SAB_API_KEY`.
3. Query
   `GET /sabnzbd/api?mode=get_config&output=json&apikey=<key>`.
4. Parse `config.categories` and require an exact category name.
5. Build a multipart request containing one file under `nzbfile`, plus
   `mode=addfile`, `output=json`, `cat=<category>`, and the API key.
6. `POST /sabnzbd/api` and parse the SAB-compatible add response.
7. Treat HTTP 200 with `status: true` and one `nzo_ids` entry as accepted.
8. Treat a false status, missing ID, non-2xx response, or unreadable JSON as a
   definitive rejection unless request delivery itself was uncertain.

The SAB client also rejects redirects. This prevents API keys in request URLs
or referrer metadata from being disclosed to a redirect target.

The current server accepts `apikey` in query or form data. Prefer sending it in
the query for multipart requests so authentication occurs before multipart
parsing. Error messages must never include the request URL because it contains
the API key.

The local endpoint parses multipart uploads with a 2 MiB memory threshold, not a
strict request-size cap. Product behavior requires a 256 MiB complete-request
upload limit. The server wraps accepted request bodies with
`http.MaxBytesReader`, and the command validates the source and constructed
request against the same limit. `ParseMultipartForm(2 << 20)` remains only the
memory threshold; it does not itself enforce the 256 MiB maximum because larger
bodies may spill to disk.

### NZB Validation

Require an existing, non-empty regular file within the 256 MiB upload limit.
Use a streaming XML decoder and require:

- well-formed XML;
- the first document element's local name is `nzb`;
- the document reaches a valid end without malformed XML; and
- trailing non-whitespace data is rejected.

Do not require a particular XML namespace or inspect article contents during
CLI validation. This keeps validation format-aware without duplicating TorBox
or server semantics.

## Category Validation

Torrent and NZB category sets are queried independently because they are
separate compatibility interfaces and may evolve independently.

For qBittorrent, continue parsing the top-level category map and matching the
requested key exactly.

For SAB, parse:

```json
{
  "config": {
    "categories": [
      {"name": "tv"}
    ]
  }
}
```

Match `name` exactly after trimming the command input. Do not lowercase,
create, or substitute a category. Do not use the unauthenticated HTML category
page.

## Result Classification

Separate failures by phase:

- local validation and request construction: definitive, no submission sent;
- authentication and category lookup: definitive, no submission sent;
- connection failure before a submission request is attempted: definitive
  local-service reachability failure;
- non-2xx or protocol-level rejection with a received response: definitive
  server rejection; and
- cancellation, timeout, connection reset, or other transport failure after
  starting the submission request: uncertain outcome.

HTTP transports cannot always prove whether a failed request was delivered.
For submission calls, prefer the safe uncertain classification whenever that
distinction cannot be established. Tell the operator to inspect the
qBittorrent-compatible queue for torrents or SAB-compatible queue/history for
NZBs before retrying.

The server's submission fingerprint remains the primary duplicate defense, but
the CLI must not promise that retrying an uncertain request is always harmless.

## Output And Secret Handling

On success, write one line to stdout:

- magnet: source kind, normalized info hash, and category;
- torrent file: source kind, base filename, and category; or
- NZB file: source kind, base filename, category, and returned non-secret NZO
  ID when available.

Write errors to stderr through the existing `main` dispatch and return a
non-zero status. Do not print full magnets, API keys, passwords, request URLs
containing credentials, file content, or server response bodies that might
reflect submitted secrets.

## Testing

### Argument And Validation Tests

- `--category` is required.
- Exactly one of all three source flags is required.
- Positional arguments and mixed flags are rejected without network access.
- Valid and invalid magnet cases preserve secret redaction.
- Torrent file checks cover missing, directory, non-regular, empty, oversized,
  malformed, deeply nested, valid, and trailing-data inputs.
- NZB checks cover missing, directory, non-regular, empty, oversized, malformed
  XML, wrong root, trailing data, namespaced root, and valid input.

### Torrent HTTP Tests

- Login uses `admin`, the configured password, and a cookie-backed session.
- Missing password fails before requests.
- Category lookup precedes submission and rejects unknown categories.
- Magnet submission uses URL encoding and sends no rename field.
- Torrent upload uses one multipart file and preserves bytes and base filename.
- Authentication, category, protocol, and server rejection errors are clear.
- Submission timeout and cancellation are classified as uncertain.

### NZB HTTP Tests

- Missing SAB API key fails before requests.
- Category lookup uses `mode=get_config` and authenticates with the API key.
- Unknown and unreadable category responses prevent upload.
- Upload uses `mode=addfile`, one `nzbfile`, the selected category, and JSON
  output.
- The API key is not included in errors or captured command output.
- Accepted response requires `status: true` and one NZO ID.
- False status, malformed JSON, missing IDs, and non-2xx responses are rejected.
- Upload timeout, cancellation, and connection reset are classified as
  uncertain and reference the SAB queue/history.

### Integration Coverage

- Run the command client against the real in-memory TorBoxarr router and store
  for one magnet, torrent file, and NZB file.
- Verify exactly one persisted job with the expected source type, client kind,
  category, payload, and accepted state.
- Verify duplicate server behavior remains authoritative.

### Validation Commands

```text
git diff --check
gofmt -w <changed Go files>
go vet ./...
go test -count=1 ./...
go build ./cmd/torboxarr
go test -race -count=1 -p 1 -parallel=1 ./...
```

## Current Gaps

- No known implementation gaps remain for the specified command flows.

## Implementation Status

- The server and command share and enforce the 256 MiB SAB request limit.
- The command validates and submits magnets, torrent files, and NZB files.
- SAB category lookup, authentication, upload handling, response validation, and
  uncertain-outcome reporting are implemented.
- Real-router integration coverage verifies all three source types, persisted
  payloads, source metadata, and duplicate handling.
- Source URIs are redacted from compatibility logs and fallback display names,
  and returned NZO IDs are checked before confirmation output.
- Job creation and its initial event are atomic, submissions enter directly in
  the processable `submit_pending` state, and rejected or duplicate upload
  attempts clean their private payload directories.
- Both compatibility clients reject redirects, request-write tracking is
  race-safe, and local torrent/NZB parsers reject noncanonical dictionaries and
  trailing XML metadata.
