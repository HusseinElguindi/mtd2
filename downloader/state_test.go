package downloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingWriter tracks bytes written and fires reached once total crosses
// the threshold, letting a test cancel a download mid-transfer.
type countingWriter struct {
	http.ResponseWriter
	total     *atomic.Int64
	threshold int64
	reached   func()
	once      *sync.Once
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if w.total.Add(int64(n)) >= w.threshold {
		w.once.Do(w.reached)
	}
	// Pace the transfer so cancellation reliably lands mid-download.
	time.Sleep(time.Millisecond)
	return n, err
}

// blobServer serves blob with range support and a fixed ETag, counting
// served body bytes into total and calling reached at the threshold.
func blobServer(blob []byte, etag string, total *atomic.Int64, threshold int64, reached func()) *httptest.Server {
	var once sync.Once
	if reached == nil {
		reached = func() {}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		cw := &countingWriter{ResponseWriter: w, total: total, threshold: threshold, reached: reached, once: &once}
		http.ServeContent(cw, r, "blob", time.Time{}, bytes.NewReader(blob))
	}))
}

func TestResume(t *testing.T) {
	blob := testBlob(4 << 20)
	out := filepath.Join(t.TempDir(), "out.bin")

	// Session 1: cancel once ~1 MiB has been served.
	ctx, cancel := context.WithCancel(t.Context())
	var served1 atomic.Int64
	srv1 := blobServer(blob, `"v1"`, &served1, 1<<20, cancel)
	d, err := New(Options{URL: srv1.URL, Output: out, ChunkSize: 256 << 10, Client: srv1.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(ctx); err == nil {
		t.Fatal("Run: expected cancellation error, got nil")
	}
	srv1.Close()

	st, err := loadState(StatePath(out))
	if err != nil || st == nil {
		t.Fatalf("state after cancel: st=%v err=%v", st, err)
	}
	var doneBytes int64
	for _, n := range st.Done {
		doneBytes += n
	}
	if doneBytes <= 0 || doneBytes >= int64(len(blob)) {
		t.Fatalf("partial done bytes = %d, want in (0, %d)", doneBytes, len(blob))
	}

	// Session 2: same content and ETag on a fresh server; must complete
	// from state, fetching substantially less than the whole file. The
	// state's URL is rewritten to the new server's (the test moved hosts;
	// real resumes reuse the URL).
	var served2 atomic.Int64
	srv2 := blobServer(blob, `"v1"`, &served2, 1<<62, nil)
	defer srv2.Close()
	st.URL = srv2.URL
	if err := st.save(StatePath(out)); err != nil {
		t.Fatalf("rewrite state URL: %v", err)
	}
	d2, err := New(Options{URL: srv2.URL, Output: out, Client: srv2.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d2.Run(t.Context()); err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch after resume: got %x, want %x", got, want)
	}
	if max := int64(len(blob)) - doneBytes + 64<<10; served2.Load() > max {
		t.Errorf("resume served %d bytes, want <= %d (should skip the %d already done)", served2.Load(), max, doneBytes)
	}
	if _, err := os.Stat(StatePath(out)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file still present after successful download: %v", err)
	}
}

func TestResumeValidatorMismatch(t *testing.T) {
	blob := testBlob(2 << 20)
	out := filepath.Join(t.TempDir(), "out.bin")

	ctx, cancel := context.WithCancel(t.Context())
	var served atomic.Int64
	srv1 := blobServer(blob, `"v1"`, &served, 256<<10, cancel)
	d, err := New(Options{URL: srv1.URL, Output: out, ChunkSize: 256 << 10, Client: srv1.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(ctx); err == nil {
		t.Fatal("Run: expected cancellation error, got nil")
	}
	srv1.Close()

	// Same size, new ETag: the content changed and resuming must refuse.
	var served2 atomic.Int64
	srv2 := blobServer(blob, `"v2"`, &served2, 1<<62, nil)
	defer srv2.Close()
	st, err := loadState(StatePath(out))
	if err != nil || st == nil {
		t.Fatalf("state after cancel: st=%v err=%v", st, err)
	}
	st.URL = srv2.URL
	if err := st.save(StatePath(out)); err != nil {
		t.Fatalf("rewrite state URL: %v", err)
	}
	d2, err := New(Options{URL: srv2.URL, Output: out, Client: srv2.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d2.Run(t.Context()); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("resume with changed ETag: got %v, want ErrStateMismatch", err)
	}
}

func TestStateValidate(t *testing.T) {
	base := func() *state {
		return &state{
			URL: "http://example/x", Size: 100 << 20, ETag: `"e1"`,
			LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
			ChunkSize:    8 << 20, Done: make([]int64, 13),
		}
	}
	probe := ProbeResult{
		Size: 100 << 20, RangesSupported: true, ETag: `"e1"`,
		LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
	}

	if err := base().validate("http://example/x", probe); err != nil {
		t.Errorf("matching state: %v", err)
	}

	// Validators are only compared when both sides have them.
	p := probe
	p.ETag, p.LastModified = "", ""
	if err := base().validate("http://example/x", p); err != nil {
		t.Errorf("server dropped validators, size matches: %v", err)
	}

	mutations := map[string]func(*state){
		"url":       func(s *state) { s.URL = "http://example/y" },
		"size":      func(s *state) { s.Size++ },
		"etag":      func(s *state) { s.ETag = `"e2"` },
		"modified":  func(s *state) { s.LastModified = "Tue, 03 Jan 2006 15:04:05 GMT" },
		"chunksize": func(s *state) { s.ChunkSize = 0 },
		"chunks":    func(s *state) { s.Done = s.Done[:5] },
	}
	for name, mutate := range mutations {
		st := base()
		mutate(st)
		if err := st.validate("http://example/x", probe); !errors.Is(err, ErrStateMismatch) {
			t.Errorf("%s mutation: got %v, want ErrStateMismatch", name, err)
		}
	}
}

func TestCorruptStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin.mtd.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path); err == nil {
		t.Error("corrupt state file: expected error, got nil")
	}
	if st, err := loadState(filepath.Join(dir, "missing.json")); st != nil || err != nil {
		t.Errorf("missing state file: got st=%v err=%v, want nil, nil", st, err)
	}
}
