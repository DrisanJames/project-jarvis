package domainagent

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ISP posture levels, worst first.
const (
	PostureBlocked = "blocked"
	PostureRepair  = "repair"
	PostureWatch   = "watch"
	PostureHealthy = "healthy"
	PostureUnknown = "unknown"
)

// postureRank orders postures by severity (lower = worse).
var postureRank = map[string]int{
	PostureBlocked: 0,
	PostureRepair:  1,
	PostureWatch:   2,
	PostureHealthy: 3,
	PostureUnknown: 4,
}

// WorsePosture returns the more severe of two postures.
func WorsePosture(a, b string) string {
	ra, ok := postureRank[a]
	if !ok {
		ra = postureRank[PostureUnknown]
	}
	rb, ok := postureRank[b]
	if !ok {
		rb = postureRank[PostureUnknown]
	}
	if rb < ra {
		return b
	}
	return a
}

// ISPPosture is the per-ISP health verdict inside a briefing.
type ISPPosture struct {
	ISP              string   `json:"isp"`
	Posture          string   `json:"posture"`
	OpenPct          float64  `json:"open_pct"`
	HumanClicks      int      `json:"human_clicks"`
	MachineOpenShare float64  `json:"machine_open_share"`
	Sends            int      `json:"sends"`
	Reasons          []string `json:"reasons"`
}

// Briefing is the deterministic v1 recommendation document attached to a plan.
type Briefing struct {
	Domain         string       `json:"domain"`
	WindowDays     int          `json:"window_days"`
	GeneratedAt    time.Time    `json:"generated_at"`
	Postures       []ISPPosture `json:"postures"`
	Flags          []string     `json:"flags"`
	VolumeGuidance string       `json:"volume_guidance"`
	Rationale      string       `json:"rationale"`
}

// ClassifyPosture applies the v1 posture rules to an ISP's windowed totals.
// Shared by GenerateBriefing and the /domains posture_worst recompute so the
// thresholds cannot drift between the two surfaces.
func ClassifyPosture(sends, humanOpens, machineOpens, bounces int) (string, []string) {
	openPct := 0.0
	if sends > 0 {
		openPct = 100 * float64(humanOpens) / float64(sends)
	}
	if sends >= 500 && humanOpens == 0 && bounces > 0 {
		return PostureBlocked, []string{"zero opens with bounces — exclude from sends"}
	}
	if sends >= 1000 {
		var reasons []string
		if openPct < 12 {
			reasons = append(reasons, fmt.Sprintf("human open rate %.1f%% below 12%% repair floor", openPct))
		}
		if machineOpens >= humanOpens {
			reasons = append(reasons, "machine opens meet or exceed human opens")
		}
		if len(reasons) > 0 {
			return PostureRepair, reasons
		}
	}
	if openPct < 25 {
		return PostureWatch, []string{fmt.Sprintf("human open rate %.1f%% below 25%% watch threshold", openPct)}
	}
	return PostureHealthy, nil
}

// GenerateBriefing aggregates the scorecard over the last `days` days
// (default 7) for one sending domain and produces a deterministic briefing.
func GenerateBriefing(ctx context.Context, db *sql.DB, orgID, domain string, days int) (Briefing, error) {
	if days <= 0 {
		days = 7
	}
	today := utcDate(time.Now())
	from := today.AddDate(0, 0, -(days - 1))

	briefing := Briefing{
		Domain:      strings.ToLower(strings.TrimSpace(domain)),
		WindowDays:  days,
		GeneratedAt: time.Now().UTC(),
		Postures:    []ISPPosture{},
		Flags:       []string{},
	}

	// Per-ISP window aggregates.
	type ispAgg struct {
		sends, humanOpens, machineOpens, humanClicks, bounces int
	}
	aggs := map[string]*ispAgg{}
	rows, err := db.QueryContext(ctx, `
		SELECT isp, SUM(sends), SUM(human_opens), SUM(machine_opens), SUM(human_clicks),
		       SUM(hard_bounces + soft_bounces + other_bounces)
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND sending_domain = $2 AND day >= $3
		GROUP BY isp
	`, orgID, briefing.Domain, from)
	if err != nil {
		return briefing, fmt.Errorf("briefing isp aggregate: %w", err)
	}
	for rows.Next() {
		var name string
		a := &ispAgg{}
		if err := rows.Scan(&name, &a.sends, &a.humanOpens, &a.machineOpens, &a.humanClicks, &a.bounces); err != nil {
			rows.Close()
			return briefing, fmt.Errorf("briefing isp scan: %w", err)
		}
		aggs[name] = a
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return briefing, fmt.Errorf("briefing isp rows: %w", err)
	}
	rows.Close()

	// Per-day bounce totals — used for the 3d-vs-prior-3d bounce trend and
	// the missing-day gap flag.
	dayBounces := map[string]int{}
	dayRows, err := db.QueryContext(ctx, `
		SELECT day, SUM(hard_bounces + soft_bounces + other_bounces)
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND sending_domain = $2 AND day >= $3
		GROUP BY day
	`, orgID, briefing.Domain, from)
	if err != nil {
		return briefing, fmt.Errorf("briefing day aggregate: %w", err)
	}
	for dayRows.Next() {
		var d time.Time
		var bounces int
		if err := dayRows.Scan(&d, &bounces); err != nil {
			dayRows.Close()
			return briefing, fmt.Errorf("briefing day scan: %w", err)
		}
		dayBounces[utcDate(d).Format("2006-01-02")] = bounces
	}
	if err := dayRows.Err(); err != nil {
		dayRows.Close()
		return briefing, fmt.Errorf("briefing day rows: %w", err)
	}
	dayRows.Close()

	// Classify each ISP.
	totalHumanOpens, totalHumanClicks := 0, 0
	postureCounts := map[string]int{}
	anyBlocked := false
	for name, a := range aggs {
		posture, reasons := ClassifyPosture(a.sends, a.humanOpens, a.machineOpens, a.bounces)
		openPct := 0.0
		if a.sends > 0 {
			openPct = 100 * float64(a.humanOpens) / float64(a.sends)
		}
		machineShare := 0.0
		if a.humanOpens+a.machineOpens > 0 {
			machineShare = float64(a.machineOpens) / float64(a.humanOpens+a.machineOpens)
		}
		if reasons == nil {
			reasons = []string{}
		}
		briefing.Postures = append(briefing.Postures, ISPPosture{
			ISP:              name,
			Posture:          posture,
			OpenPct:          openPct,
			HumanClicks:      a.humanClicks,
			MachineOpenShare: machineShare,
			Sends:            a.sends,
			Reasons:          reasons,
		})
		postureCounts[posture]++
		if posture == PostureBlocked {
			anyBlocked = true
		}
		totalHumanOpens += a.humanOpens
		totalHumanClicks += a.humanClicks
	}
	// Deterministic order: severity worst-first, then volume, then name.
	sort.Slice(briefing.Postures, func(i, j int) bool {
		pi, pj := briefing.Postures[i], briefing.Postures[j]
		if postureRank[pi.Posture] != postureRank[pj.Posture] {
			return postureRank[pi.Posture] < postureRank[pj.Posture]
		}
		if pi.Sends != pj.Sends {
			return pi.Sends > pj.Sends
		}
		return pi.ISP < pj.ISP
	})

	// Bounce trend: last 3 days vs the prior 3 days.
	last3, prior3 := 0, 0
	for i := 0; i < 3; i++ {
		last3 += dayBounces[today.AddDate(0, 0, -i).Format("2006-01-02")]
		prior3 += dayBounces[today.AddDate(0, 0, -(i+3)).Format("2006-01-02")]
	}
	bounceSpike := last3 > 2*prior3 && last3 > 0

	if anyBlocked || bounceSpike {
		briefing.VolumeGuidance = "hold"
	} else {
		briefing.VolumeGuidance = "+20% eligible"
	}

	// Flags.
	if float64(totalHumanClicks) > 0.5*float64(totalHumanOpens) && totalHumanClicks > 0 {
		briefing.Flags = append(briefing.Flags, "click bot-filter suspect: human clicks implausibly high vs opens")
	}
	missingDays := 0
	for d := from; !d.After(today); d = d.AddDate(0, 0, 1) {
		if _, ok := dayBounces[d.Format("2006-01-02")]; !ok {
			missingDays++
		}
	}
	if missingDays > 0 {
		briefing.Flags = append(briefing.Flags, fmt.Sprintf("scorecard gap: missing day(s) — %d of %d days have no rows", missingDays, days))
	}
	for _, p := range briefing.Postures {
		if p.Posture == PostureBlocked || p.Posture == PostureRepair {
			briefing.Flags = append(briefing.Flags, fmt.Sprintf("%s posture %s: %s", p.ISP, p.Posture, strings.Join(p.Reasons, "; ")))
		}
	}

	// Rationale: 2-4 plain sentences.
	var rationale []string
	if len(briefing.Postures) == 0 {
		rationale = append(rationale, fmt.Sprintf("No scorecard data for %s in the last %d days, so per-ISP posture is unknown.", briefing.Domain, days))
	} else {
		rationale = append(rationale, fmt.Sprintf(
			"Over the last %d days %s shows %d healthy, %d watch, %d repair, and %d blocked ISP families.",
			days, briefing.Domain,
			postureCounts[PostureHealthy], postureCounts[PostureWatch],
			postureCounts[PostureRepair], postureCounts[PostureBlocked]))
	}
	if bounceSpike {
		rationale = append(rationale, fmt.Sprintf("Bounces over the last 3 days (%d) are more than double the prior 3 days (%d).", last3, prior3))
	}
	if briefing.VolumeGuidance == "hold" {
		rationale = append(rationale, "Volume guidance is hold until blocked ISPs and bounce pressure are cleared.")
	} else {
		rationale = append(rationale, "No blocked ISPs and bounces are stable, so the domain is eligible for the +20% ramp.")
	}
	if len(briefing.Flags) > 0 {
		rationale = append(rationale, fmt.Sprintf("%d flag(s) need operator review before deploy.", len(briefing.Flags)))
	}
	briefing.Rationale = strings.Join(rationale, " ")

	return briefing, nil
}
