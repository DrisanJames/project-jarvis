package preferences

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func hubWith(mode Mode, prefs ...Preference) *Hub {
	h := NewHub(nil, tOrg)
	h.SetModeFunc(func() Mode { return mode })
	for _, p := range prefs {
		h.Apply(p)
	}
	return h
}

func paused(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

func TestShouldSend_PauseScopes(t *testing.T) {
	now := time.Now()
	h := hubWith(ModeShadow,
		Preference{EmailHash: EmailHash("all@x.com"), PausedUntil: paused(24 * time.Hour)},
		Preference{EmailHash: EmailHash("brand@x.com"), BrandRoot: "discountblog.com", PausedUntil: paused(24 * time.Hour)},
		Preference{EmailHash: EmailHash("ended@x.com"), PausedUntil: paused(-time.Hour)},
	)
	cases := []struct {
		email, brand string
		want         bool
	}{
		{"all@x.com", "discountblog.com", false},
		{"ALL@x.com ", "quizfiesta.com", false},
		{"brand@x.com", "discountblog.com", false},
		{"brand@x.com", "quizfiesta.com", true},
		{"ended@x.com", "discountblog.com", true},
		{"nobody@x.com", "discountblog.com", true},
	}
	for _, c := range cases {
		if got, _ := h.ShouldSend(c.email, c.brand, "", now); got != c.want {
			t.Errorf("ShouldSend(%q,%q) = %v, want %v", c.email, c.brand, got, c.want)
		}
	}
}

func TestShouldSend_TopicOnlyWhenTagged(t *testing.T) {
	h := hubWith(ModeEnforce, Preference{EmailHash: EmailHash("t@x.com"), Topics: map[string]bool{"loans": false}})
	if ok, _ := h.ShouldSend("t@x.com", "x.com", "", time.Now()); !ok {
		t.Fatal("untagged campaign must not be blocked by a topic opt-out")
	}
	if ok, r := h.ShouldSend("t@x.com", "x.com", "loans", time.Now()); ok || r != "topic_opt_out" {
		t.Fatalf("tagged campaign: ok=%v reason=%q", ok, r)
	}
	if ok, _ := h.ShouldSend("t@x.com", "x.com", "insurance", time.Now()); !ok {
		t.Fatal("other topics stay subscribed")
	}
}

// Shadow never alters a send, and counts every would-skip by site+reason.
func TestGate_ShadowNeverSkipsAndCounts(t *testing.T) {
	h := hubWith(ModeShadow, Preference{EmailHash: EmailHash("p@x.com"), PausedUntil: paused(time.Hour)})
	for i := 0; i < 3; i++ {
		if skip, reason := h.Gate("send_worker", "p@x.com", "x.com", "", time.Now()); skip || reason != "paused" {
			t.Fatalf("shadow: skip=%v reason=%q", skip, reason)
		}
	}
	if skip, _ := h.Gate("planner", "p@x.com", "x.com", "", time.Now()); skip {
		t.Fatal("shadow planner must not skip")
	}
	if skip, _ := h.Gate("send_worker", "clean@x.com", "x.com", "", time.Now()); skip {
		t.Fatal("clean address must not skip")
	}
	st := h.Status()
	if st.Mode != "shadow" || st.Checked != 5 {
		t.Fatalf("status mode=%s checked=%d", st.Mode, st.Checked)
	}
	if st.WouldSkip["send_worker.paused"] != 3 || st.WouldSkip["planner.paused"] != 1 {
		t.Fatalf("would_skip = %v", st.WouldSkip)
	}
	if len(st.Skipped) != 0 {
		t.Fatalf("shadow must record no skips, got %v", st.Skipped)
	}
}

func TestGate_EnforceSkips(t *testing.T) {
	h := hubWith(ModeEnforce, Preference{EmailHash: EmailHash("p@x.com"), PausedUntil: paused(time.Hour)})
	if skip, reason := h.Gate("clickdrip", "p@x.com", "x.com", "", time.Now()); !skip || reason != "paused" {
		t.Fatalf("enforce: skip=%v reason=%q", skip, reason)
	}
	if h.Status().Skipped["clickdrip.paused"] != 1 {
		t.Fatalf("skipped = %v", h.Status().Skipped)
	}
}

func TestGate_NilHubIsNoop(t *testing.T) {
	var h *Hub
	if skip, _ := h.Gate("send_worker", "p@x.com", "x.com", "", time.Now()); skip {
		t.Fatal("nil hub must never skip")
	}
	if h.Status().Wired {
		t.Fatal("nil hub reports wired")
	}
}

func TestModeFromEnv_DefaultShadow(t *testing.T) {
	t.Setenv("PREFERENCES_MODE", "")
	if ModeFromEnv() != ModeShadow {
		t.Fatal("unset must be shadow")
	}
	t.Setenv("PREFERENCES_MODE", "bogus")
	if ModeFromEnv() != ModeShadow {
		t.Fatal("unknown value must be shadow")
	}
	t.Setenv("PREFERENCES_MODE", " Enforce ")
	if ModeFromEnv() != ModeEnforce {
		t.Fatal("enforce must parse")
	}
}

// Apply with a resumed (no-longer-paused) row evicts it — refresh on write.
func TestApply_ResumeEvicts(t *testing.T) {
	h := hubWith(ModeEnforce, Preference{EmailHash: EmailHash("p@x.com"), PausedUntil: paused(time.Hour)})
	h.Apply(Preference{EmailHash: EmailHash("p@x.com"), Frequency: FrequencyWeekly})
	if ok, _ := h.ShouldSend("p@x.com", "x.com", "", time.Now()); !ok {
		t.Fatal("resume must take effect immediately in this task")
	}
}

func TestLoad_ReplacesSetAndBounds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewHub(db, tOrg)
	h.SetMaxRows(2)
	future := time.Now().Add(time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_subscriber_preferences")).
		WithArgs(tOrg, 3).
		WillReturnRows(sqlmock.NewRows([]string{"email_hash", "brand_root", "paused_until", "topics"}).
			AddRow(EmailHash("a@x.com"), "", future, "{}").
			AddRow(EmailHash("b@x.com"), "x.com", future, "{}").
			AddRow(EmailHash("c@x.com"), "", future, "{}"))
	if err := h.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := h.Status()
	if st.LoadedRows != 2 || !st.Truncated {
		t.Fatalf("loaded=%d truncated=%v", st.LoadedRows, st.Truncated)
	}
	if ok, _ := h.ShouldSend("a@x.com", "x.com", "", time.Now()); ok {
		t.Fatal("loaded pause not applied")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_MissingTableIsEmptyNotError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	h := NewHub(db, tOrg)
	mock.ExpectQuery("mailing_subscriber_preferences").
		WillReturnError(errors.New(`pq: relation "mailing_subscriber_preferences" does not exist`))
	if err := h.Load(context.Background()); err != nil {
		t.Fatalf("missing table must not error: %v", err)
	}
	if ok, _ := h.ShouldSend("a@x.com", "x.com", "", time.Now()); !ok {
		t.Fatal("empty hub must allow")
	}
}
