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
	line("total     %s", totalBar(p, totalBarWidth, r.color))
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

// Bars are drawn in braille. A cell is two columns of 4 dots, and each
// column fills bottom-up on its own: the left column stands for the first
// half of the cell, the right for the second. A plain bar fills the left
// column and then the right, advancing a dot (1/8 cell) at a time.
const (
	barFill  = "\x1b[36m"       // cyan: downloaded
	barHead  = "\x1b[93m"       // yellow: a cell an active chunk is writing into
	barTrack = "\x1b[38;5;238m" // dark grey: not yet downloaded
	barReset = "\x1b[0m"
)

// Braille dot bits for each column, bottom to top.
var (
	leftDots  = [4]rune{0x40, 0x04, 0x02, 0x01}
	rightDots = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// cell is one character of a bar: how many dots (0-4) are lit in each
// column, and whether an active chunk's write position falls inside it.
type cell struct {
	left, right int
	head        bool
}

func (c cell) glyph() rune {
	if c.left == 0 && c.right == 0 {
		return '⣀' // empty track
	}
	r := rune(0x2800)
	for i := range c.left {
		r |= leftDots[i]
	}
	for i := range c.right {
		r |= rightDots[i]
	}
	return r
}

// bar draws done/total as a bar width cells wide that advances a dot
// (1/8 cell) at a time.
func bar(done, total int64, width int, color bool) string {
	cells := make([]cell, width)
	if total > 0 {
		dots := int(float64(done) / float64(total) * float64(width*8))
		dots = min(max(dots, 0), width*8)
		for i := range cells {
			n := min(max(dots-i*8, 0), 8)
			cells[i].left, cells[i].right = min(n, 4), max(n-4, 0)
		}
	}
	return drawCells(cells, color)
}

// segmentBar draws the whole file as a map, IDM-style: every chunk's
// segment fills in at its own place in the file. Each cell column covers
// total/(2*width) bytes and lights up as far as the chunks overlapping it
// have downloaded, so dots sit where the data actually is, even when a
// chunk boundary falls mid-cell. Cells an active chunk is about to write
// into are highlighted.
func segmentBar(p downloader.Progress, width int, color bool) string {
	cells := make([]cell, width)
	if p.Total <= 0 {
		return drawCells(cells, color)
	}
	// Column j covers bytes [edge(j), edge(j+1)). Integer math keeps a
	// fully downloaded column at exactly 4 dots; float spans summed across
	// a chunk boundary can land a hair under and floor, leaving a notch in
	// the top edge.
	cols := int64(2 * width)
	edge := func(j int) int64 { return p.Total * int64(j) / cols }
	// colOf inverts edge: the last column whose first byte is at or before b.
	colOf := func(b int64) int { return int(((b+1)*cols - 1) / p.Total) }
	got := make([]int64, cols) // downloaded bytes falling in each column
	for _, c := range p.Chunks {
		start, end := c.Offset, c.Offset+c.Done
		if c.State == downloader.ChunkActive && c.Done < c.Length {
			cells[colOf(end)/2].head = true // where the next byte lands
		}
		if end <= start {
			continue
		}
		for j := colOf(start); j <= colOf(end-1); j++ {
			got[j] += min(end, edge(j+1)) - max(start, edge(j))
		}
	}
	for j, g := range got {
		lvl := 0
		if span := edge(j+1) - edge(j); span > 0 {
			lvl = int(g * 4 / span)
			if g > 0 && lvl == 0 {
				lvl = 1 // show that something landed here
			}
		} else if downloaded(p.Chunks, edge(j)) {
			lvl = 4 // file smaller than the bar: show the byte at edge(j)
		}
		if j%2 == 0 {
			cells[j/2].left = lvl
		} else {
			cells[j/2].right = lvl
		}
	}
	return drawCells(cells, color)
}

func downloaded(chunks []downloader.ChunkProgress, off int64) bool {
	for _, c := range chunks {
		if off >= c.Offset && off < c.Offset+c.Done {
			return true
		}
	}
	return false
}

func drawCells(cells []cell, color bool) string {
	var b strings.Builder
	cur := ""
	for _, c := range cells {
		if color {
			want := barTrack
			switch {
			case c.head:
				want = barHead
			case c.left > 0 || c.right > 0:
				want = barFill
			}
			if want != cur {
				b.WriteString(want)
				cur = want
			}
		}
		b.WriteRune(c.glyph())
	}
	if color {
		b.WriteString(barReset)
	}
	return b.String()
}

// totalBarWidth is wider than the per-chunk bars: the total bar maps the
// whole file, so it needs the room to show each chunk's place in it.
const totalBarWidth = 48

// totalBar draws the whole-file map: a solid IDM-style band in color, or
// the braille map when color is off (the band needs background colors).
func totalBar(p downloader.Progress, width int, color bool) string {
	if !color {
		return segmentBar(p, width, false)
	}
	return bandBar(p, width)
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
func bandBar(p downloader.Progress, width int) string {
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
	b.WriteString(barReset)
	return b.String()
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
