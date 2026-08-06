package worker

import (
	"strings"
	"testing"
)

func TestNamesFromExtra(t *testing.T) {
	cases := []struct {
		name        string
		extra       string
		wantFirst   string
		wantLast    string
	}{
		{"both present", `{"first_name":"Derek","last_name":"Delfino","state":"FL"}`, "Derek", "Delfino"},
		{"api door empty strings", `{"first_name":"","last_name":"","zip":"","metadata":null}`, "", ""},
		{"keys absent", `{"source":"drivesource","drive_file_id":"x"}`, "", ""},
		{"nil extra", "", "", ""},
		{"invalid json", `{not json`, "", ""},
		{"whitespace only", `{"first_name":"   "}`, "", ""},
		{"trims", `{"first_name":" Ann ","last_name":" Lee "}`, "Ann", "Lee"},
		{"caps at 100 runes", `{"first_name":"` + strings.Repeat("a", 150) + `"}`, strings.Repeat("a", 100), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var extra []byte
			if c.extra != "" {
				extra = []byte(c.extra)
			}
			first, last := namesFromExtra(extra)
			if first != c.wantFirst || last != c.wantLast {
				t.Fatalf("namesFromExtra(%q) = (%q,%q), want (%q,%q)",
					c.extra, first, last, c.wantFirst, c.wantLast)
			}
		})
	}
}
