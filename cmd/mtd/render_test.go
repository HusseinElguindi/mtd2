package main

import (
	"testing"

	"mtd2/downloader"
)

func TestBar(t *testing.T) {
	cases := []struct {
		done, total int64
		want        string
	}{
		{0, 0, "⣀⣀⣀⣀"},
		{0, 100, "⣀⣀⣀⣀"},
		{100, 100, "⣿⣿⣿⣿"},
		{50, 100, "⣿⣿⣀⣀"},
		{1, 32, "⡀⣀⣀⣀"},  // one dot of one cell
		{15, 32, "⣿⣷⣀⣀"}, // 15/32 of 4 cells = 1 7/8 cells
		{200, 100, "⣿⣿⣿⣿"},
		{-5, 100, "⣀⣀⣀⣀"},
	}
	for _, c := range cases {
		if got := bar(c.done, c.total, 4, false); got != c.want {
			t.Errorf("bar(%d, %d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
	if got, want := bar(1, 32, 4, true), barFill+"⡀"+barTrack+"⣀⣀⣀"+barReset; got != want {
		t.Errorf("color bar = %q, want %q", got, want)
	}
}

func TestSegmentBar(t *testing.T) {
	// Four 100-byte chunks drawn over 4 cells: one cell per chunk.
	p := downloader.Progress{Total: 400, Chunks: []downloader.ChunkProgress{
		{Offset: 0, Length: 100, Done: 100, State: downloader.ChunkDone},
		{Offset: 100, Length: 100, Done: 50, State: downloader.ChunkActive},
		{Offset: 200, Length: 100, Done: 0, State: downloader.ChunkPending},
		{Offset: 300, Length: 100, Done: 1, State: downloader.ChunkActive},
	}}
	if got, want := segmentBar(p, 4, false), "⣿⡇⣀⡀"; got != want {
		t.Errorf("segmentBar = %q, want %q", got, want)
	}
	want := barFill + "⣿" + barHead + "⡇" + barTrack + "⣀" + barHead + "⡀" + barReset
	if got := segmentBar(p, 4, true); got != want {
		t.Errorf("color segmentBar = %q, want %q", got, want)
	}

	// Chunks that don't line up with cells: chunk 1's first 10 bytes top
	// up cell 0, and the single byte it has in cell 1 still shows a dot.
	p = downloader.Progress{Total: 80, Chunks: []downloader.ChunkProgress{
		{Offset: 0, Length: 30, Done: 30, State: downloader.ChunkDone},
		{Offset: 30, Length: 50, Done: 11, State: downloader.ChunkActive},
	}}
	if got, want := segmentBar(p, 2, false), "⣿⡀"; got != want {
		t.Errorf("unaligned segmentBar = %q, want %q", got, want)
	}
}
