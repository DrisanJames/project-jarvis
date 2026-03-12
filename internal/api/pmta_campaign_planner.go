package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const (
	pmtaExecutionModeStandard = "standard"
	pmtaExecutionModeWave     = "pmta_isp_wave"

	// Mandatory throttle defaults. Every campaign MUST be spread across a
	// delivery window — single-wave blasts are never acceptable.
	defaultThrottleDuration = 4 * time.Hour
	defaultCadenceMinutes   = 15
	minWavesPerISP          = 4
)

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
}

type pmtaAudiencePlan struct {
	RecipientsByISP  map[string][]pmtaSelectedRecipient
	CountsByISP      map[string]int
	TotalSeen        int
	AfterSuppression int
	SelectedTotal    int
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
			if baseStart.Before(now.Add(5 * time.Minute)) {
				return pmtaNormalizedCampaign{}, fmt.Errorf("scheduled_at must be at least 5 minutes in the future")
			}
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
			normalized.Plans = append(normalized.Plans, plan)
			if !targetSeen[plan.ISP] {
				targetSeen[plan.ISP] = true
				if isCanonicalISP(plan.ISP) {
					normalized.TargetISPs = append(normalized.TargetISPs, engine.ISP(plan.ISP))
				}
			}
		}
		if len(normalized.Plans) == 0 {
			return pmtaNormalizedCampaign{}, fmt.Errorf("at least one isp_plan is required")
		}
		if len(normalized.TargetISPs) == 0 && len(input.TargetISPs) > 0 {
			normalized.TargetISPs = append(normalized.TargetISPs, input.TargetISPs...)
		}
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
) (pmtaAudiencePlan, error) {
	globalSuppSet := make(map[string]bool)
	gsRows, gsErr := db.QueryContext(ctx, "SELECT md5_hash FROM mailing_global_suppressions")
	if gsErr == nil {
		defer gsRows.Close()
		for gsRows.Next() {
			var h string
			if gsRows.Scan(&h) == nil {
				globalSuppSet[strings.ToLower(h)] = true
			}
		}
	}

	exclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.ExclusionLists)
	for _, slID := range exclusionIDs {
		slRows, slErr := db.QueryContext(ctx, "SELECT md5_hash FROM mailing_suppression_entries WHERE list_id = $1", slID)
		if slErr != nil {
			continue
		}
		var hashes []string
		for slRows.Next() {
			var h string
			if slRows.Scan(&h) == nil {
				hashes = append(hashes, h)
			}
		}
		slRows.Close()
		if len(hashes) > 0 {
			suppMatcher.LoadList(slID, hashes)
		}
	}

	exclusionSegEmails, err := loadExclusionSegmentEmails(ctx, db, input.ExclusionSegments)
	if err != nil {
		return pmtaAudiencePlan{}, err
	}

	allowedISPs := make(map[string]bool, len(normalized.Plans))
	planMap := make(map[string]pmtaNormalizedPlan, len(normalized.Plans))
	for _, plan := range normalized.Plans {
		allowedISPs[plan.ISP] = true
		planMap[plan.ISP] = plan
	}

	inclusionIDs := resolveListNamesToIDs(ctx, db, orgID, input.InclusionLists)
	var qualified []pmtaSelectedRecipient
	seenEmails := make(map[string]bool)
	selectionRank := 0

	qualifyEmail := func(subID, email, sourceType, sourceID string) bool {
		emailLower := strings.ToLower(strings.TrimSpace(email))
		if emailLower == "" || seenEmails[emailLower] {
			return false
		}
		seenEmails[emailLower] = true
		hash := md5.Sum([]byte(emailLower))
		md5Hex := hex.EncodeToString(hash[:])
		if globalSuppSet[md5Hex] {
			return false
		}
		if len(exclusionIDs) > 0 && suppMatcher.IsSuppressed(emailLower, exclusionIDs) {
			return false
		}
		if exclusionSegEmails[emailLower] {
			return false
		}
		domain := emailLower
		if idx := strings.LastIndex(emailLower, "@"); idx >= 0 {
			domain = emailLower[idx+1:]
		}
		isp := domainToISPLookup(domain)
		if len(allowedISPs) > 0 && !allowedISPs[isp] {
			return false
		}
		selectionRank++
		qualified = append(qualified, pmtaSelectedRecipient{
			SubscriberID:  subID,
			Email:         emailLower,
			ISP:           isp,
			SourceType:    sourceType,
			SourceID:      sourceID,
			SelectionRank: selectionRank,
		})
		return true
	}

	streamList := func(listID string) error {
		rows, err := db.QueryContext(ctx, `
			SELECT s.id::text, s.email
			FROM mailing_subscribers s
			WHERE s.list_id = $1 AND s.status IN ('active','confirmed')
			ORDER BY s.created_at ASC, s.id ASC
		`, listID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var subID, email string
			if rows.Scan(&subID, &email) == nil {
				qualifyEmail(subID, email, "list", listID)
			}
		}
		return nil
	}

	streamSegment := func(segmentID string) error {
		var segListID *string
		var conditionsRaw sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT list_id::text, conditions::text FROM mailing_segments WHERE id = $1`, segmentID,
		).Scan(&segListID, &conditionsRaw); err != nil {
			return err
		}
		var listIDVal interface{}
		if segListID != nil && *segListID != "" {
			listIDVal = *segListID
		}
		query, args := buildSegmentQuery(conditionsRaw.String, listIDVal)
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var subID, email string
			if rows.Scan(&subID, &email) == nil {
				qualifyEmail(subID, email, "segment", segmentID)
			}
		}
		return nil
	}

	if len(input.SendPriority) > 0 {
		for _, item := range input.SendPriority {
			switch item.Type {
			case "list":
				resolved := resolveListNamesToIDs(ctx, db, orgID, []string{item.ID})
				for _, listID := range resolved {
					if err := streamList(listID); err != nil {
						return pmtaAudiencePlan{}, err
					}
				}
			case "segment":
				if err := streamSegment(item.ID); err != nil {
					return pmtaAudiencePlan{}, err
				}
			}
		}
	} else {
		for _, listID := range inclusionIDs {
			if err := streamList(listID); err != nil {
				return pmtaAudiencePlan{}, err
			}
		}
		for _, segmentID := range input.InclusionSegments {
			if err := streamSegment(segmentID); err != nil {
				return pmtaAudiencePlan{}, err
			}
		}
	}

	recipientsByISP := make(map[string][]pmtaSelectedRecipient, len(normalized.Plans))
	for _, rec := range qualified {
		recipientsByISP[rec.ISP] = append(recipientsByISP[rec.ISP], rec)
	}

	selectedByISP := make(map[string][]pmtaSelectedRecipient, len(normalized.Plans))
	countsByISP := make(map[string]int, len(normalized.Plans))
	selectedTotal := 0
	for isp, plan := range planMap {
		recipients := append([]pmtaSelectedRecipient(nil), recipientsByISP[isp]...)
		if plan.RandomizeAudience && len(recipients) > 1 {
			rand.Shuffle(len(recipients), func(i, j int) {
				recipients[i], recipients[j] = recipients[j], recipients[i]
			})
		}
		if plan.Quota > 0 && len(recipients) > plan.Quota {
			recipients = recipients[:plan.Quota]
		}
		selectedByISP[isp] = recipients
		countsByISP[isp] = len(recipients)
		selectedTotal += len(recipients)
	}

	return pmtaAudiencePlan{
		RecipientsByISP:  selectedByISP,
		CountsByISP:      countsByISP,
		TotalSeen:        len(seenEmails),
		AfterSuppression: len(qualified),
		SelectedTotal:    selectedTotal,
	}, nil
}

func loadExclusionSegmentEmails(ctx context.Context, db dbQuerier, segmentIDs []string) (map[string]bool, error) {
	emails := make(map[string]bool)
	for _, segmentID := range segmentIDs {
		var segListID *string
		var conditionsRaw sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT list_id::text, conditions::text FROM mailing_segments WHERE id = $1`, segmentID,
		).Scan(&segListID, &conditionsRaw); err != nil {
			return nil, err
		}
		var listIDVal interface{}
		if segListID != nil && *segListID != "" {
			listIDVal = *segListID
		}
		query, args := buildSegmentQuery(conditionsRaw.String, listIDVal)
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var subID, email string
			if rows.Scan(&subID, &email) == nil {
				emails[strings.ToLower(strings.TrimSpace(email))] = true
			}
		}
		rows.Close()
	}
	return emails, nil
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

func coalesceString(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func isCanonicalISP(isp string) bool {
	switch isp {
	case string(engine.ISPGmail), string(engine.ISPYahoo), string(engine.ISPMicrosoft),
		string(engine.ISPApple), string(engine.ISPComcast), string(engine.ISPAtt),
		string(engine.ISPCox), string(engine.ISPCharter):
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
func preflightDeployCheck(ctx context.Context, db *sql.DB, orgID string, sendingDomain string) preflightResult {
	res := preflightResult{OK: true}

	// 1. Sending profile exists with an IP pool
	var profileID, ipPool sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT id::text, ip_pool
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta'
		  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
		  AND status = 'active'
		ORDER BY created_at DESC LIMIT 1
	`, orgID, sendingDomain).Scan(&profileID, &ipPool)
	if err != nil || !profileID.Valid {
		res.OK = false
		res.Errors = append(res.Errors, preflightError{
			Check:   "sending_profile",
			Message: fmt.Sprintf("no active PMTA sending profile found for domain %s", sendingDomain),
		})
		return res
	}
	if !ipPool.Valid || strings.TrimSpace(ipPool.String) == "" {
		res.OK = false
		res.Errors = append(res.Errors, preflightError{
			Check:   "ip_pool",
			Message: fmt.Sprintf("sending profile %s has no IP pool assigned", profileID.String),
		})
		return res
	}

	// 2. IP pool has active IPs with valid VMTA names
	rows, qErr := db.QueryContext(ctx, `
		SELECT ip.hostname, ip.status
		FROM mailing_ip_addresses ip
		JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
		WHERE pool.name = $1
		  AND ip.status IN ('active', 'warmup')
		  AND pool.status = 'active'
	`, ipPool.String)
	if qErr != nil {
		res.OK = false
		res.Errors = append(res.Errors, preflightError{
			Check:   "ip_pool_query",
			Message: fmt.Sprintf("failed to query IP pool %s: %v", ipPool.String, qErr),
		})
		return res
	}
	defer rows.Close()

	activeIPs := 0
	for rows.Next() {
		var hostname, status string
		rows.Scan(&hostname, &status)
		activeIPs++
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
		res.OK = false
		res.Errors = append(res.Errors, preflightError{
			Check:   "ip_pool_empty",
			Message: fmt.Sprintf("IP pool %s has zero active/warmup IPs", ipPool.String),
		})
		return res
	}

	// 3. DKIM DNS record exists
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

	// 4. SPF record exists
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

	// 5. PMTA server is reachable (SMTP port check — use profile's smtp_port, not hardcoded 25)
	var smtpHost string
	var smtpPort int
	db.QueryRowContext(ctx, `
		SELECT smtp_host, COALESCE(smtp_port, 587) FROM mailing_sending_profiles
		WHERE id = $1`, profileID.String).Scan(&smtpHost, &smtpPort)
	if smtpHost != "" {
		addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)
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

// waveSanityCheck validates the normalized wave plan meets minimum throttling
// requirements. Called after normalizePMTACampaignInput + buildPMTAWaveSpecs.
func waveSanityCheck(plans []pmtaNormalizedPlan, wavesByISP map[string][]pmtaWaveSpec) error {
	var issues []string
	for _, plan := range plans {
		isp := plan.ISP
		waves := wavesByISP[isp]
		if len(waves) == 0 {
			continue // no audience for this ISP — nothing to validate
		}
		if len(waves) < minWavesPerISP {
			issues = append(issues, fmt.Sprintf("ISP %s has only %d waves (min %d)", isp, len(waves), minWavesPerISP))
		}
		if len(waves) >= 2 {
			first := waves[0].ScheduledAt
			last := waves[len(waves)-1].ScheduledAt
			span := last.Sub(first)
			if span < defaultThrottleDuration-15*time.Minute {
				issues = append(issues, fmt.Sprintf("ISP %s wave span is %v (min %v)", isp, span.Round(time.Minute), defaultThrottleDuration))
			}
		}
	}
	if len(issues) > 0 {
		return fmt.Errorf("wave sanity check failed: %s", strings.Join(issues, "; "))
	}
	return nil
}
