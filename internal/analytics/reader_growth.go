package analytics

// Growth rollup READ layer — the delivery half of mailing_growth_daily.
//
// One aggregate per (Denver day × sending-domain apex × ISP) over a date
// range, used by worker.GrowthRollupWorker to populate the growth fact table
// that backs the Reporting → Growth screen. This is the ONLY place the
// delivery numbers come from: the lake is delivery truth (METRIC_CONTRACT), PG
// counters are not.
//
// Every expression here is the file's existing canonical one — localDtExpr
// (Denver day), ispExpr (ISP from the REAL recipient address, not the polluted
// stored isp_group), brandExpr (stored brand, VMTA-code fallback), and
// eventTypeExpr (the read-time bounce taxonomy that keeps administrative
// flushes and reputation blocks OUT of hard/soft). Reusing them is what makes
// the Growth tab agree with the Overview/Dimensions tabs on the same screen.
//
// TRANSPORTS: source IN ('pmta','ses','kumo') — the real senders. source='app'
// is the PG→lake mirror and DOUBLE-COUNTS every delivery (2026-07-28: app
// 659,741 vs ses 655,636 for the same day); 'relayed_to_ses' is a PMTA→SES
// handoff, never a recipient delivery. Both are excluded.
//
// BRAND ATTRIBUTION HONESTY: the live emitters wrote brand='' before
// 2026-07-01 and SES rows carry no VMTA to fall back on, so days before
// 2026-07-02 resolve only partially (measured: 17%–92% by day; 100% from
// 2026-07-02 onward). Unresolvable rows land under brand '' and the API
// surfaces them as "(unattributed)" rather than silently dropping volume.
//
// READ ONLY. No Postgres, no writes.

import (
	"context"
	"strconv"
	"strings"
)

// GrowthDeliveryRow is one (day, brand, isp) delivery bucket.
type GrowthDeliveryRow struct {
	Day             string `json:"day"`   // YYYY-MM-DD, Denver
	Brand           string `json:"brand"` // apex domain; "" when unresolvable
	ISP             string `json:"isp"`
	Delivered       int64  `json:"delivered"`
	HardBounce      int64  `json:"hard_bounce"`
	SoftBounce      int64  `json:"soft_bounce"`
	ReputationBlock int64  `json:"reputation_block"`
	Complaints      int64  `json:"complaints"`
}

// growthSourceIn is the real-transport allowlist (see the file header).
const growthSourceIn = "('pmta','ses','kumo')"

// buildGrowthDeliverySQL renders the range aggregate. fromDt/toDt are Denver
// days already validated by the caller; they are rendered through sqlStr like
// every other literal in this package. The dt partition bound is widened ±1
// day because a Denver day straddles two UTC partitions (same rule as
// buildBreakdownSQL).
func buildGrowthDeliverySQL(fromDt, toDt string) string {
	et := eventTypeExpr
	cnt := func(pred string) string {
		return "COUNT(DISTINCT CASE WHEN " + pred + " THEN event_uid END)"
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(localDtExpr)
	b.WriteString(" d, ")
	b.WriteString(brandExpr)
	b.WriteString(" b, ")
	b.WriteString(ispExpr)
	b.WriteString(" isp, ")
	b.WriteString(cnt(et + " = 'delivered'"))
	b.WriteString(" delivered, ")
	b.WriteString(cnt(et + " = 'hard_bounce'"))
	b.WriteString(" hard, ")
	b.WriteString(cnt(et + " = 'soft_bounce'"))
	b.WriteString(" soft, ")
	b.WriteString(cnt(et + " = 'reputation_block'"))
	b.WriteString(" repblock, ")
	b.WriteString(cnt(et + " = 'complaint'"))
	b.WriteString(" complaints")
	b.WriteString(" FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE dt BETWEEN ")
	b.WriteString(sqlStr(shiftDt(fromDt, -1)))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(shiftDt(toDt, 1)))
	b.WriteString(" AND ")
	b.WriteString(localDtExpr)
	b.WriteString(" BETWEEN ")
	b.WriteString(sqlStr(fromDt))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(toDt))
	b.WriteString(" AND source IN ")
	b.WriteString(growthSourceIn)
	b.WriteString(" GROUP BY ")
	b.WriteString(localDtExpr)
	b.WriteString(", ")
	b.WriteString(brandExpr)
	b.WriteString(", ")
	b.WriteString(ispExpr)
	return b.String()
}

// GrowthDelivery returns the per-(day, brand, isp) delivery aggregate for the
// inclusive Denver-day range [fromDt, toDt].
func (r *Reader) GrowthDelivery(ctx context.Context, fromDt, toDt string) ([]GrowthDeliveryRow, error) {
	if err := validateDt("from", fromDt); err != nil {
		return nil, err
	}
	if err := validateDt("to", toDt); err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, buildGrowthDeliverySQL(fromDt, toDt))
	if err != nil {
		return nil, err
	}
	out := make([]GrowthDeliveryRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 8 {
			continue
		}
		n := func(i int) int64 { v, _ := strconv.ParseInt(row[i], 10, 64); return v }
		out = append(out, GrowthDeliveryRow{
			Day:             row[0],
			Brand:           strings.ToLower(strings.TrimSpace(row[1])),
			ISP:             row[2],
			Delivered:       n(3),
			HardBounce:      n(4),
			SoftBounce:      n(5),
			ReputationBlock: n(6),
			Complaints:      n(7),
		})
	}
	return out, nil
}

// GrowthDelivery runs against the global reader. Returns errDisabled when the
// reader is not configured (IsDisabledErr reports it).
func GrowthDelivery(ctx context.Context, fromDt, toDt string) ([]GrowthDeliveryRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.GrowthDelivery(ctx, fromDt, toDt)
}
