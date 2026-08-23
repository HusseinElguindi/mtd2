package downloader

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testBlob returns size bytes of deterministic pseudo-random data.
func testBlob(size int) []byte {
	blob := make([]byte, size)
	rand.New(rand.NewSource(42)).Read(blob)
	return blob
}

func hashFile(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash output: %v", err)
	}
	return [32]byte(h.Sum(nil))
}

func TestConcurrentDownload(t *testing.T) {
	blob := testBlob(4 << 20) // 4 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{
		URL:       srv.URL,
		Output:    out,
		ChunkSize: 128 << 10, // small chunks so the pool actually runs concurrently
		Client:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
	if want := (4 << 20) / (128 << 10); len(d.chunks) != want {
		t.Errorf("chunks = %d, want %d", len(d.chunks), want)
	}
	for _, c := range d.chunks {
		if c.done.Load() != c.length {
			t.Errorf("chunk %d done = %d, want %d", c.index, c.done.Load(), c.length)
		}
	}
}

func TestSingleStreamFallback(t *testing.T) {
	blob := testBlob(1 << 20) // 1 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore Range headers entirely: plain 200 with the full body.
		w.Write(blob)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL, Output: out, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
}

func TestChunkMath(t *testing.T) {
	// Uneven division: last chunk carries the remainder.
	chunks := buildChunks(2_500_000, 1<<20)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	var total int64
	for i, c := range chunks {
		if c.index != i {
			t.Errorf("chunk %d index = %d", i, c.index)
		}
		if c.offset != int64(i)<<20 {
			t.Errorf("chunk %d offset = %d, want %d", i, c.offset, int64(i)<<20)
		}
		total += c.length
	}
	if total != 2_500_000 {
		t.Errorf("chunk lengths sum to %d, want 2500000", total)
	}
	if last := chunks[2].length; last != 2_500_000-2<<20 {
		t.Errorf("last chunk length = %d, want %d", last, 2_500_000-2<<20)
	}

	// Derived default: ~4 chunks per worker, clamped to [8 MiB, 64 MiB].
	d := &Downloader{opts: Options{Concurrency: 8}}
	tests := []struct {
		size, want int64
	}{
		{1 << 20, minChunkSize}, // tiny file clamps up
		{1 << 30, 1 << 30 / 32}, // 1 GiB / (8*4) = 32 MiB, in range
		{1 << 40, maxChunkSize}, // huge file clamps down
	}
	for _, tt := range tests {
		if got := d.chunkSize(tt.size); got != tt.want {
			t.Errorf("chunkSize(%d) = %d, want %d", tt.size, got, tt.want)
		}
	}

	d.opts.ChunkSize = 12345
	if got := d.chunkSize(1 << 30); got != 12345 {
		t.Errorf("chunkSize override = %d, want 12345", got)
	}
}

func TestChunkRetry(t *testing.T) {
	blob := testBlob(512 << 10)
	var failed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !failed && r.Header.Get("Range") != "bytes=0-0" {
			// Cut the first real chunk request short (the probe's 0-0
			// request passes through) to exercise a retry.
			failed = true
			w.Header().Set("Content-Range", "bytes 0-524287/524288")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(blob[:100])
			return
		}
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL, Output: out, Concurrency: 1, ChunkSize: 512 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch after retry: got %x, want %x", got, want)
	}
}

func TestRedirectWithCookie(t *testing.T) {
	// Model the flow that 400s without a cookie jar: the entry URL 302s,
	// Set-Cookie on the redirect carries a ticket, and the target rejects
	// any request that doesn't present it.
	blob := testBlob(1 << 20)
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ticket", Value: "ok", Path: "/"})
		http.Redirect(w, r, "/blob", http.StatusFound)
	})
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("ticket"); err != nil || c.Value != "ok" {
			http.Error(w, "missing ticket", http.StatusBadRequest)
			return
		}
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	// No Client passed: this exercises the library's default (jarred) client.
	d, err := New(Options{URL: srv.URL + "/start", Output: out, ChunkSize: 256 << 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
}
