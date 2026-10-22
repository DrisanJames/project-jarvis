package worker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// composeFailure is a render-time defect that must never reach a recipient.
// processItem turns it into quarantineUnrenderable; the render-only Inbox
// Score path surfaces it as an error.
type composeFailure struct {
	field string
	err   error
}

// composeMessage builds the EmailMessage the transport will send for one
// queue item: offer overrides, Liquid render, image-host swap, text part,
// preview text, merge tags, SES routing headers, tracking pixel + click
// wrapping, List-Unsubscribe headers, partner disclosure, CAN-SPAM fallback,
// X-Job / Feedback-ID / idempotency headers and the zero-width subject
// encoder — in the exact order processItem applied them before this block
// was extracted (2026-10-21). It performs no sends and no queue writes.
//
// The ONLY caller besides processItem is the Inbox Score renderer
// (internal/api/inbox_score.go), which needs the bytes that leave the box,
// not a lookalike. Keep this the single composition path.
func (p *SendWorkerPool) composeMessage(ctx context.Context, item QueueItem) (*EmailMessage, *composeFailure) {
	// Offer content override: use creative HTML, subject line, from name if assigned
	if item.CreativeID != uuid.Nil {
		var creativeHTML string
		if err := p.db.QueryRowContext(ctx,
			`SELECT html_content FROM mailing_offer_creatives WHERE id=$1`,
			item.CreativeID).Scan(&creativeHTML); err == nil && creativeHTML != "" {
			item.HTMLContent = creativeHTML
		}
	}
	if item.SubjectLineID != uuid.Nil {
		var subjectText string
		if err := p.db.QueryRowContext(ctx,
			`SELECT subject_line FROM mailing_offer_subject_lines WHERE id=$1`,
			item.SubjectLineID).Scan(&subjectText); err == nil && subjectText != "" {
			item.Subject = subjectText
		}
	}
	if item.FromNameID != uuid.Nil {
		var fromNameText string
		if err := p.db.QueryRowContext(ctx,
			`SELECT from_name FROM mailing_offer_from_names WHERE id=$1`,
			item.FromNameID).Scan(&fromNameText); err == nil && fromNameText != "" {
			item.FromName = fromNameText
		}
	}

	// ── Resolve per-profile tracking domain BEFORE Liquid rendering so that
	// {{ system.unsubscribe_url }} uses the brand domain, not projectjarvis.io ──
	trackBase := p.resolveTrackingURL(ctx, item.ProfileID)

	// ── SES tenant-aware routing lookup ──
	// When the profile has via_ses=true the message relays through SES.
	// This branch originally skipped ALL tracking injection ("SES is the
	// sole tracker"), on three assumptions that did not survive contact
	// with production:
	//   1. "Double-rewriting breaks DKIM" — our injection happens BEFORE
	//      submission/signing, so it never invalidates a signature; the
	//      DKIM concern only applies to SES rewriting an already-signed
	//      body, which SES avoids by DECLINING to rewrite DKIM-signed mail.
	//   2. "t.em can't serve HTTPS for the brand hosts" — disproven: the
	//      jul02 open pixel and partner-drip's baked t.em click links both
	//      track fine over SES on the same trackBase.
	//   3. "Double-counting" — SES click capture turned out to be OFF for
	//      the 15/16 PMTA-DKIM-signed brands (see the tracking branch
	//      below), so there was mostly nothing to double-count.
	// Opens were restored 2026-07-02 (InjectOpenPixel); clicks restored
	// 2026-07-13 (RewriteClickLinks) — see applySESTracking.
	// The Dedicated path (via_ses=false) is unchanged.
	sesInfo := p.resolveProfileSES(ctx, item.ProfileID)

	// ── Personalization: full Liquid template engine with all subscriber data ──
	renderCtx := p.buildRenderContext(item, trackBase)
	templateSvc := mailing.NewTemplateService()

	subject, subjErr := templateSvc.Render("s:"+item.CampaignID.String(), item.Subject, renderCtx)
	if subjErr != nil {
		log.Printf("[SendWorkerPool] RENDER_WARN campaign=%s field=subject err=%v", item.CampaignID, subjErr)
	}
	previewText, pvErr := templateSvc.Render("pv:"+item.CampaignID.String(), item.PreviewText, renderCtx)
	if pvErr != nil {
		log.Printf("[SendWorkerPool] RENDER_WARN campaign=%s field=preview_text err=%v", item.CampaignID, pvErr)
	}
	htmlContent, htmlErr := templateSvc.Render("h:"+item.CampaignID.String()+":"+item.SubscriberID.String(), item.HTMLContent, renderCtx)
	if htmlErr != nil {
		log.Printf("[SendWorkerPool] RENDER_WARN campaign=%s field=html_content err=%v", item.CampaignID, htmlErr)
	}

	// ── FAIL CLOSED on a render error (2026-08-10 incident) ──────────────
	// TemplateService.Render returns the ORIGINAL TEMPLATE STRING when the
	// Liquid parse or render fails (internal/mailing/template_engine.go:384,
	// :396). Until today this loop only logged RENDER_WARN and shipped that
	// string, so three messages reached a real inbox as raw Liquid source —
	// every {{ }} and {% %} visible to the recipient. A body we could not
	// render is not a body: quarantine it instead of mailing it.
	if rf := firstRenderFailure(
		renderFailure{"subject", subjErr},
		renderFailure{"preview_text", pvErr},
		renderFailure{"html_content", htmlErr},
	); rf != nil {
		return nil, &composeFailure{field: rf.field, err: rf.err}
	}
	// raw_creative profiles (operator 2026-07-25, wcl-heloc): the creative ships
	// AS-IS — strip any store-time compliance-footer block; the CAN-SPAM fallback
	// injection below is skipped too. List-Unsubscribe headers still apply.
	if sesInfo.RawCreative {
		htmlContent = stripUnsubDisclaimerBlock(htmlContent)
	}

	// Brand-match creative image hosts to the sending brand's img.<apex> CDN
	// (neutral img.projectjarvis.io -> img.<apex>) when that domain is
	// provisioned for this profile's sending brand; otherwise the neutral host
	// is left in place (still serves). Applied here — after render, before the
	// text/preview/tracking steps — so it covers EVERY body source (queue
	// inline, content snapshot, offer-creative override) in one place. Pure host
	// rewrite (same S3 path); idempotent; never touches money/tracking/beacon
	// URLs (those don't carry the img. host).
	if !brandImageHostSwapDisabled() {
		if imgHost := p.resolveImageHost(ctx, item.ProfileID); imgHost != "" {
			htmlContent = strings.ReplaceAll(htmlContent, neutralImageHost, "https://"+imgHost)
		}
	}

	textContent, textErr := templateSvc.Render("t:"+item.CampaignID.String()+":"+item.SubscriberID.String(), item.TextContent, renderCtx)
	if textErr != nil {
		log.Printf("[SendWorkerPool] RENDER_WARN campaign=%s field=text_content err=%v", item.CampaignID, textErr)
		// Same fail-closed rule as the html/subject bodies above: the
		// text/plain part is recipient-visible, so raw source must not ship.
		return nil, &composeFailure{field: "text_content", err: textErr}
	}

	// Detect unresolved template syntax in rendered subject (high signal)
	if strings.Contains(subject, "{{") || strings.Contains(subject, "{%") {
		log.Printf("[SendWorkerPool] UNRESOLVED_TOKEN campaign=%s field=subject rendered=%q", item.CampaignID, subject)
	}

	// Text/plain fallback is generated AFTER the tracking branches below (see
	// autoText), never from the pre-tracking HTML.
	autoText := strings.TrimSpace(textContent) == "" && strings.TrimSpace(htmlContent) != ""

	if previewText != "" {
		htmlContent = InjectPreviewText(htmlContent, previewText)
	}

	htmlContent = ReplaceTrackingMergeTags(htmlContent, item.CampaignID.String(), item.SubscriberID.String(), item.CreativeID.String())

	// ── Tracking + Unsubscribe ──
	headers := make(map[string]string)
	var unsubURL string

	// SES tenant-aware send: inject the SES routing headers BEFORE the
	// tracking-pixel branch so they're attached even when trackBase is
	// empty. PMTA's HTTP bridge passes these through to the SES SMTP
	// relay; SES routes the message to the named tenant + config set.
	if sesInfo.ViaSES {
		if sesInfo.ConfigSet != "" {
			headers["X-SES-CONFIGURATION-SET"] = sesInfo.ConfigSet
		}
		if sesInfo.TenantName != "" {
			headers["X-SES-TENANT"] = sesInfo.TenantName
		}
		// Stamp SES MessageTags so the configuration-set's event destination
		// (Send/Delivery/Bounce/Complaint/Open/Click) can be joined back to
		// the exact campaign + subscriber + send attempt. recipient_send_id is
		// the outbox queue row id (item.ID) — the same value carried in
		// X-Message-ID and Feedback-ID — so SES events, PMTA accounting, and
		// our tracking all reconcile to one identity. SES tag values are
		// restricted to [A-Za-z0-9_-] (UUIDs and ISP names are safe; sanitize
		// defensively).
		headers["X-SES-MESSAGE-TAGS"] = buildSESMessageTags(
			item.ID.String(),
			item.CampaignID.String(),
			item.SubscriberID.String(),
			ClassifySubscriberISP(item.Email),
		)
	}

	if trackBase != "" && !sesInfo.ViaSES {
		htmlContent = p.injectTrackingPixelAndLinks(
			htmlContent,
			item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(),
			trackBase,
		)
	} else if trackBase != "" && sesInfo.ViaSES {
		// SES relay sends historically skipped ALL tracking injection ("SES is
		// the sole tracker") — wrong twice over:
		//   - OPENS (fixed 2026-07-02): SES-native open tracking captures <1%
		//     of opens, so partner-drip / gmail / yahoo-family engagement never
		//     reached PG or the engaged-tier segments (0.4% drip open rate vs
		//     41% board; the tier starved). InjectOpenPixel restored them.
		//   - CLICKS (fixed 2026-07-13): SES silently DECLINES to click-wrap
		//     PMTA-DKIM-signed mail — 15 of 16 brands — so money links went
		//     out unwrapped and clicks were invisible everywhere (PG, lake,
		//     engaged tiers). RewriteClickLinks restores t.em click-wrapping
		//     with the exact same skip rules as the Dedicated path. For the
		//     one brand SES does wrap (RRU), the t.em link additionally gets
		//     SES-wrapped — a functional double redirect (t.em resolves to the
		//     full money URL, SES wraps THAT) — accepted for uniform semantics
		//     over a per-brand branch; RRU keeps SES-native capture too.
		// Kill switches: DISABLE_SES_OPEN_PIXEL=true, DISABLE_SES_CLICK_WRAP=true.
		htmlContent = applySESTracking(htmlContent,
			item.CampaignID.String(), item.SubscriberID.String(), item.ID.String(),
			trackBase, p.orgID, p.trackingSecret)
	}

	// Text/plain fallback from the FINAL html — after click-wrapping on either
	// transport — so the text part carries the same t.<domain> links as the HTML.
	// Measured 2026-10-22 (Inbox Score gate vs live render): generating it before
	// tracking shipped every raw money / advertiser host (www.codefortwo.com,
	// cloud.email-libertymutual.com, ...) unwrapped in text/plain while the HTML
	// was clean — the SURBL hit (URIBL_CT_SURBL +1.6) on Liberty came only from
	// that part, text-mode clicks bypassed tracking, and the proof path (which
	// already generated text last) could not reproduce what live mail sent.
	// Journey sends did this correctly already (journey_clickdrip_sender.go).
	if autoText {
		textContent = GenerateTextFromHTML(htmlContent)
	}

	// ── List-Unsubscribe + in-body token resolution — SHARED across BOTH
	// transports. RFC 8058 one-click List-Unsubscribe is a Gmail/Yahoo
	// bulk-sender REQUIREMENT (Feb-2024 mandate). It previously lived only
	// inside the Dedicated/PMTA branch above, so every SES-relayed send (gmail,
	// apple, and the entire yahoo-family NL ramp — all SES) shipped WITHOUT it,
	// damaging placement on exactly the lanes being ramped. The header
	// construction and the {{ system.*_url }} safety-net replacements are
	// transport-agnostic, so they run here after either tracking branch. Kill
	// switch DISABLE_LIST_UNSUB_HEADERS=true restores the pre-fix behavior (no
	// header on EITHER path) without a redeploy; token resolution still runs so
	// no send ships a literal, unresolved unsubscribe token.
	if trackBase != "" {
		var brandUnsubURL string
		unsubURL, brandUnsubURL = p.buildUnsubscribeHeaders(
			item.CampaignID.String(), item.SubscriberID.String(),
			item.BrandRoot, item.FromEmail, trackBase, headers)

		// Safety net: replace any unresolved system tokens in both HTML and text
		htmlContent = strings.ReplaceAll(htmlContent, "{{ system.unsubscribe_url }}", unsubURL)
		htmlContent = strings.ReplaceAll(htmlContent, "{{system.unsubscribe_url}}", unsubURL)
		textContent = strings.ReplaceAll(textContent, "{{ system.unsubscribe_url }}", unsubURL)
		textContent = strings.ReplaceAll(textContent, "{{system.unsubscribe_url}}", unsubURL)
		htmlContent = strings.ReplaceAll(htmlContent, "{{ system.brand_unsubscribe_url }}", brandUnsubURL)
		htmlContent = strings.ReplaceAll(htmlContent, "{{system.brand_unsubscribe_url}}", brandUnsubURL)
		textContent = strings.ReplaceAll(textContent, "{{ system.brand_unsubscribe_url }}", brandUnsubURL)
		textContent = strings.ReplaceAll(textContent, "{{system.brand_unsubscribe_url}}", brandUnsubURL)
		prefsURL := GeneratePreferencesURL(p.orgID, item.SubscriberID.String(), item.BrandRoot, trackBase, p.trackingSecret, time.Now())
		htmlContent = strings.ReplaceAll(htmlContent, "{{ system.preferences_url }}", prefsURL)
		htmlContent = strings.ReplaceAll(htmlContent, "{{system.preferences_url}}", prefsURL)
		textContent = strings.ReplaceAll(textContent, "{{ system.preferences_url }}", prefsURL)
		textContent = strings.ReplaceAll(textContent, "{{system.preferences_url}}", prefsURL)

		// Systematic partner disclosure (operator 2026-08-07): sender-ID header
		// at the top, intended-for + unsubscribe footer at the bottom, on EVERY
		// dispatched email — injected here with concrete strings (label, email,
		// signed unsub URL), never tokens. raw_creative profiles (first-party,
		// em.wcl-heloc.com) opt out, same as the compliance-footer bypass.
		// Runs BEFORE the CAN-SPAM fallback so the footer's unsub link
		// satisfies its check and the fallback stays a kill-switch safety net.
		if !sesInfo.RawCreative {
			discLabel := brand.Label(item.BrandRoot)
			if discLabel == "" {
				discLabel = brand.Label(brandRootFromEmail(item.FromEmail))
			}
			now := time.Now()
			htmlContent = injectPartnerDisclosureHeader(htmlContent, discLabel)
			htmlContent = injectIntendedForFooter(htmlContent, item.Email, unsubURL, now)
			if hdr, ftr := disclosureTextParts(discLabel, item.Email, unsubURL, now); hdr != "" || ftr != "" {
				// Same legacy-block rule as the HTML injector: creatives that
				// still carry a baked-in disclosure keep it (once, not twice).
				if hdr != "" && !strings.Contains(strings.ToLower(textContent), legacyDisclosurePhrase) {
					textContent = hdr + "\n\n" + textContent
				}
				if ftr != "" && !strings.Contains(textContent, "This email was intended for ") {
					textContent = strings.TrimRight(textContent, "\n") + "\n\n" + ftr + "\n"
				}
			}
		}

		// CAN-SPAM: if no unsub link exists in the body, inject one before </body>.
		// raw_creative profiles opt out — the creative owns its own compliance.
		if !sesInfo.RawCreative && !strings.Contains(strings.ToLower(htmlContent), "/track/unsubscribe/") {
			unsubBlock := fmt.Sprintf(
				`<div style="text-align:center;padding:16px;font-size:12px;color:#999;font-family:Arial,sans-serif;">`+
					`<a href="%s" style="color:#999;text-decoration:underline;">Unsubscribe</a></div>`, unsubURL)
			if idx := strings.LastIndex(strings.ToLower(htmlContent), "</body>"); idx >= 0 {
				htmlContent = htmlContent[:idx] + unsubBlock + htmlContent[idx:]
			} else {
				htmlContent += unsubBlock
			}
		}
	}
	headers["X-Job"] = item.CampaignID.String()

	// Idempotency correlation. When the durable outbox is active and the
	// queue row carries a deterministic idempotency_key, expose it as a
	// custom SMTP header so PMTA's accounting stream (and by extension the
	// inbound /engine/webhook pipeline) can correlate each accounting record
	// back to the exact outbox row that produced it. Nil-UUIDs (legacy rows
	// inserted before the outbox schema migration) are skipped so legacy
	// traffic continues to behave verbatim.
	if item.IdempotencyKey != uuid.Nil {
		headers["X-Ignite-Idempotency-Key"] = item.IdempotencyKey.String()
	}

	// Feedback-ID enables Gmail FBL (Postmaster Tools) complaint attribution.
	headers["Feedback-ID"] = buildFeedbackID(item.CampaignID.String(), item.FromEmail)

	// Zero-width Subject steganography (subject_zw_encode.go): Yahoo-only,
	// per-sending-domain-gated, OFF by default. When enabled for this profile
	// and the recipient is Yahoo, the visible subject is unchanged but carries
	// an invisible per-domain secret payload. No-op for every other lane. Kill
	// switch: DISABLE_SUBJECT_ZW_ENCODE=true.
	recipientISP := ClassifySubscriberISP(item.Email)
	subject = p.maybeEncodeSubject(ctx, subject, recipientISP, item.ProfileID)

	msg := &EmailMessage{
		ID:           item.ID.String(),
		CampaignID:   item.CampaignID.String(),
		SubscriberID: item.SubscriberID.String(),
		Email:        item.Email,
		FromName:     item.FromName,
		FromEmail:    item.FromEmail,
		ReplyTo:      item.ReplyTo,
		Subject:      subject,
		HTMLContent:  htmlContent,
		TextContent:  textContent,
		PreviewText:  previewText,
		ProfileID:    item.ProfileID,
		ESPType:      item.ESPType,
		RecipientISP: recipientISP,
		AssignedVMTA: item.AssignedVMTA,
		Headers:      headers,
	}

	return msg, nil
}
