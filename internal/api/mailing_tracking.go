package api

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

func emailHash(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("%x", h)
}

// Helper to sign tracking data
func signData(data, key string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// verifySig checks that the HMAC-SHA256 signature (truncated to 16 hex
// chars, matching send_worker.trackSign) is valid for the given data.
// Returns true when verification passes OR when no signing key is configured.
func (svc *MailingService) verifySig(encoded, sig string) bool {
	if svc.signingKey == "" || sig == "" {
		return true
	}
	expected := signData(encoded, svc.signingKey)[:16]
	return hmac.Equal([]byte(expected), []byte(sig))
}

// ========== REAL-TIME TRACKING HANDLERS ==========

// HandleTrackOpen records an email open event
func (svc *MailingService) HandleTrackOpen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	encoded := chi.URLParam(r, "data")
	sig := chi.URLParam(r, "sig")

	if sig != "" && !svc.verifySig(encoded, sig) {
		log.Printf("TRACK OPEN: invalid signature for data=%s", encoded[:min(32, len(encoded))])
		svc.serveTrackingPixel(w)
		return
	}

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		svc.serveTrackingPixel(w)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 4 {
		svc.serveTrackingPixel(w)
		return
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	emailID, _ := uuid.Parse(parts[3])

	// Reject malformed/tampered tracking URLs. uuid.Parse returns the
	// zero UUID (00000000-...) on error, so treating any nil component
	// as a miss prevents us from writing rows keyed on uuid.Nil (which
	// would collapse unrelated tracking events onto a single synthetic
	// row and poison downstream analytics).
	if campaignID == uuid.Nil || subscriberID == uuid.Nil || emailID == uuid.Nil {
		log.Printf("TRACK OPEN: rejecting tracking pixel with nil ids (camp=%s sub=%s email_id=%s)", campaignID, subscriberID, emailID)
		svc.serveTrackingPixel(w)
		return
	}

	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	isp := extractISP(email)
	log.Printf("TRACK OPEN: campaign=%s subscriber=%s email=%s isp=%s", campaignID, subscriberID, email, isp)

	// Fire in-memory tracker FIRST so dashboards update even if DB write fails
	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "open", email, isp)
	}

	// MPP detection: open within 120s of the sent event (sent timestamp is reliable; delivered is batch-delayed)
	isMachineOpen := false
	var refAt time.Time
	err = svc.db.QueryRowContext(ctx, `
		SELECT event_at FROM mailing_tracking_events
		WHERE subscriber_id = $1 AND campaign_id = $2 AND event_type = 'sent'
		ORDER BY event_at DESC LIMIT 1
	`, subscriberID, campaignID).Scan(&refAt)
	if err == nil && time.Since(refAt) <= 120*time.Second {
		isMachineOpen = true
	}

	if _, err := svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent, device_type, sending_domain, is_machine_open, recipient_domain)
		SELECT $1, $2, $3, $4, 'opened', NOW(), $5::inet, $6, $7,
			LOWER(SPLIT_PART(c.from_email, '@', 2)), $8,
			LOWER(SPLIT_PART(s.email, '@', 2))
		FROM mailing_campaigns c
		LEFT JOIN mailing_subscribers s ON s.id = $4::uuid
		WHERE c.id = $3
		ON CONFLICT DO NOTHING
	`, emailID, orgID, campaignID, subscriberID, extractIPFromRemoteAddr(r.RemoteAddr), r.UserAgent(), detectDeviceType(r.UserAgent()), isMachineOpen); err != nil {
		log.Printf("TRACK OPEN DB ERROR: %v", err)
	}

	// Stamp offer attribution from campaign queue
	if campaignID != uuid.Nil && subscriberID != uuid.Nil {
		svc.db.ExecContext(ctx, `
			UPDATE mailing_tracking_events te
			SET offer_id = q.offer_id, creative_id = q.creative_id,
				subject_line_id = q.subject_line_id, from_name_id = q.from_name_id
			FROM mailing_campaign_queue q
			WHERE q.campaign_id = $1 AND q.subscriber_id = $2
			  AND te.campaign_id = $1 AND te.subscriber_id = $2
			  AND te.offer_id IS NULL AND q.offer_id IS NOT NULL
		`, campaignID, subscriberID)
	}

	if isMachineOpen {
		log.Printf("TRACK OPEN MPP: campaign=%s subscriber=%s delta=%v", campaignID, subscriberID, time.Since(refAt))
	}

	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET open_count = COALESCE(open_count, 0) + 1 WHERE id = $1`, campaignID)

	svc.db.ExecContext(ctx, `
		UPDATE mailing_subscribers SET total_opens = COALESCE(total_opens, 0) + 1, last_open_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, subscriberID)

	// Master List Migration P2 — shadow write to subscriber_domain_state.
	// Sending domain resolved from the campaign's from_email; empty means
	// the helper is a no-op (defense against campaigns with missing data).
	sdsSendingDomain := mailing.ResolveSendingDomainForCampaign(ctx, svc.db, campaignID)
	mailing.UpsertSDSOpen(ctx, svc.db, subscriberID, sdsSendingDomain)
	mailing.RecomputeSDSScoreLocal(ctx, svc.db, subscriberID, sdsSendingDomain)

	domain := ""
	if atIdx := strings.LastIndex(email, "@"); atIdx >= 0 {
		domain = strings.ToLower(email[atIdx+1:])
	}
	eHash := emailHash(email)
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, isp, total_opens, last_open_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 1, NOW(), NOW())
		ON CONFLICT (email_hash) DO UPDATE SET total_opens = mailing_inbox_profiles.total_opens + 1, last_open_at = NOW(), updated_at = NOW()
	`, eHash, email, domain, isp)
	svc.recomputeInboxProfileScore(ctx, eHash)

	svc.updateEngagementScore(ctx, subscriberID)
	svc.updateISPAgent(ctx, campaignID, isp, "open")
	svc.incrOpenEngagement(isp, isMachineOpen)

	svc.serveTrackingPixel(w)
}

// HandleTrackClick records a click event and redirects
func (svc *MailingService) HandleTrackClick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	encoded := chi.URLParam(r, "data")
	sig := chi.URLParam(r, "sig")

	if sig != "" && !svc.verifySig(encoded, sig) {
		log.Printf("TRACK CLICK: invalid signature for data=%s", encoded[:min(32, len(encoded))])
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "Invalid tracking link", http.StatusBadRequest)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 5 {
		http.Error(w, "Invalid tracking data", http.StatusBadRequest)
		return
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	emailID, _ := uuid.Parse(parts[3])
	originalURL := parts[4]

	// Reject malformed/tampered tracking URLs before any DB write. Nil
	// UUID components would either fail FK constraints (logged and
	// silently dropped) or collapse unrelated events onto a synthetic
	// zero-UUID row. For click tracking we still redirect when a URL
	// is present so the user experience is not broken.
	if campaignID == uuid.Nil || subscriberID == uuid.Nil || emailID == uuid.Nil {
		log.Printf("TRACK CLICK: rejecting tracking link with nil ids (camp=%s sub=%s email_id=%s)", campaignID, subscriberID, emailID)
		if originalURL != "" {
			http.Redirect(w, r, originalURL, http.StatusFound)
			return
		}
		http.Error(w, "Invalid tracking data", http.StatusBadRequest)
		return
	}

	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	isp := extractISP(email)
	log.Printf("TRACK CLICK: campaign=%s subscriber=%s email_id=%s url=%s isp=%s", campaignID, subscriberID, emailID, originalURL, isp)

	// Fire in-memory tracker FIRST
	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "click", email, isp)
	}

	if _, err := svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent, device_type, link_url, sending_domain, recipient_domain)
		SELECT $1, $2, $3, $4, 'clicked', NOW(), $5::inet, $6, $7, $8,
			LOWER(SPLIT_PART(c.from_email, '@', 2)),
			LOWER(SPLIT_PART(s.email, '@', 2))
		FROM mailing_campaigns c
		LEFT JOIN mailing_subscribers s ON s.id = $4::uuid
		WHERE c.id = $3
	`, uuid.New(), orgID, campaignID, subscriberID, extractIPFromRemoteAddr(r.RemoteAddr), r.UserAgent(), detectDeviceType(r.UserAgent()), originalURL); err != nil {
		log.Printf("TRACK CLICK DB ERROR: %v", err)
	}

	// Stamp offer attribution from campaign queue
	if campaignID != uuid.Nil && subscriberID != uuid.Nil {
		svc.db.ExecContext(ctx, `
			UPDATE mailing_tracking_events te
			SET offer_id = q.offer_id, creative_id = q.creative_id,
				subject_line_id = q.subject_line_id, from_name_id = q.from_name_id
			FROM mailing_campaign_queue q
			WHERE q.campaign_id = $1 AND q.subscriber_id = $2
			  AND te.campaign_id = $1 AND te.subscriber_id = $2
			  AND te.offer_id IS NULL AND q.offer_id IS NOT NULL
		`, campaignID, subscriberID)
	}

	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET click_count = COALESCE(click_count, 0) + 1 WHERE id = $1`, campaignID)

	svc.db.ExecContext(ctx, `
		UPDATE mailing_subscribers SET total_clicks = COALESCE(total_clicks, 0) + 1, last_click_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, subscriberID)

	// Master List Migration P2 — shadow write to subscriber_domain_state.
	sdsClickDomain := mailing.ResolveSendingDomainForCampaign(ctx, svc.db, campaignID)
	mailing.UpsertSDSClick(ctx, svc.db, subscriberID, sdsClickDomain)
	mailing.RecomputeSDSScoreLocal(ctx, svc.db, subscriberID, sdsClickDomain)

	clickDomain := ""
	if atIdx := strings.LastIndex(email, "@"); atIdx >= 0 {
		clickDomain = strings.ToLower(email[atIdx+1:])
	}
	clickEHash := emailHash(email)
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, isp, total_clicks, last_click_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 1, NOW(), NOW())
		ON CONFLICT (email_hash) DO UPDATE SET total_clicks = mailing_inbox_profiles.total_clicks + 1, last_click_at = NOW(), updated_at = NOW()
	`, clickEHash, email, clickDomain, isp)
	svc.recomputeInboxProfileScore(ctx, clickEHash)

	svc.updateEngagementScore(ctx, subscriberID)
	svc.updateISPAgent(ctx, campaignID, isp, "click")
	svc.incrClickEngagement(isp, subscriberID.String())

	redirectURL := enrichOwnedDomainURL(originalURL, subscriberID, emailID, campaignID)
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
}

// ownedDomains is kept here as a package-local alias for brand.OwnedDomains
// so that enrichOwnedDomainURL below continues to compile without touching
// every callsite. All new callers should use brand.OwnedDomains directly.
var ownedDomains = brand.OwnedDomains

// BrandRoot is a thin wrapper around brand.Root so existing callers in
// this package can reference api.BrandRoot. New callers should prefer
// brand.Root / brand.RootFromEmail directly.
func BrandRoot(sendingDomain string) string { return brand.Root(sendingDomain) }

// enrichOwnedDomainURL encodes subscriber identity into the URL **path** rather
// than query parameters. ISPs (Apple LTP, Yahoo, Firefox ETP) strip tracking
// query params but cannot strip path segments without breaking the link.
//
// Input:  https://discountblog.com/blog/best-deals
// Output: https://discountblog.com/e/<base64url(sid|eid|cid)>/blog/best-deals
//
// The brand site's Next.js middleware intercepts /e/<token>/, decodes the
// identity, sets a first-party HttpOnly cookie, and rewrites to the clean path.
func enrichOwnedDomainURL(rawURL string, subscriberID, emailID, campaignID uuid.UUID) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := strings.ToLower(u.Hostname())
	owned := false
	for _, d := range ownedDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			owned = true
			break
		}
	}
	if !owned {
		return rawURL
	}
	token := base64.RawURLEncoding.EncodeToString(
		[]byte(subscriberID.String() + "|" + emailID.String() + "|" + campaignID.String()),
	)
	cleanPath := strings.TrimPrefix(u.Path, "/")
	u.Path = "/e/" + token + "/" + cleanPath
	return u.String()
}

// VersionTrackUnsubscribe tracks handler semantics — bumped to 2.0 with the
// introduction of brand-scoped tokens (4-part payload).
const VersionTrackUnsubscribe = "2.0"

// HandleTrackUnsubscribe records an unsubscribe event and feeds the suppression
// repositories. Branches on token payload shape:
//
//   3-part (org|campaign|subscriber)        → GLOBAL unsubscribe. Flips
//       subscriber.status to 'unsubscribed', writes mailing_suppressions +
//       mailing_global_suppressions. Classic bottom-link / legacy behaviour.
//
//   4-part (org|campaign|subscriber|brand)  → BRAND-SCOPED unsubscribe. Does
//       NOT touch subscriber.status (they remain mailable for other brands);
//       writes mailing_domain_suppressions via globalHub.SuppressScoped so the
//       in-memory hub updates atomically with the DB. Validates the brand
//       token against the campaign's actual from_email brand root as
//       defense-in-depth — a signed URL is enough to prove intent, but
//       validating also catches accidental mis-encoding and forged payloads
//       that somehow leaked past the HMAC check.
func (svc *MailingService) HandleTrackUnsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	encoded := chi.URLParam(r, "data")
	sig := chi.URLParam(r, "sig")

	w.Header().Set("X-Api-Version", VersionTrackUnsubscribe)

	if sig != "" && !svc.verifySig(encoded, sig) {
		log.Printf("TRACK UNSUB: invalid signature for data=%s", encoded[:min(32, len(encoded))])
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "Invalid unsubscribe link", http.StatusBadRequest)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 3 {
		http.Error(w, "Invalid data", http.StatusBadRequest)
		return
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])

	// 4-part payload = brand-scoped. Empty brand after trim collapses to
	// global for safety (never silently accept a malformed brand token).
	brandRoot := ""
	if len(parts) >= 4 {
		brandRoot = strings.ToLower(strings.TrimSpace(parts[3]))
	}

	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	ispGroup := extractISP(email)
	remoteIPPtr := extractIPFromRemoteAddr(r.RemoteAddr)
	remoteIP := ""
	if remoteIPPtr != nil {
		remoteIP = *remoteIPPtr
	}

	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "unsubscribe", email, ispGroup)
	}

	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, 'unsubscribed', NOW(), $5::inet, $6)
	`, uuid.New(), orgID, campaignID, subscriberID, remoteIPPtr, r.UserAgent())

	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET unsubscribe_count = COALESCE(unsubscribe_count, 0) + 1 WHERE id = $1`, campaignID)

	if brandRoot != "" {
		// Defense-in-depth: validate the brand token matches the campaign's
		// actual sending brand. The HMAC alone proves the URL came from us,
		// but this also catches: (a) accidental copy/paste of a token across
		// campaigns, (b) forged tokens that slipped past a compromised
		// signing key. Mismatch → fall through to global so the user is
		// still unsubscribed (fail-safe for the subscriber).
		var campFromEmail string
		svc.db.QueryRowContext(ctx, `SELECT COALESCE(from_email,'') FROM mailing_campaigns WHERE id = $1`, campaignID).Scan(&campFromEmail)
		expected := brand.RootFromEmail(campFromEmail)
		if expected == "" || expected == brandRoot {
			if email != "" && svc.globalHub != nil {
				if err := svc.globalHub.SuppressScoped(ctx, email, brandRoot, "user_unsubscribe", "tracking_link", ispGroup, remoteIP, campaignID.String()); err != nil {
					log.Printf("TRACK UNSUB brand=%s: SuppressScoped failed: %v", brandRoot, err)
				}
			}
			log.Printf("TRACK UNSUBSCRIBE (brand): campaign=%s subscriber=%s email=%s brand=%s -> brand suppression", campaignID, subscriberID, logger.RedactEmail(email), brandRoot)
		} else {
			log.Printf("TRACK UNSUB brand mismatch: token=%s expected=%s — falling through to GLOBAL as fail-safe", brandRoot, expected)
			brandRoot = "" // force the global path below
		}
	}

	if brandRoot == "" {
		svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE id = $1`, subscriberID)

		svc.db.ExecContext(ctx, `
			INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at, updated_at)
			VALUES ($1, $2, 'User unsubscribed', 'unsubscribe', true, NOW(), NOW())
			ON CONFLICT (email) DO UPDATE SET active = true, reason = 'User unsubscribed', updated_at = NOW()
		`, uuid.New(), email)

		// Route the global write through the hub so the in-memory cache
		// updates atomically with the DB (fixes opportunistic cache drift
		// between boot-time LoadFromDB and per-request writes).
		if email != "" && svc.globalHub != nil {
			if _, err := svc.globalHub.Suppress(ctx, email, "unsubscribe", "tracking_link", ispGroup, "", "", remoteIP, campaignID.String()); err != nil {
				log.Printf("TRACK UNSUB: global hub Suppress failed: %v", err)
			}
		} else if email != "" {
			// Fallback: hub not wired (should not happen in production).
			emailLower := strings.ToLower(strings.TrimSpace(email))
			md5Hash := fmt.Sprintf("%x", md5.Sum([]byte(emailLower)))
			svc.db.ExecContext(ctx, `
				INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, isp, created_at)
				VALUES (gen_random_uuid(), $1, $2, $3, 'unsubscribe', 'tracking_link', $4, NOW())
				ON CONFLICT (organization_id, md5_hash) DO UPDATE SET reason = 'unsubscribe', updated_at = NOW()
			`, orgID, emailLower, md5Hash, ispGroup)
		}

		log.Printf("TRACK UNSUBSCRIBE (global): campaign=%s subscriber=%s email=%s → global suppression", campaignID, subscriberID, logger.RedactEmail(email))
	}

	// Master List Migration P2 — shadow write to subscriber_domain_state.
	// Unsubscribes are domain-scoped in the new model. Brand-scoped
	// tokens (4-part) stamp the SDS row; legacy 3-part tokens fall back
	// to global AND stamp SDS for the campaign's sending domain so the
	// per-domain state also reflects intent.
	mailing.UpsertSDSUnsub(ctx, svc.db, subscriberID, mailing.ResolveSendingDomainForCampaign(ctx, svc.db, campaignID))

	// RFC 8058: ISP one-click POST expects a minimal 200 response, not HTML.
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	if brandRoot != "" {
		w.Write([]byte(fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Unsubscribed</title></head>
<body style="font-family:system-ui,Arial,sans-serif;text-align:center;padding:60px 20px;background:#f8f9fa;">
<div style="max-width:480px;margin:auto;background:#fff;border-radius:12px;padding:40px;box-shadow:0 2px 12px rgba(0,0,0,.08);">
<h1 style="color:#1a1a2e;font-size:24px;">You have been unsubscribed</h1>
<p style="color:#64748b;font-size:15px;">You will no longer receive emails from <strong>%s</strong>. This change is effective immediately. You may still receive emails from our other brands.</p>
</div></body></html>`, html.EscapeString(brandRoot))))
	} else {
		w.Write([]byte(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Unsubscribed</title></head>
<body style="font-family:system-ui,Arial,sans-serif;text-align:center;padding:60px 20px;background:#f8f9fa;">
<div style="max-width:480px;margin:auto;background:#fff;border-radius:12px;padding:40px;box-shadow:0 2px 12px rgba(0,0,0,.08);">
<h1 style="color:#1a1a2e;font-size:24px;">You have been unsubscribed</h1>
<p style="color:#64748b;font-size:15px;">You will no longer receive emails from us. This change is effective immediately.</p>
</div></body></html>`))
	}
}

// serveTrackingPixel returns a 1x1 transparent GIF
func (svc *MailingService) serveTrackingPixel(w http.ResponseWriter) {
	// 1x1 transparent GIF
	pixel := []byte{
		0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
		0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x2c,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02,
		0x02, 0x44, 0x01, 0x00, 0x3b,
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Write(pixel)
}

// updateEngagementScore recalculates subscriber engagement score
func (svc *MailingService) updateEngagementScore(ctx context.Context, subscriberID uuid.UUID) {
	// Get engagement metrics
	var totalOpens, totalClicks, totalEmails int
	var lastOpenAt, lastClickAt *time.Time
	
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_opens, 0), COALESCE(total_clicks, 0), COALESCE(total_emails_received, 1),
			   last_open_at, last_click_at
		FROM mailing_subscribers WHERE id = $1
	`, subscriberID).Scan(&totalOpens, &totalClicks, &totalEmails, &lastOpenAt, &lastClickAt)
	
	// Calculate engagement score (0-100)
	openRate := float64(totalOpens) / float64(totalEmails) * 100
	clickRate := float64(totalClicks) / float64(totalEmails) * 100
	
	// Base score from rates
	score := (openRate * 0.4) + (clickRate * 0.6)
	
	// Recency bonus
	if lastOpenAt != nil && time.Since(*lastOpenAt) < 7*24*time.Hour {
		score += 20
	} else if lastOpenAt != nil && time.Since(*lastOpenAt) < 30*24*time.Hour {
		score += 10
	}
	
	// Cap at 100
	if score > 100 {
		score = 100
	}
	
	svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET engagement_score = $2, updated_at = NOW() WHERE id = $1`, subscriberID, score)
}

// extractISP derives the ISP group name from an email address.
// Delegates to the canonical isp.Group classifier for consistency.
func extractISP(email string) string {
	g := isp.Group(email)
	if g == isp.Other || g == "" {
		return "unknown"
	}
	return g
}

// recomputeInboxProfileScore recalculates engagement_score (0–1 scale) for an
// inbox profile based on its own counters: open rate, click rate, and recency.
func (svc *MailingService) recomputeInboxProfileScore(ctx context.Context, eHash string) {
	var totalSends, totalOpens, totalClicks int
	var lastOpen *time.Time
	err := svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_sends,0), COALESCE(total_opens,0), COALESCE(total_clicks,0), last_open_at
		FROM mailing_inbox_profiles WHERE email_hash = $1
	`, eHash).Scan(&totalSends, &totalOpens, &totalClicks, &lastOpen)
	if err != nil {
		return
	}
	if totalSends == 0 {
		return
	}

	openRate := float64(totalOpens) / float64(totalSends)
	clickRate := float64(totalClicks) / float64(totalSends)
	score := (openRate * 0.6) + (clickRate * 0.4)

	if lastOpen != nil {
		daysSince := time.Since(*lastOpen).Hours() / 24
		if daysSince < 7 {
			score += 0.20
		} else if daysSince < 30 {
			score += 0.10
		}
	}

	if score > 1.0 {
		score = 1.0
	}

	svc.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles SET engagement_score = $2, updated_at = NOW()
		WHERE email_hash = $1
	`, eHash, score)
}

// updateISPAgent upserts ISP agent metrics when tracking events occur.
// The mailing_isp_agents table uses (organization_id, domain) as unique key.
func (svc *MailingService) updateISPAgent(ctx context.Context, campaignID uuid.UUID, isp, eventType string) {
	if isp == "" || isp == "unknown" {
		return
	}
	orgID := "00000000-0000-0000-0000-000000000001"
	domain := strings.ToLower(isp) + ".com"

	var openInc, clickInc int
	switch eventType {
	case "open":
		openInc = 1
	case "click":
		clickInc = 1
	}

	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_isp_agents (id, organization_id, isp, domain, total_opens, total_clicks, status, last_active_at, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'active', NOW(), NOW(), NOW())
		ON CONFLICT (organization_id, domain) DO UPDATE SET
			total_opens = mailing_isp_agents.total_opens + $4,
			total_clicks = mailing_isp_agents.total_clicks + $5,
			status = 'active',
			last_active_at = NOW(),
			updated_at = NOW()
	`, orgID, isp, domain, openInc, clickInc)
}

// ensureTrackingSchema runs idempotent DDL at startup to guarantee tracking columns exist.
// Runs with a timeout to avoid blocking server startup if locks are held.
func (svc *MailingService) ensureTrackingSchema() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stmts := []string{
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT`,
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS event_time TIMESTAMPTZ DEFAULT NOW()`,
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS organization_id UUID`,
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS device_type VARCHAR(20)`,
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS link_url TEXT`,
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}'`,
		`ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS is_unique BOOLEAN DEFAULT false`,
	}
	for _, s := range stmts {
		if _, err := svc.db.ExecContext(ctx, s); err != nil {
			log.Printf("[tracking] schema migration (non-fatal): %v", err)
		}
	}

	svc.db.ExecContext(ctx, `DO $$ DECLARE r RECORD; BEGIN
		FOR r IN SELECT con.conname FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			WHERE rel.relname = 'mailing_tracking_events' AND con.contype = 'c'
			AND pg_get_constraintdef(con.oid) ILIKE '%event_type%'
		LOOP EXECUTE 'ALTER TABLE mailing_tracking_events DROP CONSTRAINT IF EXISTS ' || quote_ident(r.conname);
		END LOOP;
	END $$`)

	svc.db.ExecContext(ctx, `DO $$ BEGIN ALTER TABLE mailing_tracking_events ALTER COLUMN organization_id DROP NOT NULL; EXCEPTION WHEN OTHERS THEN NULL; END $$`)

	svc.db.ExecContext(ctx, `ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS email TEXT`)
	svc.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_inbox_profiles_email_text ON mailing_inbox_profiles(email)`)

	log.Println("[tracking] schema reconciliation complete")
}

// detectDeviceType determines device type from User-Agent
func detectDeviceType(ua string) string {
	ua = strings.ToLower(ua)
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		return "mobile"
	}
	if strings.Contains(ua, "tablet") || strings.Contains(ua, "ipad") {
		return "tablet"
	}
	return "desktop"
}

// GetTrackingEvents returns real-time tracking events for a campaign
func (svc *MailingService) HandleGetTrackingEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	eventType := r.URL.Query().Get("type") // opened, clicked, bounced, complained, unsubscribed
	limitInt := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limitInt = v
	}

	query := `
		SELECT e.id, COALESCE(s.email, ''), e.event_type,
		       e.event_at, e.ip_address, e.user_agent, e.device_type, e.link_url
		FROM mailing_tracking_events e
		LEFT JOIN mailing_subscribers s ON e.subscriber_id = s.id
		WHERE e.campaign_id = $1
	`
	args := []interface{}{campaignID}

	if eventType != "" {
		query += " AND e.event_type = $2"
		args = append(args, eventType)
	}

	nextArg := len(args) + 1
	query += fmt.Sprintf(" ORDER BY e.event_at DESC LIMIT $%d", nextArg)
	args = append(args, limitInt)
	
	rows, err := svc.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("Error querying tracking events: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	
	var events []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var email, evtType string
		var deviceType *string
		var eventTime time.Time
		var ipAddress, userAgent, linkURL *string
		rows.Scan(&id, &email, &evtType, &eventTime, &ipAddress, &userAgent, &deviceType, &linkURL)
		events = append(events, map[string]interface{}{
			"id": id.String(), "email": email, "event_type": evtType,
			"event_time": eventTime, "ip_address": ipAddress,
			"device_type": deviceType, "link_url": linkURL,
		})
	}
	if events == nil { events = []map[string]interface{}{} }
	
	excludeMPP := r.URL.Query().Get("exclude_mpp") == "true"
	summary, _ := ComputeMetrics(ctx, svc.db, MetricsFilter{
		CampaignID: campaignID,
		ExcludeMPP: excludeMPP,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id": campaignID,
		"exclude_mpp": excludeMPP,
		"events":      events,
		"summary": map[string]interface{}{
			"sent":            summary.Sent,
			"delivered":       summary.Delivered,
			"opened":          summary.Opens,
			"clicked":         summary.Clicks,
			"hard_bounced":    summary.HardBounces,
			"soft_bounced":    summary.SoftBounces,
			"complained":      summary.Complaints,
			"unsubscribed":    summary.Unsubscribes,
			"open_rate":       summary.OpenRate,
			"click_rate":      summary.ClickRate,
			"hard_bounce_rate": summary.HardBounceRate,
			"soft_bounce_rate": summary.SoftBounceRate,
			"complaint_rate":  summary.ComplaintRate,
			"delivery_rate":   summary.DeliveryRate,
		},
	})
}

func calculateRate(count, total int) float64 {
	if total == 0 { return 0 }
	return float64(count) / float64(total) * 100
}

// incrOpenEngagement pushes open counts to Redis engagement telemetry.
func (svc *MailingService) incrOpenEngagement(ispName string, isMachineOpen bool) {
	if svc.redisClient == nil {
		return
	}
	engineISP := engine.NormalizeISPName(ispName)
	if engineISP == "" {
		return
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	key1h := engine.EngagementRedisKey1h(engineISP, now)
	pipe := svc.redisClient.Pipeline()
	pipe.HIncrBy(ctx, key1h, engine.EngFieldOpens, 1)
	if !isMachineOpen {
		pipe.HIncrBy(ctx, key1h, engine.EngFieldTrueOpens, 1)
	}
	pipe.Expire(ctx, key1h, engine.EngagementTTL1h)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("[engagement-redis] open incr error: %v", err)
	}
}

// incrClickEngagement pushes click counts to Redis engagement telemetry.
func (svc *MailingService) incrClickEngagement(ispName string, subscriberID string) {
	if svc.redisClient == nil {
		return
	}
	engineISP := engine.NormalizeISPName(ispName)
	if engineISP == "" {
		return
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	key1h := engine.EngagementRedisKey1h(engineISP, now)
	uniqKey := engine.EngagementUniqueKey(engineISP, now)

	pipe := svc.redisClient.Pipeline()
	pipe.HIncrBy(ctx, key1h, engine.EngFieldClicks, 1)
	pipe.Expire(ctx, key1h, engine.EngagementTTL1h)
	pipe.PFAdd(ctx, uniqKey, subscriberID)
	pipe.Expire(ctx, uniqKey, engine.EngagementTTL1h)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("[engagement-redis] click incr error: %v", err)
	}
}

func extractIPFromRemoteAddr(addr string) *string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return &host
	}
	if addr != "" {
		return &addr
	}
	return nil
}

// HandleInboundMailtoUnsubscribe processes forwarded inbound emails sent to
// unsub+{encoded}@{domain} addresses. It supports two input formats:
//
//  1. Direct JSON: { "recipient": "unsub+TOKEN@em.quizfiesta.com" }
//  2. AWS SNS envelope: SubscriptionConfirmation (auto-confirmed) and
//     Notification messages wrapping an SES inbound email notification.
//
// The encoded local-part contains the base64-encoded orgID|campaignID|subscriberID token.
func (svc *MailingService) HandleInboundMailtoUnsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var recipient string

	// Detect SNS envelope vs direct JSON by checking for "Type" field.
	var envelope struct {
		Type         string `json:"Type"`
		SubscribeURL string `json:"SubscribeURL"`
		Message      string `json:"Message"`
	}
	if err := json.Unmarshal(bodyBytes, &envelope); err == nil && envelope.Type != "" {
		switch envelope.Type {
		case "SubscriptionConfirmation":
			log.Printf("INBOUND UNSUB: SNS SubscriptionConfirmation — confirming via %s", envelope.SubscribeURL)
			if envelope.SubscribeURL != "" {
				resp, err := http.Get(envelope.SubscribeURL)
				if err != nil {
					log.Printf("INBOUND UNSUB: failed to confirm SNS subscription: %v", err)
				} else {
					resp.Body.Close()
					log.Printf("INBOUND UNSUB: SNS subscription confirmed (status %d)", resp.StatusCode)
				}
			}
			w.WriteHeader(http.StatusOK)
			return

		case "Notification":
			recipient = svc.extractRecipientFromSESNotification(envelope.Message)
			if recipient == "" {
				log.Printf("INBOUND UNSUB: could not extract recipient from SNS notification")
				w.WriteHeader(http.StatusOK)
				return
			}

		default:
			log.Printf("INBOUND UNSUB: unknown SNS Type=%s", envelope.Type)
			w.WriteHeader(http.StatusOK)
			return
		}
	} else {
		// Direct JSON payload: { "recipient": "unsub+TOKEN@domain" }
		var payload struct {
			Recipient string `json:"recipient"`
		}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		recipient = payload.Recipient
	}

	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		http.Error(w, "missing recipient", http.StatusBadRequest)
		return
	}

	// Extract the +encoded portion: unsub+TOKEN@domain
	// Only lowercase for the prefix check — the base64 token is case-sensitive.
	localPart := recipient
	if atIdx := strings.Index(recipient, "@"); atIdx >= 0 {
		localPart = recipient[:atIdx]
	}
	plusIdx := strings.Index(localPart, "+")
	if plusIdx < 0 || !strings.HasPrefix(strings.ToLower(localPart), "unsub+") {
		log.Printf("INBOUND UNSUB: not an unsub address: %s", logger.RedactEmail(recipient))
		w.WriteHeader(http.StatusOK)
		return
	}
	encoded := localPart[plusIdx+1:]

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		log.Printf("INBOUND UNSUB: base64 decode error for recipient=%s: %v", logger.RedactEmail(recipient), err)
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 3 {
		http.Error(w, "invalid token data", http.StatusBadRequest)
		return
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])

	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)

	if email == "" {
		log.Printf("INBOUND UNSUB: subscriber %s not found", subscriberID)
		w.WriteHeader(http.StatusOK)
		return
	}

	isp := extractISP(email)

	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "unsubscribe", email, isp)
	}

	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at)
		VALUES ($1, $2, $3, $4, 'unsubscribed', NOW())
	`, uuid.New(), orgID, campaignID, subscriberID)

	svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE id = $1`, subscriberID)
	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET unsubscribe_count = COALESCE(unsubscribe_count, 0) + 1 WHERE id = $1`, campaignID)

	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at, updated_at)
		VALUES ($1, $2, 'User unsubscribed (mailto)', 'mailto_unsubscribe', true, NOW(), NOW())
		ON CONFLICT (email) DO UPDATE SET active = true, reason = 'User unsubscribed (mailto)', updated_at = NOW()
	`, uuid.New(), email)

	if email != "" {
		emailLower := strings.ToLower(strings.TrimSpace(email))
		md5Hash := fmt.Sprintf("%x", md5.Sum([]byte(emailLower)))
		svc.db.ExecContext(ctx, `
			INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, isp, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 'unsubscribe', 'mailto_unsubscribe', $4, NOW())
			ON CONFLICT (organization_id, md5_hash) DO UPDATE SET reason = 'unsubscribe', updated_at = NOW()
		`, orgID, emailLower, md5Hash, isp)
	}

	// Master List Migration P2 — shadow write to subscriber_domain_state.
	// Mailto/inbound unsubs resolve the sending domain from the campaign
	// so the per-domain state reflects the user's intent for this brand.
	mailing.UpsertSDSUnsub(ctx, svc.db, subscriberID, mailing.ResolveSendingDomainForCampaign(ctx, svc.db, campaignID))

	log.Printf("INBOUND MAILTO UNSUB: campaign=%s subscriber=%s email=%s → global suppression", campaignID, subscriberID, logger.RedactEmail(email))
	w.WriteHeader(http.StatusOK)
}

// extractRecipientFromSESNotification parses an SES inbound email notification
// (JSON string from the SNS Message field) and returns the first recipient that
// looks like an unsub+ address.
func (svc *MailingService) extractRecipientFromSESNotification(messageJSON string) string {
	var sesNotification struct {
		NotificationType string `json:"notificationType"`
		Receipt          struct {
			Recipients []string `json:"recipients"`
		} `json:"receipt"`
		Mail struct {
			Source      string   `json:"source"`
			Destination []string `json:"destination"`
		} `json:"mail"`
	}
	if err := json.Unmarshal([]byte(messageJSON), &sesNotification); err != nil {
		log.Printf("INBOUND UNSUB: failed to parse SES notification JSON: %v", err)
		// Log first 500 chars for debugging
		snippet := messageJSON
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		log.Printf("INBOUND UNSUB: raw message snippet: %s", snippet)
		return ""
	}

	log.Printf("INBOUND UNSUB: SES notification type=%s receipt.recipients=%v mail.destination=%v mail.source=%s",
		sesNotification.NotificationType,
		sesNotification.Receipt.Recipients,
		sesNotification.Mail.Destination,
		sesNotification.Mail.Source)

	candidates := sesNotification.Receipt.Recipients
	if len(candidates) == 0 {
		candidates = sesNotification.Mail.Destination
	}
	for _, addr := range candidates {
		if strings.Contains(strings.ToLower(addr), "unsub+") {
			return addr
		}
	}

	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}
