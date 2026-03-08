package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const (
	pmtaExecutionModeStandard = "standard"
	pmtaExecutionModeWave     = "pmta_isp_wave"
)

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
					Mode:      "single",
					BatchSize: quotaMap[ispName],
				},
				TimeSpans: []pmtaNormalizedTimeSpan{{
					StartAt:  baseStart,
					EndAt:    baseStart,
					Timezone: defaultTZ,
					Source:   "legacy_translation",
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
	if cadence.Mode == "" {
		cadence.Mode = "single"
	}
	if cadence.Mode != "single" && cadence.Mode != "interval" {
		return pmtaNormalizedPlan{}, fmt.Errorf("isp_plan cadence mode for %s must be 'single' or 'interval'", isp)
	}
	if cadence.Mode == "interval" && cadence.EveryMinutes <= 0 {
		return pmtaNormalizedPlan{}, fmt.Errorf("isp_plan cadence every_minutes for %s must be > 0", isp)
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
			EndAt:    startAt,
			Timezone: timezone,
			Source:   "default",
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
	db *sql.DB,
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

func loadExclusionSegmentEmails(ctx context.Context, db *sql.DB, segmentIDs []string) (map[string]bool, error) {
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

	batchSize := plan.Cadence.BatchSize
	if batchSize <= 0 || batchSize > recipientCount {
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
		if len(plan.TimeSpans) == 0 {
			plan.TimeSpans = []pmtaNormalizedTimeSpan{{
				StartAt: time.Now().UTC(),
				EndAt:   time.Now().UTC(),
			}}
		}
		for idx, span := range plan.TimeSpans {
			if remaining <= 0 {
				break
			}
			planned := remaining
			if len(plan.TimeSpans)-idx > 1 {
				planned = int(float64(remaining) / float64(len(plan.TimeSpans)-idx))
				if planned == 0 {
					planned = remaining
				}
			}
			waves = append(waves, pmtaWaveSpec{
				WaveNumber:        len(waves) + 1,
				ScheduledAt:       span.StartAt,
				WindowStartAt:     span.StartAt,
				WindowEndAt:       span.EndAt,
				CadenceMinutes:    0,
				BatchSize:         planned,
				PlannedRecipients: planned,
				IdempotencyKey:    fmt.Sprintf("%s:%s:%d", campaignID, plan.ISP, len(waves)+1),
			})
			remaining -= planned
		}
		if remaining > 0 && len(waves) > 0 {
			waves[len(waves)-1].PlannedRecipients += remaining
		}
	}

	if len(waves) == 0 {
		waves = append(waves, pmtaWaveSpec{
			WaveNumber:        1,
			ScheduledAt:       time.Now().UTC(),
			WindowStartAt:     time.Now().UTC(),
			WindowEndAt:       time.Now().UTC(),
			BatchSize:         recipientCount,
			PlannedRecipients: recipientCount,
			IdempotencyKey:    fmt.Sprintf("%s:%s:%d", campaignID, plan.ISP, 1),
		})
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
