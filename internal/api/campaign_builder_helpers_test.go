//go:build ignore
// Skipped: references removed symbols (ISPQuota, classifyAndCapSubscribers).

package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func makeSubscribers(emails []string) []subscriber {
	subs := make([]subscriber, len(emails))
	for i, e := range emails {
		subs[i] = subscriber{ID: uuid.New(), Email: e, Status: "confirmed"}
	}
	return subs
}

func TestClassifyAndCapSubscribers_WithQuotas(t *testing.T) {
	registry := engine.NewISPRegistry()

	var emails []string
	for i := 0; i < 40; i++ {
		emails = append(emails, "user@gmail.com")
	}
	for i := 0; i < 30; i++ {
		emails = append(emails, "user@yahoo.com")
	}
	for i := 0; i < 20; i++ {
		emails = append(emails, "user@outlook.com")
	}
	for i := 0; i < 10; i++ {
		emails = append(emails, "user@att.net")
	}

	subs := makeSubscribers(emails)
	quotas := []ISPQuota{
		{ISP: "gmail", Volume: 20},
		{ISP: "yahoo", Volume: 15},
	}

	buckets := classifyAndCapSubscribers(registry, subs, quotas)

	if len(buckets["gmail"]) != 20 {
		t.Errorf("gmail: want 20, got %d", len(buckets["gmail"]))
	}
	if len(buckets["yahoo"]) != 15 {
		t.Errorf("yahoo: want 15, got %d", len(buckets["yahoo"]))
	}
	if len(buckets["microsoft"]) != 20 {
		t.Errorf("microsoft: want 20 (uncapped), got %d", len(buckets["microsoft"]))
	}
	if len(buckets["att"]) != 10 {
		t.Errorf("att: want 10 (uncapped), got %d", len(buckets["att"]))
	}

	// Verify no subscriber appears in multiple buckets
	seen := make(map[uuid.UUID]string)
	for ispKey, subs := range buckets {
		for _, s := range subs {
			if prev, exists := seen[s.ID]; exists {
				t.Errorf("subscriber %s appears in both %s and %s", s.ID, prev, ispKey)
			}
			seen[s.ID] = ispKey
		}
	}

	total := 0
	for _, subs := range buckets {
		total += len(subs)
	}
	if total != 65 { // 20 + 15 + 20 + 10
		t.Errorf("total: want 65, got %d", total)
	}
}

func TestClassifyAndCapSubscribers_EmptyQuotas(t *testing.T) {
	registry := engine.NewISPRegistry()

	subs := makeSubscribers([]string{"a@gmail.com", "b@yahoo.com", "c@outlook.com"})

	buckets := classifyAndCapSubscribers(registry, subs, nil)
	if len(buckets) != 1 {
		t.Errorf("expected 1 bucket (backward compat), got %d", len(buckets))
	}
	if len(buckets[""]) != 3 {
		t.Errorf("expected all 3 subscribers in empty-key bucket, got %d", len(buckets[""]))
	}
}

func TestClassifyAndCapSubscribers_UnknownDomain(t *testing.T) {
	registry := engine.NewISPRegistry()

	subs := makeSubscribers([]string{"a@gmail.com", "b@unknowndomain12345.xyz"})
	quotas := []ISPQuota{
		{ISP: "gmail", Volume: 1},
	}

	buckets := classifyAndCapSubscribers(registry, subs, quotas)
	if len(buckets["gmail"]) != 1 {
		t.Errorf("gmail: want 1, got %d", len(buckets["gmail"]))
	}
	if len(buckets["other"]) != 1 {
		t.Errorf("other: want 1, got %d", len(buckets["other"]))
	}
}
