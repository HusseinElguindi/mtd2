package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// writeBufSize is the size of each worker's coalescing buffer: reads from
// the response body are accumulated until the buffer is full (or the body
// ends), then written with a single file.WriteAt.
//
// Why this write path looks the way it does:
//
//   - Page cache, not O_DIRECT: WriteAt is a pwrite, which is a memcpy into
//     kernel page-cache pages; the kernel batches and schedules the actual
//     disk flush on its own. There is no user-visible "disk buffer size" to
//     match, so plain buffered writes are the right tool and O_DIRECT would
//     only add alignment constraints and lose the kernel's write-behind.
//
//   - Coalescing matters: TLS hands Read ~16 KiB per call (one TLS record),
//     so an uncoalesced read-then-write loop issues ~16 KiB writes no matter
//     how large a buffer it passes to Read. Filling the buffer first (an
//     io.ReadFull-style loop) is what makes the buffer size meaningful.
//
//   - Page alignment helps: a write covering whole pages lets the kernel
//     install and dirty full pages outright; a partial-page write to a page
//     that is not resident forces a read-modify-write to fill in the rest.
//     256 KiB is a common multiple of 4 KiB (Linux) and 16 KiB (Apple
//     Silicon) pages, and MiB-aligned chunk offsets plus full-buffer writes
//     keep every write page-aligned and a whole multiple of the page size.
//
//   - Why 256 KiB and not more or less: at >=64 KiB the ~2 us syscall cost
//     is already negligible per write; going past ~1 MiB buys nothing and
//     coarsens progress and crash-resume granularity (bytes are only counted
//     done after they are written); per-worker memory stays trivial.
const writeBufSize = 256 * 1024

// Chunk-size derivation bounds. The default chunk size targets ~4 chunks
// per worker (size / (concurrency*4)) so a slow stream doesn't become a
// long single-connection tail, clamped so chunks stay large enough that
// per-request latency (~1 RTT on a pooled keep-alive connection) is
// negligible.
const (
	minChunkSize = 8 << 20  // 8 MiB
	maxChunkSize = 64 << 20 // 64 MiB
)

// chunkRetries is the number of attempts made per chunk before giving up;
// retries re-range from the chunk's done counter so no bytes are re-fetched.
const chunkRetries = 3

// retryBackoff is the base delay between chunk retry attempts; attempt n
// waits n * retryBackoff.
const retryBackoff = 250 * time.Millisecond

// chunk is one contiguous byte range of the output file, downloaded
// front-to-back by a single worker. done is the count of contiguous bytes
// completed from the chunk's start, advanced atomically after each write —
// so one number fully describes the chunk's progress, and a range request
// starting at offset+done resumes it exactly.
type chunk struct {
	index  int
	offset int64
	length int64
	done   atomic.Int64
	active atomic.Bool // a worker is currently fetching this chunk
}

// Options configures a Downloader.
type Options struct {
	// URL is the resource to download. Required.
	URL string
	// Output is the path of the file to write. Required.
	Output string
	// Concurrency is the number of parallel range workers. Defaults to 8.
	Concurrency int
	// ChunkSize overrides the size-derived default chunk size when > 0.
	ChunkSize int64
	// Client is the HTTP client used for all requests; all workers share
	// it so connections are pooled. Defaults to http.DefaultClient.
	Client *http.Client
}

// NewClient returns the HTTP client the downloader uses by default,
// tuned for download flows a stock http.Client mishandles:
//
//   - A cookie jar: redirect flows commonly Set-Cookie on the 302 (session
//     token, signed ticket) and expect it back on the redirected request.
//   - No Referer on redirect hops: Go's client adds one automatically when
//     following a redirect (a fresh request to the same URL has none), and
//     some signed-URL hosts reject referred requests — making a followed
//     redirect fail where pasting the Location URL works. curl sends no
//     Referer either.
//
// transport is the underlying RoundTripper; nil means http.DefaultTransport.
func NewClient(transport http.RoundTripper) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Jar:       jar,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			req.Header.Del("Referer")
			// Location headers in the wild carry literal spaces and other
			// bytes that are illegal in a request target (e.g. a filename
			// in a query parameter). curl's parser normalizes them; Go's
			// net/url passes RawQuery through verbatim, and a raw space in
			// an HTTP/2 :path is a malformed request that servers reject
			// with an opaque 400 before any handler runs. Escape just the
			// illegal bytes here, in place, before the hop is sent — this
			// also cleans the FinalURL that chunk requests reuse.
			req.URL.RawQuery = escapeIllegalQuery(req.URL.RawQuery)
			return nil
		},
	}, nil
}

// escapeIllegalQuery percent-encodes only the bytes that cannot appear
// literally in an HTTP request target's query component: controls and
// space (<= 0x20), DEL and non-ASCII (>= 0x7f), and the few characters
// HTTP forbids raw (" < > \ ^ ` { | }). Everything else — including
// [ ] ( ) ! ' and existing %XX escapes — passes through untouched:
// re-encoding or reordering parameters can invalidate signed URLs, and
// matching curl's tolerance is the interoperable choice.
func escapeIllegalQuery(q string) string {
	illegal := func(c byte) bool {
		return c <= 0x20 || c >= 0x7f || strings.IndexByte("\"<>\\^`{|}", c) >= 0
	}
	i := 0
	for i < len(q) && !illegal(q[i]) {
		i++
	}
	if i == len(q) {
		return q
	}
	var b strings.Builder
	b.WriteString(q[:i])
	for ; i < len(q); i++ {
		if c := q[i]; illegal(c) {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Downloader downloads one URL to one output file, in concurrent byte-range
// chunks when the server supports them, falling back to a single stream
// otherwise.
type Downloader struct {
	opts   Options
	client *http.Client

	size   int64
	chunks []*chunk
	prog   tracker

	// fetchURL is the redirect-resolved URL all data requests go to; it
	// starts as the probe's FinalURL and is refreshed by re-probing when
	// a chunk retries (in case a signed URL expired mid-download). The
	// user-supplied opts.URL remains the download's identity (state file,
	// resume validation).
	fetchMu  sync.Mutex
	fetchURL string

	// origProbe is the session's first probe; URL refreshes are accepted
	// only when a fresh probe still matches it (see probeMatches).
	origProbe    ProbeResult
	refreshGroup singleflight.Group
}

func (d *Downloader) getFetchURL() string {
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()
	return d.fetchURL
}

func (d *Downloader) setFetchURL(u string) {
	d.fetchMu.Lock()
	d.fetchURL = u
	d.fetchMu.Unlock()
}

// refreshFetchURL re-resolves the redirect chain from the original URL,
// picking up a fresh ticket/signature if the old one stopped working. A
// failed refresh keeps the current URL — the retry then fails through the
// normal path with the real error.
// Concurrent retries collapse into one probe via singleflight: several
// workers failing at once (one server blip) must not hammer the entry URL
// with parallel re-mints it may rate-limit.
func (d *Downloader) refreshFetchURL(ctx context.Context) {
	d.refreshGroup.Do("refresh", func() (any, error) {
		probe, err := Probe(ctx, d.client, d.opts.URL)
		if err != nil || probe.FinalURL == "" {
			return nil, nil
		}
		// Adopt the fresh URL only if it still describes the same
		// resource: a re-mint resolving to different content (origin file
		// changed, another variant) must not be spliced into the existing
		// chunk grid — that would mix bytes of two files and report
		// success.
		if probeMatches(d.origProbe, probe) {
			d.setFetchURL(probe.FinalURL)
		}
		return nil, nil
	})
}

// badResponseError marks a server response that suggests the fetch URL
// went stale (expired ticket, wrong resource) — the one failure class
// where re-resolving the redirect chain can help a retry.
type badResponseError struct{ msg string }

func (e *badResponseError) Error() string { return e.msg }

// probeMatches reports whether two probes plausibly describe the same
// resource: sizes must match, and each validator is compared when both
// probes carry it.
func probeMatches(a, b ProbeResult) bool {
	switch {
	case a.Size != b.Size:
		return false
	case a.ETag != "" && b.ETag != "" && a.ETag != b.ETag:
		return false
	case a.LastModified != "" && b.LastModified != "" && a.LastModified != b.LastModified:
		return false
	}
	return true
}

// New returns a Downloader for the given options, applying defaults.
func New(opts Options) (*Downloader, error) {
	if opts.URL == "" {
		return nil, errors.New("downloader: URL is required")
	}
	if opts.Output == "" {
		return nil, errors.New("downloader: Output is required")
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	// A pasted URL can carry the same illegal query bytes a redirect can
	// (net/url keeps RawQuery verbatim); sanitize it once so every request
	// and the state file's identity use the escaped form consistently.
	if u, err := url.Parse(opts.URL); err == nil {
		u.RawQuery = escapeIllegalQuery(u.RawQuery)
		opts.URL = u.String()
	}
	client := opts.Client
	if client == nil {
		var err error
		if client, err = NewClient(nil); err != nil {
			return nil, err
		}
	}
	return &Downloader{opts: opts, client: client}, nil
}

// Run probes the server and downloads the resource to the output file,
// blocking until the download completes, fails, or ctx is cancelled.
func (d *Downloader) Run(ctx context.Context) error {
	probe, err := Probe(ctx, d.client, d.opts.URL)
	if err != nil {
		return err
	}
	d.size = probe.Size
	d.origProbe = probe
	d.setFetchURL(probe.FinalURL)

	f, err := os.OpenFile(d.opts.Output, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if !probe.RangesSupported || probe.Size <= 0 {
		if err := d.singleStream(ctx, f); err != nil {
			return err
		}
		// A stale state file (from when the server still supported
		// ranges) describes data this full re-download just replaced.
		os.Remove(StatePath(d.opts.Output))
		return f.Close()
	}

	// Resume: a valid state file restores each chunk's done counter (and
	// pins the chunk grid to the one it was saved with); an invalid one is
	// a hard error so a changed remote never corrupts the partial file.
	statePath := StatePath(d.opts.Output)
	cs := d.chunkSize(probe.Size)
	st, err := loadState(statePath)
	if err != nil {
		return err
	}
	if st != nil {
		if err := st.validate(d.opts.URL, probe); err != nil {
			return err
		}
		// The state's done counters describe bytes already in the output
		// file, so the pair must match locally too: a deleted or resized
		// output with a leftover sidecar would "resume" into a hole-filled
		// file and report success. This check must precede the Truncate
		// below, which would silence it by resizing the file.
		if fi, err := f.Stat(); err != nil || fi.Size() != st.Size {
			return fmt.Errorf("%w: output file %s does not match the saved state (delete %s to restart)",
				ErrStateMismatch, d.opts.Output, statePath)
		}
		cs = st.ChunkSize
	}

	// Preallocate with Truncate before workers start: it fixes i_size up
	// front so concurrent WriteAt calls at any offset are plain in-bounds
	// writes with no i_size-extension lock contention, and on filesystems
	// with sparse-file support the file is holes, so a partial-page write
	// into a never-written region doesn't trigger a read of existing data.
	if err := f.Truncate(probe.Size); err != nil {
		return fmt.Errorf("preallocate %s: %w", d.opts.Output, err)
	}
	d.chunks = buildChunks(probe.Size, cs)
	if st != nil {
		st.restore(d.chunks)
	} else {
		st = &state{
			URL:          d.opts.URL,
			Size:         probe.Size,
			ETag:         probe.ETag,
			LastModified: probe.LastModified,
			ChunkSize:    cs,
			Done:         make([]int64, len(d.chunks)),
		}
	}

	stopProg := d.prog.begin(d.chunks, probe.Size)
	defer stopProg()

	stop := make(chan struct{})
	saverDone := make(chan struct{})
	go func() {
		defer close(saverDone)
		st.runSaver(statePath, d.chunks, stop)
	}()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(d.opts.Concurrency)
	for _, c := range d.chunks {
		if c.done.Load() >= c.length {
			continue // already complete (e.g. restored from state)
		}
		g.Go(func() error { return d.downloadChunk(ctx, f, c) })
	}
	err = g.Wait()

	// Stop the saver and wait for its final flush before deciding the
	// state file's fate: on failure it holds the resume point; on success
	// it is obsolete.
	close(stop)
	<-saverDone
	if err != nil {
		return err
	}
	os.Remove(statePath)
	return f.Close()
}

// chunkSize returns the configured chunk size, or the size-derived default:
// ~4 chunks per worker, clamped to [minChunkSize, maxChunkSize].
func (d *Downloader) chunkSize(size int64) int64 {
	if d.opts.ChunkSize > 0 {
		return d.opts.ChunkSize
	}
	cs := size / int64(d.opts.Concurrency*4)
	return min(max(cs, minChunkSize), maxChunkSize)
}

// buildChunks divides [0, size) into fixed-size chunks; the last chunk
// carries the remainder.
func buildChunks(size, chunkSize int64) []*chunk {
	n := (size + chunkSize - 1) / chunkSize
	chunks := make([]*chunk, 0, n)
	for off := int64(0); off < size; off += chunkSize {
		chunks = append(chunks, &chunk{
			index:  len(chunks),
			offset: off,
			length: min(chunkSize, size-off),
		})
	}
	return chunks
}

// downloadChunk fetches one chunk, retrying transient errors up to
// chunkRetries attempts with brief backoff. Each retry re-ranges from the
// chunk's current done counter, so no completed bytes are re-fetched.
func (d *Downloader) downloadChunk(ctx context.Context, f *os.File, c *chunk) error {
	c.active.Store(true)
	defer c.active.Store(false)

	var lastErr error
	for attempt := 1; attempt <= chunkRetries; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt-1) * retryBackoff):
			}
			// Re-resolve the redirect chain only when the server's
			// response suggests a stale URL (expired ticket); local I/O
			// errors and truncated bodies gain nothing from a re-probe,
			// and entry URLs often rate-limit ticket re-mints.
			var bad *badResponseError
			if errors.As(lastErr, &bad) {
				d.refreshFetchURL(ctx)
			}
		}
		lastErr = d.fetchChunk(ctx, f, c)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return lastErr
		}
	}
	return fmt.Errorf("chunk %d: %w (after %d attempts)", c.index, lastErr, chunkRetries)
}

// fetchChunk issues one range request for the chunk's remaining bytes and
// copies the body to the file via the coalescing write loop.
func (d *Downloader) fetchChunk(ctx context.Context, f *os.File, c *chunk) error {
	start := c.offset + c.done.Load()
	end := c.offset + c.length - 1
	if start > end {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.getFetchURL(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return &badResponseError{fmt.Sprintf("range request: unexpected status %s", resp.Status)}
	}
	// A 206 alone doesn't prove the server honored our start offset: one
	// that ignores the range start (easiest to hit on a retry, where
	// start sits mid-chunk) would have its byte 0 written at our offset,
	// corrupting the file with no error. Require the echoed start.
	switch gotStart, err := parseContentRangeStart(resp.Header.Get("Content-Range")); {
	case err != nil:
		return &badResponseError{fmt.Sprintf("range request: %v", err)}
	case gotStart != start:
		return &badResponseError{fmt.Sprintf("range request: server range starts at %d, requested %d", gotStart, start)}
	}

	// Bound the body to the bytes we asked for: a server that honors the
	// range start but streams to EOF would otherwise be written past the
	// chunk's end, silently overwriting regions other workers own (and
	// are possibly writing concurrently).
	if err := d.copyToFile(f, io.LimitReader(resp.Body, end-start+1), start, &c.done); err != nil {
		return err
	}
	if got := c.done.Load(); got < c.length {
		return fmt.Errorf("body ended early: %d of %d bytes", got, c.length)
	}
	return nil
}

// copyToFile is the coalescing write loop: it fills a writeBufSize buffer
// from body (io.ReadFull accumulates the ~16 KiB reads TLS delivers), then
// writes the full buffer with one positional f.WriteAt — short-write-safe
// and cursor-free, so the shared file handle needs no mutex — and advances
// done after each write. See the writeBufSize doc comment for why the
// buffer is 256 KiB and why full-buffer WriteAt is the shape of this loop.
func (d *Downloader) copyToFile(f *os.File, body io.Reader, off int64, done *atomic.Int64) error {
	src := timedReader{r: body, bytes: &d.prog.bytesRead, nanos: &d.prog.readNanos}
	buf := make([]byte, writeBufSize)
	for {
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			t0 := time.Now()
			_, werr := f.WriteAt(buf[:n], off)
			d.prog.writeNanos.Add(time.Since(t0).Nanoseconds())
			if werr != nil {
				return werr
			}
			d.prog.bytesWrite.Add(int64(n))
			off += int64(n)
			done.Add(int64(n))
		}
		switch rerr {
		case nil:
			continue
		case io.EOF, io.ErrUnexpectedEOF:
			return nil
		default:
			return rerr
		}
	}
}

// singleStream downloads the whole resource with one plain GET, for servers
// without range support (or with unknown size). Chunked resume is not
// possible in this mode; the body is streamed sequentially through the same
// coalescing write loop. When the size is known the file is represented as
// a single chunk so progress reporting still works.
func (d *Downloader) singleStream(ctx context.Context, f *os.File) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.getFetchURL(), nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: unexpected status %s", d.opts.URL, resp.Status)
	}

	size := resp.ContentLength
	if size < 0 {
		size = d.size
	}
	c := &chunk{length: size}
	c.active.Store(true)
	defer c.active.Store(false)
	d.chunks = []*chunk{c}
	stopProg := d.prog.begin(d.chunks, max(size, 0))
	defer stopProg()

	if size > 0 {
		if err := f.Truncate(size); err != nil {
			return fmt.Errorf("preallocate %s: %w", d.opts.Output, err)
		}
	}
	if err := d.copyToFile(f, resp.Body, 0, &c.done); err != nil {
		return err
	}
	if size > 0 && c.done.Load() < size {
		return fmt.Errorf("body ended early: %d of %d bytes", c.done.Load(), size)
	}
	return nil
}
