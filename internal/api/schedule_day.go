package api

// Send-Day Schedule (day-clock) — read-only view of a single send day's
// campaigns, one band per transport (SES, PMTA, Kumo) with every lane inside it,
// each with its send window, offer mix and recipient total. Backs the portal
// "Schedule" tab (ScheduleView.tsx).
//
// Campaigns are selected by their Denver send day and the lane/offer come from
// the canonical "<day> - <Brand> - <LANE>[ - <offer>]" name. The transport comes
// from the sending PROFILE (scheduleRoute), never the domain prefix: em.* mails
// on Kumo and PMTA alike, and NX lanes on PMTA and Kumo share one name shape.
// Bands are always all three, so an empty transport reads as empty, not missing.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ScheduleDayVersion = "2.0.0"

// scheduleLaneNames turns lane tokens into plain language (PORTAL = plain
// language, operator 2026-10-27). An unknown token keeps its words.
var scheduleLaneNames = strings.NewReplacer(
	"NL-GM-COLD-NEWSLETTER", "Gmail cold newsletter",
	"NL-GM-NEWSLETTER", "Gmail newsletter",
	"NL-COLD-NEWSLETTER", "Cold newsletter",
	"NL-ENG-NEWSLETTER", "Engaged newsletter",
	"NL-YF-NEWSLETTER", "Yahoo family newsletter",
	"NL-WARMUP-NEWSLETTER", "Warmup newsletter",
	"OFFER-WARMUP", "Warmup offer",
	"OFFER-ENG", "Engaged offer",
	"OFFER-YF", "Yahoo family offer",
	"KUMO-WARM", "Kumo warm",
	"-", " ",
)

var (
	scheduleDayRe     = regexp.MustCompile(`-D\d+`)
	scheduleReleaseRe = regexp.MustCompile(`-R\d+-W\d+$`)
	scheduleClockRe   = regexp.MustCompile(`^\d{4}(-|$)`)
)

// scheduleTransports fixes the band order.
var scheduleTransports = []struct{ key, label string }{
	{"ses", "SES"},
	{"pmta", "PMTA"},
	{"kumo", "Kumo"},
}

type scheduleLane struct {
	Name   string         `json:"name"`
	Label  string         `json:"label"`
	Start  string         `json:"start"`  // ISO8601 UTC (earliest campaign)
	Window int            `json:"window"` // hours
	N      int            `json:"n"`      // campaign count
	Rec    int64          `json:"rec"`    // sum total_recipients
	Offers map[string]int `json:"offers"` // offer token -> count
}

type scheduleBand struct {
	Key   string          `json:"key"`
	Label string          `json:"label"`
	N     int             `json:"n"`
	Rec   int64           `json:"rec"`
	Lanes []*scheduleLane `json:"lanes"`
}

type ispPlanEnvelope struct {
	IspPlans []struct {
		TimeSpans []map[string]interface{} `json:"time_spans"`
	} `json:"isp_plans"`
}

// windowHoursFromISPQuotas mirrors schedule_report.parse_window_hours: max(end)
// - min(start) across every plan's time_spans (accepts start/start_at/start_utc
// and end/end_at/end_utc). Returns 0 when nothing parseable.
func windowHoursFromISPQuotas(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var env ispPlanEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0
	}
	var minStart, maxEnd time.Time
	parse := func(v interface{}) (time.Time, bool) {
		s, ok := v.(string)
		if !ok || s == "" {
			return time.Time{}, false
		}
		s = strings.Replace(s, "Z", "+00:00", 1)
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-07:00", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	for _, p := range env.IspPlans {
		for _, ts := range p.TimeSpans {
			for _, k := range []string{"start", "start_at", "start_utc"} {
				if t, ok := parse(ts[k]); ok {
					if minStart.IsZero() || t.Before(minStart) {
						minStart = t
					}
				}
			}
			for _, k := range []string{"end", "end_at", "end_utc"} {
				if t, ok := parse(ts[k]); ok {
					if maxEnd.IsZero() || t.After(maxEnd) {
						maxEnd = t
					}
				}
			}
		}
	}
	if minStart.IsZero() || maxEnd.IsZero() || !maxEnd.After(minStart) {
		return 0
	}
	return int((maxEnd.Sub(minStart).Hours()) + 0.5)
}

// scheduleRoute names the transport a profile actually mails on. Kumo first:
// its profiles carry vendor_type='pmta'. Then SES, which includes the PMTA
// profiles that relay to SES (pool <code>-ses-pool or ses-relay-a).
func scheduleRoute(vendorType, routingMode, ipPool string, viaSES bool) string {
	switch {
	case routingMode == "kumo":
		return "kumo"
	case vendorType == "ses", viaSES, strings.Contains(strings.ToLower(ipPool), "ses"):
		return "ses"
	default:
		return "pmta"
	}
}

// scheduleLaneOffer splits a campaign name into its lane token and offer. Day
// and wave numbers leave the token so a lane is one row across the day. A trailing
// HHMM clock is not an offer; a board cell's offer IS its lane token.
func scheduleLaneOffer(name string) (lane, offer string) {
	parts := strings.Split(name, " - ")
	if len(parts) >= 3 {
		lane = strings.TrimSpace(parts[2])
	}
	if len(parts) >= 4 {
		offer = scheduleClockRe.ReplaceAllString(strings.TrimSpace(parts[3]), "")
	}
	if lane == "" {
		lane = "Other"
	}
	if offer == "" {
		offer = lane
	}
	lane = scheduleReleaseRe.ReplaceAllString(scheduleDayRe.ReplaceAllString(lane, ""), "")
	return lane, offer
}

// HandleScheduleDay GET /api/mailing/schedule/day?date=YYYY-MM-DD
func (s *Server) HandleScheduleDay(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context required")
		return
	}

	denver, _ := time.LoadLocation("America/Denver")
	if denver == nil {
		denver = time.UTC
	}
	dateStr := strings.TrimSpace(r.URL.Query().Get("date"))
	var day time.Time
	if dateStr == "" {
		now := time.Now().In(denver)
		day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, denver)
		dateStr = day.Format("2006-01-02")
	} else {
		day, err = time.ParseInLocation("2006-01-02", dateStr, denver)
		if err != nil {
			respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
	}

	const q = `
		SELECT c.name, c.scheduled_at, COALESCE(c.total_recipients,0),
		       COALESCE(p.vendor_type,''), COALESCE(p.routing_mode,''),
		       COALESCE(p.ip_pool,''), COALESCE(p.via_ses,false),
		       COALESCE(c.isp_quotas::text,'')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON c.sending_profile_id = p.id
		WHERE c.organization_id = $1
		  AND c.status NOT IN ('cancelled','draft','deleted')
		  AND c.name NOT LIKE '[partner-drip]%'
		  AND c.scheduled_at >= $2 AND c.scheduled_at < $3
		ORDER BY c.scheduled_at, c.name`
	dayEnd := day.AddDate(0, 0, 1)
	rows, err := s.mailingDB.QueryContext(ctx, q, orgID, day, dayEnd)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	defer rows.Close()

	bands := map[string]map[string]*scheduleLane{}
	ends := map[*scheduleLane]time.Time{} // lane -> latest campaign end
	var totalRec int64
	var totalN int
	offerSet := map[string]bool{}

	for rows.Next() {
		var name, vendorType, routingMode, ipPool, ispq string
		var viaSES bool
		var scheduledAt time.Time
		var rec int64
		if err := rows.Scan(&name, &scheduledAt, &rec, &vendorType, &routingMode, &ipPool, &viaSES, &ispq); err != nil {
			continue
		}
		route := scheduleRoute(vendorType, routingMode, ipPool, viaSES)
		token, offer := scheduleLaneOffer(name)
		if bands[route] == nil {
			bands[route] = map[string]*scheduleLane{}
		}
		l := bands[route][token]
		if l == nil {
			l = &scheduleLane{Name: route + ":" + token, Label: scheduleLaneNames.Replace(token), Offers: map[string]int{}}
			bands[route][token] = l
		}
		sched := scheduledAt.UTC().Format(time.RFC3339)
		if l.Start == "" || sched < l.Start {
			l.Start = sched
		}
		win := windowHoursFromISPQuotas([]byte(ispq))
		if win == 0 {
			win = 1 // display fallback
		}
		if end := scheduledAt.Add(time.Duration(win) * time.Hour); end.After(ends[l]) {
			ends[l] = end
		}
		l.N++
		l.Rec += rec
		l.Offers[offer]++
		totalRec += rec
		totalN++
		offerSet[offer] = true
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}

	// non-nil so JSON emits [] (a nil slice marshals to null, which the
	// frontend cannot spread)
	out := make([]scheduleBand, 0, len(scheduleTransports))
	lanes := 0
	for _, t := range scheduleTransports {
		band := scheduleBand{Key: t.key, Label: t.label, Lanes: []*scheduleLane{}}
		for _, l := range bands[t.key] {
			start, _ := time.Parse(time.RFC3339, l.Start)
			l.Window = int(math.Ceil(ends[l].Sub(start).Hours()))
			band.N += l.N
			band.Rec += l.Rec
			band.Lanes = append(band.Lanes, l)
		}
		sort.Slice(band.Lanes, func(i, j int) bool {
			if band.Lanes[i].Start != band.Lanes[j].Start {
				return band.Lanes[i].Start < band.Lanes[j].Start
			}
			return band.Lanes[i].Name < band.Lanes[j].Name
		})
		lanes += len(band.Lanes)
		out = append(out, band)
	}

	offers := make([]string, 0, len(offerSet))
	for o := range offerSet {
		offers = append(offers, o)
	}
	sort.Strings(offers)

	// The day-clock is the Denver day, midnight to midnight, DST included.
	spanStart := day.UnixMilli()
	spanEnd := dayEnd.UnixMilli()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"version":          ScheduleDayVersion,
		"date":             dateStr,
		"day_label":        day.Format("January 2"),
		"span_start_ms":    spanStart,
		"span_end_ms":      spanEnd,
		"total_campaigns":  totalN,
		"total_recipients": totalRec,
		"lanes":            lanes,
		"offers":           offers,
		"bands":            out,
	})
}
