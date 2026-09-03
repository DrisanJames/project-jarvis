package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// TestDripSupplyActiveIndexParity pins that the partial unique indexes the
// schema (WP1, dripSupplyMigrations) creates are byte-for-byte the DDL the
// contracts package (WP2, ActiveIndexDDL) keys its duplicate-active handling
// on. If either side drifts, activation's 23505 path degrades to a generic
// SQL error — REQ-118 §1.1 "exactly one active per subject".
func TestDripSupplyActiveIndexParity(t *testing.T) {
	ws := regexp.MustCompile(`\s+`)
	norm := func(s string) string { return strings.TrimSpace(ws.ReplaceAllString(s, " ")) }
	have := map[string]string{}
	for _, m := range dripSupplyMigrations {
		if strings.HasPrefix(m.name, "req118_uq_") {
			have[norm(m.sql)] = m.name
		}
	}
	if len(have) != 4 {
		t.Fatalf("expected 4 req118_uq_* entries in dripSupplyMigrations, found %d", len(have))
	}
	for _, kind := range dripsupply.AllKinds() {
		want, err := dripsupply.ActiveIndexDDL(kind)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := have[norm(want)]; !ok {
			t.Errorf("kind %s: ActiveIndexDDL %q has no matching entry in dripSupplyMigrations", kind, want)
		}
	}
}
