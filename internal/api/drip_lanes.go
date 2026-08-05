package api

// Drip Lanes — operator control surface for the partner-drip lanes
// (operator 2026-08-05: "I should be able to manage this from the UI").
//
// One row per partner_datasets lane: which offer it mails, its daily
// new-record budget, pause state, and live queue depth. The daily budget is
// enforced supply-side by agents/jobs/drip_lane_release.py, which keeps at
// most `daily_cap` rows in status='ready' and parks the rest 'held'.

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

type dripLane struct {
	DatasetID string `json:"dataset_id"`
	Slug      string `json:"slug"`
	Vertical  string `json:"vertical"`
	OfferID   string `json:"offer_id"`
	OfferName string `json:"offer_name"`
	DailyCap  int    `json:"daily_cap"`
	Paused    bool   `json:"paused"`
	Ready     int    `json:"ready"`
	Held      int    `json:"held"`
	MailedTdy int    `json:"mailed_today"`
}

// HandleListDripLanes GET /api/mailing/drip-lanes
func (s *PMTACampaignService) HandleListDripLanes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT d.id::text, d.slug, d.vertical,
		       COALESCE(d.offer_id::text,''), COALESCE(o.name,''),
		       COALESCE(d.daily_cap,0), d.paused_emergency,
		       COALESCE(q.ready,0), COALESCE(q.held,0), COALESCE(q.mailed_today,0)
		FROM partner_datasets d
		LEFT JOIN mailing_offers o ON o.id = d.offer_id
		LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE status='ready')  ready,
			       count(*) FILTER (WHERE status='held')   held,
			       count(*) FILTER (WHERE status='mailed'
			         AND (mailed_at AT TIME ZONE 'America/Denver')::date = current_date) mailed_today
			FROM partner_clean_queue WHERE dataset_id = d.id
		) q ON TRUE
		WHERE d.status = 'active'
		ORDER BY COALESCE(q.ready,0) + COALESCE(q.held,0) DESC`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "drip-lanes query failed")
		return
	}
	defer rows.Close()
	out := []dripLane{}
	for rows.Next() {
		var l dripLane
		if err := rows.Scan(&l.DatasetID, &l.Slug, &l.Vertical, &l.OfferID, &l.OfferName,
			&l.DailyCap, &l.Paused, &l.Ready, &l.Held, &l.MailedTdy); err != nil {
			continue
		}
		out = append(out, l)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"lanes": out})
}

type dripLaneUpdate struct {
	DatasetID string `json:"dataset_id"`
	OfferID   string `json:"offer_id"`  // "" leaves unchanged; "none" clears
	DailyCap  *int   `json:"daily_cap"` // nil leaves unchanged; 0 = uncapped
	Paused    *bool  `json:"paused"`
}

// HandleUpdateDripLane POST /api/mailing/drip-lanes/update
func (s *PMTACampaignService) HandleUpdateDripLane(w http.ResponseWriter, r *http.Request) {
	var u dripLaneUpdate
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil || u.DatasetID == "" {
		respondError(w, http.StatusBadRequest, "dataset_id required")
		return
	}
	if u.DailyCap != nil {
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE partner_datasets SET daily_cap=$1, updated_at=NOW() WHERE id=$2::uuid`,
			*u.DailyCap, u.DatasetID); err != nil {
			respondError(w, http.StatusInternalServerError, "daily_cap update failed")
			return
		}
	}
	if u.OfferID != "" {
		var q string
		var arg interface{}
		if u.OfferID == "none" {
			q, arg = `UPDATE partner_datasets SET offer_id=NULL, updated_at=NOW() WHERE id=$1::uuid`, nil
		} else {
			q, arg = `UPDATE partner_datasets SET offer_id=$1::uuid, updated_at=NOW() WHERE id=$2::uuid`, u.OfferID
		}
		var err error
		if arg == nil {
			_, err = s.db.ExecContext(r.Context(), q, u.DatasetID)
		} else {
			_, err = s.db.ExecContext(r.Context(), q, arg, u.DatasetID)
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "offer update failed")
			return
		}
	}
	if u.Paused != nil {
		var reason sql.NullString
		if *u.Paused {
			reason = sql.NullString{String: "operator (Drip Lanes screen)", Valid: true}
		}
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE partner_datasets SET paused_emergency=$1, paused_reason=$2,
			 paused_at = CASE WHEN $1 THEN NOW() ELSE NULL END, updated_at=NOW()
			 WHERE id=$3::uuid`, *u.Paused, reason, u.DatasetID); err != nil {
			respondError(w, http.StatusInternalServerError, "pause update failed")
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}
