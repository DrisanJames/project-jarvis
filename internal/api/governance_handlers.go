package api

import (
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"
)

// --- Provider Qualification ---

type ispQualVerdict struct {
	Verdict         string   `json:"verdict"`
	Acceptance      float64  `json:"acceptance"`
	DeferralRate    float64  `json:"deferral_rate"`
	HardBounceRate  float64  `json:"hard_bounce_rate"`
	SoftBounceRate  float64  `json:"soft_bounce_rate"`
	ComplaintRate   float64  `json:"complaint_rate"`
	Sent            int      `json:"sent"`
	Delivered       int      `json:"delivered"`
	Deferred        int      `json:"deferred"`
	HardBounced     int      `json:"hard_bounced"`
	SoftBounced     int      `json:"soft_bounced"`
	Complained      int      `json:"complained"`
	QuotaMultiplier float64  `json:"quota_multiplier"`
	Reasons         []string `json:"reasons,omitempty"`
}

type providerQualResponse struct {
	EvaluatedAt   string                    `json:"evaluated_at"`
	LookbackHours int                       `json:"lookback_hours"`
	TotalEvents   int                       `json:"total_events"`
	ISPs          map[string]*ispQualVerdict `json:"isps"`
}

// HandleProviderQualification returns per-ISP qualification verdicts based on
// the last 24 hours of delivery data. Each ISP gets: full (1.0x quota),
// reduced (0.5x), or paused (0x) based on acceptance, deferral, bounce, and
// complaint rates.
func (s *PMTACampaignService) HandleProviderQualification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lookback := 24

	query := `
		SELECT
			COALESCE(LOWER(p.recipient_isp), 'other') AS isp,
			COUNT(*) FILTER (WHERE te.event_type = 'sent') AS sent,
			COUNT(*) FILTER (WHERE te.event_type = 'delivered') AS delivered,
			COUNT(*) FILTER (WHERE te.event_type = 'deferred') AS deferred,
			COUNT(*) FILTER (WHERE te.event_type = 'bounced' AND (` + HardBounceSQL("te") + `)) AS hard_bounced,
			COUNT(*) FILTER (WHERE te.event_type = 'bounced' AND NOT (` + HardBounceSQL("te") + `)) AS soft_bounced,
			COUNT(*) FILTER (WHERE te.event_type = 'complained') AS complained
		FROM mailing_tracking_events te
		JOIN mailing_campaign_plan_recipients p
		  ON p.campaign_id = te.campaign_id AND p.subscriber_id = te.subscriber_id
		WHERE te.created_at >= NOW() - INTERVAL '24 hours'
		GROUP BY COALESCE(LOWER(p.recipient_isp), 'other')
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		log.Printf("[Governance] provider qualification query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	resp := providerQualResponse{
		EvaluatedAt:   time.Now().UTC().Format(time.RFC3339),
		LookbackHours: lookback,
		ISPs:          make(map[string]*ispQualVerdict),
	}

	for rows.Next() {
		var ispName string
		var v ispQualVerdict
		if err := rows.Scan(&ispName, &v.Sent, &v.Delivered, &v.Deferred, &v.HardBounced, &v.SoftBounced, &v.Complained); err != nil {
			continue
		}

		total := v.Delivered + v.Deferred + v.HardBounced + v.SoftBounced
		if total == 0 {
			v.Verdict = "insufficient_data"
			v.QuotaMultiplier = 0.5
			v.Reasons = append(v.Reasons, "no delivery events in lookback window")
			resp.ISPs[ispName] = &v
			resp.TotalEvents += v.Sent
			continue
		}

		v.Acceptance = float64(v.Delivered) / float64(total)
		v.DeferralRate = float64(v.Deferred) / float64(total)
		v.HardBounceRate = float64(v.HardBounced) / float64(total)
		v.SoftBounceRate = float64(v.SoftBounced) / float64(total)
		if v.Delivered > 0 {
			v.ComplaintRate = float64(v.Complained) / float64(v.Delivered)
		}

		v.Verdict = "full"
		v.QuotaMultiplier = 1.0
		v.Reasons = nil

		// Complaint rate thresholds
		if v.ComplaintRate > 0.0008 {
			v.Verdict = "paused"
			v.QuotaMultiplier = 0.0
			v.Reasons = append(v.Reasons, "complaint rate > 0.08%")
		} else if v.ComplaintRate > 0.0003 {
			applyReduced(&v, "complaint rate > 0.03%")
		}

		// Hard bounce thresholds
		if v.HardBounceRate > 0.03 {
			v.Verdict = "paused"
			v.QuotaMultiplier = 0.0
			v.Reasons = append(v.Reasons, "hard bounce rate > 3%")
		} else if v.HardBounceRate > 0.015 {
			applyReduced(&v, "hard bounce rate > 1.5%")
		}

		// Deferral thresholds
		if v.DeferralRate > 0.25 {
			v.Verdict = "paused"
			v.QuotaMultiplier = 0.0
			v.Reasons = append(v.Reasons, "deferral rate > 25%")
		} else if v.DeferralRate > 0.15 {
			applyReduced(&v, "deferral rate > 15%")
		}

		// Acceptance thresholds
		if v.Acceptance < 0.80 {
			v.Verdict = "paused"
			v.QuotaMultiplier = 0.0
			v.Reasons = append(v.Reasons, "acceptance rate < 80%")
		} else if v.Acceptance < 0.90 {
			applyReduced(&v, "acceptance rate < 90%")
		}

		// Earned ramp for sustained high performance (higher tier checked first)
		if v.Verdict == "full" && v.Acceptance >= 0.97 && v.Sent >= 500 {
			v.QuotaMultiplier = 1.25
			v.Reasons = append(v.Reasons, "sustained acceptance >= 97%, earned +25% ramp")
		} else if v.Verdict == "full" && v.Acceptance >= 0.94 && v.Sent >= 200 {
			v.QuotaMultiplier = 1.15
			v.Reasons = append(v.Reasons, "sustained acceptance >= 94%, earned +15% ramp")
		}

		resp.ISPs[ispName] = &v
		resp.TotalEvents += v.Sent
	}

	respondJSON(w, http.StatusOK, resp)
}

func applyReduced(v *ispQualVerdict, reason string) {
	if v.Verdict != "paused" {
		v.Verdict = "reduced"
		v.QuotaMultiplier = 0.5
	}
	v.Reasons = append(v.Reasons, reason)
}

// --- Source Qualification ---

type listQualVerdict struct {
	Verdict        string   `json:"verdict"`
	ListID         string   `json:"list_id"`
	AcceptanceRate float64  `json:"acceptance_rate"`
	ComplaintRate  float64  `json:"complaint_rate"`
	BounceRate     float64  `json:"bounce_rate"`
	EvalCount      int      `json:"eval_count"`
	LifetimeSent   int      `json:"lifetime_sent"`
	Cap            int      `json:"cap,omitempty"`
	Reasons        []string `json:"reasons,omitempty"`
}

type sourceQualResponse struct {
	EvaluatedAt string                      `json:"evaluated_at"`
	Lists       map[string]*listQualVerdict `json:"lists"`
}

// HandleSourceQualification evaluates acquisition list quality from recent
// mailing_list_quality_metrics. Returns per-list verdicts: pass (full volume),
// cap (limited first exposure), or fail (excluded from sends).
func (s *PMTACampaignService) HandleSourceQualification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listsParam := r.URL.Query().Get("lists")
	if listsParam == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "lists parameter required (comma-separated names)"})
		return
	}

	listNames := strings.Split(listsParam, ",")
	for i := range listNames {
		listNames[i] = strings.TrimSpace(listNames[i])
	}

	orgID := getOrgID(r)

	resp := sourceQualResponse{
		EvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		Lists:       make(map[string]*listQualVerdict),
	}

	for _, name := range listNames {
		if name == "" {
			continue
		}

		v := &listQualVerdict{}

		var listID sql.NullString
		err := s.db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_lists
			WHERE organization_id = $1 AND (name = $2 OR id::text = $2)
			LIMIT 1
		`, orgID, name).Scan(&listID)
		if err != nil || !listID.Valid {
			v.Verdict = "not_found"
			v.Reasons = append(v.Reasons, "list not found in database")
			resp.Lists[name] = v
			continue
		}
		v.ListID = listID.String

		var evalCount int
		var avgAcceptance, avgComplaint, avgBounce float64
		var totalSent int
		err = s.db.QueryRowContext(ctx, `
			SELECT
				COUNT(*),
				COALESCE(AVG(acceptance_rate), 0),
				COALESCE(AVG(complaint_rate), 0),
				COALESCE(AVG(bounce_rate), 0),
				COALESCE(SUM(total_sent), 0)
			FROM mailing_list_quality_metrics
			WHERE list_id = $1 AND evaluated_at >= NOW() - INTERVAL '14 days'
		`, listID.String).Scan(&evalCount, &avgAcceptance, &avgComplaint, &avgBounce, &totalSent)
		if err != nil {
			v.Verdict = "error"
			v.Reasons = append(v.Reasons, "failed to query quality metrics")
			resp.Lists[name] = v
			continue
		}

		v.EvalCount = evalCount
		v.AcceptanceRate = avgAcceptance
		v.ComplaintRate = avgComplaint
		v.BounceRate = avgBounce
		v.LifetimeSent = totalSent

		if evalCount == 0 {
			v.Verdict = "cap"
			v.Cap = 500
			v.Reasons = append(v.Reasons, "no quality history — capped at 500 for first exposure")
			resp.Lists[name] = v
			continue
		}

		if evalCount == 1 {
			v.Verdict = "cap"
			v.Cap = 500
			v.Reasons = append(v.Reasons, "only 1 evaluation — capped until more data")
			resp.Lists[name] = v
			continue
		}

		failed := false
		if avgComplaint > 0.0005 {
			v.Verdict = "fail"
			v.Reasons = append(v.Reasons, "complaint rate > 0.05%")
			failed = true
		}
		if avgBounce > 0.02 {
			v.Verdict = "fail"
			v.Reasons = append(v.Reasons, "bounce rate > 2%")
			failed = true
		}
		if avgAcceptance < 0.90 {
			v.Verdict = "fail"
			v.Reasons = append(v.Reasons, "acceptance rate < 90%")
			failed = true
		}

		if !failed {
			if avgAcceptance >= 0.95 && avgComplaint < 0.0005 && avgBounce < 0.02 {
				v.Verdict = "pass"
			} else {
				v.Verdict = "cap"
				v.Cap = 1000
				v.Reasons = append(v.Reasons, "marginal quality — capped at 1000")
			}
		}

		resp.Lists[name] = v
	}

	log.Printf("[Governance] source qualification evaluated %d lists", len(resp.Lists))
	respondJSON(w, http.StatusOK, resp)
}
