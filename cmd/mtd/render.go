package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"mtd2/downloader"
)

// renderer draws live download progress. On a TTY it redraws a block of
// lines in place (active chunks, an overall bar, a stats line) ten times a
// second; on anything else it degrades to a periodic one-line log.
type renderer struct {
	out      io.Writer
	fd       int // out's file descriptor, for the terminal size
	tty      bool
	interval time.Duration
	color    bool  // ANSI colors allowed (TTY and NO_COLOR unset)
	drawn    []int // screen width of each line of the previous frame, to move back over
	rows     []int // chunk index on each row of the previous frame's chunk list
}

func newRenderer(out *os.File) *renderer {
	fi, err := out.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	interval := 2 * time.Second
	if tty {
		interval = 100 * time.Millisecond
	}
	color := tty && os.Getenv("NO_COLOR") == ""
	return &renderer{out: out, fd: int(out.Fd()), tty: tty, color: color, interval: interval}
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
	// Ask the terminal to hold the frame and show it all at once (where
	// synchronized output is supported; others ignore it), and hide the
	// cursor while drawing, so a half-drawn frame never shows.
	b.WriteString("\x1b[?2026h\x1b[?25l")
	// Turn off autowrap for the frame: \x1b[%dA moves up screen rows, not
	// lines, so a line wider than the terminal would wrap onto a second row
	// and the next frame would start too low. Long lines are cut at the
	// right edge instead.
	b.WriteString("\x1b[?7l")
	cols, rows, err := term.GetSize(r.fd)
	if err != nil {
		cols, rows = 0, 0
	}
	// Return to the top of the previous frame.
	if up := frameRows(r.drawn, cols); up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	var drawn []int
	// Each line overwrites the previous frame's and then erases what is
	// left of it (\x1b[K), rather than blanking the row first, so no row
	// is ever shown empty.
	line := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		b.WriteString(s + "\x1b[K\n")
		w := screenWidth(s)
		if cols > 0 {
			w = min(w, cols) // the rest was cut off at the right edge
		}
		drawn = append(drawn, w)
	}

	chunksDone := 0
	for _, c := range p.Chunks {
		if c.State == downloader.ChunkDone {
			chunksDone++
		}
	}
	r.rows = assignRows(r.rows, p.Chunks)
	w, haveWindow := activeWindow(p)
	footer := 3
	if haveWindow {
		footer++
	}
	// The frame must fit on screen: cursor-up stops at the top row, so a
	// taller frame scrolls every time it is drawn, and the next one starts
	// lower. Past the screen height, list as many chunks as fit and count
	// the rest on one line.
	shown := len(r.rows)
	if rows > 0 {
		// One row is left for the cursor, which sits below the frame.
		if room := rows - 1 - footer; shown > room {
			shown = max(room-1, 0)
		}
	}
	for _, i := range r.rows[:shown] {
		c := p.Chunks[i]
		line("chunk %3d %s %9s / %s", c.Index, bar(c.Done, c.Length, 20, r.color),
			fmtBytes(c.Done), fmtBytes(c.Length))
	}
	if more := len(r.rows) - shown; more > 0 {
		line("          … %d more active chunks", more)
	}
	line("total    %s", totalBar(p, totalBarWidth, r.color))
	if haveWindow {
		line("active   %s", totalBar(w, totalBarWidth, r.color))
	}
	line("          %s / %s  (%d/%d chunks)",
		fmtBytes(p.Downloaded), fmtBytes(p.Total), chunksDone, len(p.Chunks))
	line("%s", r.statsLine(p))
	// The frame shrinks as chunks finish; clear whatever the previous,
	// taller frame left below this one, then restore autowrap and the
	// cursor and let the terminal show the frame.
	b.WriteString("\x1b[J\x1b[?7h\x1b[?25h\x1b[?2026l")

	r.drawn = drawn
	io.WriteString(r.out, b.String())
}

// frameRows is how many screen rows a frame whose lines were drawn at the
// given widths takes up now that the terminal is cols wide. Most terminals,
// tmux included, reflow on resize: when the terminal narrows, a line wider
// than it is rewrapped onto more rows, which moves the top of the frame up.
// (Widening never rejoins lines that were drawn as separate lines, so each
// still takes at least one row.)
func frameRows(widths []int, cols int) int {
	n := 0
	for _, w := range widths {
		if cols > 0 && w > cols {
			n += (w + cols - 1) / cols
		} else {
			n++
		}
	}
	return n
}

// screenWidth is how many columns s takes on screen: its runes, less ANSI
// escape sequences. Everything the renderer draws is one column wide.
func screenWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// CSI: parameters, then a final byte in @-~.
			i += 2
			for i < len(s) && (s[i] < '@' || s[i] > '~') {
				i++
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		w++
	}
	return w
}

// assignRows keeps each active chunk on the row it had in the previous
// frame, so rows don't shift up every time a chunk above them finishes. A
// chunk that has just started takes the row of one that has just finished;
// rows left over are dropped, and chunks left over are added at the end.
// It returns indexes into chunks, which are assumed to be in index order.
func assignRows(prev []int, chunks []downloader.ChunkProgress) []int {
	isActive := func(i int) bool {
		return i < len(chunks) && chunks[i].State == downloader.ChunkActive
	}
	onRow := make(map[int]bool, len(prev))
	for _, i := range prev {
		onRow[i] = true
	}
	var started []int
	for i, c := range chunks {
		if c.State == downloader.ChunkActive && !onRow[i] {
			started = append(started, i)
		}
	}
	rows := make([]int, 0, len(prev)+len(started))
	for _, i := range prev {
		switch {
		case isActive(i):
			rows = append(rows, i)
		case len(started) > 0:
			rows = append(rows, started[0])
			started = started[1:]
		}
	}
	return append(rows, started...)
}

// finish leaves the last frame in place and moves on.
func (r *renderer) finish() {
	r.drawn = nil
	r.rows = nil
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

// totalBar draws a map of the whole file: a lead-in cell and then width
// cells of band. The lead-in has room for the tick of a chunk that starts
// at the band's left edge (see bandCells). Without color the map is
// braille, since the band needs background colors.
func totalBar(p downloader.Progress, width int, color bool) string {
	if !color {
		return " " + segmentBar(p, width, false)
	}
	return drawBand(bandCells(p, width))
}

const (
	bandFill   = 33  // blue: downloaded
	bandTrack  = 237 // dark grey: not yet downloaded
	bandMarker = 196 // red: where an active chunk starts
)

// leftEighths[k-1] fills the left k/8 of a cell with the foreground color;
// the rest of the cell shows the background.
var leftEighths = []rune("▏▎▍▌▋▊▉")

// minTickCells is the narrowest a chunk can be drawn, in cells, and still
// get its own segment and tick. Below it chunk starts can't be placed
// evenly, so the band falls back to a plain byte map without ticks.
const minTickCells = 2

// bandCell is one cell of a band: how many of its eighths are downloaded,
// drawn from the left, and whether an active chunk starts at its right
// edge.
type bandCell struct {
	fill int
	tick bool
}

// bandCells lays the file out over width cells, like IDM's "download
// progress by connections" bar, and returns width+1 cells: a lead-in
// followed by the band.
//
// When chunks are at least minTickCells wide, each chunk gets its own
// whole cells: its start and end snap to the nearest cell boundary, and
// its fill is drawn from its first cell to the right, to the eighth, in
// proportion to Done/Length. Snapping keeps every chunk's segment the
// same width to within one cell and puts the fill right against the
// chunk's start, so chunks at the same progress look the same. An active
// chunk's tick goes on the right edge of the cell before its first cell
// (the lead-in for a chunk at the left edge), so it never covers the
// chunk's own fill.
//
// Narrower chunks can't be snapped without distorting the map, so then
// each cell just shows how many of its eighths are downloaded, from the
// left, with no ticks. Where in the cell those eighths are is dropped:
// with several chunk edges per cell, placing them would make the band
// flicker between left- and right-filled cells instead of reading as an
// even comb.
func bandCells(p downloader.Progress, width int) []bandCell {
	cells := make([]bandCell, width+1)
	if p.Total <= 0 {
		return cells
	}
	band := cells[1:]
	var maxLen int64
	for _, c := range p.Chunks {
		maxLen = max(maxLen, c.Length)
	}
	if maxLen*int64(width) < minTickCells*p.Total {
		filled := coverage(p, width*8)
		for i, f := range filled {
			if f {
				band[i/8].fill++
			}
		}
		return cells
	}

	// boundary rounds a byte offset to the nearest cell boundary.
	boundary := func(off int64) int {
		return int((2*off*int64(width) + p.Total) / (2 * p.Total))
	}
	for _, c := range p.Chunks {
		lo, hi := boundary(c.Offset), boundary(c.Offset+c.Length)
		if c.Length > 0 {
			eighths := int(min(max(c.Done, 0), c.Length) * int64(8*(hi-lo)) / c.Length)
			for i := lo; i < hi; i++ {
				band[i].fill = min(max(eighths-8*(i-lo), 0), 8)
			}
		}
		if c.State == downloader.ChunkActive {
			cells[lo].tick = true // band[lo-1], or the lead-in when lo is 0
		}
	}
	return cells
}

// drawBand renders bandCells' output. Each cell is two colors: a left
// eighth block in blue over the grey track, or a red tick over the color
// most of the cell has.
func drawBand(cells []bandCell) string {
	var b strings.Builder
	if cells[0].tick {
		fmt.Fprintf(&b, "\x1b[38;5;%dm▕%s", bandMarker, barReset)
	} else {
		b.WriteByte(' ')
	}
	cur := ""
	for _, c := range cells[1:] {
		g, fg, bg := ' ', bandTrack, bandTrack
		switch {
		case c.tick:
			g, fg = '▕', bandMarker
			if c.fill >= 4 {
				bg = bandFill
			}
		case c.fill == 8:
			fg, bg = bandFill, bandFill
		case c.fill == 0:
		default:
			g, fg = leftEighths[c.fill-1], bandFill
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
