package worker

import (
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/brain"
)

type brainDigestNotifier struct{ titles, bodies []string }

func (c *brainDigestNotifier) Notify(title, body string) error {
	c.titles = append(c.titles, title)
	c.bodies = append(c.bodies, body)
	return nil
}
func (c *brainDigestNotifier) Name() string { return "capture" }

// No notifier ⇒ no post, no panic. Failures ⇒ WARN with the failing names.
func TestBrainEvalDigest(t *testing.T) {
	w := &BrainEvalWorker{}
	w.postDigest(3, 0, nil) // must not panic without a notifier

	c := &brainDigestNotifier{}
	w.SetNotifier(c)
	w.postDigest(8, 0, nil)
	if len(c.titles) != 1 || !strings.Contains(c.titles[0]+c.bodies[0], "8/8") {
		t.Fatalf("all-pass digest = %v / %v", c.titles, c.bodies)
	}
	c = &brainDigestNotifier{}
	w.SetNotifier(c)
	line := brainEvalFailLine(brain.Eval{Name: "fc_moo_20260909_lake_delivery"}, brain.EvalRun{Error: "row count: expected 1, actual 0"})
	w.postDigest(7, 1, []string{line})
	got := strings.Join(c.titles, " ") + " " + strings.Join(c.bodies, " ")
	if !strings.Contains(got, "7/8") || !strings.Contains(got, "fc_moo_20260909_lake_delivery") || !strings.Contains(got, "WARN") {
		t.Fatalf("failing digest = %q", got)
	}
}
