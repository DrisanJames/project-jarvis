package api

// partner_feed_funnel.go — GET /api/mailing/data-partners/feed-funnel
//
// The Data Partners Overview decision surface. It answers, from ONE source of
// truth (partner_clean_queue, which stamps every drip record with its source
// partner_id + dataset_id + routed vertical), the operator's standing questions:
//
//   - which data FEEDS route into which FUNNELS (partner/dataset → vertical)
//   - how each feed's data is PERFORMING (records → mailed → engaged)
//   - how ENGAGED the data is, and which stream is most VALUABLE (engagement rate,
//     ranked) — so a badly-routed or dead stream is obvious at a glance
//   - the FUNNEL definition itself (the multi-touch ladder: per-touch creative +
//     subject + from-name) for the drill-in drawer
//
// Value here = ENGAGEMENT (engaged_at set / mailed). Conversions/revenue are a
// sparser, offer-attributed signal that lives on the drip-performance funnel
// endpoint; this surface ranks on the dense engagement signal and labels it as
// such. Read-only; re-routing decisions are advisory (the UI flags move
// candidates — it does not mutate partner_datasets.vertical).

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// feedFunnelRow is one data-feed stream routed to one funnel (vertical). The
// grain is (partner, dataset, vertical) so a partner feeding two funnels, or a
// funnel fed by several partners, both read correctly.
type feedFunnelRow struct {
	PartnerID    string  `json:"partner_id"`
	PartnerName  string  `json:"partner_name"`
	DatasetID    string  `json:"dataset_id"`
	DatasetName  string  `json:"dataset_name"`
	Vertical     string  `json:"vertical"`
	Records      int64   `json:"records"`
	Mailed       int64   `json:"mailed"`
	Engaged      int64   `json:"engaged"`
	EngRatePct   float64 `json:"eng_rate_pct"` // engaged / mailed * 100
	LastMailedAt *string `json:"last_mailed_at"`
}

// ladderTouch is one touch in a funnel's multi-touch ladder (touch 1 is the
// initial creative from partner_drip_creatives; touches 2..N are the follow-ups
// from partner_drip_followup_creatives). Brand-representative: we surface the
// 'db' lane so the drawer shows one clean ladder; other brands run parallel
// subject variants of the same offer sequence.
type ladderTouch struct {
	Vertical         string `json:"vertical"`
	TouchNumber      int    `json:"touch_number"`
	CreativeFilename string `json:"creative_filename"`
	SubjectLine      string `json:"subject_line"`
	FromName         string `json:"from_name"`
}

// HandleGetFeedFunnel serves GET /api/mailing/data-partners/feed-funnel.
func (h *PartnerAdminHandler) HandleGetFeedFunnel(w http.ResponseWriter, r *http.Request) {
	// A grouped scan of partner_clean_queue (same cost class as the drip-funnel
	// query) plus two small creative-table reads. Bounded so IO pressure can't
	// hang the dashboard.
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	resp := map[string]interface{}{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}

	feeds, err := h.feedFunnelRows(ctx)
	if err != nil {
		resp["feeds_error"] = err.Error()
	} else {
		resp["feeds"] = feeds
	}

	ladders, err := h.funnelLadders(ctx)
	if err != nil {
		resp["ladders_error"] = err.Error()
	} else {
		resp["ladders"] = ladders
	}

	respondJSON(w, http.StatusOK, resp)
}

// feedFunnelRows aggregates partner_clean_queue to (partner, dataset, vertical)
// with lifecycle counts. LEFT JOINs so a record whose partner/dataset row was
// removed still counts (bucketed as unknown) rather than silently vanishing.
func (h *PartnerAdminHandler) feedFunnelRows(ctx context.Context) ([]feedFunnelRow, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT
			COALESCE(pcq.partner_id::text, ''),
			COALESCE(dp.name, '(unknown partner)'),
			COALESCE(pcq.dataset_id::text, ''),
			COALESCE(pd.name, '(unknown dataset)'),
			COALESCE(pcq.vertical, ''),
			COUNT(*),
			COUNT(*) FILTER (WHERE pcq.mailed_at IS NOT NULL),
			COUNT(*) FILTER (WHERE pcq.engaged_at IS NOT NULL),
			MAX(pcq.mailed_at)
		FROM partner_clean_queue pcq
		LEFT JOIN partner_datasets pd ON pd.id = pcq.dataset_id
		LEFT JOIN data_partners   dp ON dp.id = pcq.partner_id
		GROUP BY 1, 2, 3, 4, 5
		HAVING COUNT(*) > 0
		ORDER BY COUNT(*) FILTER (WHERE pcq.engaged_at IS NOT NULL) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []feedFunnelRow{}
	for rows.Next() {
		var f feedFunnelRow
		var lastMailed sql.NullTime
		if err := rows.Scan(&f.PartnerID, &f.PartnerName, &f.DatasetID, &f.DatasetName,
			&f.Vertical, &f.Records, &f.Mailed, &f.Engaged, &lastMailed); err != nil {
			return nil, err
		}
		if f.Mailed > 0 {
			f.EngRatePct = float64(f.Engaged) / float64(f.Mailed) * 100
		}
		if lastMailed.Valid {
			s := lastMailed.Time.UTC().Format(time.RFC3339)
			f.LastMailedAt = &s
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// funnelLadders returns each funnel's touch ladder for the 'db' brand lane:
// touch 1 (initial) from partner_drip_creatives + touches 2..N (follow-ups)
// from partner_drip_followup_creatives.
func (h *PartnerAdminHandler) funnelLadders(ctx context.Context) ([]ladderTouch, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT vertical, 1 AS touch_number,
		       COALESCE(creative_filename, ''), COALESCE(subject_line, ''), COALESCE(from_name, '')
		FROM partner_drip_creatives
		WHERE brand = 'db' AND active
		UNION ALL
		SELECT vertical, touch_number,
		       COALESCE(creative_filename, ''), COALESCE(subject_line, ''), COALESCE(from_name, '')
		FROM partner_drip_followup_creatives
		WHERE brand = 'db' AND active
		ORDER BY vertical, touch_number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ladderTouch{}
	for rows.Next() {
		var t ladderTouch
		if err := rows.Scan(&t.Vertical, &t.TouchNumber, &t.CreativeFilename, &t.SubjectLine, &t.FromName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
