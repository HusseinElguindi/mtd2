package main

import (
	"fmt"
	"slices"
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

func chunks(n int, size int64, state func(i int) (int64, downloader.ChunkState)) downloader.Progress {
	p := downloader.Progress{Total: int64(n) * size}
	for i := range n {
		done, st := state(i)
		p.Chunks = append(p.Chunks, downloader.ChunkProgress{
			Index: i, Offset: int64(i) * size, Length: size, Done: done, State: st,
		})
	}
	return p
}

// Equal chunks at equal progress draw as identical, evenly spaced
// segments: a tick, then the chunk's fill starting in its first cell.
func TestBandCellsEven(t *testing.T) {
	p := chunks(10, 1000, func(int) (int64, downloader.ChunkState) { return 300, downloader.ChunkActive })
	cells := bandCells(p, 40)
	if !cells[0].tick {
		t.Error("lead-in has no tick for the chunk at the left edge")
	}
	// 4 cells per chunk is 32 eighths, and 30% of that is 9: one full
	// cell and one eighth. The next chunk's tick is on the last cell.
	for i := range 10 {
		want := []bandCell{{fill: 8}, {fill: 1}, {}, {tick: i < 9}}
		if got := cells[1+4*i : 5+4*i]; !slices.Equal(got, want) {
			t.Errorf("chunk %d: %+v, want %+v", i, got, want)
		}
	}
}

// When chunks don't divide the band evenly, each one still starts on the
// nearest cell boundary, so segments differ by at most a cell and every
// fill starts right after its tick.
func TestBandCellsUneven(t *testing.T) {
	p := chunks(10, 1000, func(int) (int64, downloader.ChunkState) { return 300, downloader.ChunkActive })
	cells := bandCells(p, 48)
	var ticks []int
	for i, c := range cells {
		if c.tick {
			ticks = append(ticks, i) // a chunk starts at band cell i
		}
	}
	if want := []int{0, 5, 10, 14, 19, 24, 29, 34, 38, 43}; !slices.Equal(ticks, want) {
		t.Fatalf("chunk starts = %v, want %v", ticks, want)
	}
	for _, lo := range ticks {
		if c := cells[1+lo]; c.fill != 8 {
			t.Errorf("chunk at cell %d: first cell %+v, want full", lo, c)
		}
	}
}

// Chunks narrower than minTickCells get no ticks; each cell shows exactly
// as many eighths as are downloaded, and equal chunks at equal progress
// make a comb that repeats evenly.
func TestBandCellsDense(t *testing.T) {
	p := chunks(40, 1000, func(i int) (int64, downloader.ChunkState) {
		switch {
		case i < 10:
			return 1000, downloader.ChunkDone
		case i < 20:
			return 350, downloader.ChunkActive
		}
		return 0, downloader.ChunkPending
	})
	cells := bandCells(p, 48)
	got := 0
	for _, c := range cells {
		if c.tick {
			t.Fatal("dense band has ticks")
		}
		got += c.fill
	}
	want := 0
	for _, f := range coverage(p, 48*8) {
		if f {
			want++
		}
	}
	if got != want {
		t.Errorf("band shows %d eighths, want %d", got, want)
	}
	// 40 chunks over 48 cells is 5 chunks per 6 cells, so the active
	// stretch (chunks 10-19, cells 12-23) is one 6-cell pattern twice.
	if a, b := cells[1+12:1+18], cells[1+18:1+24]; !slices.Equal(a, b) {
		t.Errorf("active stretch doesn't repeat: %+v then %+v", a, b)
	}
}

// A finished download is a solid band at any chunk count and width.
func TestBandCellsCompleteIsSolid(t *testing.T) {
	for _, n := range []int{1, 3, 8, 10, 40, 333} {
		p := chunks(n, 7777, func(int) (int64, downloader.ChunkState) { return 7777, downloader.ChunkDone })
		for _, w := range []int{20, 33, 48} {
			for i, c := range bandCells(p, w)[1:] {
				if c != (bandCell{fill: 8}) {
					t.Errorf("%d chunks, width %d: cell %d = %+v", n, w, i, c)
					break
				}
			}
		}
	}
}

func TestDrawBand(t *testing.T) {
	cells := []bandCell{{tick: true}, {fill: 8}, {fill: 3}, {fill: 5, tick: true}, {fill: 3, tick: true}, {}}
	esc := func(fg, bg int) string { return fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm", fg, bg) }
	want := fmt.Sprintf("\x1b[38;5;%dm▕", bandMarker) + barReset +
		esc(bandFill, bandFill) + " " +
		esc(bandFill, bandTrack) + "▍" +
		esc(bandMarker, bandFill) + "▕" + // a tick over a mostly filled cell
		esc(bandMarker, bandTrack) + "▕" + // and over a mostly empty one
		esc(bandTrack, bandTrack) + " " + barReset
	if got := drawBand(cells); got != want {
		t.Errorf("drawBand =\n%q\nwant\n%q", got, want)
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

func TestAssignRows(t *testing.T) {
	chunks := func(active ...int) []downloader.ChunkProgress {
		cs := make([]downloader.ChunkProgress, 8)
		for i := range cs {
			cs[i] = downloader.ChunkProgress{Index: i, State: downloader.ChunkPending}
		}
		for _, i := range active {
			cs[i].State = downloader.ChunkActive
		}
		return cs
	}
	cases := []struct {
		name   string
		prev   []int
		active []int
		want   []int
	}{
		{"first frame", nil, []int{0, 1, 2}, []int{0, 1, 2}},
		{"steady", []int{0, 1, 2}, []int{0, 1, 2}, []int{0, 1, 2}},
		{"finished chunk's row goes to the new one", []int{0, 1, 2}, []int{0, 2, 3}, []int{0, 3, 2}},
		{"rows close up when nothing replaces them", []int{0, 1, 2}, []int{0, 2}, []int{0, 2}},
		{"extra new chunks go at the end", []int{3, 1}, []int{1, 4, 5}, []int{4, 1, 5}},
		{"done", []int{4, 1}, nil, []int{}},
	}
	for _, c := range cases {
		if got := assignRows(c.prev, chunks(c.active...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: assignRows(%v) = %v, want %v", c.name, c.prev, got, c.want)
		}
	}
}
