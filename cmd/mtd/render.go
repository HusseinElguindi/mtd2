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
	line("total     %s %9s / %s  (%d/%d chunks)", segmentBar(p, 20, r.color),
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

// Bars are drawn in braille. Each cell holds 8 dots, so a cell has 9 fill
// levels: brailleLevels[n] is a cell with n dots lit, filling the left
// column bottom-up and then the right, which reads as left-to-right.
var brailleLevels = []rune("⣀⡀⡄⡆⡇⣇⣧⣷⣿")

const (
	barFill  = "\x1b[36m"       // cyan: downloaded
	barHead  = "\x1b[93m"       // yellow: a cell an active chunk is writing into
	barTrack = "\x1b[38;5;238m" // dark grey: not yet downloaded
	barReset = "\x1b[0m"
)

// cell is one character of a bar: how many of its 8 dots are lit, and
// whether an active chunk's write position falls inside it.
type cell struct {
	level int
	head  bool
}

// bar draws done/total as a bar width cells wide that advances a dot
// (1/8 cell) at a time.
func bar(done, total int64, width int, color bool) string {
	cells := make([]cell, width)
	if total > 0 {
		dots := int(float64(done) / float64(total) * float64(width*8))
		dots = min(max(dots, 0), width*8)
		for i := range cells {
			cells[i].level = min(max(dots-i*8, 0), 8)
		}
	}
	return drawCells(cells, color)
}

// segmentBar draws the whole file as a map, IDM-style: each cell covers
// total/width bytes, and lights up only as far as the chunks overlapping
// it have actually downloaded, so every chunk's segment fills in at its
// own place in the file. Cells holding an active chunk's write position
// are highlighted.
func segmentBar(p downloader.Progress, width int, color bool) string {
	cells := make([]cell, width)
	if p.Total <= 0 {
		return drawCells(cells, color)
	}
	span := float64(p.Total) / float64(width)
	got := make([]float64, width) // downloaded bytes falling in each cell
	for _, c := range p.Chunks {
		start, end := float64(c.Offset), float64(c.Offset+c.Done)
		if end <= start {
			continue
		}
		first := int(start / span)
		last := min(int((end-1)/span), width-1)
		for i := first; i <= last; i++ {
			lo, hi := max(start, float64(i)*span), min(end, float64(i+1)*span)
			got[i] += hi - lo
		}
		if c.State == downloader.ChunkActive && c.Done < c.Length {
			cells[last].head = true
		}
	}
	for i, g := range got {
		lvl := int(g / span * 8)
		if g > 0 && lvl == 0 {
			lvl = 1 // show that something landed here
		}
		cells[i].level = min(lvl, 8)
	}
	return drawCells(cells, color)
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
			case c.level > 0:
				want = barFill
			}
			if want != cur {
				b.WriteString(want)
				cur = want
			}
		}
		b.WriteRune(brailleLevels[c.level])
	}
	if color {
		b.WriteString(barReset)
	}
	return b.String()
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
