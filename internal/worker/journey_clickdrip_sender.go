package worker

// JourneyClickDripSender is the send path for click-drip reminder emails.
//
// Why a dedicated sender (and not the JourneyExecutor's simple emailSender
// callback): click-drip reminders MUST go out through the real PMTA pipeline
// on the SAME sending profile the subscriber originally clicked from, with
// the same IP-pool/warmup routing, DKIM domain, open/click tracking, and a
// mailing_message_log row for ops visibility + frequency accounting. The
// minimal `func(ctx, email, subject, html, fromName, fromEmail) error`
// callback can't carry the profile id / ISP / subscriber id those require.
//
// This sender reuses the exact battle-tested primitives the campaign send
// worker uses — it does NOT reimplement them:
//   - ProfileBasedSender.Send  → PMTA HTTP-bridge dispatch + per-ISP VMTA
//     pool selection + warmup-limit enforcement (esp_profile.go / esp_pmta_api.go)
//   - InjectTrackingPixelAndLinks → open pixel + signed click-link rewrite
//   - ReplaceTrackingMergeTags     → {{ tracking.* }} merge tags
//   - mailing_message_log INSERT   → identical shape to send_worker.markSent
//
// Volume is low by construction (only subscribers who clicked an offer, a
// handful of staggered touches each), so per-enrollment direct dispatch is
// the right model — the high-throughput wave/quota machinery would be
// overkill and would require a 'sending' shadow campaign that the batch
// dispatcher would then have to manage.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// defaultOrgID matches the platform's single-tenant organization used across
// startup migrations and seed data.
const clickDripDefaultOrgID = "00000000-0000-0000-0000-000000000001"

// clickDripShadowNamespace is a fixed UUID namespace used to derive a
// deterministic shadow-campaign id per Everflow offer (UUID v5). This lets
// ensureShadowCampaign resolve the row with a primary-key lookup instead of a
// 90k-row sequential scan on (campaign_type, name) — the latter measured at
// ~27s in production and blew the executor's 30s context (every reminder send
// failed with "context deadline exceeded"). The namespace value is arbitrary
// but MUST stay stable so ids remain reproducible across deploys.
var clickDripShadowNamespace = uuid.MustParse("a7f3c2d1-9b8e-4c6a-8d5f-1e2b3c4d5e6f")

// shadowCampaignID returns the deterministic shadow-campaign id for one
// (offer, journey node) pair.
//
// It was originally per-OFFER, which collapsed all four reminder touches onto a
// single mailing_campaigns row. Because tracking events attach to campaign_id,
// that made per-node opens/clicks/conversions unattributable — every touch's
// engagement landed in the same bucket, and /journeys/{id}/node-stats (built
// exactly to split them) had nothing to group by. Deriving the id per node
// gives each touch its own row, so the entire existing campaign-metrics stack
// segments the funnel for free.
//
// nodeID == "" reproduces the ORIGINAL per-offer string byte-for-byte, so the
// legacy ids stay resolvable: reminders already in flight carry unsubscribe /
// view-in-browser tokens minted against them, and those rows must keep
// resolving after this change ships.
func shadowCampaignID(everflowOfferID, nodeID, contentHash string) string {
	seed := "click-drip-shadow-offer-" + everflowOfferID
	if nodeID != "" {
		seed += "-node-" + nodeID
	}
	if contentHash != "" {
		seed += "-v-" + contentHash
	}
	return uuid.NewSHA1(clickDripShadowNamespace, []byte(seed)).String()
}

// touchContentHash identifies one CREATIVE VERSION of a touch: the subject,
// preheader, from-name override and body that went out together.
//
// Operator rule (2026-08-02): a touch's metrics are the LIFETIME value of that
// creative + subject combination, and changing ANY part of it must sunset the
// old numbers as a historical aggregate rather than blending them into the new
// copy's stats. Folding this hash into the shadow-campaign id gives every
// version its own campaign_id, so the split happens in the SAME place all other
// engagement is already attributed — no per-version counters, no backfill, and
// an old version's numbers simply stop moving the moment the copy changes.
//
// Whitespace is normalized so a reformat is not mistaken for a copy change; any
// real edit to any field is.
func touchContentHash(subject, preheader, fromOverride, body string) string {
	norm := func(v string) string { return strings.Join(strings.Fields(v), " ") }
	sum := sha256.Sum256([]byte(strings.Join([]string{
		norm(subject), norm(preheader), norm(fromOverride), norm(body),
	}, "\x1f")))
	return hex.EncodeToString(sum[:])[:12]
}

// JourneyClickDripSender dispatches a single click-drip reminder through PMTA.
type JourneyClickDripSender struct {
	db             *sql.DB
	profileSender  *ProfileBasedSender
	trackingURL    string // global fallback tracking base URL
	trackingSecret string

	// shadowIDCache memoizes resolveShadowCampaignID. The mapping from
	// (offer, node, content hash) to a campaign id is IMMUTABLE once
	// established — version 0 keeps the legacy id forever and a new version
	// keeps whichever id it was first recorded under — so this is a pure
	// memo, not a cache with staleness. It keeps the version-0 lookup off
	// the per-send path after the first resolution.
	shadowIDCache sync.Map // string -> string

	// stampColsMissing latches when mailing_campaigns lacks the node-attribution
	// columns, so ensureShadowCampaign falls back to the un-stamped INSERT
	// instead of failing the send.
	//
	// WHY THIS EXISTS (2026-08-02 incident): the columns ship via the startup
	// migration runner, whose 5s statement budget includes the ACCESS EXCLUSIVE
	// lock wait on mailing_campaigns. Under active sending that lock is
	// contended, the ALTER timed out ("skipped — will retry next boot"), and the
	// new binary went live referencing a column that did not exist — every
	// click-drip reminder then failed with `column "journey_key" ... does not
	// exist`. Attribution is a REPORTING nicety; it must never be able to take
	// the send path down. Same defensive shape as JourneyEventEnroller's
	// routingColsOK and loadLaneRouting's "does not exist" tolerance.
	//
	// Guarded by mu because Send runs concurrently across journey executor
	// goroutines. Re-probed after stampRecheckAfter so the stamps switch back on
	// by themselves once the DDL lands — no restart required.
	mu               sync.Mutex
	stampColsMissing bool
	stampLastProbe   time.Time
}

// stampRecheckAfter bounds how long the sender stays in un-stamped fallback
// before retrying the stamped INSERT. Short enough that a successful migration
// starts producing attribution within minutes; long enough that a genuinely
// absent column costs one failed INSERT per interval, not per send.
const stampRecheckAfter = 5 * time.Minute

// stampsDisabled reports whether to skip the attribution columns on this INSERT.
func (s *JourneyClickDripSender) stampsDisabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stampColsMissing {
		return false
	}
	if time.Since(s.stampLastProbe) > stampRecheckAfter {
		// Let one attempt through to see whether the DDL has landed.
		s.stampColsMissing = false
		return false
	}
	return true
}

// markStampsMissing latches the fallback after a missing-column error.
func (s *JourneyClickDripSender) markStampsMissing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stampColsMissing = true
	s.stampLastProbe = time.Now()
}

// NewJourneyClickDripSender builds the sender. profileSender is the same
// instance the server already constructs for the campaign send pool, so all
// per-profile sender caching / IP-pool state is shared.
func NewJourneyClickDripSender(db *sql.DB, profileSender *ProfileBasedSender, trackingURL, trackingSecret string) *JourneyClickDripSender {
	return &JourneyClickDripSender{
		db:             db,
		profileSender:  profileSender,
		trackingURL:    trackingURL,
		trackingSecret: trackingSecret,
	}
}

// ClickDripSendParams carries everything executeEmailNode resolved for one
// reminder touch.
type ClickDripSendParams struct {
	JourneyID       string // text id, e.g. "click-drip-4touch-72h"
	NodeID          string
	EverflowOfferID string
	ReminderSeq     int
	SubscriberID    string // uuid string
	SubscriberEmail string
	Subject         string
	HTMLContent     string
	FromName        string
	FromEmail       string
	ProfileID       string // mailing_sending_profiles.id resolved by the enroller
	// Preheader participates in the creative-version hash even though the
	// sender does not render it directly — a preheader edit is a copy change.
	Preheader string
	// ContentHash identifies this touch's creative version. Empty means the
	// caller did not version this send, which reproduces the pre-2026-08-02
	// per-node id exactly.
	ContentHash string
}

// Send dispatches one reminder. Returns an error only for conditions worth
// retrying (transient send failures); permanent skips are logged and return
// nil so the executor advances the enrollment rather than looping.
func (s *JourneyClickDripSender) Send(ctx context.Context, p ClickDripSendParams) error {
	if s.profileSender == nil {
		return fmt.Errorf("click-drip sender: no profile sender configured")
	}
	if p.ProfileID == "" {
		// Without a profile we'd fall back to the platform-default IP pool,
		// which would send the brand's creative from the wrong domain (DKIM/
		// SPF misalignment → spam). Refuse rather than send misaligned mail.
		return fmt.Errorf("click-drip sender: no sending_profile_id for subscriber %s (offer %s) — refusing to send on default pool", p.SubscriberEmail, p.EverflowOfferID)
	}

	orgID := s.resolveOrgID(ctx, p.SubscriberID)
	campaignID, err := s.ensureShadowCampaign(ctx, orgID, p)
	if err != nil {
		return fmt.Errorf("click-drip sender: ensure shadow campaign: %w", err)
	}

	emailID := uuid.NewString()
	html := p.HTMLContent

	// Brand-match creative image hosts to the sending brand's img.<apex> CDN,
	// same as the campaign send worker (send_worker.go). The reused clicked
	// creative carries the neutral img.projectjarvis.io host; swap it to
	// img.<brand> when provisioned for this profile, else leave neutral. Done
	// before tracking/merge-tag rewrites so the bare creative host is matched.
	if !brandImageHostSwapDisabled() {
		if imgHost := lookupBrandImageHost(ctx, s.db, p.ProfileID); imgHost != "" {
			html = strings.ReplaceAll(html, neutralImageHost, "https://"+imgHost)
		}
	}

	// Merge tags + tracking rewrite, mirroring send_worker's ordering.
	// replaceMoneyMergeTags FIRST: the scheduler bakes lowercase
	// {{subscriber.id}}/{{brand.domain}} into every money URL
	// (&sub1=...&sub2=...) and ReplaceTrackingMergeTags only covers the
	// UPPERCASE family — without this, every journey reminder shipped the
	// literal tags and its Everflow postbacks arrived unattributable
	// (sub1="{{subscriber.id}}"). Found 2026-06-12 via operator postback audit.
	html = replaceMoneyMergeTags(html, p.SubscriberID, p.FromEmail)
	html = ReplaceTrackingMergeTags(html, campaignID, p.SubscriberID)
	trackBase := s.resolveTrackingURL(ctx, p.ProfileID)
	if trackBase != "" {
		html = InjectTrackingPixelAndLinks(html, campaignID, p.SubscriberID, emailID, trackBase, orgID, s.trackingSecret)

		// Safety net (send_worker.go parity): when the executor's Liquid
		// render was skipped (subscriber row failed to load), the creative
		// still carries literal {{ system.* }} tokens. Those MUST NOT reach
		// PMTA: the bridge parses the injected 'content' as a template AFTER
		// our quoted-printable encoding, and QP's '=' escaping / soft
		// line-breaks split a token mid-name ("{{ system.preferences_ur=")
		// → HTTP 422 "unexpected `=`, expected end of variable block", which
		// killed every such touch. Replace them with real signed URLs here so
		// no raw '{{ system.* }}' ever reaches the QP encoder and the
		// reminder always ships a working unsubscribe link (CAN-SPAM).
		html = s.renderSystemURLTokens(html, orgID, campaignID, p.SubscriberID, brandRootFromEmail(p.FromEmail), trackBase)

		// Systematic partner disclosure (operator 2026-08-07): sender-ID
		// header + intended-for/unsubscribe footer on every reminder touch,
		// injected with concrete strings — this is the path that shipped raw
		// '{{ brand.name }}' tokens to inboxes on 2026-08-04 (no Liquid brand
		// context here). Injected BEFORE GenerateTextFromHTML below so the
		// text part inherits both lines. raw_creative profiles (first-party,
		// em.wcl-heloc.com) are exempt, matching send_worker semantics.
		if !s.profileRawCreative(ctx, p.ProfileID) {
			unsubURL := GenerateUnsubscribeURL(orgID, campaignID, p.SubscriberID, trackBase, s.trackingSecret)
			html = injectPartnerDisclosureHeader(html, brand.Label(brandRootFromEmail(p.FromEmail)))
			html = injectIntendedForFooter(html, p.SubscriberEmail, unsubURL, time.Now())
		}
	}

	headers := buildClickDripHeaders(orgID, campaignID, p, trackBase, s.trackingSecret)

	// text/plain alternative (send_worker.go parity): generated from the FINAL
	// html — after tracking/system-URL rewrites — so the text part carries real
	// signed unsubscribe/tracking URLs, never literal {{ system.* }} tokens.
	// Without this the ESP builders emit multipart/alternative with a single
	// HTML part, and every journey touch shipped with no plain-text body.
	textContent := GenerateTextFromHTML(html)

	msg := &EmailMessage{
		ID:           emailID,
		CampaignID:   campaignID,
		SubscriberID: p.SubscriberID,
		Email:        p.SubscriberEmail,
		FromName:     p.FromName,
		FromEmail:    p.FromEmail,
		Subject:      p.Subject,
		HTMLContent:  html,
		TextContent:  textContent,
		ProfileID:    p.ProfileID,
		ESPType:      "pmta",
		RecipientISP: ClassifySubscriberISP(p.SubscriberEmail),
		Headers:      headers,
	}

	result, err := s.profileSender.Send(ctx, msg)
	if err != nil {
		return fmt.Errorf("click-drip send to %s (profile=%s): %w", p.SubscriberEmail, p.ProfileID, err)
	}
	if result == nil || !result.Success {
		return fmt.Errorf("click-drip send to %s returned no success", p.SubscriberEmail)
	}

	s.writeMessageLog(ctx, result.MessageID, orgID, campaignID, p.SubscriberID, p.SubscriberEmail)
	log.Printf("JourneyClickDripSender: sent reminder step=%d offer=%s to %s via profile=%s (vmta=%s, campaign=%s)",
		p.ReminderSeq, p.EverflowOfferID, p.SubscriberEmail, p.ProfileID, result.VMTA, campaignID)
	return nil
}

// buildClickDripHeaders assembles a reminder touch's SMTP headers: the
// X-Journey-* diagnostics plus — when a tracking base exists — the shared
// RFC 8058 one-click List-Unsubscribe pair, via the exact same helper the
// campaign send worker uses (list_unsub_headers.go), so journey reminders are
// header-identical to broadcast sends.
//
// Gap fixed 2026-07-21: reminders shipped with ONLY the X-Journey-* headers —
// no List-Unsubscribe at all — while Google Postmaster flagged every sending
// domain "Not compliant" for missing one-click unsubscribe.
func buildClickDripHeaders(orgID, campaignID string, p ClickDripSendParams, trackBase, secret string) map[string]string {
	headers := map[string]string{
		"X-Journey-ID":       p.JourneyID,
		"X-Click-Drip-Offer": p.EverflowOfferID,
		"X-Click-Drip-Step":  fmt.Sprintf("%d", p.ReminderSeq),
	}
	if trackBase != "" {
		BuildListUnsubscribeHeaders(orgID, campaignID, p.SubscriberID,
			brandRootFromEmail(p.FromEmail), p.FromEmail, trackBase, secret, headers)
	}
	return headers
}

// replaceMoneyMergeTags substitutes the scheduler-pipeline lowercase merge
// tags that ReplaceTrackingMergeTags (UPPERCASE family) does not cover:
// {{subscriber.id}} (Everflow sub1) and {{brand.domain}} (sub2), including
// whitespace and URL-encoded variants. Brand root derives from the sending
// address (…@em.<apex> → <apex>), matching send_worker's
// rc["brand"]["domain"] semantics.
func replaceMoneyMergeTags(html, subscriberID, fromEmail string) string {
	for _, tag := range []string{"{{subscriber.id}}", "{{ subscriber.id }}"} {
		html = strings.ReplaceAll(html, tag, subscriberID)
	}
	html = strings.ReplaceAll(html, "%7B%7Bsubscriber.id%7D%7D", subscriberID)
	if brand := brandRootFromEmail(fromEmail); brand != "" {
		for _, tag := range []string{"{{brand.domain}}", "{{ brand.domain }}"} {
			html = strings.ReplaceAll(html, tag, brand)
		}
		html = strings.ReplaceAll(html, "%7B%7Bbrand.domain%7D%7D", brand)
	}
	return html
}

// brandRootFromEmail derives the brand apex from a sending address
// (…@em.<apex> → <apex>), matching send_worker's rc["brand"]["domain"] /
// item.BrandRoot semantics. Returns "" when no domain can be derived.
func brandRootFromEmail(fromEmail string) string {
	if i := strings.LastIndex(fromEmail, "@"); i >= 0 {
		return strings.TrimPrefix(fromEmail[i+1:], "em.")
	}
	return ""
}

// SystemURLs returns the broadcast-parity {{ system.* }} URL values for one
// click-drip touch, built with the SAME generators the campaign send worker
// uses in buildRenderContext (send_worker.go: GenerateUnsubscribeURL /
// GenerateBrandUnsubscribeURL / "%s/preferences?sid=%s"). The executor merges
// these into the Liquid render context so the creative's footer links render
// to real signed URLs — BuildContext is called there with campaign=nil, so it
// cannot populate them itself. The campaign id is the deterministic per-offer
// shadow-campaign id, i.e. the same id the subsequent Send() logs against, so
// unsubscribe tokens resolve to the row the message is attributed to.
// Returns nil when no tracking base is configured (no sensible URL to build).
func (s *JourneyClickDripSender) SystemURLs(ctx context.Context, everflowOfferID, nodeID, contentHash, subscriberID, profileID, fromEmail string) map[string]interface{} {
	trackBase := s.resolveTrackingURL(ctx, profileID)
	if trackBase == "" || subscriberID == "" {
		return nil
	}
	// MUST use the same (offer, node) id ensureShadowCampaign/Send resolve, or
	// the unsubscribe + view-in-browser tokens would point at a different
	// campaign row than the message is attributed to.
	campaignID := shadowCampaignID(everflowOfferID, nodeID, contentHash)
	orgID := s.resolveOrgID(ctx, subscriberID)
	return map[string]interface{}{
		"unsubscribe_url":       GenerateUnsubscribeURL(orgID, campaignID, subscriberID, trackBase, s.trackingSecret),
		"brand_unsubscribe_url": GenerateBrandUnsubscribeURL(orgID, campaignID, subscriberID, brandRootFromEmail(fromEmail), trackBase, s.trackingSecret),
		"preferences_url":       fmt.Sprintf("%s/preferences?sid=%s", trackBase, subscriberID),
		"view_in_browser_url":   fmt.Sprintf("%s/view?cid=%s&sid=%s", trackBase, campaignID, subscriberID),
	}
}

// renderSystemURLTokens replaces any literal {{ system.* }} URL tokens still
// present in the creative with real signed URLs — the same post-render safety
// net the campaign send worker applies after tracking injection
// (send_worker.go processQueueItem), covering the case where the executor's
// Liquid render was skipped. It also injects a minimal unsubscribe block when
// the body carries none (CAN-SPAM), mirroring send_worker's fallback, so a
// click-drip reminder can never ship without a working unsubscribe link.
// residualMustacheRe matches any leftover '{{ ... }}' token (non-greedy, no
// nested braces). Used as the last-resort strip before QP encoding.
var residualMustacheRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

func (s *JourneyClickDripSender) renderSystemURLTokens(html, orgID, campaignID, subscriberID, brandRoot, trackBase string) string {
	unsubURL := GenerateUnsubscribeURL(orgID, campaignID, subscriberID, trackBase, s.trackingSecret)
	brandUnsubURL := GenerateBrandUnsubscribeURL(orgID, campaignID, subscriberID, brandRoot, trackBase, s.trackingSecret)
	prefsURL := fmt.Sprintf("%s/preferences?sid=%s", trackBase, subscriberID)
	for tag, url := range map[string]string{
		"{{ system.unsubscribe_url }}":       unsubURL,
		"{{system.unsubscribe_url}}":         unsubURL,
		"{{ system.brand_unsubscribe_url }}": brandUnsubURL,
		"{{system.brand_unsubscribe_url}}":   brandUnsubURL,
		"{{ system.preferences_url }}":       prefsURL,
		"{{system.preferences_url}}":         prefsURL,
	} {
		html = strings.ReplaceAll(html, tag, url)
	}

	// EXHAUSTIVE SWEEP (2026-08-04): the enumerated map above covers only the
	// three URL tokens. Any OTHER surviving '{{ ... }}' — e.g. the footer's
	// '{{ system.current_year }}' — hits the same QP-split → PMTA 422 class
	// ("unexpected `=`" at the copyright line), which cost ~1-2% of touches.
	// Render the known scalars, then strip anything still unresolved so no raw
	// mustache can reach the QP encoder.
	html = strings.ReplaceAll(html, "{{ system.current_year }}", strconv.Itoa(time.Now().Year()))
	html = strings.ReplaceAll(html, "{{system.current_year}}", strconv.Itoa(time.Now().Year()))
	if strings.Contains(html, "{{") {
		html = residualMustacheRe.ReplaceAllString(html, "")
	}

	// CAN-SPAM: if no unsub link exists in the body, inject one before </body>
	// (identical block to send_worker.go's fallback).
	if !strings.Contains(strings.ToLower(html), "/track/unsubscribe/") {
		unsubBlock := fmt.Sprintf(
			`<div style="text-align:center;padding:16px;font-size:12px;color:#999;font-family:Arial,sans-serif;">`+
				`<a href="%s" style="color:#999;text-decoration:underline;">Unsubscribe</a></div>`, unsubURL)
		if idx := strings.LastIndex(strings.ToLower(html), "</body>"); idx >= 0 {
			html = html[:idx] + unsubBlock + html[idx:]
		} else {
			html += unsubBlock
		}
	}
	return html
}

// resolveOrgID returns the subscriber's organization, falling back to the
// single-tenant default.
func (s *JourneyClickDripSender) resolveOrgID(ctx context.Context, subscriberID string) string {
	if sid, err := uuid.Parse(subscriberID); err == nil && sid != uuid.Nil {
		var org sql.NullString
		_ = s.db.QueryRowContext(ctx,
			`SELECT organization_id::text FROM mailing_subscribers WHERE id=$1`, sid).Scan(&org)
		if org.Valid && org.String != "" {
			return org.String
		}
	}
	return clickDripDefaultOrgID
}

// resolveTrackingURL returns the profile's tracking domain if configured,
// else the global tracking URL. Mirrors SendWorkerPool.resolveTrackingURL so
// open/click links carry the brand's tracking host.
func (s *JourneyClickDripSender) resolveTrackingURL(ctx context.Context, profileID string) string {
	base := s.trackingURL
	if profileID == "" {
		return base
	}
	pid, err := uuid.Parse(profileID)
	if err != nil {
		return base
	}
	var trackingDomain, sendingDomain sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(tracking_domain,''), COALESCE(sending_domain,'') FROM mailing_sending_profiles WHERE id=$1`,
		pid).Scan(&trackingDomain, &sendingDomain)
	if err != nil {
		return base
	}
	if trackingDomain.Valid && trackingDomain.String != "" {
		return normalizeTrackBase(trackingDomain.String)
	}
	return base
}

// profileRawCreative reports whether the sending profile is flagged
// raw_creative (first-party mail, e.g. em.wcl-heloc.com) and therefore exempt
// from partner-disclosure injection. Fails open to FALSE (inject) — a
// transient lookup error must not let an offer touch ship without its
// disclosure; missing-column errors (pre-migration DB) also land here.
func (s *JourneyClickDripSender) profileRawCreative(ctx context.Context, profileID string) bool {
	pid, err := uuid.Parse(profileID)
	if err != nil {
		return false
	}
	var raw sql.NullBool
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(raw_creative, FALSE) FROM mailing_sending_profiles WHERE id=$1`,
		pid).Scan(&raw); err != nil {
		return false
	}
	return raw.Valid && raw.Bool
}

func normalizeTrackBase(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return d
	}
	if !strings.HasPrefix(d, "http://") && !strings.HasPrefix(d, "https://") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
}

// ensureShadowCampaign returns the id of a persistent, INERT campaign row used
// purely as an attribution + message_log FK anchor. One row per
// (everflow_offer_id), reused across all click-drip touches.
//
// Isolation contract (fixed 2026-06-01 after a double-send incident):
// click-drip reminders are delivered DIRECTLY by JourneyClickDripSender via the
// ProfileBasedSender (PMTA HTTP bridge). The anchor row must therefore never be
// claimable by ANY campaign lifecycle worker. The previous version used
// campaign_type='journey_node' + execution_mode='pmta_isp_wave' + status='draft',
// which exactly matched the JourneyEmailNodeActivator / audience-finalizer /
// wave-planner pipeline — so the anchor was promoted draft→preparing→scheduled
// and a SECOND, wave-based send was planned for an email already sent directly.
//
// The anchor now uses three independently-inert properties so no claim
// predicate in the codebase can match it:
//   - status='sent'              (terminal; send/schedule/finalize workers skip)
//   - campaign_type='click_drip' (NOT 'journey_node'/'regular'; no worker claims
//     it, and it stays out of regular campaign lists)
//   - execution_mode='standard'  (NOT 'pmta_isp_wave'; the wave planner and
//     campaign_health_monitor only act on pmta_isp_wave)
//
// The id is DETERMINISTIC per offer (shadowCampaignID), so the existence check
// is a primary-key lookup rather than a (campaign_type, name) sequential scan.
// The prior name-scan version took ~27s on a 90k-row mailing_campaigns table
// and tripped the executor's 30s context, failing every reminder send.

// resolveShadowCampaignID picks the campaign id this touch's metrics belong to,
// and is the whole reason wiring ContentHash did not detach production history.
//
// THE HAZARD: shadowCampaignID seeds a UUIDv5 with
// "click-drip-shadow-offer-<offer>-node-<node>[-v-<hash>]". ContentHash was
// never populated, so EVERY production campaign id was minted from the hashless
// seed. Introducing the hash changes the seed, changes the UUID, and every
// historical lake event stays bound to the id nothing points at any more — all
// 22 lanes would have read zero on the next send.
//
// THE FIX: the hashless id is VERSION 0. The first version we ever record for a
// (offer, node) adopts it, so metrics continue on the id the lake already has.
// Only a LATER creative change — a hash we have not seen for this node — mints a
// new versioned id and freezes the previous version's numbers, which is the
// behaviour the versioning was designed for. See METRIC_CONTRACT.md §10.12.
func (s *JourneyClickDripSender) resolveShadowCampaignID(ctx context.Context, p ClickDripSendParams) string {
	legacyID := shadowCampaignID(p.EverflowOfferID, p.NodeID, "")
	if p.ContentHash == "" || p.NodeID == "" || p.EverflowOfferID == "" {
		return legacyID
	}

	memoKey := p.EverflowOfferID + "\x00" + p.NodeID + "\x00" + p.ContentHash
	if v, ok := s.shadowIDCache.Load(memoKey); ok {
		if id, _ := v.(string); id != "" {
			return id
		}
	}
	resolved := legacyID
	memoize := true
	defer func() {
		if memoize {
			s.shadowIDCache.Store(memoKey, resolved)
		}
	}()

	// Has this (offer, node) ever recorded a version? If the registry is empty
	// for it, this hash is version 0 and inherits the legacy id.
	var seen int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_clickdrip_touch_versions
		 WHERE everflow_offer_id = $1 AND node_id = $2
	`, p.EverflowOfferID, p.NodeID).Scan(&seen)
	if err != nil {
		// Registry unavailable (pre-migration). The SAFE default is the legacy
		// id: it keeps history attached. A missing split is recoverable, a
		// detached three months of metrics is not. NOT memoized — a transient
		// registry error must not pin this touch to the legacy id forever.
		memoize = false
		return legacyID
	}
	if seen == 0 {
		return legacyID
	}

	// Known version for this node? Reuse whatever id it was first recorded
	// under — including the legacy one for version 0.
	var known sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT shadow_campaign_id::text FROM mailing_clickdrip_touch_versions
		 WHERE everflow_offer_id = $1 AND node_id = $2 AND content_hash = $3
	`, p.EverflowOfferID, p.NodeID, p.ContentHash).Scan(&known); err == nil &&
		known.Valid && known.String != "" {
		resolved = known.String
		return resolved
	}

	// Genuinely new creative version -> its own campaign id.
	resolved = shadowCampaignID(p.EverflowOfferID, p.NodeID, p.ContentHash)
	return resolved
}

func (s *JourneyClickDripSender) ensureShadowCampaign(ctx context.Context, orgID string, p ClickDripSendParams) (string, error) {
	campaignID := s.resolveShadowCampaignID(ctx, p)
	name := fmt.Sprintf("Click-Drip Reminder · offer %s", p.EverflowOfferID)
	if p.NodeID != "" {
		name = fmt.Sprintf("Click-Drip Reminder · offer %s · %s", p.EverflowOfferID, p.NodeID)
	}

	// Fast path: primary-key existence check.
	var existing sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT id::text FROM mailing_campaigns WHERE id=$1`, campaignID).Scan(&existing)
	if existing.Valid && existing.String != "" {
		return existing.String, nil
	}

	// Resolve internal offer UUID for offer_id linkage (non-fatal if absent).
	var offerUUID uuid.NullUUID
	_ = s.db.QueryRowContext(ctx,
		`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1 LIMIT 1`,
		p.EverflowOfferID).Scan(&offerUUID)

	var profileUUID uuid.NullUUID
	if pid, err := uuid.Parse(p.ProfileID); err == nil {
		profileUUID = uuid.NullUUID{UUID: pid, Valid: true}
	}

	// Node attribution stamps (2026-08-01). journey_key holds the VARCHAR
	// journey id that the UUID journey_id column cannot; journey_offer_id is the
	// lane scope the click-funnel screen groups by (all lanes share one journey);
	// journey_wave_index carries the reminder sequence so touches sort without
	// parsing the node id. NULL-safe: a send with no node/journey context stamps
	// nothing and behaves exactly as before.
	var journeyKey, journeyNode sql.NullString
	var waveIndex sql.NullInt32
	if p.NodeID != "" {
		journeyNode = sql.NullString{String: p.NodeID, Valid: true}
		waveIndex = sql.NullInt32{Int32: int32(p.ReminderSeq), Valid: true}
	}
	if p.JourneyID != "" {
		journeyKey = sql.NullString{String: p.JourneyID, Valid: true}
	}
	journeyOffer := sql.NullString{String: p.EverflowOfferID, Valid: p.EverflowOfferID != ""}

	// Insert with the deterministic id; ON CONFLICT (id) collapses the race
	// where a concurrent touch for the same offer inserted first.
	//
	// Two shapes: the stamped INSERT (node attribution) and a legacy fallback
	// without those columns. Attribution is reporting metadata — if the columns
	// are absent because their DDL has not landed yet, the reminder must still
	// SEND. See stampColsMissing for the incident that motivated this.
	const stampedInsert = `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status,
			subject, from_name, from_email,
			campaign_type, execution_mode,
			sending_profile_id, offer_id,
			journey_key, journey_node_id, journey_offer_id, journey_wave_index,
			total_recipients, max_recipients,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, 'sent',
			$4, $5, $6,
			'click_drip', 'standard',
			$7, $8,
			$9, $10, $11, $12,
			0, 0,
			NOW(), NOW()
		)
		ON CONFLICT (id) DO UPDATE SET
			journey_key      = COALESCE(mailing_campaigns.journey_key,      EXCLUDED.journey_key),
			journey_node_id  = COALESCE(mailing_campaigns.journey_node_id,  EXCLUDED.journey_node_id),
			journey_offer_id = COALESCE(mailing_campaigns.journey_offer_id, EXCLUDED.journey_offer_id),
			journey_wave_index = COALESCE(mailing_campaigns.journey_wave_index, EXCLUDED.journey_wave_index)
		WHERE mailing_campaigns.journey_node_id IS NULL
		   OR mailing_campaigns.journey_offer_id IS NULL`
	const legacyInsert = `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status,
			subject, from_name, from_email,
			campaign_type, execution_mode,
			sending_profile_id, offer_id,
			total_recipients, max_recipients,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, 'sent',
			$4, $5, $6,
			'click_drip', 'standard',
			$7, $8,
			0, 0,
			NOW(), NOW()
		)
		ON CONFLICT (id) DO NOTHING`

	insertLegacy := func() error {
		_, err := s.db.ExecContext(ctx, legacyInsert,
			campaignID, orgID, name,
			p.Subject, p.FromName, p.FromEmail,
			profileUUID, offerUUID,
		)
		return err
	}

	// Record what this creative version SAID, so the screen can show a
	// superseded version's copy alongside its frozen metrics. Best-effort: a
	// failure here must never block the send (the metrics split itself does not
	// depend on this row — it comes from the version-keyed campaign id).
	s.recordTouchVersion(ctx, p, campaignID)

	if s.stampsDisabled() {
		if err := insertLegacy(); err != nil {
			return "", err
		}
		return campaignID, nil
	}

	_, err := s.db.ExecContext(ctx, stampedInsert,
		campaignID, orgID, name,
		p.Subject, p.FromName, p.FromEmail,
		profileUUID, offerUUID,
		journeyKey, journeyNode, journeyOffer, waveIndex,
	)
	if err != nil {
		if isMissingColumnErr(err) {
			// The attribution DDL has not landed. Degrade to the legacy shape
			// rather than fail the send; re-probed after stampRecheckAfter.
			log.Printf("JourneyClickDripSender: node-attribution columns absent (%v) — sending without attribution until the DDL lands", err)
			s.markStampsMissing()
			if lerr := insertLegacy(); lerr != nil {
				return "", lerr
			}
			return campaignID, nil
		}
		return "", err
	}
	return campaignID, nil
}

func (s *JourneyClickDripSender) writeMessageLog(ctx context.Context, messageID, orgID, campaignID, subscriberID, email string) {
	logCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(logCtx, `
		INSERT INTO mailing_message_log (id, message_id, organization_id, campaign_id, subscriber_id, email, esp_type, sent_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'pmta', NOW())
	`, messageID, orgID, campaignID, subscriberID, email)
	if err != nil {
		log.Printf("JourneyClickDripSender: message_log insert failed (msg=%s sub=%s): %v", messageID, email, err)
	}
}

// isMissingColumnErr reports whether err is Postgres' undefined-column error
// (SQLSTATE 42703). Matched on the driver's error text because the sender takes
// *sql.DB, not a pq-typed connection — same string-probe shape the enroller uses
// for its "does not exist" tolerance.
func isMissingColumnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "does not exist") &&
		(strings.Contains(s, "column") || strings.Contains(s, "journey_key") ||
			strings.Contains(s, "journey_offer_id"))
}

// recordTouchVersion upserts this touch's creative version into the registry
// that backs the screen's version history.
//
// The METRICS split does not depend on this table — that comes from the
// version-keyed shadow campaign id. This row is the human-readable half:
// what the copy actually said and when it was live, so a sunset aggregate can
// be shown next to the words that earned it. Any failure is logged and
// swallowed; reporting metadata must never block a send (2026-08-02 incident).
func (s *JourneyClickDripSender) recordTouchVersion(ctx context.Context, p ClickDripSendParams, campaignID string) {
	if p.ContentHash == "" || p.EverflowOfferID == "" || p.NodeID == "" {
		return
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_clickdrip_touch_versions (
			everflow_offer_id, node_id, content_hash, sequence_index,
			subject, preheader, from_name_override, body_html,
			shadow_campaign_id, first_seen_at, last_seen_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::uuid,NOW(),NOW())
		ON CONFLICT (everflow_offer_id, node_id, content_hash)
		DO UPDATE SET last_seen_at = NOW(),
		              shadow_campaign_id = COALESCE(mailing_clickdrip_touch_versions.shadow_campaign_id, EXCLUDED.shadow_campaign_id)
	`, p.EverflowOfferID, p.NodeID, p.ContentHash, p.ReminderSeq,
		p.Subject, p.Preheader, p.FromName, p.HTMLContent, campaignID)
	if err != nil {
		log.Printf("JourneyClickDripSender: record touch version (offer=%s node=%s): %v",
			p.EverflowOfferID, p.NodeID, err)
		return
	}
	// Anything else for this (offer,node) is now historical. Marking it here —
	// on the first send of the new version — is what makes "changing the
	// creative sunsets the old numbers" true in the data rather than only in
	// the UI.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE mailing_clickdrip_touch_versions
		   SET superseded_at = COALESCE(superseded_at, NOW())
		 WHERE everflow_offer_id=$1 AND node_id=$2 AND content_hash <> $3
		   AND superseded_at IS NULL
	`, p.EverflowOfferID, p.NodeID, p.ContentHash); err != nil {
		log.Printf("JourneyClickDripSender: sunset prior versions (offer=%s node=%s): %v",
			p.EverflowOfferID, p.NodeID, err)
	}
}
