package guard

import "testing"

func TestDBHost(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@db.example.com:5432/app":            "db.example.com",
		"postgres://u:p@localhost:5432/app?sslmode=disable": "localhost",
		"host=apex-postgres port=5432 user=app dbname=app":  "apex-postgres",
		"": "",
	}
	for dsn, want := range cases {
		if got := dbHost(dsn); got != want {
			t.Errorf("dbHost(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestDecision(t *testing.T) {
	const remote = "postgres://u:p@prod-db.us-west-2.rds.amazonaws.com:5432/app"
	const local = "postgres://u:p@localhost:5432/app"

	tests := []struct {
		name        string
		dsn         string
		mutating    bool
		confirmed   bool
		wantAllowed bool
	}{
		{"local mutating unconfirmed → allowed", local, true, false, true},
		{"local read unconfirmed → allowed", local, false, false, true},
		{"remote read unconfirmed → allowed (read-only)", remote, false, false, true},
		{"remote mutating unconfirmed → REFUSED", remote, true, false, false},
		{"remote mutating confirmed → allowed", remote, true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := decision(tc.dsn, "tool", tc.mutating, tc.confirmed)
			if got != tc.wantAllowed {
				t.Errorf("decision() allowed = %v, want %v (msg=%q)", got, tc.wantAllowed, msg)
			}
			if msg == "" {
				t.Errorf("decision() returned empty message")
			}
		})
	}
}
