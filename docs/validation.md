# Validation

Local checks completed on macOS arm64 with Go 1.27.4 and FFmpeg/FFprobe 4.3.2:

- `go test -race -timeout 60s -count=1 ./...`: 29 passing test cases,
  including subtests; real audio tests enabled with `REQUIRE_FFMPEG=1`.
- `go vet ./...`: passed.
- `go mod verify`: all downloaded modules verified.
- `CGO_ENABLED=0 go build -trimpath ./cmd/media-job-api`: passed.
- A separate real HTTP run uploaded generated WAV bytes, waited for the worker's
  PCM digest, replayed an idempotency key, and deleted the terminal job.
- The process refused a non-loopback listener without a bearer token.

Generated fixtures cover 16-bit PCM WAV, 24-bit PCM AIFF, 16/24-bit FLAC, and
24-bit ALAC. They compare normalized PCM hashes across containers. Other tests
reject AAC, invalid data, empty decoded audio, and a truncated WAV fixture.

The race detector finding no problem is evidence for the exercised paths, not
proof that all possible interleavings are safe. The store tests also exercise
cancellation before worker registration and late completion after cancellation.

Docker is not installed on the local development machine. The CI workflow
contains an independent container build and HTTP verification check, alongside
Go 1.26/1.27 test jobs on Linux and macOS. The repository's Actions results are
the authority for those remote checks; this document does not assume they pass.

Not established by these checks: throughput under sustained load, behavior on
network filesystems, malicious-decoder isolation, or authenticity of uploaded
recordings. No live Samery configuration, media collection, or Telegram session
was used in the tests.
