package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cold-ingestion trend for the Property Ledger.
//
// Operator ask 2026-08-20: "a report that captures day over day our ingestion
// totals by sending domain … so if a feed is already injecting 10k day over day
// of cold data then the 50k injection number becomes 40k."
//
// A lane's growth ask is only ever the SHORTFALL against what its feed already
// delivers. Without this the operator sizes every clean-ask gross, and the
// estate over-orders cleaning for lanes whose feeds are already filling them
// (measured 2026-08-20: 177,013 of a 619,798 three-day ask was already arriving
// on its own) while zero-feed lanes get no signal that they have no supply.
//
// Attribution follows the FEED↔DOMAIN 1:1 doctrine (docs/FEED_DOMAIN_MAP.md):
// partner_clean_queue rows carry a dataset, the dataset carries a vertical, and
// partner_drip_vertical_roster maps vertical → sending-domain brand. A vertical
// with several homes (consumer → tot/bwp/mrd) splits by roster weight, so the
// per-brand numbers sum back to the true intake with no double count.
//
// Brand codes here are ORCHESTRATOR codes (bwp/hws/lpl/mrd/rru/tot/wfy/yih) —
// the same vocabulary the rest of the ledger speaks. See the CODE-SYSTEM LAW
// note in FEED_DOMAIN_MAP.md: brandident short codes silently fail-open.
//
// None of the three source tables carries organization_id (verified 2026-08-20),
// so there is no org predicate to apply — same as the sibling coverage handler.

const ledgerIngestionMaxDays = 60

// ledgerIngestionTimeout bounds the whole handler. Until idx_pcq_ingested_dataset
// finishes building (concurrentIndexSpecs, calm-IO window after a deploy) the
// day/dataset grouping is a full scan of the ~10 GB partner_clean_queue heap —
// measured at 51s on 2026-08-20. The server's WriteTimeout is 5 minutes, so
// without an explicit deadline a few open ledger tabs would each pin a
// long-running heap scan; that read amplification is the documented shape of
// the 2026-06-09 IO-saturation and the RDS burst-exhaustion incidents. Fail
// the panel fast and visibly instead of degrading the database underneath the
// send path.
const ledgerIngestionTimeout = 25 * time.Second

// ledgerIngestionBrandDomain maps orchestrator brand code → sending domain, so
// the screen can label rows the way VDM and the operator do.
var ledgerIngestionBrandDomain = map[string]string{
	"db": "em.discountblog.com", "wfy": "em.warrantyforyou.com",
	"rb": "em.ratesbazar.com", "fc": "em.financialcalculate.com",
	"yih": "em.yourinsurancehub.com", "cp": "em.consumerpro.net",
	"ht": "em.historythinking.com", "qf": "em.quizfiesta.com",
	"tot": "em.thingoftheday.org", "bwp": "em.businessweeklypro.com",
	"mrd": "em.myrepairdiy.com", "ci": "em.casainsure.com",
	"lpl": "em.learnpersonalloans.com", "hws": "em.homewarrantyservices.org",
	"rru": "em.refinanceratesusa.com", "mh": "em.myownhealth.net",
	"wcl": "em.wcl-heloc.com",
}

// HandleLedgerIngestion GET …/property-ledger/ingestion?days=N
//
// Returns one row per sending domain with its per-day cold intake over the
// window, plus the recent-average daily rate the operator nets a clean-ask
// against. Verticals with no active roster home are reported separately rather
// than dropped — an unmapped feed is invisible to every per-domain surface, and
// silence there must not read as zero.
func (s *PMTACampaignService) HandleLedgerIngestion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ledgerIngestionTimeout)
	defer cancel()

	days := 14
	if q := strings.TrimSpace(r.URL.Query().Get("days")); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > ledgerIngestionMaxDays {
			respondError(w, http.StatusBadRequest, "days must be 1.."+strconv.Itoa(ledgerIngestionMaxDays))
			return
		}
		days = n
	}
	// Denver operating day, matching the rest of the ledger (I-1).
	end := time.Now().In(propertyLedgerLoc)
	start := end.AddDate(0, 0, -(days - 1))
	from := start.Format("2006-01-02")
	to := end.Format("2006-01-02")

	// Bucket the queue once by (Denver ingest-day, dataset), then resolve the
	// dataset's vertical and split across that vertical's roster homes by
	// weight. Grouping on dataset_id rather than the queue's own `vertical`
	// column is deliberate: `vertical` is unindexed on this 11.2M-row table and
	// filtering or grouping by it seq-scans (documented query-shape footgun),
	// while (ingested_at, dataset_id) is a covering range scan.
	rows, err := s.db.QueryContext(ctx, `
		WITH daily AS (
			SELECT (q.ingested_at AT TIME ZONE 'America/Denver')::date AS day,
			       q.dataset_id,
			       count(*)::numeric AS n
			FROM partner_clean_queue q
			WHERE q.ingested_at >= ($1::date AT TIME ZONE 'America/Denver')
			  AND q.ingested_at <  (($2::date + 1) AT TIME ZONE 'America/Denver')
			GROUP BY 1, 2
		), vert AS (
			SELECT daily.day, d.vertical, sum(daily.n) AS n
			FROM daily JOIN partner_datasets d ON d.id = daily.dataset_id
			GROUP BY 1, 2
		), homes AS (
			SELECT vertical, brand,
			       weight::numeric AS weight,
			       sum(weight::numeric) OVER (PARTITION BY vertical) AS total_weight
			FROM partner_drip_vertical_roster
			WHERE active
		)
		SELECT h.brand, vert.day::text,
		       round(sum(vert.n * h.weight / NULLIF(h.total_weight, 0)))::bigint AS records
		FROM vert JOIN homes h ON h.vertical = vert.vertical
		GROUP BY 1, 2
		ORDER BY 1, 2`, from, to)
	if err != nil {
		if ctx.Err() != nil {
			respondError(w, http.StatusServiceUnavailable,
				"ingestion trend timed out — the (ingested_at, dataset_id) index is still building; retry shortly or narrow the window")
			return
		}
		respondError(w, http.StatusInternalServerError, "ingestion query failed")
		return
	}
	defer rows.Close()

	byBrand := map[string]map[string]int64{}
	for rows.Next() {
		var brand, day string
		var n int64
		if err := rows.Scan(&brand, &day, &n); err != nil {
			respondError(w, http.StatusInternalServerError, "ingestion scan failed")
			return
		}
		if byBrand[brand] == nil {
			byBrand[brand] = map[string]int64{}
		}
		byBrand[brand][day] = n
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "ingestion read failed")
		return
	}

	// Day axis, oldest → newest, so the client renders a stable grid even for
	// days on which nothing landed anywhere.
	dayList := make([]string, 0, days)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		dayList = append(dayList, d.Format("2006-01-02"))
	}
	// The rate an ask is netted against excludes TODAY: the current Denver day
	// is still filling, and a partial day would understate every feed and
	// inflate every clean-ask. Average the completed days in the window.
	rateDays := dayList
	if len(rateDays) > 1 {
		rateDays = rateDays[:len(rateDays)-1]
	}
	// Recent rate: last 7 completed days (or fewer if the window is shorter).
	if len(rateDays) > 7 {
		rateDays = rateDays[len(rateDays)-7:]
	}

	out := make([]map[string]interface{}, 0, len(byBrand))
	var estateTotal int64
	for brand, series := range byBrand {
		var total, recent int64
		for _, d := range dayList {
			total += series[d]
		}
		for _, d := range rateDays {
			recent += series[d]
		}
		estateTotal += total
		rate := float64(0)
		if len(rateDays) > 0 {
			rate = float64(recent) / float64(len(rateDays))
		}
		out = append(out, map[string]interface{}{
			"brand":          brand,
			"sending_domain": ledgerIngestionBrandDomain[brand],
			"by_day":         series,
			"window_total":   total,
			"per_day":        rate,
		})
	}

	// Verticals carrying intake with no active roster home — supply that exists
	// but belongs to no lane, and so appears in no per-domain total.
	unmapped := []map[string]interface{}{}
	uRows, err := s.db.QueryContext(ctx, `
		WITH daily AS (
			SELECT q.dataset_id, count(*)::bigint AS n
			FROM partner_clean_queue q
			WHERE q.ingested_at >= ($1::date AT TIME ZONE 'America/Denver')
			  AND q.ingested_at <  (($2::date + 1) AT TIME ZONE 'America/Denver')
			GROUP BY 1
		)
		SELECT COALESCE(d.vertical, '(no vertical)') AS vertical, sum(daily.n)::bigint
		FROM daily JOIN partner_datasets d ON d.id = daily.dataset_id
		WHERE NOT EXISTS (
			SELECT 1 FROM partner_drip_vertical_roster rst
			WHERE rst.active AND rst.vertical = d.vertical)
		GROUP BY 1 ORDER BY 2 DESC`, from, to)
	if err == nil {
		defer uRows.Close()
		for uRows.Next() {
			var v string
			var n int64
			if err := uRows.Scan(&v, &n); err == nil {
				unmapped = append(unmapped, map[string]interface{}{"vertical": v, "records": n})
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"from":         from,
		"to":           to,
		"days":         dayList,
		"rate_days":    len(rateDays),
		"rows":         out,
		"unmapped":     unmapped,
		"estate_total": estateTotal,
	})
}
