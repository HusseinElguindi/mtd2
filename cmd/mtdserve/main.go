// Command mtdserve is a local range-serving network simulator for testing
// the mtd downloader without touching the internet.
//
// It serves a deterministic pseudo-random blob at /blob via
// http.ServeContent, which provides correct Range/206 semantics for free.
// The blob is generated lazily from a seeded PRNG, so any --size works
// without allocating the blob in memory, and the content (and its SHA-256,
// printed on startup) is reproducible across runs.
//
// Flags simulate adverse network conditions: per-connection bandwidth
// throttling (--rate), per-request latency (--latency), random mid-body
// connection kills (--flaky), servers without range support (--no-range),
// and a settable ETag (--etag) to test resume validation.
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func main() {
	var (
		addr    = flag.String("addr", "localhost:8080", "listen address")
		sizeStr = flag.String("size", "64MiB", "blob size (e.g. 500MB, 1GiB)")
		rateStr = flag.String("rate", "", "per-connection bandwidth limit (e.g. 5MB/s); empty = unlimited")
		latency = flag.Duration("latency", 0, "artificial delay before serving each request")
		flaky   = flag.Float64("flaky", 0, "probability [0,1] of killing a connection mid-body")
		noRange = flag.Bool("no-range", false, "ignore Range headers (serve 200 with full body)")
		etag    = flag.String("etag", "", "ETag header value to send (quotes added if missing)")
		seed    = flag.Int64("seed", 1, "PRNG seed for blob content")
	)
	flag.Parse()

	size, err := parseSize(*sizeStr)
	if err != nil {
		log.Fatalf("invalid --size: %v", err)
	}
	var rate int64
	if *rateStr != "" {
		rate, err = parseSize(strings.TrimSuffix(*rateStr, "/s"))
		if err != nil {
			log.Fatalf("invalid --rate: %v", err)
		}
	}

	blob := &blobReader{size: size, seed: *seed}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(blob, 0, size)); err != nil {
		log.Fatalf("hashing blob: %v", err)
	}
	fmt.Printf("blob: %d bytes, sha256 %x\n", size, h.Sum(nil))
	fmt.Printf("serving http://%s/blob\n", *addr)

	modTime := time.Now()
	http.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		if *latency > 0 {
			time.Sleep(*latency)
		}
		if *noRange {
			r.Header.Del("Range")
			// ServeContent still advertises range support; overwrite after.
			w.Header().Set("Accept-Ranges", "none")
		}
		if *etag != "" {
			tag := *etag
			if !strings.HasPrefix(tag, `"`) {
				tag = `"` + tag + `"`
			}
			w.Header().Set("ETag", tag)
		}
		var out http.ResponseWriter = w
		if rate > 0 || *flaky > 0 {
			sw := &shapedWriter{ResponseWriter: w, rate: rate}
			if *flaky > 0 && rand.Float64() < *flaky {
				// Kill the connection somewhere in the body.
				sw.killAt = 1 + rand.Int63n(size)
			}
			out = sw
		}
		http.ServeContent(out, r, "blob", modTime, io.NewSectionReader(blob, 0, size))
	})

	log.Fatal(http.ListenAndServe(*addr, nil))
}

// blobReader generates a deterministic pseudo-random blob of the given size
// without storing it. Byte i of the blob is byte i%8 of the little-endian
// splitmix64 output for block i/8, keyed by seed — so reads at any offset
// are cheap and reproducible. It implements io.ReaderAt; wrap it in an
// io.SectionReader to get the io.ReadSeeker http.ServeContent needs.
type blobReader struct {
	size int64
	seed int64
}

func (b *blobReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= b.size {
		return 0, io.EOF
	}
	n := len(p)
	if rem := b.size - off; int64(n) > rem {
		n = int(rem)
	}
	for i := 0; i < n; {
		block := (off + int64(i)) / 8
		v := splitmix64(uint64(b.seed) + uint64(block))
		for j := int((off + int64(i)) % 8); j < 8 && i < n; j++ {
			p[i] = byte(v >> (8 * j))
			i++
		}
	}
	if int64(n) == b.size-off && n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// splitmix64 is the SplitMix64 mixing function: a bijective avalanche of x,
// giving high-quality pseudo-random output from sequential inputs.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// shapedWriter wraps a ResponseWriter to pace writes to a target bytes/sec
// (sleeping as needed) and, if killAt > 0, to abort the connection after
// roughly that many body bytes — exercising the downloader's retry path.
type shapedWriter struct {
	http.ResponseWriter
	rate    int64 // bytes/sec; 0 = unlimited
	killAt  int64 // kill connection after this many bytes; 0 = never
	written int64
	start   time.Time
}

func (s *shapedWriter) Write(p []byte) (int, error) {
	if s.killAt > 0 && s.written+int64(len(p)) > s.killAt {
		// Write the partial prefix, then abort the connection. ServeContent
		// swallows copy errors, so panic with ErrAbortHandler: net/http
		// recovers it quietly and drops the connection.
		if n := s.killAt - s.written; n > 0 {
			s.pace(p[:n])
			s.ResponseWriter.Write(p[:n])
		}
		panic(http.ErrAbortHandler)
	}
	if err := s.pace(p); err != nil {
		return 0, err
	}
	n, err := s.ResponseWriter.Write(p)
	s.written += int64(n)
	return n, err
}

// pace sleeps long enough that cumulative throughput stays at or below rate.
func (s *shapedWriter) pace(p []byte) error {
	if s.rate <= 0 {
		return nil
	}
	if s.start.IsZero() {
		s.start = time.Now()
	}
	// Time at which (written + len(p)) bytes are allowed to have been sent.
	due := s.start.Add(time.Duration(float64(s.written+int64(len(p))) / float64(s.rate) * float64(time.Second)))
	if d := time.Until(due); d > 0 {
		time.Sleep(d)
	}
	return nil
}

// parseSize parses a human-readable byte count like "500MB", "8MiB", or a
// bare number. Decimal (KB/MB/GB) and binary (KiB/MiB/GiB) suffixes are
// supported, case-insensitively; "K"/"M"/"G" alone mean the binary unit.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
		{"B", 1}, {"", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			if num == "" {
				continue
			}
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				continue
			}
			if f < 0 {
				return 0, errors.New("negative size")
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	return 0, fmt.Errorf("cannot parse size %q", s)
}
