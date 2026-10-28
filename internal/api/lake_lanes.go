package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sending LANES for the lake (operator 2026-10-28: "within the reporting of our
// software we only have SES and MTA — we need to include the new lanes").
//
// A lane is a sending ENVIRONMENT: the SES account, each PMTA box, each KumoMTA
// box. The lake carries no sending host, but every pmta/kumo row carries the
// profile's pool_prefix as the second token of `vmta` ("vmta-rr-ses" → rr,
// "mta-dbjy-gn1" → dbjy, "mta-ufo-07" → ufo), and every active sending profile
// names its box in api_endpoint (the injector) — so box = the set of
// pool_prefixes whose profiles point at that injector. The resolver reads that
// set from mailing_sending_profiles and caches it; the breakdown handler turns a
// `lane` query param into source IN (...) + split_part(vmta,'-',2) IN (...).
//
//	lane     source   scope
//	ses      ses      —                      (SES account truth: delivered/open/click/bounce)
//	mta      pmta     —                      (both PMTA boxes; = the old "MTA")
//	pmta_a   pmta     prefixes on 15.204.101.125 (+ the shared 'ses-relay' VMTA — its pool
//	                  ses-relay-a is wired to Server A only, verified 2026-10-28)
//	pmta_b   pmta     prefixes on 15.204.107.107
//	kumo     kumo     —                      (both Kumo boxes)
//	colo1    kumo     everything on kumo that is NOT colo2 (the yahoo-set estate; its cold-board cells
//	                  inject VMTA codes — hlj/pmd/usf/trb — that no colo1 profile owns, so colo1 is
//	                  defined as the REST of the transport, laneRest)
//	colo2    kumo     prefixes on 15.204.105.55   (the warmup-agent box, egress 199.235.0.0/24)
//
// Gmail is NOT a lane here: the gm./gt. senders relay through SES, and SES
// delivery rows carry no sending domain, so "Gmail" in the lake is the ISP
// filter (isp_group=gmail), not a transport.

// laneHosts maps an injector host (api_endpoint host, fallback smtp_host) to
// its lane key. New boxes are added HERE; profiles pointing at an unknown host
// fall into no box lane (they still count under mta/kumo/combined).
var laneHosts = map[string]string{
	"15.204.101.125": "pmta_a",
	"15.204.107.107": "pmta_b",
	"40.160.129.116": "colo1",
	"15.204.105.55":  "colo2",
}

// laneSource is the lake `source` each lane reads from.
var laneSource = map[string]string{
	"ses": "ses", "mta": "pmta", "pmta_a": "pmta", "pmta_b": "pmta",
	"kumo": "kumo", "colo1": "kumo", "colo2": "kumo",
}

// laneLabels is the one vocabulary the portal shows (Agents tab uses the same).
var laneLabels = map[string]string{
	"ses": "SES", "mta": "MTA", "pmta_a": "PMTA A", "pmta_b": "PMTA B",
	"kumo": "Kumo", "colo1": "Colo 1", "colo2": "Colo 2",
}

// laneRest defines a box as its transport MINUS another box (NOT IN the other
// box's codes) instead of by its own profiles.
var laneRest = map[string]string{"colo1": "colo2"}

var laneOrder = []string{"ses", "mta", "pmta_a", "pmta_b", "kumo", "colo1", "colo2"}

// sesRelayVMTACode is the second token of the shared relay VMTA name
// 'ses-relay' (pool ses-relay-a). It is not a profile pool_prefix, so the
// resolver pins it to the box that owns the ses-relay-a pool.
const sesRelayVMTACode = "relay"

var poolPrefixRe = regexp.MustCompile(`^[a-z0-9]{1,12}$`)

// LaneScope is what the breakdown handler needs for one lane.
type LaneScope struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Source   string   `json:"source"`
	Prefixes []string `json:"prefixes,omitempty"` // nil = whole source
	Exclude  []string `json:"exclude,omitempty"`  // laneRest: the other box's codes (NOT IN)
	Profiles int      `json:"profiles"`
}

type laneResolver struct {
	db      *sql.DB
	ttl     time.Duration
	mu      sync.Mutex
	at      time.Time
	scopes  map[string]LaneScope
	lastErr error
}

var lanes = &laneResolver{ttl: 10 * time.Minute}

func laneHostOf(endpoint, smtpHost string) string {
	if endpoint != "" {
		if u, err := url.Parse(endpoint); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return smtpHost
}

// buildLaneScopes is the pure mapping (profiles → lanes) so it can be tested
// without a database. rows: (host, poolPrefix, ipPool).
type laneProfileRow struct{ host, poolPrefix, ipPool string }

func buildLaneScopes(rows []laneProfileRow) map[string]LaneScope {
	byLane := map[string]map[string]bool{}
	count := map[string]int{}
	for _, r := range rows {
		lane, ok := laneHosts[r.host]
		if !ok {
			continue
		}
		if byLane[lane] == nil {
			byLane[lane] = map[string]bool{}
		}
		count[lane]++
		if poolPrefixRe.MatchString(r.poolPrefix) {
			byLane[lane][r.poolPrefix] = true
		}
		if r.ipPool == "ses-relay-a" {
			byLane[lane][sesRelayVMTACode] = true
		}
	}
	out := map[string]LaneScope{}
	for _, k := range laneOrder {
		sc := LaneScope{Key: k, Label: laneLabels[k], Source: laneSource[k]}
		if set, boxed := byLane[k]; boxed {
			for p := range set {
				sc.Prefixes = append(sc.Prefixes, p)
			}
			sort.Strings(sc.Prefixes)
			sc.Profiles = count[k]
		}
		if _, isBox := laneHosts[hostForLane(k)]; isBox && sc.Prefixes == nil {
			// a box lane with no profiles must scope to NOTHING, not to the whole source
			sc.Prefixes = []string{}
		}
		out[k] = sc
	}
	for k, other := range laneRest {
		sc := out[k]
		sc.Prefixes, sc.Exclude = nil, out[other].Prefixes
		if sc.Exclude == nil {
			sc.Exclude = []string{}
		}
		out[k] = sc
	}
	return out
}

func hostForLane(lane string) string {
	for h, l := range laneHosts {
		if l == lane {
			return h
		}
	}
	return ""
}

func (lr *laneResolver) load(ctx context.Context) (map[string]LaneScope, error) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.scopes != nil && time.Since(lr.at) < lr.ttl {
		return lr.scopes, nil
	}
	if lr.db == nil {
		return nil, fmt.Errorf("lane resolver: no db")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := lr.db.QueryContext(ctx, `SELECT COALESCE(api_endpoint,''), COALESCE(smtp_host,''), COALESCE(pool_prefix,''), COALESCE(ip_pool,'')
	                                       FROM mailing_sending_profiles WHERE vendor_type='pmta'`) // every status: a retired profile's history still belongs to its box
	if err != nil {
		lr.lastErr = err
		if lr.scopes != nil { // stale-while-error, same posture as the ban registry
			return lr.scopes, nil
		}
		return nil, err
	}
	defer rows.Close()
	var prs []laneProfileRow
	for rows.Next() {
		var ep, host, px, pool string
		if err := rows.Scan(&ep, &host, &px, &pool); err != nil {
			return nil, err
		}
		prs = append(prs, laneProfileRow{host: laneHostOf(ep, host), poolPrefix: px, ipPool: pool})
	}
	lr.scopes, lr.at, lr.lastErr = buildLaneScopes(prs), time.Now(), nil
	return lr.scopes, nil
}

// laneScope resolves a `lane` query value; "" and "combined" mean no lane.
func (lr *laneResolver) laneScope(ctx context.Context, lane string) (*LaneScope, error) {
	lane = strings.ToLower(strings.TrimSpace(lane))
	if lane == "" || lane == "combined" {
		return nil, nil
	}
	if _, ok := laneSource[lane]; !ok {
		return nil, fmt.Errorf("invalid lane %q (one of %s)", lane, strings.Join(laneOrder, ","))
	}
	scopes, err := lr.load(ctx)
	if err != nil {
		return nil, err
	}
	sc := scopes[lane]
	return &sc, nil
}

// HandleLakeLanes lists the lanes with their resolved scope — what the portal's
// transport selector offers and how each one is scoped in the lake.
func (s *Server) HandleLakeLanes(w http.ResponseWriter, r *http.Request) {
	_, _ = GetOrgIDFromRequest(r)
	lanes.db = s.mailingDB
	scopes, err := lanes.load(r.Context())
	if err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	out := make([]LaneScope, 0, len(laneOrder))
	for _, k := range laneOrder {
		out = append(out, scopes[k])
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"lanes": out, "hosts": laneHosts, "resolved_at": lanes.at})
}
