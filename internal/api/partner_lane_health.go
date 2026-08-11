package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

// Partner Lane Health — GET /api/mailing/data-partners/lanes/{vertical}/health
//
// The Data Partners screen already answers "which feeds route where, and how
// engaged is the data" (FeedFunnelBoard) and "how is the drip pacing"
// (DripPerformancePanel). This answers the three questions those two cannot,
// each of which cost real records on 2026-08-11:
//
//  1. DELIVERY per ISP — sent / delivered / bounced. A lane can be claiming and
//     "mailing" perfectly while an ISP delivers nothing (Comcast: 82 sent, 0
//     delivered, 66 blocked). Engagement panels show a shrug; delivery shows the
//     block.
//
//  2. INTEGRITY — records CLAIMED vs campaign RECIPIENTS vs actually SENT.
//     partner_clean_queue.status='mailed' is a terminal claim stamp, not proof of
//     a send. When wave campaign names collided, 349 records were stamped against
//     a campaign that had 2 recipients and were never mailed or retried; ~1,118
//     were lost in a day before anyone noticed, because every screen showed
//     "mailed". A claimed-vs-sent gap is the signature and is surfaced here.
//
//  3. ATTRIBUTION READINESS — whether a conversion could even be attributed.
//     This lane's money CTA carries tokenid={{ custom.tid }}; tid is populated on
//     0.2% of sends and the offer has no everflow_offer_id, so conversions are
//     structurally invisible no matter how well it mails. "0 conversions" and
//     "conversions cannot be measured" look identical on every other screen.
//
// Read-only. Counting rules match agents/reporting/partner_lane_report.py so the
// portal and the CLI cannot disagree.

type laneISPRow struct {
	ISP       string `json:"isp"`
	Received  int64  `json:"received"`
	Claimed   int64  `json:"claimed"`
	Ready     int64  `json:"ready"`
	Suppressed int64 `json:"suppressed"`
	DeadLetter int64 `json:"dead_letter"`
	Sent      int64  `json:"sent"`
	Delivered int64  `json:"delivered"`
	Bounced   int64  `json:"bounced"`
}

type laneHealthResponse struct {
	Vertical string `json:"vertical"`

	Ingest struct {
		Records    int64   `json:"records"`
		LastHour   int64   `json:"last_hour"`
		LastRecord *string `json:"last_record_at"`
	} `json:"ingest"`

	Funnel map[string]int64 `json:"funnel"`

	Integrity struct {
		Claimed    int64 `json:"claimed"`
		Recipients int64 `json:"recipients"`
		Sent       int64 `json:"sent"`
		// Burned = claimed on a TERMINAL campaign with no 'sent' event. Records
		// in flight are excluded, so this is loss, not lag.
		Burned int64 `json:"burned"`
	} `json:"integrity"`

	ISPs []laneISPRow `json:"isps"`

	Attribution struct {
		Claimed          int64  `json:"claimed"`
		WithTID          int64  `json:"with_tid"`
		WithFirstName    int64  `json:"with_first_name"`
		OfferName        string `json:"offer_name"`
		EverflowOfferID  string `json:"everflow_offer_id"`
		Conversions      int64  `json:"conversions"`
	} `json:"attribution"`
}

func (h *PartnerAdminHandler) HandleGetLaneHealth(w http.ResponseWriter, r *http.Request) {
	vertical := strings.TrimSpace(chi.URLParam(r, "vertical"))
	if vertical == "" {
		writeJSONError(w, "vertical required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	out := laneHealthResponse{Vertical: vertical, Funnel: map[string]int64{}}

	// Dataset ids for the vertical — every query below is scoped to these.
	var dsIDs []string
	rows, err := h.db.QueryContext(ctx,
		`SELECT id::text FROM partner_datasets WHERE vertical = $1`, vertical)
	if err != nil {
		writeJSONError(w, "lane_health_datasets: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			dsIDs = append(dsIDs, id)
		}
	}
	rows.Close()
	if len(dsIDs) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}

	// Campaign ids: partner-drip waves are named "[partner-drip] <vertical> ...".
	var cIDs []string
	if crows, cerr := h.db.QueryContext(ctx,
		`SELECT id::text FROM mailing_campaigns WHERE name LIKE $1`,
		"[partner-drip] "+vertical+" %"); cerr == nil {
		for crows.Next() {
			var id string
			if crows.Scan(&id) == nil {
				cIDs = append(cIDs, id)
			}
		}
		crows.Close()
	}
	if len(cIDs) == 0 {
		cIDs = []string{"00000000-0000-0000-0000-000000000000"}
	}

	_ = h.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(record_count),0),
		       COUNT(*) FILTER (WHERE received_at > NOW() - INTERVAL '1 hour'),
		       MAX(received_at)::text
		FROM partner_inbound_batches WHERE dataset_id = ANY($1)`,
		pq.Array(dsIDs)).Scan(&out.Ingest.Records, &out.Ingest.LastHour, &out.Ingest.LastRecord)

	if frows, ferr := h.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM partner_clean_queue WHERE dataset_id = ANY($1) GROUP BY 1`,
		pq.Array(dsIDs)); ferr == nil {
		for frows.Next() {
			var s string
			var n int64
			if frows.Scan(&s, &n) == nil {
				out.Funnel[s] = n
			}
		}
		frows.Close()
	}

	_ = h.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total_recipients),0), COALESCE(SUM(sent_count),0)
		FROM mailing_campaigns WHERE id = ANY($1)`,
		pq.Array(cIDs)).Scan(&out.Integrity.Recipients, &out.Integrity.Sent)
	out.Integrity.Claimed = out.Funnel["mailed"]

	// Burned: claimed, campaign TERMINAL, and no 'sent' event for that
	// (campaign, subscriber). In-flight campaigns are excluded so this never
	// counts a record that is merely pending.
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM partner_clean_queue q
		JOIN mailing_campaigns c ON c.id = q.mailed_campaign_id
		WHERE q.dataset_id = ANY($1)
		  AND q.status = 'mailed'
		  AND q.subscriber_id IS NOT NULL
		  AND c.status IN ('sent','completed','completed_with_errors','cancelled','failed')
		  AND NOT EXISTS (
		      SELECT 1 FROM mailing_tracking_events e
		      WHERE e.campaign_id = q.mailed_campaign_id
		        AND e.subscriber_id = q.subscriber_id
		        AND e.event_type = 'sent')`,
		pq.Array(dsIDs)).Scan(&out.Integrity.Burned)

	if irows, ierr := h.db.QueryContext(ctx, `
		WITH q AS (
		  SELECT DISTINCT subscriber_id, isp_family, status
		  FROM partner_clean_queue WHERE dataset_id = ANY($1)
		),
		e AS (
		  SELECT DISTINCT subscriber_id, event_type
		  FROM mailing_tracking_events
		  WHERE campaign_id = ANY($2) AND subscriber_id IS NOT NULL
		)
		SELECT COALESCE(NULLIF(q.isp_family,''),'other') AS isp,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE q.status='mailed'),
		       COUNT(*) FILTER (WHERE q.status='ready'),
		       COUNT(*) FILTER (WHERE q.status='suppressed_eo'),
		       COUNT(*) FILTER (WHERE q.status='dead_letter'),
		       COUNT(DISTINCT q.subscriber_id) FILTER (WHERE q.subscriber_id IN (SELECT subscriber_id FROM e WHERE event_type='sent')),
		       COUNT(DISTINCT q.subscriber_id) FILTER (WHERE q.subscriber_id IN (SELECT subscriber_id FROM e WHERE event_type='delivered')),
		       COUNT(DISTINCT q.subscriber_id) FILTER (WHERE q.subscriber_id IN (SELECT subscriber_id FROM e WHERE event_type='bounced'))
		FROM q GROUP BY 1 ORDER BY 2 DESC`,
		pq.Array(dsIDs), pq.Array(cIDs)); ierr == nil {
		for irows.Next() {
			var row laneISPRow
			if irows.Scan(&row.ISP, &row.Received, &row.Claimed, &row.Ready,
				&row.Suppressed, &row.DeadLetter, &row.Sent, &row.Delivered, &row.Bounced) == nil {
				out.ISPs = append(out.ISPs, row)
			}
		}
		irows.Close()
	}

	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE COALESCE(s.custom_fields->>'tid','') <> ''),
		       COUNT(*) FILTER (WHERE COALESCE(s.first_name,'') <> '')
		FROM partner_clean_queue q
		JOIN mailing_subscribers s ON s.id = q.subscriber_id
		WHERE q.dataset_id = ANY($1) AND q.status = 'mailed'`,
		pq.Array(dsIDs)).Scan(&out.Attribution.Claimed, &out.Attribution.WithTID, &out.Attribution.WithFirstName)

	var offerName, efID sql.NullString
	_ = h.db.QueryRowContext(ctx, `
		SELECT o.name, COALESCE(o.everflow_offer_id,'')
		FROM partner_drip_creatives c JOIN mailing_offers o ON o.id = c.offer_id
		WHERE c.vertical = $1 AND c.active LIMIT 1`, vertical).Scan(&offerName, &efID)
	out.Attribution.OfferName = offerName.String
	out.Attribution.EverflowOfferID = efID.String

	_ = h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_everflow_conversions WHERE campaign_id = ANY($1)`,
		pq.Array(cIDs)).Scan(&out.Attribution.Conversions)

	writeJSON(w, http.StatusOK, out)
}
