package main

import (
	"fmt"
	"io"
	"math"
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
	lines    int // lines drawn by the previous frame, to move back over
}

func newRenderer(out *os.File) *renderer {
	fi, err := out.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	interval := 2 * time.Second
	if tty {
		interval = 100 * time.Millisecond
	}
	return &renderer{out: out, tty: tty, interval: interval}
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
	// Return to the top of the previous frame; \x1b[2K clears each line as
	// it is redrawn, so shorter frames leave no residue.
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
		line("chunk %3d %s %9s / %s", c.Index, bar(c.Done, c.Length, 20),
			fmtBytes(c.Done), fmtBytes(c.Length))
	}
	chunksDone := 0
	for _, c := range p.Chunks {
		if c.State == downloader.ChunkDone {
			chunksDone++
		}
	}
	line("total     %s %9s / %s  (%d/%d chunks)", bar(p.Downloaded, p.Total, 20),
		fmtBytes(p.Downloaded), fmtBytes(p.Total), chunksDone, len(p.Chunks))
	line("%s", r.statsLine(p))

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

func bar(done, total int64, width int) string {
	if total <= 0 {
		return "[" + strings.Repeat(" ", width) + "]"
	}
	filled := int(math.Round(float64(done) / float64(total) * float64(width)))
	filled = min(max(filled, 0), width)
	b := strings.Repeat("=", filled)
	if filled > 0 && filled < width {
		b = b[:filled-1] + ">"
	}
	return "[" + b + strings.Repeat(" ", width-filled) + "]"
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
