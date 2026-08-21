package worker

import (
	"os"
	"testing"
)

// mustReadSource reads a file from this package's directory for source-level
// guards (used by the "never writes daily_budget" fixture).
func mustReadSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
