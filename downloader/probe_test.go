package downloader

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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
