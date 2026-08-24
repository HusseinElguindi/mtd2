package downloader

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestNewTransportSizesPool(t *testing.T) {
	tr := NewTransport(16)
	if tr.MaxIdleConnsPerHost != 17 || tr.MaxIdleConns != 17 || tr.MaxConnsPerHost != 17 {
		t.Errorf("pool = idlePerHost %d, idle %d, conns %d; want 17 each (concurrency + the probe)",
			tr.MaxIdleConnsPerHost, tr.MaxIdleConns, tr.MaxConnsPerHost)
	}
	if got := NewTransport(0).MaxIdleConnsPerHost; got != DefaultConcurrency+1 {
		t.Errorf("NewTransport(0) idle pool = %d, want %d", got, DefaultConcurrency+1)
	}
}

// The tuned transport must be a clone: http.DefaultTransport is shared by
// every other HTTP user in the process.
func TestNewTransportDoesNotMutateDefault(t *testing.T) {
	def := http.DefaultTransport.(*http.Transport)
	before := *def
	NewTransport(64)
	if def.MaxIdleConnsPerHost != before.MaxIdleConnsPerHost || def.MaxConnsPerHost != before.MaxConnsPerHost {
		t.Error("NewTransport mutated http.DefaultTransport instead of cloning it")
	}
	if http.DefaultTransport.(*http.Transport) != def {
		t.Error("NewTransport replaced http.DefaultTransport")
	}
}

// End to end: N workers plus the probe must not exceed N+1 sockets, which
// is what MaxConnsPerHost promises — retries and URL refreshes included.
//
// Note what this does not prove: a stock, undersized pool also passes this
// bound on a workload this small, because churn needs several workers to
// go idle at once. The deterministic guard for pool sizing is
// TestNewTransportSizesPool; this one guards the cap holding end to end.
func TestDownloadReusesConnections(t *testing.T) {
	const concurrency = 8
	srv, conns := countingServer(t, testBlob(24<<20))

	client, err := NewClient(NewTransport(concurrency))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{
		URL:         srv.URL,
		Output:      out,
		Concurrency: concurrency,
		ChunkSize:   1 << 20, // 24 chunks over 8 workers
		Client:      client,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := conns.Load(); got > concurrency+1 {
		t.Errorf("25 requests over %d workers opened %d connections, want at most %d",
			concurrency, got, concurrency+1)
	}
}
