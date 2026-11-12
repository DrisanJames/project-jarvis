package worker

// SegmentGridWorker — ownership, pass schedule and pass ordering
// (operator 2026-11-11: "we're mailing to fresh segments daily"; every
// engagement segment for send day D must be fresh by 21:00 America/Denver on
// D-1).
//
// Everything here is OFF by default: with no env set the worker keeps its
// 05:30 UTC once-per-UTC-day target and the 20h ledger skip exactly as
// before, and the Postgres builders keep building grid segments.
//
//	SEGMENT_GRID_SOLE_OWNER=true  the grid worker is the ONLY builder of the
//	                              segments it owns: the nightly Postgres
//	                              materializer and the pre-dispatch refresher
//	                              stop rebuilding them (internal/api).
//	SEGMENT_GRID_DAILY_AT=HH:MM   daily pass at this America/Denver wall-clock
//	                              time (DST-safe) instead of 05:30 UTC.
//	SEGMENT_GRID_SECOND_AT=HH:MM  optional second pass (America/Denver); only
//	                              read when SEGMENT_GRID_DAILY_AT is set.

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

const (
	segmentGridSoleOwnerEnv = "SEGMENT_GRID_SOLE_OWNER"
	segmentGridDailyAtEnv   = "SEGMENT_GRID_DAILY_AT"
	segmentGridSecondAtEnv  = "SEGMENT_GRID_SECOND_AT"
	segmentGridScheduleTZ   = "America/Denver"

	// segmentGridPriorityHorizon: segments included by a draft/scheduled
	// campaign anchored inside this window are built first in a pass.
	segmentGridPriorityHorizon = 36 * time.Hour
)

// ---------------------------------------------------------------------------
// Ownership
// ---------------------------------------------------------------------------

// GridOwnsSegment reports whether the grid worker builds this segment: the
// grid NAME pattern AND conditions the lake build can express. It is the same
// pair enumerateGridSegments applies, so a name-matching row whose conditions
// fail closed is NOT owned and stays with the Postgres materializer.
func GridOwnsSegment(name, conditionsRaw string) bool {
	if !gridNameRe.MatchString(name) {
		return false
	}
	_, err := parseGridConditions(name, conditionsRaw)
	return err == nil
}

func segmentGridWorkerDisabled() bool {
	return os.Getenv("DISABLE_SEGMENT_GRID_WORKER") == "true"
}

// GridSoleOwnerEnabled reports whether the Postgres builders must leave
// grid-owned segments to the grid worker. It needs the opt-in env AND a grid
// worker that can actually build (not kill-switched, lake reader up) —
// otherwise nobody would build those segments.
func GridSoleOwnerEnabled() bool {
	return gridSoleOwnerEnabled(os.Getenv, analytics.ReaderEnabled)
}

func gridSoleOwnerEnabled(getenv func(string) string, readerEnabled func() bool) bool {
	if !strings.EqualFold(strings.TrimSpace(getenv(segmentGridSoleOwnerEnv)), "true") {
		return false
	}
	if getenv("DISABLE_SEGMENT_GRID_WORKER") == "true" {
		return false
	}
	return readerEnabled()
}

// ---------------------------------------------------------------------------
// Pass schedule
// ---------------------------------------------------------------------------

// gridClock is a wall-clock time of day.
type gridClock struct{ hour, min int }

func parseGridClock(raw string) (gridClock, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(raw))
	if err != nil {
		return gridClock{}, false
	}
	return gridClock{t.Hour(), t.Minute()}, true
}

// gridSchedule is the env-configured pass schedule. loc == nil is the legacy
// schedule (05:30 UTC, once per UTC day) and every caller must then take the
// pre-existing code path untouched.
type gridSchedule struct {
	loc    *time.Location
	clocks []gridClock
}

// loadGridSchedule reads the schedule env. Anything unset or unparseable
// yields the legacy schedule — a typo must never move or stop the pass.
func loadGridSchedule(getenv func(string) string) gridSchedule {
	raw := strings.TrimSpace(getenv(segmentGridDailyAtEnv))
	second := strings.TrimSpace(getenv(segmentGridSecondAtEnv))
	if raw == "" {
		if second != "" {
			log.Printf("[SegmentGrid] %s ignored: it needs %s", segmentGridSecondAtEnv, segmentGridDailyAtEnv)
		}
		return gridSchedule{}
	}
	first, ok := parseGridClock(raw)
	if !ok {
		log.Printf("[SegmentGrid] invalid %s=%q (want HH:MM) — keeping the %02d:%02dZ default",
			segmentGridDailyAtEnv, raw, segmentGridTargetHourUTC, segmentGridTargetMinUTC)
		return gridSchedule{}
	}
	loc, err := time.LoadLocation(segmentGridScheduleTZ)
	if err != nil {
		log.Printf("[SegmentGrid] %s unavailable (%v) — keeping the %02d:%02dZ default",
			segmentGridScheduleTZ, err, segmentGridTargetHourUTC, segmentGridTargetMinUTC)
		return gridSchedule{}
	}
	s := gridSchedule{loc: loc, clocks: []gridClock{first}}
	if second != "" {
		if c, ok := parseGridClock(second); ok && c != first {
			s.clocks = append(s.clocks, c)
		} else {
			log.Printf("[SegmentGrid] invalid %s=%q (want HH:MM, different from %s) — no second pass",
				segmentGridSecondAtEnv, second, segmentGridDailyAtEnv)
		}
	}
	return s
}

// lastTarget returns the most recent scheduled instant at or before now (zero
// when the schedule is legacy). Built with time.Date in the schedule's
// location, so it follows the wall clock across DST changes.
func (s gridSchedule) lastTarget(now time.Time) time.Time {
	var best time.Time
	if s.loc == nil {
		return best
	}
	n := now.In(s.loc)
	for _, c := range s.clocks {
		for _, back := range []int{0, 1} {
			t := time.Date(n.Year(), n.Month(), n.Day()-back, c.hour, c.min, 0, 0, s.loc)
			if !t.After(now) {
				if t.After(best) {
					best = t
				}
				break
			}
		}
	}
	return best
}

func (s gridSchedule) describe() string {
	if s.loc == nil {
		return ""
	}
	parts := make([]string, len(s.clocks))
	for i, c := range s.clocks {
		parts[i] = time.Date(2000, 1, 1, c.hour, c.min, 0, 0, time.UTC).Format("15:04")
	}
	return strings.Join(parts, " + ") + " " + segmentGridScheduleTZ
}

// ---------------------------------------------------------------------------
// Pass ordering
// ---------------------------------------------------------------------------

// loadCampaignSegmentIDs returns the segments included by a draft/scheduled
// campaign anchored in the next segmentGridPriorityHorizon.
func (w *SegmentGridWorker) loadCampaignSegmentIDs(ctx context.Context) (map[string]bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(cctx, `
		SELECT DISTINCT a.segment_id::text
		FROM mailing_campaigns c
		JOIN mailing_campaign_audiences a ON a.campaign_id = c.id AND a.role = 'include'
		WHERE c.status IN ('draft','scheduled')
		  AND c.scheduled_at > NOW()
		  AND c.scheduled_at <= NOW() + ($1 * INTERVAL '1 second')
	`, int(segmentGridPriorityHorizon.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// gridOrderByPriority moves the segments in first to the front, keeping the
// existing (name) order inside each group.
func gridOrderByPriority(need []gridSegmentRow, first map[string]bool) []gridSegmentRow {
	if len(first) == 0 {
		return need
	}
	out := make([]gridSegmentRow, 0, len(need))
	for _, s := range need {
		if first[s.spec.ID] {
			out = append(out, s)
		}
	}
	for _, s := range need {
		if !first[s.spec.ID] {
			out = append(out, s)
		}
	}
	return out
}
