package api

// dashboard_console_deferring_isps.go — Dashboard Console: deferring-ISPs card.
//
// GET /api/mailing/dashboard/deferring-isps?hours=24
//
// Read-only. Returns, for the requested lookback window, each ISP's total
// deferral count plus its top-3 deferral reason codes (dsn_status) with one
// representative DSN diagnostic sample per reason.
//
// Source of truth: pmta_acct_raw, the raw PMTA accounting stream. This reuses
// the exact deferral definition and ISP derivation proven in
// handlers_deliverability.go (runDrilldown / HandleGetDeferrals):
//   - deferral rows  = deferredFilterSQL (record_type IN ('t','tq'))
//   - ISP dimension  = ispCoalesceSQL   (COALESCE(NULLIF(recipient_isp,''),'other'))
// so this card agrees number-for-number with the ISP Health Center's
// /deliverability/deferrals drill-down for the same window (METRIC_CONTRACT §5:
// the deferral funnel keys on the raw recipient_isp today; migration pending).
//
// NOTE ON pmta_acct_raw ONLY: this is raw PMTA accounting, not the Athena lake.
// The lake (METRIC_CONTRACT §1) is authoritative for blended delivery outcomes;
// pmta_acct_raw is the correct, richer source for PMTA transient/DSN drill-down
// (it carries dsn_status + dsn_diag, which the lake does not surface per-code).
// This card is a PMTA-server operational view, not a cross-transport delivery
// metric — SES deferrals are not represented here.

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// VersionDashboardDeferringISPs tracks this endpoint's response contract.
//
// 1.0 (2026-07-02): initial — per-ISP deferral counts with top-3 reason codes
// over pmta_acct_raw, reusing deferredFilterSQL + ispCoalesceSQL.
const VersionDashboardDeferringISPs = "1.0"

type deferReason struct {
	Code   string `json:"code"`   // dsn_status, e.g. "4.7.1"
	Count  int    `json:"count"`  // deferral rows for this ISP+code in the window
	Sample string `json:"sample"` // one representative dsn_diag string (truncated)
}

type deferISPRow struct {
	ISP      string        `json:"isp"`
	Deferred int           `json:"deferred"`
	Reasons  []deferReason `json:"reasons"`
}

// HandleDashboardDeferringISPs serves GET /api/mailing/dashboard/deferring-isps.
func (s *Server) HandleDashboardDeferringISPs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 168 {
			hours = n
		}
	}
	// Pass the window as a Postgres interval literal (e.g. "24 hours").
	interval := strconv.Itoa(hours) + " hours"

	// Group by ISP × dsn_status once; roll up + cap to top-3 reasons in Go.
	// (isp,code) combos are small — a handful of ISPs × a dozen DSN codes.
	query := `
		SELECT
			` + ispCoalesceSQL + ` AS isp,
			COALESCE(NULLIF(dsn_status, ''), 'unspecified') AS code,
			COUNT(*) AS cnt,
			COALESCE(MAX(NULLIF(dsn_diag, '')), '') AS sample
		FROM pmta_acct_raw
		WHERE received_at > NOW() - $1::interval
		  AND ` + deferredFilterSQL + `
		GROUP BY 1, 2
		ORDER BY cnt DESC`

	rows, err := s.mailingDB.QueryContext(ctx, query, interval)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	defer rows.Close()

	byISP := map[string]*deferISPRow{}
	for rows.Next() {
		var isp, code, sample string
		var cnt int
		if err := rows.Scan(&isp, &code, &cnt, &sample); err != nil {
			continue
		}
		if len(sample) > 240 {
			sample = sample[:237] + "..."
		}
		row, ok := byISP[isp]
		if !ok {
			row = &deferISPRow{ISP: isp, Reasons: []deferReason{}}
			byISP[isp] = row
		}
		row.Deferred += cnt
		row.Reasons = append(row.Reasons, deferReason{Code: code, Count: cnt, Sample: sample})
	}

	out := make([]deferISPRow, 0, len(byISP))
	for _, row := range byISP {
		// Reasons come out of the query in global count-desc order; re-sort
		// per-ISP to be safe, then cap to the top 3.
		sort.SliceStable(row.Reasons, func(i, j int) bool {
			return row.Reasons[i].Count > row.Reasons[j].Count
		})
		if len(row.Reasons) > 3 {
			row.Reasons = row.Reasons[:3]
		}
		out = append(out, *row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Deferred > out[j].Deferred })

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"hours":        hours,
		"rows":         out,
		"api_version":  VersionDashboardDeferringISPs,
		"generated_at": time.Now().UTC(),
	})
}
