# The network layer: requests, chunks, redirects

This page covers the decisions the code can't explain by itself: why the
probe looks the way it does, what a chunk-sized request actually costs on
a pooled connection, how the chunk size falls out of two opposing
pressures, and why redirects are resolved exactly once. API details live
in godoc; measured write-path numbers live in [write-path.md](write-path.md).

## The probe: `GET` with `Range: bytes=0-0`

Before downloading anything, `Probe` issues a `GET` with
`Range: bytes=0-0` — a request for the first byte only. The response
answers three questions in one round trip:

- **Does the server honor ranges?** A `206 Partial Content` proves it
  end-to-end. A `200 OK` means the `Range` header was ignored, so only a
  single-stream download is possible.
- **How big is the file?** The `Content-Range` header of a 206 carries
  the total (`bytes 0-0/12345`); a 200 falls back to `Content-Length`.
- **What identifies this version?** `ETag` / `Last-Modified` are saved as
  validators so a resume can detect that the remote changed.

The obvious alternative, `HEAD`, is less reliable in practice. Many
servers and CDN configurations treat `HEAD` as an afterthought: some
disallow it outright, some omit `Content-Length` or `Accept-Ranges` on
`HEAD` while serving ranges fine, and `Accept-Ranges: bytes` is
advertisory either way — a header claiming range support is weaker
evidence than a 206 actually delivering a byte range. Asking for one real
byte tests the exact code path every chunk request will use, and costs
one byte of transfer.

## What a chunk request costs: connection reuse

All workers share one `http.Client`, which means one `http.Transport`
connection pool. After the first request to a host, its TCP+TLS
connection is kept alive and reused, so a new chunk request is **not** a
new connection — no TCP handshake, no TLS handshake. What remains is
roughly one round-trip of request latency (send headers, wait for the
response to start), plus whatever per-request work the server does.

That reuse is not free by default, though — it takes two departures from
stock Go, each worth a paragraph because each was measured, not assumed.

**The idle pool has to be sized to the worker count.** Go parks returned
connections in a per-host idle pool holding
`DefaultMaxIdleConnsPerHost` — **two** — connections. With eight workers
that pool overflows constantly: a connection handed back to a full pool is
closed rather than parked, so the worker's next chunk request has nothing
to pick up and redials. Measured over a 41-request download at concurrency
8, where the floor is 9 connections (one per worker plus the probe), the
stock pool dialed between 9 and 14 across runs; sized to `concurrency+1`
it dials 9 every time. The waste is a handshake — several RTTs of dead
time before any byte of that chunk moves — and it lands unpredictably,
which is the worse property. `NewTransport` sizes `MaxIdleConns`,
`MaxIdleConnsPerHost`, and `MaxConnsPerHost` together; the last one also
makes `-c` an honest promise, since N workers then open at most N+1
sockets rather than approximately that many.

It clones `http.DefaultTransport` rather than tuning it, because that
value is process-wide: mutating it would reconfigure every other HTTP user
in the program, including the `-v` dump path that wraps it.

**The probe's body has to be drained.** Go returns a connection to the
idle pool only after its body is read to EOF; closing a body with bytes
outstanding kills the socket. `Probe` asks for one byte and used to close
without reading it, so the handshake it paid for was discarded and the
first chunk request opened a second connection — three sequential probes
opened three connections, one when drained. The drain is bounded
(`probeDrainLimit`) and lives only in the 206 branch: a server can answer
a range request with far more than the byte asked for, and in the 200
branch the body *is* the whole file, so an unbounded drain there would
download the resource in order to recycle a socket.

That RTT is the number chunk sizing has to respect. Worked example at a
50 ms RTT with ~10 MB/s per stream:

- An 8 MiB chunk transfers in ~800 ms, so 50 ms of dead time per request
  is **~6%** overhead.
- A 32 MiB chunk transfers in ~3.2 s, so the same 50 ms is **~0.15%**.

There is also a softer cost to many small requests: entry points and CDNs
rate-limit, log, and occasionally throttle clients that issue hundreds of
requests for one file. Fewer, larger requests are friendlier and
indistinguishable in throughput.

## Chunk sizing: two opposing pressures

Chunks are the unit of work distribution, so their size is a compromise
between two failure modes:

- **Too few chunks (the load-balancing floor).** With exactly one chunk
  per worker — the classic approach — the download ends when the
  *slowest* stream finishes its fixed share. One degraded connection
  turns the tail of the download into a long single-stream crawl, with
  every other worker idle. A few chunks per worker lets fast workers pick
  up the slack.
- **Too many chunks (the overhead ceiling).** Every chunk pays the ~1 RTT
  request cost above, and hundreds of requests per file invite
  rate-limiting.

A handful of chunks per worker captures nearly all of the balancing
benefit — beyond that, more chunks only add requests. The default
therefore targets **~4 chunks per worker** (`size / (concurrency*4)`,
see the `chunkSize` method), clamped to **[8 MiB, 64 MiB]**
(`minChunkSize`, `maxChunkSize`):

- The 8 MiB floor keeps small files from degenerating into confetti and
  keeps per-request overhead comfortably sub-1% on realistic links.
- The 64 MiB ceiling keeps chunks — the unit of retry and of balancing —
  from growing so large on huge files that either becomes coarse.

At the defaults, a 1 GiB file at concurrency 8 gets 32 MiB chunks: 32
requests total, ~0.15% RTT overhead, and each worker's share still split
four ways for balancing. Chunk size does **not** set resume granularity —
resume re-enters a chunk at an exact byte offset (see
[resume.md](resume.md)).

## Redirects: resolve once, fetch from the final URL

Real download entry points frequently answer with a `302` to a minted,
ticket-like URL (a signed path on a CDN host). Two properties of that
pattern shape the design:

- **Minting is not free or unlimited.** Entry URLs that generate a fresh
  ticket per request commonly reject rapid or concurrent re-mints. A
  design where every chunk request re-follows the redirect chain sends
  `1 + chunks` mint requests — with 8 workers starting at once, that's a
  burst the entry point may refuse outright.
- **The minted URL serves many requests happily.** It's the entry point
  that is precious, not the target.

So the redirect chain is resolved **once**, by the probe:
`ProbeResult.FinalURL` records the URL that actually served the response,
and every chunk request goes there directly.

Signed URLs can expire mid-download, so a chunk *retry* may refresh the
URL by re-probing the original — with three guards, each protecting
against a specific failure:

- **Single-flighted** (`refreshFetchURL`, via `x/sync/singleflight`): if
  several workers fail at once — one server blip — their retries collapse
  into a single re-probe instead of a burst of re-mints.
- **Only on server-response errors** (`badResponseError`: an unexpected
  status, or a range the server didn't honor): a local I/O error or a
  truncated body says nothing about the URL, so re-probing on those would
  spend a mint for nothing.
- **Validator-guarded** (`probeMatches`): the fresh probe's size and
  validators must match the session's first probe, otherwise the new URL
  is refused. If the origin file changed mid-download, adopting the new
  URL would splice bytes of two different files into one output and
  report success.

## Client defaults

`NewClient` configures the default `http.Client` with three departures
from stock Go, each earned by a real failure mode; the detailed rationale
lives in the code comments there:

- A **cookie jar** (with the public-suffix list), because redirect flows
  commonly set a session cookie on the 302 that the target expects back.
- **No `Referer` on redirect hops**, matching curl, because Go adds one
  automatically and a followed redirect should not differ from pasting
  the `Location` URL by hand.
- **Escaping of illegal query bytes when following redirects**, because
  `Location` headers in the wild carry literal spaces
  (`https://example.com/file?dload=a name.mp4`) that Go would forward
  verbatim as a malformed request target, drawing an opaque `400` from
  the server's request parser.

The transport underneath comes from `NewTransport`, which adds the
pool sizing described under [connection reuse](#what-a-chunk-request-costs-connection-reuse)
above. A caller that passes its own transport — the CLI does, to layer
`-v` request dumping — should wrap `NewTransport(concurrency)` rather than
`http.DefaultTransport`, or the run silently gets a two-connection pool.

## Known gap: HTTP/2 collapses the parallel download

Unresolved, and worth knowing before reading a benchmark. `ForceAttemptHTTP2`
is on in Go's default transport, so against an HTTPS server that
negotiates h2 — most CDNs — the workers do not get N TCP connections.
They get N *streams multiplexed over one* connection, and every
per-connection bandwidth limit then applies to all of them at once. On a
server throttling 10 MiB/s per connection, the same 24 MiB download took
**574 ms over HTTP/1.1 (9 connections) and 3.90 s over HTTP/2 (1
connection)** — a 6.8x difference, with nothing in the tool reporting
anything unusual. Two smaller penalties ride along: a lost packet stalls
every stream rather than one, and Go's h2 client advertises a 4 MiB
per-stream window, capping a stream at window/RTT (~40 MiB/s at 100 ms).

Forcing HTTP/1.1 takes both `Transport.TLSNextProto = map[...]{}` (to
disable the h2 upgrade path) and `TLSClientConfig.NextProtos =
{"http/1.1"}` (to stop advertising h2 in ALPN) — set one alone and you get
either a handshake `EOF` or an HTTP/1 parser reading an h2 SETTINGS frame.
Whether to force it, and when, is undecided: h2 saves handshakes and costs
nothing against a server that rate-limits per request rather than per
connection.
