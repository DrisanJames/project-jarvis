package api

// data_pipeline_agent_scorecard.go — makes the pipeline agent MEASURABLE: one
// bounded read of the last 7 days of data_pipeline_agent_log (served by the
// (org_id, at DESC) index), scored in Go for the last 24 hours and 7 days.
//
// Definitions (what the screen's tiles mean):
//   - checks / ok / warn / fail: entries of kind 'check' and their status;
//     pass_rate = ok ÷ checks × 100.
//   - cycle: a run of check entries with less than pipelineAgentCycleGap
//     between consecutive ones (one agent pass posts several checks).
//     avg_minutes_between_cycles = mean gap between cycle starts;
//     longest_gap_minutes = the largest gap between consecutive check
//     entries in the window, plus the open gap from the last check to now.
//   - open failure: a check title whose latest 'fail' has no later 'ok'
//     (a later 'warn' does not clear it); age from the first fail since the
//     last ok. Computed over the 7-day read.
//   - forecast accuracy: kind 'learned' with numeric body.forecast and
//     body.actual (actual ≠ 0) → mean absolute percent error; null when none.

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"
)

const (
	pipelineAgentCycleGap      = 5 * time.Minute
	pipelineAgentScorecardRows = 20000
)

const dataPipelineAgentScorecardSQL = `
	SELECT at, kind, status, title, body::text
	FROM data_pipeline_agent_log
	WHERE org_id = $1 AND at >= $2
	ORDER BY at DESC, id DESC
	LIMIT $3`

type pipelineAgentWindow struct {
	Checks                  int      `json:"checks"`
	OK                      int      `json:"ok"`
	Warn                    int      `json:"warn"`
	Fail                    int      `json:"fail"`
	PassRate                *float64 `json:"pass_rate"`
	Cycles                  int      `json:"cycles"`
	AvgMinutesBetweenCycles *float64 `json:"avg_minutes_between_cycles"`
	LongestGapMinutes       *float64 `json:"longest_gap_minutes"`
	Alerts                  int      `json:"alerts"`
	Decisions               int      `json:"decisions"`
	Repairs                 int      `json:"repairs"`
	Learned                 int      `json:"learned"`
	// mean absolute percent error over learned entries with forecast/actual
	ForecastMAPE    *float64 `json:"forecast_mape"`
	ForecastSamples int      `json:"forecast_samples"`
}

type pipelineAgentOpenFailure struct {
	Title      string  `json:"title"`
	Since      string  `json:"since"`
	AgeMinutes float64 `json:"age_minutes"`
	Agent      string  `json:"agent"`
}

type pipelineAgentCheckLatest struct {
	Title  string `json:"title"`
	Status string `json:"status"`
	At     string `json:"at"`
	Agent  string `json:"agent"`
}

type pipelineAgentScorecard struct {
	Last24h      pipelineAgentWindow        `json:"last_24h"`
	Last7d       pipelineAgentWindow        `json:"last_7d"`
	OpenFailures []pipelineAgentOpenFailure `json:"open_failures"`
	ChecksLatest []pipelineAgentCheckLatest `json:"checks_latest"`
	Truncated    bool                       `json:"truncated"`
}

type agentLogRow struct {
	at                  time.Time
	kind, status, title string
	body                map[string]interface{}
	agent               string
}

func agentNameFromBody(body json.RawMessage) string {
	var obj map[string]interface{}
	if json.Unmarshal(body, &obj) == nil {
		if a, ok := obj["agent"].(string); ok && a != "" {
			return a
		}
	}
	return pipelineAgentDefaultName
}

func (s *DataPipelineService) agentScorecard(ctx context.Context, org string) (*pipelineAgentScorecard, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, dataPipelineAgentScorecardSQL, org, now.Add(-7*24*time.Hour).UTC(), pipelineAgentScorecardRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var log []agentLogRow
	for rows.Next() {
		var x agentLogRow
		var body string
		if err := rows.Scan(&x.at, &x.kind, &x.status, &x.title, &body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &x.body); err != nil {
			x.body = nil // a non-object body scores as empty (POST only admits objects)
		}
		x.agent = pipelineAgentDefaultName
		if a, ok := x.body["agent"].(string); ok && a != "" {
			x.agent = a
		}
		log = append(log, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sc := scorePipelineAgent(log, now)
	sc.Truncated = len(log) >= pipelineAgentScorecardRows
	return sc, nil
}

// scorePipelineAgent is the pure scoring (rows ascending by at).
func scorePipelineAgent(log []agentLogRow, now time.Time) *pipelineAgentScorecard {
	// The read is newest-first (so a truncated read keeps the newest rows).
	sort.SliceStable(log, func(i, j int) bool { return log[i].at.Before(log[j].at) })
	sc := &pipelineAgentScorecard{
		Last24h:      scoreAgentWindow(log, now.Add(-24*time.Hour), now),
		Last7d:       scoreAgentWindow(log, now.Add(-7*24*time.Hour), now),
		OpenFailures: []pipelineAgentOpenFailure{},
		ChecksLatest: []pipelineAgentCheckLatest{},
	}
	type state struct {
		latest    agentLogRow
		openSince *time.Time
		openAgent string
	}
	byTitle := map[string]*state{}
	var titles []string
	for _, x := range log {
		if x.kind != "check" {
			continue
		}
		st := byTitle[x.title]
		if st == nil {
			st = &state{}
			byTitle[x.title] = st
			titles = append(titles, x.title)
		}
		st.latest = x
		switch x.status {
		case "fail":
			if st.openSince == nil {
				at := x.at
				st.openSince, st.openAgent = &at, x.agent
			}
		case "ok":
			st.openSince = nil
		}
	}
	sort.Strings(titles)
	for _, t := range titles {
		st := byTitle[t]
		sc.ChecksLatest = append(sc.ChecksLatest, pipelineAgentCheckLatest{
			Title: t, Status: st.latest.status, At: st.latest.at.UTC().Format(time.RFC3339), Agent: st.latest.agent,
		})
		if st.openSince != nil {
			sc.OpenFailures = append(sc.OpenFailures, pipelineAgentOpenFailure{
				Title: t, Since: st.openSince.UTC().Format(time.RFC3339),
				AgeMinutes: now.Sub(*st.openSince).Minutes(), Agent: st.openAgent,
			})
		}
	}
	return sc
}

func scoreAgentWindow(log []agentLogRow, from, now time.Time) pipelineAgentWindow {
	var w pipelineAgentWindow
	var checkTimes []time.Time
	var apeSum float64
	for _, x := range log {
		if x.at.Before(from) || x.at.After(now) {
			continue
		}
		switch x.kind {
		case "check":
			w.Checks++
			checkTimes = append(checkTimes, x.at)
			switch x.status {
			case "ok":
				w.OK++
			case "warn":
				w.Warn++
			case "fail":
				w.Fail++
			}
		case "alert":
			w.Alerts++
		case "decision":
			w.Decisions++
		case "repair":
			w.Repairs++
		case "learned":
			w.Learned++
			f, fok := x.body["forecast"].(float64)
			a, aok := x.body["actual"].(float64)
			if fok && aok && a != 0 {
				apeSum += math.Abs(f-a) / math.Abs(a) * 100
				w.ForecastSamples++
			}
		}
	}
	if w.Checks > 0 {
		w.PassRate = ptrF64(float64(w.OK) / float64(w.Checks) * 100)
	}
	if w.ForecastSamples > 0 {
		w.ForecastMAPE = ptrF64(apeSum / float64(w.ForecastSamples))
	}
	if len(checkTimes) > 0 {
		var starts []time.Time
		longest := now.Sub(checkTimes[len(checkTimes)-1]).Minutes() // open gap to now
		for i, t := range checkTimes {
			if i == 0 || t.Sub(checkTimes[i-1]) >= pipelineAgentCycleGap {
				starts = append(starts, t)
			}
			if i > 0 {
				longest = math.Max(longest, t.Sub(checkTimes[i-1]).Minutes())
			}
		}
		w.Cycles = len(starts)
		w.LongestGapMinutes = ptrF64(longest)
		if len(starts) > 1 {
			w.AvgMinutesBetweenCycles = ptrF64(starts[len(starts)-1].Sub(starts[0]).Minutes() / float64(len(starts)-1))
		}
	}
	return w
}
