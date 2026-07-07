package api

// Send-Day Schedule (day-clock) — read-only view of a single send day's
// campaigns, grouped into board waves (the engaged tier's W1/W2/W3) and sidecar
// / peppering lanes, with each lane's send window, route (PMTA/SES/Kumo), offer
// mix and recipient total. Backs the portal "Schedule" tab (ScheduleView.tsx).
//
// This is the live-data twin of the operator's schedule_report.py generator:
// campaigns are selected by their Denver send day, the lane/offer come from the
// canonical "<day> - <Brand> - <LANE> - <offer>" name, the window is derived
// from isp_quotas.isp_plans[].time_spans[], and the route from the sending
// profile's domain (em.* = PMTA, m.* = SES, else Kumo/other).

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

const ScheduleDayVersion = "1.0.0"

// board wave families (name lane prefix) -> display label + meta.
var scheduleBoardFamilies = []struct{ key, label, meta string }{
	{"W1-CLK1", "W1 · Clicker", "first touch"},
	{"W2-ENG", "W2 · Engaged", "mid-day"},
	{"W3-ENG2", "W3 · Engaged 2", "evening"},
}

// sidecar lane token -> friendly label (falls back to the token itself).
var scheduleSidecarLabels = map[string]string{
	"SamsYahooAcclim": "Sam's Yahoo/AOL acclim",
	"CLK30-PMTA":      "Peppering · 30D clickers",
	"SPICY-T1":        "Spicy backlog T1", "SPICY-T2": "Spicy backlog T2",
	"SPICY-T3": "Spicy backlog T3", "SPICY-T4": "Spicy backlog T4",
	"Clicker-Broad":   "Clicker-Campaign broad",
	"OptOut-LibTest":  "Opt-out corpus test",
	"OptOut-SamsTest": "Opt-out corpus test (gated)",
	"MOOv2-Mortgage":  "MOOv2 mortgage",
}

var scheduleGatedTokens = map[string]bool{"OptOut-SamsTest": true}

type scheduleLane struct {
	Name   string         `json:"name"`
	Label  string         `json:"label"`
	Meta   string         `json:"meta,omitempty"`
	Start  string         `json:"start"`            // ISO8601 UTC (earliest campaign)
	Window int            `json:"window"`           // hours
	N      int            `json:"n"`                // campaign count
	Rec    int64          `json:"rec"`              // sum total_recipients
	Offers map[string]int `json:"offers"`           // offer token -> count
	Esp    map[string]int `json:"esp"`              // pmta/ses/kumo -> count
	Route  string         `json:"route"`            // PMTA | SES | PMTA+SES | KUMO
	Gated  bool           `json:"gated,omitempty"`
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

func scheduleRoute(sendingDomain string) string {
	d := strings.ToLower(sendingDomain)
	switch {
	case strings.HasPrefix(d, "em."):
		return "pmta"
	case strings.HasPrefix(d, "m."):
		return "ses"
	case d == "":
		return "other"
	default:
		return "kumo"
	}
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
		       COALESCE(p.sending_domain,''), COALESCE(c.isp_quotas::text,'')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON c.sending_profile_id = p.id
		WHERE c.organization_id = $1
		  AND c.status NOT IN ('cancelled','draft')
		  AND c.name NOT LIKE '[partner-drip]%'
		  AND (c.scheduled_at AT TIME ZONE 'America/Denver')::date = $2::date
		ORDER BY c.scheduled_at, c.name`
	rows, err := s.mailingDB.QueryContext(ctx, q, orgID, dateStr)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	defer rows.Close()

	board := map[string]*scheduleLane{}
	side := map[string]*scheduleLane{}
	winByGroup := map[string]int{} // group key -> max window seen

	upsert := func(store map[string]*scheduleLane, key, name, label, meta string) *scheduleLane {
		l := store[key]
		if l == nil {
			l = &scheduleLane{Name: name, Label: label, Meta: meta,
				Offers: map[string]int{}, Esp: map[string]int{}}
			store[key] = l
		}
		return l
	}

	var totalRec int64
	var totalN int
	offerSet := map[string]bool{}

	for rows.Next() {
		var name, sched, dom, ispq string
		var scheduledAt time.Time
		var rec int64
		if err := rows.Scan(&name, &scheduledAt, &rec, &dom, &ispq); err != nil {
			continue
		}
		sched = scheduledAt.UTC().Format(time.RFC3339)
		parts := strings.Split(name, " - ")
		lane, offer := "", ""
		if len(parts) >= 3 {
			lane = strings.TrimSpace(parts[2])
		}
		if len(parts) >= 4 {
			offer = strings.TrimSpace(parts[3])
		}
		if offer == "" {
			offer = "other"
		}
		route := scheduleRoute(dom)
		win := windowHoursFromISPQuotas([]byte(ispq))

		// board family?
		var fam string
		for _, f := range scheduleBoardFamilies {
			if strings.HasPrefix(lane, f.key) {
				fam = f.key
				break
			}
		}
		var l *scheduleLane
		var gkey string
		if fam != "" {
			var label, meta string
			for _, f := range scheduleBoardFamilies {
				if f.key == fam {
					label, meta = f.label, f.meta
				}
			}
			gkey = "b:" + fam
			l = upsert(board, fam, fam, label, meta)
			_ = meta
		} else {
			token := lane
			if token == "" || strings.Contains(strings.ToLower(offer), "warmup") {
				token = "KUMO"
			}
			label := scheduleSidecarLabels[token]
			if label == "" {
				label = token
			}
			gkey = "s:" + token
			l = upsert(side, token, token, label, "")
			if scheduleGatedTokens[token] {
				l.Gated = true
			}
		}
		l.N++
		l.Rec += rec
		l.Offers[offer]++
		l.Esp[route]++
		if l.Start == "" || sched < l.Start {
			l.Start = sched
		}
		if win > winByGroup[gkey] {
			winByGroup[gkey] = win
		}
		totalRec += rec
		totalN++
		offerSet[offer] = true
	}

	finalize := func(l *scheduleLane, gkey string) {
		l.Window = winByGroup[gkey]
		if l.Window == 0 {
			l.Window = 8 // display fallback
		}
		pmta, ses, kumo := l.Esp["pmta"], l.Esp["ses"], l.Esp["kumo"]
		switch {
		case pmta > 0 && ses > 0:
			l.Route = "PMTA+SES"
		case ses > 0:
			l.Route = "SES"
		case kumo > 0:
			l.Route = "KUMO"
		default:
			l.Route = "PMTA"
		}
	}

	var boardOut, sideOut []*scheduleLane
	for _, f := range scheduleBoardFamilies {
		if l := board[f.key]; l != nil {
			finalize(l, "b:"+f.key)
			boardOut = append(boardOut, l)
		}
	}
	for token, l := range side {
		finalize(l, "s:"+token)
		sideOut = append(sideOut, l)
	}
	sort.Slice(boardOut, func(i, j int) bool { return boardOut[i].Start < boardOut[j].Start })
	sort.Slice(sideOut, func(i, j int) bool { return sideOut[i].Start < sideOut[j].Start })

	offers := make([]string, 0, len(offerSet))
	for o := range offerSet {
		offers = append(offers, o)
	}
	sort.Strings(offers)

	lanes := len(boardOut) + len(sideOut)
	// day-clock anchor: 07:00Z of the send day, spanning +21h (matches the ops
	// day-clock); extended if any campaign falls outside.
	anchor := time.Date(day.Year(), day.Month(), day.Day(), 7, 0, 0, 0, time.UTC)
	spanStart := anchor.UnixMilli()
	spanEnd := anchor.Add(21 * time.Hour).UnixMilli()

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
		"board_waves":      boardOut,
		"sidecars":         sideOut,
	})
}
