package analytics

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The Reporting cube: ONE Athena query that returns every metric the Overview
// and Dimensions tabs slice, so the browser can re-cut it by sending route,
// provider, sending domain and day with no further trip to the lake.
//
// Rules carried over from the tabs it replaces (METRIC_CONTRACT §1/§4):
//   - Delivery comes from source IN (pmta, ses, kumo). Opens and clicks come
//     from source='app' ONLY; the SES webhook's own open/click rows are mirrored
//     into app, and app's delivered/bounce rows are mirrors of the delivery
//     sources, so taking each event from one side counts it once.
//   - Days are America/Denver, so the dt partition bound widens by a day either
//     side and the exact local day is a separate predicate.
//   - COUNT(DISTINCT event_uid), the same dedup as Breakdown.
//
// app rows carry no sending box, and delivery rows carry only the apex in
// `brand`. Both gaps close through campaign_id: camp_lane takes a campaign's
// lane from its delivered rows, camp_dom takes its sending domain from app rows.
// Both maps read back cubeLaneLookbackDays before `from`, because a campaign
// sent earlier can still collect opens inside the window. A campaign with no
// delivered row in that span keeps a blank lane.

const cubeLaneLookbackDays = 3

// maxCubeRows bounds one response. The cube has no display limit; this only
// stops a pathological range from filling the task's memory, and is reported
// to the page as truncated.
const maxCubeRows = 400000

// CubeDims are the optional extra dimensions, each fetched at most once per
// range when a user first picks it. They are too wide to ship in the main cube.
var cubeDims = map[string]string{
	"email_domain": ispDomainExpr,
	"vmta":         "vmta",
	"pool":         "pool",
}

// CubeLanes names the VMTA codes that place a pmta/kumo row on a box. Anything
// on kumo outside Colo2 is Colo 1 (see lake_lanes.go laneRest); pmta rows on no
// known box stay 'mta' so they still count under Combined and MTA.
type CubeLanes struct {
	PmtaA []string
	PmtaB []string
	Colo2 []string
}

// CubeFilter selects one cube. Dim is "" for the main cube. Brand scopes a Dim
// cube server-side: the main cube carries the sending domain and is filtered in
// the browser, but a Dim cube drops that column to stay small.
type CubeFilter struct {
	From  string
	To    string
	Dim   string
	Brand string
	Lanes CubeLanes
}

// CubeRow is one bucket. Value is set on Dim cubes only.
type CubeRow struct {
	LocalDt string
	Lane    string
	ISP     string
	Brand   string
	Event   string
	Value   string
	Count   int64
}

// Cube is the query result, unencoded.
type Cube struct {
	Rows      []CubeRow
	Truncated bool
}

var cubeBrandRe = regexp.MustCompile(`^[a-zA-Z0-9.\-]{1,255}$`)

func cubeCodeList(codes []string) (string, error) {
	seen := map[string]bool{}
	vals := make([]string, 0, len(codes))
	for _, c := range codes {
		if !vmtaCodeRe.MatchString(c) {
			return "", fmt.Errorf("invalid vmta code %q", c)
		}
		if !seen[c] {
			seen[c] = true
			vals = append(vals, c)
		}
	}
	sort.Strings(vals)
	for i, v := range vals {
		vals[i] = sqlStr(v)
	}
	return strings.Join(vals, ", "), nil
}

// cubeLaneExpr maps a delivery row to its lane key.
func cubeLaneExpr(l CubeLanes) (string, error) {
	var b strings.Builder
	b.WriteString("CASE WHEN source = 'ses' THEN 'ses'")
	for _, box := range []struct {
		lane, source string
		codes        []string
	}{
		{"pmta_a", "pmta", l.PmtaA},
		{"pmta_b", "pmta", l.PmtaB},
		{"colo2", "kumo", l.Colo2},
	} {
		if len(box.codes) == 0 {
			continue
		}
		list, err := cubeCodeList(box.codes)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, " WHEN source = '%s' AND split_part(vmta, '-', 2) IN (%s) THEN '%s'", box.source, list, box.lane)
	}
	b.WriteString(" WHEN source = 'pmta' THEN 'mta' WHEN source = 'kumo' THEN 'colo1' ELSE '' END")
	return b.String(), nil
}

func buildCubeSQL(f CubeFilter) (string, error) {
	if err := validateDt("from", f.From); err != nil {
		return "", err
	}
	if err := validateDt("to", f.To); err != nil {
		return "", err
	}
	if f.From > f.To {
		return "", errors.New("from must be on or before to")
	}
	dimExpr := ""
	if f.Dim != "" {
		e, ok := cubeDims[f.Dim]
		if !ok {
			return "", fmt.Errorf("invalid dim %q", f.Dim)
		}
		dimExpr = e
	}
	if f.Brand != "" && !cubeBrandRe.MatchString(f.Brand) {
		return "", errors.New("invalid value for brand")
	}
	laneExpr, err := cubeLaneExpr(f.Lanes)
	if err != nil {
		return "", err
	}

	dtFrom, dtTo := shiftDt(f.From, -1), shiftDt(f.To, 1)
	window := "dt BETWEEN " + sqlStr(dtFrom) + " AND " + sqlStr(dtTo)
	mapWindow := "dt BETWEEN " + sqlStr(shiftDt(f.From, -cubeLaneLookbackDays-1)) + " AND " + sqlStr(dtTo)
	localDay := localDtExpr + " BETWEEN " + sqlStr(f.From) + " AND " + sqlStr(f.To)

	evCols := func(lane, brandCol string) string {
		cols := []string{
			localDtExpr + " AS local_dt", lane + " AS lane", ispExpr + " AS isp",
			brandCol + " AS brand", eventTypeExpr + " AS et", "event_uid", "campaign_id",
		}
		if dimExpr != "" {
			cols = append(cols, dimExpr+" AS dim_value")
		}
		return strings.Join(cols, ", ")
	}

	var b strings.Builder
	b.WriteString("WITH lane_rows AS (SELECT campaign_id, ")
	b.WriteString(laneExpr)
	b.WriteString(" AS lane, COUNT(*) AS n FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE ")
	b.WriteString(mapWindow)
	b.WriteString(" AND source IN ('kumo', 'pmta', 'ses') AND event_type = 'delivered' AND campaign_id <> '' GROUP BY 1, 2),")
	b.WriteString(" camp_lane AS (SELECT campaign_id, max_by(lane, n) AS lane FROM lane_rows GROUP BY 1),")
	b.WriteString(" camp_dom AS (SELECT campaign_id, arbitrary(brand) AS sd FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE ")
	b.WriteString(mapWindow)
	b.WriteString(" AND source = 'app' AND event_type = 'delivered' AND campaign_id <> '' AND brand <> '' GROUP BY 1),")
	b.WriteString(" ev AS (SELECT ")
	b.WriteString(evCols(laneExpr, brandExpr))
	b.WriteString(" FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE ")
	b.WriteString(window)
	b.WriteString(" AND source IN ('kumo', 'pmta', 'ses') AND event_type NOT IN ('open', 'click') AND ")
	b.WriteString(localDay)
	b.WriteString(" UNION ALL SELECT ")
	b.WriteString(evCols("''", "brand"))
	b.WriteString(" FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE ")
	b.WriteString(window)
	b.WriteString(" AND source = 'app' AND event_type IN ('open', 'click') AND ")
	b.WriteString(localDay)
	b.WriteString(")")

	lane := "COALESCE(NULLIF(e.lane, ''), c.lane, '')"
	domain := "COALESCE(d.sd, e.brand, '')"
	selects := []string{lane + " AS lane", "e.isp", "e.et"}
	if dimExpr != "" {
		selects = append(selects, "COALESCE(e.dim_value, '') AS dim_value")
	} else {
		selects = append([]string{"e.local_dt"}, append(selects, domain+" AS brand")...)
	}
	b.WriteString(" SELECT ")
	b.WriteString(strings.Join(selects, ", "))
	b.WriteString(", COUNT(DISTINCT e.event_uid) AS c FROM ev e")
	b.WriteString(" LEFT JOIN camp_lane c ON c.campaign_id = e.campaign_id")
	b.WriteString(" LEFT JOIN camp_dom d ON d.campaign_id = e.campaign_id")
	if f.Brand != "" {
		lower := sqlStr(strings.ToLower(f.Brand))
		suffix := sqlStr("%." + strings.ToLower(f.Brand))
		fmt.Fprintf(&b, " WHERE lower(%s) = %s OR lower(%s) LIKE %s", domain, lower, domain, suffix)
	}
	groups := make([]string, len(selects))
	for i := range selects {
		groups[i] = strconv.Itoa(i + 1)
	}
	b.WriteString(" GROUP BY ")
	b.WriteString(strings.Join(groups, ", "))
	b.WriteString(" ORDER BY c DESC LIMIT ")
	b.WriteString(strconv.Itoa(maxCubeRows + 1))
	return b.String(), nil
}

// parseCubeRows turns result rows into CubeRows. It reads by column name so the
// main and Dim shapes share one path.
func parseCubeRows(cols []string, rows [][]string) (Cube, error) {
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}
	cell := func(row []string, name string) string {
		if i, ok := idx[name]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	for _, need := range []string{"lane", "isp", "et", "c"} {
		if _, ok := idx[need]; !ok {
			return Cube{}, fmt.Errorf("cube result missing column %s", need)
		}
	}
	out := Cube{}
	if len(rows) > maxCubeRows {
		rows = rows[:maxCubeRows]
		out.Truncated = true
	}
	out.Rows = make([]CubeRow, 0, len(rows))
	for _, row := range rows {
		n, err := strconv.ParseInt(cell(row, "c"), 10, 64)
		if err != nil {
			return Cube{}, fmt.Errorf("cube count %q: %w", cell(row, "c"), err)
		}
		out.Rows = append(out.Rows, CubeRow{
			LocalDt: cell(row, "local_dt"), Lane: cell(row, "lane"), ISP: cell(row, "isp"),
			Brand: cell(row, "brand"), Event: cell(row, "et"), Value: cell(row, "dim_value"), Count: n,
		})
	}
	return out, nil
}

// FetchCube runs the cube query against the global reader.
func FetchCube(ctx context.Context, f CubeFilter) (Cube, error) {
	r := getReader()
	if r == nil {
		return Cube{}, errDisabled
	}
	sql, err := buildCubeSQL(f)
	if err != nil {
		return Cube{}, err
	}
	cols, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return Cube{}, err
	}
	return parseCubeRows(cols, rows)
}
