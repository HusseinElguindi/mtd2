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
	bandFill  = 33  // blue: downloaded
	bandTrack = 237 // dark grey: not yet downloaded
)

// shadeLevel buckets how many of a cell's eighths are downloaded into
// 0 (empty), 1-3 (light, medium, dark shade) or 4 (full).
func shadeLevel(n int) int {
	switch {
	case n == 0:
		return 0
	case n <= 2:
		return 1
	case n <= 5:
		return 2
	case n <= 7:
		return 3
	default:
		return 4
	}
}

// Shade glyphs by level. In color the shades are drawn in blue over the
// grey track, so the gaps in their dot patterns show the track rather
// than the terminal background; full and empty cells are solid
// background. Without color, ░ stands in for the track.
var (
	colorShades = []rune(" ░▒▓ ")
	plainShades = []rune("░▒▒▓█")
)

// bandBar draws the file as a band, like IDM's "download progress by
// connections" bar: downloaded ranges fill in at their place in the file,
// so each chunk's fill grows rightward from its start. Each cell is shaded
// by how much of it is downloaded.
func bandBar(p downloader.Progress, width int, color bool) string {
	filled := coverage(p, width*8)
	var b strings.Builder
	cur := ""
	for i := range width {
		n := 0
		for _, f := range filled[i*8 : i*8+8] {
			if f {
				n++
			}
		}
		lvl := shadeLevel(n)
		if !color {
			b.WriteRune(plainShades[lvl])
			continue
		}
		bg := bandTrack
		if lvl == 4 {
			bg = bandFill
		}
		if want := fmt.Sprintf("\x1b[38;5;%dm\x1b[48;5;%dm", bandFill, bg); want != cur {
			b.WriteString(want)
			cur = want
		}
		b.WriteRune(colorShades[lvl])
	}
	if color {
		b.WriteString(barReset)
	}
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
