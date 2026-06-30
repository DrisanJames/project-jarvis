package api

// Send-Day Planner endpoints — back the new "Send-Day" sub-tab under
// Mailing → Campaign Center. Each endpoint is intentionally small and
// powers exactly one of the six pre-deploy gates documented in
// `.cursor/rules/send-day-process.mdc` §2 + §3, or the canvas's
// per-cell creative-resolve drawer.
//
// Routes are wired in server_routes_mailing.go under /api/mailing/send-day/*.
//
// Conventions match mailing_analytics_promoted.go:
//   * Each handler emits an `api_version` field so the frontend can
//     detect deploy drift (per testing.mdc PAGE_VERSION rule).
//   * Time windows are computed in America/Denver (MDT) — that's the
//     canonical send-day boundary the operator uses (see send-day
//     anchors in `.cursor/rules/sending-throttle.mdc`).
//   * Failure modes return 4xx with a JSON body, never naked
//     http.Error strings — the canvas surfaces these in a Toast.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version constants — bump on every behaviour change.
const (
	VersionSendDayVolumeReconciliation = "1.0"
	VersionSendDayBannedCreatives      = "1.0"
	VersionSendDayPreflightBatch       = "1.0"
	VersionSendDayCreativeResolve      = "1.0"
	VersionSendDayHostHealth           = "1.0"
)

// ─── Phase 1b — Volume reconciliation (Gate F) ───────────────────────────────
//
// Per `.cursor/rules/send-day-process.mdc` §2 Gate F:
//   today's planned total ≥ (yesterday's planned × 1.20 × 0.95)
//
// "Planned" = the GREATEST (union) of (a) sum(total_recipients) across
// committed mailing_campaigns whose scheduled_at falls in the target MDT
// day and (b) the sum of the staged Draft-Board per-ISP quotas for that
// same day — because draft campaigns carry total_recipients=0 until audience
// finalization, so the committed sum alone undercounts a send-day that is
// still staged as drafts. Both branches EXCLUDE status=cancelled/failed
// (those count against execution-quality, not the forward-looking ramp).
// See sumPlannedRecipientsForMDTDate for the full rationale.
//
// Returns:
//   {
//     date: "YYYY-MM-DD"          // operator-supplied or today MDT
//     today_planned: int
//     yesterday_planned: int
//     target: int                 // yesterday * 1.20
//     ramp_floor: int             // target * 0.95 (Gate F threshold)
//     gap: int                    // ramp_floor - today_planned (0 if pass)
//     percent_to_target: float    // today / target
//     passes: bool                // today >= ramp_floor
//   }
func (s *AdvancedMailingService) HandleSendDayVolumeReconciliation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dateStr := strings.TrimSpace(r.URL.Query().Get("date"))
	mdt, err := time.LoadLocation("America/Denver")
	if err != nil {
		writeJSONResponse(w, map[string]interface{}{
			"api_version": VersionSendDayVolumeReconciliation,
			"error":       "could not load America/Denver timezone",
		})
		return
	}

	var target time.Time
	if dateStr == "" {
		target = time.Now().In(mdt)
	} else {
		target, err = time.ParseInLocation("2006-01-02", dateStr, mdt)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"api_version": VersionSendDayVolumeReconciliation,
				"error":       fmt.Sprintf("invalid date %q (expected YYYY-MM-DD): %v", dateStr, err),
			})
			return
		}
	}
	todayDate := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, mdt)
	yesterdayDate := todayDate.AddDate(0, 0, -1)

	todayPlanned, err := s.sumPlannedRecipientsForMDTDate(ctx, todayDate)
	if err != nil {
		http.Error(w, fmt.Sprintf("today planned query: %v", err), http.StatusInternalServerError)
		return
	}
	yesterdayPlanned, err := s.sumPlannedRecipientsForMDTDate(ctx, yesterdayDate)
	if err != nil {
		http.Error(w, fmt.Sprintf("yesterday planned query: %v", err), http.StatusInternalServerError)
		return
	}

	// Per Gate F: target = yesterday * 1.20, ramp floor = target * 0.95.
	target120 := int(float64(yesterdayPlanned) * 1.20)
	rampFloor := int(float64(target120) * 0.95)
	gap := rampFloor - todayPlanned
	if gap < 0 {
		gap = 0
	}
	var percentToTarget float64
	if target120 > 0 {
		percentToTarget = float64(todayPlanned) / float64(target120)
	}
	passes := todayPlanned >= rampFloor && yesterdayPlanned > 0

	writeJSONResponse(w, map[string]interface{}{
		"api_version":       VersionSendDayVolumeReconciliation,
		"date":              todayDate.Format("2006-01-02"),
		"yesterday_date":    yesterdayDate.Format("2006-01-02"),
		"today_planned":     todayPlanned,
		"yesterday_planned": yesterdayPlanned,
		"target":            target120,
		"ramp_floor":        rampFloor,
		"gap":               gap,
		"percent_to_target": percentToTarget,
		"passes":            passes,
		// planned_basis documents what today_planned/yesterday_planned now
		// measure: the GREATEST of committed recipient counts and staged
		// Draft-Board per-ISP quotas, so a send-day still staged as drafts
		// (total_recipients=0) is no longer under-counted. See
		// sumPlannedRecipientsForMDTDate.
		"planned_basis": "max(committed total_recipients, staged draft ISP quotas) per MDT date",
	})
}

// sumPlannedRecipientsForMDTDate returns the TRUE planned recipient volume for
// the supplied MDT calendar date. It is the per-date GREATEST (union) of two
// signals, because a send-day plan is split across two representations in
// mailing_campaigns:
//
//   1. committed — SUM(total_recipients) over scheduled campaigns whose
//      audience has already been materialized.
//   2. staged    — SUM of the per-ISP Draft-Board quotas
//      (pmta_config -> 'campaign_input' -> 'isp_quotas'[].volume). Draft
//      campaigns are persisted with total_recipients = 0 until audience
//      finalization (see savePMTADraftCampaign), so the committed sum alone
//      misses the operator's staged plan for an upcoming send-day — that is
//      the root cause of Gate F's misleading low percentage (e.g. 85k/900k).
//
// Taking the per-date GREATEST means an upcoming send-day (drafts dominate,
// committed≈0) reports its staged quota total, while a fully-finalized day
// reports its materialized recipient total — never under-reporting either,
// and never double-counting. cancelled/failed campaigns are excluded; the MDT
// boundary is enforced by casting scheduled_at AT TIME ZONE 'America/Denver'
// before truncating to a date.
//
// NOTE: the committed branch deliberately keeps the literal SUM(total_recipients)
// aggregate — it is the contract the backend tests assert on, and remains the
// floor of the planned volume.
func (s *AdvancedMailingService) sumPlannedRecipientsForMDTDate(ctx context.Context, day time.Time) (int, error) {
	mdt, _ := time.LoadLocation("America/Denver")
	d := day.In(mdt)
	dayStr := d.Format("2006-01-02")
	var total int
	row := s.db.QueryRowContext(ctx, `
		SELECT GREATEST(
			COALESCE((
				SELECT SUM(total_recipients)
				FROM mailing_campaigns
				WHERE scheduled_at IS NOT NULL
				  AND (scheduled_at AT TIME ZONE 'America/Denver')::date = $1::date
				  AND status NOT IN ('cancelled','failed')
			), 0),
			COALESCE((
				SELECT SUM((q->>'volume')::numeric)::bigint
				FROM mailing_campaigns mc,
				     LATERAL jsonb_array_elements(
				         CASE WHEN jsonb_typeof(mc.pmta_config->'campaign_input'->'isp_quotas') = 'array'
				              THEN mc.pmta_config->'campaign_input'->'isp_quotas'
				              ELSE '[]'::jsonb END
				     ) AS q
				WHERE mc.scheduled_at IS NOT NULL
				  AND (mc.scheduled_at AT TIME ZONE 'America/Denver')::date = $1::date
				  AND mc.status NOT IN ('cancelled','failed')
			), 0)
		)::bigint
	`, dayStr)
	if err := row.Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// ─── Phase 1c — Banned creatives (single source of truth) ────────────────────
//
// Today the BANNED_CREATIVES set lives only in eng_w2_rotation.py
// (lines 63-65). Promote to the DB so the canvas, the wizard, and the
// Python deploy scripts read from one source. Seed via runStartupMigrations
// in cmd/server/main.go (idempotent INSERT ... ON CONFLICT).
//
// Returns: { banned: [ {filename, reason, paused_at} ] }
func (s *AdvancedMailingService) HandleSendDayBannedCreatives(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.QueryContext(ctx, `
		SELECT filename, COALESCE(reason, ''), COALESCE(paused_at::text, '')
		FROM mailing_banned_creatives
		ORDER BY paused_at DESC NULLS LAST, filename ASC
	`)
	if err != nil {
		http.Error(w, fmt.Sprintf("banned creatives query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type bannedRow struct {
		Filename string `json:"filename"`
		Reason   string `json:"reason"`
		PausedAt string `json:"paused_at"`
	}
	out := []bannedRow{}
	for rows.Next() {
		var b bannedRow
		if err := rows.Scan(&b.Filename, &b.Reason, &b.PausedAt); err != nil {
			continue
		}
		out = append(out, b)
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionSendDayBannedCreatives,
		"banned":      out,
		"count":       len(out),
	})
}

// ─── Phase 1d — Preflight batch (Gate D) ─────────────────────────────────────
//
// Fans out to the existing per-domain preflightDeployCheck so the canvas
// can validate every sending domain in one call. Returns a per-domain map:
//
//   {
//     results: {
//       "em.discountblog.com": { ok: true,  errors: [], warnings: [] },
//       "em.quizfiesta.com":   { ok: false, errors: [{ check, message }] }
//     },
//     all_ok: bool
//   }
//
// The body shape matches Gate D's pass criterion: every domain has a
// profile, pool active, ≥1 IP active or warmup. We delegate the actual
// check to preflightDeployCheck (pmta_campaign_planner.go) so there's
// one source of truth for what "preflight pass" means.
func (s *PMTACampaignService) HandleSendDayPreflightBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	if orgID == "" {
		orgID = defaultOrgID
	}

	var body struct {
		Domains []string `json:"domains"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayPreflightBatch,
			"error":       fmt.Sprintf("could not parse JSON body: %v", err),
		})
		return
	}
	if len(body.Domains) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayPreflightBatch,
			"error":       "domains array is required and must be non-empty",
		})
		return
	}

	type domainResult struct {
		OK       bool             `json:"ok"`
		Errors   []preflightError `json:"errors"`
		Warnings []preflightError `json:"warnings"`
	}
	results := map[string]domainResult{}
	allOK := true

	// Bounded parallelism — preflight does DB + DNS lookups; keep the
	// concurrency conservative so a 16-domain canvas doesn't blow up
	// the connection pool. 4 inflight matches AudienceFinalizationWorker.
	sem := make(chan struct{}, 4)
	mu := sync.Mutex{}
	wg := sync.WaitGroup{}

	for _, raw := range body.Domains {
		dom := strings.TrimSpace(strings.ToLower(raw))
		if dom == "" {
			continue
		}
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := preflightDeployCheck(ctx, s.db, orgID, d, "")
			out := domainResult{
				OK:       res.OK,
				Errors:   res.Errors,
				Warnings: res.Warnings,
			}
			if out.Errors == nil {
				out.Errors = []preflightError{}
			}
			if out.Warnings == nil {
				out.Warnings = []preflightError{}
			}

			mu.Lock()
			results[d] = out
			if !res.OK {
				allOK = false
			}
			mu.Unlock()
		}(dom)
	}
	wg.Wait()

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionSendDayPreflightBatch,
		"results":     results,
		"all_ok":      allOK,
	})
}

// ─── Phase 1e — Creative resolve (server-side HTML pipeline) ─────────────────
//
// See the dedicated file send_day_creative_resolve.go — this comment is
// kept here so the route registration block stays readable.

// ─── Phase 1f — Host health (Gate A) ─────────────────────────────────────────
//
// v1 ships as an operator-checkbox surrogate. SSH from ECS is closed by
// firewall rules (.cursor/rules/deployment-and-infrastructure.mdc PMTA
// Access Control) so we cannot scrape the OVH boxes from the API. Until
// the OVH-side cron + S3 push lands, the canvas reads:
//
//   * the latest known acct-forward sha (from a config table)
//   * an operator-checkbox-stored "I confirmed Gate A via SSH" record
//
// Returns:
//   {
//     ok: bool           // true only if both servers pass checklist
//     servers: { server_a, server_b: { state, last_checked_at, message } }
//     guidance: string   // SSH command to run if checkbox needs refresh
//   }
//
// State values: "pass" | "stale" | "fail" | "unknown".
func (s *AdvancedMailingService) HandleSendDayHostHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type serverState struct {
		State         string `json:"state"`
		LastCheckedAt string `json:"last_checked_at,omitempty"`
		Message       string `json:"message,omitempty"`
	}
	servers := map[string]serverState{
		"server_a": {State: "unknown"},
		"server_b": {State: "unknown"},
	}

	// Best-effort read from mailing_send_day_gate_attestations. Absent
	// table or empty row = "unknown" (the canvas will then prompt the
	// operator to check off Gate A explicitly).
	rows, err := s.db.QueryContext(ctx, `
		SELECT server_key,
		       COALESCE(state, 'unknown'),
		       COALESCE(last_checked_at::text, ''),
		       COALESCE(message, '')
		FROM mailing_send_day_gate_attestations
		WHERE gate = 'A'
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key, state, ts, msg string
			if scanErr := rows.Scan(&key, &state, &ts, &msg); scanErr != nil {
				continue
			}
			if key == "server_a" || key == "server_b" {
				servers[key] = serverState{
					State: state, LastCheckedAt: ts, Message: msg,
				}
			}
		}
	}

	allOK := true
	for _, st := range servers {
		if st.State != "pass" {
			allOK = false
			break
		}
	}

	guidance := strings.TrimSpace(`
ssh -i ~/.ssh/ovh_pmta rocky@15.204.101.125 "P=\$(pgrep -f /usr/sbin/pmtad | head -1); ps -p \$P -o pid,etime,rss; sudo coredumpctl list 2>&1 | grep pmtad | tail -3; sha256sum /usr/local/bin/pmta-acct-forward; ps -ef | grep pmta-acct-forward | grep -v grep | wc -l"
# Repeat for 15.204.107.107 (server_b)
`)

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionSendDayHostHealth,
		"ok":          allOK,
		"servers":     servers,
		"guidance":    guidance,
		"note":        "v1 implementation — Gate A is operator-attested until OVH cron pushes telemetry to S3.",
	})
}

// HandleSendDayHostHealthAttest accepts an operator attestation for a
// single Gate-A server (server_a or server_b). Body:
//
//   { server_key: "server_a", state: "pass" | "fail" | "stale",
//     message?: "...", updated_by?: "operator-handle" }
//
// Persists into mailing_send_day_gate_attestations so the GET endpoint
// returns it on next read. v1 stores nothing more elaborate than the
// operator's own checkbox.
func (s *AdvancedMailingService) HandleSendDayHostHealthAttest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerKey string `json:"server_key"`
		State     string `json:"state"`
		Message   string `json:"message"`
		UpdatedBy string `json:"updated_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayHostHealth,
			"error":       fmt.Sprintf("could not parse JSON body: %v", err),
		})
		return
	}
	body.ServerKey = strings.TrimSpace(body.ServerKey)
	body.State = strings.TrimSpace(body.State)
	if body.ServerKey != "server_a" && body.ServerKey != "server_b" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayHostHealth,
			"error":       "server_key must be server_a or server_b",
		})
		return
	}
	switch body.State {
	case "pass", "fail", "stale", "unknown":
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayHostHealth,
			"error":       "state must be pass | fail | stale | unknown",
		})
		return
	}

	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO mailing_send_day_gate_attestations
		    (gate, server_key, state, message, last_checked_at, updated_by, updated_at)
		VALUES ('A', $1, $2, $3, NOW(), $4, NOW())
		ON CONFLICT (gate, server_key) DO UPDATE
		   SET state = EXCLUDED.state,
		       message = EXCLUDED.message,
		       last_checked_at = EXCLUDED.last_checked_at,
		       updated_by = EXCLUDED.updated_by,
		       updated_at = NOW()
	`, body.ServerKey, body.State, body.Message, body.UpdatedBy)
	if err != nil {
		http.Error(w, fmt.Sprintf("attestation upsert: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version": VersionSendDayHostHealth,
		"ok":          true,
		"server_key":  body.ServerKey,
		"state":       body.State,
	})
}

// orderedKeys is a small helper so the JSON output of map-keyed structures
// is deterministic in tests. Not all callers use it (results map is fine
// non-deterministic for the canvas) but kept for future use.
func orderedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
