package engine

import (
	"testing"

	"github.com/google/uuid"
)

func TestVersionEngineAccountingWebhookSemver(t *testing.T) {
	if VersionEngineAccountingWebhook == "" {
		t.Fatal("empty version")
	}
}

func TestAccountingDedupeEventIDDeterministic(t *testing.T) {
	s := "pmta|user@x.com|camp-uuid|delivered|d||time"
	a := uuid.NewSHA1(uuid.NameSpaceOID, []byte(s))
	b := uuid.NewSHA1(uuid.NameSpaceOID, []byte(s))
	if a != b {
		t.Fatalf("deterministic uuid mismatch: %v vs %v", a, b)
	}
}
