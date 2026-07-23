package worker

import (
	"context"
	"testing"

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
