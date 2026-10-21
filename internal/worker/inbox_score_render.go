package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// ── Inbox Score: render-only access to the real send path ───────────────────
//
// The Inbox Score feature needs the bytes a recipient would receive for a
// campaign BEFORE the wave is enqueued, so a spam-filter profile can score
// them. Everything below reuses the worker's own functions — the same
// column mapping as the claim SELECT (claimISPForOne), the same dispatch URL
// sanitizer, the same mutation, the same composeMessage and the same
// BuildRFC822 — and performs no writes. Where a pre-dispatch render cannot
// know a send-time value, the difference is reported in `deltas` rather than
// silently approximated.

// RenderWaveID is the fixed wave id used for pre-dispatch renders. Content
// mutation is seeded by (subscriber, wave); boards ship content_locked=true so
// it does not apply there, and when it does apply the delta is reported.
var RenderWaveID = uuid.MustParse("00000000-0000-0000-0000-00000000c0de")

// NewRenderPool returns a SendWorkerPool configured exactly like the live
// pool's tracking/compose settings but never started: no workers, no claims,
// no sends. It exists only to call ComposeForRender.
func NewRenderPool(db *sql.DB, trackingURL, trackingSecret, orgID string) *SendWorkerPool {
	p := NewSendWorkerPool(db, 1)
	p.SetTrackingConfig(trackingURL, trackingSecret, orgID)
	return p
}

// RenderItemResult is the QueueItem the worker would have claimed for
// (campaign, email), plus the documented differences from a real send.
type RenderItemResult struct {
	Item        QueueItem
	Deltas      []string
	ContentHash string // sha256 over (html, plain, content_locked) — the snapshot identity
	Locked      bool
}

// LoadRenderItem builds the QueueItem for a campaign and a recipient email
// without a queue row. Column choices mirror claimISPForOne's SELECT; the
// base HTML goes through the same sanitizeVariantURLsAtDispatch as
// enqueueWave, and the same mutation as renderSnapshotBody when the campaign
// is not content_locked.
func LoadRenderItem(ctx context.Context, db *sql.DB, orgID, campaignID, email string) (*RenderItemResult, error) {
	email = strings.TrimSpace(email)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("email is required")
	}
	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return nil, fmt.Errorf("campaign_id is not a UUID")
	}

	var it QueueItem
	var profileID, espType string
	var locked bool
	var campOrg string
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(camp.subject,''), COALESCE(camp.html_content,''), COALESCE(camp.plain_content,''),
		       COALESCE(camp.preview_text,''),
		       COALESCE(camp.from_name,''), COALESCE(camp.from_email,''), COALESCE(camp.reply_to,''),
		       COALESCE(camp.sending_profile_id::text,''), COALESCE(sp.vendor_type,'ses'),
		       COALESCE(camp.name,''), COALESCE(camp.content_locked, FALSE),
		       COALESCE(camp.offer_id, '00000000-0000-0000-0000-000000000000'),
		       COALESCE(camp.creative_id, '00000000-0000-0000-0000-000000000000'),
		       COALESCE(camp.subject_line_id, '00000000-0000-0000-0000-000000000000'),
		       COALESCE(camp.organization_id::text,'')
		FROM mailing_campaigns camp
		LEFT JOIN mailing_sending_profiles sp ON sp.id = camp.sending_profile_id
		WHERE camp.id = $1 AND camp.organization_id = $2`,
		campUUID, orgID,
	).Scan(&it.Subject, &it.HTMLContent, &it.TextContent, &it.PreviewText,
		&it.FromName, &it.FromEmail, &it.ReplyTo, &profileID, &espType,
		&it.CampaignName, &locked, &it.OfferID, &it.CreativeID, &it.SubjectLineID, &campOrg)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("campaign not found")
	}
	if err != nil {
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	it.CampaignID = campUUID
	it.ProfileID = profileID
	it.ESPType = espType
	it.Email = email
	it.BrandRoot = brand.RootFromEmail(it.FromEmail)
	it.WaveID = RenderWaveID

	res := &RenderItemResult{Locked: locked}

	// Subscriber on file? Same fields the claim SELECT reads; lookup via the
	// lower(TRIM(email)) index (the only email index on mailing_subscribers).
	var subID uuid.UUID
	var status, source string
	serr := db.QueryRowContext(ctx, `
		SELECT s.id, COALESCE(s.first_name,''), COALESCE(s.last_name,''), s.custom_fields,
		       COALESCE(s.engagement_score,0), COALESCE(s.total_emails_received,0),
		       COALESCE(s.total_opens,0), COALESCE(s.total_clicks,0),
		       s.last_open_at, s.last_click_at, s.last_email_at, s.optimal_send_hour_utc,
		       COALESCE(s.timezone,''), COALESCE(s.status,'confirmed'), COALESCE(s.source,''),
		       COALESCE(s.subscribed_at, s.created_at)
		FROM mailing_subscribers s
		WHERE s.organization_id = $1 AND lower(TRIM(s.email)) = lower(TRIM($2))
		ORDER BY s.created_at DESC LIMIT 1`,
		orgID, email,
	).Scan(&subID, &it.FirstName, &it.LastName, &it.CustomFields, &it.EngagementScore,
		&it.TotalEmailsReceived, &it.TotalOpens, &it.TotalClicks,
		&it.LastOpenAt, &it.LastClickAt, &it.LastEmailAt, &it.OptimalSendHourUTC,
		&it.Timezone, &status, &source, &it.SubscribedAt)
	switch {
	case serr == sql.ErrNoRows:
		subID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("inbox-score:"+strings.ToLower(email)))
		it.SubscriberStatus = "confirmed"
		res.Deltas = append(res.Deltas, "recipient is not a subscriber on file; rendered as a new recipient with no name or history")
	case serr != nil:
		return nil, fmt.Errorf("load subscriber: %w", serr)
	default:
		it.SubscriberStatus = status
		it.SubscriberSource = source
	}
	it.SubscriberID = subID
	// X-Message-ID on a real send is the queue row id, unknown before enqueue;
	// a deterministic stand-in keeps renders reproducible. Seeds are matched
	// on X-Campaign-ID, which IS identical on the real send.
	it.ID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("inbox-score:"+campaignID+":"+strings.ToLower(email)))
	res.Deltas = append(res.Deltas, "X-Message-ID / Message-ID / MIME boundary are per-send random values on a live send; pinned here")
	res.Deltas = append(res.Deltas, "from_name_id (per-recipient from-name rotation) is chosen at enqueue; render uses the campaign from_name")

	// Same base-content preparation as enqueueWave: dispatch URL sanitizer on
	// the campaign HTML, then the snapshot identity, then the per-recipient
	// mutation renderSnapshotBody would apply when not content_locked.
	brandKey := deriveBrandKey(it.FromEmail)
	sanitized := sanitizeVariantURLsAtDispatch([]campaignVariant{{HTMLContent: it.HTMLContent}}, brandKey)
	it.HTMLContent = sanitized[0].HTMLContent
	res.ContentHash = computeContentHash(it.HTMLContent, it.TextContent, locked)
	if !locked {
		seed := computeMutationSeed(it.SubscriberID, RenderWaveID.String())
		it.HTMLContent = mutateHTMLHash(it.HTMLContent, seed)
		it.Subject = mutateSubjectLine(it.Subject, seed, brandKey)
		res.Deltas = append(res.Deltas, "campaign is not content_locked: per-recipient HTML/subject mutation applied with the render wave seed (a live send seeds by its real wave id)")
	}
	res.Item = it
	return res, nil
}

// ComposeForRender runs the worker's composeMessage for a render item. A
// compose failure (unrenderable body) is returned as an error naming the
// field, mirroring the worker's quarantine reason.
func (p *SendWorkerPool) ComposeForRender(ctx context.Context, item QueueItem) (*EmailMessage, error) {
	msg, cf := p.composeMessage(ctx, item)
	if cf != nil {
		return nil, fmt.Errorf("unrenderable %s: %v", cf.field, cf.err)
	}
	return msg, nil
}

// RenderRFC822 pins Message-ID and boundary deterministically from the item
// identity so the same (campaign, email) renders to identical bytes twice.
func RenderRFC822(msg *EmailMessage) string {
	msgDomain := "mail.projectjarvis.io"
	if parts := strings.SplitN(msg.FromEmail, "@", 2); len(parts) == 2 && parts[1] != "" {
		msgDomain = parts[1]
	}
	messageID := fmt.Sprintf("%s@%s", msg.ID, msgDomain)
	boundary := fmt.Sprintf("=_%s", strings.ReplaceAll(msg.ID, "-", "")[:16])
	return BuildRFC822(msg, messageID, boundary)
}
