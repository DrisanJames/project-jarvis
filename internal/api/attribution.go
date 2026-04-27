package api

// Click/conversion attribution matcher.
//
// Source of the problem: Everflow click + conversion exports do NOT carry our
// subscriber_id (sub1..sub5 were never threaded through the offer URL when
// these campaigns shipped). The only fields shared between the export and our
// own click-tracking events are IP address and timestamp. So attribution here
// is "find the mailing_tracking_events.clicked row from the same IP within a
// tight time window where the link points at the offer". When that fails we
// fall back to a wider IP-only search restricted to the same offer.
//
// This file is split into:
//   * Pure types and the matcher (this file) — testable with sqlmock
//   * CSV parsers (attribution_csv.go) — accept the exact column layout of
//     Everflow's "clicks" and "conversions" exports
//   * HTTP handler (attribution_handler.go) — multipart upload UI endpoint
//
// Keeping the matcher pure (db.QueryContext + plain rows in/out) means the
// same code path is exercised by the HTTP handler, the unit tests, and the
// cmd/attribution-match CLI used to generate the cross-check report.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"time"
)

// AttributionDefaultWindow is the symmetric time window used when looking up
// a click event in mailing_tracking_events from an Everflow click row.
// Everflow's redirect chain typically completes in well under 5s, but Outlook
// and other prefetchers can introduce delays. 2 minutes is the conservative
// middle ground: tight enough to avoid attributing two unrelated clicks from
// the same NAT pool to each other, loose enough to absorb prefetch latency.
const AttributionDefaultWindow = 2 * time.Minute

// AttributionFallbackWindow is the lookback used when the tight window finds
// nothing. We still constrain to the same offer link and same IP, so the only
// extra risk is attributing a later/earlier click on the same offer to this
// row. 14 days lines up with our standard engagement-window analytics.
const AttributionFallbackWindow = 14 * 24 * time.Hour

// ClickRow is the normalized representation of a single row from Everflow's
// "Clicks" CSV export. Only fields used by the matcher are retained; the
// dozens of device/geo columns are dropped on parse to keep the payload small.
type ClickRow struct {
	RowIndex      int       `json:"row_index"`
	Timestamp     time.Time `json:"timestamp"`
	OfferName     string    `json:"offer_name"`
	IPAddress     string    `json:"ip_address"`
	TransactionID string    `json:"transaction_id"`
	Browser       string    `json:"browser,omitempty"`
	Country       string    `json:"country,omitempty"`
	ISP           string    `json:"isp,omitempty"`
	ErrorCode     string    `json:"error_code,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
}

// ConversionRow is the normalized representation of one row from Everflow's
// "Conversions" CSV export. SessionUserIP is the user's IP at the moment the
// click was recorded by Everflow; ConversionUserIP is the advertiser's
// postback IP and is intentionally not used for attribution.
type ConversionRow struct {
	RowIndex         int       `json:"row_index"`
	ConversionID     string    `json:"conversion_id"`
	TransactionID    string    `json:"transaction_id"`
	ConversionTime   time.Time `json:"conversion_time"`
	ClickTime        time.Time `json:"click_time"`
	OfferName        string    `json:"offer_name"`
	Revenue          float64   `json:"revenue"`
	SessionUserIP    string    `json:"session_user_ip"`
	ConversionUserIP string    `json:"conversion_user_ip"`
	Country          string    `json:"country,omitempty"`
	ISP              string    `json:"isp,omitempty"`
}

// SubscriberProfile is the slice of mailing_subscribers we surface in the UI
// for matched rows. Kept narrow on purpose — the consumer of this attribution
// review is QA / ops, not a CRM, so we expose what's needed to identify and
// triage the user without dumping every column on the table.
type SubscriberProfile struct {
	SubscriberID  string     `json:"subscriber_id"`
	Email         string     `json:"email"`
	FirstName     string     `json:"first_name,omitempty"`
	LastName      string     `json:"last_name,omitempty"`
	Status        string     `json:"status,omitempty"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	LastEngagedAt *time.Time `json:"last_engaged_at,omitempty"`
}

// MatchedClick pairs an input click row with the tracking event we resolved
// it to plus the subscriber profile. ConfidenceTier is "tight" when we found
// the event inside the strict window, "fallback" when only the wide IP-only
// lookup succeeded.
type MatchedClick struct {
	Row            ClickRow          `json:"row"`
	CampaignID     string            `json:"campaign_id"`
	CampaignName   string            `json:"campaign_name,omitempty"`
	LinkURL        string            `json:"link_url,omitempty"`
	EventAt        time.Time         `json:"event_at"`
	OffsetSeconds  int64             `json:"offset_seconds"`
	ConfidenceTier string            `json:"confidence_tier"`
	Subscriber     SubscriberProfile `json:"subscriber"`
}

// UnmatchedClick captures the reason a row could not be attributed so the UI
// can surface a histogram (bot scanners vs. unknown IPs vs. retention edge).
type UnmatchedClick struct {
	Row    ClickRow `json:"row"`
	Reason string   `json:"reason"`
	Detail string   `json:"detail,omitempty"`
}

// MatchedConversion / UnmatchedConversion mirror the click variants. We keep
// them as separate types instead of generic "Matched" so the JSON shape can
// surface conversion-specific fields (revenue, conversion_id) without
// awkward optional fields.
type MatchedConversion struct {
	Row            ConversionRow     `json:"row"`
	CampaignID     string            `json:"campaign_id"`
	CampaignName   string            `json:"campaign_name,omitempty"`
	LinkURL        string            `json:"link_url,omitempty"`
	EventAt        time.Time         `json:"event_at"`
	OffsetSeconds  int64             `json:"offset_seconds"`
	ConfidenceTier string            `json:"confidence_tier"`
	Subscriber     SubscriberProfile `json:"subscriber"`
}

type UnmatchedConversion struct {
	Row    ConversionRow `json:"row"`
	Reason string        `json:"reason"`
	Detail string        `json:"detail,omitempty"`
}

// AttributionResult is the single payload returned by both the HTTP endpoint
// and the CLI. The summary block lets the UI render counters without scanning
// arrays client-side.
type AttributionResult struct {
	GeneratedAt          time.Time             `json:"generated_at"`
	OrgID                string                `json:"org_id,omitempty"`
	WindowSeconds        int                   `json:"window_seconds"`
	FallbackWindowHours  int                   `json:"fallback_window_hours"`
	OfferLinkPattern     string                `json:"offer_link_pattern"`
	TotalClicks          int                   `json:"total_clicks"`
	TotalConversions     int                   `json:"total_conversions"`
	MatchedClicks        []MatchedClick        `json:"matched_clicks"`
	UnmatchedClicks      []UnmatchedClick      `json:"unmatched_clicks"`
	MatchedConversions   []MatchedConversion   `json:"matched_conversions"`
	UnmatchedConversions []UnmatchedConversion `json:"unmatched_conversions"`
	UnmatchedReasons     map[string]int        `json:"unmatched_reasons"`
}

// AttributionOptions tunes the matcher. Zero-value uses production defaults.
type AttributionOptions struct {
	Window           time.Duration
	FallbackWindow   time.Duration
	OfferLikePattern string // ILIKE pattern matched against link_url; e.g. "%trugreen%"
	OrgID            string // optional, scopes both joins to a single tenant
}

func (o AttributionOptions) resolved() AttributionOptions {
	if o.Window <= 0 {
		o.Window = AttributionDefaultWindow
	}
	if o.FallbackWindow <= 0 {
		o.FallbackWindow = AttributionFallbackWindow
	}
	if strings.TrimSpace(o.OfferLikePattern) == "" {
		o.OfferLikePattern = "%trugreen%"
	}
	return o
}

// Reason codes used in UnmatchedClick / UnmatchedConversion. Constants so the
// frontend can switch on them without string fragility.
const (
	ReasonInvalidIP        = "invalid_ip"
	ReasonNoEventInWindow  = "no_event_in_window"
	ReasonBotScannerIP     = "bot_scanner_ip"
	ReasonOutsideRetention = "outside_retention"
	ReasonSubscriberMissing = "subscriber_missing"
	ReasonDBError          = "db_error"
)

// botScannerCIDRs are well-known network-checker ranges that produce clicks
// no real subscriber ever made: Outlook Safe Links, Microsoft Defender, Apple
// Mail Privacy Protection, Barracuda, Mimecast. Maintained as a static list
// because (a) it changes rarely, (b) the alternative is querying an external
// service mid-attribution which would balloon latency. Add new ranges here
// as we discover them in unmatched buckets.
var botScannerCIDRs = []string{
	// Microsoft / Outlook / Defender
	"40.92.0.0/15", "40.107.0.0/16", "52.100.0.0/14", "104.47.0.0/17",
	"40.95.0.0/16", "13.107.6.152/31", "13.107.18.10/31", "13.107.140.6/32",
	"40.96.0.0/13", "20.0.0.0/8", // Azure broadly — Outlook / Bing scanners
	"13.64.0.0/11", "13.96.0.0/13",
	// Boydton VA Microsoft hosts seen in TruGreen exports
	"135.232.0.0/16",
	// Google / Gmail link prefetch
	"66.249.64.0/19", "64.233.160.0/19", "209.85.128.0/17",
	// Apple Mail Privacy Protection (iCloud Private Relay covers many ranges,
	// these are the most common in our logs)
	"17.0.0.0/8",
	// Cloudflare scanner / WARP (some click prefetch from email clients)
	"104.16.0.0/12", "172.64.0.0/13",
}

var botScannerNets = mustParseCIDRs(botScannerCIDRs)

func mustParseCIDRs(in []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(in))
	for _, c := range in {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// IsBotScannerIP reports whether the given IP literal lives in one of the
// known link-checker ranges. Exposed for tests and the CLI report.
func IsBotScannerIP(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	for _, n := range botScannerNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// MatchAttribution runs both click and conversion matching and assembles the
// final result. Pure function over (db, rows, options) so the same path is
// used by HTTP, CLI, and tests.
func MatchAttribution(
	ctx context.Context,
	db *sql.DB,
	clicks []ClickRow,
	conversions []ConversionRow,
	opts AttributionOptions,
) (AttributionResult, error) {
	opts = opts.resolved()

	res := AttributionResult{
		GeneratedAt:         time.Now().UTC(),
		OrgID:               opts.OrgID,
		WindowSeconds:       int(opts.Window.Seconds()),
		FallbackWindowHours: int(opts.FallbackWindow.Hours()),
		OfferLinkPattern:    opts.OfferLikePattern,
		TotalClicks:         len(clicks),
		TotalConversions:    len(conversions),
		UnmatchedReasons:    map[string]int{},
	}

	for _, row := range clicks {
		matched, unmatched, err := matchSingleClick(ctx, db, row, opts)
		if err != nil {
			res.UnmatchedClicks = append(res.UnmatchedClicks, UnmatchedClick{
				Row:    row,
				Reason: ReasonDBError,
				Detail: err.Error(),
			})
			res.UnmatchedReasons[ReasonDBError]++
			continue
		}
		if matched != nil {
			res.MatchedClicks = append(res.MatchedClicks, *matched)
			continue
		}
		res.UnmatchedClicks = append(res.UnmatchedClicks, *unmatched)
		res.UnmatchedReasons[unmatched.Reason]++
	}

	for _, row := range conversions {
		matched, unmatched, err := matchSingleConversion(ctx, db, row, opts)
		if err != nil {
			res.UnmatchedConversions = append(res.UnmatchedConversions, UnmatchedConversion{
				Row:    row,
				Reason: ReasonDBError,
				Detail: err.Error(),
			})
			res.UnmatchedReasons[ReasonDBError]++
			continue
		}
		if matched != nil {
			res.MatchedConversions = append(res.MatchedConversions, *matched)
			continue
		}
		res.UnmatchedConversions = append(res.UnmatchedConversions, *unmatched)
		res.UnmatchedReasons[unmatched.Reason]++
	}

	return res, nil
}

func matchSingleClick(
	ctx context.Context,
	db *sql.DB,
	row ClickRow,
	opts AttributionOptions,
) (*MatchedClick, *UnmatchedClick, error) {
	if net.ParseIP(strings.TrimSpace(row.IPAddress)) == nil {
		return nil, &UnmatchedClick{
			Row:    row,
			Reason: ReasonInvalidIP,
			Detail: "IP address could not be parsed: " + row.IPAddress,
		}, nil
	}

	// Tight window first.
	rec, err := lookupClickEvent(ctx, db, row.IPAddress, row.Timestamp, opts.Window, opts, true)
	if err != nil {
		return nil, nil, err
	}
	if rec != nil {
		profile, err := loadSubscriber(ctx, db, rec.SubscriberID, opts.OrgID)
		if err != nil {
			return nil, nil, err
		}
		if profile == nil {
			return nil, &UnmatchedClick{
				Row:    row,
				Reason: ReasonSubscriberMissing,
				Detail: "matched event " + rec.EventID + " but mailing_subscribers row not found",
			}, nil
		}
		return &MatchedClick{
			Row:            row,
			CampaignID:     rec.CampaignID,
			CampaignName:   rec.CampaignName,
			LinkURL:        rec.LinkURL,
			EventAt:        rec.EventAt,
			OffsetSeconds:  int64(rec.EventAt.Sub(row.Timestamp).Seconds()),
			ConfidenceTier: "tight",
			Subscriber:     *profile,
		}, nil, nil
	}

	// Fallback: wide IP-only window restricted to the same offer.
	rec, err = lookupClickEvent(ctx, db, row.IPAddress, row.Timestamp, opts.FallbackWindow, opts, false)
	if err != nil {
		return nil, nil, err
	}
	if rec != nil {
		profile, err := loadSubscriber(ctx, db, rec.SubscriberID, opts.OrgID)
		if err != nil {
			return nil, nil, err
		}
		if profile == nil {
			return nil, &UnmatchedClick{
				Row:    row,
				Reason: ReasonSubscriberMissing,
				Detail: "matched event " + rec.EventID + " but mailing_subscribers row not found",
			}, nil
		}
		return &MatchedClick{
			Row:            row,
			CampaignID:     rec.CampaignID,
			CampaignName:   rec.CampaignName,
			LinkURL:        rec.LinkURL,
			EventAt:        rec.EventAt,
			OffsetSeconds:  int64(rec.EventAt.Sub(row.Timestamp).Seconds()),
			ConfidenceTier: "fallback",
			Subscriber:     *profile,
		}, nil, nil
	}

	// Nothing matched. Categorize so the dashboard can show why.
	if IsBotScannerIP(row.IPAddress) {
		return nil, &UnmatchedClick{
			Row:    row,
			Reason: ReasonBotScannerIP,
			Detail: "IP belongs to a known email security/scanner range (Outlook, Defender, Apple MPP, etc.)",
		}, nil
	}
	return nil, &UnmatchedClick{
		Row:    row,
		Reason: ReasonNoEventInWindow,
		Detail: fmt.Sprintf("no clicked event for %s within %s of %s on offer matching %q",
			row.IPAddress, opts.FallbackWindow, row.Timestamp.UTC().Format(time.RFC3339), opts.OfferLikePattern),
	}, nil
}

func matchSingleConversion(
	ctx context.Context,
	db *sql.DB,
	row ConversionRow,
	opts AttributionOptions,
) (*MatchedConversion, *UnmatchedConversion, error) {
	ip := strings.TrimSpace(row.SessionUserIP)
	if net.ParseIP(ip) == nil {
		return nil, &UnmatchedConversion{
			Row:    row,
			Reason: ReasonInvalidIP,
			Detail: "session_user_ip could not be parsed: " + row.SessionUserIP,
		}, nil
	}

	// Tight window centered on click_date (NOT conversion_time — we're trying
	// to find the email click, not the merchant postback).
	rec, err := lookupClickEvent(ctx, db, ip, row.ClickTime, opts.Window, opts, true)
	if err != nil {
		return nil, nil, err
	}
	if rec != nil {
		profile, err := loadSubscriber(ctx, db, rec.SubscriberID, opts.OrgID)
		if err != nil {
			return nil, nil, err
		}
		if profile == nil {
			return nil, &UnmatchedConversion{
				Row:    row,
				Reason: ReasonSubscriberMissing,
				Detail: "matched event " + rec.EventID + " but mailing_subscribers row not found",
			}, nil
		}
		return &MatchedConversion{
			Row:            row,
			CampaignID:     rec.CampaignID,
			CampaignName:   rec.CampaignName,
			LinkURL:        rec.LinkURL,
			EventAt:        rec.EventAt,
			OffsetSeconds:  int64(rec.EventAt.Sub(row.ClickTime).Seconds()),
			ConfidenceTier: "tight",
			Subscriber:     *profile,
		}, nil, nil
	}

	rec, err = lookupClickEvent(ctx, db, ip, row.ClickTime, opts.FallbackWindow, opts, false)
	if err != nil {
		return nil, nil, err
	}
	if rec != nil {
		profile, err := loadSubscriber(ctx, db, rec.SubscriberID, opts.OrgID)
		if err != nil {
			return nil, nil, err
		}
		if profile == nil {
			return nil, &UnmatchedConversion{
				Row:    row,
				Reason: ReasonSubscriberMissing,
				Detail: "matched event " + rec.EventID + " but mailing_subscribers row not found",
			}, nil
		}
		return &MatchedConversion{
			Row:            row,
			CampaignID:     rec.CampaignID,
			CampaignName:   rec.CampaignName,
			LinkURL:        rec.LinkURL,
			EventAt:        rec.EventAt,
			OffsetSeconds:  int64(rec.EventAt.Sub(row.ClickTime).Seconds()),
			ConfidenceTier: "fallback",
			Subscriber:     *profile,
		}, nil, nil
	}

	if IsBotScannerIP(ip) {
		return nil, &UnmatchedConversion{
			Row:    row,
			Reason: ReasonBotScannerIP,
			Detail: "session IP belongs to a scanner range — conversion is real but click attribution impossible from this IP",
		}, nil
	}
	return nil, &UnmatchedConversion{
		Row:    row,
		Reason: ReasonNoEventInWindow,
		Detail: fmt.Sprintf("no clicked event for %s within %s of %s on offer matching %q",
			ip, opts.FallbackWindow, row.ClickTime.UTC().Format(time.RFC3339), opts.OfferLikePattern),
	}, nil
}

// trackingEventRecord is the trimmed mailing_tracking_events row we need.
type trackingEventRecord struct {
	EventID      string
	SubscriberID string
	CampaignID   string
	CampaignName string
	LinkURL      string
	EventAt      time.Time
}

// lookupClickEvent runs the actual SQL. The query is deliberately structured
// so the `tight` and `fallback` paths share the same shape — only the window
// width changes — which lets sqlmock tests assert against a single regex.
func lookupClickEvent(
	ctx context.Context,
	db *sql.DB,
	ip string,
	ts time.Time,
	window time.Duration,
	opts AttributionOptions,
	requireOfferMatch bool,
) (*trackingEventRecord, error) {
	// We always restrict by the offer ILIKE pattern. Even on the "fallback"
	// path the constraint is the same — it's the time window that widens.
	// Without the offer constraint a 14-day IP-only lookup would attribute
	// every TruGreen click on shared NAT to whichever subscriber clicked any
	// email last from that IP, which is almost guaranteed to be wrong.
	_ = requireOfferMatch

	args := []interface{}{
		ip,
		ts.UTC(),
		window.Seconds(),
		opts.OfferLikePattern,
	}
	orgFilter := ""
	if opts.OrgID != "" {
		orgFilter = " AND e.organization_id = $5::uuid"
		args = append(args, opts.OrgID)
	}

	q := fmt.Sprintf(`
		SELECT e.id::text,
		       COALESCE(e.subscriber_id::text, ''),
		       COALESCE(e.campaign_id::text, ''),
		       COALESCE(c.name, ''),
		       COALESCE(e.link_url, ''),
		       e.event_at
		FROM mailing_tracking_events e
		LEFT JOIN mailing_campaigns c ON c.id = e.campaign_id
		WHERE e.event_type = 'clicked'
		  AND e.ip_address = $1::inet
		  AND e.event_at BETWEEN ($2::timestamptz - ($3 || ' seconds')::interval)
		                     AND ($2::timestamptz + ($3 || ' seconds')::interval)
		  AND e.link_url ILIKE $4%s
		ORDER BY abs(extract(epoch FROM e.event_at - $2::timestamptz))
		LIMIT 1
	`, orgFilter)

	row := db.QueryRowContext(ctx, q, args...)
	var rec trackingEventRecord
	err := row.Scan(&rec.EventID, &rec.SubscriberID, &rec.CampaignID, &rec.CampaignName, &rec.LinkURL, &rec.EventAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookupClickEvent: %w", err)
	}
	if rec.SubscriberID == "" {
		// Click event existed but was not linked to a subscriber — that's
		// effectively a miss for attribution purposes.
		return nil, nil
	}
	return &rec, nil
}

// loadSubscriber pulls the small profile slice we surface in the UI. nil
// (no error) means the subscriber row is gone — which can happen when a
// list was wiped after the click event was logged. Callers must distinguish
// from the database error case.
func loadSubscriber(ctx context.Context, db *sql.DB, subscriberID, orgID string) (*SubscriberProfile, error) {
	if subscriberID == "" {
		return nil, nil
	}
	args := []interface{}{subscriberID}
	orgFilter := ""
	if orgID != "" {
		orgFilter = " AND organization_id = $2::uuid"
		args = append(args, orgID)
	}
	// last_engaged_at is computed as GREATEST(last_click_at, last_open_at) —
	// neither exists alone on the table; this matches the convention used
	// elsewhere when surfacing a single "engagement recency" value.
	q := fmt.Sprintf(`
		SELECT id::text,
		       email,
		       COALESCE(first_name, ''),
		       COALESCE(last_name, ''),
		       COALESCE(status, ''),
		       created_at,
		       GREATEST(last_click_at, last_open_at) AS last_engaged_at
		FROM mailing_subscribers
		WHERE id = $1::uuid%s
		LIMIT 1
	`, orgFilter)

	var p SubscriberProfile
	var createdAt sql.NullTime
	var lastEngaged sql.NullTime
	err := db.QueryRowContext(ctx, q, args...).Scan(
		&p.SubscriberID, &p.Email, &p.FirstName, &p.LastName, &p.Status,
		&createdAt, &lastEngaged,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loadSubscriber: %w", err)
	}
	if createdAt.Valid {
		t := createdAt.Time
		p.CreatedAt = &t
	}
	if lastEngaged.Valid {
		t := lastEngaged.Time
		p.LastEngagedAt = &t
	}
	return &p, nil
}
