package api

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// MetricsFilter controls what events are included in ComputeMetrics.
type MetricsFilter struct {
	OrgID         string
	CampaignID    string
	SendingDomain string
	ISP           string // ISP group name (e.g. "gmail"); requires subscriber join
	StartDate     time.Time
	EndDate       time.Time
	ExcludeMPP    bool // exclude opens with is_machine_open=true
}

// MetricsResult holds counts and pre-computed rates from mailing_tracking_events.
// Rate conventions:
//   - OpenRate, ClickRate: denominator = Delivered
//   - BounceRates, ComplaintRate, DeferralRate, DeliveryRate, UnsubRate: denominator = Sent
type MetricsResult struct {
	Sent         int `json:"sent"`
	Delivered    int `json:"delivered"`
	Opens        int `json:"opens"`
	Clicks       int `json:"clicks"`
	HardBounces  int `json:"hard_bounces"`
	SoftBounces  int `json:"soft_bounces"`
	Complaints   int `json:"complaints"`
	Unsubscribes int `json:"unsubscribes"`
	Deferred     int `json:"deferred"`

	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	HardBounceRate float64 `json:"hard_bounce_rate"`
	SoftBounceRate float64 `json:"soft_bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	DeliveryRate   float64 `json:"delivery_rate"`
	UnsubRate      float64 `json:"unsubscribe_rate"`
	DeferralRate   float64 `json:"deferral_rate"`
}

// ISPMetricsResult pairs an ISP group with its computed metrics.
type ISPMetricsResult struct {
	ISP         string        `json:"isp"`
	DisplayName string        `json:"display_name"`
	Metrics     MetricsResult `json:"metrics"`
}

// ComputeMetrics returns consistent, correctly-denominated metrics.
//
// Delivery metrics (delivered, hard/soft bounces, complaints, deferrals) are
// sourced from pmta_acct_daily_summary when a CampaignID filter is provided,
// making PMTA accounting the authoritative source. Engagement metrics (sent,
// opens, clicks, unsubs) always come from mailing_tracking_events.
//
// When no CampaignID is set (org-wide / date-range queries), all metrics fall
// back to mailing_tracking_events for backward compatibility.
func ComputeMetrics(ctx context.Context, db *sql.DB, f MetricsFilter) (MetricsResult, error) {
	var r MetricsResult

	if f.CampaignID != "" {
		// PMTA-authoritative path: delivery metrics from summary table
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(delivered), 0), COALESCE(SUM(hard_bounced), 0),
			       COALESCE(SUM(soft_bounced), 0), COALESCE(SUM(complained), 0),
			       COALESCE(SUM(deferred), 0)
			FROM pmta_acct_daily_summary
			WHERE campaign_id = $1::uuid
		`, f.CampaignID).Scan(&r.Delivered, &r.HardBounces, &r.SoftBounces, &r.Complaints, &r.Deferred)
		if err != nil {
			return r, fmt.Errorf("ComputeMetrics(summary): %w", err)
		}

		// Engagement metrics from tracking events
		mppClause := mppOpenClause(f.ExcludeMPP)
		engQuery := fmt.Sprintf(`
			SELECT
				COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'opened' %s THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'unsubscribed' THEN 1 ELSE 0 END), 0)
			FROM mailing_tracking_events t
			WHERE t.campaign_id = $1::uuid
		`, mppClause)
		err = db.QueryRowContext(ctx, engQuery, f.CampaignID).Scan(
			&r.Sent, &r.Opens, &r.Clicks, &r.Unsubscribes,
		)
		if err != nil {
			return r, fmt.Errorf("ComputeMetrics(engagement): %w", err)
		}

		// If summary has delivery data but tracking has no sent events yet,
		// infer sent from total summary records to avoid 0-sent with N-delivered.
		if r.Sent == 0 && r.Delivered > 0 {
			r.Sent = r.Delivered + r.HardBounces + r.SoftBounces
		}

		r.computeRates()
		return r, nil
	}

	// Fallback: org-wide or date-range queries use tracking events for everything
	where, args := buildMetricsWhere(f, false)
	mppClause := mppOpenClause(f.ExcludeMPP)

	query := fmt.Sprintf(`
		SELECT
			COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'opened' %s THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'unsubscribed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN t.event_type = 'deferred' THEN 1 ELSE 0 END), 0)
		FROM mailing_tracking_events t
		%s
		WHERE %s
	`, mppClause, hardBounceSQL, hardBounceSQL, subscriberJoin(f), strings.Join(where, " AND "))

	err := db.QueryRowContext(ctx, query, args...).Scan(
		&r.Sent, &r.Delivered, &r.Opens, &r.Clicks,
		&r.HardBounces, &r.SoftBounces, &r.Complaints, &r.Unsubscribes, &r.Deferred,
	)
	if err != nil {
		return r, fmt.Errorf("ComputeMetrics: %w", err)
	}

	r.computeRates()
	return r, nil
}

// ComputeMetricsByISP returns per-ISP breakdowns.
//
// When CampaignID is provided, delivery metrics come from pmta_acct_daily_summary
// (already keyed by recipient_isp — no expensive subscriber join needed).
// Engagement metrics come from mailing_tracking_events.
func ComputeMetricsByISP(ctx context.Context, db *sql.DB, f MetricsFilter) ([]ISPMetricsResult, error) {
	ispMap := make(map[string]*MetricsResult)

	if f.CampaignID != "" {
		// Delivery metrics from PMTA summary (already grouped by ISP)
		dRows, err := db.QueryContext(ctx, `
			SELECT recipient_isp, COALESCE(SUM(delivered),0), COALESCE(SUM(hard_bounced),0),
			       COALESCE(SUM(soft_bounced),0), COALESCE(SUM(complained),0), COALESCE(SUM(deferred),0)
			FROM pmta_acct_daily_summary
			WHERE campaign_id = $1::uuid
			GROUP BY recipient_isp
		`, f.CampaignID)
		if err != nil {
			return nil, fmt.Errorf("ComputeMetricsByISP(summary): %w", err)
		}
		defer dRows.Close()

		for dRows.Next() {
			var ispGroup string
			var m MetricsResult
			if err := dRows.Scan(&ispGroup, &m.Delivered, &m.HardBounces, &m.SoftBounces, &m.Complaints, &m.Deferred); err != nil {
				continue
			}
			ispMap[ispGroup] = &m
		}
		dRows.Close()

		// Engagement metrics from tracking events, grouped by recipient domain
		mppClause := mppOpenClause(f.ExcludeMPP)
		engQuery := fmt.Sprintf(`
			SELECT COALESCE(LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))), '') AS domain,
				COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'opened' %s THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'unsubscribed' THEN 1 ELSE 0 END), 0)
			FROM mailing_tracking_events t
			LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
			WHERE t.campaign_id = $1::uuid
			GROUP BY domain
		`, mppClause)
		eRows, err := db.QueryContext(ctx, engQuery, f.CampaignID)
		if err != nil {
			return nil, fmt.Errorf("ComputeMetricsByISP(engagement): %w", err)
		}
		defer eRows.Close()

		for eRows.Next() {
			var domain string
			var sent, opens, clicks, unsubs int
			if err := eRows.Scan(&domain, &sent, &opens, &clicks, &unsubs); err != nil {
				continue
			}
			group := isp.GroupFromDomain(domain)
			if existing, ok := ispMap[group]; ok {
				existing.Sent += sent
				existing.Opens += opens
				existing.Clicks += clicks
				existing.Unsubscribes += unsubs
			} else {
				ispMap[group] = &MetricsResult{Sent: sent, Opens: opens, Clicks: clicks, Unsubscribes: unsubs}
			}
		}
	} else {
		// Fallback: no campaign filter — use tracking events for everything
		where, args := buildMetricsWhere(f, true)
		mppClause := mppOpenClause(f.ExcludeMPP)

		query := fmt.Sprintf(`
			SELECT
				COALESCE(LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))), '') AS domain,
				COALESCE(SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'opened' %s THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND %s THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND NOT (%s) THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'unsubscribed' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN t.event_type = 'deferred' THEN 1 ELSE 0 END), 0)
			FROM mailing_tracking_events t
			LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
			WHERE %s
			GROUP BY domain
		`, mppClause, hardBounceSQL, hardBounceSQL, strings.Join(where, " AND "))

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("ComputeMetricsByISP: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var domain string
			var m MetricsResult
			if err := rows.Scan(&domain, &m.Sent, &m.Delivered, &m.Opens, &m.Clicks,
				&m.HardBounces, &m.SoftBounces, &m.Complaints, &m.Unsubscribes, &m.Deferred); err != nil {
				continue
			}

			group := isp.GroupFromDomain(domain)
			if existing, ok := ispMap[group]; ok {
				existing.Sent += m.Sent
				existing.Delivered += m.Delivered
				existing.Opens += m.Opens
				existing.Clicks += m.Clicks
				existing.HardBounces += m.HardBounces
				existing.SoftBounces += m.SoftBounces
				existing.Complaints += m.Complaints
				existing.Unsubscribes += m.Unsubscribes
				existing.Deferred += m.Deferred
			} else {
				ispMap[group] = &m
			}
		}
	}

	// Infer sent from delivery totals when tracking events lack sent records
	for _, m := range ispMap {
		if m.Sent == 0 && m.Delivered > 0 {
			m.Sent = m.Delivered + m.HardBounces + m.SoftBounces
		}
	}

	var results []ISPMetricsResult
	for _, g := range isp.KnownGroups() {
		if m, ok := ispMap[g]; ok {
			m.computeRates()
			results = append(results, ISPMetricsResult{
				ISP:         g,
				DisplayName: metricsISPDisplayName(g),
				Metrics:     *m,
			})
		}
	}
	if m, ok := ispMap[isp.Other]; ok {
		m.computeRates()
		results = append(results, ISPMetricsResult{
			ISP:         isp.Other,
			DisplayName: "Other",
			Metrics:     *m,
		})
	}

	return results, nil
}

// ComputeMetricsTotals sums an ISP breakdown into a single aggregate result.
func ComputeMetricsTotals(byISP []ISPMetricsResult) MetricsResult {
	var t MetricsResult
	for _, r := range byISP {
		t.Sent += r.Metrics.Sent
		t.Delivered += r.Metrics.Delivered
		t.Opens += r.Metrics.Opens
		t.Clicks += r.Metrics.Clicks
		t.HardBounces += r.Metrics.HardBounces
		t.SoftBounces += r.Metrics.SoftBounces
		t.Complaints += r.Metrics.Complaints
		t.Unsubscribes += r.Metrics.Unsubscribes
		t.Deferred += r.Metrics.Deferred
	}
	t.computeRates()
	return t
}

func (r *MetricsResult) computeRates() {
	if r.Delivered > 0 {
		d := float64(r.Delivered)
		r.OpenRate = metricsRound2(float64(r.Opens) / d * 100)
		r.ClickRate = metricsRound2(float64(r.Clicks) / d * 100)
	}
	if r.Sent > 0 {
		s := float64(r.Sent)
		r.HardBounceRate = metricsRound2(float64(r.HardBounces) / s * 100)
		r.SoftBounceRate = metricsRound2(float64(r.SoftBounces) / s * 100)
		r.ComplaintRate = metricsRound3(float64(r.Complaints) / s * 100)
		r.DeliveryRate = metricsRound2(float64(r.Delivered) / s * 100)
		r.UnsubRate = metricsRound2(float64(r.Unsubscribes) / s * 100)
		r.DeferralRate = metricsRound2(float64(r.Deferred) / s * 100)
	}
}

// HardBounceCategories is the canonical list of bounce_type values that
// indicate a permanent delivery failure. Used by metrics.go, engine/ingest.go,
// and all analytics handlers. Any change here propagates everywhere.
var HardBounceCategories = []string{
	"hard", "bad-mailbox", "bad-domain", "inactive-mailbox",
	"no-answer-from-host", "routing-errors", "policy-related", "bad-connection",
}

// IsHardBounceCategory returns true if the given PMTA bounce category
// represents a permanent (hard) bounce.
func IsHardBounceCategory(cat string) bool {
	for _, c := range HardBounceCategories {
		if c == cat {
			return true
		}
	}
	return false
}

// hardBounceSQL matches both the send_worker format ("hard") and the PMTA
// ingest format (raw PMTA categories). This ensures ComputeMetrics correctly
// classifies bounces regardless of which write path created the record.
var hardBounceSQL = `COALESCE(t.bounce_type,'') IN ('hard','bad-mailbox','bad-domain','inactive-mailbox','no-answer-from-host','routing-errors','policy-related','bad-connection')`

// HardBounceSQL returns the canonical SQL fragment for identifying hard bounces
// in mailing_tracking_events. Use this instead of hardcoding bounce_type lists.
// The alias parameter is the table alias (e.g. "t", "d", "te").
func HardBounceSQL(alias string) string {
	return `COALESCE(` + alias + `.bounce_type,'') IN ('hard','bad-mailbox','bad-domain','inactive-mailbox','no-answer-from-host','routing-errors','policy-related','bad-connection')`
}

// ── Query building helpers ───────────────────────────────────────────────────

func buildMetricsWhere(f MetricsFilter, alwaysJoinSubscriber bool) ([]string, []interface{}) {
	where := []string{"1=1"}
	var args []interface{}
	argN := 0

	next := func() string {
		argN++
		return fmt.Sprintf("$%d", argN)
	}

	if f.OrgID != "" {
		where = append(where, "t.organization_id = "+next()+"::uuid")
		args = append(args, f.OrgID)
	}
	if f.CampaignID != "" {
		where = append(where, "t.campaign_id = "+next()+"::uuid")
		args = append(args, f.CampaignID)
	}
	if f.SendingDomain != "" {
		where = append(where, "t.sending_domain = "+next())
		args = append(args, f.SendingDomain)
	}
	if f.ISP != "" {
		where = append(where, "LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2))) = ANY("+next()+"::text[])")
		args = append(args, ispDomainsForGroup(f.ISP))
	}
	if !f.StartDate.IsZero() {
		where = append(where, "t.event_at >= "+next())
		args = append(args, f.StartDate)
	}
	if !f.EndDate.IsZero() {
		where = append(where, "t.event_at <= "+next())
		args = append(args, f.EndDate)
	}

	return where, args
}

func subscriberJoin(f MetricsFilter) string {
	if f.ISP != "" {
		return "LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id"
	}
	return ""
}

func mppOpenClause(excludeMPP bool) string {
	if excludeMPP {
		return "AND COALESCE(t.is_machine_open, FALSE) = FALSE"
	}
	return ""
}

// ispDomainsForGroup returns the list of email domains that belong to a given
// ISP group. Used to build SQL ANY() filters when the MetricsFilter.ISP is set.
//
// Must stay in sync with internal/pkg/isp/isp.go domainToISP map.
func ispDomainsForGroup(group string) []string {
	domainMap := map[string][]string{
		"gmail":     {"gmail.com", "googlemail.com"},
		"yahoo":     {"yahoo.com", "ymail.com", "rocketmail.com", "yahoo.co.uk", "yahoo.ca", "yahoo.co.jp"},
		"aol":       {"aol.com", "aim.com"},
		"microsoft": {"outlook.com", "hotmail.com", "live.com", "msn.com"},
		"apple":     {"icloud.com", "me.com", "mac.com"},
		"comcast":   {"comcast.net", "xfinity.com"},
		"charter":   {"charter.net", "spectrum.net"},
		"att":       {"att.net"},
		"sbcglobal": {"sbcglobal.net", "bellsouth.net"},
		"cox":       {"cox.net"},
	}
	if domains, ok := domainMap[strings.ToLower(group)]; ok {
		return domains
	}
	return []string{}
}

// metricsISPDisplayNames maps ISP group keys to human-readable labels shown
// in API responses and consumed by the frontend analytics dashboard.
var metricsISPDisplayNames = map[string]string{
	"gmail":     "Gmail",
	"yahoo":     "Yahoo",
	"aol":       "AOL",
	"microsoft": "Microsoft / Outlook",
	"apple":     "Apple / iCloud",
	"comcast":   "Comcast / Xfinity",
	"charter":   "Charter / Spectrum",
	"att":       "AT&T",
	"sbcglobal": "SBC Global / BellSouth",
	"cox":       "Cox",
	"other":     "Other",
}

func metricsISPDisplayName(group string) string {
	if name, ok := metricsISPDisplayNames[group]; ok {
		return name
	}
	return group
}

// ── Rounding helpers (2 and 3 decimal places) ────────────────────────────────

func metricsRound2(v float64) float64 {
	return math.Round(v*100) / 100
}

func metricsRound3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
