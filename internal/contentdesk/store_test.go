package contentdesk

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	tOrg  = "00000000-0000-0000-0000-000000000001"
	tArt  = "33333333-3333-3333-3333-333333333333"
	tSite = "44444444-4444-4444-4444-444444444444"
)

func newMock(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db), mock
}

func q(s string) string { return regexp.QuoteMeta(s) }

func runCols() []string { return []string{"id", "status", "attempts", "output", "stale"} }

// ── pipeline idempotency (content_pipeline_runs) ──

func TestRunStage_FirstRunCallsOnceAndRecords(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectQuery(q("INSERT INTO content_pipeline_runs")).WithArgs(tOrg, tArt, StageDraft, "h1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run1"))
	mock.ExpectExec(q("SET status = 'succeeded'")).WithArgs("run1", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	p := &Pipeline{Store: s}
	calls := 0
	out, u, err := p.RunStage(context.Background(), tOrg, tArt, StageDraft, "h1", func(context.Context) (any, Usage, error) {
		calls++
		return map[string]int{"x": 1}, Usage{USD: 0.5}, nil
	})
	if err != nil || calls != 1 || string(out) != `{"x":1}` || u.USD != 0.5 {
		t.Fatalf("err=%v calls=%d out=%s u=%+v", err, calls, out, u)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStage_DoubleFireIsInFlightAndNeverCallsTheModel(t *testing.T) {
	s, mock := newMock(t)
	// The second fire loses the INSERT (unique key) and finds the first still running.
	mock.ExpectQuery(q("INSERT INTO content_pipeline_runs")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(q("SELECT id, status, attempts")).WithArgs(tArt, StageResearch, "h1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(runCols()).AddRow("run1", "running", 1, []byte("null"), false))
	p := &Pipeline{Store: s}
	calls := 0
	_, _, err := p.RunStage(context.Background(), tOrg, tArt, StageResearch, "h1", func(context.Context) (any, Usage, error) {
		calls++
		return nil, Usage{}, nil
	})
	if !errors.Is(err, ErrInFlight) || calls != 0 {
		t.Fatalf("double fire must not run: err=%v calls=%d", err, calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStage_SucceededInputIsReusedNotRerun(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectQuery(q("INSERT INTO content_pipeline_runs")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(q("SELECT id, status, attempts")).
		WillReturnRows(sqlmock.NewRows(runCols()).AddRow("run1", "succeeded", 1, []byte(`{"result":{"x":2},"usage":{"usd":1.5}}`), false))
	p := &Pipeline{Store: s}
	calls := 0
	out, u, err := p.RunStage(context.Background(), tOrg, tArt, StageResearch, "h1", func(context.Context) (any, Usage, error) {
		calls++
		return nil, Usage{}, nil
	})
	if err != nil || calls != 0 || string(out) != `{"x":2}` || u.USD != 1.5 {
		t.Fatalf("err=%v calls=%d out=%s u=%+v", err, calls, out, u)
	}
}

func TestRunStage_FailedRetakeAndExhaustion(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectQuery(q("INSERT INTO content_pipeline_runs")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(q("SELECT id, status, attempts")).
		WillReturnRows(sqlmock.NewRows(runCols()).AddRow("run1", "failed", 1, []byte("null"), false))
	mock.ExpectQuery(q("SET status = 'running', attempts = attempts + 1")).WithArgs("run1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run1"))
	mock.ExpectExec(q("SET status = 'succeeded'")).WillReturnResult(sqlmock.NewResult(0, 1))
	p := &Pipeline{Store: s, MaxAttempts: 3}
	calls := 0
	fn := func(context.Context) (any, Usage, error) { calls++; return 1, Usage{}, nil }
	if _, _, err := p.RunStage(context.Background(), tOrg, tArt, StageDraft, "h1", fn); err != nil || calls != 1 {
		t.Fatalf("retake: err=%v calls=%d", err, calls)
	}
	mock.ExpectQuery(q("INSERT INTO content_pipeline_runs")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(q("SELECT id, status, attempts")).
		WillReturnRows(sqlmock.NewRows(runCols()).AddRow("run1", "failed", 3, []byte("null"), false))
	if _, _, err := p.RunStage(context.Background(), tOrg, tArt, StageDraft, "h1", fn); !errors.Is(err, ErrRunExhausted) || calls != 1 {
		t.Fatalf("exhausted: err=%v calls=%d", err, calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStage_BudgetExceededRefundsTheAttempt(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectQuery(q("INSERT INTO content_pipeline_runs")).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run1"))
	mock.ExpectExec(q("attempts = GREATEST(attempts - 1, 0)")).WithArgs("run1", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	p := &Pipeline{Store: s}
	_, _, err := p.RunStage(context.Background(), tOrg, tArt, StageResearch, "h1", func(context.Context) (any, Usage, error) {
		return nil, Usage{}, ErrBudgetExceeded
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ── approval binds to the revision hash ──

func reviewHead(status, hash string, checks string, consequential bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"status", "rid", "hash", "checks", "cons"}).AddRow(status, "rev1", hash, []byte(checks), consequential)
}

func TestSubmitReview_HashMismatchIsRejectedAndNothingIsWritten(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(q("FOR UPDATE OF a")).WithArgs(tArt, tOrg).WillReturnRows(reviewHead("in_review", strings.Repeat("b", 64), `{}`, false))
	mock.ExpectRollback()
	_, _, err := s.SubmitReview(context.Background(), tOrg, tArt, ReviewInput{RevisionHash: strings.Repeat("a", 64),
		Reviewer: "ann", Role: "primary", Decision: "approve"})
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err) // no INSERT content_reviews, no status UPDATE
	}
}

func TestSubmitReview_ApproveOnMatchingHash(t *testing.T) {
	s, mock := newMock(t)
	h := strings.Repeat("c", 64)
	mock.ExpectBegin()
	mock.ExpectQuery(q("FOR UPDATE OF a")).WillReturnRows(reviewHead("in_review", h, `{"code":[{"name":"schema","passed":true,"severity":"S1"}],"judgment":[]}`, false))
	mock.ExpectQuery(q("SELECT reviewer, role, decision, findings FROM content_reviews")).WithArgs("rev1").
		WillReturnRows(sqlmock.NewRows([]string{"reviewer", "role", "decision", "findings"}))
	mock.ExpectQuery(q("INSERT INTO content_reviews")).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow("rv1", time.Now()))
	mock.ExpectExec(q("UPDATE content_articles SET status = $1")).WithArgs(StatusApproved, tArt, tOrg).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	_, out, err := s.SubmitReview(context.Background(), tOrg, tArt, ReviewInput{RevisionHash: h, Reviewer: "ann", Role: "primary", Decision: "approve"})
	if err != nil || out.Status != StatusApproved {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitReview_BlockedApproveWritesNothing(t *testing.T) {
	s, mock := newMock(t)
	h := strings.Repeat("c", 64)
	mock.ExpectBegin()
	mock.ExpectQuery(q("FOR UPDATE OF a")).WillReturnRows(reviewHead("in_review", h,
		`{"code":[],"judgment":[{"id":"j1","kind":"claim","verdict":"unsupported","block_id":"b1","sentence_idx":0}]}`, false))
	mock.ExpectQuery(q("SELECT reviewer, role, decision, findings FROM content_reviews")).
		WillReturnRows(sqlmock.NewRows([]string{"reviewer", "role", "decision", "findings"}))
	mock.ExpectRollback()
	_, out, err := s.SubmitReview(context.Background(), tOrg, tArt, ReviewInput{RevisionHash: h, Reviewer: "ann", Role: "primary", Decision: "approve"})
	if !errors.Is(err, ErrApprovalBlocked) || len(out.Blockers) != 1 {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ── a claim version bump invalidates approval and harvestability ──

func TestNewClaimVersion_InvalidatesApprovalAndHarvestability(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(q("SELECT version, claim_key, question")).WithArgs(tOrg, tClaim).
		WillReturnRows(sqlmock.NewRows([]string{"version", "claim_key", "question", "origin"}).AddRow(1, "ira_limit", "What is the limit?", tArt))
	mock.ExpectExec(q("INSERT INTO content_claims")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(q("UPDATE content_articles a SET")).WithArgs(tOrg, tClaim, 2).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("approved-article").AddRow("live-article"))
	mock.ExpectCommit()
	c, flagged, err := s.NewClaimVersion(context.Background(), tOrg, tClaim, Claim{Text: "The limit is $7,500.", Type: ClaimSourcedFact, Status: ClaimSupported})
	if err != nil || c.Version != 2 || len(flagged) != 2 || c.ClaimKey != "ira_limit" {
		t.Fatalf("err=%v c=%+v flagged=%v", err, c, flagged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// Pin WHAT the invalidation does, since sqlmock cannot run it.
	for _, want := range []string{"'changes_requested'", "harvestable = FALSE", "'approved'", "'live_confirmed'", "'publishing'",
		"(e->>'version')::int < $3", "r.id = a.current_revision_id"} {
		if !strings.Contains(flagStaleArticlesSQL, want) {
			t.Errorf("invalidation SQL lost %q", want)
		}
	}
}

// ── the manifest-drop guard ──

func releasePrelude(mock sqlmock.Sqlmock, prevManifest string, withdrawn ...string) {
	mock.ExpectBegin()
	mock.ExpectExec(q("pg_advisory_xact_lock")).WithArgs("content_release:" + tSite).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(q("SELECT COUNT(*) FROM content_releases")).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(q("SELECT a.id, r.id, r.revision_hash, a.slug")).WithArgs(tOrg, tSite).
		WillReturnRows(sqlmock.NewRows([]string{"a", "r", "h", "s"}).AddRow("A", "rA", "hA", "a"))
	mock.ExpectQuery(q("SELECT manifest FROM content_releases")).WillReturnRows(sqlmock.NewRows([]string{"m"}).AddRow([]byte(prevManifest)))
	rows := sqlmock.NewRows([]string{"id"})
	for _, w := range withdrawn {
		rows.AddRow(w)
	}
	mock.ExpectQuery(q("status = 'withdrawn'")).WillReturnRows(rows)
}

func TestCreateRelease_RefusesToDropALiveArticle(t *testing.T) {
	s, mock := newMock(t)
	releasePrelude(mock, `[{"article_id":"A","slug":"a"},{"article_id":"B","slug":"b"}]`)
	mock.ExpectRollback()
	_, err := s.CreateRelease(context.Background(), tOrg, tSite)
	if !errors.Is(err, ErrManifestDrop) || !strings.Contains(err.Error(), "B") {
		t.Fatalf("want ErrManifestDrop naming B, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err) // no INSERT content_releases
	}
}

func TestCreateRelease_WithdrawnArticleMayDrop(t *testing.T) {
	s, mock := newMock(t)
	releasePrelude(mock, `[{"article_id":"A","slug":"a"},{"article_id":"B","slug":"b"}]`, "B")
	mock.ExpectQuery(q("INSERT INTO content_releases")).WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).AddRow("rel1", time.Now()))
	mock.ExpectExec(q("SET status = 'publishing'")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	rel, err := s.CreateRelease(context.Background(), tOrg, tSite)
	if err != nil || rel.ID != "rel1" || len(rel.Manifest) != 1 || rel.ManifestHash == "" {
		t.Fatalf("err=%v rel=%+v", err, rel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetReleaseStatus_LiveConfirmedHarvestsOnlyManifestRevisions(t *testing.T) {
	s, mock := newMock(t)
	man, _ := json.Marshal([]ManifestEntry{{ArticleID: tArt, RevisionID: "55555555-5555-5555-5555-555555555555", RevisionHash: "h", Slug: "a"}})
	mock.ExpectBegin()
	mock.ExpectQuery(q("FROM content_releases WHERE id = $1 AND org_id = $2 FOR UPDATE")).
		WillReturnRows(sqlmock.NewRows([]string{"site", "status", "manifest", "hash", "started"}).AddRow(tSite, ReleaseDeployed, man, "mh", time.Now()))
	mock.ExpectQuery(q("UPDATE content_releases SET status = $1")).WillReturnRows(sqlmock.NewRows([]string{"f"}).AddRow(time.Now()))
	mock.ExpectExec(q("a.current_revision_id = m.revision_id")).WithArgs(tOrg, sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if _, err := s.SetReleaseStatus(context.Background(), tOrg, "66666666-6666-6666-6666-666666666666", ReleaseLiveConfirmed, ""); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertOpsReport_OlderReportNeverOverwrites(t *testing.T) {
	s, mock := newMock(t)
	mock.ExpectQuery(q("WHERE content_ops_reports.generated_at <= EXCLUDED.generated_at")).
		WillReturnRows(sqlmock.NewRows([]string{"received_at"})) // conflict WHERE false → no row
	r, err := s.UpsertOpsReport(context.Background(), tOrg, OpsReport{Source: "site_probes", GeneratedAt: time.Now().Add(-time.Hour)})
	if err != nil || !r.ReceivedAt.IsZero() {
		t.Fatalf("an out-of-order report is a no-op, not an error: %v %+v", err, r)
	}
}
