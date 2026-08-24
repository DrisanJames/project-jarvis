package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

const (
	pmtaExecutionModeStandard = "standard"
	pmtaExecutionModeWave     = "pmta_isp_wave"

	// Mandatory throttle defaults. Every campaign MUST be spread across a
	// delivery window — single-wave blasts are never acceptable.
	defaultThrottleDuration = 8 * time.Hour
	defaultCadenceMinutes   = 15
	minWavesPerISP          = 4
)

// resolveOfferID looks up the offer_id linked to a campaign. Returns "" if
// the campaign has no offer or the ID is empty/invalid.
func resolveOfferID(ctx context.Context, db dbQuerier, campaignID string) string {
	if campaignID == "" {
		return ""
	}
	var oid sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT offer_id::text FROM mailing_campaigns WHERE id = $1 AND offer_id IS NOT NULL`,
		campaignID).Scan(&oid)
	if err != nil || !oid.Valid || oid.String == "" || oid.String == "00000000-0000-0000-0000-000000000000" {
		return ""
	}
	return oid.String
}

// contentLockResolver abstracts the minimum query surface needed so the helper
// can be driven by either *sql.Tx or *sql.DB without pulling in dbQuerier's
// full QueryContext requirement.
type contentLockResolver interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// resolveContentLocked decides the campaign's content_locked value at deploy
// time. Precedence:
//  1. Explicit payload override (non-nil) — used verbatim.
//  2. Offer-level default — looked up by offer_id if present in the payload.
//  3. nil — caller should fall through to the column's DB default (FALSE).
//
// Content-locked campaigns bypass subject/HTML fingerprint mutations at wave
// dispatch so strict advertisers (e.g. TruGreen) receive byte-faithful
// creative. See worker/pmta_wave_dispatcher.go EnqueuePMTAWave.
func resolveContentLocked(ctx context.Context, q contentLockResolver, explicit *bool, offerID string) *bool {
	if explicit != nil {
		v := *explicit
		return &v
	}
	if offerID == "" || offerID == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	var locked sql.NullBool
	if err := q.QueryRowContext(ctx,
		`SELECT content_locked FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&locked); err != nil {
		return nil
	}
	if !locked.Valid {
		return nil
	}
	v := locked.Bool
	return &v
}

// dbQuerier abstracts the common query methods shared by *sql.DB and *sql.Conn
// so that callers can pass a dedicated connection with custom session settings
// (e.g. extended statement_timeout) without changing the query logic.
type dbQuerier interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

type pmtaNormalizedTimeSpan struct {
	StartAt  time.Time
	EndAt    time.Time
	Timezone string
	Source   string
}

type pmtaNormalizedPlan struct {
	ISP               string
	Quota             int
	RandomizeAudience bool
	ThrottleStrategy  string
	Timezone          string
	Cadence           engine.PMTACadenceInput
	TimeSpans         []pmtaNormalizedTimeSpan
}

type pmtaNormalizedCampaign struct {
	SendMode      string
	TargetISPs    []engine.ISP
	Plans         []pmtaNormalizedPlan
	LegacyInput   bool
	Assumptions   []string
	EarliestStart time.Time
}

type pmtaSelectedRecipient struct {
	SubscriberID  string
	Email         string
	ISP           string
	SourceType    string
	SourceID      string
	SelectionRank int

	// IsReserve marks a recipient as belonging to the cap-aware reserve
	// pool — over-selected beyond plan.Quota so the wave dispatcher can
	// substitute capped subscribers with the next available reserve. See
	// reservePoolMultiplier and the planner truncation site. When false,
	// the row writes to mailing_campaign_plan_recipients with
	// status='selected'; when true, status='reserve'. Defaults to false
	// when the kill switch DISABLE_RESERVE_POOL is on (no over-select).
	IsReserve bool

	// Per-domain engagement priority hints (set by qualifyEmail when
	// SDS state is available; used by the post-streaming sort that
	// reorders each ISP's pool before quota truncation). Lowercase so
	// callers outside this package can't depend on them — they are an
	// in-process implementation detail of the audience finalizer.
	sdsCrossEngaged bool
	sdsStateRank    int       // 1=engaged, 2=probe, 3=cold, 4=suppressed
	sdsLastEngageAt time.Time // max(last_open_at, last_click_at) for tie-break
}

type pmtaAudiencePlan struct {
	RecipientsByISP map[string][]pmtaSelectedRecipient
	// CountsByISP reports the count of NON-reserve (selected) recipients
	// per ISP. Wave planning, isp_plans.audience_selected_count, and
	// every downstream consumer that pre-dates the reserve pool reads
	// this field — it MUST exclude reserves to keep wave sizing correct.
	CountsByISP map[string]int
	// ReserveCountsByISP reports the count of reserve recipients per ISP.
	// Persistence layer stamps mailing_campaign_isp_plans.audience_reserve_count
	// from this map. When DISABLE_RESERVE_POOL=true, every entry is 0.
	ReserveCountsByISP map[string]int
	TotalSeen          int
	AfterSuppression   int
	SelectedTotal      int
	// ReserveTotal is the sum of ReserveCountsByISP — convenience for
	// log lines and audit dashboards. Always equals 0 under the kill switch.
	ReserveTotal int
	// SuppressionReasons buckets every dropped candidate by reason
	// (dedup_or_empty, isp_not_allowed, isp_quota_cap, offer_suppression,
	// global_or_brand_suppression, exclusion_list, excluded_segment,
	// recently_mailed, sds_cold_suppressed, sds_recent_24h). Powers the
	// per-campaign audience funnel (targeted vs suppressed by reason).
	SuppressionReasons map[string]int
	// PlanWarnings carries structured, per-source planning warnings
	// (REQ-043: a 0-member inclusion/exclusion segment was previously a
	// server-log-only skip). Persisted onto the campaign's plan record by
	// persistAudienceFunnel (mailing_campaign_audience_funnel.plan_warnings)
	// so the Draft Board and QA sweeps can see "segment X contributed 0".
	PlanWarnings []pmtaPlanWarning
}

// pmtaPlanWarning is one structured planning warning. Deduplicated per plan
// run on (Code, Scope, SegmentID) and persisted wholesale on every finalize
// (the funnel row is an ON CONFLICT (campaign_id) DO UPDATE upsert), so a
// scheduler re-fire / re-finalization replaces the set instead of
// accumulating duplicates.
type pmtaPlanWarning struct {
	Code      string `json:"code"`       // e.g. planWarnSegmentZeroMembers
	Scope     string `json:"scope"`      // "inclusion" | "exclusion"
	SegmentID string `json:"segment_id"` // the mailing_segments id that contributed 0
	Detail    string `json:"detail"`     // ledger-derived: never built / build failed / built empty / ...
}

// planWarnSegmentZeroMembers: a named segment contributed 0 recipients
// (inclusion) or 0 exclusion emails (exclusion) at plan time.
const planWarnSegmentZeroMembers = "segment_zero_members"

// planWarnReserveShortfall (REQ-C17): a segment_reserves entry accepted fewer
// recipients than its declared reserve during the reserved-first pass. The
// plan proceeds (unfilled seats fall through to the remaining sources — never
// a mid-flight 0-recipient hard failure), but the shortfall is LOUD: this
// structured warning persists with the plan, and the promote post-conditions
// verify endpoint FAILs the campaign's reserve_fill check against it.
const planWarnReserveShortfall = "reserve_shortfall"

// segmentBuildFailClosedDisabled is the REQ-043 kill switch. Set
// DISABLE_SEGMENT_BUILD_FAIL_CLOSED=true to revert a 0-member EXCLUSION
// segment with no verified successful build to the legacy warn-and-skip
// behavior (structured plan warnings are still recorded — only the
// fail-closed enforcement is neutralized). Tier-risky enforcement changes on
// the send path always carry an env kill switch (REQ-007 lesson).
func segmentBuildFailClosedDisabled() bool {
	return os.Getenv("DISABLE_SEGMENT_BUILD_FAIL_CLOSED") == "true"
}

// rotatingAudienceSelectionEnabled controls whether the cold-pool / cold-
// ramp recipient selection ROTATES daily instead of picking the same
// members every day. It is the kill switch for the day-salted stripe added
// to the master-selection cold-fallback ORDER BY (streamSDSColdFallback).
//
// Default ON (rotation active). Set DISABLE_ROTATING_AUDIENCE_SELECTION=true
// to revert the cold-fallback ordering to its prior day-stable form
// (hashtext(sub.id::text || $domain)), i.e. the exact pre-rotation behavior.
// This is the one-move rollback for the rotation change.
//
// WHY: the cold-fallback selects first-touch subscribers (no SDS row for
// this sending domain). Its prior ORDER BY hashed only (subscriber,domain),
// which is STABLE across days, so the same members sorted to the front of
// the pool every day and the same quota-sized prefix was drawn each send —
// FIFO-equivalent. Fresh members deeper in the pool were never tried. The
// day salt (Denver calendar day) rotates the stripe daily while remaining
// stable/reproducible WITHIN a day, and keeps the per-(subscriber,domain)
// disjoint-brand property (still keyed on the sending domain) intact.
func rotatingAudienceSelectionEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("DISABLE_ROTATING_AUDIENCE_SELECTION"))) != "true"
}

// recencyAudienceDrawEnabled — operator ruling 2026-08-24 ("perform this
// recency approach"): a CAPPED inclusion-segment draw fills MOST-RECENTLY
// ENGAGED FIRST (SDS last_click_at/last_open_at for THIS sending domain),
// replacing the daily-rotating hash order as the default. The accepted
// trade-off is explicit: the same most-recent members are drawn every day
// until they age past the window — the repetition the rotation existed to
// prevent is now the intended behavior for engagement-window segments.
// Kill switch: RECENCY_AUDIENCE_DRAW_DISABLED=true falls back to rotation.
func recencyAudienceDrawEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv("RECENCY_AUDIENCE_DRAW_DISABLED"))) != "true"
}

// segmentBuildLedgerState classifies a segment that read 0 rows from
// mailing_segment_members using mailing_segment_build_ledger (written by
// every member-writing build path — see segment_ledger.go).
//
// builtEmpty=true means "built and genuinely empty": the ledger has a
// successful build (last_built_at IS NOT NULL), its most recent status is
// 'ok' (a stale 'running' row is coerced to 'failed', matching
// coerceLedgerStatus), and the last good build recorded 0 members.
// Everything else — no ledger row, never-successful build, failed/blocked
// last build, a ledger count that disagrees with the 0 readable members
// (membership wiped after a good build), or a ledger read error — returns
// builtEmpty=false with a detail string saying why.
//
// NOTE: segment_ledger.go declares the ledger "observability, not control
// flow" for BUILDERS (a build must never fail on a ledger write). REQ-043
// deliberately promotes ledger READS to control flow on the exclusion path:
// it is the only signal that distinguishes "never built" (fail-closed —
// an unbuilt exclusion silently widens the audience) from "built empty"
// (legitimately excludes nobody).
func segmentBuildLedgerState(ctx context.Context, db dbQuerier, segmentID string) (builtEmpty bool, detail string) {
	var builtAt, updatedAt sql.NullTime
	var status sql.NullString
	var count sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT last_built_at, last_build_status, updated_at, subscriber_count
		FROM mailing_segment_build_ledger
		WHERE segment_id = $1::uuid
	`, segmentID).Scan(&builtAt, &status, &updatedAt, &count)
	if err == sql.ErrNoRows {
		return false, "never built (no build-ledger row)"
	}
	if err != nil {
		return false, fmt.Sprintf("build-ledger read error: %v", err)
	}
	if !builtAt.Valid {
		return false, fmt.Sprintf("never successfully built (last attempt status=%s)", strings.TrimSpace(status.String))
	}
	effective := coerceLedgerStatus(status.String, updatedAt.Time, time.Now())
	if effective != "ok" {
		return false, fmt.Sprintf("last build status=%s (last good build %s)", effective, builtAt.Time.UTC().Format(time.RFC3339))
	}
	if count.Valid && count.Int64 > 0 {
		return false, fmt.Sprintf("build ledger reports %d members from the last good build but 0 are readable (membership wiped after build?)", count.Int64)
	}
	return true, fmt.Sprintf("built and genuinely empty (last good build %s)", builtAt.Time.UTC().Format(time.RFC3339))
}

// reservePoolMultiplier returns the over-select factor applied to each
// ISP's plan.Quota at audience-finalize time. Default 1.5 (i.e. keep
// quota + 50% as reserves). Env override: RESERVE_POOL_MULTIPLIER (must
// be >= 1.0; values below 1.0 are clamped to 1.0 to prevent the over-
// select path from accidentally REDUCING the audience).
//
// Set DISABLE_RESERVE_POOL=true to revert the planner to its pre-reserve
// behavior (hard truncation at plan.Quota). The kill switch makes the
// multiplier effectively 1.0 even if RESERVE_POOL_MULTIPLIER is set.
func reservePoolMultiplier() float64 {
	if os.Getenv("DISABLE_RESERVE_POOL") == "true" {
		return 1.0
	}
	raw := strings.TrimSpace(os.Getenv("RESERVE_POOL_MULTIPLIER"))
	if raw == "" {
		return 1.5
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 1.0 {
		return 1.5
	}
	return v
}

type pmtaWaveSpec struct {
	WaveNumber        int
	ScheduledAt       time.Time
	WindowStartAt     time.Time
	WindowEndAt       time.Time
	CadenceMinutes    int
	BatchSize         int
	PlannedRecipients int
	IdempotencyKey    string
}

func normalizePMTACampaignInput(input engine.PMTACampaignInput) (pmtaNormalizedCampaign, error) {
	now := time.Now().UTC()
	sendMode := input.SendMode
	if sendMode == "" {
		sendMode = "immediate"
	}
	if sendMode != "immediate" && sendMode != "scheduled" {
		return pmtaNormalizedCampaign{}, fmt.Errorf("send_mode must be 'immediate' or 'scheduled'")
	}

	// Segment reserves (REQ-C17): validate at the door so a malformed
	// reservation is a 400 at deploy, never a silent no-op at plan time.
	// Absent/empty slice = zero iterations = today's behavior untouched.
	for i, sr := range input.SegmentReserves {
		if strings.TrimSpace(sr.SegmentID) == "" {
			return pmtaNormalizedCampaign{}, fmt.Errorf("segment_reserves[%d]: segment_id is required", i)
		}
		if sr.Reserve <= 0 {
			return pmtaNormalizedCampaign{}, fmt.Errorf("segment_reserves[%d] (segment %s): reserve must be > 0", i, sr.SegmentID)
		}
	}

	defaultTZ := strings.TrimSpace(input.Timezone)
	if defaultTZ == "" {
		defaultTZ = "UTC"
	}
	defaultThrottle := strings.TrimSpace(input.ThrottleStrategy)
	if defaultThrottle == "" {
		defaultThrottle = "auto"
	}

	normalized := pmtaNormalizedCampaign{
		SendMode:    sendMode,
		LegacyInput: len(input.ISPPlans) == 0,
	}

	pastDueRecovery := false
	if sendMode == "scheduled" && input.ScheduledAt != nil {
		scheduledUTC := input.ScheduledAt.UTC()
		if scheduledUTC.Before(now) {
			log.Printf("[normalizePMTA] WARNING: scheduled_at %s is in the past — recovering as immediate send", scheduledUTC.Format(time.RFC3339))
			recoveredAt := now.Add(2 * time.Minute)
			input.ScheduledAt = &recoveredAt
			sendMode = "immediate"
			pastDueRecovery = true
		} else if scheduledUTC.Before(now.Add(5 * time.Minute)) {
			log.Printf("[normalizePMTA] WARNING: scheduled_at %s is < 5 min away — recovering as immediate send", scheduledUTC.Format(time.RFC3339))
			recoveredAt := now.Add(2 * time.Minute)
			input.ScheduledAt = &recoveredAt
			sendMode = "immediate"
			pastDueRecovery = true
		}
	}

	if len(input.ISPPlans) == 0 {
		if len(input.TargetISPs) == 0 {
			return pmtaNormalizedCampaign{}, fmt.Errorf("at least one target ISP is required")
		}
		baseStart := now
		if sendMode == "scheduled" {
			if input.ScheduledAt == nil {
				return pmtaNormalizedCampaign{}, fmt.Errorf("scheduled_at is required when send_mode is 'scheduled'")
			}
			baseStart = input.ScheduledAt.UTC()
		} else if pastDueRecovery {
			baseStart = input.ScheduledAt.UTC()
		}
		quotaMap := make(map[string]int, len(input.ISPQuotas))
		for _, q := range input.ISPQuotas {
			if q.Volume > 0 {
				quotaMap[strings.ToLower(strings.TrimSpace(q.ISP))] = q.Volume
			}
		}
		for _, isp := range input.TargetISPs {
			ispName := strings.ToLower(strings.TrimSpace(string(isp)))
			if ispName == "" {
				continue
			}
			normalized.TargetISPs = append(normalized.TargetISPs, engine.ISP(ispName))
			normalized.Plans = append(normalized.Plans, pmtaNormalizedPlan{
				ISP:               ispName,
				Quota:             quotaMap[ispName],
				RandomizeAudience: input.RandomizeAudience,
				ThrottleStrategy:  defaultThrottle,
				Timezone:          defaultTZ,
				Cadence: engine.PMTACadenceInput{
					Mode:         "interval",
					EveryMinutes: defaultCadenceMinutes,
					BatchSize:    0, // auto-calculated in buildPMTAWaveSpecs
				},
				TimeSpans: []pmtaNormalizedTimeSpan{{
					StartAt:  baseStart,
					EndAt:    baseStart.Add(defaultThrottleDuration),
					Timezone: defaultTZ,
					Source:   "legacy_throttle_window",
				}},
			})
		}
		normalized.Assumptions = append(normalized.Assumptions,
			"Translated legacy PMTA wizard input into one ISP plan per selected ISP.",
			"Applied the campaign-level schedule to every generated ISP plan.",
		)
	} else {
		targetSeen := make(map[string]bool)
		for _, rawPlan := range input.ISPPlans {
			plan, err := normalizeISPPlan(rawPlan, sendMode, defaultTZ, defaultThrottle, input.ScheduledAt, now)
			if err != nil {
				return pmtaNormalizedCampaign{}, err
			}
			if targetSeen[plan.ISP] {
				continue // deduplicate — take the first plan per ISP
			}
			normalized.Plans = append(normalized.Plans, plan)
			targetSeen[plan.ISP] = true
			if isCanonicalISP(plan.ISP) || plan.ISP == "other" {
				normalized.TargetISPs = append(normalized.TargetISPs, engine.ISP(plan.ISP))
			}
		}
		if len(normalized.Plans) == 0 {
			return pmtaNormalizedCampaign{}, fmt.Errorf("at least one isp_plan is required")
		}
		if len(normalized.TargetISPs) == 0 && len(input.TargetISPs) > 0 {
			normalized.TargetISPs = append(normalized.TargetISPs, input.TargetISPs...)
		}
	}

	if pastDueRecovery {
		normalized.SendMode = sendMode
	}

	if len(normalized.Plans) == 0 {
		return pmtaNormalizedCampaign{}, fmt.Errorf("at least one ISP plan is required")
	}

	normalized.EarliestStart = normalized.Plans[0].TimeSpans[0].StartAt
	for _, plan := range normalized.Plans {
		for _, span := range plan.TimeSpans {
			if span.StartAt.Before(normalized.EarliestStart) {
				normalized.EarliestStart = span.StartAt
			}
		}
	}

	return normalized, nil
}

func normalizeISPPlan(
	raw engine.PMTAISPScheduleInput,
	sendMode, defaultTZ, defaultThrottle string,
	legacyScheduledAt *time.Time,
	now time.Time,
) (pmtaNormalizedPlan, error) {
	isp := strings.ToLower(strings.TrimSpace(raw.ISP))
	if isp == "" {
		return pmtaNormalizedPlan{}, fmt.Errorf("isp_plan isp is required")
	}

	timezone := strings.TrimSpace(raw.Timezone)
	if timezone == "" {
		timezone = defaultTZ
	}
	cadence := raw.Cadence
	// Force interval mode — single-wave blasts are never allowed.
	if cadence.Mode == "" || cadence.Mode == "single" {
		cadence.Mode = "interval"
	}
	if cadence.Mode != "interval" {
		return pmtaNormalizedPlan{}, fmt.Errorf("isp_plan cadence mode for %s must be 'interval'", isp)
	}
	if cadence.EveryMinutes <= 0 {
		cadence.EveryMinutes = defaultCadenceMinutes
	}
	if cadence.BatchSize < 0 {
		return pmtaNormalizedPlan{}, fmt.Errorf("isp_plan batch_size for %s must be >= 0", isp)
	}

	throttleStrategy := strings.TrimSpace(raw.ThrottleStrategy)
	if throttleStrategy == "" {
		throttleStrategy = defaultThrottle
	}

		spans, err := normalizeTimeSpans(raw.TimeSpans, sendMode, timezone, legacyScheduledAt, now)
		if err != nil {
			return pmtaNormalizedPlan{}, err
		}

	return pmtaNormalizedPlan{
		ISP:               isp,
		Quota:             raw.Quota,
		RandomizeAudience: raw.RandomizeAudience,
		ThrottleStrategy:  throttleStrategy,
		Timezone:          timezone,
		Cadence:           cadence,
		TimeSpans:         spans,
	}, nil
}

func normalizeTimeSpans(
	rawSpans []engine.PMTATimeSpanInput,
	sendMode, timezone string,
	legacyScheduledAt *time.Time,
	now time.Time,
) ([]pmtaNormalizedTimeSpan, error) {
	if len(rawSpans) == 0 {
		startAt := now
		if sendMode == "scheduled" && legacyScheduledAt != nil {
			startAt = legacyScheduledAt.UTC()
		}
		return []pmtaNormalizedTimeSpan{{
			StartAt:  startAt,
			EndAt:    startAt.Add(defaultThrottleDuration),
			Timezone: timezone,
			Source:   "default_throttle_window",
		}}, nil
	}

	spans := make([]pmtaNormalizedTimeSpan, 0, len(rawSpans))
	for _, raw := range rawSpans {
		switch raw.Type {
		case "", "absolute":
			if raw.StartAt == nil {
				return nil, fmt.Errorf("absolute time span requires start_at")
			}
			startAt := raw.StartAt.UTC()
			endAt := startAt
			if raw.EndAt != nil {
				endAt = raw.EndAt.UTC()
			}
			if sendMode == "scheduled" && startAt.Before(now) {
				return nil, fmt.Errorf("scheduled time span start_at (%s) is in the past", startAt.Format(time.RFC3339))
			}
			if endAt.Before(startAt) {
				return nil, fmt.Errorf("time span end_at must be after start_at")
			}
			spans = append(spans, pmtaNormalizedTimeSpan{
				StartAt:  startAt,
				EndAt:    endAt,
				Timezone: coalesceString(raw.Timezone, timezone),
				Source:   coalesceString(raw.Source, "manual"),
			})
		case "weekly":
			if raw.StartHour == nil || raw.EndHour == nil {
				return nil, fmt.Errorf("weekly time span requires start_hour and end_hour")
			}
			startAt, endAt, err := resolveWeeklyTimeSpan(raw.DayOfWeek, *raw.StartHour, *raw.EndHour, coalesceString(raw.Timezone, timezone), now)
			if err != nil {
				return nil, err
			}
			spans = append(spans, pmtaNormalizedTimeSpan{
				StartAt:  startAt,
				EndAt:    endAt,
				Timezone: coalesceString(raw.Timezone, timezone),
				Source:   coalesceString(raw.Source, "weekly"),
			})
		default:
			return nil, fmt.Errorf("unsupported time span type %q", raw.Type)
		}
	}

	return spans, nil
}

func resolveWeeklyTimeSpan(dayOfWeek string, startHour, endHour int, timezone string, now time.Time) (time.Time, time.Time, error) {
	if dayOfWeek == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("weekly time span requires day_of_week")
	}
	if startHour < 0 || startHour > 23 {
		return time.Time{}, time.Time{}, fmt.Errorf("start_hour must be between 0 and 23")
	}
	if endHour < 0 || endHour > 24 {
		return time.Time{}, time.Time{}, fmt.Errorf("end_hour must be between 0 and 24")
	}
	if endHour < startHour {
		return time.Time{}, time.Time{}, fmt.Errorf("end_hour must be >= start_hour")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	localNow := now.In(loc)
	targetDay, ok := weekdayIndex(dayOfWeek)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported day_of_week %q", dayOfWeek)
	}
	daysUntil := (targetDay - int(localNow.Weekday()) + 7) % 7
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), startHour, 0, 0, 0, loc)
	if daysUntil == 0 && !start.After(localNow) {
		daysUntil = 7
	}
	start = start.AddDate(0, 0, daysUntil)
	end := time.Date(start.Year(), start.Month(), start.Day(), endHour, 0, 0, 0, loc)
	return start.UTC(), end.UTC(), nil
}

func weekdayIndex(day string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(day)) {
	case "sunday":
		return 0, true
	case "monday":
		return 1, true
	case "tuesday":
		return 2, true
	case "wednesday":
		return 3, true
	case "thursday":
		return 4, true
	case "friday":
		return 5, true
	case "saturday":
		return 6, true
	default:
		return 0, false
	}
}

func planPMTAAudience(
	ctx context.Context,
	db dbQuerier,
	orgID string,
	input engine.PMTACampaignInput,
	normalized pmtaNormalizedCampaign,
	suppMatcher *SuppressionMatcher,
	globalHub *engine.GlobalSuppressionHub,
	offerSuppMgr ...*OfferSuppressionManager,
) (pmtaAudiencePlan, error) {
	planStart := time.Now()
	log.Printf("[PlanAudience] starting for campaign %s (%d inclusion lists, %d ISP plans)", input.CampaignID, len(input.InclusionLists), len(normalized.Plans))

	// Resolve offer ID: explicit field takes precedence, otherwise look up from the campaign record
	offerID := input.OfferID
	if offerID == "" {
		offerID = resolveOfferID(ctx, db, input.CampaignID)
	}

	// Offer suppression: prefer in-memory Bloom filter (O(1) per email),
	// fall back to loading subscriber IDs from DB only if no Bloom available.
	var offerSuppMgrRef *OfferSuppressionManager
	if len(offerSuppMgr) > 0 {
		offerSuppMgrRef = offerSuppMgr[0]
	}
	// A Bloom is only usable if one is actually loaded for THIS offer —
	// the manager being non-nil isn't enough. Offers without a loaded Bloom
	// (no Optizmo sync, or load failure) must use the DB set, otherwise
	// their offer-level suppressions are silently skipped at planning time.
	useBloomForOffer := offerID != "" && offerSuppMgrRef != nil && offerSuppMgrRef.HasBloom(offerID)
	offerSuppSet := make(map[string]bool)
	if offerID != "" && !useBloomForOffer {
		osRows, osErr := db.QueryContext(ctx,
			`SELECT subscriber_id::text FROM mailing_offer_suppressions WHERE offer_id = $1`, offerID)
		if osErr == nil {
			defer osRows.Close()
			for osRows.Next() {
				var sid string
				if osRows.Scan(&sid) == nil {
					offerSuppSet[sid] = true
				}
			}
		}
		if len(offerSuppSet) > 0 {
			log.Printf("[PlanAudience] loaded %d offer-level suppressions for offer %s (DB fallback)", len(offerSuppSet), offerID)
		}
	} else if useBloomForOffer {
		log.Printf("[PlanAudience] using Bloom filter for offer %s suppression checks", offerID)
	}
	log.Printf("[PlanAudience] offer suppressions loaded in %v", time.Since(planStart))

	// Derive brand root once for this campaign — the planner iterates over
	// millions of candidates in qualifyEmail, so computing this per-email
	// would burn CPU on a constant value. Empty brand root collapses brand
	// checks to pure global checks for unknown sending domains.
	campaignBrandRoot := brand.Root(input.SendingDomain)
	log.Printf("[PlanAudience] using GlobalSuppressionHub (in-memory, hub=%v, brand_root=%q) at %v", globalHub != nil, campaignBrandRoot, time.Since(planStart))

	exclusionIDs, resolveErr := resolveListNamesToIDs(ctx, db, orgID, input.ExclusionLists)
	if resolveErr != nil {
		return pmtaAudiencePlan{}, fmt.Errorf("resolve exclusion lists: %w", resolveErr)
	}
	for _, slID := range exclusionIDs {
		// FAIL CLOSED (REQ-002): a campaign that names an exclusion list must
		// never plan without it. A load error fails the plan (the finalizer
		// marks the campaign 'failed' with this reason); an EMPTY list is
		// fine — zero hashes simply exclude nobody.
		slRows, slErr := db.QueryContext(ctx, "SELECT md5_hash FROM mailing_suppression_entries WHERE list_id = $1", slID)
		if slErr != nil {
			return pmtaAudiencePlan{}, fmt.Errorf("load exclusion suppression list %s: %w (fail-closed: campaign must not plan without its exclusions)", slID, slErr)
		}
		var hashes []string
		for slRows.Next() {
			var h string
			if slRows.Scan(&h) == nil {
				hashes = append(hashes, h)
			}
		}
		rowsErr := slRows.Err()
		slRows.Close()
		if rowsErr != nil {
			return pmtaAudiencePlan{}, fmt.Errorf("read exclusion suppression list %s: %w (fail-closed: partial exclusion load)", slID, rowsErr)
		}
		if len(hashes) > 0 {
			suppMatcher.LoadList(slID, hashes)
		}
	}
	log.Printf("[PlanAudience] exclusion lists loaded (%d lists) in %v", len(exclusionIDs), time.Since(planStart))

	// Structured plan warnings (REQ-043). Deduplicated on
	// (code, scope, segment_id) so a segment reachable through BOTH
	// send_priority and inclusion_segments (streamSegment fires twice)
	// records its zero-members warning once.
	var planWarnings []pmtaPlanWarning
	planWarnSeen := make(map[string]bool)
	addPlanWarning := func(w pmtaPlanWarning) {
		key := w.Code + "|" + w.Scope + "|" + w.SegmentID
		if planWarnSeen[key] {
			return
		}
		planWarnSeen[key] = true
		planWarnings = append(planWarnings, w)
	}

	exclusionSegEmails, exclusionSegWarnings, err := loadExclusionSegmentEmails(ctx, db, input.ExclusionSegments)
	if err != nil {
		return pmtaAudiencePlan{}, err
	}
	for _, w := range exclusionSegWarnings {
		addPlanWarning(w)
	}

	// Per-domain engagement engine — SA-2 (per-domain frequency cap +
	// state filter + cross-engaged prioritization).
	//
	// Two bulk reads, both soft-fail: a query error here logs a warning
	// and returns an empty map so the planner continues on the legacy
	// path. The SQL-injected list-path filter (see streamList below)
	// remains independently active even if these maps are empty.
	//
	// Maps are local to this finalize call — no goroutine sharing — so
	// no synchronization is needed for the lookups inside qualifyEmail.
	sdsFilterOn := sdsFilterEnabled() && strings.TrimSpace(input.SendingDomain) != ""
	sdsState := loadSDSStateForDomain(ctx, db, input.SendingDomain)
	crossEngagedIDs := loadCrossEngagedSet(ctx, db)
	if sdsFilterOn {
		log.Printf("[finalizer/sds] domain=%q loaded sds_state_rows=%d cross_engaged=%d in %v",
			input.SendingDomain, len(sdsState), len(crossEngagedIDs), time.Since(planStart))
	}
	// Per-finalize observability counters. These count the gross effect
	// of the SDS filter across all qualifyEmail invocations; the totals
	// land in the [finalizer] log line at the bottom of the function.
	var sdsFilteredRecent24h, sdsFilteredCold int
	var sdsSelectedEngaged, sdsSelectedProbe int

	// Remail gap: load emails that received a send within the last MinRemailHours.
	// Only enforced for list-sourced (cold) subscribers, not segment openers.
	// NOTE: this query uses the main db connection (passed as `db`) with a generous
	// timeout. Previously a 30s sub-context was used, but when it expired the Go
	// pq driver cancelled the query on the dedicated *sql.Conn, which poisoned the
	// connection for all subsequent operations ("sql: connection is already closed").
	recentlyMailed := map[string]bool{}
	if input.MinRemailHours > 0 {
		sendingDomains := deriveSendingDomainVariants(input.SendingDomain)
		domainParams := make([]string, len(sendingDomains))
		for i := range sendingDomains {
			domainParams[i] = fmt.Sprintf("$%d", i+2)
		}
		rmQuery := fmt.Sprintf(`
			SELECT DISTINCT LOWER(s.email)
			FROM mailing_tracking_events te
			JOIN mailing_subscribers s ON s.id = te.subscriber_id
			WHERE te.event_type IN ('sent','delivered')
			  AND te.sending_domain IN (%s)
			  AND te.event_at >= NOW() - make_interval(hours => $1)
		`, strings.Join(domainParams, ","))
		rmArgs := make([]any, 0, 1+len(sendingDomains))
		rmArgs = append(rmArgs, input.MinRemailHours)
		for _, d := range sendingDomains {
			rmArgs = append(rmArgs, d)
		}
		rmRows, rmErr := db.QueryContext(ctx, rmQuery, rmArgs...)
		if rmErr == nil {
			for rmRows.Next() {
				var email string
				if rmRows.Scan(&email) == nil {
					recentlyMailed[email] = true
				}
			}
			rmRows.Close()
		} else {
			// FAIL CLOSED (REQ-002): the operator explicitly asked for a
			// remail gap (MinRemailHours > 0). Planning without it would
			// silently re-mail recently-touched addresses.
			return pmtaAudiencePlan{}, fmt.Errorf("load recently-mailed set for min_remail_hours=%d: %w (fail-closed: remail gap cannot be enforced)", input.MinRemailHours, rmErr)
		}
		log.Printf("[PlanAudience] loaded %d recently-mailed emails (last %dh, domains=%v) in %v",
			len(recentlyMailed), input.MinRemailHours, sendingDomains, time.Since(planStart))
	}

	allowedISPs := make(map[string]bool, len(normalized.Plans))
	planMap := make(map[string]pmtaNormalizedPlan, len(normalized.Plans))
	ispQuota := make(map[string]int, len(normalized.Plans))
	// ispReservePool is the inflated streaming target — plan.Quota * reserveMult.
	// The early-cutoff function and the per-ISP qualification gate both consult
	// this map so we keep accepting rows past the plain quota up to the reserve
	// ceiling. Without this the planner would short-circuit at quota and the
	// over-select in the truncation block would never see any extra rows.
	streamReserveMult := reservePoolMultiplier()
	ispReservePool := make(map[string]int, len(normalized.Plans))
	for _, plan := range normalized.Plans {
		allowedISPs[plan.ISP] = true
		planMap[plan.ISP] = plan
		ispQuota[plan.ISP] = plan.Quota // 0 = unlimited
		if plan.Quota > 0 && streamReserveMult > 1.0 {
			pool := int(float64(plan.Quota) * streamReserveMult)
			if pool < plan.Quota {
				pool = plan.Quota
			}
			ispReservePool[plan.ISP] = pool
		} else {
			ispReservePool[plan.ISP] = plan.Quota
		}
	}

	inclusionIDs, inclErr := resolveListNamesToIDs(ctx, db, orgID, input.InclusionLists)
	if inclErr != nil {
		return pmtaAudiencePlan{}, fmt.Errorf("resolve inclusion lists: %w", inclErr)
	}
	var qualified []pmtaSelectedRecipient
	seenEmails := make(map[string]bool)
	selectionRank := 0
	qualifiedPerISP := make(map[string]int, len(normalized.Plans))

	// Audience funnel: count every candidate dropped, bucketed by reason, so
	// the operator gets first-class "targeted vs suppressed (by reason)"
	// optics without per-recipient event volume. Persisted as a single
	// aggregate row per campaign by the finalizer. Cheap: in-memory int map.
	suppressionReasons := make(map[string]int)
	deny := func(reason string) bool {
		suppressionReasons[reason]++
		return false
	}

	// Async ISP backfill: collect subscriber IDs that need ISP set, flush in background.
	var ispBackfill []struct{ ID, ISP string }

	allQuotasMet := func() bool {
		hasBounded := false
		for isp, q := range ispQuota {
			if q <= 0 {
				continue // unlimited ISP — always "met" for cutoff purposes
			}
			hasBounded = true
			// Target the inflated reserve pool ceiling so we keep
			// streaming past the raw quota and pick up reserve
			// candidates. Falls back to q if reserves are disabled.
			target := ispReservePool[isp]
			if target < q {
				target = q
			}
			if qualifiedPerISP[isp] < target {
				return false
			}
		}
		return hasBounded // false if ALL ISPs are unlimited (must scan everything)
	}

	qualifyEmail := func(subID, email, sourceType, sourceID string) bool {
		emailLower := strings.ToLower(strings.TrimSpace(email))
		if emailLower == "" || seenEmails[emailLower] {
			return deny("dedup_or_empty")
		}
		seenEmails[emailLower] = true

		// ISP classification + quota check FIRST (cheap string ops, avoids
		// expensive MD5/suppression work for quota-met ISPs)
		domain := emailLower
		if idx := strings.LastIndex(emailLower, "@"); idx >= 0 {
			domain = emailLower[idx+1:]
		}
		isp := domainToISPLookup(domain)
		if len(allowedISPs) > 0 && !allowedISPs[isp] {
			return deny("isp_not_allowed")
		}
		// Per-ISP qualification gate. Use the inflated reserve target so
		// over-select can fill the reserve pool past plan.Quota. When
		// the reserve pool is disabled (mult <= 1.0) ispReservePool[isp]
		// equals ispQuota[isp], so behavior matches the legacy gate.
		if q := ispQuota[isp]; q > 0 {
			target := ispReservePool[isp]
			if target < q {
				target = q
			}
			if qualifiedPerISP[isp] >= target {
				return deny("isp_quota_cap")
			}
		}

		// Offer suppression: Bloom filter O(1) or DB set fallback
		if useBloomForOffer {
			if suppressed, avail := offerSuppMgrRef.IsSuppressed(offerID, emailLower); avail && suppressed {
				return deny("offer_suppression")
			}
		} else if offerSuppSet[subID] {
			return deny("offer_suppression")
		}

		hash := md5.Sum([]byte(emailLower))
		md5Hex := hex.EncodeToString(hash[:])
		// Single read-lock check covers both global and brand suppressions.
		// Passing campaignBrandRoot="" falls back to global-only so unknown
		// sending domains keep behaving exactly as before.
		if globalHub != nil && globalHub.IsSuppressedForBrand(emailLower, campaignBrandRoot) {
			return deny("global_or_brand_suppression")
		}
		if len(exclusionIDs) > 0 && suppMatcher.IsSuppressedMD5(md5Hex, exclusionIDs) {
			return deny("exclusion_list")
		}
		if exclusionSegEmails[emailLower] {
			return deny("excluded_segment")
		}
		if sourceType == "list" && len(recentlyMailed) > 0 && recentlyMailed[emailLower] {
			return deny("recently_mailed")
		}

		// Per-domain engagement engine — SA-2 in-memory filter.
		// Applies to candidates from EVERY source path (list, segment,
		// SDS, cold-fallback) so it cannot be bypassed by a code path
		// that didn't get the SQL-level treatment. The SDS-source path
		// already filters out unsubscribed / hard-bounced / complained
		// rows at the SQL layer, so the additional state check is a
		// no-op there in steady state.
		var sdsRowVal sdsRow
		var sdsRowFound bool
		if sdsFilterOn {
			sdsRowVal, sdsRowFound = sdsState[subID]
			if sdsRowFound {
				switch sdsRowVal.state {
				case "cold", "suppressed":
					sdsFilteredCold++
					return deny("sds_cold_suppressed")
				}
				if sdsRowVal.lastMailedAt.Valid &&
					time.Since(sdsRowVal.lastMailedAt.Time) < 20*time.Hour {
					sdsFilteredRecent24h++
					return deny("sds_recent_24h")
				}
			}
		}

		selectionRank++
		rec := pmtaSelectedRecipient{
			SubscriberID:  subID,
			Email:         emailLower,
			ISP:           isp,
			SourceType:    sourceType,
			SourceID:      sourceID,
			SelectionRank: selectionRank,
		}
		// Stamp priority hints for the post-streaming sort. Done even
		// when sdsFilterOn=false so the zero-value sort key is stable
		// (every recipient has the same key → preserves insertion order).
		if sdsRowFound {
			rec.sdsStateRank = sdsStateRank(sdsRowVal.state)
			rec.sdsLastEngageAt = sdsLastEngagedAt(sdsRowVal)
			if sdsRowVal.state == "engaged" {
				sdsSelectedEngaged++
			} else {
				sdsSelectedProbe++
			}
		} else {
			rec.sdsStateRank = sdsStateRank("probe")
			sdsSelectedProbe++
		}
		if crossEngagedIDs[subID] {
			rec.sdsCrossEngaged = true
		}

		qualified = append(qualified, rec)
		qualifiedPerISP[isp]++
		ispBackfill = append(ispBackfill, struct{ ID, ISP string }{subID, isp})
		return true
	}

	totalQuota := 0
	hasUnlimited := false
	for _, q := range ispQuota {
		if q <= 0 {
			hasUnlimited = true
			continue
		}
		totalQuota += q
	}
	if hasUnlimited && totalQuota == 0 {
		totalQuota = 50000
	}

	listQualityCheck := func(listID string) bool {
		var complaintRate, bounceRate float64
		var evalCount int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(AVG(complaint_rate), 0), COALESCE(AVG(bounce_rate), 0)
			FROM mailing_list_quality_metrics
			WHERE list_id = $1 AND evaluated_at >= NOW() - INTERVAL '14 days'
		`, listID).Scan(&evalCount, &complaintRate, &bounceRate)
		if err != nil || evalCount == 0 {
			return true
		}
		if complaintRate > 0.0008 {
			log.Printf("[PlanAudience] EXCLUDING list %s: complaint_rate=%.4f%% (threshold=0.08%%, evals=%d)", safePrefix(listID, 12), complaintRate*100, evalCount)
			return false
		}
		if bounceRate > 0.03 {
			log.Printf("[PlanAudience] EXCLUDING list %s: bounce_rate=%.2f%% (threshold=3%%, evals=%d)", safePrefix(listID, 12), bounceRate*100, evalCount)
			return false
		}
		return true
	}

	streamList := func(listID string) error {
		if allQuotasMet() {
			return nil
		}
		if !listQualityCheck(listID) {
			return nil
		}
		// Per-domain engagement engine — SA-2 SQL injection.
		// When sending_domain is non-empty AND the kill switch is off,
		// the helper returns a LEFT JOIN on mailing_subscriber_domain_state
		// + WHERE clauses that exclude cold/suppressed and the 20h cap +
		// an ORDER BY that prioritises cross_engaged then engaged > probe
		// then most-recent engagement. When sending_domain is empty the
		// helper returns a no-op clause and ORDER BY s.id (the safe
		// deterministic fallback that historical list-path tests assume
		// because they call planPMTAAudience with SendingDomain unset).
		var args []any
		args = append(args, listID)
		sdsCl := buildSDSEligibilityClause(input.SendingDomain, "s", len(args))
		args = append(args, sdsCl.BindArgs...)
		query := `SELECT s.id::text, s.email FROM mailing_subscribers s ` +
			sdsCl.Join +
			` WHERE s.list_id = $1 AND s.status IN ('active','confirmed') AND s.is_bot = false ` +
			sdsCl.Where + ` ` + sdsCl.OrderBy
		if totalQuota > 0 {
			scanLimit := totalQuota * 3
			if scanLimit < 10000 {
				scanLimit = 10000
			}
			query += fmt.Sprintf(` LIMIT %d`, scanLimit)
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		staleRows := 0
		for rows.Next() {
			var subID, email string
			if rows.Scan(&subID, &email) == nil {
				if qualifyEmail(subID, email, "list", listID) {
					staleRows = 0
				} else {
					staleRows++
				}
			}
			if allQuotasMet() {
				break
			}
			if staleRows > 5000 {
				log.Printf("[PlanAudience] stale-row exit for list %s after %d consecutive non-qualifying rows", listID, staleRows)
				break
			}
		}
		return nil
	}

	// streamSegmentN streams one segment's members through qualifyEmail.
	// maxAccept > 0 caps the ACCEPTED draw at that many recipients (REQ-C17
	// reserved-first capped draws); maxAccept == 0 preserves the original
	// uncapped behavior byte-for-byte. Returns how many recipients this call
	// accepted (used by the reserve-shortfall check).
	streamSegmentN := func(segmentID string, maxAccept int) (int, error) {
		accepted := 0
		if allQuotasMet() {
			return accepted, nil
		}
		// ONE members read (REQ-043 DoD-3): emptiness is derived from the
		// row loop instead of a separate COUNT(*), so a rebuild or cleanup
		// committing between two statements can no longer make the planner
		// pass a >0 count gate and then read 0 rows (or vice versa).
		//
		// Rotation (2026-07-15): a CAPPED inclusion-segment send — the family
		// CONDUIT ramp draws volume:900 per cell from CONDUIT-<ISP>-<BRAND>
		// pools (~5,400 members) — previously read this segment in HEAP order
		// with no ORDER BY, so allQuotasMet() cut off after the SAME front-N
		// members every day (FIFO). Once mailed, a member has an SDS row and
		// is NOT a cold-fallback (no-SDS-row) candidate, so the cold-fallback
		// rotation fix does NOT reach these members — they are read here.
		//
		// When rotation is ON *and* the send is FULLY BOUNDED (every ISP quota
		// finite — no volume:0 lane), salt the order by the Denver calendar
		// day so the daily draw ROTATES (stable/reproducible WITHIN a day),
		// bounded by a scanLimit LIMIT mirroring streamList's oversample. No
		// new bind arg: segment membership is already brand-partitioned so
		// subscriber_id + Denver-day is a sufficient rotating key.
		//
		// The UNBOUNDED volume:0 path is left EXACTLY as before (no ORDER BY,
		// no LIMIT): an unlimited send mails everyone (order is irrelevant),
		// and sorting an unbounded segment would risk work_mem spill / the
		// 30s statement_timeout — the legitimate reason NOT to touch it.
		segQuery := `SELECT subscriber_id::text, email FROM mailing_segment_members WHERE segment_id = $1`
		segArgs := []interface{}{segmentID}
		if !hasUnlimited && totalQuota > 0 {
			scanLimit := totalQuota * 3
			if scanLimit < 10000 {
				scanLimit = 10000
			}
			switch {
			case recencyAudienceDrawEnabled():
				// RECENCY DRAW (operator ruling 2026-08-24): the cap fills
				// most-recently-engaged first. Ordered by the subscriber's
				// GLOBAL last engagement (mailing_subscribers PK join, hot
				// cache: measured 10s on a 33k segment) rather than the
				// per-domain SDS join (75s cold — past the 30s statement
				// budget, which would silently skip the segment). For a
				// single-brand engagement segment global recency is the
				// faithful proxy; per-domain precision would need a
				// build-time precompute. GREATEST ignores NULLs, so
				// click-only or open-only histories rank by their real
				// instant; never-engaged members sort last.
				segQuery = `SELECT m.subscriber_id::text, m.email
					FROM mailing_segment_members m
					LEFT JOIN mailing_subscribers s ON s.id = m.subscriber_id
					WHERE m.segment_id = $1
					ORDER BY GREATEST(s.last_click_at, s.last_open_at) DESC NULLS LAST` +
					fmt.Sprintf(` LIMIT %d`, scanLimit)
			case rotatingAudienceSelectionEnabled():
				segQuery += ` ORDER BY hashtext(subscriber_id::text || to_char(now() AT TIME ZONE 'America/Denver', 'YYYY-MM-DD')) ASC` +
					fmt.Sprintf(` LIMIT %d`, scanLimit)
			}
		}
		rows, err := db.QueryContext(ctx, segQuery, segArgs...)
		if err != nil {
			log.Printf("[PlanAudience] segment %s materialized read error: %v, skipping", segmentID, err)
			return accepted, nil
		}
		defer rows.Close()
		matCount := 0
		quotasCut := false
		for rows.Next() {
			matCount++
			var subID, email string
			if rows.Scan(&subID, &email) == nil {
				if qualifyEmail(subID, email, "segment", segmentID) {
					accepted++
				}
			}
			// REQ-C17 reserved-draw cap: stop once this segment has accepted
			// its reserve. quotasCut suppresses the zero-members warning path
			// (accepted >= maxAccept > 0 implies matCount > 0 anyway).
			if maxAccept > 0 && accepted >= maxAccept {
				quotasCut = true
				break
			}
			if allQuotasMet() {
				quotasCut = true
				break
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			log.Printf("[PlanAudience] segment %s materialized read error after %d rows: %v, skipping", segmentID, matCount, rowsErr)
			return accepted, nil
		}
		if matCount == 0 && !quotasCut {
			// REQ-043 DoD-2: a 0-member inclusion segment is a structured,
			// operator-visible warning on the campaign's plan record —
			// never only a server log line. The ledger detail distinguishes
			// "never built / build failed" from "built and genuinely empty".
			_, detail := segmentBuildLedgerState(ctx, db, segmentID)
			addPlanWarning(pmtaPlanWarning{
				Code:      planWarnSegmentZeroMembers,
				Scope:     "inclusion",
				SegmentID: segmentID,
				Detail:    detail,
			})
			log.Printf("[PlanAudience] WARNING: segment %s contributed 0 materialized members — %s", segmentID, detail)
		}
		return accepted, nil
	}
	streamSegment := func(segmentID string) error {
		_, err := streamSegmentN(segmentID, 0)
		return err
	}

	// ── Segment reserves (REQ-C17) — reserved-first capped draws ──
	// Runs BEFORE every audience-source branch below (master-selection
	// hybrid prelude, send_priority, and the plain inclusion loops), so a
	// reserved segment gets its floor before ANY other source can drain the
	// ISP quotas — the fix for the fresh-drew-zero class (Lane 3 F5).
	// Guarded by len()>0: an absent/empty field leaves the planner
	// byte-identical to pre-change behavior (golden-tested). Draws are
	// capped at each entry's reserve; the same segment participates again
	// uncapped in its normal source position (seenEmails dedup prevents
	// double-selection), so unfilled reserve seats fall through to the
	// remaining sources. Shortfalls are LOUD: structured plan warning +
	// the promote verify endpoint fails reserve_fill against the floor.
	if len(input.SegmentReserves) > 0 {
		reserveStart := time.Now()
		log.Printf("[PlanAudience/reserves] reserved-first pass start — %d reserved segment(s)", len(input.SegmentReserves))
		for i, sr := range input.SegmentReserves {
			segID := strings.TrimSpace(sr.SegmentID)
			if allQuotasMet() {
				log.Printf("[PlanAudience/reserves] all quotas met before reserve %d/%d (%s) — remaining reserves cannot fill", i+1, len(input.SegmentReserves), safePrefix(segID, 36))
			}
			accepted, err := streamSegmentN(segID, sr.Reserve)
			if err != nil {
				log.Printf("[PlanAudience/reserves] reserve %d/%d segment %s stream error (continuing): %v", i+1, len(input.SegmentReserves), safePrefix(segID, 36), err)
			}
			if accepted < sr.Reserve {
				addPlanWarning(pmtaPlanWarning{
					Code:      planWarnReserveShortfall,
					Scope:     "inclusion",
					SegmentID: segID,
					Detail:    fmt.Sprintf("reserved %d, accepted %d — unfilled seats fall through to remaining sources", sr.Reserve, accepted),
				})
				log.Printf("[PlanAudience/reserves] WARNING reserve_shortfall: segment %s reserved %d, accepted %d", safePrefix(segID, 36), sr.Reserve, accepted)
			} else {
				log.Printf("[PlanAudience/reserves] reserve %d/%d segment %s filled %d/%d", i+1, len(input.SegmentReserves), safePrefix(segID, 36), accepted, sr.Reserve)
			}
		}
		log.Printf("[PlanAudience/reserves] reserved-first pass complete in %v, qualified=%d", time.Since(reserveStart), len(qualified))
	}

	// Master List Migration P3b/P5c — master-selection path.
	//
	// When the campaign has use_master_selection=true (now the default
	// for all newly-created campaigns) we bypass the legacy list_id /
	// brand×ISP-list iteration and source candidates from
	// mailing_subscriber_domain_state scoped to this campaign's sending
	// domain. Brand×ISP quotas are still enforced downstream by planMap;
	// SDS selection only decides WHO is mailable for this sending domain
	// right now.
	//
	// KILL SWITCH: the `else` branch below still runs the legacy list_id
	// path for any campaign with the flag explicitly set to false. It
	// exists for one release only so operators have an emergency rollback
	// path if the master-selection query regresses; after the release
	// following this one, delete everything guarded by !useMasterSelection
	// and delete the flag column itself.
	//
	// Selection gates:
	//   - subscriber global hard bounce / complaint => excluded
	//   - SDS unsubscribed / hard bounce on this domain => excluded
	//   - warmup_status='warming': require at least one observed open in
	//     the last 90d (stricter while domain is earning reputation)
	//   - warmup_status='engaged': require score_local >= 0.05 (the post-
	//     warmup floor; score_local is the 0..1 per-domain signal)
	//   - warmup_status='cold': allow through (first-touch cohort), the
	//     per-domain daily cap handles rate-limiting
	//   - warmup_status='dormant': excluded (deliverability risk)
	useMasterSelection := false
	if input.CampaignID != "" {
		_ = db.QueryRowContext(ctx,
			`SELECT COALESCE(use_master_selection, false) FROM mailing_campaigns WHERE id = $1::uuid`,
			input.CampaignID,
		).Scan(&useMasterSelection)
	}
	if useMasterSelection {
		log.Printf("[PlanAudience] use_master_selection=true — sourcing from mailing_subscriber_domain_state for domain %q (min_remail_hours=%d)", input.SendingDomain, input.MinRemailHours)
		if strings.TrimSpace(input.SendingDomain) == "" {
			return pmtaAudiencePlan{}, fmt.Errorf("master selection requires a non-empty sending_domain")
		}
		normalizedDomain := strings.ToLower(strings.TrimSpace(input.SendingDomain))

		// Primary pass — subscribers WITH an SDS row for this domain. Fast
		// and indexed; this is the steady-state query for every established
		// sending domain. Ordered by per-domain engagement so the best
		// openers on this domain get picked first.
		//
		// Filters applied (see master-list design doc P1/P3b):
		//   - sub.organization_id: multi-tenant guardrail even though we
		//     are single-tenant today; protects against future regressions
		//   - sub.status IN ('active','confirmed'): webhooks globally flip
		//     status to 'unsubscribed'/'bounced'/'complained' for address-
		//     level terminal events, so honouring status here blocks a
		//     cross-brand leak where the SDS row for THIS domain hasn't
		//     been stamped yet but the address was terminated elsewhere
		//   - sub.hard_bounced_at / complained_at: denormalised global
		//     suppressions (index hit instead of join on suppression_list)
		//   - sub.is_bot = false: flagged scanners (set by the data-import
		//     pipeline and the startup bot backstop; the in-email honeypot
		//     trap was retired 2026-07-22). Excluding here prevents a
		//     reinforcement loop where bot opens
		//     inflate score_local → planner prefers bots → more sends.
		//   - sds.unsubscribed_at / hard_bounced_at / complained_at:
		//     per-domain terminal events
		//   - warmup_status gating: cold → first-touch cohort allowed
		//     through, warming requires recent opens, engaged requires
		//     score_local floor, dormant excluded
		streamSDS := func() error {
			if allQuotasMet() {
				return nil
			}
			scanLimit := totalQuota * 4
			if scanLimit < 20000 {
				scanLimit = 20000
			}
			// Master List Migration: honour MinRemailHours as a per-domain
			// recency gate against SDS.last_mailed_at. This is strictly
			// better than the legacy mailing_tracking_events scan because
			// SDS is already scoped to (subscriber, sending_domain) and
			// has an index on (sending_domain, last_mailed_at).
			args := []any{normalizedDomain}
			orgClause := ""
			if strings.TrimSpace(orgID) != "" {
				args = append(args, orgID)
				orgClause = fmt.Sprintf(" AND sub.organization_id = $%d", len(args))
			}
			remailClause := ""
			if input.MinRemailHours > 0 {
				args = append(args, input.MinRemailHours)
				remailClause = fmt.Sprintf(" AND (sds.last_mailed_at IS NULL OR sds.last_mailed_at < NOW() - make_interval(hours => $%d))", len(args))
			}
			args = append(args, scanLimit)
			limitParam := fmt.Sprintf("$%d", len(args))
			query := `
				SELECT sub.id::text, sub.email
				FROM mailing_subscribers sub
				JOIN mailing_subscriber_domain_state sds
				  ON sds.subscriber_id = sub.id
				 AND sds.sending_domain = $1
				WHERE sub.status IN ('active','confirmed')
				  AND sub.hard_bounced_at IS NULL
				  AND sub.complained_at IS NULL
				  AND sub.is_bot = false` + orgClause + `
				  AND sds.unsubscribed_at IS NULL
				  AND sds.hard_bounced_at IS NULL
				  AND sds.complained_at IS NULL
				  AND sds.warmup_status <> 'dormant'
				  AND (
				        sds.warmup_status = 'cold'
				     OR (sds.warmup_status = 'warming'
				         AND sds.last_open_at IS NOT NULL
				         AND sds.last_open_at > NOW() - INTERVAL '90 days')
				     OR (sds.warmup_status = 'engaged'
				         AND sds.score_local >= 0.05)
				  )` + remailClause + `
				ORDER BY sds.score_local DESC NULLS LAST, sds.last_open_at DESC NULLS LAST
				LIMIT ` + limitParam
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("master selection query: %w", err)
			}
			defer rows.Close()
			streamed := 0
			for rows.Next() {
				var subID, email string
				if rows.Scan(&subID, &email) == nil {
					qualifyEmail(subID, email, "sds", input.SendingDomain)
					streamed++
				}
				if allQuotasMet() {
					break
				}
			}
			log.Printf("[PlanAudience] master selection streamed %d candidates from SDS for domain %s", streamed, input.SendingDomain)
			return nil
		}

		// Cold-fallback pass — subscribers with NO SDS row for this
		// sending domain. Two practical scenarios this covers:
		//   1. Cold-start: a freshly-onboarded sending domain has zero
		//      SDS rows. The primary pass yields nothing. Without this
		//      fallback the campaign would deploy with an empty audience.
		//   2. Partial coverage: subscribers imported after the P3a
		//      backfill have no SDS row on any domain yet. Treating them
		//      as cold-cohort lets them enter the warming rotation on
		//      first send (UpsertSDSSend will create their SDS row and
		//      flip status to 'warming'). Without this they are
		//      permanently invisible to master selection.
		//
		// Per-ISP + per-brand stripe:
		// The fallback runs ONE query per ISP that still has a shortfall.
		// Each query is scoped to `sub.isp = <isp>` and ordered by a
		// deterministic hash of (sub.id, sending_domain). Two brands
		// launching welcome campaigns at the same minute hash to disjoint
		// slices of the same ISP pool instead of both grabbing the top-
		// engagement records, which was the root cause of the 2026-04-20
		// cross-brand audience overlap. Quotas, pool configuration at the
		// application level, and PMTA pools all key on these ISP names,
		// so the per-ISP filter is also what makes quota math sound when
		// the pool's ISP mix is skewed (e.g. mostly gmail) relative to
		// what the campaign needs (e.g. yahoo quota > gmail quota).
		//
		// The ISP value used for filtering is mailing_subscribers.isp —
		// populated at write time by the lazy backfill below, and eagerly
		// maintained by the ISPBackfillWorker (internal/worker/isp_backfill.go).
		// A final catch-all pass picks up any stragglers whose isp column
		// is still NULL/empty, so correctness is preserved even if the
		// backfill worker hasn't caught up to a recent import.
		streamSDSColdFallback := func() error {
			if allQuotasMet() {
				return nil
			}

			runPerISP := func(ispName string, shortfall int, isCatchAll bool) {
				if shortfall <= 0 {
					return
				}
				// Oversample 4x to absorb suppressions and SDS races.
				// The hot path has filters (exclusions, suppression, bloom)
				// that may reject a material fraction of candidates.
				scanLimit := shortfall * 4
				if scanLimit < 5000 {
					scanLimit = 5000
				}
				args := []any{normalizedDomain}
				orgClause := ""
				if strings.TrimSpace(orgID) != "" {
					args = append(args, orgID)
					orgClause = fmt.Sprintf(" AND sub.organization_id = $%d", len(args))
				}

				var ispClause string
				if isCatchAll {
					ispClause = ` AND (sub.isp IS NULL OR sub.isp = '')`
				} else {
					args = append(args, ispName)
					ispClause = fmt.Sprintf(" AND sub.isp = $%d", len(args))
				}

				args = append(args, scanLimit)
				limitParam := fmt.Sprintf("$%d", len(args))

				// Per-(subscriber, sending_domain) stripe, ROTATING daily.
				// hashtext() is index-friendly when wrapped in ORDER BY and
				// gives each brand a disjoint slice of the eligible pool
				// (keyed on the sending domain $1) — the 2026-04-20 cross-
				// brand overlap fix. Tie-break by engagement_score so within
				// a stripe the higher-engagement records still land first.
				//
				// The stripe is salted with the Denver calendar day so the
				// daily draw ROTATES to different cold members each day
				// instead of re-hitting the same quota-sized prefix (FIFO-
				// equivalent). The salt is stable WITHIN a Denver day, so the
				// selection is reproducible for retries/re-finalizes on the
				// same day. Gated by rotatingAudienceSelectionEnabled(); the
				// kill switch restores the prior day-stable ordering. The
				// disjoint-brand property is preserved because the domain ($1)
				// remains in the hash input either way.
				stripeExpr := `hashtext(sub.id::text || $1)`
				if rotatingAudienceSelectionEnabled() {
					stripeExpr = `hashtext(sub.id::text || $1 || to_char(now() AT TIME ZONE 'America/Denver', 'YYYY-MM-DD'))`
				}
				query := `
					SELECT sub.id::text, sub.email
					FROM mailing_subscribers sub
					WHERE sub.status IN ('active','confirmed')
					  AND sub.hard_bounced_at IS NULL
					  AND sub.complained_at IS NULL
					  AND sub.is_bot = false` + orgClause + ispClause + `
					  AND NOT EXISTS (
					    SELECT 1 FROM mailing_subscriber_domain_state sds
					    WHERE sds.subscriber_id = sub.id
					      AND sds.sending_domain = $1
					  )
					ORDER BY ` + stripeExpr + ` ASC,
					         sub.engagement_score DESC NULLS LAST
					LIMIT ` + limitParam
				rows, err := db.QueryContext(ctx, query, args...)
				if err != nil {
					// Non-fatal — log and continue. Primary pass may
					// already have filled quota and the fallback is
					// defensive.
					log.Printf("[PlanAudience] cold-fallback query failed for isp=%s domain=%s: %v", ispName, normalizedDomain, err)
					return
				}
				defer rows.Close()
				streamed := 0
				for rows.Next() {
					var subID, email string
					if rows.Scan(&subID, &email) == nil {
						qualifyEmail(subID, email, "sds_cold", input.SendingDomain)
						streamed++
					}
					if allQuotasMet() {
						break
					}
				}
				log.Printf("[PlanAudience] cold-fallback isp=%s streamed=%d shortfall=%d catchall=%v domain=%s", ispName, streamed, shortfall, isCatchAll, normalizedDomain)
			}

			// Sort ISPs by descending shortfall so the biggest gaps run
			// first — gives the best chance of hitting allQuotasMet before
			// smaller gaps even need to fire.
			type ispShortfall struct {
				isp       string
				shortfall int
			}
			shortfalls := make([]ispShortfall, 0, len(ispQuota))
			hasBounded := false
			for isp, q := range ispQuota {
				if q <= 0 {
					// Unlimited plan: no ceiling, so there's no "shortfall"
					// to size to. Use a modest fixed scan so unlimited plans
					// still receive per-ISP striped candidates.
					shortfalls = append(shortfalls, ispShortfall{isp: isp, shortfall: 1250})
					continue
				}
				hasBounded = true
				need := q - qualifiedPerISP[isp]
				if need > 0 {
					shortfalls = append(shortfalls, ispShortfall{isp: isp, shortfall: need})
				}
			}
			if hasBounded && len(shortfalls) == 0 {
				return nil
			}
			// Deterministic ordering: largest shortfall first, tie-break
			// by ISP name so logs are reproducible across runs.
			sort.Slice(shortfalls, func(i, j int) bool {
				if shortfalls[i].shortfall != shortfalls[j].shortfall {
					return shortfalls[i].shortfall > shortfalls[j].shortfall
				}
				return shortfalls[i].isp < shortfalls[j].isp
			})

			for _, sf := range shortfalls {
				if allQuotasMet() {
					break
				}
				// Recompute live shortfall each iteration — earlier ISPs
				// may have pulled in cross-classified stragglers (shouldn't
				// happen with the isp filter, but we respect the invariant).
				live := sf.shortfall
				if q := ispQuota[sf.isp]; q > 0 {
					live = q - qualifiedPerISP[sf.isp]
				}
				if live <= 0 {
					continue
				}
				runPerISP(sf.isp, live, false)
			}

			// Catch-all pass for subscribers whose isp column is still
			// empty — this window should shrink to zero once the
			// ISPBackfillWorker has completed its first full pass.
			// Protects correctness for freshly-imported subscribers.
			if !allQuotasMet() {
				remaining := 0
				for isp, q := range ispQuota {
					if q <= 0 {
						continue
					}
					need := q - qualifiedPerISP[isp]
					if need > 0 {
						remaining += need
					}
				}
				if remaining > 0 || !hasBounded {
					if remaining <= 0 {
						remaining = 5000
					}
					runPerISP("", remaining, true)
				}
			}
			return nil
		}

		// ------------------------------------------------------------------
		// Hybrid priority prelude (Master List Migration P7c / vendor-batch
		// integration). When a master-selection campaign ALSO specifies
		// send_priority or inclusion_segments, drain those sources FIRST to
		// fill per-ISP quotas. Whatever quota is still open after the prelude
		// falls through to the existing SDS + SDS-cold-fallback passes below.
		//
		// Use case: a freshly-imported vendor cohort (e.g. "WestCapital
		// Verified - Homeowners Jan 2026") lives on the Verified External
		// Imports - Master list with no per-domain SDS rows yet. The operator
		// points send_priority at the cohort's segment; we drain the segment
		// first (cold/first-touch path), and any remaining ISP quota is
		// topped up from the engaged pool via SDS. Strictly additive —
		// campaigns without send_priority/inclusion_segments run the SDS
		// passes directly, matching pre-change behaviour.
		//
		// Backwards-compat invariants:
		//   - streamList / streamSegment already apply global suppression,
		//     hard-bounce, complaint, and per-ISP quota filters via the
		//     shared qualifyEmail closure. No duplicate filtering here.
		//   - allQuotasMet() is the shared gate; we stop iterating priority
		//     sources as soon as every ISP quota is filled.
		//   - Errors from streamList/streamSegment are logged and skipped
		//     rather than returned: a mis-specified priority source should
		//     not block SDS from running. This matches the non-master
		//     SendPriority branch below.
		preludePriorities := len(input.SendPriority) + len(input.InclusionSegments) + len(inclusionIDs)
		if preludePriorities > 0 {
			preludeStart := time.Now()
			log.Printf("[PlanAudience/hybrid] prelude start — send_priority=%d inclusion_segments=%d inclusion_lists=%d (master-selection fills remaining quota)",
				len(input.SendPriority), len(input.InclusionSegments), len(inclusionIDs))
			for idx, item := range input.SendPriority {
				if allQuotasMet() {
					log.Printf("[PlanAudience/hybrid] all quotas met at priority %d/%d, prelude early exit", idx+1, len(input.SendPriority))
					break
				}
				itemStart := time.Now()
				switch item.Type {
				case "list":
					resolved, _ := resolveListNamesToIDs(ctx, db, orgID, []string{item.ID})
					for _, listID := range resolved {
						if err := streamList(listID); err != nil {
							log.Printf("[PlanAudience/hybrid] priority %d/%d list %s stream error (continuing): %v",
								idx+1, len(input.SendPriority), safePrefix(listID, 36), err)
						}
					}
					log.Printf("[PlanAudience/hybrid] priority %d/%d list %s streamed in %v, qualified=%d",
						idx+1, len(input.SendPriority), safePrefix(item.ID, 36), time.Since(itemStart), len(qualified))
				case "segment":
					if err := streamSegment(item.ID); err != nil {
						log.Printf("[PlanAudience/hybrid] priority %d/%d segment %s stream error (continuing): %v",
							idx+1, len(input.SendPriority), safePrefix(item.ID, 36), err)
					}
					log.Printf("[PlanAudience/hybrid] priority %d/%d segment %s streamed in %v, qualified=%d",
						idx+1, len(input.SendPriority), safePrefix(item.ID, 36), time.Since(itemStart), len(qualified))
				}
			}
			for i, listID := range inclusionIDs {
				if allQuotasMet() {
					log.Printf("[PlanAudience/hybrid] all quotas met at inclusion_list %d/%d, prelude early exit", i+1, len(inclusionIDs))
					break
				}
				if err := streamList(listID); err != nil {
					log.Printf("[PlanAudience/hybrid] inclusion_list %d/%d (%s) stream error (continuing): %v",
						i+1, len(inclusionIDs), safePrefix(listID, 12), err)
				}
			}
			for i, segmentID := range input.InclusionSegments {
				if allQuotasMet() {
					log.Printf("[PlanAudience/hybrid] all quotas met at inclusion_segment %d/%d, prelude early exit", i+1, len(input.InclusionSegments))
					break
				}
				if err := streamSegment(segmentID); err != nil {
					log.Printf("[PlanAudience/hybrid] inclusion_segment %d/%d (%s) stream error (continuing): %v",
						i+1, len(input.InclusionSegments), safePrefix(segmentID, 12), err)
				}
			}
			log.Printf("[PlanAudience/hybrid] prelude complete in %v, qualified=%d quotas_met=%t — falling through to SDS for remaining quota",
				time.Since(preludeStart), len(qualified), allQuotasMet())
		}

		if err := streamSDS(); err != nil {
			return pmtaAudiencePlan{}, err
		}
		if err := streamSDSColdFallback(); err != nil {
			return pmtaAudiencePlan{}, err
		}
	} else if len(input.SendPriority) > 0 {
		log.Printf("[PlanAudience] processing %d send_priority items, elapsed %v", len(input.SendPriority), time.Since(planStart))
		for idx, item := range input.SendPriority {
			if allQuotasMet() {
				log.Printf("[PlanAudience] all quotas met at priority item %d/%d, qualified=%d", idx+1, len(input.SendPriority), len(qualified))
				break
			}
			itemStart := time.Now()
			switch item.Type {
			case "list":
				resolved, _ := resolveListNamesToIDs(ctx, db, orgID, []string{item.ID})
				for _, listID := range resolved {
					if err := streamList(listID); err != nil {
						return pmtaAudiencePlan{}, err
					}
				}
				log.Printf("[PlanAudience] priority %d/%d list %s streamed in %v, qualified=%d", idx+1, len(input.SendPriority), safePrefix(item.ID, 36), time.Since(itemStart), len(qualified))
			case "segment":
				if err := streamSegment(item.ID); err != nil {
					return pmtaAudiencePlan{}, err
				}
				log.Printf("[PlanAudience] priority %d/%d segment %s streamed in %v, qualified=%d", idx+1, len(input.SendPriority), safePrefix(item.ID, 36), time.Since(itemStart), len(qualified))
			}
		}
	} else {
		for i, listID := range inclusionIDs {
			if allQuotasMet() {
				break
			}
			listStart := time.Now()
			if err := streamList(listID); err != nil {
				return pmtaAudiencePlan{}, err
			}
			log.Printf("[PlanAudience] list %d/%d (%s) streamed in %v, qualified so far: %d",
				i+1, len(inclusionIDs), safePrefix(listID, 12), time.Since(listStart), len(qualified))
		}
		for _, segmentID := range input.InclusionSegments {
			if allQuotasMet() {
				break
			}
			if err := streamSegment(segmentID); err != nil {
				return pmtaAudiencePlan{}, err
			}
		}
	}

	if len(ispQuota) > 0 {
		log.Printf("[PlanAudience] early-cutoff: scanned %d unique emails, qualified %d (quotas: %v, filled: %v)",
			len(seenEmails), len(qualified), ispQuota, qualifiedPerISP)
	}

	recipientsByISP := make(map[string][]pmtaSelectedRecipient, len(normalized.Plans))
	for _, rec := range qualified {
		recipientsByISP[rec.ISP] = append(recipientsByISP[rec.ISP], rec)
	}

	selectedByISP := make(map[string][]pmtaSelectedRecipient, len(normalized.Plans))
	countsByISP := make(map[string]int, len(normalized.Plans))
	reserveCountsByISP := make(map[string]int, len(normalized.Plans))
	selectedTotal := 0
	reserveTotal := 0
	reserveMult := reservePoolMultiplier()
	for isp, plan := range planMap {
		recipients := append([]pmtaSelectedRecipient(nil), recipientsByISP[isp]...)
		// Per-domain engagement engine — SA-2 priority sort. Runs
		// BEFORE quota truncation so the quota-trimmed slice keeps the
		// best subscribers. Skipped when RandomizeAudience is true so
		// the operator's randomization request still wins (used for
		// A/B-style tests where deterministic ordering would bias
		// results). When the SDS pre-load returned no data every
		// recipient has the zero-value sort key and the sort is a no-op
		// preserving insertion order.
		if !plan.RandomizeAudience && sdsFilterOn && len(recipients) > 1 {
			sort.SliceStable(recipients, func(i, j int) bool {
				if recipients[i].sdsCrossEngaged != recipients[j].sdsCrossEngaged {
					return recipients[i].sdsCrossEngaged
				}
				if recipients[i].sdsStateRank != recipients[j].sdsStateRank {
					return recipients[i].sdsStateRank < recipients[j].sdsStateRank
				}
				if !recipients[i].sdsLastEngageAt.Equal(recipients[j].sdsLastEngageAt) {
					return recipients[i].sdsLastEngageAt.After(recipients[j].sdsLastEngageAt)
				}
				return recipients[i].SelectionRank < recipients[j].SelectionRank
			})
		}
		if plan.RandomizeAudience && len(recipients) > 1 {
			rand.Shuffle(len(recipients), func(i, j int) {
				recipients[i], recipients[j] = recipients[j], recipients[i]
			})
		}
		// Cap-aware reserve pool (Slice 2):
		//   Keep up to plan.Quota * reserveMult recipients. The first
		//   plan.Quota are selected (IsReserve=false). Anything beyond
		//   is tagged IsReserve=true so persistence writes them with
		//   status='reserve'. The wave dispatcher (Slice 4) draws from
		//   selected first, falling back to reserves when the worker
		//   cap-skips a subscriber.
		//
		//   reserveMult=1.0 (kill switch or explicit operator override)
		//   collapses to the prior hard-truncation behavior.
		selectedCount := len(recipients)
		reserveCount := 0
		if plan.Quota > 0 {
			reserveLimit := plan.Quota
			if reserveMult > 1.0 {
				reserveLimit = int(float64(plan.Quota) * reserveMult)
				if reserveLimit < plan.Quota {
					reserveLimit = plan.Quota
				}
			}
			if len(recipients) > reserveLimit {
				recipients = recipients[:reserveLimit]
			}
			if len(recipients) > plan.Quota {
				selectedCount = plan.Quota
				reserveCount = len(recipients) - plan.Quota
				for i := plan.Quota; i < len(recipients); i++ {
					recipients[i].IsReserve = true
				}
			} else {
				selectedCount = len(recipients)
				reserveCount = 0
			}
		}
		selectedByISP[isp] = recipients
		countsByISP[isp] = selectedCount
		reserveCountsByISP[isp] = reserveCount
		selectedTotal += selectedCount
		reserveTotal += reserveCount
	}

	// Per-finalize observability. One line, every campaign — easy to
	// grep for when triaging "why did campaign X get N recipients?"
	// questions. The detailed counts give us a clear signal of whether
	// the SDS filter is doing meaningful work or is effectively a
	// no-op for this campaign.
	log.Printf("[finalizer] campaign=%s domain=%q sds_filter: enabled=%t in=%d filtered_24h=%d filtered_cold=%d selected_engaged=%d selected_probe=%d selected_total=%d reserve_total=%d reserve_mult=%.2f",
		input.CampaignID, input.SendingDomain, sdsFilterOn,
		len(seenEmails), sdsFilteredRecent24h, sdsFilteredCold,
		sdsSelectedEngaged, sdsSelectedProbe, selectedTotal, reserveTotal, reserveMult)

	// Async backfill: write ISP to subscribers that don't have it set yet.
	if len(ispBackfill) > 0 {
		go func() {
			bgCtx := context.Background()
			for i := 0; i < len(ispBackfill); i += 500 {
				end := i + 500
				if end > len(ispBackfill) {
					end = len(ispBackfill)
				}
				for _, entry := range ispBackfill[i:end] {
					db.ExecContext(bgCtx,
						"UPDATE mailing_subscribers SET isp = $1 WHERE id = $2 AND (isp IS NULL OR isp = '')",
						entry.ISP, entry.ID)
				}
			}
		}()
	}

	return pmtaAudiencePlan{
		RecipientsByISP:    selectedByISP,
		CountsByISP:        countsByISP,
		ReserveCountsByISP: reserveCountsByISP,
		TotalSeen:          len(seenEmails),
		AfterSuppression:   len(qualified),
		SelectedTotal:      selectedTotal,
		ReserveTotal:       reserveTotal,
		SuppressionReasons: suppressionReasons,
		PlanWarnings:       planWarnings,
	}, nil
}

func loadExclusionSegmentEmails(ctx context.Context, db dbQuerier, segmentIDs []string) (map[string]bool, []pmtaPlanWarning, error) {
	emails := make(map[string]bool)
	var warnings []pmtaPlanWarning
	for _, segmentID := range segmentIDs {
		segStart := time.Now()

		// FAIL CLOSED (REQ-002): a load error on a named exclusion segment
		// fails the plan — never a silent skip. ONE members read (REQ-043
		// DoD-3 sibling): emptiness is derived from the row loop, not a
		// separate COUNT(*) racing the read.
		rows, err := db.QueryContext(ctx,
			`SELECT email FROM mailing_segment_members WHERE segment_id = $1`, segmentID)
		if err != nil {
			return nil, nil, fmt.Errorf("read exclusion segment %s: %w (fail-closed: campaign must not plan without its exclusions)", segmentID, err)
		}
		matCount := 0
		for rows.Next() {
			var email string
			if rows.Scan(&email) == nil {
				emails[strings.ToLower(strings.TrimSpace(email))] = true
				matCount++
			}
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return nil, nil, fmt.Errorf("read exclusion segment %s: %w (fail-closed: partial exclusion load)", segmentID, rowsErr)
		}
		if matCount == 0 {
			// REQ-043 DoD-1: "never built / failed build" is NOT the same
			// as "built and genuinely empty". An unbuilt exclusion silently
			// WIDENS the audience (the suppression-gap analog of the jun29
			// collapse class), so it FAILS the plan — matching the REQ-002
			// exclusion-list fail-closed idiom above — unless the
			// DISABLE_SEGMENT_BUILD_FAIL_CLOSED kill switch is on.
			// A verified empty build legitimately excludes nobody and only
			// records a structured warning on the campaign's plan record.
			builtEmpty, detail := segmentBuildLedgerState(ctx, db, segmentID)
			if !builtEmpty && !segmentBuildFailClosedDisabled() {
				return nil, nil, fmt.Errorf("exclusion segment %s has 0 materialized members and no verified build — %s (fail-closed: an unbuilt exclusion would silently widen the audience; rebuild the segment or set DISABLE_SEGMENT_BUILD_FAIL_CLOSED=true to override)", segmentID, detail)
			}
			warnings = append(warnings, pmtaPlanWarning{
				Code:      planWarnSegmentZeroMembers,
				Scope:     "exclusion",
				SegmentID: segmentID,
				Detail:    detail,
			})
			log.Printf("[loadExclusionSegmentEmails] WARNING: exclusion segment %s excludes nobody — %s (fail_closed_disabled=%t)",
				segmentID, detail, segmentBuildFailClosedDisabled())
			continue
		}
		log.Printf("[loadExclusionSegmentEmails] segment %s: %d emails from materialized cache in %v",
			safePrefix(segmentID, 12), matCount, time.Since(segStart))
	}
	return emails, warnings, nil
}

func buildPMTAWaveSpecs(campaignID string, plan pmtaNormalizedPlan, recipientCount int) []pmtaWaveSpec {
	if recipientCount <= 0 {
		return nil
	}

	// Calculate the number of waves that fit in the delivery window.
	// batch_size=0 means "auto-calculate" — spread the audience evenly
	// across the full window. We NEVER allow a single-wave blast.
	cadenceMin := plan.Cadence.EveryMinutes
	if cadenceMin <= 0 {
		cadenceMin = defaultCadenceMinutes
	}

	totalWindowMinutes := 0
	for _, span := range plan.TimeSpans {
		d := int(span.EndAt.Sub(span.StartAt).Minutes())
		if d > 0 {
			totalWindowMinutes += d
		}
	}
	if totalWindowMinutes < cadenceMin {
		totalWindowMinutes = int(defaultThrottleDuration.Minutes())
		if len(plan.TimeSpans) > 0 {
			plan.TimeSpans[0].EndAt = plan.TimeSpans[0].StartAt.Add(defaultThrottleDuration)
		}
	}

	maxWaves := totalWindowMinutes/cadenceMin + 1
	if maxWaves < minWavesPerISP {
		maxWaves = minWavesPerISP
	}

	batchSize := plan.Cadence.BatchSize
	if batchSize <= 0 {
		batchSize = (recipientCount + maxWaves - 1) / maxWaves
		if batchSize < 1 {
			batchSize = 1
		}
	}
	if batchSize > recipientCount {
		batchSize = recipientCount
	}
	// Guard: if clamped batchSize would produce fewer than minWavesPerISP
	// waves for a non-trivial audience, recalculate to meet the minimum.
	if recipientCount >= 500 && batchSize > 0 {
		estimatedWaves := (recipientCount + batchSize - 1) / batchSize
		if estimatedWaves < minWavesPerISP {
			batchSize = (recipientCount + minWavesPerISP - 1) / minWavesPerISP
		}
	}

	var waves []pmtaWaveSpec
	remaining := recipientCount

	switch {
	case plan.Cadence.Mode == "interval" && len(plan.TimeSpans) > 0:
		for _, span := range plan.TimeSpans {
			if remaining <= 0 {
				break
			}
			scheduledAt := span.StartAt
			waveLimit := batchSize
			for !scheduledAt.After(span.EndAt) && remaining > 0 {
				planned := pmtaMinInt(waveLimit, remaining)
				waves = append(waves, pmtaWaveSpec{
					WaveNumber:        len(waves) + 1,
					ScheduledAt:       scheduledAt,
					WindowStartAt:     span.StartAt,
					WindowEndAt:       span.EndAt,
					CadenceMinutes:    plan.Cadence.EveryMinutes,
					BatchSize:         waveLimit,
					PlannedRecipients: planned,
					IdempotencyKey:    fmt.Sprintf("%s:%s:%d", campaignID, plan.ISP, len(waves)+1),
				})
				remaining -= planned
				if plan.Cadence.EveryMinutes <= 0 {
					break
				}
				scheduledAt = scheduledAt.Add(time.Duration(plan.Cadence.EveryMinutes) * time.Minute)
			}
		}
	default:
		// Even in the default/single-mode fallback, enforce interval delivery.
		if len(plan.TimeSpans) == 0 {
			startAt := time.Now().UTC()
			plan.TimeSpans = []pmtaNormalizedTimeSpan{{
				StartAt: startAt,
				EndAt:   startAt.Add(defaultThrottleDuration),
			}}
		}
		for _, span := range plan.TimeSpans {
			if remaining <= 0 {
				break
			}
			scheduledAt := span.StartAt
			for remaining > 0 && !scheduledAt.After(span.EndAt) {
				planned := batchSize
				if planned > remaining {
					planned = remaining
				}
				waves = append(waves, pmtaWaveSpec{
					WaveNumber:        len(waves) + 1,
					ScheduledAt:       scheduledAt,
					WindowStartAt:     span.StartAt,
					WindowEndAt:       span.EndAt,
					CadenceMinutes:    cadenceMin,
					BatchSize:         batchSize,
					PlannedRecipients: planned,
					IdempotencyKey:    fmt.Sprintf("%s:%s:%d", campaignID, plan.ISP, len(waves)+1),
				})
				remaining -= planned
				scheduledAt = scheduledAt.Add(time.Duration(cadenceMin) * time.Minute)
			}
		}
	}

	// If somehow no waves were generated, create a throttled fallback
	// spread across the default window — NEVER a single blast.
	if len(waves) == 0 {
		startAt := time.Now().UTC()
		endAt := startAt.Add(defaultThrottleDuration)
		totalWaves := int(defaultThrottleDuration.Minutes()) / defaultCadenceMinutes
		if totalWaves < minWavesPerISP {
			totalWaves = minWavesPerISP
		}
		fallbackBatch := (recipientCount + totalWaves - 1) / totalWaves
		if fallbackBatch < 1 {
			fallbackBatch = 1
		}
		rem := recipientCount
		at := startAt
		for w := 0; w < totalWaves && rem > 0; w++ {
			planned := fallbackBatch
			if planned > rem {
				planned = rem
			}
			waves = append(waves, pmtaWaveSpec{
				WaveNumber:        w + 1,
				ScheduledAt:       at,
				WindowStartAt:     startAt,
				WindowEndAt:       endAt,
				CadenceMinutes:    defaultCadenceMinutes,
				BatchSize:         fallbackBatch,
				PlannedRecipients: planned,
				IdempotencyKey:    fmt.Sprintf("%s:%s:%d", campaignID, plan.ISP, w+1),
			})
			rem -= planned
			at = at.Add(time.Duration(defaultCadenceMinutes) * time.Minute)
		}
	}

	return waves
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func coalesceString(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// isCanonicalISP returns true when the ISP name matches one of the 10 major
// ISP groups that have dedicated sending pools and therefore receive their
// own ISP plan (quotas, waves, cadence) during campaign deployment.
//
// Adding an ISP here means the campaign planner will:
//   1. Create a dedicated mailing_campaign_isp_plans row for it.
//   2. Build independent wave specs with separate cadence timing.
//   3. Route its traffic through a dedicated IP pool (via isp.PoolSuffix).
//
// Non-canonical ISPs (verizon, protonmail, zoho, "other", etc.) are still
// sent — they route through the "general" pool — but they don't get their
// own plan/wave cadence; they piggyback on the catch-all plan.
func isCanonicalISP(isp string) bool {
	switch isp {
	case string(engine.ISPGmail), string(engine.ISPYahoo), string(engine.ISPAol),
		string(engine.ISPMicrosoft), string(engine.ISPApple), string(engine.ISPComcast),
		string(engine.ISPAtt), string(engine.ISPSbcglobal), string(engine.ISPCox), string(engine.ISPCharter):
		return true
	default:
		return false
	}
}

func pmtaMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func mustUUID(value string) uuid.UUID {
	id, _ := uuid.Parse(value)
	return id
}

// preflightError holds a single preflight check failure.
type preflightError struct {
	Check   string `json:"check"`
	Message string `json:"message"`
}

// preflightResult aggregates all preflight outcomes.
type preflightResult struct {
	OK       bool             `json:"ok"`
	Errors   []preflightError `json:"errors,omitempty"`
	Warnings []preflightError `json:"warnings,omitempty"`
}

// preflightDeployCheck validates infrastructure readiness before campaign
// deployment. It is intentionally fail-fast: any error means the campaign
// should NOT be deployed.
//
// When overrideProfileID is non-empty, the profile lookup is pinned to that
// UUID (must be vendor_type='pmta', status='active', and owned by orgID). This
// matches the pin semantics in resolvePMTASendingProfile so the preflight
// validates the same profile the deploy will actually use. When empty, the
// legacy by-domain auto-lookup runs unchanged.
func preflightDeployCheck(ctx context.Context, db *sql.DB, orgID string, sendingDomain string, overrideProfileID string) preflightResult {
	res := preflightResult{OK: true}

	// 1. Sending profile exists with an IP pool (or, for SES tenant routes,
	// an SES configuration_set + tenant_name; the IP-pool / DKIM-DNS / SPF
	// checks below are conditionally skipped for via_ses=true profiles
	// because SES is the relay — the pool name on those profiles refers
	// to a PMTA virtual-mta-pool defined in /etc/pmta/config, not to a
	// row in mailing_ip_addresses, so the legacy "active IPs > 0" check
	// always trips. SES handles DKIM signing on the apex em.<brand>
	// identity (Easy DKIM) and uses its own envelope sender for SPF, so
	// the m.<brand> sending_domain DNS checks are also irrelevant on
	// SES routes — the only checks that still apply are PMTA bridge
	// reachability and that the SES routing fields are populated.
	var profileID, ipPool, poolPrefix, sesConfigSet, sesTenantName sql.NullString
	var viaSES sql.NullBool
	var err error
	if overrideProfileID != "" {
		err = db.QueryRowContext(ctx, `
			SELECT id::text, ip_pool, COALESCE(pool_prefix, ''),
			       COALESCE(via_ses, false), ses_configuration_set, ses_tenant_name
			FROM mailing_sending_profiles
			WHERE organization_id = $1 AND id = $2
			  AND vendor_type = 'pmta'
			  AND status = 'active'
		`, orgID, overrideProfileID).Scan(&profileID, &ipPool, &poolPrefix, &viaSES, &sesConfigSet, &sesTenantName)
	} else {
		// Resolver order: is_default profiles ALWAYS win the by-domain
		// lookup. See the matching comment in
		// HandleSendTestEmail / mailing_sending.go for full rationale.
		// Without is_default DESC, a newer SES-relay or IP-warming profile
		// can silently hijack the daily-ops profile when both share
		// sending_domain — diagnosed against em.discountblog.com on
		// 2026-05-30 when an SES Relay profile created at 23:50 UTC
		// won the resolver against the established OVH PMTA profile.
		err = db.QueryRowContext(ctx, `
			SELECT id::text, ip_pool, COALESCE(pool_prefix, ''),
			       COALESCE(via_ses, false), ses_configuration_set, ses_tenant_name
			FROM mailing_sending_profiles
			WHERE organization_id = $1 AND vendor_type = 'pmta'
			  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
			  AND status = 'active'
			ORDER BY is_default DESC, created_at DESC LIMIT 1
		`, orgID, sendingDomain).Scan(&profileID, &ipPool, &poolPrefix, &viaSES, &sesConfigSet, &sesTenantName)
	}
	if err != nil || !profileID.Valid {
		var msg string
		if overrideProfileID != "" {
			msg = fmt.Sprintf("pinned PMTA sending profile %s not found, not vendor=pmta, or not active for org", overrideProfileID)
		} else {
			msg = fmt.Sprintf("no active PMTA sending profile found for domain %s", sendingDomain)
		}
		res.OK = false
		res.Errors = append(res.Errors, preflightError{
			Check:   "sending_profile",
			Message: msg,
		})
		return res
	}

	isSESRoute := viaSES.Valid && viaSES.Bool

	if isSESRoute {
		// SES tenant route: validate SES routing fields are populated.
		// Pool name is a PMTA virtual-mta-pool (e.g. db-ses-pool) that
		// PMTA matches on the X-Virtual-MTA header to relay outbound to
		// email-smtp.us-west-1.amazonaws.com:587 — not an IP-table pool.
		cs := strings.TrimSpace(sesConfigSet.String)
		tn := strings.TrimSpace(sesTenantName.String)
		if cs == "" || tn == "" {
			res.OK = false
			res.Errors = append(res.Errors, preflightError{
				Check:   "ses_routing_fields",
				Message: fmt.Sprintf("SES tenant profile %s has via_ses=true but is missing ses_configuration_set (%q) or ses_tenant_name (%q)", profileID.String, cs, tn),
			})
			return res
		}
	} else {
		// Dedicated PMTA route: legacy IP-pool / DKIM-DNS / SPF checks apply.
		if (!ipPool.Valid || strings.TrimSpace(ipPool.String) == "") && (!poolPrefix.Valid || strings.TrimSpace(poolPrefix.String) == "") {
			res.OK = false
			res.Errors = append(res.Errors, preflightError{
				Check:   "ip_pool",
				Message: fmt.Sprintf("sending profile %s has no IP pool or pool_prefix assigned", profileID.String),
			})
			return res
		}

		// 2. IP pool has active IPs with valid VMTA names
		var poolQuery string
		var poolArg interface{}
		pfx := ""
		if poolPrefix.Valid {
			pfx = strings.TrimSpace(poolPrefix.String)
		}
		if pfx != "" {
			poolQuery = `
				SELECT ip.hostname, ip.status, pool.name
				FROM mailing_ip_addresses ip
				JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
				WHERE pool.name LIKE $1 || '-%'
				  AND ip.status IN ('active', 'warmup')
				  AND pool.status = 'active'`
			poolArg = pfx
		} else {
			poolQuery = `
				SELECT ip.hostname, ip.status, pool.name
				FROM mailing_ip_addresses ip
				JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
				WHERE pool.name = $1
				  AND ip.status IN ('active', 'warmup')
				  AND pool.status = 'active'`
			poolArg = ipPool.String
		}

		rows, qErr := db.QueryContext(ctx, poolQuery, poolArg)
		if qErr != nil {
			res.OK = false
			res.Errors = append(res.Errors, preflightError{
				Check:   "ip_pool_query",
				Message: fmt.Sprintf("failed to query IP pools for prefix=%s pool=%s: %v", pfx, ipPool.String, qErr),
			})
			return res
		}
		defer rows.Close()

		activeIPs := 0
		ispPoolsWithIPs := make(map[string]bool)
		for rows.Next() {
			var hostname, status, poolName string
			rows.Scan(&hostname, &status, &poolName)
			activeIPs++
			ispPoolsWithIPs[poolName] = true
			vmta := hostname
			if dotIdx := strings.Index(vmta, "."); dotIdx > 0 {
				vmta = vmta[:dotIdx]
			}
			if len(vmta) < 2 || strings.Contains(vmta, ".") {
				res.Warnings = append(res.Warnings, preflightError{
					Check:   "vmta_name",
					Message: fmt.Sprintf("IP hostname %q produces suspicious VMTA name %q", hostname, vmta),
				})
			}
		}
		if activeIPs == 0 {
			poolDesc := ipPool.String
			if pfx != "" {
				poolDesc = pfx + "-*"
			}
			res.OK = false
			res.Errors = append(res.Errors, preflightError{
				Check:   "ip_pool_empty",
				Message: fmt.Sprintf("IP pools matching %s have zero active/warmup IPs", poolDesc),
			})
			return res
		}
		if pfx != "" && len(ispPoolsWithIPs) < 9 {
			res.Warnings = append(res.Warnings, preflightError{
				Check:   "isp_pool_coverage",
				Message: fmt.Sprintf("only %d of 9 ISP pools have active IPs for prefix %s", len(ispPoolsWithIPs), pfx),
			})
		}

		// 3. DKIM DNS record exists (apex DKIM at the sending domain).
		// SES tenant routes skip this because SES Easy DKIM keys live at
		// <token>._domainkey.em.<brand>, not at dkim._domainkey.<sending_domain>.
		dkimHost := "dkim._domainkey." + sendingDomain
		txts, dkimErr := net.LookupTXT(dkimHost)
		hasDKIM := false
		if dkimErr == nil {
			for _, txt := range txts {
				if strings.Contains(txt, "v=DKIM1") || strings.Contains(txt, "p=") {
					hasDKIM = true
					break
				}
			}
		}
		if !hasDKIM {
			res.OK = false
			res.Errors = append(res.Errors, preflightError{
				Check:   "dkim_dns",
				Message: fmt.Sprintf("no DKIM record found at %s — emails will fail DMARC", dkimHost),
			})
		}

		// 4. SPF record exists. SES tenant routes skip this because the
		// envelope sender (Return-Path) is rewritten to feedback-smtp.<region>.amazonses.com,
		// so SPF on the m.<brand> sending domain isn't what the recipient checks.
		txts, spfErr := net.LookupTXT(sendingDomain)
		hasSPF := false
		spfCount := 0
		if spfErr == nil {
			for _, txt := range txts {
				if strings.HasPrefix(strings.TrimSpace(txt), "v=spf1") {
					hasSPF = true
					spfCount++
				}
			}
		}
		if !hasSPF {
			res.Warnings = append(res.Warnings, preflightError{
				Check:   "spf_dns",
				Message: fmt.Sprintf("no SPF record found for %s", sendingDomain),
			})
		}
		if spfCount > 1 {
			res.OK = false
			res.Errors = append(res.Errors, preflightError{
				Check:   "spf_duplicate",
				Message: fmt.Sprintf("%s has %d SPF records — this causes a permerror; remove duplicates", sendingDomain, spfCount),
			})
		}
	}

	// 5. PMTA server is reachable (SMTP port check — use profile's smtp_port, not hardcoded 25)
	var smtpHost string
	var smtpPort int
	db.QueryRowContext(ctx, `
		SELECT smtp_host, COALESCE(smtp_port, 587) FROM mailing_sending_profiles
		WHERE id = $1`, profileID.String).Scan(&smtpHost, &smtpPort)
	if smtpHost != "" {
		addr := net.JoinHostPort(smtpHost, fmt.Sprintf("%d", smtpPort))
		conn, dialErr := net.DialTimeout("tcp", addr, 5*time.Second)
		if dialErr != nil {
			res.Warnings = append(res.Warnings, preflightError{
				Check:   "pmta_reachable",
				Message: fmt.Sprintf("PMTA SMTP unreachable at %s — %v (may be expected from ECS)", addr, dialErr),
			})
		} else {
			conn.Close()
		}
	}

	return res
}

// HandlePreflightCheck exposes the infrastructure readiness check as a GET endpoint.
func (s *Server) HandlePreflightCheck(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "domain parameter required"})
		return
	}
	orgID := getOrganizationID(r)
	overrideProfileID := r.URL.Query().Get("sending_profile_id")
	result := preflightDeployCheck(r.Context(), s.mailingDB, orgID, domain, overrideProfileID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// isUserExplicitSpan returns true when the time-span source indicates the user
// explicitly chose the delivery window duration in the PMTA wizard. When true,
// the minimum-span safety check is skipped — the user accepts the volume spike.
func isUserExplicitSpan(source string) bool {
	switch source {
	case "duration-calc", "manual":
		return true
	}
	return false
}

// waveSanityCheck validates the normalized wave plan meets minimum throttling
// requirements. Called after normalizePMTACampaignInput + buildPMTAWaveSpecs.
//
// The minimum span is proportional to the actual recipient count (sum of
// PlannedRecipients across waves), not the quota. The quota is the warmup
// ceiling and can be much larger than the actual list size for a given ISP.
//
// When the user explicitly set the delivery window (Source = "duration-calc"
// or "manual"), the minimum-span enforcement is skipped for that ISP — the
// user is deliberately choosing a shorter window.
func waveSanityCheck(plans []pmtaNormalizedPlan, wavesByISP map[string][]pmtaWaveSpec) error {
	const smallISPThreshold = 500
	var issues []string
	for _, plan := range plans {
		isp := plan.ISP
		waves := wavesByISP[isp]
		if len(waves) == 0 {
			continue
		}
		actualRecipients := 0
		for _, w := range waves {
			actualRecipients += w.PlannedRecipients
		}
		if actualRecipients < smallISPThreshold {
			continue
		}
		if len(waves) < minWavesPerISP {
			issues = append(issues, fmt.Sprintf("ISP %s has only %d waves (min %d)", isp, len(waves), minWavesPerISP))
		}
		if len(waves) >= 2 {
			userExplicit := false
			for _, ts := range plan.TimeSpans {
				if isUserExplicitSpan(ts.Source) {
					userExplicit = true
					break
				}
			}
			if !userExplicit {
				first := waves[0].ScheduledAt
				last := waves[len(waves)-1].ScheduledAt
				span := last.Sub(first)
				minSpan := minSpanForVolume(actualRecipients)
				if span < minSpan-15*time.Minute {
					issues = append(issues, fmt.Sprintf("ISP %s wave span is %v (min %v, %d recipients)", isp, span.Round(time.Minute), minSpan.Round(time.Minute), actualRecipients))
				}
			}
		}
	}
	if len(issues) > 0 {
		return fmt.Errorf("wave sanity check failed: %s", strings.Join(issues, "; "))
	}
	return nil
}

// minSpanForVolume computes the minimum delivery window for a given recipient
// count. 100 msgs/hr is the warmup-safe baseline. The result is clamped
// between 1 hour and defaultThrottleDuration (8h).
func minSpanForVolume(recipients int) time.Duration {
	const conservativeHourlyRate = 100
	if recipients <= 0 {
		return defaultThrottleDuration
	}
	proportional := time.Duration(float64(recipients) / float64(conservativeHourlyRate) * float64(time.Hour))
	if proportional > defaultThrottleDuration {
		return defaultThrottleDuration
	}
	if proportional < 1*time.Hour {
		return 1 * time.Hour
	}
	return proportional
}

// deriveSendingDomainVariants returns a slice containing the input domain
// plus its em./m. siblings. For example, "em.discountblog.com" produces
// ["em.discountblog.com", "m.discountblog.com"]. This ensures remail gap
// lookups cover all sending domain variants for a given brand.
func deriveSendingDomainVariants(domain string) []string {
	prefixes := []string{"em.", "m."}
	var baseDomain string
	for _, p := range prefixes {
		if strings.HasPrefix(domain, p) {
			baseDomain = domain[len(p):]
			break
		}
	}
	if baseDomain == "" {
		return []string{domain}
	}
	variants := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		variants = append(variants, p+baseDomain)
	}
	return variants
}
