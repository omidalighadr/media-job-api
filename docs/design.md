# Design notes

## Why SQLite instead of a broker

Verification jobs carry relatively small files and run on one machine. Storing
uploads as BLOBs keeps accepting a job atomic: there is no interval in which a
queue row points to a file that has not been moved into place. The tradeoff is
extra database/WAL I/O and memory use. This version is intended for small private
queues, not terabytes of media or multiple service replicas.

The database uses WAL and `synchronous=FULL`. A single `database/sql` connection
serializes short state operations. Workers run their subprocesses concurrently,
outside database operations. Claiming a job is one `UPDATE ... RETURNING`
statement; only a queued row can be claimed. A database-assigned sequence orders
claims by acceptance, independently of wall-clock adjustments. Admission checks and insertion share
one transaction. Schema version 1 is created transactionally and tracked with
`PRAGMA user_version`; a newer schema is rejected instead of being modified.

An OS `flock` protects a dedicated data directory. Recovery runs only after that
lock is acquired. The lock file can remain after exit; ownership belongs to the
open descriptor, not to the file's presence. This implementation targets local
macOS/Linux filesystems. Network filesystems and Windows are not supported.

## State transitions

```text
queued -> running -> succeeded
   |          |  -> failed
   |          |  -> queued (shutdown/crash recovery)
   v          v
 canceled   canceled
```

Cancellation first commits the canceled state and discards the stored upload,
then cancels a registered worker context. A worker checks the state again after
registering its cancel function, covering cancellation between claim and
registration. Completion uses `WHERE status='running'`, so a result arriving
after cancellation cannot revive the job.

The two phases also mean a canceled job may briefly have a subprocess still
exiting. Cancellation is a terminal state decision, not a claim that OS resource
cleanup has already finished. The processor must honor context cancellation;
the production implementation uses `exec.CommandContext` for both tools.

## Audio result

FFprobe supplies the codec, sample rate, channels, and nominal bit depth.
FFmpeg maps only the first audio stream to `pcm_s32le` and its hash muxer. It
does not rewrite the original upload. Protocol access is restricted to local
files, and demuxers are restricted to FLAC, WAV, AIFF, and MOV/MP4. Temporary
files use random names and owner-only permissions inside the private work
folder. Metadata/hash output is bounded; stderr is discarded rather than
returned to clients. A timeout kills a stuck probe or decoder.

The digest describes a particular PCM representation, not the compressed file.
Metadata such as artwork does not participate. There is no authenticity or
perceptual-quality score. With multiple streams, later streams are not checked.

## Operational boundaries

- One private owner, one process, one data directory.
- Bearer authentication is optional only for an explicitly numeric loopback bind.
- No URLs, music service credentials, Telegram session, or bot code is involved.
- No automatic retry for a normal verification failure. Interrupted work retries
  after recovery; repeated crashes can therefore retry the same job repeatedly.
- Results persist until deletion; released SQLite pages can contain old bytes.
- Readiness checks the database, not FFmpeg availability after startup.
- Disk-full or database write failures are logged; a running record that cannot
  be finalized can require a restart/recovery after storage is repaired.

Useful next steps would be retention/quota controls and finer error categories.
Object storage, leases, and a separate broker should be considered only when
there is an actual need for multiple machines.

## References

- [Go HTTP server](https://pkg.go.dev/net/http)
- [Go subprocess cancellation](https://pkg.go.dev/os/exec#CommandContext)
- [SQLite WAL](https://www.sqlite.org/wal.html)
- [SQLite transactions](https://www.sqlite.org/lang_transaction.html)
- [SQLite RETURNING](https://www.sqlite.org/lang_returning.html)
- [modernc SQLite driver](https://pkg.go.dev/modernc.org/sqlite)
- [FFmpeg hash muxer](https://ffmpeg.org/ffmpeg-formats.html#hash-1)
