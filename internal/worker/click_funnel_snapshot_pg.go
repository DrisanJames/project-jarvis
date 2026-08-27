package worker

// Click-funnel snapshot — the POSTGRES gather half.
//
// Postgres is the SYSTEM OF RECORD for journey state (enrollments, execution
// log, exits, conversions) and lane configuration. The lake has no journey
// concept at all — its 24 columns carry no enrollment, node, or exit — so these
// facts cannot come from Athena today. What this file guarantees instead is
// that NONE of it is read on the request path: every query here runs inside the
// snapshot worker, off the hot path, with a generous budget, and the result is
// published to S3.
//
// See docs/METRIC_CONTRACT.md §10.13. Making Athena the sole source for flow
// too requires emitting journey events into the lake; that is the target state,
// not this file.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// ── exit classification (METRIC_CONTRACT §10.3) ─────────────────────────────
//
// exit_reason mixes behavioral exits (the lane did something) with
// ADMINISTRATIVE ones (an operator bulk-purged the lane). Blending them made
// offer 420 read as 96.4% attrition when 99.3% of those exits were two June
// operator purges. Administrative exits are excluded from every rate
// denominator.
//
// The markers are an explicit list, not a guess: every one was read off
// production exit_reason values on 2026-08-25. An unmatched reason falls to
// 'behavioral' — the conservative direction, because a mis-filed behavioral
// exit understates health rather than inventing it.
const clickFunnelExitClassSQL = `CASE
    WHEN e.status <> 'exited' THEN 'n/a'
    WHEN e.exit_reason IS NULL OR e.exit_reason = '' THEN 'behavioral'
    WHEN e.exit_reason ~ '^converted' THEN 'converted'
    WHEN e.exit_reason ~ '(operator|retired|lane_separation|cleanup|qa_probe)' THEN 'administrative'
    ELSE 'behavioral'
END`

// clickFunnelMaturityGraceHours is added to a lane's ladder before an
// enrollment counts as mature. Covers executor lag and deferral retry — a
// touch that deferred for six hours has not failed to complete, it is late.
const clickFunnelMaturityGraceHours = 24.0

// clickFunnelDefaultLookbackHours bounds per-touch conversion attribution when
// a lane's ladder cannot be measured from its graph.
const clickFunnelDefaultLookbackHours = 72.0

// ── lane configuration ──────────────────────────────────────────────────────

type cfLaneRow struct {
	OfferID, OfferName, JourneyID, JourneyName              string
	Enabled                                                 bool
	PayoutType, RoutingState, RedirectOffer, Recommendation string
	SlugInlets                                              int
}

// gatherLanes reads lane config. Deliberately NO per-lane correlated
// subqueries: the old list handler carried six of them plus a
// mailing_message_log join and measured 4.0s for 22 lanes, 1.15s of it in the
// touches_30d subquery alone. Counts come from the dedicated grouped queries
// below, each of which is a single pass.
func (w *ClickFunnelSnapshotWorker) gatherLanes(ctx context.Context) ([]cfLaneRow, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT m.everflow_offer_id,
		       COALESCE((SELECT o.name FROM mailing_offers o
		                  WHERE o.everflow_offer_id = m.everflow_offer_id
		                  ORDER BY o.updated_at DESC NULLS LAST LIMIT 1), ''),
		       COALESCE(m.click_journey_id, ''),
		       COALESCE(j.name, ''),
		       m.enabled,
		       COALESCE(m.payout_type, ''),
		       COALESCE(m.routing_state, 'active'),
		       COALESCE(m.redirect_offer_id, ''),
		       COALESCE(m.routing_recommendation, ''),
		       COALESCE(si.n, 0)
		FROM mailing_offer_journey_map m
		LEFT JOIN mailing_journeys j ON j.id = m.click_journey_id
		LEFT JOIN (
		    SELECT everflow_offer_id, COUNT(*) n
		    FROM mailing_offer_slug_map WHERE enabled GROUP BY 1
		) si ON si.everflow_offer_id = m.everflow_offer_id
		ORDER BY m.everflow_offer_id
	`)
	if err != nil {
		return nil, fmt.Errorf("lanes: %w", err)
	}
	defer rows.Close()
	out := []cfLaneRow{}
	for rows.Next() {
		var l cfLaneRow
		if err := rows.Scan(&l.OfferID, &l.OfferName, &l.JourneyID, &l.JourneyName, &l.Enabled,
			&l.PayoutType, &l.RoutingState, &l.RedirectOffer, &l.Recommendation, &l.SlugInlets); err != nil {
			return nil, fmt.Errorf("lanes scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (w *ClickFunnelSnapshotWorker) gatherOrphanInlets(ctx context.Context) []string {
	out := []string{}
	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT s.everflow_offer_id
		FROM mailing_offer_slug_map s
		LEFT JOIN mailing_offer_journey_map m ON m.everflow_offer_id = s.everflow_offer_id
		WHERE s.enabled AND m.everflow_offer_id IS NULL
		ORDER BY 1
	`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var o string
		if rows.Scan(&o) == nil {
			out = append(out, o)
		}
	}
	return out
}

// ── journey graphs ──────────────────────────────────────────────────────────

// cfGraphNode mirrors the persisted node JSON. Both delay spellings are
// honoured — the seed migration writes delayUnit/delayValue, JourneyBuilder
// writes delay_hours/delay_minutes, and reading only one renders every wait as
// zero.
type cfGraphNode struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Config struct {
		DelayUnit     string  `json:"delayUnit"`
		DelayValue    float64 `json:"delayValue"`
		DelayHours    float64 `json:"delay_hours"`
		DelayMinutes  float64 `json:"delay_minutes"`
		ReminderIndex *int    `json:"reminder_sequence_index"`
	} `json:"config"`
}

func (g cfGraphNode) delayMillis() int64 {
	if g.Config.DelayValue > 0 {
		switch strings.ToLower(g.Config.DelayUnit) {
		case "minutes", "minute", "min":
			return int64(g.Config.DelayValue * 60000)
		case "days", "day":
			return int64(g.Config.DelayValue * 86400000)
		case "seconds", "second", "sec":
			return int64(g.Config.DelayValue * 1000)
		default:
			return int64(g.Config.DelayValue * 3600000)
		}
	}
	return int64(g.Config.DelayHours*3600000 + g.Config.DelayMinutes*60000)
}

// gatherGraphs loads every journey graph once, keyed by journey id. All 22
// lanes currently share one journey, so this is a handful of rows.
func (w *ClickFunnelSnapshotWorker) gatherGraphs(ctx context.Context) (map[string][]cfGraphNode, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT id, COALESCE(nodes::text,'[]') FROM mailing_journeys`)
	if err != nil {
		return nil, fmt.Errorf("graphs: %w", err)
	}
	defer rows.Close()
	out := map[string][]cfGraphNode{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("graphs scan: %w", err)
		}
		var g []cfGraphNode
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			// A malformed graph must not kill the whole snapshot; that lane
			// renders with no nodes and an explicit note.
			continue
		}
		out[id] = g
	}
	return out, rows.Err()
}

// ladderHours sums the delay nodes — the lane's full sequence duration, which
// is the maturity threshold for completion (METRIC_CONTRACT §10.2).
func ladderHours(graph []cfGraphNode) float64 {
	var ms int64
	for _, g := range graph {
		if strings.EqualFold(g.Type, "delay") {
			ms += g.delayMillis()
		}
	}
	return float64(ms) / 3600000.0
}

// ── flow ────────────────────────────────────────────────────────────────────

type cfFlowKey struct{ Offer, Node string }
type cfFlow struct{ Reached, ErrorEnrollments, ErrorAttempts int }

// gatherFlow is ONE pass over the execution log for every lane.
//
// Errors are counted BOTH ways on purpose (METRIC_CONTRACT §10.9): the log
// writes one row per ATTEMPT, so offer 420's touch 4 showed 26,908 attempts
// from FOUR enrollments — three of them retrying every two minutes for 13 days.
// Reporting only the row count reads as 26,908 broken mailboxes.
//
// Measured on prod 2026-08-25: 5.6s for the whole estate (162 rows).
func (w *ClickFunnelSnapshotWorker) gatherFlow(ctx context.Context) (map[cfFlowKey]cfFlow, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT e.enrollment_offer_id, l.node_id,
		       COUNT(DISTINCT l.enrollment_id) FILTER (WHERE l.action <> 'error'),
		       COUNT(DISTINCT l.enrollment_id) FILTER (WHERE l.action  = 'error'),
		       COUNT(*)                        FILTER (WHERE l.action  = 'error')
		FROM mailing_journey_execution_log l
		JOIN mailing_journey_enrollments e ON e.id = l.enrollment_id
		WHERE e.enrollment_offer_id IS NOT NULL
		GROUP BY 1, 2
	`)
	if err != nil {
		return nil, fmt.Errorf("flow: %w", err)
	}
	defer rows.Close()
	out := map[cfFlowKey]cfFlow{}
	for rows.Next() {
		var k cfFlowKey
		var f cfFlow
		if err := rows.Scan(&k.Offer, &k.Node, &f.Reached, &f.ErrorEnrollments, &f.ErrorAttempts); err != nil {
			return nil, fmt.Errorf("flow scan: %w", err)
		}
		out[k] = f
	}
	return out, rows.Err()
}

// gatherAwaiting is the STATE family — who is sitting at which node right now.
func (w *ClickFunnelSnapshotWorker) gatherAwaiting(ctx context.Context) (map[cfFlowKey]int, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT enrollment_offer_id, COALESCE(current_node_id, ''), COUNT(*)
		FROM mailing_journey_enrollments
		WHERE status = 'active' AND enrollment_offer_id IS NOT NULL
		GROUP BY 1, 2
	`)
	if err != nil {
		return nil, fmt.Errorf("awaiting: %w", err)
	}
	defer rows.Close()
	out := map[cfFlowKey]int{}
	for rows.Next() {
		var k cfFlowKey
		var n int
		if err := rows.Scan(&k.Offer, &k.Node, &n); err != nil {
			return nil, fmt.Errorf("awaiting scan: %w", err)
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ── cohort ──────────────────────────────────────────────────────────────────

type cfCohort struct {
	Total                 int
	Active                int
	InFlight              int // immature — reported separately, never in a rate
	MatureEnrolled        int
	MatureCompleted       int
	ExitsBehavioral       int
	ExitsAdmin            int
	ExitsConverted        int
	GoalReached           int
	MedianEnrollToConv    sql.NullFloat64
	MedianFirstSendToConv sql.NullFloat64
}

// gatherCohort computes the COHORT family per lane, with per-lane maturity.
//
// The maturity threshold differs per lane (it is that lane's ladder + grace),
// so it arrives as a parameterized VALUES join rather than a constant. Lanes
// absent from the list get the default.
func (w *ClickFunnelSnapshotWorker) gatherCohort(ctx context.Context, maturity map[string]float64) (map[string]*cfCohort, error) {
	if len(maturity) == 0 {
		return map[string]*cfCohort{}, nil
	}
	// Parameterized VALUES — offer ids are DB-sourced but still never
	// interpolated into SQL text.
	vals := make([]string, 0, len(maturity))
	args := make([]interface{}, 0, len(maturity)*2)
	i := 1
	for offer, hrs := range maturity {
		vals = append(vals, fmt.Sprintf("($%d::text, $%d::float8)", i, i+1))
		args = append(args, offer, hrs)
		i += 2
	}
	q := `
		WITH mat(ofr, hrs) AS (VALUES ` + strings.Join(vals, ", ") + `),
		cls AS (
		    SELECT e.enrollment_offer_id AS ofr,
		           e.status, e.enrolled_at, e.converted_at,
		           ` + clickFunnelExitClassSQL + ` AS exit_class,
		           (e.enrolled_at <= NOW() - make_interval(secs => (m.hrs * 3600)::int)) AS mature
		    FROM mailing_journey_enrollments e
		    JOIN mat m ON m.ofr = e.enrollment_offer_id
		)
		SELECT ofr,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'active'),
		       COUNT(*) FILTER (WHERE NOT mature AND exit_class <> 'administrative'),
		       COUNT(*) FILTER (WHERE mature AND exit_class <> 'administrative'),
		       COUNT(*) FILTER (WHERE mature AND exit_class <> 'administrative' AND status = 'converted'),
		       COUNT(*) FILTER (WHERE exit_class = 'behavioral'),
		       COUNT(*) FILTER (WHERE exit_class = 'administrative'),
		       COUNT(*) FILTER (WHERE exit_class = 'converted'),
		       percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY EXTRACT(EPOCH FROM (converted_at - enrolled_at)) / 3600.0
		       ) FILTER (WHERE converted_at IS NOT NULL)
		FROM cls
		GROUP BY ofr`
	rows, err := w.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cohort: %w", err)
	}
	defer rows.Close()
	out := map[string]*cfCohort{}
	for rows.Next() {
		var offer string
		c := &cfCohort{}
		if err := rows.Scan(&offer, &c.Total, &c.Active, &c.InFlight, &c.MatureEnrolled,
			&c.MatureCompleted, &c.ExitsBehavioral, &c.ExitsAdmin, &c.ExitsConverted,
			&c.MedianEnrollToConv); err != nil {
			return nil, fmt.Errorf("cohort scan: %w", err)
		}
		out[offer] = c
	}
	return out, rows.Err()
}

// ── conversions ─────────────────────────────────────────────────────────────

type cfConversions struct{ PostEnrollment, PreTouch, DripAttributed int }

// gatherConversions produces the THREE figures the contract requires
// (METRIC_CONTRACT §10.5). Most click-drip conversions are caused by the
// ORIGINAL click, not a drip touch: offer 420 lifetime is 73 post-enrollment
// conversions of which 52 (71%) had no drip send at all before converting.
// Reporting one number credits the drip with the click's work.
func (w *ClickFunnelSnapshotWorker) gatherConversions(ctx context.Context) (map[string]*cfConversions, map[string]sql.NullFloat64, error) {
	rows, err := w.db.QueryContext(ctx, `
		WITH conv AS (
		    SELECT id, enrollment_offer_id AS ofr, enrolled_at, converted_at
		    FROM mailing_journey_enrollments
		    WHERE converted_at IS NOT NULL AND enrollment_offer_id IS NOT NULL
		),
		f AS (
		    SELECT conv.*, (
		        SELECT MIN(l.executed_at) FROM mailing_journey_execution_log l
		        WHERE l.enrollment_id = conv.id AND l.node_type = 'email' AND l.action <> 'error'
		    ) AS first_send
		    FROM conv
		)
		SELECT ofr,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE first_send IS NULL OR converted_at <  first_send),
		       COUNT(*) FILTER (WHERE first_send IS NOT NULL AND converted_at >= first_send),
		       percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY EXTRACT(EPOCH FROM (converted_at - first_send)) / 3600.0
		       ) FILTER (WHERE first_send IS NOT NULL AND converted_at >= first_send)
		FROM f GROUP BY ofr
	`)
	if err != nil {
		return nil, nil, fmt.Errorf("conversions: %w", err)
	}
	defer rows.Close()
	out := map[string]*cfConversions{}
	med := map[string]sql.NullFloat64{}
	for rows.Next() {
		var offer string
		c := &cfConversions{}
		var m sql.NullFloat64
		if err := rows.Scan(&offer, &c.PostEnrollment, &c.PreTouch, &c.DripAttributed, &m); err != nil {
			return nil, nil, fmt.Errorf("conversions scan: %w", err)
		}
		out[offer], med[offer] = c, m
	}
	return out, med, rows.Err()
}

// gatherNodeConversions does LAST-TOUCH attribution WITHIN A DECLARED LOOKBACK.
//
// The previous implementation had no lookback: any email touch before
// converted_at could claim the conversion, however long before. A conversion
// outside every touch's lookback is lane-attributed, never touch-attributed.
func (w *ClickFunnelSnapshotWorker) gatherNodeConversions(ctx context.Context, lookback map[string]float64) (map[cfFlowKey]int, error) {
	if len(lookback) == 0 {
		return map[cfFlowKey]int{}, nil
	}
	vals := make([]string, 0, len(lookback))
	args := make([]interface{}, 0, len(lookback)*2)
	i := 1
	for offer, hrs := range lookback {
		vals = append(vals, fmt.Sprintf("($%d::text, $%d::float8)", i, i+1))
		args = append(args, offer, hrs)
		i += 2
	}
	q := `
		WITH lb(ofr, hrs) AS (VALUES ` + strings.Join(vals, ", ") + `),
		conv AS (
		    SELECT e.id, e.enrollment_offer_id AS ofr, e.converted_at, lb.hrs
		    FROM mailing_journey_enrollments e
		    JOIN lb ON lb.ofr = e.enrollment_offer_id
		    WHERE e.converted_at IS NOT NULL
		),
		lt AS (
		    SELECT conv.ofr, (
		        SELECT l.node_id FROM mailing_journey_execution_log l
		        WHERE l.enrollment_id = conv.id
		          AND l.node_type = 'email' AND l.action <> 'error'
		          AND l.executed_at <= conv.converted_at
		          AND l.executed_at >= conv.converted_at - make_interval(secs => (conv.hrs * 3600)::int)
		        ORDER BY l.executed_at DESC LIMIT 1
		    ) AS node_id
		    FROM conv
		)
		SELECT ofr, node_id, COUNT(*) FROM lt WHERE node_id IS NOT NULL GROUP BY 1, 2`
	rows, err := w.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("node conversions: %w", err)
	}
	defer rows.Close()
	out := map[cfFlowKey]int{}
	for rows.Next() {
		var k cfFlowKey
		var n int
		if err := rows.Scan(&k.Offer, &k.Node, &n); err != nil {
			return nil, fmt.Errorf("node conversions scan: %w", err)
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ── copy + Creative Studio ──────────────────────────────────────────────────

type cfCopy struct {
	Subject, Preheader, FromOverride      string
	ProofID, ProofName, ProofApproval     string
	ProofActive, Enabled, HasBodySnapshot bool
	UpdatedAt                             time.Time
}

type cfCopyKey struct {
	Offer string
	Seq   int
}

// gatherCopy reads per-touch copy joined to its Creative Studio proof.
//
// ProofSendable (computed by the caller) mirrors the SENDER's gate exactly:
// journey_executor.go refuses a proof that is not approved AND active and falls
// through to the body snapshot, then to the clicked campaign's creative. A
// touch whose proof is not sendable is therefore mailing something other than
// what this screen shows unless that is surfaced.
func (w *ClickFunnelSnapshotWorker) gatherCopy(ctx context.Context) (map[cfCopyKey]cfCopy, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT rs.everflow_offer_id, rs.sequence_index,
		       COALESCE(rs.subject,''), COALESCE(rs.preheader,''),
		       COALESCE(rs.from_name_override,''), COALESCE(rs.enabled,false),
		       COALESCE(rs.updated_at, NOW()),
		       COALESCE(rs.proof_id::text,''),
		       COALESCE(pf.name,''), COALESCE(pf.approval_status,''), COALESCE(pf.is_active,false),
		       (COALESCE(rs.body_html,'') <> '')
		FROM mailing_offer_reminder_subjects rs
		LEFT JOIN mailing_offer_proofs pf ON pf.id = rs.proof_id
	`)
	if err != nil {
		return nil, fmt.Errorf("copy: %w", err)
	}
	defer rows.Close()
	out := map[cfCopyKey]cfCopy{}
	for rows.Next() {
		var k cfCopyKey
		var c cfCopy
		if err := rows.Scan(&k.Offer, &k.Seq, &c.Subject, &c.Preheader, &c.FromOverride,
			&c.Enabled, &c.UpdatedAt, &c.ProofID, &c.ProofName, &c.ProofApproval,
			&c.ProofActive, &c.HasBodySnapshot); err != nil {
			return nil, fmt.Errorf("copy scan: %w", err)
		}
		out[k] = c
	}
	return out, rows.Err()
}

// ── node attribution ────────────────────────────────────────────────────────

// gatherNodeCampaigns maps each lane's per-node shadow campaign to its node —
// the ONLY join key that lets the lake attribute engagement per touch, because
// the lake has no node column.
func (w *ClickFunnelSnapshotWorker) gatherNodeCampaigns(ctx context.Context) (map[cfFlowKey]string, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT journey_offer_id, journey_node_id, id::text
		FROM mailing_campaigns
		WHERE journey_offer_id IS NOT NULL AND journey_node_id IS NOT NULL
	`)
	if err != nil {
		// The attribution columns ship with a migration that can lag this
		// binary. Absent columns mean no node is attributed — flow still
		// renders, engagement is explicitly marked unattributed.
		if strings.Contains(err.Error(), "does not exist") {
			return map[cfFlowKey]string{}, nil
		}
		return nil, fmt.Errorf("node campaigns: %w", err)
	}
	defer rows.Close()
	out := map[cfFlowKey]string{}
	for rows.Next() {
		var k cfFlowKey
		var id string
		if err := rows.Scan(&k.Offer, &k.Node, &id); err != nil {
			return nil, fmt.Errorf("node campaigns scan: %w", err)
		}
		out[k] = id
	}
	return out, rows.Err()
}

// ── orphan node-attribution repair ──────────────────────────────────────────

// clickFunnelShadowNamespace mirrors clickDripShadowNamespace in
// journey_clickdrip_sender.go. Kept as a literal here because the repair proves
// its mapping in SQL, and pinned by a test so the two can never drift.
const clickFunnelShadowNamespace = "a7f3c2d1-9b8e-4c6a-8d5f-1e2b3c4d5e6f"

// repairOrphanNodeStamps re-attributes click-drip shadow campaigns that were
// inserted through the LEGACY (unstamped) path.
//
// WHAT BROKE: nine campaigns were created between 01:38 and 02:31 on
// 2026-08-02, inside the window where the node-attribution DDL had not yet
// landed. The id is deterministic and the insert was ON CONFLICT (id) DO
// NOTHING, so every later send collided and no-opped — those nodes could never
// self-repair, and their touches were invisible to the funnel screen forever.
// Offer 420's touch 2 had 3,051 sends and rendered "not node-attributed".
//
// WHY IT IS HERE AND NOT A MIGRATION: it is a backfill, and runStartupMigrations
// runs each statement under a 5s budget that includes lock wait. Shipped there
// first, it logged "TIMEOUT (skipped — will retry next boot)" on the first prod
// boot — a silent absence. This runs inside the snapshot worker's lease, off the
// request path, with a 120s budget, and retries every tick until it is a no-op.
//
// THE MAPPING IS PROVED, NOT PARSED. Campaign names are presentation strings.
// A row is only repaired when its OWN id equals the UUIDv5 recomputed from the
// (offer, node) parsed out of its name — a name that does not reproduce the id
// is left alone. The write is guarded to NULL columns so a correct existing
// mapping can never be overwritten.
func (w *ClickFunnelSnapshotWorker) repairOrphanNodeStamps(ctx context.Context) {
	res, err := w.db.ExecContext(ctx, `
		UPDATE mailing_campaigns c
		   SET journey_key        = COALESCE(c.journey_key, 'click-drip-4touch-72h'),
		       journey_node_id    = m.node_id,
		       journey_offer_id   = m.offer_id,
		       journey_wave_index = COALESCE(c.journey_wave_index,
		           NULLIF(regexp_replace(m.node_id, '^email-', ''), '')::int)
		  FROM (
		      SELECT id,
		             substring(name from 'offer ([0-9]+)')    AS offer_id,
		             substring(name from '· (email-[0-9]+)$') AS node_id
		        FROM mailing_campaigns
		       WHERE campaign_type = 'click_drip'
		         AND (journey_node_id IS NULL OR journey_offer_id IS NULL)
		         AND name ~ '· email-[0-9]+$'
		  ) m
		 WHERE c.id = m.id
		   AND m.offer_id IS NOT NULL AND m.node_id IS NOT NULL
		   AND (c.journey_node_id IS NULL OR c.journey_offer_id IS NULL)
		   AND c.id = uuid_generate_v5($1::uuid,
		         'click-drip-shadow-offer-' || m.offer_id || '-node-' || m.node_id)
	`, clickFunnelShadowNamespace)
	if err != nil {
		// Never fatal to the snapshot: an unrepaired node renders with an
		// explicit "not measurable" alert, which is the honest state.
		log.Printf("[ClickFunnelSnapshot] orphan stamp repair: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[ClickFunnelSnapshot] repaired node attribution on %d orphaned shadow campaign(s)", n)
	}
}
