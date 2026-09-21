package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
)

// fakeEOValidator returns a canned response, mimicking what
// emailoversight.Client.Validate would hand back after its own decoding
// (including the ResultIDRejected stamp on REJECT shapes).
type fakeEOValidator struct {
	resp *emailoversight.ValidationResponse
	err  error
}

func (f *fakeEOValidator) Validate(_ context.Context, _ string) (*emailoversight.ValidationResponse, error) {
	return f.resp, f.err
}

func newTestValidator(resp *emailoversight.ValidationResponse) *PartnerValidator {
	return NewPartnerValidator(nil, &fakeEOValidator{resp: resp}, PartnerValidatorConfig{})
}

// TestCallEORejectShapeSuppresses: EO account-suppression REJECT responses
// ({"result":"REJECT","reason":"Suppressed Email Address"}) are a FINAL
// verdict — same class as Undeliverable. Before the fix their missing
// ResultId (0) hit the retry arm → 3 attempts → dead_letter.
func TestCallEORejectShapeSuppresses(t *testing.T) {
	pv := newTestValidator(&emailoversight.ValidationResponse{
		Email:    "x@example.com",
		Result:   "REJECT",
		Reason:   "Suppressed Email Address",
		ResultID: emailoversight.ResultIDRejected,
	})
	out := pv.callEO(context.Background(), pendingRecord{email: "x@example.com"})
	if out.kind != outcomeSuppress {
		t.Fatalf("kind = %d, want outcomeSuppress (%d)", out.kind, outcomeSuppress)
	}
	if out.result != "Suppressed Email Address" {
		t.Errorf("result = %q, want %q (recorded as eo_result)", out.result, "Suppressed Email Address")
	}
}

// TestCallEORejectShapeSuppressesEvenWithoutStampedID: classification must
// key off the reject SHAPE, not the synthetic id — a REJECT with a raw
// ResultID of 0 (e.g. from a caller that bypassed Client.Validate's stamp)
// must still suppress, never retry.
func TestCallEORejectShapeSuppressesEvenWithoutStampedID(t *testing.T) {
	pv := newTestValidator(&emailoversight.ValidationResponse{
		Email:  "x@example.com",
		Result: "REJECT",
		Reason: "Suppressed Email Address",
		// ResultID left 0 — the pre-fix trap value.
	})
	out := pv.callEO(context.Background(), pendingRecord{email: "x@example.com"})
	if out.kind != outcomeSuppress {
		t.Fatalf("kind = %d, want outcomeSuppress (%d)", out.kind, outcomeSuppress)
	}
}

// TestCallEONormalShapeRegression pins the pre-existing mapping:
// Verified/Complainer → ready; 0/11 → retry; other ids → suppress.
func TestCallEONormalShapeRegression(t *testing.T) {
	cases := []struct {
		name     string
		resp     emailoversight.ValidationResponse
		wantKind eoOutcomeKind
	}{
		{"verified_ready", emailoversight.ValidationResponse{ResultID: 1, Result: "Verified"}, outcomeReady},
		{"complainer_ready", emailoversight.ValidationResponse{ResultID: 7, Result: "Complainer"}, outcomeReady},
		{"retry_zero", emailoversight.ValidationResponse{ResultID: 0, Result: "Retry"}, outcomeRetry},
		{"unknown_retries", emailoversight.ValidationResponse{ResultID: 11, Result: "Unknown"}, outcomeRetry},
		{"undeliverable_suppresses", emailoversight.ValidationResponse{ResultID: 4, Result: "Undeliverable"}, outcomeSuppress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.resp
			pv := newTestValidator(&resp)
			out := pv.callEO(context.Background(), pendingRecord{email: "a@example.com"})
			if out.kind != tc.wantKind {
				t.Fatalf("kind = %d, want %d (result=%q)", out.kind, tc.wantKind, tc.resp.Result)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Data Ingest events (REQ 2026-09-20 U2)
// -----------------------------------------------------------------------------

// emailKeyedEO returns a different canned verdict per address, so one batch can
// carry all three outcome classes — which is the whole point of the assertion
// below (one event per OUTCOME, not one per row and not one per batch).
type emailKeyedEO struct {
	byEmail map[string]*emailoversight.ValidationResponse
}

func (f *emailKeyedEO) Validate(_ context.Context, email string) (*emailoversight.ValidationResponse, error) {
	if r, ok := f.byEmail[email]; ok {
		return r, nil
	}
	return nil, errors.New("no canned response")
}

// TestValidator_EmitsOneEventPerOutcomePerBatch pins the aggregation contract:
// a 500-row batch must produce a HANDFUL of events (one per dataset × verdict),
// never 500. The counter itself is dark in a unit test, so the assertion runs
// through dataingest.SetTestSink — the hook that exists precisely because Emit
// is a deliberate no-op with the bus unwired.
//
// It also pins the rule the Supply Ledger mirror already carries: a retry is
// NOT a verdict. Only the attempt that exhausts MaxRetries is verdict_dead.
func TestValidator_EmitsOneEventPerOutcomePerBatch(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_LEDGER_MIRROR_DISABLED", "1") // the ledger has its own tests
	dataingest.ResetDatasetClassCache()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	const dataset = "22222222-2222-2222-2222-222222222222"
	const partner = "33333333-3333-3333-3333-333333333333"

	pv := NewPartnerValidator(db, &emailKeyedEO{byEmail: map[string]*emailoversight.ValidationResponse{
		"ready1@gmail.com":  {ResultID: 1, Result: "Verified"},
		"ready2@yahoo.com":  {ResultID: 7, Result: "Complainer"},
		"bad@outlook.com":   {ResultID: 4, Result: "Undeliverable"},
		"dead@example.com":  {ResultID: 11, Result: "Unknown"}, // retry, and out of attempts
		"again@example.com": {ResultID: 11, Result: "Unknown"}, // retry, attempts remain
	}}, PartnerValidatorConfig{MaxRetries: 3})

	batch := []pendingRecord{
		{id: "1", email: "ready1@gmail.com", datasetID: dataset, partnerID: partner, vertical: "lane_a"},
		{id: "2", email: "ready2@yahoo.com", datasetID: dataset, partnerID: partner, vertical: "lane_a"},
		{id: "3", email: "bad@outlook.com", datasetID: dataset, partnerID: partner, vertical: "lane_a"},
		{id: "4", email: "dead@example.com", attempts: 2, datasetID: dataset, partnerID: partner, vertical: "lane_a"},
		{id: "5", email: "again@example.com", attempts: 0, datasetID: dataset, partnerID: partner, vertical: "lane_a"},
	}

	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = 'ready'`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = 'ready'`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = 'suppressed_eo'`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_global_suppressions`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = 'dead_letter'`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = 'pending_eo'`).WillReturnResult(sqlmock.NewResult(0, 1))
	// ONE dataset lookup for the whole batch — the 60s cache is what keeps this
	// off the database on a real tick.
	mock.ExpectQuery(`SELECT d.source_channel`).
		WithArgs(dataset).
		WillReturnRows(sqlmock.NewRows([]string{"source_channel", "last_batch_class"}).AddRow("api_feed", nil))

	var got []dataingest.Event
	var mu sync.Mutex
	restore := dataingest.SetTestSink(func(ev dataingest.Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	defer dataingest.SetTestSink(restore)

	pv.validateAndApply(context.Background(), batch)

	byTransition := map[string]dataingest.Event{}
	for _, ev := range got {
		if _, dup := byTransition[ev.Transition]; dup {
			t.Fatalf("two events for transition %q — the contract is ONE per (dataset, outcome): %+v", ev.Transition, got)
		}
		byTransition[ev.Transition] = ev
	}
	if len(got) != 3 {
		t.Fatalf("emitted %d events, want 3 (ready / suppressed / dead): %+v", len(got), got)
	}
	want := map[string]int64{
		dataingest.TransitionVerdictReady:      2,
		dataingest.TransitionVerdictSuppressed: 1,
		dataingest.TransitionVerdictDead:       1,
	}
	for transition, n := range want {
		ev, ok := byTransition[transition]
		if !ok {
			t.Fatalf("no %s event: %+v", transition, got)
		}
		if ev.N != n {
			t.Errorf("%s n = %d, want %d", transition, ev.N, n)
		}
		if ev.Source != dataingest.SourceValidator {
			t.Errorf("%s source_path = %q, want %q", transition, ev.Source, dataingest.SourceValidator)
		}
		if ev.DatasetID != dataset || ev.PartnerID != partner || ev.Lane != "lane_a" {
			t.Errorf("%s provenance = %s/%s/%s, want %s/%s/lane_a", transition, ev.DatasetID, ev.PartnerID, ev.Lane, dataset, partner)
		}
		if ev.Class != dataingest.ClassDynamic {
			t.Errorf("%s supply_class = %q, want dynamic (dataset source_channel=api_feed)", transition, ev.Class)
		}
		var sum int64
		for _, v := range ev.ByISP {
			sum += v
		}
		if sum != ev.N {
			t.Errorf("%s by_isp sums to %d but n=%d — the event would be DROPPED by Validate", transition, sum, ev.N)
		}
	}
	// by_isp comes from the EMAIL, never from isp_family text.
	if ready := byTransition[dataingest.TransitionVerdictReady]; ready.ByISP["gmail"] != 1 || ready.ByISP["yahoo"] != 1 {
		t.Errorf("verdict_ready by_isp = %v, want gmail:1 yahoo:1", ready.ByISP)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}
