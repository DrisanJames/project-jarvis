package worker

import "testing"

func TestResolveEOConcurrency(t *testing.T) {
	cases := []struct {
		cfg  int
		env  string
		want int
	}{
		{0, "", 10}, {12, "", 12}, {10, "20", 20}, {10, "41", 40}, {10, "400", 40},
		{10, "0", 10}, {10, "-3", 10}, {10, "abc", 10}, {0, "abc", 10}, {60, "", 40}, {10, " 25 ", 25},
	}
	for _, c := range cases {
		if got, src := ResolveEOConcurrency(c.cfg, c.env); got != c.want {
			t.Errorf("ResolveEOConcurrency(%d, %q) = %d (%s), want %d", c.cfg, c.env, got, src, c.want)
		}
	}
}
