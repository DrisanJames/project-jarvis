package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// Reporting data for the Overview, Dimensions and Campaign Lookup tabs.
//
//	GET /api/mailing/analytics/lake/cube?from&to[&dim=email_domain|vmta|pool&brand=][&refresh=1]
//	GET /api/mailing/analytics/campaign-tree?from&to
//	GET /api/mailing/analytics/campaign-tree/campaigns?sending_domain&day
//
// The cube is one Athena query the browser slices locally (reader_cube.go). One
// scan is ~9 GB and ~30s over 7 days, so results are held for cubeTTL and
// concurrent requests for the same cube share one scan. The scan runs on its own
// context: a user who leaves mid-load must not throw away the work for the next.
//
// Rows are dictionary-encoded: each row is indexes into dict plus the count,
// which keeps a 40k-row response near 1 MB.

const (
	cubeTTL        = 5 * time.Minute
	cubeScanLimit  = 170 * time.Second
	cubeMaxDays    = 92
	cubeMaxEntries = 32
)

type lakeCubePayload struct {
	From      string              `json:"from"`
	To        string              `json:"to"`
	Dim       string              `json:"dim,omitempty"`
	Columns   []string            `json:"columns"`
	Dict      map[string][]string `json:"dict"`
	Rows      [][]int64           `json:"rows"`
	Truncated bool                `json:"truncated"`
	FetchedAt time.Time           `json:"fetched_at"`
}

// encodeCube dictionary-encodes rows. Columns are named so the page never
// depends on their order.
func encodeCube(f analytics.CubeFilter, c analytics.Cube) *lakeCubePayload {
	cols := []string{"local_dt", "lane", "isp", "brand", "event_type"}
	pick := func(r analytics.CubeRow) []string { return []string{r.LocalDt, r.Lane, r.ISP, r.Brand, r.Event} }
	if f.Dim != "" {
		cols = []string{"value", "lane", "isp", "event_type"}
		pick = func(r analytics.CubeRow) []string { return []string{r.Value, r.Lane, r.ISP, r.Event} }
	}
	index := make([]map[string]int64, len(cols))
	dict := make(map[string][]string, len(cols))
	for i, name := range cols {
		index[i] = map[string]int64{}
		dict[name] = []string{}
	}
	rows := make([][]int64, 0, len(c.Rows))
	for _, r := range c.Rows {
		vals := pick(r)
		enc := make([]int64, len(cols)+1)
		for i, v := range vals {
			id, ok := index[i][v]
			if !ok {
				id = int64(len(dict[cols[i]]))
				index[i][v] = id
				dict[cols[i]] = append(dict[cols[i]], v)
			}
			enc[i] = id
		}
		enc[len(cols)] = r.Count
		rows = append(rows, enc)
	}
	return &lakeCubePayload{
		From: f.From, To: f.To, Dim: f.Dim, Columns: append(cols, "count"), Dict: dict, Rows: rows,
		Truncated: c.Truncated, FetchedAt: time.Now().UTC(),
	}
}

type cubeEntry struct {
	payload *lakeCubePayload
	at      time.Time
}

// cubeStore is the TTL cache in front of the scan. order is insertion order for
// eviction, since the cache is bounded by entries, not bytes.
type cubeStore struct {
	mu      sync.Mutex
	entries map[string]cubeEntry
	order   []string
	flight  singleflight.Group
	fetch   func(context.Context, analytics.CubeFilter) (analytics.Cube, error)
	now     func() time.Time
}

func newCubeStore(fetch func(context.Context, analytics.CubeFilter) (analytics.Cube, error)) *cubeStore {
	return &cubeStore{entries: map[string]cubeEntry{}, fetch: fetch, now: time.Now}
}

var lakeCubes = newCubeStore(analytics.FetchCube)

func (cs *cubeStore) get(key string) (*lakeCubePayload, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	e, ok := cs.entries[key]
	if !ok || cs.now().Sub(e.at) >= cubeTTL {
		return nil, false
	}
	return e.payload, true
}

func (cs *cubeStore) put(key string, p *lakeCubePayload) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.entries[key]; !ok {
		cs.order = append(cs.order, key)
	}
	cs.entries[key] = cubeEntry{payload: p, at: cs.now()}
	for len(cs.order) > cubeMaxEntries {
		delete(cs.entries, cs.order[0])
		cs.order = cs.order[1:]
	}
}

// load returns the cube, scanning at most once per key however many callers
// arrive together. refresh skips the cache but still shares a running scan.
func (cs *cubeStore) load(ctx context.Context, key string, f analytics.CubeFilter, refresh bool) (*lakeCubePayload, error) {
	if !refresh {
		if p, ok := cs.get(key); ok {
			return p, nil
		}
	}
	ch := cs.flight.DoChan(key, func() (interface{}, error) {
		scanCtx, cancel := context.WithTimeout(context.Background(), cubeScanLimit)
		defer cancel()
		c, err := cs.fetch(scanCtx, f)
		if err != nil {
			return nil, err
		}
		p := encodeCube(f, c)
		cs.put(key, p)
		return p, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*lakeCubePayload), nil
	}
}

func cubeRangeDays(from, to string) (int, error) {
	a, err := time.Parse("2006-01-02", from)
	if err != nil {
		return 0, fmt.Errorf("invalid from")
	}
	b, err := time.Parse("2006-01-02", to)
	if err != nil {
		return 0, fmt.Errorf("invalid to")
	}
	if b.Before(a) {
		return 0, fmt.Errorf("from must be on or before to")
	}
	return int(b.Sub(a).Hours()/24) + 1, nil
}

// HandleLakeCube serves one cube. Bad input is a 400 {"error":...}; a dark
// reader is 200 {"disabled":true} like the other lake routes.
func (s *Server) HandleLakeCube(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true})
		return
	}
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	days, err := cubeRangeDays(from, to)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if days > cubeMaxDays {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("pick %d days or fewer", cubeMaxDays)})
		return
	}
	lanes.db = s.mailingDB
	scopes, err := lanes.load(r.Context())
	if err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	f := analytics.CubeFilter{
		From: from, To: to, Dim: q.Get("dim"), Brand: strings.ToLower(strings.TrimSpace(q.Get("brand"))),
		Lanes: analytics.CubeLanes{
			PmtaA: scopes["pmta_a"].Prefixes, PmtaB: scopes["pmta_b"].Prefixes, Colo2: scopes["colo2"].Prefixes,
		},
	}
	if f.Dim == "" {
		f.Brand = "" // the main cube is brand-sliced in the browser
	}
	key := strings.Join([]string{orgID.String(), from, to, f.Dim, f.Brand}, "|")
	p, err := lakeCubes.load(r.Context(), key, f, q.Get("refresh") == "1")
	if err != nil {
		if analytics.IsDisabledErr(err) {
			respondJSON(w, http.StatusOK, map[string]interface{}{"disabled": true})
			return
		}
		if r.Context().Err() != nil {
			return
		}
		log.Printf("[lake-cube] %s %s..%s dim=%q: %v", orgID, from, to, f.Dim, err)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, p)
}

// campaignTreeStatuses are the campaigns that have actually gone out. Scheduled,
// draft and failed-before-send campaigns have nothing to report.
var campaignTreeStatuses = []string{"sending", "paused", "sent", "completed", "completed_with_errors"}

var campaignTreeDomainRe = regexp.MustCompile(`^[a-zA-Z0-9.\-]{1,255}$`)

const campaignTreeSendingDomain = `COALESCE(NULLIF(lower(split_part(c.from_email, '@', 2)), ''), lower(sp.sending_domain), '')`

const campaignTreeDay = `(COALESCE(c.scheduled_at, c.started_at, c.created_at) AT TIME ZONE 'America/Denver')::date`

// campaignTreeWhere bounds the raw timestamp a day either side of the Denver
// range so the scheduled_at index still applies, then selects the exact days.
func campaignTreeWhere() string {
	quoted := make([]string, len(campaignTreeStatuses))
	for i, st := range campaignTreeStatuses {
		quoted[i] = "'" + st + "'"
	}
	return `WHERE c.organization_id = $1
		  AND c.status IN (` + strings.Join(quoted, ", ") + `)
		  AND COALESCE(c.sent_count, 0) > 0
		  AND c.scheduled_at >= ($2::date - 1)::timestamptz
		  AND c.scheduled_at <  ($3::date + 2)::timestamptz
		  AND ` + campaignTreeDay + ` BETWEEN $2::date AND $3::date`
}

type campaignTreeNode struct {
	SendingDomain string `json:"sending_domain"`
	Day           string `json:"day"`
	Campaigns     int64  `json:"campaigns"`
	Sent          int64  `json:"sent"`
}

// HandleCampaignTree returns sending domain × day counts for the Campaign
// Lookup drill-down. The page builds apex → sub domain → date from these rows
// and loads the campaigns of a date only when it is opened.
func (s *Server) HandleCampaignTree(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	days, err := cubeRangeDays(from, to)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if days > cubeMaxDays {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("pick %d days or fewer", cubeMaxDays)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rows, err := s.mailingDB.QueryContext(ctx, `
		SELECT `+campaignTreeSendingDomain+` AS sending_domain,
		       (`+campaignTreeDay+`)::text AS day,
		       COUNT(*), COALESCE(SUM(c.sent_count), 0)
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		`+campaignTreeWhere()+`
		GROUP BY 1, 2`, orgID, from, to)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []campaignTreeNode{}
	for rows.Next() {
		var n campaignTreeNode
		if err := rows.Scan(&n.SendingDomain, &n.Day, &n.Campaigns, &n.Sent); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SendingDomain != out[j].SendingDomain {
			return out[i].SendingDomain < out[j].SendingDomain
		}
		return out[i].Day > out[j].Day
	})
	respondJSON(w, http.StatusOK, map[string]interface{}{"from": from, "to": to, "rows": out})
}

const campaignTreeListLimit = 500

type campaignTreeLeaf struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	ScheduledAt string `json:"scheduled_at"`
	Sent        int64  `json:"sent"`
}

// HandleCampaignTreeCampaigns lists one sending domain's campaigns for one day,
// largest first.
func (s *Server) HandleCampaignTreeCampaigns(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	q := r.URL.Query()
	domain := strings.ToLower(strings.TrimSpace(q.Get("sending_domain")))
	day := q.Get("day")
	if domain != "" && !campaignTreeDomainRe.MatchString(domain) {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid value for sending_domain"})
		return
	}
	if _, err := cubeRangeDays(day, day); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid day"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rows, err := s.mailingDB.QueryContext(ctx, `
		SELECT c.id::text, COALESCE(c.name, ''), COALESCE(c.status, ''),
		       to_char(COALESCE(c.scheduled_at, c.started_at, c.created_at) AT TIME ZONE 'America/Denver', 'HH24:MI'),
		       COALESCE(c.sent_count, 0)
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		`+campaignTreeWhere()+`
		  AND `+campaignTreeSendingDomain+` = $4
		ORDER BY c.sent_count DESC NULLS LAST
		LIMIT `+strconv.Itoa(campaignTreeListLimit+1), orgID, day, day, domain)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []campaignTreeLeaf{}
	for rows.Next() {
		var c campaignTreeLeaf
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &c.ScheduledAt, &c.Sent); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	truncated := len(out) > campaignTreeListLimit
	if truncated {
		out = out[:campaignTreeListLimit]
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"campaigns": out, "truncated": truncated})
}
