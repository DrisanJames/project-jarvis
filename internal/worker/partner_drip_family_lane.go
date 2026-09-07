package worker

// ─────────────────────────────────────────────────────────────────────────────
// yahoo_family — the ONLY drip lane that mails the yahoo family
// ─────────────────────────────────────────────────────────────────────────────
//
// Operator 2026-09-07. The family = yahoo.com (+ymail/rocketmail), AOL, AT&T,
// Cox, Comcast, SBCGlobal. This lane exists to GROW volume on those ISPs and
// to HARVEST engagers for the broadcast: every record gets a five-touch
// newsletter sequence (one touch per day, one slot creative per touch), an
// open OR a click exits the record to the engaged tier (broadcast picks it
// up), and a record that reaches touch 5 without engaging is obsolete.
//
// What this file adds to the orchestrator (each is small and lane-scoped):
//
//   1. FamilyLaneVertical — the vertical string; the partner_datasets CHECK
//      constraint admits it (cmd/server/main.go sep07 migration).
//   2. DB-backed creatives. Every other lane reads its creative from disk
//      (cfg.CreativesDir). This lane mails FRESH newsletters registered in
//      Creative Studio, so partner_drip_creatives / partner_drip_followup_creatives
//      carry a nullable creative_id → mailing_creatives. resolveCreative and
//      resolveFollowupCreative take that branch when the column is set.
//      Fail-closed: the row must be approval_status='approved' and carry a
//      newsletter producer stamp, or the touch does not ship.
//   3. Zero offers, ever. newsletterGuard refuses any HTML that carries a money
//      host, an /o/ offer link, a smart-link hash, an Everflow hop or an
//      affiliate marker. Applied to EVERY creative this lane resolves, on every
//      touch, before deploy. A refused creative is a failed touch, not a warn.
//   4. Exit on open OR click. The engagement marker stamps engaged_at from
//      clicks estate-wide; for this lane it also stamps from opens and closes
//      the ladder (terminal_reason='engaged_exit', next_touch_at NULL).
//   5. Home-brand pin. Injected records carry extra_metadata.home_brand so all
//      five touches come from the same sending domain and the per-domain ladder
//      accounting is exact (same mechanism the converters_ lanes use).
//
// Volume is governed OUTSIDE this file by the REQ-118 contracts: the domain
// contract's daily_max_by_isp (all touches) and the lane's desired_daily_intros
// (touch 1), filed nightly by agents/jobs/yahoo_family_lane.py from the ladder's
// exits. The mediator enforces this lane only (DRIP_SUPPLY_CANARY=*:*:yahoo_family).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lib/pq"
)

// FamilyLaneVertical is the lane key (partner_clean_queue.vertical).
const FamilyLaneVertical = "yahoo_family"

// FamilyLaneISPs is the family this lane owns, in canonical ISP-class names.
var FamilyLaneISPs = []string{"yahoo", "aol", "att", "cox", "comcast", "sbcglobal"}

// FamilyLaneProducerStamps are the mailing_creatives.approved_by values a
// creative_id may resolve to. Mirrors api.NewsletterProducerStamps plus this
// lane's own slot builder; kept as a list here because the worker package
// cannot import internal/api.
var FamilyLaneProducerStamps = []string{
	"yahoo_family_slot_stage",
	"legacy_newsletter_stage",
	"kumo_newsletter_stage",
}

// EngagedExitReason is the terminal_reason stamped when an open or a click
// exits a record from this lane's ladder.
const EngagedExitReason = "engaged_exit"

// IsFamilyLane reports whether a vertical is this lane.
func IsFamilyLane(vertical string) bool {
	return strings.EqualFold(strings.TrimSpace(vertical), FamilyLaneVertical)
}

// newsletterBannedRE — anything that could hand the reader to an advertiser.
// Money hosts (mirrors dripMoneyHostHrefRE + internal/api/money_link_check.go
// hosts), the /o/ offer gateway, the smart-link hash shape, Everflow hops and
// generic affiliate markers. Case-insensitive, matched on the raw HTML so
// obfuscation via attributes does not help.
var newsletterBannedRE = regexp.MustCompile(`(?i)(` +
	`cratoolpro\.com|eos57ytf\.com|k8k0hfdt\.com|xnonu\.com|muqes\.com|jyqye\.com|codefortwo\.com|` +
	`everflow|trkclk|/aff/|[?&]aff=|` +
	`https?://[^"'\s>]+/o/[^"'\s>]+/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}` +
	`)`)

// ErrNewsletterOffer is returned when a creative for the family lane carries
// anything that resolves to an offer.
var ErrNewsletterOffer = errors.New("family lane creative carries an offer/money link — refused")

// newsletterGuard refuses HTML that is not a zero-offer newsletter.
func newsletterGuard(html string) error {
	if strings.TrimSpace(html) == "" {
		return errors.New("family lane creative is empty — refused")
	}
	if m := newsletterBannedRE.FindString(html); m != "" {
		return fmt.Errorf("%w: %q", ErrNewsletterOffer, truncateForLog(m, 80))
	}
	return nil
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// studioCreativeSQL loads an APPROVED newsletter creative by id. approved_by
// must be a newsletter producer stamp — an offer creative that happens to be
// approved in the Studio is not selectable here.
const studioCreativeSQL = `
	SELECT COALESCE(html_content,''), COALESCE(subject,''), COALESCE(preheader,''), filename, brand_code
	FROM mailing_creatives
	WHERE id = $1 AND approval_status = 'approved' AND approved_by = ANY($2::text[])
	  AND COALESCE(html_content,'') <> ''`

// loadStudioCreative fills c.htmlBody (and subject/preheader when the row
// carries them and the drip row left them blank) from mailing_creatives.
func (po *PartnerDripOrchestrator) loadStudioCreative(ctx context.Context, creativeID string, c *creativeRec) error {
	var html, subject, preheader, filename, brandCode string
	err := po.db.QueryRowContext(ctx, studioCreativeSQL, creativeID, pq.Array(FamilyLaneProducerStamps)).
		Scan(&html, &subject, &preheader, &filename, &brandCode)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("creative_id %s: no APPROVED newsletter creative (approved_by must be one of %v)", creativeID, FamilyLaneProducerStamps)
	}
	if err != nil {
		return fmt.Errorf("creative_id %s: %w", creativeID, err)
	}
	if err := newsletterGuard(html); err != nil {
		return fmt.Errorf("creative_id %s (%s): %w", creativeID, filename, err)
	}
	c.htmlBody = html
	c.filename = filename
	if strings.TrimSpace(c.subject) == "" {
		c.subject = subject
	}
	if strings.TrimSpace(c.preheader) == "" {
		c.preheader = preheader
	}
	return nil
}

// familyEngagedExitSQL — for the family lane, an OPEN is engagement too
// (operator: "did not open or click become obsolete"). Stamps engaged_at from
// opens on this lane's datasets, then closes the ladder for every engaged
// record so the follow-up claim never sees it again and the exit is auditable
// by reason. Verdict-filtered like the click marker (humanVerdictSQL) so an
// MPP/scanner fetch does not exit a record.
func familyEngagedExitSQL(verdictFiltered bool) (markOpens, closeLadder string) {
	verdict := ""
	if verdictFiltered {
		verdict = "AND " + humanVerdictSQL
	}
	markOpens = `
		UPDATE partner_clean_queue q
		SET engaged_at = e.first_open
		FROM (
			SELECT te.subscriber_id, c.partner_dataset_id, MIN(te.event_at) AS first_open
			FROM mailing_tracking_events te
			JOIN mailing_campaigns c ON c.id = te.campaign_id
			JOIN partner_datasets d ON d.id = c.partner_dataset_id
			WHERE te.event_type = 'opened'
			  AND d.vertical = $1
			  AND te.event_at > NOW() - make_interval(mins => $2)
			  ` + verdict + `
			GROUP BY te.subscriber_id, c.partner_dataset_id
		) e
		WHERE q.subscriber_id = e.subscriber_id
		  AND q.dataset_id = e.partner_dataset_id
		  AND q.vertical = $1
		  AND q.engaged_at IS NULL`
	closeLadder = `
		UPDATE partner_clean_queue
		SET next_touch_at = NULL,
		    terminal_reason = '` + EngagedExitReason + `',
		    extra_metadata = COALESCE(extra_metadata, '{}'::jsonb)
		                     || jsonb_build_object('ladder_exit', '` + EngagedExitReason + `', 'ladder_exit_at', NOW()::text)
		WHERE vertical = $1
		  AND engaged_at IS NOT NULL
		  AND terminal_reason IS NULL`
	return markOpens, closeLadder
}
