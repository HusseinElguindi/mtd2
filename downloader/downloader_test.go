package downloader

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	for _, leftover := range []string{PartPath(out), StatePath(out)} {
		if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still present after completion (err=%v)", leftover, err)
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

func TestRedirectResolvedOnce(t *testing.T) {
	// Model an entry URL that mints a single-use redirect target and 400s
	// on re-mints (as many ticketed download services do): the probe may
	// hit /start once, and every chunk request must go straight to the
	// minted URL — with 8 chunks, re-following the redirect per chunk
	// would fail immediately.
	blob := testBlob(2 << 20)
	var mints atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if mints.Add(1) > 1 {
			http.Error(w, "ticket already issued", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/v1/ticket-abc", http.StatusFound)
	})
	mux.HandleFunc("/v1/ticket-abc", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL + "/start", Output: out, ChunkSize: 256 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("entry URL hit %d times, want exactly 1 (the probe)", n)
	}
}

func TestNoRefererOnRedirect(t *testing.T) {
	// The default client follows redirects without a Referer, matching
	// curl (Go adds one to followed hops by default). This is deliberate
	// hygiene for picky hosts, not the fix for any observed failure —
	// the 400-via-redirect bug turned out to be illegal query bytes; see
	// TestRedirectLocationWithIllegalQuery.
	blob := testBlob(1 << 20)
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/ticket", http.StatusFound)
	})
	mux.HandleFunc("/v1/ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" {
			http.Error(w, "referred requests not allowed", http.StatusBadRequest)
			return
		}
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	// No Client passed: exercises the library's default client.
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

func TestEscapeIllegalQuery(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a=1&b=2", "a=1&b=2"}, // clean: untouched (and same backing string)
		{"dload=SITE - [abc] Title! (360).mp4", "dload=SITE%20-%20[abc]%20Title!%20(360).mp4"},
		{"sig=a%2Bb%2F", "sig=a%2Bb%2F"},                   // existing escapes preserved, not double-encoded
		{"k=[]()!'*,;", "k=[]()!'*,;"},                     // sub-delims curl tolerates stay literal
		{"k=a\tb", "k=a%09b"},                              // control byte
		{"k=caf\xc3\xa9", "k=caf%C3%A9"},                   // non-ASCII
		{"k=\"<>\\^`{|}", "k=%22%3C%3E%5C%5E%60%7B%7C%7D"}, // HTTP-forbidden raw bytes
	}
	for _, tt := range tests {
		if got := escapeIllegalQuery(tt.in); got != tt.want {
			t.Errorf("escapeIllegalQuery(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedirectLocationWithIllegalQuery(t *testing.T) {
	// Real-world 302s carry literal spaces in Location query strings (e.g.
	// a filename in a dload= parameter). A raw space in the request target
	// is malformed HTTP; servers (nginx, and Go's own) reject it with a
	// generic 400 before any handler runs — so following such a redirect
	// fails while a hand-cleaned URL works. The client must escape the
	// illegal bytes when following.
	blob := testBlob(1 << 20)
	const dload = "SITE - [abc] Title! (360).mp4"
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		// Set Location by hand: http.Redirect would clean the URL, and the
		// point is to emit the raw bytes real servers emit.
		w.Header().Set("Location", "/v1/ticket?dload="+dload+"&sig=a%2Bb")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/v1/ticket", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("dload"); got != dload {
			t.Errorf("dload arrived as %q, want %q", got, dload)
		}
		if got := r.URL.Query().Get("sig"); got != "a+b" {
			t.Errorf("sig arrived as %q, want %q (double-encoded?)", got, "a+b")
		}
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	// No Client passed: exercises the default client's redirect handling.
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

// rangeStart extracts the start offset of a request's Range header
// ("bytes=start-end"); test servers use it to misbehave realistically.
func rangeStart(t *testing.T, r *http.Request) int64 {
	t.Helper()
	var start, end int64
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
		t.Errorf("unparseable Range %q: %v", r.Header.Get("Range"), err)
	}
	return start
}

func TestServerIgnoresRangeEnd(t *testing.T) {
	// A server that honors the range start but streams to EOF must not let
	// one worker overwrite the chunks that follow its own.
	blob := testBlob(1 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := rangeStart(t, r)
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, len(blob)-1, len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(blob[start:]) // ignores the requested end
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL, Output: out, ChunkSize: 256 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
	for _, c := range d.chunks {
		if got := c.done.Load(); got != c.length {
			t.Errorf("chunk %d done = %d, want exactly %d (over-delivery not bounded?)", c.index, got, c.length)
		}
	}
}

func TestServerIgnoresRangeStart(t *testing.T) {
	// A 206 whose Content-Range starts at 0 when we asked mid-file must be
	// rejected, not written at the requested offset.
	blob := testBlob(1 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob)) // honest probe
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(blob)-1, len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(blob) // ignores the requested start entirely
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL, Output: out, ChunkSize: 256 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = d.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "server range starts at") {
		t.Fatalf("Run = %v, want a range-start mismatch error", err)
	}
}

func TestRefreshRejectsChangedResource(t *testing.T) {
	// A chunk retry re-probes the entry URL for a fresh ticket. If that
	// re-probe resolves to DIFFERENT content (origin changed mid-download),
	// the new URL must be rejected — splicing two files together would
	// corrupt the output silently.
	blobV1 := testBlob(512 << 10)
	blobV2 := bytes.ToUpper(blobV1) // same size, different content

	var probes, v1Fails atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) == 1 {
			http.Redirect(w, r, "/v1/ticket", http.StatusFound)
		} else {
			http.Redirect(w, r, "/v2/ticket", http.StatusFound) // re-mint: new resource
		}
	})
	mux.HandleFunc("/v1/ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" && v1Fails.Add(1) == 1 {
			// Fail the first chunk request with a server error — the
			// failure class that triggers a refresh probe on retry.
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(blobV1))
	})
	mux.HandleFunc("/v2/ticket", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(blobV2))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL + "/start", Output: out, Concurrency: 1, ChunkSize: 512 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blobV1); got != want {
		t.Errorf("output does not match the ORIGINAL content: the refresh adopted a changed resource")
	}
	if probes.Load() < 2 {
		t.Errorf("refresh probe never happened (probes=%d); test exercised nothing", probes.Load())
	}
}

func TestNoRefreshOnBodyError(t *testing.T) {
	// A truncated body is an I/O failure, not a stale-URL signal: the
	// retry must NOT re-probe the entry URL (which may rate-limit ticket
	// re-mints — modeled here as a hard 400 on any second mint).
	blob := testBlob(512 << 10)
	var mints, fails atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if mints.Add(1) > 1 {
			http.Error(w, "ticket already issued", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/v1/ticket", http.StatusFound)
	})
	mux.HandleFunc("/v1/ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" && fails.Add(1) == 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(blob)-1, len(blob)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(blob[:100]) // truncate: body ends early
			return
		}
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(blob))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL + "/start", Output: out, Concurrency: 1, ChunkSize: 512 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("entry URL hit %d times, want 1: an I/O error triggered a refresh probe", n)
	}
}

func TestSingleStreamUnknownLengthTruncatesStaleTail(t *testing.T) {
	// No range support AND no Content-Length: the body alone defines the
	// size. A longer previous attempt's bytes must not survive past the
	// new end of file.
	blob := testBlob(256 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush mid-body to force chunked encoding (ContentLength -1).
		w.Write(blob[:len(blob)/2])
		w.(http.Flusher).Flush()
		w.Write(blob[len(blob)/2:])
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	stale := bytes.Repeat([]byte{0xAA}, len(blob)*2) // longer earlier attempt
	if err := os.WriteFile(PartPath(out), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{URL: srv.URL, Output: out, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len(blob)) {
		t.Errorf("output size = %d, want %d (stale tail not truncated)", fi.Size(), len(blob))
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
}

func TestRefuseOverwrite(t *testing.T) {
	blob := testBlob(256 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "blob", time.Now(), bytes.NewReader(blob))
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	precious := []byte("someone else's data")
	if err := os.WriteFile(out, precious, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{URL: srv.URL, Output: out, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); !errors.Is(err, ErrOutputExists) {
		t.Fatalf("Run over existing file: got %v, want ErrOutputExists", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, precious) {
		t.Fatal("existing file was modified by a refused run")
	}

	// Force overwrites.
	d2, err := New(Options{URL: srv.URL, Output: out, Force: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d2.Run(t.Context()); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch after forced overwrite: got %x, want %x", got, want)
	}
}

func TestRenameBlockedKeepsPartAndState(t *testing.T) {
	// If the final name appears while the download runs, completion must
	// refuse the rename and keep both part and sidecar, so a Force rerun
	// resumes (instantly, if all chunks finished) and retries the rename.
	blob := testBlob(2 << 20)
	out := filepath.Join(t.TempDir(), "out.bin")
	precious := []byte("appeared mid-download")

	var served atomic.Int64
	srv := blobServer(blob, `"v1"`, &served, 64<<10, func() {
		os.WriteFile(out, precious, 0o644) // final name appears early on
	})
	defer srv.Close()

	d, err := New(Options{URL: srv.URL, Output: out, ChunkSize: 256 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); !errors.Is(err, ErrOutputExists) {
		t.Fatalf("Run with final name appearing: got %v, want ErrOutputExists", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, precious) {
		t.Fatal("pre-existing final file was clobbered")
	}
	for _, kept := range []string{PartPath(out), StatePath(out)} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s not kept after blocked rename: %v", kept, err)
		}
	}

	// Force rerun: resumes from the completed part and renames over.
	before := served.Load()
	d2, err := New(Options{URL: srv.URL, Output: out, Force: true, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d2.Run(t.Context()); err != nil {
		t.Fatalf("forced rerun: %v", err)
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch after forced rerun: got %x, want %x", got, want)
	}
	if delta := served.Load() - before; delta > 64<<10 {
		t.Errorf("forced rerun re-downloaded %d bytes; expected an (almost) instant resume", delta)
	}
	if _, err := os.Stat(PartPath(out)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("part file still present after completion: %v", err)
	}
}
