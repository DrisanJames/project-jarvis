package api

// Day Cards — per-sending-domain daily campaign panel for the PMTA board path.
//
// Read surface only in this file; the cancel+redeploy rebuild lives in
// day_cards_rebuild.go. Both hang off PMTACampaignService and are registered
// under /api/mailing/pmta-campaign/day-cards* in RegisterRoutes
// (handlers_pmta_campaign.go).
//
// NOTE ON THE NUMBERS: every counter here (sent/delivered/open/click/bounce/
// unsub) is the PG-denormalized mailing_campaigns column. Per-ISP delivery
// TRUTH is the VDM / analytics lake (PG delivered_count is an ingestion rate,
// not delivery — see memory pg-delivery-confirmation-undercount). This panel
// is judgement context for the operator, never a delivery claim.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// dayCardsQueryTimeout bounds each panel query; a deadline maps to a friendly
// 503 exactly like board_grid.go's respondLoadError.
const dayCardsQueryTimeout = 15 * time.Second

type dayCardDomain struct {
	SendingDomain string `json:"sending_domain"`
	Transport     string `json:"transport"` // kumo | ses | pmta
	ProfileIDs    int    `json:"profile_ids"`
}

// HandleDayCardsDomains lists the active sending domains from
// mailing_sending_profiles with a transport label. ALL active domains are
// included — the 11 kumo warmup domains (routing_mode='kumo') must appear.
// GET /api/mailing/pmta-campaign/day-cards/domains
func (s *PMTACampaignService) HandleDayCardsDomains(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx, cancel := context.WithTimeout(r.Context(), dayCardsQueryTimeout)
	defer cancel()

	// Transport precedence per domain mirrors the per-profile label in
	// HandleSendingDomains: routing_mode='kumo' → kumo, via_ses → ses, else
	// pmta. A domain with mixed profiles takes the strongest label.
	rows, err := s.db.QueryContext(ctx, `
		SELECT sending_domain,
		       CASE WHEN BOOL_OR(LOWER(COALESCE(routing_mode,'')) = 'kumo') THEN 'kumo'
		            WHEN BOOL_OR(COALESCE(via_ses, FALSE)) THEN 'ses'
		            ELSE 'pmta' END AS transport,
		       COUNT(*) AS profile_ids
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND status = 'active'
		  AND sending_domain IS NOT NULL AND sending_domain != ''
		GROUP BY sending_domain
		ORDER BY sending_domain`, orgID)
	if err != nil {
		respondDayCardsLoadError(w, err)
		return
	}
	defer rows.Close()

	out := []dayCardDomain{}
	for rows.Next() {
		var d dayCardDomain
		if err := rows.Scan(&d.SendingDomain, &d.Transport, &d.ProfileIDs); err != nil {
			respondDayCardsLoadError(w, err)
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		respondDayCardsLoadError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"domains": out})
}

type dayCard struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	ScheduledAt     string `json:"scheduled_at"`
	Slot            string `json:"slot"` // Denver HH24:MI
	OfferID         string `json:"offer_id,omitempty"`
	OfferKey        string `json:"offer_key,omitempty"`
	OfferName       string `json:"offer_name,omitempty"`
	Subject         string `json:"subject"`
	FromName        string `json:"from_name"`
	TotalRecipients int    `json:"total_recipients"`
	QueuedCount     int    `json:"queued_count"`
	SentCount       int    `json:"sent_count"`
	DeliveredCount  int    `json:"delivered_count"`
	OpenCount       int    `json:"open_count"`
	ClickCount      int    `json:"click_count"`
	// Bounces are always surfaced split (hard = reputation, soft = usually
	// transient — §6); BounceCount is the sum for compact displays.
	BounceCount      int    `json:"bounce_count"`
	HardBounceCount  int    `json:"hard_bounce_count"`
	SoftBounceCount  int    `json:"soft_bounce_count"`
	UnsubscribeCount int    `json:"unsubscribe_count"`
	HasConfig        bool   `json:"has_config"`
	RebuiltFrom      string `json:"rebuilt_from,omitempty"`
	Transport        string `json:"transport"`
}

type dayCardsTotals struct {
	Campaigns    int `json:"campaigns"`
	Recipients   int `json:"recipients"`
	Sent         int `json:"sent"`
	Delivered    int `json:"delivered"`
	Opened       int `json:"opened"`
	Clicked      int `json:"clicked"`
	Bounced      int `json:"bounced"`
	Unsubscribed int `json:"unsubscribed"`
}

// HandleDayCards returns the campaign cards for one sending domain on one
// Denver day, plus the previous day's completed sends as judgement context.
// GET /api/mailing/pmta-campaign/day-cards?domain=<sending_domain>&date=YYYY-MM-DD
func (s *PMTACampaignService) HandleDayCards(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	domain := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("domain")))
	if domain == "" {
		respondError(w, http.StatusBadRequest, "domain is required (a sending domain, e.g. em.discountblog.com)")
		return
	}
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	if date == "" {
		// The board day is a DENVER day (§6) — server-local "today" is a
		// different date for a few hours around midnight.
		date = denverToday(time.Now()).Format("2006-01-02")
	}
	dayTime, err := time.Parse("2006-01-02", date)
	if err != nil {
		respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	prevDate := dayTime.AddDate(0, 0, -1).Format("2006-01-02")

	ctx, cancel := context.WithTimeout(r.Context(), dayCardsQueryTimeout)
	defer cancel()

	cards, err := s.loadDayCards(ctx, orgID, domain, date, false)
	if err != nil {
		respondDayCardsLoadError(w, err)
		return
	}
	prevCards, err := s.loadDayCards(ctx, orgID, domain, prevDate, true)
	if err != nil {
		respondDayCardsLoadError(w, err)
		return
	}

	var totals dayCardsTotals
	totals.Campaigns = len(prevCards)
	for _, c := range prevCards {
		totals.Recipients += c.TotalRecipients
		totals.Sent += c.SentCount
		totals.Delivered += c.DeliveredCount
		totals.Opened += c.OpenCount
		totals.Clicked += c.ClickCount
		totals.Bounced += c.BounceCount
		totals.Unsubscribed += c.UnsubscribeCount
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"date":   date,
		"domain": domain,
		"cards":  cards,
		"prev_day": map[string]interface{}{
			"date":      prevDate,
			"totals":    totals,
			"campaigns": prevCards,
		},
	})
}

// loadDayCards runs ONE indexed query for one (org, sending domain, Denver
// day). The Denver bounds are computed here as two timestamptz instants so
// the WHERE stays sargable on idx_campaigns_org_sched (organization_id,
// scheduled_at) — an AT TIME ZONE on the column would defeat the index.
//
// completedOnly restricts to the prev-day statuses that carry real outcomes.
// Otherwise cancelled rows ARE included (with their status) so rebuild
// history stays visible; only 'deleted' is hidden.
func (s *PMTACampaignService) loadDayCards(ctx context.Context, orgID, domain, date string, completedOnly bool) ([]dayCard, error) {
	dayStart, dayEnd, err := denverDayBounds(date)
	if err != nil {
		return nil, err
	}

	statusFilter := `c.status <> 'deleted'`
	if completedOnly {
		statusFilter = `c.status IN ('sent','completed','completed_with_errors','sending')`
	}

	// Sending domain lives on the PROFILE, not the campaign row — LEFT JOIN
	// mailing_sending_profiles is the sanctioned resolution (board_grid.go).
	q := `
SELECT c.id::text,
       COALESCE(c.name,''),
       COALESCE(c.status,''),
       c.scheduled_at,
       to_char(c.scheduled_at AT TIME ZONE 'America/Denver','HH24:MI') AS slot,
       COALESCE(c.offer_id::text,''),
       COALESCE(c.offer_key,''),
       COALESCE(o.name,''),
       COALESCE(c.subject,''),
       COALESCE(c.from_name,''),
       COALESCE(c.total_recipients,0),
       COALESCE(c.queued_count,0),
       COALESCE(c.sent_count,0),
       COALESCE(c.delivered_count,0),
       COALESCE(c.open_count,0),
       COALESCE(c.click_count,0),
       COALESCE(c.hard_bounce_count,0),
       COALESCE(c.soft_bounce_count,0),
       COALESCE(c.unsubscribe_count,0),
       (c.pmta_config -> 'campaign_input') IS NOT NULL AS has_config,
       COALESCE(c.pmta_config ->> 'rebuilt_from','')    AS rebuilt_from,
       CASE WHEN LOWER(COALESCE(sp.routing_mode,'')) = 'kumo' THEN 'kumo'
            WHEN COALESCE(sp.via_ses, FALSE) THEN 'ses'
            ELSE 'pmta' END AS transport
  FROM mailing_campaigns c
  LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
  LEFT JOIN mailing_offers o ON o.id = c.offer_id
 WHERE c.organization_id = $1
   AND c.scheduled_at >= $2 AND c.scheduled_at < $3
   AND sp.sending_domain = $4
   AND ` + statusFilter + `
 ORDER BY c.scheduled_at ASC, c.name ASC`

	rows, err := s.db.QueryContext(ctx, q, orgID, dayStart, dayEnd, domain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []dayCard{}
	for rows.Next() {
		var c dayCard
		var schedAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &schedAt, &c.Slot,
			&c.OfferID, &c.OfferKey, &c.OfferName, &c.Subject, &c.FromName,
			&c.TotalRecipients, &c.QueuedCount, &c.SentCount, &c.DeliveredCount,
			&c.OpenCount, &c.ClickCount, &c.HardBounceCount, &c.SoftBounceCount,
			&c.UnsubscribeCount, &c.HasConfig, &c.RebuiltFrom, &c.Transport); err != nil {
			return nil, err
		}
		if schedAt.Valid {
			c.ScheduledAt = schedAt.Time.UTC().Format(time.RFC3339)
		}
		c.BounceCount = c.HardBounceCount + c.SoftBounceCount
		out = append(out, c)
	}
	return out, rows.Err()
}

// respondDayCardsLoadError maps a query failure onto the wire — a deadline is
// a friendly 503; anything else is logged in full and answered generically
// (same posture as board_grid.go respondLoadError).
func respondDayCardsLoadError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		respondError(w, http.StatusServiceUnavailable,
			"the day-cards query timed out after 15s — the database is busy, retry shortly")
		return
	}
	log.Printf("[day-cards] load failed: %v", err)
	respondError(w, http.StatusInternalServerError, "day-cards query failed")
}
