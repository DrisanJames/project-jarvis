package worker

import (
	"os"
	"strings"
	"testing"
)

// 2026-09-11: refresh() re-issued four no-op ADD COLUMNs on mailing_lists on
// every tick; each queued ACCESS EXCLUSIVE behind long readers. The columns
// belong to the boot migrations (catalog-probed), never to a ticking worker.
func TestListRefresh_NoDDL(t *testing.T) {
	src, err := os.ReadFile("list_refresh.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(src)), "ALTER TABLE") {
		t.Fatal("list_refresh.go must not run DDL; its columns come from runStartupMigrations")
	}
}
