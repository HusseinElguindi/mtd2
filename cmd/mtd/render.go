package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mtd2/downloader"
)

// renderer draws live download progress. On a TTY it redraws a block of
// lines in place (active chunks, an overall bar, a stats line) ten times a
// second; on anything else it degrades to a periodic one-line log.
type renderer struct {
	out      io.Writer
	tty      bool
	interval time.Duration
	color    bool // ANSI colors allowed (TTY and NO_COLOR unset)
	lines    int  // lines drawn by the previous frame, to move back over
}

func newRenderer(out *os.File) *renderer {
	fi, err := out.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	interval := 2 * time.Second
	if tty {
		interval = 100 * time.Millisecond
	}
	color := tty && os.Getenv("NO_COLOR") == ""
	return &renderer{out: out, tty: tty, color: color, interval: interval}
}

func (r *renderer) render(p downloader.Progress) {
	if p.Chunks == nil {
		return // not started yet
	}
	if !r.tty {
		fmt.Fprintln(r.out, r.statsLine(p))
		return
	}

	var b strings.Builder
	// Turn off autowrap for the frame: \x1b[%dA moves up screen rows, not
	// lines, so a line wider than the terminal would wrap onto a second row
	// and the next frame would start too low. Long lines are cut at the
	// right edge instead.
	b.WriteString("\x1b[?7l")
	// Return to the top of the previous frame; \x1b[2K clears each line as
	// it is redrawn.
	if r.lines > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", r.lines)
	}
	lines := 0
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, "\x1b[2K"+format+"\n", args...)
		lines++
	}

	for _, c := range p.Chunks {
		if c.State != downloader.ChunkActive {
			continue
		}
		line("chunk %3d %s %9s / %s", c.Index, bar(c.Done, c.Length, 20, r.color),
			fmtBytes(c.Done), fmtBytes(c.Length))
	}
	chunksDone := 0
	for _, c := range p.Chunks {
		if c.State == downloader.ChunkDone {
			chunksDone++
		}
	}
	line("total     %s", bandBar(p, totalBarWidth, r.color))
	if w, ok := activeWindow(p); ok {
		line("active    %s", bandBar(w, totalBarWidth, r.color))
	}
	line("          %s / %s  (%d/%d chunks)",
		fmtBytes(p.Downloaded), fmtBytes(p.Total), chunksDone, len(p.Chunks))
	line("%s", r.statsLine(p))
	// The frame shrinks as chunks finish; clear whatever the previous,
	// taller frame left below this one, then restore autowrap.
	b.WriteString("\x1b[J\x1b[?7h")

	r.lines = lines
	io.WriteString(r.out, b.String())
}

// finish leaves the last frame in place and moves on.
func (r *renderer) finish() {
	r.lines = 0
}

func (r *renderer) statsLine(p downloader.Progress) string {
	eta := "--:--"
	rate := p.DownloadRate
	if rate <= 0 {
		rate = p.AvgRate
	}
	if rate > 0 && p.Total > p.Downloaded {
		d := time.Duration(float64(p.Total-p.Downloaded) / rate * float64(time.Second))
		eta = fmtDuration(d)
	}
	return fmt.Sprintf("dl %s/s · net %s/s/conn · disk %s/s · avg %s/s · ETA %s",
		fmtBytes(int64(p.DownloadRate)), fmtBytes(int64(p.NetworkRate)),
		fmtBytes(int64(p.DiskRate)), fmtBytes(int64(p.AvgRate)), eta)
}

// Bars are drawn as solid blocks: downloaded cells are filled and each
// cell is split into eighths, so a bar advances 1/8 cell at a time.
const barReset = "\x1b[0m"

// bar draws done/total as a solid bar width cells wide, filling left to
// right. It is the whole-file band for a file made of one chunk.
func bar(done, total int64, width int, color bool) string {
	p := downloader.Progress{Total: total}
	if total > 0 {
		done = min(max(done, 0), total)
		p.Chunks = []downloader.ChunkProgress{{Length: total, Done: done, State: downloader.ChunkDone}}
	}
	return bandBar(p, width, color)
}

func downloaded(chunks []downloader.ChunkProgress, off int64) bool {
	for _, c := range chunks {
		if off >= c.Offset && off < c.Offset+c.Done {
			return true
		}
	}
	return false
}

// totalBarWidth is wider than the per-chunk bars: the total bar maps the
// whole file, so it needs the room to show each chunk's place in it.
const totalBarWidth = 48

// activeWindow zooms in on the stretch of the file the active chunks
// span, as a Progress of its own. Chunks are handed out in file order, so
// on a large file every connection works within a sliver of it that the
// whole-file bar can't resolve; the zoomed bar shows each one. ok is
// false when nothing is active or the window is already the whole file.
func activeWindow(p downloader.Progress) (w downloader.Progress, ok bool) {
	lo, hi := int64(-1), int64(-1)
	for _, c := range p.Chunks {
		if c.State != downloader.ChunkActive {
			continue
		}
		if lo < 0 || c.Offset < lo {
			lo = c.Offset
		}
		hi = max(hi, c.Offset+c.Length)
	}
	if lo < 0 || hi-lo >= p.Total {
		return w, false
	}
	w.Total = hi - lo
	for _, c := range p.Chunks {
		if c.Offset >= hi || c.Offset+c.Length <= lo {
			continue
		}
		c.Offset -= lo // chunks don't straddle active ones, so this stays in [0, w.Total)
		w.Chunks = append(w.Chunks, c)
	}
	return w, true
}

const (
	bandFill   = 33  // blue: downloaded
	bandTrack  = 237 // dark grey: not yet downloaded
	bandMarker = 196 // red: where each chunk starts
)

// leftEighths[k-1] fills the left k/8 of a cell with the foreground color;
// the rest of the cell shows the background.
var leftEighths = []rune("▏▎▍▌▋▊▉")

// bandBar draws the file as a solid band, like IDM's "download progress by
// connections" bar: downloaded ranges are filled in blue at their place in
// the file, so each chunk's fill grows rightward from its start, and a red
// tick marks where each active connection started.
//
// Each cell is split into eighths. A terminal cell has only two colors, so
// a cell can show exactly one fill edge: filled-then-empty (fill colored
// eighth block over the track background) or empty-then-filled (the
// colors swapped). Cells with more than one edge take the closest match.
//
// Without color the band is drawn in plain blocks: █ downloaded, ░ not
// yet, eighth blocks for the edges, and no connection ticks.
func bandBar(p downloader.Progress, width int, color bool) string {
	filled := coverage(p, width*8)
	markers := make([]bool, width)
	if p.Total > 0 {
		for _, c := range p.Chunks {
			if c.State == downloader.ChunkActive {
				markers[int(c.Offset*int64(width)/p.Total)] = true
			}
		}
	}

	var b strings.Builder
	cur := ""
	for i := range width {
		var bits [8]bool
		copy(bits[:], filled[i*8:])
		g, fg, bg := fitCell(bits)
		if !color {
			b.WriteRune(plainGlyph(g, fg, bg))
			continue
		}
		if markers[i] {
			// The tick replaces the cell's own edge; keep the color most of
			// the cell has behind it.
			n := 0
			for _, f := range bits {
				if f {
					n++
				}
			}
			g, fg, bg = '▏', bandMarker, bandTrack
			if n >= 4 {
				bg = bandFill
			}
		}
		if want := fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm", fg, bg); want != cur {
			b.WriteString(want)
			cur = want
		}
		b.WriteRune(g)
	}
	if color {
		b.WriteString(barReset)
	}
	return b.String()
}

// plainGlyph redraws a fitCell result without colors. A filled-then-empty
// edge keeps its eighth block over the blank cell; an empty-then-filled
// one has no left-aligned glyph, so it rounds to a right eighth or half.
func plainGlyph(g rune, fg, bg int) rune {
	switch {
	case fg == bg && fg == bandFill:
		return '█'
	case fg == bg:
		return '░'
	case fg == bandFill:
		return g
	case g >= '▌': // half or less of the cell is empty
		return '▐'
	default:
		return '▕'
	}
}

// fitCell picks the glyph and colors that best draw one cell's eighths:
// the first k eighths filled, or the last k, whichever differs from bits
// in the fewest places.
func fitCell(bits [8]bool) (glyph rune, fg, bg int) {
	best, bestCost, suffix := 0, 9, false
	for k := 0; k <= 8; k++ {
		pre, suf := 0, 0
		for i, f := range bits {
			if f != (i < k) {
				pre++
			}
			if f != (i >= 8-k) {
				suf++
			}
		}
		if pre < bestCost {
			best, bestCost, suffix = k, pre, false
		}
		if suf < bestCost {
			best, bestCost, suffix = k, suf, true
		}
	}
	switch {
	case best == 0:
		return ' ', bandTrack, bandTrack
	case best == 8:
		return ' ', bandFill, bandFill
	case suffix: // empty-then-filled: draw the empty part over a filled background
		return leftEighths[8-best-1], bandTrack, bandFill
	default:
		return leftEighths[best-1], bandFill, bandTrack
	}
}

// coverage splits the file into n equal units and reports which are
// downloaded: a unit counts once at least half its bytes are in. Units
// smaller than a byte (a file shorter than n) take the byte they start on.
func coverage(p downloader.Progress, n int) []bool {
	out := make([]bool, n)
	if p.Total <= 0 {
		return out
	}
	edge := func(j int) int64 { return p.Total * int64(j) / int64(n) }
	unitOf := func(b int64) int { return int(((b+1)*int64(n) - 1) / p.Total) }
	got := make([]int64, n)
	for _, c := range p.Chunks {
		start, end := c.Offset, c.Offset+c.Done
		if end <= start {
			continue
		}
		for j := unitOf(start); j <= unitOf(end-1); j++ {
			got[j] += min(end, edge(j+1)) - max(start, edge(j))
		}
	}
	for j, g := range got {
		if span := edge(j+1) - edge(j); span > 0 {
			out[j] = g*2 >= span
		} else {
			out[j] = downloaded(p.Chunks, edge(j))
		}
	}
	return out
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
