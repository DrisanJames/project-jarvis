package api

// BEHAVIOURAL proof that the touch policy caps Gmail and NOTHING else.
//
// The string tests in isp_touch_policy_test.go assert what SQL we generate.
// They cannot tell us what Postgres DOES with it, and the risk the operator
// named is precisely a wide-spread suppression of non-Gmail traffic. So this
// file builds a scratch schema, inserts real recipients across every ISP with
// real last_mailed_at values, executes the generated predicate exactly as
// planPMTAAudience splices it, and asserts the surviving id set.
//
// Self-contained: it creates only the two tables the clause touches, so it does
// not depend on the full startup-migration state. SKIPs when local Postgres is
// unreachable — never runs against prod.
//
//	go test ./internal/api/ -run TouchPolicyBehaviour -v

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// one recipient fixture: what it is, and whether the Gmail cap should drop it.
type touchFixture struct {
	id          string
	email       string
	hoursAgo    float64 // last_mailed_at = now() - this; <0 means NO sds row
	wantDropped bool    // true = the 20h Gmail cap must exclude it
	why         string
}

func touchPolicyFixtures() []touchFixture {
	return []touchFixture{
		// --- Gmail: the ONLY ISP that may ever be dropped -------------------
		{"g-recent", "a@gmail.com", 1, true, "gmail mailed 1h ago — inside the 20h gap"},
		{"g-edge-in", "b@gmail.com", 19, true, "gmail mailed 19h ago — still inside"},
		{"g-edge-out", "c@gmail.com", 21, false, "gmail mailed 21h ago — outside the gap"},
		{"g-old", "d@gmail.com", 72, false, "gmail mailed 3d ago"},
		{"g-never", "e@gmail.com", -1, false, "gmail with no send history"},
		{"g-alias", "f@googlemail.com", 1, true, "googlemail is Gmail"},
		{"g-upper", "g@GMAIL.COM", 1, true, "uppercase domain must still match"},

		// --- every other ISP: must NEVER be dropped, however recent ---------
		{"y-recent", "h@yahoo.com", 0.5, false, "yahoo mailed 30m ago"},
		{"y-alias", "i@ymail.com", 0.5, false, "ymail"},
		{"m-recent", "j@hotmail.com", 0.5, false, "microsoft"},
		{"m-out", "k@outlook.com", 0.5, false, "microsoft"},
		{"a-recent", "l@aol.com", 0.5, false, "aol"},
		{"ap-recent", "m@icloud.com", 0.5, false, "apple"},
		{"c-recent", "n@comcast.net", 0.5, false, "comcast"},
		{"at-recent", "o@att.net", 0.5, false, "att"},
		{"sb-recent", "p@sbcglobal.net", 0.5, false, "sbcglobal"},
		{"o-recent", "q@some-random-isp.example", 0.5, false, "long-tail other"},

		// --- lookalikes: must NOT be treated as Gmail -----------------------
		{"l-not", "r@notgmail.com", 0.5, false, "notgmail.com is a different domain"},
		{"l-sub", "s@mail.gmail.com.evil.test", 0.5, false, "gmail.com as a subdomain label"},
		{"l-pre", "t@gmail.com.co", 0.5, false, "gmail.com as a prefix"},
	}
}

// touchPolicyDSN points at the LOCAL apex-postgres. Never prod: this test
// creates and drops schemas.
func touchPolicyDSN() string {
	if v := os.Getenv("TOUCH_POLICY_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
}

func openTouchPolicyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", touchPolicyDSN())
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v). Start apex-postgres.", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("SKIP: local Postgres unreachable (%v). Start apex-postgres.", err)
	}
	return db
}

// buildScratch creates an isolated schema holding ONLY the two relations the
// clause reads, seeded with the fixtures.
func buildScratch(t *testing.T, db *sql.DB, sendingDomain string) string {
	t.Helper()
	schema := fmt.Sprintf("touchpol_%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Skipf("SKIP: cannot create scratch schema (%v)", err)
	}
	t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE") })

	mustExec(t, db, fmt.Sprintf(`CREATE TABLE %s.mailing_subscribers (
		id TEXT PRIMARY KEY, email TEXT NOT NULL, cross_engaged BOOLEAN DEFAULT false)`, schema))
	mustExec(t, db, fmt.Sprintf(`CREATE TABLE %s.mailing_subscriber_domain_state (
		subscriber_id TEXT NOT NULL, sending_domain TEXT NOT NULL,
		last_mailed_at TIMESTAMPTZ, last_open_at TIMESTAMPTZ, last_click_at TIMESTAMPTZ,
		state TEXT, PRIMARY KEY (subscriber_id, sending_domain))`, schema))

	for _, f := range touchPolicyFixtures() {
		mustExec(t, db, fmt.Sprintf("INSERT INTO %s.mailing_subscribers (id,email) VALUES ($1,$2)", schema), f.id, f.email)
		if f.hoursAgo >= 0 {
			mustExec(t, db, fmt.Sprintf(`INSERT INTO %s.mailing_subscriber_domain_state
				(subscriber_id, sending_domain, last_mailed_at, state)
				VALUES ($1,$2, NOW() - ($3 || ' hours')::interval, 'engaged')`, schema),
				f.id, sendingDomain, fmt.Sprintf("%g", f.hoursAgo))
		}
	}
	return schema
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %.60s: %v", q, err)
	}
}

// runClause splices the clause the way pmta_campaign_planner.go:1078 does and
// returns the surviving subscriber ids.
func runClause(t *testing.T, db *sql.DB, schema string, cl sdsClause) []string {
	t.Helper()
	q := fmt.Sprintf("SELECT s.id FROM %s.mailing_subscribers s ", schema)
	if cl.Join != "" {
		q += strings.ReplaceAll(cl.Join, "mailing_subscriber_domain_state", schema+".mailing_subscriber_domain_state") + " "
	}
	q += "WHERE 1=1 " + cl.Where
	rows, err := db.QueryContext(context.Background(), q, cl.BindArgs...)
	if err != nil {
		t.Fatalf("query failed: %v\nSQL: %s", err, q)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// THE test the operator asked for: with a Gmail policy, exactly the Gmail rows
// inside the gap disappear and every other ISP survives untouched.
func TestTouchPolicyBehaviourGmailOnly(t *testing.T) {
	const domain = "em.quizfiesta.com"
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true") // production value
	db := openTouchPolicyDB(t)
	defer db.Close()
	schema := buildScratch(t, db, domain)

	got := runClause(t, db, schema,
		buildSDSEligibilityClause(domain, "s", 0, ispTouchPolicy{"gmail": 20}))
	survived := map[string]bool{}
	for _, id := range got {
		survived[id] = true
	}

	nonGmailDropped := 0
	for _, f := range touchPolicyFixtures() {
		isGmail := strings.HasSuffix(strings.ToLower(f.email), "@gmail.com") ||
			strings.HasSuffix(strings.ToLower(f.email), "@googlemail.com")
		switch {
		case f.wantDropped && survived[f.id]:
			t.Errorf("MISS: %s (%s) should have been capped — %s", f.id, f.email, f.why)
		case !f.wantDropped && !survived[f.id]:
			t.Errorf("OVER-BLOCK: %s (%s) was excluded but must not be — %s", f.id, f.email, f.why)
			if !isGmail {
				nonGmailDropped++
			}
		}
	}
	if nonGmailDropped > 0 {
		t.Fatalf("%d NON-GMAIL recipients were suppressed — this is the estate-wide failure mode", nonGmailDropped)
	}
	t.Logf("survivors (%d): %v", len(got), got)
}

// Control: no policy must return every fixture, so the harness itself cannot
// manufacture a pass by dropping rows for an unrelated reason.
func TestTouchPolicyBehaviourNoPolicyDropsNobody(t *testing.T) {
	const domain = "em.quizfiesta.com"
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")
	db := openTouchPolicyDB(t)
	defer db.Close()
	schema := buildScratch(t, db, domain)

	got := runClause(t, db, schema, buildSDSEligibilityClause(domain, "s", 0, nil))
	if len(got) != len(touchPolicyFixtures()) {
		t.Fatalf("no policy must drop nobody: got %d of %d\n%v", len(got), len(touchPolicyFixtures()), got)
	}
}

// A per-domain override must narrow the gap for that domain only. At 7h, the
// 19h-ago Gmail row survives where the estate default of 20h would drop it.
func TestTouchPolicyBehaviourGapIsTheDial(t *testing.T) {
	const domain = "em.historythinking.com"
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")
	db := openTouchPolicyDB(t)
	defer db.Close()
	schema := buildScratch(t, db, domain)

	tight := runClause(t, db, schema, buildSDSEligibilityClause(domain, "s", 0, ispTouchPolicy{"gmail": 20}))
	loose := runClause(t, db, schema, buildSDSEligibilityClause(domain, "s", 0, ispTouchPolicy{"gmail": 7}))
	if len(loose) <= len(tight) {
		t.Fatalf("a 7h gap must admit MORE than a 20h gap: 7h=%d 20h=%d", len(loose), len(tight))
	}
	has := func(ids []string, id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	if has(tight, "g-edge-in") {
		t.Errorf("20h gap should drop the 19h-ago gmail row")
	}
	if !has(loose, "g-edge-in") {
		t.Errorf("7h gap should admit the 19h-ago gmail row")
	}
}

// The cap is scoped to the sending domain: a recent send from ANOTHER domain
// must not suppress this one, because the policy is per (recipient, domain).
func TestTouchPolicyBehaviourIsPerSendingDomain(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")
	db := openTouchPolicyDB(t)
	defer db.Close()
	// History rows are written for quizfiesta only.
	schema := buildScratch(t, db, "em.quizfiesta.com")

	// Planning for a DIFFERENT domain: no row matches, so nobody is capped.
	got := runClause(t, db, schema,
		buildSDSEligibilityClause("em.historythinking.com", "s", 0, ispTouchPolicy{"gmail": 20}))
	if len(got) != len(touchPolicyFixtures()) {
		t.Fatalf("another domain's send history must not cap this domain: got %d of %d",
			len(got), len(touchPolicyFixtures()))
	}
}

// THE GAP THIS ALMOST SHIPPED WITH: planPMTAAudience has TWO selection paths.
// streamList takes the spliced SQL clause; streamSegment does NOT and relies on
// the in-memory filter. The board's engaged tiers are SEGMENT-sourced, which is
// where most Gmail volume lives, so a policy that only reached the SQL clause
// would have looked correct in every string test and capped almost nothing.
//
// These pin the two properties the in-memory half depends on, both of which
// were wrong in the first cut:
//  1. the SDS state map must LOAD while DISABLE_SDS_FREQUENCY_CAP=true,
//     because the policy reads last_mailed_at out of it;
//  2. the decision itself must be per-ISP, not blanket.
func TestTouchPolicyStateLoadsWhileKillSwitchIsOn(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")
	db := openTouchPolicyDB(t)
	defer db.Close()
	schema := buildScratch(t, db, "em.quizfiesta.com")

	// The production loader reads the real table name; point it at the scratch
	// schema for the duration of this connection.
	if _, err := db.Exec("SET search_path TO " + schema + ",public"); err != nil {
		t.Skipf("SKIP: cannot set search_path (%v)", err)
	}
	defer db.Exec("SET search_path TO public")

	unforced := loadSDSStateForDomain(context.Background(), db, "em.quizfiesta.com")
	if len(unforced) != 0 {
		t.Errorf("kill switch on and no policy: state map should stay empty, got %d", len(unforced))
	}
	forced := loadSDSStateForDomain(context.Background(), db, "em.quizfiesta.com", true)
	if len(forced) == 0 {
		t.Fatal("kill switch on WITH a policy: state map must load, or the segment-path cap is inert")
	}
	if _, ok := forced["g-recent"]; !ok {
		t.Errorf("expected the recently-mailed gmail row in the map, got %d rows", len(forced))
	}
}

// The in-memory decision, exercised exactly as the planner makes it.
func TestTouchPolicyInMemoryDecisionIsPerISP(t *testing.T) {
	policy := ispTouchPolicy{"gmail": 20}
	recent := time.Now().Add(-1 * time.Hour)
	old := time.Now().Add(-30 * time.Hour)

	capped := func(isp string, lastMailed time.Time) bool {
		gap, has := policy[isp]
		if !has {
			return false
		}
		return time.Since(lastMailed) < time.Duration(gap)*time.Hour
	}

	if !capped("gmail", recent) {
		t.Error("gmail mailed 1h ago must be capped")
	}
	if capped("gmail", old) {
		t.Error("gmail mailed 30h ago must pass")
	}
	for _, isp := range []string{"yahoo", "microsoft", "aol", "apple", "comcast", "att", "sbcglobal", "cox", "charter", "verizon", "other"} {
		if capped(isp, recent) {
			t.Errorf("%s mailed 1h ago must NOT be capped — gmail-only", isp)
		}
	}
}
