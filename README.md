# mtd

A concurrent, resumable HTTP downloader. Files are fetched in parallel
byte-range chunks, written to a shared preallocated file with positional
writes (no locks), and progress is persisted so interrupted downloads
resume where they left off.

## Build

```
go build ./cmd/mtd
go build ./cmd/mtdserve   # optional: local test server
```

## Usage

```
mtd [-o output] [-c concurrency] [-s chunk-size] [-f] [--restart] <url>
```

| Flag | Meaning |
|------|---------|
| `-o` | output file (default: last URL path element) |
| `-c` | number of parallel connections (default 8) |
| `-s` | chunk size, e.g. `16MiB` (default: derived from file size) |
| `-f` | overwrite an existing output file |
| `--restart` | discard saved state and partial data, start over |
| `-v` | dump HTTP request/response headers to stderr |

## Resume

Interrupt a download (Ctrl-C) and rerun the same command: it picks up
from the saved state in `<output>.mtd.json`, refusing if the remote file
changed. Downloads are staged in `<output>.mtd.part` and renamed on
completion, so a file at the final name is always a complete download.

## Local testing with mtdserve

`mtdserve` serves a deterministic blob with real Range/206 semantics and
knobs for simulating adverse networks: `--size`, `--rate` (per-connection
throttle), `--latency`, `--flaky` (kill connections mid-body),
`--no-range`, `--etag`, `--seed`. It prints the blob's SHA-256 on
startup for verifying downloads.

```
# terminal 1
./mtdserve --size 500MiB --rate 5MiB/s

# terminal 2
./mtd http://localhost:8080/blob
```

Ctrl-C the download mid-way and rerun it to watch a resume.

## Design docs

The docs cover what the code can't say — derivations, OS mechanics, and
trade-off boundaries:

- [docs/write-path.md](docs/write-path.md) — how N workers write one file
  with no locks, page-cache mechanics, and the 256 KiB buffer derivation.
- [docs/network.md](docs/network.md) — the probe, connection reuse, chunk
  sizing math, and redirect handling.
- [docs/resume.md](docs/resume.md) — the state model and why a crash can
  never corrupt output.
