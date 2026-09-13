package worker

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

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

// A pass that ran less than half an interval ago is skipped (double-boot guard);
// nothing is listed or run.
func TestBrainEvalSkipsRecentPass(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT max(ran_at) FROM jarvis_brain_eval_runs")).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(time.Now().Add(-30 * time.Second)))
	mock.ExpectExec("INSERT INTO mailing_worker_heartbeats").WillReturnResult(sqlmock.NewResult(0, 1))
	w := NewBrainEvalWorker(db, nil, brain.NewRunner(db, brain.NewStore(db), nil, "", "", "test"))
	if p, f := w.RunOnce(context.Background()); p != 0 || f != 0 {
		t.Fatalf("expected skip, got %d/%d", p, f)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("suite must not be listed after a recent pass: %v", err)
	}
}
