package downloader

import (
	"crypto/sha256"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestProgressDuringDownload(t *testing.T) {
	blob := testBlob(2 << 20)
	var served atomic.Int64
	srv := blobServer(blob, `"v1"`, &served, 1<<62, nil) // paced writes, no cancel
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.bin")
	d, err := New(Options{URL: srv.URL, Output: out, Concurrency: 2, ChunkSize: 256 << 10, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if p := d.Progress(); p.Downloaded != 0 || p.Chunks != nil {
		t.Errorf("Progress before Run = %+v, want zero values", p)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(t.Context()) }()

	// Poll until we observe a mid-flight snapshot with meaningful rates
	// and an active chunk; the server paces writes, so the download runs
	// long enough for several samples.
	var sawActive, sawRates bool
	deadline := time.After(30 * time.Second)
poll:
	for {
		select {
		case err := <-runErr:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			break poll
		case <-deadline:
			t.Fatal("download did not finish in time")
		case <-time.After(20 * time.Millisecond):
		}

		p := d.Progress()
		if p.Total != int64(len(blob)) {
			continue // not started yet
		}
		if p.Downloaded < 0 || p.Downloaded > p.Total {
			t.Fatalf("Downloaded = %d, out of [0, %d]", p.Downloaded, p.Total)
		}
		for _, c := range p.Chunks {
			switch c.State {
			case ChunkActive:
				sawActive = true
			case ChunkPending, ChunkDone:
			default:
				t.Fatalf("chunk %d in unknown state %q", c.Index, c.State)
			}
			if c.State == ChunkDone && c.Done != c.Length {
				t.Fatalf("chunk %d done-state with %d/%d bytes", c.Index, c.Done, c.Length)
			}
		}
		if p.Downloaded > 0 && p.DownloadRate > 0 && p.NetworkRate > 0 && p.DiskRate > 0 && p.AvgRate > 0 {
			sawRates = true
		}
	}

	if !sawActive {
		t.Error("never observed an active chunk mid-download")
	}
	if !sawRates {
		t.Error("never observed all four rates positive mid-download")
	}

	final := d.Progress()
	if final.Downloaded != final.Total {
		t.Errorf("final Downloaded = %d, want %d", final.Downloaded, final.Total)
	}
	for _, c := range final.Chunks {
		if c.State != ChunkDone {
			t.Errorf("final chunk %d state = %q, want done", c.Index, c.State)
		}
	}
	if got, want := hashFile(t, out), sha256.Sum256(blob); got != want {
		t.Errorf("output hash mismatch: got %x, want %x", got, want)
	}
}
