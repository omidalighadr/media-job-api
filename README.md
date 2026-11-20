# media-job-api

A small Go service for verifying uploaded lossless audio in the background.
Upload a file, receive a job ID, then fetch its status and PCM digest. The queue
survives restarts and supports cancellation.

This is a standalone backend extracted from the kind of audio checks I need in
Samery. It does not download music or connect to Telegram.

## Run locally

Requirements: **Go 1.26+**, **FFmpeg and FFprobe on PATH**, macOS or Linux.
Use a currently supported, patched Go release. No account, API key, or Redis is
needed for local use.

```sh
git clone https://github.com/omidalighadr/media-job-api.git
cd media-job-api
go mod download
go build -o media-job-api ./cmd/media-job-api
./media-job-api -data ./data
```

The default address is `127.0.0.1:8080`. Keep `data` dedicated to this service.
The SQLite database and private work directory are created there automatically.
A second process cannot use the same directory while the first owns it.

## Try a job

Generate a short, original test fixture:

```sh
ffmpeg -f lavfi -i 'sine=frequency=440:duration=1' -c:a flac sample.flac
```

Upload raw file bytes, rather than a multipart form:

```sh
curl -i http://127.0.0.1:8080/v1/jobs \
  -H 'Content-Type: application/octet-stream' \
  -H 'Idempotency-Key: sample-1' \
  --data-binary @sample.flac
```

The response is `202 Accepted`, with a job ID and a `Location` header.
Replace `JOB_ID` below with that ID:

```sh
curl http://127.0.0.1:8080/v1/jobs/JOB_ID
```

A completed job contains a result shaped like this (digest shortened here):

```json
{
  "id": "...",
  "status": "succeeded",
  "created_at": "...",
  "updated_at": "...",
  "result": {
    "codec": "flac",
    "sample_rate_hz": "44100",
    "channels": 1,
    "bits_per_sample": 16,
    "pcm_s32le_sha256": "..."
  }
}
```

```sh
# Cancel queued or running work. Repeating this request is harmless.
curl -X POST http://127.0.0.1:8080/v1/jobs/JOB_ID/cancel

# Remove a terminal job and its idempotency key.
curl -X DELETE http://127.0.0.1:8080/v1/jobs/JOB_ID
```

## What verification means

The service probes the **first audio stream**, checks its codec and bit depth,
and decodes that stream from beginning to end with FFmpeg's `-xerror` enabled.
It hashes the decoded **signed 32-bit little-endian PCM** without resampling or
changing the channel count. Empty output is rejected.

Supported inputs are FLAC, ALAC in an MP4/M4A container, and integer PCM in WAV
or AIFF, at 8, 16, 24, or 32 bits where the codec supports it. Floating-point PCM,
lossy codecs, playlists, URLs, video-only files, and encrypted audio are outside
the scope of this version. File extensions are not trusted.

A successful result means the accepted stream decoded without a reported error.
It cannot prove that a file came from a particular service, was never converted
from MP3, or contains the complete original recording. A cleanly shortened file
can still decode successfully. A digest is useful for comparing decoded samples
only when sample rate, channel layout/order, and normalization assumptions also
match. This API does not implement an audio comparison endpoint.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/v1/jobs` | Upload raw bytes; `202` new job, `200` idempotent replay |
| GET | `/v1/jobs/{id}` | Read status and result; `404` if absent |
| POST | `/v1/jobs/{id}/cancel` | Cancel active work; return current state |
| DELETE | `/v1/jobs/{id}` | Delete terminal job; `409` while active |
| GET | `/healthz` | Process liveness |
| GET | `/readyz` | Database availability |

Errors have the form `{"error":"upload_too_large"}`. Common responses:

- `400`: empty/incomplete upload or invalid idempotency key.
- `401`: missing or incorrect bearer token.
- `409`: same idempotency key with different bytes, or deletion of an active job.
- `413`: upload exceeds the configured limit.
- `415`: content type/encoding is unsupported.
- `503`: upload slots or active queue capacity are full. Retry with backoff.

`Idempotency-Key` is optional: 1–128 visible ASCII characters without spaces.
Within this single-owner service, the same key and SHA-256 of uploaded bytes
return the original job. The key does not expire until that job is deleted.
Replaying a canceled or failed job returns its existing state; use a new key to
submit another attempt. A retry still sends the upload bytes over the network.

Job states are `queued`, `running`, `succeeded`, `failed`, and `canceled`.
A failed job reports `verification_failed` or `verification_timeout` without
returning subprocess diagnostics or local paths.

## Limits and persistence

```text
-listen          127.0.0.1:8080
-data            ./data
-workers         2       (1–8)
-max-upload-mib  32      (1–256)
-queue-limit     32      (1–1000, queued + running)
-job-timeout     2m
```

Two uploads may be read concurrently. Each active worker also holds an input in
memory. These are request/concurrency bounds, not a hard memory budget: FFmpeg,
Go buffers, SQLite, and filesystem caches need additional memory.

Input bytes and the queue record commit in one SQLite transaction. Interrupted
running jobs become queued when the exclusive owner starts again. This provides
**at-least-once execution** after a crash; verification has no external side
effects. Failed jobs are not retried automatically. Graceful shutdown cancels
subprocesses and requeues interrupted work. User cancellation takes precedence
over any late worker result.

Terminal jobs release their input bytes from the logical database. Results stay
until explicitly deleted. SQLite may retain freed pages and WAL contents for
reuse; this is **not secure erasure**, and the database file may not shrink.
There is no automatic retention policy or storage quota. Monitor disk space;
back up a stopped service or use SQLite's online backup facilities. Do not copy
only the live `.db` file while ignoring its WAL.

## Remote use

A non-loopback listener requires `MEDIA_JOB_TOKEN`:

```sh
export MEDIA_JOB_TOKEN="$(openssl rand -hex 32)"
./media-job-api -listen 0.0.0.0:8080 -data ./data
```

Add `Authorization: Bearer ...` to every `/v1/` request. Health probes are public.
Use a TLS reverse proxy and restrict network access. This is a single-owner
service, not a public multi-user upload platform: there are no per-user quotas,
roles, or ownership checks. FFmpeg processes have time limits and format/protocol
restrictions, but are not isolated by an application-level OS sandbox. For
untrusted uploads, run in a constrained container and keep FFmpeg patched.

## Docker

```sh
docker build -t media-job-api .
docker volume create media-jobs
docker run --rm --init --read-only --cap-drop ALL \
  --security-opt no-new-privileges --memory 512m --cpus 2 --pids-limit 128 \
  -p 127.0.0.1:8080:8080 -e MEDIA_JOB_TOKEN \
  -v media-jobs:/data media-job-api
```

Set `MEDIA_JOB_TOKEN` first. The image runs as a non-root user; a bind mount must
be writable by UID 10001. The Go binary uses no CGO; FFmpeg remains a runtime
dependency. The Docker recipe is provided for reproducibility; see
[validation](docs/validation.md) for what has actually been executed.

## Develop

```sh
go test -race -timeout 60s ./...
go vet ./...
go build ./cmd/media-job-api
```

Audio integration tests generate their own PCM fixtures. They skip when FFmpeg
is missing; set `REQUIRE_FFMPEG=1` to make its absence fail the run, as CI does.
Tests cover restart recovery, exclusive directory ownership, concurrent claims,
cancellation races, queue limits, idempotency, HTTP errors, and real decoding.

Code is split into `internal/jobs` (storage and workers), `internal/audio`
(FFmpeg verification), `internal/httpapi` (HTTP), and `cmd/media-job-api`
(configuration and shutdown). See [design notes](docs/design.md) for the main
tradeoffs and [contributing](CONTRIBUTING.md) for the development workflow.

## License

MIT. FFmpeg and dependencies retain their own licenses.
