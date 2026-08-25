package downloader

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeRangeSupported(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 4096)
	modTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-0" {
			t.Errorf("Range header = %q, want %q", got, "bytes=0-0")
		}
		http.ServeContent(w, r, "blob", modTime, bytes.NewReader(blob))
	}))
	defer srv.Close()

	res, err := Probe(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.RangesSupported {
		t.Error("RangesSupported = false, want true")
	}
	if res.Size != int64(len(blob)) {
		t.Errorf("Size = %d, want %d", res.Size, len(blob))
	}
	if res.LastModified == "" {
		t.Error("LastModified is empty, want captured value")
	}
}

func TestProbeNoRangeSupport(t *testing.T) {
	const body = "hello, this server ignores Range headers"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Plain 200 with no Range handling.
		w.Header().Set("Content-Length", "40")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	res, err := Probe(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.RangesSupported {
		t.Error("RangesSupported = true, want false")
	}
	if res.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", res.Size, len(body))
	}
}

func TestProbeCapturesValidators(t *testing.T) {
	const etag = `"abc123"`
	const lastMod = "Mon, 02 Jan 2006 15:04:05 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", lastMod)
		w.Header().Set("Content-Range", "bytes 0-0/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	res, err := Probe(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.ETag != etag {
		t.Errorf("ETag = %q, want %q", res.ETag, etag)
	}
	if res.LastModified != lastMod {
		t.Errorf("LastModified = %q, want %q", res.LastModified, lastMod)
	}
	if res.Size != 1000 {
		t.Errorf("Size = %d, want 1000", res.Size)
	}
}

func TestProbeUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if _, err := Probe(t.Context(), srv.Client(), srv.URL); err == nil {
		t.Fatal("Probe on 404 succeeded, want error")
	}
}

func TestParseContentRangeTotal(t *testing.T) {
	tests := []struct {
		header  string
		want    int64
		wantErr bool
	}{
		{"bytes 0-0/12345", 12345, false},
		{"bytes 0-0/*", 0, true},
		{"bytes 0-0", 0, true},
		{"12345", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		got, err := parseContentRangeTotal(tt.header)
		if tt.wantErr != (err != nil) {
			t.Errorf("parseContentRangeTotal(%q) error = %v, wantErr %v", tt.header, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseContentRangeTotal(%q) = %d, want %d", tt.header, got, tt.want)
		}
	}
}

// countingServer serves blob with real Range semantics and counts the TCP
// connections opened against it.
func countingServer(t *testing.T, blob []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "blob", time.Time{}, bytes.NewReader(blob))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// The probe's one-byte body must be drained, or its connection is closed
// instead of parked and every probe pays a fresh handshake.
func TestProbeReusesConnection(t *testing.T) {
	srv, conns := countingServer(t, testBlob(1<<20))
	for i := range 3 {
		if _, err := Probe(t.Context(), srv.Client(), srv.URL); err != nil {
			t.Fatalf("Probe %d: %v", i, err)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("3 sequential probes opened %d connections, want 1 (probe body not drained?)", got)
	}
}

// The drain is bounded and 206-only: a server that ignores the Range
// header answers with the whole file, and draining that would download the
// resource just to recycle a socket.
func TestProbeDoesNotDrainUnrangedBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 200 whose body starts flowing and then stalls indefinitely.
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("x"), 1<<10))
		w.(http.Flusher).Flush()
		<-release
	}))
	// Order matters: the server's Close waits for in-flight handlers, so
	// the stalled one has to be released first (defers run last-in-first).
	defer srv.Close()
	defer close(release)

	done := make(chan error, 1)
	go func() {
		_, err := Probe(t.Context(), srv.Client(), srv.URL)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Probe blocked on an unranged body; the drain must stay in the 206 branch")
	}
}
