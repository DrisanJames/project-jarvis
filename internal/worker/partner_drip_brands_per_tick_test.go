package worker

import (
	"os"
	"testing"
)

// A cell mints Effective/ActiveIntervals every interval and can only SPEND when its
// brand's turn comes. Whatever is unspent at the day boundary is destroyed by the
// day-roll. Measured on yahoo_family 2026-09-09, burst clamp already removed: spend
// against mint was a UNIFORM 83% across yahoo/aol/att/comcast/sbcglobal. Uniform
// across ISPs is turn cadence, not supply and not a per-ISP ceiling.
func TestBrandsPerTickFor(t *testing.T) {
	const dflt = 4
	cases := []struct {
		name, env, vertical string
		want                int
	}{
		{"unset falls back to the estate default", "", "yahoo_family", dflt},
		{"exact prefix overrides", "yahoo_family=15", "yahoo_family", 15},
		{"prefix match, not equality", "yahoo_=15", "yahoo_family", 15},
		{"a different lane is untouched", "yahoo_family=15", "internal_auto_insurance", dflt},
		{"case insensitive both sides", "YAHOO_Family=12", "yahoo_FAMILY", 12},
		{"whitespace tolerated", "  yahoo_family = 9 ", "yahoo_family", 9},
		{"first matching pair wins", "yahoo_family=15,yahoo_=7", "yahoo_family", 15},
		{"other lanes keep the default while one is raised", "yahoo_family=15", "remodel", dflt},
		// Every malformed form must DEGRADE TO THE DEFAULT, never to 0 — a 0 would
		// fire no brands at all and silently stop the lane.
		{"zero is refused", "yahoo_family=0", "yahoo_family", dflt},
		{"negative is refused", "yahoo_family=-3", "yahoo_family", dflt},
		{"non-numeric is refused", "yahoo_family=lots", "yahoo_family", dflt},
		{"missing value is refused", "yahoo_family=", "yahoo_family", dflt},
		{"no equals sign is refused", "yahoo_family", "yahoo_family", dflt},
		{"empty prefix never matches everything", "=15", "yahoo_family", dflt},
		{"garbage does not break the good pair", "junk,,yahoo_family=15", "yahoo_family", 15},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PARTNER_DRIP_BRANDS_PER_TICK_BY_PREFIX", c.env)
			if got := brandsPerTickFor(c.vertical, dflt); got != c.want {
				t.Fatalf("brandsPerTickFor(%q) with env %q = %d, want %d", c.vertical, c.env, got, c.want)
			}
		})
	}
	_ = os.Unsetenv("PARTNER_DRIP_BRANDS_PER_TICK_BY_PREFIX")
}

// The override may never be able to stop a lane, whatever is in the env.
func TestBrandsPerTickForNeverReturnsNonPositive(t *testing.T) {
	for _, env := range []string{"", "yahoo_family=0", "yahoo_family=-1", "yahoo_family=x", "=0", ",,,"} {
		t.Setenv("PARTNER_DRIP_BRANDS_PER_TICK_BY_PREFIX", env)
		if got := brandsPerTickFor("yahoo_family", 4); got < 1 {
			t.Fatalf("env %q produced %d — a lane that fires no brands is stopped, not paced", env, got)
		}
	}
}
