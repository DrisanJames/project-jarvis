package api

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// TestDedupSegmentIDs_DropsBadInput verifies the input scrubber drops empty
// strings, duplicates, and malformed UUIDs while preserving input order.
// This matters because the helper feeds straight into a parameterized SQL
// UPDATE — an unfiltered slice would either trigger Postgres `invalid input
// syntax for type uuid` errors or, worse, send a wide UPDATE on real ids
// alongside garbage that the driver coerces.
func TestDedupSegmentIDs_DropsBadInput(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"single valid", []string{"db4aee45-0979-40da-b4b8-fda3631247f6"}, []string{"db4aee45-0979-40da-b4b8-fda3631247f6"}},
		{
			"dedup preserves first-seen order",
			[]string{
				"db4aee45-0979-40da-b4b8-fda3631247f6",
				"cb1090b5-84e6-44cf-836f-de63857e0b86",
				"db4aee45-0979-40da-b4b8-fda3631247f6",
			},
			[]string{
				"db4aee45-0979-40da-b4b8-fda3631247f6",
				"cb1090b5-84e6-44cf-836f-de63857e0b86",
			},
		},
		{
			"drops empties and whitespace",
			[]string{"", "   ", "db4aee45-0979-40da-b4b8-fda3631247f6"},
			[]string{"db4aee45-0979-40da-b4b8-fda3631247f6"},
		},
		{
			"drops malformed shapes",
			[]string{
				"not-a-uuid",
				"db4aee45-0979-40da-b4b8-fda3631247f6XX", // too long
				"db4aee45-0979-40da-b4b8-fda3631247f",    // too short
				"db4aee45z0979-40da-b4b8-fda3631247f6",   // bad separator
				"db4aee45-0979-40da-b4b8-fda3631247gg",   // non-hex chars
				"DB4AEE45-0979-40DA-B4B8-FDA3631247F6",   // upper-case still valid
			},
			[]string{"DB4AEE45-0979-40DA-B4B8-FDA3631247F6"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupSegmentIDs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got=%v want=%v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got=%q want=%q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCollectSegmentIDsForUsage_AllSources verifies every campaign-input
// surface that references a segment for audience purposes is captured by
// the collector. Lists (type=="list") MUST NOT be captured because list
// usage tracking lives on `mailing_lists`, not `mailing_segments`.
func TestCollectSegmentIDsForUsage_AllSources(t *testing.T) {
	input := engine.PMTACampaignInput{
		InclusionSegments: []string{
			"db4aee45-0979-40da-b4b8-fda3631247f6", // DB 7D Openers
			"cb1090b5-84e6-44cf-836f-de63857e0b86", // DB 30D Clickers
		},
		ExclusionSegments: []string{
			"00000000-0000-0000-0000-000000007667", // NDR-CPM-Converters
		},
		SendPriority: []engine.PriorityItem{
			{ID: "abcdef00-0000-0000-0000-000000000001", Type: "segment"},
			{ID: "11111111-2222-3333-4444-555555555555", Type: "list"},          // ignored
			{ID: "abcdef00-0000-0000-0000-000000000002", Type: "Segment"},       // case-insensitive
			{ID: "", Type: "segment"},                                           // ignored
		},
	}
	got := collectSegmentIDsForUsage(input)

	want := []string{
		"db4aee45-0979-40da-b4b8-fda3631247f6",
		"cb1090b5-84e6-44cf-836f-de63857e0b86",
		"00000000-0000-0000-0000-000000007667",
		"abcdef00-0000-0000-0000-000000000001",
		"abcdef00-0000-0000-0000-000000000002",
	}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got=%v want=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got=%q want=%q", i, got[i], want[i])
		}
	}
}

// TestBumpSegmentUsage_HappyPath verifies the SQL update fires with the
// expected shape: bumps last_used_at + usage_count + updated_at AND clears
// cleanup_warning_sent / cleanup_warned_at so an actively-used segment
// can't get archived by SegmentCleanupWorker.
func TestBumpSegmentUsage_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_segments SET
			last_used_at         = NOW(),
			usage_count          = COALESCE(usage_count, 0) + 1,
			updated_at           = NOW(),
			cleanup_warning_sent = FALSE,
			cleanup_warned_at    = NULL
		WHERE id = ANY($1::uuid[])`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	bumpSegmentUsage(context.Background(), db,
		[]string{
			"db4aee45-0979-40da-b4b8-fda3631247f6",
			"cb1090b5-84e6-44cf-836f-de63857e0b86",
		},
		"campaign-id-test")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestBumpSegmentUsage_EmptyInputIsNoOp verifies the helper short-circuits
// before issuing any SQL when there are no valid segment IDs. Critical for
// the campaign-finalize hot path: campaigns with only list-based audiences
// (no segments referenced) MUST NOT touch the DB just to confirm there's
// nothing to bump.
func TestBumpSegmentUsage_EmptyInputIsNoOp(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
	}{
		{"nil", nil},
		{"empty slice", []string{}},
		{"only invalid", []string{"not-a-uuid", "", "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New() error = %v", err)
			}
			defer db.Close()

			// Zero ExpectExec calls. ExpectationsWereMet will pass only
			// if no SQL was issued.
			bumpSegmentUsage(context.Background(), db, tc.ids, "campaign-id-test")

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations not met (helper issued unexpected SQL): %v", err)
			}
		})
	}
}

// TestBumpSegmentUsage_DBErrorIsSwallowed verifies the helper logs but does
// NOT panic or return an error when the underlying UPDATE fails. The
// finalize critical path must continue regardless of bookkeeping failures.
func TestBumpSegmentUsage_DBErrorIsSwallowed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_segments SET`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errors.New("connection refused"))

	// Should not panic. Should not propagate. Should log internally.
	bumpSegmentUsage(context.Background(), db,
		[]string{"db4aee45-0979-40da-b4b8-fda3631247f6"}, "campaign-id-test")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}
