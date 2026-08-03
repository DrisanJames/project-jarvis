package api

// Board week export (operator 2026-08-03: "I want to be able to export the
// week so I can review... This should be within the UI"). CSV of every draft +
// scheduled campaign in the window, one row per campaign — the Draft Board's
// "Export week" button downloads it.

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// HandleBoardWeekExport GET /api/mailing/pmta-campaign/board-export?days=7
func (s *PMTACampaignService) HandleBoardWeekExport(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 31 {
		days = 7
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT c.name, c.status,
		       COALESCE(to_char(c.scheduled_at AT TIME ZONE 'America/Denver',
		                        'YYYY-MM-DD HH24:MI'), '') sched_mt,
		       COALESCE(c.subject, '') subj,
		       COALESCE(c.from_name, '') ff,
		       COALESCE(c.pmta_config->'campaign_input'->>'sending_domain','') sd,
		       COALESCE(o.name, '') offer_name,
		       COALESCE(c.total_recipients, 0) recips
		FROM mailing_campaigns c
		LEFT JOIN mailing_offers o ON o.id = c.offer_id
		WHERE c.organization_id = $1
		  AND c.status IN ('draft','scheduled')
		  AND (c.scheduled_at IS NULL OR c.scheduled_at < now() + ($2 || ' days')::interval)
		ORDER BY c.scheduled_at NULLS LAST, c.name`, orgID, days)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "export query failed")
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="board-week-%s.csv"`, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"scheduled_mt", "name", "status", "offer",
		"sending_domain", "subject", "from_name", "recipients"})
	for rows.Next() {
		var name, status, sched, subj, ff, sd, offer string
		var recips int64
		if err := rows.Scan(&name, &status, &sched, &subj, &ff, &sd, &offer, &recips); err != nil {
			continue
		}
		_ = cw.Write([]string{sched, name, status, offer, sd, subj, ff,
			strconv.FormatInt(recips, 10)})
	}
	cw.Flush()
}
