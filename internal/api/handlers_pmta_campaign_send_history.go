package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Recent send history for a sending domain — the operator-visible answer to
// "is this property actually mailing right now?"
//
// It exists because some programmes are paused OPERATIONALLY, with no flag
// anywhere: the KumoMTA estate is staged every morning and then cancelled en
// masse (measured 2026-08-18 — 9 estate domains created 05:25-05:34 and all
// cancelled at 06:41, four days running, while the two Yahoo ramp pilots kept
// sending). Nothing in kumo_estate.json records that. Rather than invent a
// pause flag the platform does not have, this reports what actually happened
// to the domain's recent campaigns and lets the operator read the pattern.

type domainSendHistory struct {
	SendingDomain string         `json:"sending_domain"`
	Days          int            `json:"days"`
	Counts        map[string]int `json:"counts"`
	Total         int            `json:"total"`
	LastSentAt    string         `json:"last_sent_at,omitempty"`
	// CancelRate over terminal outcomes (sent + cancelled + failed). A domain
	// whose recent campaigns are overwhelmingly cancelled is being held back,
	// whatever the registry says.
	CancelRate float64 `json:"cancel_rate"`
}

// HandleDomainSendHistory returns the recent campaign outcomes for a sending
// domain, resolved through its sending profiles.
//
// GET /api/mailing/pmta-campaign/domain-send-history?sending_domain=…&days=7
//
// Read-only. Domain-generic — nothing here is kumo-specific; the kumo estate is
// simply the case that makes it necessary.
func (s *PMTACampaignService) HandleDomainSendHistory(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sending_domain")))
	if domain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain is required"})
		return
	}
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil && parsed > 0 && parsed <= 90 {
			days = parsed
		}
	}

	// Scoped by profile rather than by campaign name: a campaign records the
	// profile it routes through, and the name is a free-text convention.
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT c.status, COUNT(*), MAX(c.updated_at) FILTER (WHERE c.status = 'sent')
		FROM mailing_campaigns c
		JOIN mailing_sending_profiles p ON p.id = c.sending_profile_id
		WHERE c.organization_id = $1
		  AND LOWER(p.sending_domain) = $2
		  AND c.created_at > NOW() - ($3 || ' days')::interval
		GROUP BY c.status
	`, orgID, domain, strconv.Itoa(days))
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	out := domainSendHistory{SendingDomain: domain, Days: days, Counts: map[string]int{}}
	var lastSent *time.Time
	for rows.Next() {
		var status string
		var n int
		var maxSent *time.Time
		if err := rows.Scan(&status, &n, &maxSent); err != nil {
			continue
		}
		out.Counts[status] = n
		out.Total += n
		if maxSent != nil && (lastSent == nil || maxSent.After(*lastSent)) {
			lastSent = maxSent
		}
	}
	if err := rows.Err(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if lastSent != nil {
		out.LastSentAt = lastSent.UTC().Format(time.RFC3339)
	}
	out.CancelRate = cancelRateOf(out.Counts)

	respondJSON(w, http.StatusOK, out)
}

// cancelRateOf is the share of TERMINAL outcomes that were cancelled. Campaigns
// still in flight (draft/scheduled/sending/finalizing) are excluded — counting
// them would make a healthy domain mid-send look held back. Returns 0 when
// nothing has reached a terminal state yet.
func cancelRateOf(counts map[string]int) float64 {
	terminal := counts["sent"] + counts["cancelled"] + counts["failed"] + counts["completed"] +
		counts["completed_with_errors"]
	if terminal == 0 {
		return 0
	}
	return float64(counts["cancelled"]) / float64(terminal)
}
