# The write path: one file, N writers

Every worker writes into the same file handle concurrently, with no mutex,
no per-worker file handles, and no `BufWriter`-style userspace buffering
beyond one 256 KiB buffer per worker. This page explains why that is safe
and why the constants are what they are. The measured numbers come from the
findings notebook bundled in this repo, [`one-file-ten-threads.html`](../one-file-ten-threads.html),
which re-benchmarked the classic "why is multi-threaded file I/O slower in
Rust than Go" question: ~2.3 µs per 64 KiB `pwrite`, 98 ns per `lseek`, on
a page-cached ext4 file.

## Why concurrent writes to one fd need no locks

`os.File.WriteAt` compiles down to `pwrite(2)`, and `pwrite` carries its
offset *in the syscall*. There is no shared file cursor to position, so the
classic seek-then-write race — the reason people reach for
`Mutex<File>` — simply has nothing to race on. That is the whole trick:
the mutex in the naive design exists to protect the cursor, and `pwrite`
deletes the cursor.

Below the syscall, the kernel does its own locking, but at page
granularity, not file granularity. Two writers dirtying different pages of
the same file proceed in parallel; our workers write disjoint byte ranges
by construction (each chunk owns `[offset, offset+length)`), so they never
touch the same page except at chunk boundaries — and even there, at most
two writers briefly serialize on one page, which is nanoseconds against a
network-bound workload.

The one piece of shared file state writers *would* contend on is the file
size. Every write that extends a file must take the inode's size-update
lock, and ten workers all appending past EOF take turns on it. `Run`
sidesteps this by calling `Truncate(size)` once before any worker starts:
i_size is fixed up front, so every subsequent `WriteAt` is a plain
in-bounds write that never needs the extension path. On filesystems with
sparse-file support, `Truncate` also means the file is initially holes —
so writing into a never-written region installs fresh pages rather than
reading existing data first (see the alignment section below for why that
matters).

## Page cache, not O_DIRECT

A `pwrite` on a normally-opened file does not go to disk. It memcpys into
kernel page-cache pages, marks them dirty, and returns; the kernel flushes
them to the device later, batched and reordered however it likes. Two
consequences:

- **There is no "disk write buffer size" to tune to.** The question "what
  buffer size does the OS/disk want?" has no answer on this path — the
  kernel's write-behind machinery absorbs whatever sizes it is given and
  forms its own device-sized I/Os. The userspace buffer size matters for
  *syscall amortization*, not for matching hardware.
- **`O_DIRECT` would make everything worse.** It would impose hard
  alignment requirements, forfeit write-behind (workers would block on
  device latency instead of a memcpy), and buy nothing: the download is
  network-bound, and durability semantics don't change — a crash loses
  page-cache contents either way, which is exactly what the resume state
  file is for (see [resume.md](resume.md)).

This is also why the "disk rate" in the progress display reads in GiB/s:
it measures bytes over time spent inside `WriteAt`, i.e. memcpy-into-page-
cache throughput. Its huge gap above the download rate is the proof that
the workload is network-bound and the write path is never the bottleneck.

## Why 256 KiB (`writeBufSize`)

The buffer size has a floor, a ceiling, and an alignment constraint; 256
KiB sits comfortably inside all three.

**Floor — syscall amortization (~64 KiB).** A `pwrite` costs ~2.3 µs
regardless of size (the notebook measured 2.25–2.42 µs at 64 KiB). At 64
KiB per call that is already ~0.004% of the time it takes to pull those
bytes over a 100 MB/s link; at 256 KiB it is noise below measurement
error. Everything at or above ~64 KiB is fine; below it, syscall count
starts creeping into profiles.

**Ceiling — nothing past ~1 MiB (and real costs).** Doubling the buffer
past 256 KiB does not reduce any cost that still matters, but it does
coarsen two granularities: progress (the `done` counter advances only
after a full buffer is written) and crash-resume (bytes sitting in a
worker's buffer are lost on a crash, so bigger buffers mean more re-
downloaded bytes per stream). Per-worker memory is trivial either way
(8 × 256 KiB = 2 MiB), so memory is not the argument — granularity is.

**Alignment — a common multiple of every page size we run on.** 256 KiB is
a whole multiple of both 4 KiB (Linux) and 16 KiB (Apple Silicon macOS)
pages, and the write loop advances in full buffers. Chunk sizes are whole
MiBs by construction (the derived default rounds down to a MiB; see
`chunkSize`), so every chunk starts on a page boundary, and every
`WriteAt` except the chunk's final partial one starts on a page boundary
and covers whole pages. (A hand-set `-s` that isn't a page multiple
forfeits this — the write path still works, just with read-modify-write
at buffer seams.) A write that covers a
page entirely lets the kernel install and dirty the page outright; a
partial-page write to a page that is not resident forces the kernel to
read the page's existing contents first (read-modify-write) before
merging in the new bytes. Full-page writes into a preallocated sparse
file hit neither the read nor the merge.

## Why the loop coalesces reads (`copyToFile`)

The subtle bug this design avoids: **the buffer size you pass to `Read` is
not the size of the writes you get.** An HTTPS response body delivers at
most one TLS record per `Read` call — ~16 KiB — no matter how large the
destination slice is. A naive `read → write` loop therefore issues ~16 KiB
writes forever, and the 256 KiB buffer is decorative.

The notebook demonstrates the same failure mode from the other direction
with its `BufWriter` bypass table: a userspace buffer sized *equal to* the
I/O size never buffers anything — writes at or above capacity bypass it
straight to the kernel, so it is a memcpy you pay for with no amortization
in return. A buffer only earns its keep when it is strictly larger than
the reads pushed through it *and the loop actually fills it*.

`copyToFile` therefore fills the buffer with `io.ReadFull` — accumulating
however many 16 KiB TLS records it takes — and only then issues one
`WriteAt` for the full 256 KiB. That single decision is what makes the
buffer size real: it turns ~16 writes of one page-cache-unfriendly size
into one aligned, whole-page, syscall-amortized write.

## What this adds up to

Per 256 KiB of downloaded data, the write path costs one ~2 µs syscall and
one page-aligned memcpy into the page cache — roughly 0.001% of the time
the same bytes spend on a fast network link. The design's job is to make
disk I/O a rounding error and keep every worker's time in the only place
it can pay off: blocked on its socket. The rates in the progress display
(network vs. disk denominators) exist to verify exactly that, live.
