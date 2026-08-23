# Resume: state and crash safety

How an interrupted download continues without corrupting anything — and why
the guarantees hold by construction rather than by careful bookkeeping.

## One counter per chunk is enough

Each chunk downloads its byte range strictly front-to-back: a worker
requests `bytes=(offset+done)-(end)` and advances `done` as bytes land. That
ordering is the entire trick. Because a chunk can never have a hole in the
middle of its completed region, a single number — the count of contiguous
bytes finished from the chunk's start — fully describes its progress. The
sidecar's `done` array is that number for every chunk; no bitmaps, no
interval sets, no merging logic that can have bugs.

A consequence worth spelling out: **resume granularity is byte-level, not
chunk-level**. A chunk interrupted at 3.2 of 8 MiB resumes with a range
request starting exactly at byte 3.2 MiB. Chunk size decides work
distribution and request count; it has nothing to do with how much is lost
on interruption. What bounds the loss is state *staleness*, covered next.

## Why a crash cannot corrupt the file

The ordering invariant: **saved state always lags the bytes on disk.** A
worker writes with `WriteAt` first, advances the chunk's `done` counter
after the write returns, and the saver goroutine snapshots those counters
after that (every `stateSaveInterval`, 500 ms). At no point does the sidecar
ever claim a byte that isn't already in the part file.

So consider the two ways a session ends:

- **Clean interruption** (Ctrl-C, network failure): the saver's final flush
  records the exact counters. Resume loses nothing.
- **Hard crash** (`kill -9`, power loss): the sidecar is at most ~500 ms
  stale. Resume re-requests up to 500 ms worth of bytes per then-active
  chunk — bytes that are *already on disk* — and `WriteAt` overwrites them
  with identical data.

The failure mode is therefore a little redundant transfer, never a wrong
byte. There is no fsync choreography and no write-ahead log because the
design doesn't need one: replaying the tail is idempotent. (The sidecar
itself is written via temp-file-plus-rename in `state.save`, so a crash
mid-save leaves the previous snapshot intact rather than a truncated file.)

The knob for tighter crash bounds is `stateSaveInterval`, not smaller
chunks.

## Validators: refusing to resume into a different file

Resuming assumes the remote bytes haven't changed since the first session.
That assumption is checked, not hoped for. The first probe's `ETag` and
`Last-Modified` are stored in the sidecar; on resume, a fresh probe must
match — with the *both-sides rule*: each validator is compared only when
both the sidecar and the current server supply it, so a server that stops
sending an ETag degrades to the size check instead of stranding the
download. Size is always compared. Any mismatch (URL, size, validators, or
an incoherent chunk grid) is a hard refusal, `ErrStateMismatch` — the CLI
turns this into "use `--restart`". A same-size same-validator edit is the
accepted residual risk; nothing short of full re-hashing closes it.

The local half of the same check: the sidecar's counters describe bytes in
the part file, so `Run` also requires the part file's size to equal the
saved size *before* preallocating (which would mask the evidence by
resizing). A deleted or truncated part with a leftover sidecar would
otherwise "resume" into a hole-filled file and report success.

## The sidecar and the `.part` invariant

For output `video.mp4`, two artifacts exist during a download:

- `video.mp4.mtd.part` (`PartPath`) — the data, preallocated to full size,
  written concurrently by all workers.
- `video.mp4.mtd.json` (`StatePath`) — the sidecar:

```json
{
  "url": "...", "size": 123456789,
  "etag": "\"...\"", "lastModified": "...",
  "chunkSize": 8388608,
  "done": [8388608, 3355443, 0, ...]
}
```

`chunkSize` is persisted so a resume rebuilds the *same* chunk grid the
counters were recorded against, even if a different default would be derived
today.

The final name is created exactly one way: the completion rename in
`finalize`, after the last byte is written. That yields the invariant that
makes every other rule simple — **a file at the final name is always a
complete download**. From it: an existing output file is either finished
data or an unrelated file, so `Run` refuses to start over it without
`Force` (`ErrOutputExists`); a partial download is visibly partial; and if
the final name appears mid-download, the rename is refused and part +
sidecar are kept together, so a `-f` rerun "resumes" (instantly — all
chunks done) and retries only the rename. Completed data is never lost and
existing files are never silently clobbered.

Order matters in the endgame: rename first, delete the sidecar second. If
the rename fails, both artifacts survive as a coherent pair; a sidecar
deleted early would have left an orphaned part file that could only be
re-downloaded.
