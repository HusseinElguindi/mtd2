package downloader

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Rolling-window shape: the sampler records a sample every sampleInterval
// and Progress() computes rates as deltas between now and the oldest
// retained sample, so displayed speeds average over roughly the last
// rateWindow rather than jittering per refresh or flattening into a
// whole-session average. Until the window fills, the oldest sample is the
// session start, which degrades gracefully to a since-start average.
const (
	sampleInterval = 250 * time.Millisecond
	rateWindow     = 5 * time.Second
	maxSamples     = int(rateWindow / sampleInterval)
)

// ChunkState describes where a chunk is in its lifecycle.
type ChunkState string

const (
	ChunkPending ChunkState = "pending"
	ChunkActive  ChunkState = "active"
	ChunkDone    ChunkState = "done"
)

// ChunkProgress is a point-in-time view of one chunk.
type ChunkProgress struct {
	Index  int
	Offset int64
	Length int64
	Done   int64
	State  ChunkState
}

// Progress is a point-in-time view of the whole download. The three rates
// deliberately use different denominators:
//
//   - DownloadRate is bytes of progress per wall-clock second — what the
//     user experiences.
//   - NetworkRate is bytes read per second spent blocked in body reads,
//     summed across workers — per-connection socket throughput, higher
//     than DownloadRate whenever concurrency is helping.
//   - DiskRate is bytes written per second spent inside WriteAt — the
//     page-cache/device throughput. Its (typically huge) gap above
//     DownloadRate is evidence the download is network-bound.
type Progress struct {
	Total      int64
	Downloaded int64
	Chunks     []ChunkProgress

	DownloadRate float64 // bytes/wall-second, rolling window
	NetworkRate  float64 // bytes/second-spent-reading, rolling window
	DiskRate     float64 // bytes/second-spent-writing, rolling window
	AvgRate      float64 // bytes/wall-second since this session started
}

// sample is one row of the rolling window: cumulative counters at time t.
type sample struct {
	t          time.Time
	downloaded int64
	read       int64
	readNanos  int64
	written    int64
	writeNanos int64
}

// tracker accumulates transfer metrics. Workers bump the atomics on the
// hot path (two time.Now pairs per 256 KiB buffer cycle — negligible);
// everything else happens under mu at sampling/snapshot frequency.
type tracker struct {
	bytesRead  atomic.Int64
	readNanos  atomic.Int64
	bytesWrite atomic.Int64
	writeNanos atomic.Int64

	mu        sync.Mutex
	chunks    []*chunk
	total     int64
	start     time.Time
	startDone int64
	ring      []sample
}

// begin arms the tracker for a session over the given chunks and returns
// a stop function; a sampler goroutine maintains the rolling window until
// stopped.
func (tr *tracker) begin(chunks []*chunk, total int64) (stop func()) {
	tr.mu.Lock()
	tr.chunks = chunks
	tr.total = total
	tr.start = time.Now()
	tr.startDone = sumDone(chunks)
	tr.ring = append(tr.ring[:0], tr.sampleLocked())
	tr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				tr.mu.Lock()
				tr.ring = append(tr.ring, tr.sampleLocked())
				if len(tr.ring) > maxSamples {
					tr.ring = tr.ring[1:]
				}
				tr.mu.Unlock()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func sumDone(chunks []*chunk) int64 {
	var n int64
	for _, c := range chunks {
		n += c.done.Load()
	}
	return n
}

func (tr *tracker) sampleLocked() sample {
	return sample{
		t:          time.Now(),
		downloaded: sumDone(tr.chunks),
		read:       tr.bytesRead.Load(),
		readNanos:  tr.readNanos.Load(),
		written:    tr.bytesWrite.Load(),
		writeNanos: tr.writeNanos.Load(),
	}
}

// snapshot builds a Progress from a fresh sample against the oldest
// retained one, so rates are current as of the call, not the last tick.
func (tr *tracker) snapshot() Progress {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	p := Progress{Total: tr.total}
	if tr.chunks == nil {
		return p // before begin: nothing to report yet
	}
	now := tr.sampleLocked()
	p.Downloaded = now.downloaded
	p.Chunks = make([]ChunkProgress, len(tr.chunks))
	for i, c := range tr.chunks {
		cp := ChunkProgress{Index: c.index, Offset: c.offset, Length: c.length, Done: c.done.Load()}
		switch {
		case cp.Done >= cp.Length && cp.Length > 0:
			cp.State = ChunkDone
		case c.active.Load():
			cp.State = ChunkActive
		default:
			cp.State = ChunkPending
		}
		p.Chunks[i] = cp
	}

	oldest := tr.ring[0]
	p.DownloadRate = rate(now.downloaded-oldest.downloaded, now.t.Sub(oldest.t).Nanoseconds())
	p.NetworkRate = rate(now.read-oldest.read, now.readNanos-oldest.readNanos)
	p.DiskRate = rate(now.written-oldest.written, now.writeNanos-oldest.writeNanos)
	p.AvgRate = rate(now.downloaded-tr.startDone, now.t.Sub(tr.start).Nanoseconds())
	return p
}

// rate converts a byte delta over a nanosecond delta to bytes/second,
// returning 0 when no time has been observed.
func rate(bytes, nanos int64) float64 {
	if nanos <= 0 {
		return 0
	}
	return float64(bytes) / (float64(nanos) / float64(time.Second))
}

// Progress returns a point-in-time snapshot of the download. It is safe
// to call concurrently with Run; before Run has probed and started, only
// zero values are returned.
func (d *Downloader) Progress() Progress {
	return d.prog.snapshot()
}

// timedReader counts bytes and time spent in the underlying Read — the
// numerator and denominator of NetworkRate.
type timedReader struct {
	r     io.Reader
	bytes *atomic.Int64
	nanos *atomic.Int64
}

func (t timedReader) Read(p []byte) (int, error) {
	t0 := time.Now()
	n, err := t.r.Read(p)
	t.nanos.Add(time.Since(t0).Nanoseconds())
	t.bytes.Add(int64(n))
	return n, err
}
