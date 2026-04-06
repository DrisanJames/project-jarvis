package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

type poolIPInfo struct {
	PoolName       string  `json:"pool_name"`
	IPAddress      string  `json:"ip_address"`
	IPID           string  `json:"ip_id"`
	Hostname       string  `json:"hostname"`
	Status         string  `json:"status"`
	TotalSent      int     `json:"total_sent"`
	ISPDelivered   int     `json:"isp_delivered"`
	ISPBounced     int     `json:"isp_bounced"`
	ISPDeferred    int     `json:"isp_deferred"`
	AcceptancePct  *float64 `json:"acceptance_pct"`
}

type poolIsolationStatus struct {
	YahooPools []poolIPInfo `json:"yahoo_pools"`
	MHGmailPool []poolIPInfo `json:"mh_gmail_pool"`
	Preferences []struct {
		PoolName      string `json:"pool_name"`
		PreferredIP   string `json:"preferred_ip"`
		StandbyIP     string `json:"standby_ip"`
		IsolationMode string `json:"isolation_mode"`
		Reason        string `json:"reason"`
		SetAt         string `json:"set_at"`
	} `json:"preferences"`
}

func (s *PMTACampaignService) HandlePoolIsolationStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := poolIsolationStatus{}

	yahooDomains := []string{
		"yahoo.com", "myyahoo.com", "ymail.com", "rocketmail.com",
		"yahoo.ca", "yahoo.co.uk", "yahoo.co.in", "yahoo.com.au", "yahoo.co.jp",
	}
	yahooFilter := buildDomainFilter(yahooDomains)

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			p.name, host(ia.ip_address), ia.id::text, ia.hostname, ia.status,
			COALESCE(ia.total_sent, 0),
			COALESCE(yh.isp_delivered, 0),
			COALESCE(yh.isp_bounced, 0),
			COALESCE(yh.isp_deferred, 0),
			CASE WHEN COALESCE(yh.isp_delivered,0) + COALESCE(yh.isp_bounced,0) > 0
				THEN ROUND(yh.isp_delivered::numeric / (yh.isp_delivered + yh.isp_bounced) * 100, 1)
				ELSE NULL END
		FROM mailing_ip_addresses ia
		JOIN mailing_ip_pools p ON ia.pool_id = p.id
		LEFT JOIN (
			SELECT source_ip,
				SUM(CASE WHEN record_type = 'd' THEN 1 ELSE 0 END) AS isp_delivered,
				SUM(CASE WHEN record_type = 'b' THEN 1 ELSE 0 END) AS isp_bounced,
				SUM(CASE WHEN record_type IN ('t','tq') THEN 1 ELSE 0 END) AS isp_deferred
			FROM pmta_acct_raw
			WHERE %s
			GROUP BY source_ip
		) yh ON yh.source_ip = host(ia.ip_address)
		WHERE p.name LIKE '%%-yahoo-pool'
			AND ia.status IN ('active', 'warmup')
		ORDER BY p.name, COALESCE(yh.isp_delivered, 0) DESC NULLS LAST
	`, yahooFilter))
	if err != nil {
		log.Printf("[PoolIsolation] yahoo query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ip poolIPInfo
		var accPct *float64
		rows.Scan(&ip.PoolName, &ip.IPAddress, &ip.IPID, &ip.Hostname, &ip.Status,
			&ip.TotalSent, &ip.ISPDelivered, &ip.ISPBounced, &ip.ISPDeferred, &accPct)
		ip.AcceptancePct = accPct
		resp.YahooPools = append(resp.YahooPools, ip)
	}

	gmailFilter := "recipient_domain IN ('gmail.com', 'googlemail.com')"
	mhRows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			p.name, host(ia.ip_address), ia.id::text, ia.hostname, ia.status,
			COALESCE(ia.total_sent, 0),
			COALESCE(gm.isp_delivered, 0),
			COALESCE(gm.isp_bounced, 0),
			COALESCE(gm.isp_deferred, 0),
			CASE WHEN COALESCE(gm.isp_delivered,0) + COALESCE(gm.isp_bounced,0) > 0
				THEN ROUND(gm.isp_delivered::numeric / (gm.isp_delivered + gm.isp_bounced) * 100, 1)
				ELSE NULL END
		FROM mailing_ip_addresses ia
		JOIN mailing_ip_pools p ON ia.pool_id = p.id
		LEFT JOIN (
			SELECT source_ip,
				SUM(CASE WHEN record_type = 'd' THEN 1 ELSE 0 END) AS isp_delivered,
				SUM(CASE WHEN record_type = 'b' THEN 1 ELSE 0 END) AS isp_bounced,
				SUM(CASE WHEN record_type IN ('t','tq') THEN 1 ELSE 0 END) AS isp_deferred
			FROM pmta_acct_raw
			WHERE %s
			GROUP BY source_ip
		) gm ON gm.source_ip = host(ia.ip_address)
		WHERE p.name = 'mh-gmail-pool'
			AND ia.status IN ('active', 'warmup')
		ORDER BY COALESCE(gm.isp_delivered, 0) DESC NULLS LAST
	`, gmailFilter))
	if err == nil {
		defer mhRows.Close()
		for mhRows.Next() {
			var ip poolIPInfo
			var accPct *float64
			mhRows.Scan(&ip.PoolName, &ip.IPAddress, &ip.IPID, &ip.Hostname, &ip.Status,
				&ip.TotalSent, &ip.ISPDelivered, &ip.ISPBounced, &ip.ISPDeferred, &accPct)
			ip.AcceptancePct = accPct
			resp.MHGmailPool = append(resp.MHGmailPool, ip)
		}
	}

	prefRows, err := s.db.QueryContext(ctx, `
		SELECT p.name, COALESCE(host(pref_ip.ip_address),''), COALESCE(host(standby_ip.ip_address),''),
			pref.isolation_mode, COALESCE(pref.reason,''), COALESCE(pref.set_at::text,'')
		FROM mailing_ip_pool_preferences pref
		JOIN mailing_ip_pools p ON p.id = pref.pool_id
		LEFT JOIN mailing_ip_addresses pref_ip ON pref_ip.id = pref.preferred_ip_id
		LEFT JOIN mailing_ip_addresses standby_ip ON standby_ip.id = pref.standby_ip_id
		ORDER BY p.name
	`)
	if err == nil {
		defer prefRows.Close()
		for prefRows.Next() {
			var p struct {
				PoolName      string `json:"pool_name"`
				PreferredIP   string `json:"preferred_ip"`
				StandbyIP     string `json:"standby_ip"`
				IsolationMode string `json:"isolation_mode"`
				Reason        string `json:"reason"`
				SetAt         string `json:"set_at"`
			}
			prefRows.Scan(&p.PoolName, &p.PreferredIP, &p.StandbyIP, &p.IsolationMode, &p.Reason, &p.SetAt)
			resp.Preferences = append(resp.Preferences, p)
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

type isolationRequest struct {
	PoolName    string `json:"pool_name"`
	PreferredIP string `json:"preferred_ip"`
	StandbyIP   string `json:"standby_ip"`
	Reason      string `json:"reason"`
}

type isolationResponse struct {
	Actions []string `json:"actions"`
	Errors  []string `json:"errors,omitempty"`
}

func (s *PMTACampaignService) HandlePoolIsolationActivate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var reqs []isolationRequest
	if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	resp := isolationResponse{}

	for _, req := range reqs {
		if req.PoolName == "" || req.PreferredIP == "" || req.StandbyIP == "" {
			resp.Errors = append(resp.Errors, fmt.Sprintf("missing fields for %s", req.PoolName))
			continue
		}
		reason := req.Reason
		if reason == "" {
			reason = "strict isolation activated via API"
		}

		_, err := s.db.ExecContext(ctx, `
			INSERT INTO mailing_ip_pool_preferences (pool_id, preferred_ip_id, standby_ip_id, isolation_mode, reason, set_by, set_at)
			SELECT pool.id, pref_ip.id, standby_ip.id, 'strict', $4, 'pool-isolation-api', NOW()
			FROM mailing_ip_pools pool
			CROSS JOIN mailing_ip_addresses pref_ip
			CROSS JOIN mailing_ip_addresses standby_ip
			WHERE pool.name = $1
				AND pref_ip.ip_address = $2::inet
				AND standby_ip.ip_address = $3::inet
			ON CONFLICT (pool_id) DO UPDATE SET
				preferred_ip_id = EXCLUDED.preferred_ip_id,
				standby_ip_id = EXCLUDED.standby_ip_id,
				isolation_mode = 'strict',
				reason = EXCLUDED.reason,
				set_by = EXCLUDED.set_by,
				set_at = NOW()
		`, req.PoolName, req.PreferredIP, req.StandbyIP, reason)
		if err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s preference insert: %v", req.PoolName, err))
			continue
		}
		resp.Actions = append(resp.Actions, fmt.Sprintf("%s: set preferred=%s standby=%s strict", req.PoolName, req.PreferredIP, req.StandbyIP))

		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address = $1::inet AND status != 'active'
		`, req.PreferredIP)
		if err == nil {
			resp.Actions = append(resp.Actions, fmt.Sprintf("%s: ensured preferred IP active", req.PreferredIP))
		}

		res, err := s.db.ExecContext(ctx, `
			UPDATE mailing_ip_addresses SET status = 'paused', updated_at = NOW()
			WHERE pool_id = (SELECT id FROM mailing_ip_pools WHERE name = $1)
				AND ip_address NOT IN ($2::inet, $3::inet)
				AND status NOT IN ('paused', 'cold')
		`, req.PoolName, req.PreferredIP, req.StandbyIP)
		if err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s pause others: %v", req.PoolName, err))
		} else if n, _ := res.RowsAffected(); n > 0 {
			resp.Actions = append(resp.Actions, fmt.Sprintf("%s: paused %d non-selected IPs", req.PoolName, n))
		}

		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_ip_addresses SET status = 'warmup', warmup_daily_limit = 0, updated_at = NOW()
			WHERE ip_address = $1::inet AND status NOT IN ('warmup', 'paused', 'cold')
		`, req.StandbyIP)
		if err == nil {
			resp.Actions = append(resp.Actions, fmt.Sprintf("%s: standby IP set to warmup (limit=0)", req.StandbyIP))
		}
	}

	log.Printf("[PoolIsolation] activated %d pools, %d errors", len(reqs), len(resp.Errors))
	respondJSON(w, http.StatusOK, resp)
}

func buildDomainFilter(domains []string) string {
	parts := make([]string, len(domains))
	for i, d := range domains {
		parts[i] = fmt.Sprintf("'%s'", d)
	}
	return "recipient_domain IN (" + strings.Join(parts, ", ") + ")"
}
