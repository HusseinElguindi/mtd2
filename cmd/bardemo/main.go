// Command bardemo animates candidate progress bar styles side by side so
// they can be compared in a real terminal. Run: go run ./cmd/bardemo
// Temporary: remove once a style is picked.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

const (
	width = 20
	cyan  = "\x1b[36m"
	dim   = "\x1b[38;5;238m"
	grey  = "\x1b[38;5;244m"
	track = "\x1b[48;5;236m"
	fillB = "\x1b[48;5;30m\x1b[97m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
)

var eighths = []rune("▏▎▍▌▋▊▉")

func styleA(p float64) string { // eighth blocks on a grey background track
	e := int(p * width * 8)
	full, part := e/8, e%8
	s := strings.Repeat("█", full)
	n := full
	if part > 0 {
		s += string(eighths[part-1])
		n++
	}
	return track + cyan + s + strings.Repeat(" ", width-n) + reset
}

func styleB(p float64) string { // shaded blocks
	f := int(p * width)
	r := p*width - float64(f)
	mid := ""
	if f < width {
		mid = "░"
		if r > 0.66 {
			mid = "▓"
		} else if r > 0.33 {
			mid = "▒"
		}
	}
	rest := width - f
	if mid != "" {
		rest--
	}
	return cyan + strings.Repeat("█", f) + mid + reset + dim + strings.Repeat("░", rest) + reset
}

func styleC(p float64) string { // thin line, half-cell steps
	h := int(p * width * 2)
	f, half := h/2, h%2
	s := strings.Repeat("━", f)
	rest := width - f
	if half == 1 {
		s += "╸"
		rest--
	}
	return cyan + s + reset + dim + strings.Repeat("━", rest) + reset
}

func styleD(p float64, total float64) string { // background fill behind the label
	lab := fmt.Sprintf(" %3.0f%%  %5.1f / %.0f MiB", p*100, p*total, total)
	lab += strings.Repeat(" ", width+12-len(lab))
	n := int(p * float64(len(lab)))
	return fillB + lab[:n] + reset + track + grey + lab[n:] + reset
}

func styleE(p float64) string { // braille dots
	parts := []rune("⡀⡄⡆⡇⣇⣧⣷")
	e := int(p * width * 8)
	full, part := e/8, e%8
	s := strings.Repeat("⣿", full)
	n := full
	if part > 0 {
		s += string(parts[part-1])
		n++
	}
	return cyan + s + reset + dim + strings.Repeat("⣀", width-n) + reset
}

func styleF(p float64) string { // squares
	f := int(p*width + 0.5)
	return cyan + strings.Repeat("■", f) + reset + dim + strings.Repeat("■", width-f) + reset
}

func main() {
	styles := []struct {
		name string
		draw func(float64) string
	}{
		{"A  Eighth blocks on grey track (current PR)", styleA},
		{"B  Shaded blocks", styleB},
		{"C  Thin line", styleC},
		{"D  Background fill behind the label", func(p float64) string { return styleD(p, 512) }},
		{"E  Braille dots", styleE},
		{"F  Squares", styleF},
	}
	offsets := []float64{0, 0.33, 0.66}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	fmt.Print("\x1b[?25l") // hide cursor
	defer fmt.Print("\x1b[?25h")

	lines := 0
	start := time.Now()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		t := time.Since(start).Seconds() / 8 // one full sweep every 8s
		var b strings.Builder
		if lines > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", lines)
		}
		lines = 0
		for _, s := range styles {
			fmt.Fprintf(&b, "\x1b[2K%s%s%s\n", bold, s.name, reset)
			lines++
			for _, o := range offsets {
				x := t + o
				p := x - float64(int(x))
				fmt.Fprintf(&b, "\x1b[2K  %s  %3.0f%%\n", s.draw(p), p*100)
				lines++
			}
			b.WriteString("\x1b[2K\n")
			lines++
		}
		b.WriteString("\x1b[2KCtrl-C to quit\n")
		lines++
		os.Stdout.WriteString(b.String())

		select {
		case <-sig:
			return
		case <-tick.C:
		}
	}
}
