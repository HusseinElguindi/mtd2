package main

import (
	"fmt"
	"strings"
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

// Downloaded bytes in the second half of a cell light its right column,
// not its left.
func TestSegmentBarRightHalf(t *testing.T) {
	p := downloader.Progress{Total: 80, Chunks: []downloader.ChunkProgress{
		{Offset: 0, Length: 60, Done: 0, State: downloader.ChunkPending},
		{Offset: 60, Length: 20, Done: 10, State: downloader.ChunkActive},
	}}
	if got, want := segmentBar(p, 2, false), "⣀⢠"; got != want {
		t.Errorf("segmentBar = %q, want %q", got, want)
	}
}

// An active chunk that hasn't received a byte yet still marks where its
// connection will write.
func TestSegmentBarHeadBeforeFirstByte(t *testing.T) {
	p := downloader.Progress{Total: 80, Chunks: []downloader.ChunkProgress{
		{Offset: 0, Length: 40, Done: 40, State: downloader.ChunkDone},
		{Offset: 40, Length: 40, Done: 0, State: downloader.ChunkActive},
	}}
	want := barFill + "⣿" + barHead + "⣀" + barReset
	if got := segmentBar(p, 2, true); got != want {
		t.Errorf("segmentBar = %q, want %q", got, want)
	}
}

// A finished download must draw as a solid bar whatever the chunk and cell
// boundaries, with no cell short a dot from rounding.
func TestSegmentBarCompleteIsSolid(t *testing.T) {
	for _, tc := range []struct{ total, chunk int64 }{
		{64 << 20, 8 << 20}, {256 << 20, 16 << 20}, {1000003, 77777}, {7, 1},
	} {
		p := downloader.Progress{Total: tc.total}
		for off, i := int64(0), 0; off < tc.total; off, i = off+tc.chunk, i+1 {
			n := min(tc.chunk, tc.total-off)
			p.Chunks = append(p.Chunks, downloader.ChunkProgress{Index: i, Offset: off, Length: n, Done: n, State: downloader.ChunkDone})
		}
		for _, w := range []int{20, 33, 40} {
			if got := segmentBar(p, w, false); got != strings.Repeat("⣿", w) {
				t.Errorf("total=%d chunk=%d width=%d: %q", tc.total, tc.chunk, w, got)
			}
		}
	}
}

func TestFitCell(t *testing.T) {
	b := func(s string) (bits [8]bool) {
		for i, ch := range s {
			bits[i] = ch == '#'
		}
		return
	}
	cases := []struct {
		bits   string
		glyph  rune
		fg, bg int
	}{
		{"........", ' ', bandTrack, bandTrack},
		{"########", ' ', bandFill, bandFill},
		{"###.....", '▍', bandFill, bandTrack}, // fill ends 3/8 in
		{".....###", '▋', bandTrack, bandFill}, // fill starts 5/8 in
		{"##...###", '▋', bandTrack, bandFill}, // two edges: nearest single edge
		{"#.......", '▏', bandFill, bandTrack},
	}
	for _, c := range cases {
		g, fg, bg := fitCell(b(c.bits))
		if g != c.glyph || fg != c.fg || bg != c.bg {
			t.Errorf("fitCell(%s) = %q %d/%d, want %q %d/%d", c.bits, g, fg, bg, c.glyph, c.fg, c.bg)
		}
	}
}

// Each chunk's fill sits at its own place in the file and every chunk
// start gets a tick.
func TestBandBar(t *testing.T) {
	p := downloader.Progress{Total: 400, Chunks: []downloader.ChunkProgress{
		{Offset: 0, Length: 200, Done: 150, State: downloader.ChunkActive},
		{Offset: 200, Length: 200, Done: 25, State: downloader.ChunkActive},
	}}
	// 4 cells of 100 bytes: cell 0 full (tick over fill), cell 1 half,
	// cell 2 a quarter (tick over fill: the chunk has started), cell 3
	// empty.
	tick := func(bg int) string { return fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm▏", bandMarker, bg) }
	want := tick(bandFill) +
		fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm▌", bandFill, bandTrack) +
		tick(bandFill) +
		fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm ", bandTrack, bandTrack) + barReset
	if got := bandBar(p, 4); got != want {
		t.Errorf("bandBar =\n%q\nwant\n%q", got, want)
	}
}

// A chunk that starts mid-cell, after a stretch that isn't in yet, still
// gets blue right behind its tick rather than the track.
func TestBandBarTickMidCell(t *testing.T) {
	p := downloader.Progress{Total: 160, Chunks: []downloader.ChunkProgress{
		{Offset: 0, Length: 50, Done: 0, State: downloader.ChunkPending},
		{Offset: 50, Length: 110, Done: 20, State: downloader.ChunkActive},
	}}
	want := fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm▏", bandMarker, bandFill) +
		fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm ", bandTrack, bandTrack) + barReset
	if got := bandBar(p, 2); got != want {
		t.Errorf("bandBar =\n%q\nwant\n%q", got, want)
	}

	// Before its first byte lands the tick sits on the track.
	p.Chunks[1].Done = 0
	want = fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm▏", bandMarker, bandTrack) +
		fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm ", bandTrack, bandTrack) + barReset
	if got := bandBar(p, 2); got != want {
		t.Errorf("bandBar before first byte =\n%q\nwant\n%q", got, want)
	}
}

func TestActiveWindow(t *testing.T) {
	p := downloader.Progress{Total: 1000, Chunks: []downloader.ChunkProgress{
		{Index: 0, Offset: 0, Length: 100, Done: 100, State: downloader.ChunkDone},
		{Index: 1, Offset: 100, Length: 100, Done: 40, State: downloader.ChunkActive},
		{Index: 2, Offset: 200, Length: 100, Done: 100, State: downloader.ChunkDone},
		{Index: 3, Offset: 300, Length: 100, Done: 10, State: downloader.ChunkActive},
		{Index: 4, Offset: 400, Length: 600, Done: 0, State: downloader.ChunkPending},
	}}
	w, ok := activeWindow(p)
	if !ok || w.Total != 300 || len(w.Chunks) != 3 {
		t.Fatalf("activeWindow = %+v, %v; want chunks 1-3 over 300 bytes", w, ok)
	}
	for i, wantOff := range []int64{0, 100, 200} {
		if w.Chunks[i].Offset != wantOff || w.Chunks[i].Index != i+1 {
			t.Errorf("chunk %d: %+v, want index %d at offset %d", i, w.Chunks[i], i+1, wantOff)
		}
	}

	p.Chunks[1].State, p.Chunks[3].State = downloader.ChunkDone, downloader.ChunkDone
	if _, ok := activeWindow(p); ok {
		t.Error("activeWindow with nothing active: ok = true")
	}
}
