package main

import "testing"

func TestMailingDBMaxOpenConns(t *testing.T) {
	cases := map[string]int{"": 40, "abc": 40, "0": 40, "-5": 40, "5": 10, "70": 70, "150": 150, "400": 150, " 60 ": 60}
	for in, want := range cases {
		t.Setenv("DB_MAX_OPEN_CONNS", in)
		if got := mailingDBMaxOpenConns(); got != want {
			t.Errorf("DB_MAX_OPEN_CONNS=%q: got %d want %d", in, got, want)
		}
	}
}
