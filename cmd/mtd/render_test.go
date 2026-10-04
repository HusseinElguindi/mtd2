package main

import "testing"

func TestBar(t *testing.T) {
	cases := []struct {
		done, total int64
		want        string
	}{
		{0, 0, "░░░░"},
		{0, 100, "░░░░"},
		{100, 100, "████"},
		{50, 100, "██░░"},
		{1, 32, "▏░░░"},  // one eighth of one cell
		{15, 32, "█▉░░"}, // 15/32 of 4 cells = 1 7/8 cells
		{200, 100, "████"},
		{-5, 100, "░░░░"},
	}
	for _, c := range cases {
		if got := bar(c.done, c.total, 4, false); got != c.want {
			t.Errorf("bar(%d, %d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
	if got, want := bar(1, 32, 4, true), barTrack+barFill+"▏   "+barReset; got != want {
		t.Errorf("color bar = %q, want %q", got, want)
	}
}
