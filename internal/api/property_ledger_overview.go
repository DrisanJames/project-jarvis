package api

// Property Ledger — estate-wide drip OVERVIEW + operator lane labels.
//
// GET  /api/mailing/pmta-campaign/property-ledger/overview
// PUT  /api/mailing/pmta-campaign/property-ledger/lane-label
//
// The overview is the whole drip estate in one payload: every ACTIVE
// partner_datasets feed's live pcq anatomy, rolled up three ways — totals,
// by ISP, by lane (vertical). It REUSES the per-dataset supply cache
// (laneSupplyFillAnatomy, property_lane_supply.go — 120s TTL, miss-collapse
// on the slot mutex) so it NEVER adds a new full-table scan over the 11M-row
// partner_clean_queue: every count is a dataset_id-keyed indexed aggregate,
// and the whole assembled overview is cached for a further 300s.
//
// DEFINITIONS (stated once, used everywhere in this payload):
//   clean = ready + mailed_lifetime   — records that passed cleaning and are
//           either mailable now or already mailed;
//   dirty = suppressed + dead_letter  — records cleaning rejected
//           (suppressed_eo) or that terminally failed (dead_letter).
// cleaning/pending_eo/eo_in_flight/held are IN-FLIGHT and belong to neither.
//
// Lane display names come from partner_drip_lane_labels (operator-editable
// via the PUT below). The table may not exist yet in a dev DB — label reads
// are BEST-EFFORT and degrade to empty labels rather than failing the
// overview (the vertical key still renders).
//
// ORG SCOPING: the partner tables carry no organization_id column (see the
// scope note in property_lane_journey.go) — getOrgID(r) is echoed back so
// the caller sees which tenant context served the read.
//
// OVERSEER DDL (lane labels — startup migration, NOT applied here):
//
//	CREATE TABLE IF NOT EXISTS partner_drip_lane_labels (
//	    vertical     TEXT PRIMARY KEY,
//	    display_name TEXT NOT NULL,
//	    updated_by   TEXT,
//	    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
//	);

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// propertyOverviewCacheTTL bounds staleness of the WHOLE assembled overview.
// Underneath it the per-dataset anatomy cache (120s) still applies; 300s here
// means a cold overview costs one pass over the active datasets at most once
// per 5 minutes regardless of how many operators keep the screen open.
const propertyOverviewCacheTTL = 300 * time.Second

// propertyOverviewCache is package-level (PMTACampaignService fields live in
// handlers_pmta_campaign.go; the service is a process singleton in prod).
// Misses collapse on mu — concurrent callers serialize, the first assembles.
var propertyOverviewCache struct {
	mu         sync.Mutex
	computedAt time.Time
	payload    map[string]interface{}
}

// invalidatePropertyOverviewCache clears the package-level cache. Called by
// the lane-label writer (an edit must show on the next read, not TTL-later)
// and by tests, because the cache outlives per-test service instances.
// Safe to call whether or not the caller holds the mutex is NOT true — this
// takes the lock itself; never call it while holding propertyOverviewCache.mu.
func invalidatePropertyOverviewCache() {
	propertyOverviewCache.mu.Lock()
	propertyOverviewCache.computedAt = time.Time{}
	propertyOverviewCache.payload = nil
	propertyOverviewCache.mu.Unlock()
}

// propertyOverviewDatasetsSQL — feed identities only (partner_datasets is a
// tiny table); the heavy counts ride the cached per-dataset path.
const propertyOverviewDatasetsSQL = `
	SELECT id::text, name, lower(btrim(vertical))
	FROM partner_datasets
	WHERE status = 'active'
	ORDER BY vertical, name`

// propertyOverviewMailedTodayByISPSQL — per-ISP mailed-today for one dataset.
// status='mailed' is included so the scan rides idx_pcq_dataset_status_mailed
// (dataset_id, status, mailed_at) as a tight range, exactly like the anatomy
// aggregate. markMailed stamps status='mailed' and mailed_at together, so the
// predicate is the same population the headline mailed_today counts (modulo
// rows that reached a later terminal state the same day — accepted skew,
// bounded by the day window). $2/$3 = Go-computed Denver-day UTC bounds.
const propertyOverviewMailedTodayByISPSQL = `
	SELECT COALESCE(NULLIF(isp_family, ''), 'other'), COUNT(*)
	FROM partner_clean_queue
	WHERE dataset_id = $1 AND status = 'mailed'
	  AND mailed_at >= $2 AND mailed_at < $3
	GROUP BY 1`

// laneLabelsSelectSQL reads every operator label. BEST-EFFORT at every call
// site: the table may not exist yet (overseer-owned DDL) and a missing label
// must never fail a read surface.
const laneLabelsSelectSQL = `
	SELECT lower(btrim(vertical)), display_name
	FROM partner_drip_lane_labels`

// laneLabelMap loads vertical → display_name, degrading to an empty map on
// ANY error (most importantly undefined_table on a dev DB where the overseer
// migration has not landed yet).
func laneLabelMap(ctx context.Context, db *sql.DB) map[string]string {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, laneLabelsSelectSQL)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var vertical, label string
		if err := rows.Scan(&vertical, &label); err != nil {
			return map[string]string{}
		}
		if vertical != "" && label != "" {
			out[vertical] = label
		}
	}
	if rows.Err() != nil {
		return map[string]string{}
	}
	return out
}

// RegisterPropertyLedgerOverviewRoutes mounts the overview + lane-label
// routes on the /api/mailing router, NEXT TO the service's main
// RegisterRoutes mount (server_routes_mailing.go). They are registered as
// static siblings of the mounted /pmta-campaign subrouter — chi resolves the
// static path first and backtracks to the mount for everything else
// (pinned by TestPropertyOverviewRouteCoexistsWithMount).
func (s *PMTACampaignService) RegisterPropertyLedgerOverviewRoutes(r chi.Router) {
	r.Get("/pmta-campaign/property-ledger/overview", s.HandlePropertyLedgerOverview)
	r.Put("/pmta-campaign/property-ledger/lane-label", s.HandleLaneLabelUpsert)
}

// HandlePropertyLedgerOverview GET …/property-ledger/overview
func (s *PMTACampaignService) HandlePropertyLedgerOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	// Same fail-closed index gate as the per-domain supply strip — the
	// per-dataset aggregates underneath are the SAME statements.
	if !s.laneSupplyIndexGate(ctx, w) {
		return
	}

	propertyOverviewCache.mu.Lock()
	defer propertyOverviewCache.mu.Unlock()
	if propertyOverviewCache.payload != nil &&
		time.Since(propertyOverviewCache.computedAt) < propertyOverviewCacheTTL {
		payload := propertyOverviewCache.payload
		payload["organization_id"] = orgID
		respondJSON(w, http.StatusOK, payload)
		return
	}

	payload, err := s.buildPropertyLedgerOverview(ctx)
	if err != nil {
		// 2026-08-22: under an IO-starved RDS one failed sub-read used to 500
		// the whole overview. Serve the LAST GOOD payload (up to 30 min old)
		// with an explicit staleness stamp instead; 503 only when there is
		// neither live nor cached data. Never a SILENT partial — the stamp is
		// the honesty.
		if propertyOverviewCache.payload != nil &&
			time.Since(propertyOverviewCache.computedAt) < 30*time.Minute {
			payload := propertyOverviewCache.payload
			payload["organization_id"] = orgID
			payload["stale_as_of"] = propertyOverviewCache.computedAt.UTC()
			payload["stale_reason"] = err.Error()
			respondJSON(w, http.StatusOK, payload)
			return
		}
		respondError(w, http.StatusServiceUnavailable,
			"overview build failed and no recent cached copy exists — the database is under load, retry shortly ("+err.Error()+")")
		return
	}
	propertyOverviewCache.payload = payload
	propertyOverviewCache.computedAt = time.Now()
	payload["organization_id"] = orgID
	respondJSON(w, http.StatusOK, payload)
}

// propertyOverviewLane accumulates one vertical's roll-up.
type propertyOverviewLane struct {
	Vertical     string `json:"vertical"`
	DisplayName  string `json:"display_name,omitempty"`
	Feeds        int    `json:"feeds"`
	Ready        int64  `json:"ready"`
	Cleaning     int64  `json:"cleaning"`
	PendingEO    int64  `json:"pending_eo"`
	Suppressed   int64  `json:"suppressed"`
	DeadLetter   int64  `json:"dead_letter"`
	MailedToday  int64  `json:"mailed_today"`
	TrancheTotal int64  `json:"tranche_total"`
	Clean        int64  `json:"clean"`
	Dirty        int64  `json:"dirty"`
}

func (s *PMTACampaignService) buildPropertyLedgerOverview(ctx context.Context) (map[string]interface{}, error) {
	dayStart, dayEnd := laneSupplyDenverDayBoundsUTC(time.Now())

	type feedIdent struct{ id, name, vertical string }
	feeds := []feedIdent{}
	err := func() error {
		rows, err := s.db.QueryContext(ctx, propertyOverviewDatasetsSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f feedIdent
			if err := rows.Scan(&f.id, &f.name, &f.vertical); err != nil {
				return err
			}
			feeds = append(feeds, f)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, errors.New("dataset query failed")
	}

	labels := laneLabelMap(ctx, s.db)

	totals := struct {
		ready, cleaning, pendingEO, eoInFlight, held  int64
		suppressed, deadLetter, mailedToday, mailedLT int64
	}{}
	byISPReady := map[string]int64{}
	byISPMailedToday := map[string]int64{}
	laneAgg := map[string]*propertyOverviewLane{}
	laneOrder := []string{}
	feedsPartial := 0

	for _, fd := range feeds {
		// The per-dataset supply cache is the ONLY count source — a cache
		// miss runs the same dataset_id-keyed indexed aggregates the supply
		// strip runs, never a full pcq scan.
		f := &laneSupplyFeed{DatasetID: fd.id, Name: fd.name, Vertical: fd.vertical,
			ReadyByISP: []laneSupplyISP{}, SharedBrands: []string{}}
		if err := s.laneSupplyFillAnatomy(ctx, f, dayStart, dayEnd); err != nil {
			return nil, err
		}
		if f.PartialError != "" {
			feedsPartial++
		}
		mailedISP, err := s.propertyOverviewMailedTodayByISP(ctx, fd.id, dayStart, dayEnd)
		if err != nil {
			// Fail-soft: this feed's mailed-today split is UNKNOWN for this
			// build; the rest of the estate still renders. Counted + flagged.
			feedsPartial++
			mailedISP = map[string]int64{}
		}

		totals.ready += f.ReadyTotal
		totals.cleaning += f.Cleaning
		totals.pendingEO += f.PendingEO
		totals.eoInFlight += f.EOInFlight
		totals.held += f.Held
		totals.suppressed += f.Suppressed
		totals.deadLetter += f.DeadLetter
		totals.mailedToday += f.MailedToday
		totals.mailedLT += f.MailedLifetime

		for _, e := range f.ReadyByISP {
			byISPReady[e.ISP] += e.Ready
		}
		for isp, n := range mailedISP {
			byISPMailedToday[isp] += n
		}

		lane, ok := laneAgg[fd.vertical]
		if !ok {
			lane = &propertyOverviewLane{Vertical: fd.vertical, DisplayName: labels[fd.vertical]}
			laneAgg[fd.vertical] = lane
			laneOrder = append(laneOrder, fd.vertical)
		}
		lane.Feeds++
		lane.Ready += f.ReadyTotal
		lane.Cleaning += f.Cleaning
		lane.PendingEO += f.PendingEO
		lane.Suppressed += f.Suppressed
		lane.DeadLetter += f.DeadLetter
		lane.MailedToday += f.MailedToday
		lane.TrancheTotal += f.TrancheTotal
		// clean = ready + mailed_lifetime; dirty = suppressed + dead_letter.
		lane.Clean += f.ReadyTotal + f.MailedLifetime
		lane.Dirty += f.Suppressed + f.DeadLetter
	}

	byLane := make([]propertyOverviewLane, 0, len(laneOrder))
	for _, v := range laneOrder {
		byLane = append(byLane, *laneAgg[v])
	}

	// ISP rows in the canonical ledger order (strangers appended, alphabetical)
	// — same discipline as the supply strip's ready split.
	ispKeys := map[string]bool{}
	for k := range byISPReady {
		ispKeys[k] = true
	}
	for k := range byISPMailedToday {
		ispKeys[k] = true
	}
	orderedCounts := map[string]int64{}
	for k := range ispKeys {
		orderedCounts[k] = 1 // ordering only; values re-read below
	}
	byISP := []map[string]interface{}{}
	for _, e := range laneSupplyOrderReadyByISP(orderedCounts) {
		byISP = append(byISP, map[string]interface{}{
			"isp":          e.ISP,
			"ready":        byISPReady[e.ISP],
			"mailed_today": byISPMailedToday[e.ISP],
		})
	}

	return map[string]interface{}{
		"computed_at": time.Now().UTC(),
		"denver_day":  time.Now().In(propertyLedgerLoc).Format("2006-01-02"),
		"totals": map[string]int64{
			"ready":           totals.ready,
			"cleaning":        totals.cleaning,
			"pending_eo":      totals.pendingEO,
			"eo_in_flight":    totals.eoInFlight,
			"held":            totals.held,
			"suppressed":      totals.suppressed,
			"dead_letter":     totals.deadLetter,
			"mailed_today":    totals.mailedToday,
			"mailed_lifetime": totals.mailedLT,
			"clean":           totals.ready + totals.mailedLT,
			"dirty":           totals.suppressed + totals.deadLetter,
		},
		"by_isp":        byISP,
		"by_lane":       byLane,
		"feeds_partial": feedsPartial,
		"partial":       feedsPartial > 0,
		"definitions_note": "clean = ready + mailed_lifetime; dirty = suppressed (suppressed_eo) + dead_letter. " +
			"Counts are live-queue facts served through the per-dataset supply cache (120s TTL) and a 300s whole-overview cache — computed_at is honest staleness.",
		"scope_note": "Supply is a DATASET fact shared across each vertical's brand rotation — never domain-owned inventory (see /property-ledger/supply).",
	}, nil
}

func (s *PMTACampaignService) propertyOverviewMailedTodayByISP(ctx context.Context, datasetID string, dayStart, dayEnd time.Time) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, propertyOverviewMailedTodayByISPSQL, datasetID, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var isp string
		var n int64
		if err := rows.Scan(&isp, &n); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(isp))] += n
	}
	return out, rows.Err()
}

// ── Lane labels — PUT …/property-ledger/lane-label ──────────────────────────
//
// Upsert into partner_drip_lane_labels (vertical PK). An EMPTY display_name
// DELETES the row (labels are pure presentation — a tombstone would be
// noise, and the roster soft-disable rule is about SENDING bindings, not
// display strings). Writes ride the SAME env gate as roster writes
// (PROPERTY_LEDGER_ROSTER_WRITE_ENABLED — laneRosterWriteGate): ships unset
// = read-only, flipping the env is the one-move enable/rollback.

const laneLabelMaxLen = 80

const laneLabelUpsertSQL = `
	INSERT INTO partner_drip_lane_labels (vertical, display_name, updated_by, updated_at)
	VALUES ($1, $2, $3, NOW())
	ON CONFLICT (vertical) DO UPDATE SET
		display_name = EXCLUDED.display_name,
		updated_by   = EXCLUDED.updated_by,
		updated_at   = NOW()`

const laneLabelDeleteSQL = `
	DELETE FROM partner_drip_lane_labels WHERE lower(btrim(vertical)) = $1`

// HandleLaneLabelUpsert PUT …/property-ledger/lane-label
// body {"vertical":"...","display_name":"..."} — empty display_name deletes.
func (s *PMTACampaignService) HandleLaneLabelUpsert(w http.ResponseWriter, r *http.Request) {
	if !laneRosterWriteGate(w) {
		return
	}
	ctx := r.Context()
	orgID := getOrgID(r)

	var req struct {
		Vertical    string `json:"vertical"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	vertical := strings.ToLower(strings.TrimSpace(req.Vertical))
	// Same slug charset gate as the journey vertical (^[a-z0-9_-]+$, ≤64).
	if !laneJourneyValidVertical(vertical) {
		respondError(w, http.StatusBadRequest, "vertical must be a lowercase feed key (letters, digits, '_' or '-')")
		return
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if len(displayName) > laneLabelMaxLen {
		respondError(w, http.StatusBadRequest, "display_name must be 80 characters or fewer")
		return
	}
	actor := actorFromRequest(r)

	deleted := false
	if displayName == "" {
		if _, err := s.db.ExecContext(ctx, laneLabelDeleteSQL, vertical); err != nil {
			respondError(w, http.StatusInternalServerError, "lane-label delete failed")
			return
		}
		deleted = true
	} else {
		if _, err := s.db.ExecContext(ctx, laneLabelUpsertSQL, vertical, displayName, actor); err != nil {
			respondError(w, http.StatusInternalServerError, "lane-label upsert failed")
			return
		}
	}

	// A label edit must show up on the next overview read, not TTL-later.
	invalidatePropertyOverviewCache()

	writeAuditLog(ctx, s.db, actor, "property_lane_label_upsert",
		"partner_drip_lane_labels", vertical,
		nil,
		map[string]interface{}{
			"display_name": displayName, "deleted": deleted,
			"organization_id": orgID,
		})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"vertical":        vertical,
		"display_name":    displayName,
		"deleted":         deleted,
		"organization_id": orgID,
	})
}
