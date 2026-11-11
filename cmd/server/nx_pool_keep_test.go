package main

import (
	"os"
	"strings"
	"testing"
)

// The nx-pool keep guard must be installed before the first legacy migration that
// moves nx addresses, and the startup-migration session must carry the marker it
// reads — otherwise every boot empties the nx pools until the final pass.
func TestNxPoolKeepGuard_WiredBeforeLegacyMovesAndMarkerSet(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	trg := strings.Index(src, `{"nx_pool_keep_trigger_20261111", nxPoolKeepTriggerSQL}`)
	fn := strings.Index(src, `{"nx_pool_keep_fn_20261111", nxPoolKeepFnSQL}`)
	if trg < 0 || fn < 0 || fn > trg {
		t.Fatalf("keep fn/trigger migrations missing or out of order (fn=%d trg=%d)", fn, trg)
	}
	for _, legacy := range []string{`{"phase6_seed_all_ips"`, `{"may08_reassign_harvested_ips"`, `{"phase12_ipxo_out_of_yahoo"`, `{"aug26_yahoo_set_ip_reassert"`} {
		i := strings.Index(src, legacy)
		if i < 0 {
			t.Fatalf("%s not found", legacy)
		}
		if i < trg {
			t.Errorf("%s runs before the nx keep trigger is installed", legacy)
		}
	}
	if !strings.Contains(src, "execSQL(startupMigrationMarker)") || !strings.Contains(src, "tx.ExecContext(ctx, startupMigrationMarkerLocal)") {
		t.Error("startup-migration session does not set jarvis.startup_migration on both paths")
	}
	if !strings.Contains(nxPoolKeepFnSQL, "current_setting('jarvis.startup_migration', true) = 'on'") ||
		!strings.Contains(nxPoolKeepTriggerSQL, "BEFORE UPDATE OF pool_id ON mailing_ip_addresses") {
		t.Error("guard is not scoped to the startup-migration session / pool_id updates")
	}
}
