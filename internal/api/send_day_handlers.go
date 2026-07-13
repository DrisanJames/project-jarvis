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
	"database/sql"
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
	VersionSendDayHostHealth           = "1.1"
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

	todayPlanned, err := sumPlannedRecipientsForMDTDate(ctx, s.db, todayDate)
	if err != nil {
		http.Error(w, fmt.Sprintf("today planned query: %v", err), http.StatusInternalServerError)
		return
	}
	yesterdayPlanned, err := sumPlannedRecipientsForMDTDate(ctx, s.db, yesterdayDate)
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
// Free function (not a service method) so the deploy path's server-side gate
// evaluation (evaluateSendDayGates, REQ-007) can reuse the exact same Gate-F
// volume source from PMTACampaignService.
func sumPlannedRecipientsForMDTDate(ctx context.Context, db *sql.DB, day time.Time) (int, error) {
	mdt, _ := time.LoadLocation("America/Denver")
	d := day.In(mdt)
	dayStr := d.Format("2006-01-02")
	var total int
	row := db.QueryRowContext(ctx, `
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
// State values: "pass" | "stale" | "fail" | "unknown". Gate A is a DAILY
// check (pmtad uptime, coredumps, acct-forward sha) — an attestation from a
// prior MDT send-day proves nothing about today, so a persisted "pass" whose
// last_checked_at predates the current MDT send-day is degraded to "stale"
// at read time. The canvas renders stale as amber and re-prompts the operator.
func (s *AdvancedMailingService) HandleSendDayHostHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	servers, _ := readGateAServerStates(ctx, s.db, time.Now())

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

// gateAServerState is one Gate-A server's attested state as returned by
// readGateAServerStates (shared by the host-health endpoint and the deploy
// path's server-side gate evaluation).
type gateAServerState struct {
	State         string `json:"state"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`
	Message       string `json:"message,omitempty"`
}

// readGateAServerStates loads the Gate-A attestations for both PMTA servers
// and applies the freshness degrade (REQ-012): a persisted "pass" whose
// last_checked_at predates the supplied instant's MDT send-day is returned as
// "stale". Absent table / empty rows leave the slots "unknown"; the query
// error (if any) is returned so callers can distinguish "unattested" from
// "unreadable".
func readGateAServerStates(ctx context.Context, db *sql.DB, now time.Time) (map[string]gateAServerState, error) {
	servers := map[string]gateAServerState{
		"server_a": {State: "unknown"},
		"server_b": {State: "unknown"},
	}

	dayStart := startOfMDTSendDay(now)
	rows, err := db.QueryContext(ctx, `
		SELECT server_key,
		       COALESCE(state, 'unknown'),
		       last_checked_at,
		       COALESCE(message, '')
		FROM mailing_send_day_gate_attestations
		WHERE gate = 'A'
	`)
	if err != nil {
		return servers, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, state, msg string
		var ts sql.NullTime
		if scanErr := rows.Scan(&key, &state, &ts, &msg); scanErr != nil {
			continue
		}
		if key != "server_a" && key != "server_b" {
			continue
		}
		lastChecked := ""
		if ts.Valid {
			lastChecked = ts.Time.UTC().Format(time.RFC3339)
		}
		// Freshness degrade: a "pass" only holds for the send-day it was
		// attested on. Missing timestamp = unprovable freshness = stale.
		if state == "pass" && (!ts.Valid || ts.Time.Before(dayStart)) {
			state = "stale"
			msg = "attestation predates the current MDT send-day — re-verify both servers over SSH and re-attest"
		}
		servers[key] = gateAServerState{
			State: state, LastCheckedAt: lastChecked, Message: msg,
		}
	}
	return servers, nil
}

// ─── Server-side gate enforcement (REQ-007) ──────────────────────────────────
//
// The six pre-deploy gates were display-only: the Draft Board approve action
// and every programmatic POST /pmta-campaign/deploy skipped them entirely
// (findings 2026-07-13-A §2 — the 2026-06-29 volume-collapse class).
// evaluateSendDayGates is the ONE server-side evaluation both the id-less
// deploy and the draft-promotion path run before reserving a campaign.
//
// Verdict semantics are fail-closed: "pass" is the only state that clears a
// gate; "fail" AND "unknown" (source unreadable) both block, and the caller
// must require an explicit, audit-logged operator override to proceed
// (silently passing on unknown was Gate F's original hole).
//
// Sources (all REAL after REQ-012 — never re-derived client-side):
//   Gate A — mailing_send_day_gate_attestations w/ MDT staleness degrade
//   Gate B — the wave-scheduler janitor counts (zombies + expired < 50 each)
//   Gate C — deliveryBuildFlags (in-process; structurally true for this binary)
//   Gate D — preflightDeployCheck for the payload's sending domain/profile
//   Gate F — planned-volume collapse floor: today ≥ 60% of yesterday
//            (sumPlannedRecipientsForMDTDate both sides — the same floor the
//            JAOS orchestrator N1 enforces; the +20% ramp target stays
//            advisory in HandleSendDayVolumeReconciliation). Audience-bound
//            payloads (every quota volume == 0 — the standing uncapped
//            engaged-tier doctrine) are exempt: their planned volume is the
//            audience, not a cap, so the collapse floor does not apply.
//   Gate E (audit JSON in .scratch/) is operator-side by construction and is
//   written by the deploy tooling itself; it is not server-evaluable.

type sendDayGateVerdict struct {
	Gate   string `json:"gate"`
	Name   string `json:"name"`
	State  string `json:"state"` // "pass" | "fail" | "unknown"
	Detail string `json:"detail,omitempty"`
}

type sendDayGateReport struct {
	Verdicts []sendDayGateVerdict `json:"gates"`
}

// failed returns every gate that did NOT pass (fail and unknown both block).
func (r sendDayGateReport) failed() []sendDayGateVerdict {
	var out []sendDayGateVerdict
	for _, v := range r.Verdicts {
		if v.State != "pass" {
			out = append(out, v)
		}
	}
	return out
}

// sendDayGateEvalInput carries the per-deploy context the gate evaluation
// needs beyond the DB handle.
type sendDayGateEvalInput struct {
	OrgID string
	// TargetDay anchors Gate F to the send-day being deployed (the payload's
	// scheduled_at) so an evening approval of tomorrow's board reconciles
	// TOMORROW's planned volume, not today's. Zero value = now (immediate
	// sends).
	TargetDay time.Time
	// Uncapped marks an audience-bound payload (no finite ISP quota anywhere)
	// — the standing uncapped engaged-tier doctrine. Exempts Gate F.
	Uncapped bool
	// Preflight runs Gate D for the payload's sending domain. nil = the gate
	// reports "unknown" (which blocks).
	Preflight func(ctx context.Context) preflightResult
}

// evaluateSendDayGates computes the server-side verdict for gates A, B, C, D
// and F in that (deterministic) order. It never returns an error: an
// unreadable source is an "unknown" verdict, which blocks.
func evaluateSendDayGates(ctx context.Context, db *sql.DB, in sendDayGateEvalInput) sendDayGateReport {
	var report sendDayGateReport

	// Gate A — PMTA host health attestation (fresh for the current MDT day).
	{
		v := sendDayGateVerdict{Gate: "A", Name: "PMTA host health attestation"}
		servers, err := readGateAServerStates(ctx, db, time.Now())
		if err != nil {
			v.State = "unknown"
			v.Detail = fmt.Sprintf("attestation store unreadable: %v", err)
		} else {
			v.State = "pass"
			var bad []string
			for _, key := range []string{"server_a", "server_b"} {
				if st := servers[key]; st.State != "pass" {
					bad = append(bad, fmt.Sprintf("%s=%s", key, st.State))
				}
			}
			if len(bad) > 0 {
				v.State = "fail"
				v.Detail = strings.Join(bad, ", ") + " — re-verify both servers over SSH and re-attest (Send Day tab, Gate A)"
			}
		}
		report.Verdicts = append(report.Verdicts, v)
	}

	// Gate B — wave-dispatcher janitor counts (< 50 zombies AND < 50 expired).
	{
		v := sendDayGateVerdict{Gate: "B", Name: "Wave-dispatcher cleanup"}
		counts, err := queryWaveSchedulerCounts(ctx, db)
		switch {
		case err != nil:
			v.State = "unknown"
			v.Detail = fmt.Sprintf("wave-health query failed: %v", err)
		case counts.Zombies < 50 && counts.Expired < 50:
			v.State = "pass"
			v.Detail = fmt.Sprintf("zombies=%d expired=%d", counts.Zombies, counts.Expired)
		default:
			v.State = "fail"
			v.Detail = fmt.Sprintf("zombies=%d expired=%d (threshold <50 each) — run the pre-deploy janitor", counts.Zombies, counts.Expired)
		}
		report.Verdicts = append(report.Verdicts, v)
	}

	// Gate C — dead-letter classifier build check (in-process, REQ-012).
	{
		v := sendDayGateVerdict{Gate: "C", Name: "Delivery build check"}
		if deliveryBuildFlags[gateCRequiredCommit] {
			v.State = "pass"
			v.Detail = "build contains " + gateCRequiredCommit + " (IsPMTATransient classifier)"
		} else {
			v.State = "fail"
			v.Detail = "running binary lacks the " + gateCRequiredCommit + " dead-letter classifier fix"
		}
		report.Verdicts = append(report.Verdicts, v)
	}

	// Gate D — sending-profile preflight for THIS deploy's domain.
	{
		v := sendDayGateVerdict{Gate: "D", Name: "Sending-profile preflight"}
		if in.Preflight == nil {
			v.State = "unknown"
			v.Detail = "no preflight available for this payload"
		} else if res := in.Preflight(ctx); res.OK {
			v.State = "pass"
		} else {
			msgs := make([]string, len(res.Errors))
			for i, e := range res.Errors {
				msgs[i] = e.Check + ": " + e.Message
			}
			v.State = "fail"
			v.Detail = strings.Join(msgs, "; ")
		}
		report.Verdicts = append(report.Verdicts, v)
	}

	// Gate F — planned-volume collapse floor (60% of yesterday, orchestrator
	// N1 parity). Uncapped payloads are audience-bound → exempt by doctrine.
	{
		v := sendDayGateVerdict{Gate: "F", Name: "Volume reconciliation"}
		if in.Uncapped {
			v.State = "pass"
			v.Detail = "audience-bound payload (all quotas volume=0) — uncapped engaged-tier doctrine; collapse floor not applicable"
		} else {
			mdt, _ := time.LoadLocation("America/Denver")
			anchor := in.TargetDay
			if anchor.IsZero() {
				anchor = time.Now()
			}
			today := startOfMDTSendDay(anchor)
			yesterday := today.In(mdt).AddDate(0, 0, -1)
			todayPlanned, errT := sumPlannedRecipientsForMDTDate(ctx, db, today)
			yesterdayPlanned, errY := sumPlannedRecipientsForMDTDate(ctx, db, yesterday)
			switch {
			case errT != nil || errY != nil:
				v.State = "unknown"
				v.Detail = fmt.Sprintf("planned-volume query failed (today: %v, yesterday: %v)", errT, errY)
			case yesterdayPlanned == 0:
				v.State = "unknown"
				v.Detail = "no yesterday baseline — planned volume cannot be reconciled; stage the full board first or override with a reason"
			case float64(todayPlanned) >= 0.6*float64(yesterdayPlanned):
				v.State = "pass"
				v.Detail = fmt.Sprintf("planned %d vs yesterday %d (%.0f%%)", todayPlanned, yesterdayPlanned, 100*float64(todayPlanned)/float64(yesterdayPlanned))
			default:
				v.State = "fail"
				v.Detail = fmt.Sprintf("planned %d vs yesterday %d (%.0f%%) — below the 60%% collapse floor (2026-06-29 class); stage the full board as drafts first, or override with a reason", todayPlanned, yesterdayPlanned, 100*float64(todayPlanned)/float64(yesterdayPlanned))
			}
		}
		report.Verdicts = append(report.Verdicts, v)
	}

	return report
}

// startOfMDTSendDay returns midnight of the supplied instant's calendar day
// in America/Denver — the canonical send-day boundary (see file header).
// Falls back to UTC if the tz database is unavailable (containers ship
// tzdata; belt-and-braces only).
func startOfMDTSendDay(now time.Time) time.Time {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
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
