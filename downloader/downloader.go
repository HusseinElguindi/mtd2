package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
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

// Downloader downloads one URL to one output file, in concurrent byte-range
// chunks when the server supports them, falling back to a single stream
// otherwise.
type Downloader struct {
	opts   Options
	client *http.Client

	size   int64
	chunks []*chunk
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
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
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

	f, err := os.OpenFile(d.opts.Output, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if !probe.RangesSupported || probe.Size <= 0 {
		if err := d.singleStream(ctx, f); err != nil {
			return err
		}
		return f.Close()
	}

	// Preallocate with Truncate before workers start: it fixes i_size up
	// front so concurrent WriteAt calls at any offset are plain in-bounds
	// writes with no i_size-extension lock contention, and on filesystems
	// with sparse-file support the file is holes, so a partial-page write
	// into a never-written region doesn't trigger a read of existing data.
	if err := f.Truncate(probe.Size); err != nil {
		return fmt.Errorf("preallocate %s: %w", d.opts.Output, err)
	}

	d.chunks = buildChunks(probe.Size, d.chunkSize(probe.Size))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(d.opts.Concurrency)
	for _, c := range d.chunks {
		if c.done.Load() >= c.length {
			continue // already complete (e.g. restored from state)
		}
		g.Go(func() error { return d.downloadChunk(ctx, f, c) })
	}
	if err := g.Wait(); err != nil {
		return err
	}
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
	var lastErr error
	for attempt := 1; attempt <= chunkRetries; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt-1) * retryBackoff):
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.URL, nil)
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
		return fmt.Errorf("range request: unexpected status %s", resp.Status)
	}

	if err := d.copyToFile(f, resp.Body, start, &c.done); err != nil {
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
	buf := make([]byte, writeBufSize)
	for {
		n, rerr := io.ReadFull(body, buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.URL, nil)
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
	d.chunks = []*chunk{c}

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
