package worker

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"github.com/ignite/sparkpost-monitor/internal/pkg/moneylink"
	"github.com/lib/pq"
)

// SendWorkerPool manages a pool of workers for sending emails at scale
// Designed for 8.4M emails/day = 350K/hour = 5.8K/minute = ~100/second
// GlobalSuppressionChecker checks the global suppression hub (in-memory O(1)).
// IsSuppressedForBrand combines the global set AND the brand set for the
// given brand root under a SINGLE read lock — the send hot path pays one
// sync cost per candidate instead of two.
type GlobalSuppressionChecker interface {
	IsSuppressed(email string) bool
	IsSuppressedForBrand(email, brandRoot string) bool
}

// GlobalSuppressionSuppressor writes to the global suppression hub when
// bounces or complaints are detected during sending.
type GlobalSuppressionSuppressor interface {
	Suppress(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) (bool, error)
}

// OfferSuppressionChecker provides O(1) Bloom filter check for offer-level suppressions.
// Returns (suppressed, bloomAvailable). If bloomAvailable is false, fall back to DB.
type OfferSuppressionChecker interface {
	IsSuppressed(offerID, email string) (bool, bool)
}

type SendWorkerPool struct {
	db           *sql.DB
	workerID     string
	numWorkers   int
	batchSize    int
	pollInterval time.Duration

	// snapshotCache serves wave-shared base creatives for queue rows that
	// reference mailing_content_snapshots instead of carrying html_content
	// inline (LRU + singleflight; see content_snapshot.go).
	snapshotCache *SnapshotCache

	// Partner-dataset stamp resolution state (REQ-034). The per-claim-batch
	// subscriber→partner_clean_queue lookup stays DORMANT until the
	// idx_pcq_subscriber_id concurrent index is verified valid — without it
	// the ANY(ids) lookup would seq-scan the >1M-row partner_clean_queue on
	// every batch. Probed lazily, at most once per datasetResolveProbeTTL,
	// then latched true for the life of the pool. Guarded by datasetResolveMu.
	datasetResolveMu        sync.Mutex
	datasetResolveReady     bool
	datasetResolveNextProbe time.Time

	// Column-presence latch for the partner_dataset_id STAMP COLUMNS on
	// mailing_message_log / mailing_tracking_events. The send path must never
	// depend on DDL that cannot land: ADD COLUMN needs ACCESS EXCLUSIVE, and
	// under a live send (25 workers inserting continuously, tracking_events
	// partitioned 7 ways) that lock is unobtainable — verified 2026-07-13,
	// five bounded attempts all hit lock_timeout. The boot DDL loses the same
	// race, logs CRITICAL, and starts anyway; unconditional column references
	// would then fail EVERY send. So the write path probes for the columns and
	// falls back to the pre-REQ-034 SQL until they exist. Latches true.
	datasetColsMu        sync.Mutex
	datasetColsReady     bool
	datasetColsNextProbe time.Time

	// Stats
	totalSent       int64
	totalFailed     int64
	totalSkipped    int64
	totalSkippedCap int64 // subset of totalSkipped — rejected by cross-brand cap

	// Control
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
	mu      sync.RWMutex

	// ESP Senders (injected)
	sparkpostSender ESPSender
	sesSender       ESPSender
	mailgunSender   ESPSender
	sendgridSender  ESPSender
	pmtaSender      ESPSender

	// Global suppression hub (single source of truth)
	// suppressionWiringMu guards globalHub/globalSuppressor: they are wired
	// LATE, after Start, once SetMailingDB's goroutine has built the hub.
	suppressionWiringMu sync.RWMutex
	globalHub           GlobalSuppressionChecker
	globalSuppressor    GlobalSuppressionSuppressor

	// Offer-level suppression (Bloom filter O(1) with DB fallback)
	offerSuppChecker OfferSuppressionChecker

	// Tracking infrastructure
	trackingURL    string // Base URL for open/click/unsubscribe tracking
	trackingSecret string // HMAC signing key for tracking tokens
	orgID          string // Organization ID for tracking data

	profileTrackingDomainCache map[string]string // profileID -> resolved tracking base URL
	ptdMu                      sync.RWMutex

	// Per-profile brand image-host cache. Keyed by profileID; maps to the
	// sending brand's provisioned img.<apex> host, or "" when none is set up
	// (in which case creative images stay on the neutral platform CDN). One
	// DB lookup per profile, like the tracking domain. See resolveImageHost.
	profileImageHostCache map[string]string
	pihMu                 sync.RWMutex

	// SES tenant-aware routing cache. Keyed by profileID; populated lazily
	// on first send for a profile. profileSESInfo.viaSES gates internal
	// /track/click + /track/open injection (SES is the sole tracker on
	// these sends) and triggers X-SES-CONFIGURATION-SET / X-SES-TENANT
	// header injection so PMTA's relay to email-smtp.us-west-1.amazonaws.com
	// gets routed to the right tenant + config set.
	profileSESCache   map[string]profileSESInfo
	profileSESCacheAt map[string]time.Time
	psesMu            sync.RWMutex

	// Per-profile zero-width Subject-encoding cache (subject_zw_encode.go).
	// Keyed by profileID; populated lazily on first send for a profile.
	subjectZWCache map[string]subjectZWConfig
	szwMu          sync.RWMutex

	// ISP rate limiter (injected from engine.ISPRateRegistry via interface to avoid import cycle)
	rateRegistry         ISPRateLimiter
	perIPEnabled         bool
	rateLimitingDisabled bool

	// Channel-based work distribution: ISP dispatch loop pushes claimed
	// items here; worker goroutines consume and call processItem.
	workCh chan QueueItem

	// Cross-brand daily cap. Optional; nil disables the check.
	capChecker *mailing.CapChecker

	// outboxMode controls the durable injection outbox state machine.
	//   "" or "legacy" — current pending->sending->sent transitions, no
	//                    submitting state, no idempotency header.
	//   "durable"      — queued->submitting->accepted transitions,
	//                    X-Ignite-Idempotency-Key header injected, atomic
	//                    state guards on every write.
	// Default is legacy so pool construction without explicit opt-in is
	// behavior-preserving. Safe to flip at runtime between drains.
	outboxMode string

	// throughputTracker records every successful PMTA submission per
	// sending_domain over a rolling 60-second window. Drives the
	// /api/wave-processor/status JSON endpoint and the periodic
	// "[send_worker] throughput last_60s" log line. Pure observability —
	// reads never touch the send hot path. See SA-7 in
	// PER_DOMAIN_ENGAGEMENT_ENGINE_SPEC.md.
	throughputTracker *DomainThroughputTracker
}

// SetOutboxMode toggles the durable-outbox state machine. Pass "durable" to
// activate the full state machine, "" or "legacy" (or any other value) to
// preserve current behavior. Intended to be called once at startup from
// config, but safe to call while the pool is draining — the mode is read
// fresh on every claim and every transition.
func (p *SendWorkerPool) SetOutboxMode(mode string) {
	if mode == "durable" {
		p.outboxMode = "durable"
		return
	}
	p.outboxMode = "legacy"
}

// isDurable returns true when the durable outbox state machine should be
// active for every claim and transition. Kept as a method (not a bare field
// read) so future refactors can add per-campaign or per-brand overrides
// without touching every call site.
func (p *SendWorkerPool) isDurable() bool {
	return p.outboxMode == "durable"
}

// SetCapChecker wires the cross-brand daily cap enforcer. When set, the
// dispatch loop filters every claimed batch through Reserve() before
// pushing items onto the work channel. Subscribers over cap are marked
// 'skipped' with reason 'cross_brand_cap_exceeded' on the queue row,
// preventing them from being re-claimed today.
func (p *SendWorkerPool) SetCapChecker(c *mailing.CapChecker) {
	p.capChecker = c
}

// ISPRateLimiter abstracts the engine.ISPRateRegistry to avoid import cycles.
type ISPRateLimiter interface {
	AllowN(isp string, n int) int
	DistributeByIPStr(isp string, requested int) map[string]int
	HasIPListStr(isp string) bool
}

// SetRateRegistry injects the ISP rate limiter. The dispatch loop
// checks AllowN before claiming batches to enforce per-ISP rate limits.
func (p *SendWorkerPool) SetRateRegistry(r ISPRateLimiter) {
	p.rateRegistry = r
}

// SetPerIPRateLimiting enables or disables per-IP rate distribution in the
// dispatch loop. When enabled, DistributeByIP is used instead of AllowN.
func (p *SendWorkerPool) SetPerIPRateLimiting(enabled bool) {
	p.perIPEnabled = enabled
}

// SetRateLimitingDisabled bypasses the ISP rate registry gating in the dispatch
// loop. When true, batch counts flow directly to claimISPBatch without token
// bucket checks — PMTA handles per-domain throttling natively.
func (p *SendWorkerPool) SetRateLimitingDisabled(disabled bool) {
	p.rateLimitingDisabled = disabled
}

// ESPSender interface for sending via different ESPs
type ESPSender interface {
	Send(ctx context.Context, msg *EmailMessage) (*SendResult, error)
}

// EmailMessage represents an email to be sent
type EmailMessage struct {
	ID           string
	CampaignID   string
	SubscriberID string
	Email        string
	FromName     string
	FromEmail    string
	ReplyTo      string
	Subject      string
	HTMLContent  string
	TextContent  string
	PreviewText  string // Pre-header text (injected as hidden span before <body> content)
	ProfileID    string
	ESPType      string
	RecipientISP string // ISP classification for VMTA pool routing (e.g. "gmail", "yahoo")
	AssignedVMTA string // Pre-assigned VMTA from dispatch-level IP distribution (empty = use ipPool.next)
	Metadata     map[string]interface{}
	Headers      map[string]string // Custom SMTP headers (List-Unsubscribe, X-Job, etc.)
}

// InjectPreviewText prepends a hidden preheader span into the HTML body.
// This is the industry-standard technique for email preview text:
// a visually hidden span at the top of <body>, followed by whitespace padding
// to push any other preview content out of the preview pane.
func InjectPreviewText(html, previewText string) string {
	if previewText == "" || html == "" {
		return html
	}

	preheaderHTML := fmt.Sprintf(
		`<div style="display:none;font-size:1px;color:#ffffff;line-height:1px;max-height:0px;max-width:0px;opacity:0;overflow:hidden;">%s</div>`,
		previewText,
	)

	// Insert after <body> tag if present
	bodyIdx := strings.Index(strings.ToLower(html), "<body")
	if bodyIdx >= 0 {
		// Find the closing > of the <body> tag
		closeIdx := strings.Index(html[bodyIdx:], ">")
		if closeIdx >= 0 {
			insertAt := bodyIdx + closeIdx + 1
			return html[:insertAt] + preheaderHTML + html[insertAt:]
		}
	}

	// No <body> tag — prepend to the HTML
	return preheaderHTML + html
}

// SendResult contains the result of a send attempt
type SendResult struct {
	Success   bool
	MessageID string
	Error     error
	ESPType   string
	SentAt    time.Time
	VMTA      string // PMTA virtual MTA used (hostname or short name)
}

// QueueItem represents an item from the send queue
// buildFeedbackID composes the Gmail FBL header per Google's spec
// (Feedback-ID: a:b:c:SenderId) so Postmaster Tools can actually aggregate:
//
//	a = campaign id     — aggregates per campaign
//	b = brand token     — the from-domain apex label, aggregates per brand
//	c = stream type     — bcast (m.<apex>) vs drip (em.<apex>) vs other
//	SenderId = jvmail1  — mandatory, 5–15 chars, CONSTANT across the estate
//
// The previous format put subscriberID and the queue-row id in b/c — unique
// per message, which Google explicitly warns never aggregates (each identifier
// appears once and misses the volume threshold), so 2 of 3 identifiers were
// dead and the FBL dashboard could attribute nothing. Recipient-level
// reconciliation lives in X-Message-ID and the SES message tags, never here.
func buildFeedbackID(campaignID, fromEmail string) string {
	domain := fromEmail
	if atIdx := strings.LastIndex(fromEmail, "@"); atIdx >= 0 {
		domain = fromEmail[atIdx+1:]
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	stream := "other"
	switch {
	case strings.HasPrefix(domain, "m."):
		stream = "bcast"
	case strings.HasPrefix(domain, "em."):
		stream = "drip"
	}
	apex := strings.TrimPrefix(strings.TrimPrefix(domain, "em."), "m.")
	brand := apex
	if i := strings.IndexByte(apex, '.'); i > 0 {
		brand = apex[:i]
	}
	if brand == "" {
		brand = "unknown"
	}
	return fmt.Sprintf("%s:%s:%s:jvmail1", campaignID, brand, stream)
}

type QueueItem struct {
	ID           uuid.UUID
	CampaignID   uuid.UUID
	SubscriberID uuid.UUID
	Email        string
	Subject      string
	HTMLContent  string
	TextContent  string
	PreviewText  string
	FromName     string
	FromEmail    string
	ReplyTo      string
	ProfileID    string
	ESPType      string
	Priority     int
	FirstName    string
	LastName     string

	// IdempotencyKey is the deterministic UUIDv5 stamped at wave-enqueue
	// time (see worker/outbox_idempotency.go). It is zero-valued for rows
	// inserted by legacy paths that predate the durable outbox.
	// The send worker rides this key through PMTA as the
	// X-Ignite-Idempotency-Key header so accounting records can be
	// correlated back to the originating queue row without relying on the
	// opaque PMTA message-id.
	IdempotencyKey uuid.UUID

	// Extended subscriber fields for full Liquid personalization
	CustomFields        mailing.JSON
	EngagementScore     float64
	TotalEmailsReceived int
	TotalOpens          int
	TotalClicks         int
	LastOpenAt          *time.Time
	LastClickAt         *time.Time
	LastEmailAt         *time.Time
	OptimalSendHourUTC  *int
	Timezone            string
	SubscriberStatus    string
	SubscriberSource    string
	SubscribedAt        time.Time

	// Campaign metadata for template context
	CampaignName string

	// Offer lifecycle attribution
	OfferID       uuid.UUID
	CreativeID    uuid.UUID
	SubjectLineID uuid.UUID
	FromNameID    uuid.UUID

	// Pre-assigned VMTA from dispatch-level IP distribution (empty = dynamic selection)
	AssignedVMTA string

	// BrandRoot is derived from FromEmail once at claim time so the send
	// hot path (millions of calls per campaign) never recomputes it.
	// Populated in scanQueueRows via api.BrandRoot.
	BrandRoot string

	// ContentSnapshotID dereferences the wave's shared base creative in
	// mailing_content_snapshots (set-based enqueue path). Nil for legacy
	// rows that carry html_content inline — hydrateSnapshots dual-reads.
	ContentSnapshotID uuid.UUID
	// WaveID seeds the deterministic per-recipient fingerprint mutation
	// (computeMutationSeed) when rendering from a snapshot.
	WaveID uuid.UUID

	// PartnerDatasetID is the partner dataset this message is attributed to
	// (REQ-034). Seeded from the campaign's deploy-time stamp in the claim
	// SELECT, then refined per-message by resolveDatasetStamps (subscriber →
	// partner_clean_queue membership). Nil = not partner-attributed; markSent
	// writes NULL to mailing_message_log / the 'sent' tracking event.
	PartnerDatasetID uuid.UUID
}

// NewSendWorkerPool creates a new worker pool
func NewSendWorkerPool(db *sql.DB, numWorkers int) *SendWorkerPool {
	if numWorkers <= 0 {
		numWorkers = 50 // Default for ~100 emails/second with batching
	}

	return &SendWorkerPool{
		db:                db,
		workerID:          fmt.Sprintf("worker-%s", uuid.New().String()[:8]),
		numWorkers:        numWorkers,
		batchSize:         100,                    // Claim 100 items per batch
		pollInterval:      100 * time.Millisecond, // Poll frequently for low latency
		throughputTracker: NewDomainThroughputTracker(60 * time.Second),
		snapshotCache:     NewSnapshotCache(defaultSnapshotCacheSize),
	}
}

// Throughput returns the current per-sending-domain send rate over the last
// 60 seconds. Used by the /api/wave-processor/status handler. Safe to call
// at any time; the underlying tracker handles its own concurrency.
func (p *SendWorkerPool) Throughput() map[string]int {
	if p.throughputTracker == nil {
		return map[string]int{}
	}
	return p.throughputTracker.Snapshot()
}

// SetOfferSuppressionChecker connects the Bloom-based offer suppression checker.
func (p *SendWorkerPool) SetOfferSuppressionChecker(checker OfferSuppressionChecker) {
	p.offerSuppChecker = checker
}

// SetGlobalSuppressionHub connects the worker pool to the global
// suppression single source of truth for pre-send checking.
// Safe to call before OR after Start. The hub is built inside SetMailingDB,
// which main.go runs in a goroutine — so on a normal boot this wiring arrives
// LATE, after the pool's workers are already claiming. Guarded accordingly
// (2026-08-21: the boot-time assertion read a nil hub and both suppression
// wirings were silently skipped for the life of the process).
func (p *SendWorkerPool) SetGlobalSuppressionHub(hub GlobalSuppressionChecker) {
	p.suppressionWiringMu.Lock()
	p.globalHub = hub
	p.suppressionWiringMu.Unlock()
}

func (p *SendWorkerPool) getGlobalSuppressionHub() GlobalSuppressionChecker {
	p.suppressionWiringMu.RLock()
	defer p.suppressionWiringMu.RUnlock()
	return p.globalHub
}

// SetGlobalSuppressionWriter connects the worker pool to the global
// suppression hub for writing bounces/complaints during send failures.
// Safe to call before OR after Start — see SetGlobalSuppressionHub.
func (p *SendWorkerPool) SetGlobalSuppressionWriter(w GlobalSuppressionSuppressor) {
	p.suppressionWiringMu.Lock()
	p.globalSuppressor = w
	p.suppressionWiringMu.Unlock()
}

func (p *SendWorkerPool) getGlobalSuppressionWriter() GlobalSuppressionSuppressor {
	p.suppressionWiringMu.RLock()
	defer p.suppressionWiringMu.RUnlock()
	return p.globalSuppressor
}

// SetESPSenders sets the ESP sender implementations
func (p *SendWorkerPool) SetESPSenders(sparkpost, ses, mailgun, sendgrid ESPSender) {
	p.sparkpostSender = sparkpost
	p.sesSender = ses
	p.mailgunSender = mailgun
	p.sendgridSender = sendgrid
}

func (p *SendWorkerPool) SetPMTASender(sender ESPSender) {
	p.pmtaSender = sender
}

// SetTrackingConfig configures tracking pixel/click/unsubscribe injection.
func (p *SendWorkerPool) SetTrackingConfig(trackingURL, trackingSecret, orgID string) {
	p.trackingURL = trackingURL
	p.trackingSecret = trackingSecret
	p.orgID = orgID
	p.profileTrackingDomainCache = make(map[string]string)
	p.profileImageHostCache = make(map[string]string)
	p.profileSESCache = make(map[string]profileSESInfo)
	p.profileSESCacheAt = make(map[string]time.Time)
	p.subjectZWCache = make(map[string]subjectZWConfig)
}

// profileSESInfo holds the per-profile SES tenant-aware routing facts.
type profileSESInfo struct {
	ViaSES     bool
	ConfigSet  string
	TenantName string
	// RawCreative (operator 2026-07-25, wcl-heloc): send the creative AS-IS —
	// strip the stored unsub-disclaimer block and skip the CAN-SPAM fallback
	// injection. List-Unsubscribe HEADERS still ship (RFC 8058 mandate); only
	// the visible body footer is bypassed. The creative owns its compliance.
	RawCreative bool
}

// unsubDisclaimerMarkerW mirrors internal/api html_template_import.go's
// unsubDisclaimerMarker — the injector stamps this comment before its two
// <p> blocks, which makes the block deterministically strippable here.
const unsubDisclaimerMarkerW = `<!-- unsub-disclaimer -->`

// stripUnsubDisclaimerBlock removes the stored compliance-footer block (marker
// + its two <p> paragraphs) from a creative. Used only for raw_creative
// profiles; a no-op when the marker is absent.
func stripUnsubDisclaimerBlock(html string) string {
	idx := strings.Index(html, unsubDisclaimerMarkerW)
	if idx < 0 {
		return html
	}
	rest := html[idx+len(unsubDisclaimerMarkerW):]
	end := 0
	for i := 0; i < 2; i++ {
		p := strings.Index(strings.ToLower(rest[end:]), "</p>")
		if p < 0 {
			break
		}
		end += p + len("</p>")
	}
	return html[:idx] + rest[end:]
}

// sanitizeSESTagValue coerces a value into the SES MessageTag charset
// [A-Za-z0-9_-] (max 256 chars). Anything else (dots, @, etc.) becomes "_".
func sanitizeSESTagValue(v string) string {
	if v == "" {
		return "none"
	}
	var b strings.Builder
	for i, r := range v {
		if i >= 256 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// buildSESMessageTags formats the X-SES-MESSAGE-TAGS header value
// ("k1=v1, k2=v2"). recipientSendID is the outbox queue row id — the universal
// join key that SES events, PMTA accounting, and tracking all reconcile to.
func buildSESMessageTags(recipientSendID, campaignID, subscriberID, ispGroup string) string {
	pairs := []string{
		"recipient_send_id=" + sanitizeSESTagValue(recipientSendID),
		"campaign_id=" + sanitizeSESTagValue(campaignID),
		"subscriber_id=" + sanitizeSESTagValue(subscriberID),
		"isp_group=" + sanitizeSESTagValue(ispGroup),
		"route_type=ses_tenant",
	}
	return strings.Join(pairs, ", ")
}

// profileSESCacheTTL bounds how long a profile's via_ses/config-set answer is
// reused. A DB flip of via_ses now takes effect within this window with NO
// process restart — the /emergency-swap requirement (2026-07-30). Var, not
// const, so tests can shrink it.
var profileSESCacheTTL = 60 * time.Second

// resolveProfileSES returns the via_ses / ses_configuration_set / ses_tenant_name
// values for a sending profile. The result is cached for profileSESCacheTTL
// (formerly process lifetime). A nil/empty profile id, a missing row, or any DB
// error returns the zero value (viaSES=false) so the default Dedicated path is
// preserved.
func (p *SendWorkerPool) resolveProfileSES(ctx context.Context, profileID string) profileSESInfo {
	if profileID == "" {
		return profileSESInfo{}
	}
	p.psesMu.RLock()
	if cached, ok := p.profileSESCache[profileID]; ok {
		if at, ok2 := p.profileSESCacheAt[profileID]; ok2 && time.Since(at) < profileSESCacheTTL {
			p.psesMu.RUnlock()
			return cached
		}
	}
	p.psesMu.RUnlock()

	var via, raw sql.NullBool
	var configSet, tenant sql.NullString
	err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(via_ses, FALSE), COALESCE(ses_configuration_set, ''), COALESCE(ses_tenant_name, ''),
		        COALESCE(raw_creative, FALSE)
		   FROM mailing_sending_profiles WHERE id = $1`,
		profileID).Scan(&via, &configSet, &tenant, &raw)
	if err != nil {
		// Pre-migration deployments will hit `column "via_ses" does not
		// exist` until runStartupMigrations has run. Treat that as the
		// default (Dedicated) path instead of failing the send.
		if !strings.Contains(err.Error(), "via_ses") &&
			!strings.Contains(err.Error(), "ses_configuration_set") &&
			!strings.Contains(err.Error(), "ses_tenant_name") &&
			!strings.Contains(err.Error(), "raw_creative") &&
			err != sql.ErrNoRows {
			log.Printf("resolveProfileSES: profile=%s db error: %v", profileID, err)
		}
		empty := profileSESInfo{}
		p.psesMu.Lock()
		if p.profileSESCacheAt == nil {
			p.profileSESCacheAt = make(map[string]time.Time)
		}
		p.profileSESCache[profileID] = empty
		p.profileSESCacheAt[profileID] = time.Now()
		p.psesMu.Unlock()
		return empty
	}
	info := profileSESInfo{
		ViaSES:      via.Bool,
		ConfigSet:   configSet.String,
		TenantName:  tenant.String,
		RawCreative: raw.Bool,
	}
	p.psesMu.Lock()
	if p.profileSESCacheAt == nil {
		p.profileSESCacheAt = make(map[string]time.Time)
	}
	p.profileSESCache[profileID] = info
	p.profileSESCacheAt[profileID] = time.Now()
	p.psesMu.Unlock()
	if info.ViaSES {
		log.Printf("resolveProfileSES: profile=%s via_ses=true config_set=%s tenant=%s",
			profileID, info.ConfigSet, info.TenantName)
	}
	return info
}

// resolveTrackingURL returns the per-profile tracking base URL if one is
// configured, falling back to the global trackingURL.
func (p *SendWorkerPool) resolveTrackingURL(ctx context.Context, profileID string) string {
	if profileID == "" {
		return p.trackingURL
	}
	p.ptdMu.RLock()
	if cached, ok := p.profileTrackingDomainCache[profileID]; ok {
		p.ptdMu.RUnlock()
		return cached
	}
	p.ptdMu.RUnlock()

	var td, sd sql.NullString
	err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(tracking_domain,''), COALESCE(sending_domain,'') FROM mailing_sending_profiles WHERE id = $1`,
		profileID).Scan(&td, &sd)
	if err != nil {
		log.Printf("resolveTrackingURL: error loading profile %s: %v", profileID, err)
		return p.trackingURL
	}

	resolved := p.trackingURL
	source := "global"
	if td.Valid && td.String != "" {
		resolved = ensureHTTPSWorker(td.String)
		source = "profile_tracking_domain"
	} else if sd.Valid && sd.String != "" {
		resolved = ensureHTTPSWorker("trk." + sd.String)
		source = "derived_from_sending_domain"
	}
	log.Printf("resolveTrackingURL: profile=%s source=%s url=%s", profileID, source, resolved)

	p.ptdMu.Lock()
	p.profileTrackingDomainCache[profileID] = resolved
	p.ptdMu.Unlock()
	return resolved
}

// neutralImageHost is the platform CDN host that rehosted creative images
// carry at create time (RehostHTML in internal/api/image_cdn_handlers.go). The
// send path rewrites it to the sending brand's img.<apex> host when that domain
// is provisioned (lookupBrandImageHost). Brand-matching moved out of the rehost
// step in commit 5a160f4 but was only wired into the proof-send path — this
// restores it for real campaign + click-drip sends.
const neutralImageHost = "https://img.projectjarvis.io"

// brandImageHostSwapDisabled is the kill switch: set
// DISABLE_BRAND_IMAGE_HOST_SWAP=1 to ship the neutral platform CDN host on
// every send without a redeploy (instant revert to pre-fix behavior).
func brandImageHostSwapDisabled() bool {
	return os.Getenv("DISABLE_BRAND_IMAGE_HOST_SWAP") == "1"
}

// apexFromSendingDomain reduces a sending domain to its apex brand by stripping
// the known ESP subdomain label: em.ratesbazar.com / m.ratesbazar.com ->
// ratesbazar.com. Only known prefixes are stripped, so a bare apex is returned
// unchanged and an unexpected host fails closed (its img.<host> simply won't be
// provisioned -> no swap -> neutral). Prefixes are ordered longest-first so
// "em." wins over "m." on em.<brand>.
func apexFromSendingDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	for _, pfx := range []string{"mail.", "send.", "trk.", "em.", "m."} {
		if strings.HasPrefix(d, pfx) {
			return strings.TrimPrefix(d, pfx)
		}
	}
	return d
}

// lookupBrandImageHost returns the sending brand's provisioned image host
// (img.<apex>) for a profile, or "" when none is set up + verified — in which
// case creative images stay on the neutral platform CDN (never a broken or
// wrong-brand host). The S3 object/path is identical across brands (every
// img.<brand> CloudFront alias fronts the same bucket), so the send-time swap
// is a pure, brand-correct host rewrite. Scoped to the profile's org so a
// future multi-tenant split can't cross brands. ssl_status 'issued' is accepted
// alongside 'active' — issued brands serve identically (same alias); requiring
// 'active' would leave provisioned brands stuck on the neutral host. Shared by
// the campaign send worker (cached) and the click-drip sender (direct).
// NB: mailing_sending_profiles uses organization_id; mailing_image_domains uses
// org_id — different column names, intentionally.
func lookupBrandImageHost(ctx context.Context, db *sql.DB, profileID string) string {
	if profileID == "" {
		return ""
	}
	var sendingDomain, orgID sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sending_domain,''), organization_id::text FROM mailing_sending_profiles WHERE id = $1`,
		profileID).Scan(&sendingDomain, &orgID); err != nil {
		log.Printf("lookupBrandImageHost: error loading profile %s: %v", profileID, err)
		return "" // fail safe: no swap, stay on the (working) neutral host
	}
	apex := apexFromSendingDomain(sendingDomain.String)
	if apex == "" {
		return ""
	}
	candidate := "img." + apex
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT domain FROM mailing_image_domains
		 WHERE org_id::text = $1 AND lower(domain) = lower($2) AND verified = true
		   AND ssl_status IN ('active', 'issued')
		 LIMIT 1`, orgID.String, candidate).Scan(&got)
	switch {
	case err == nil:
		return got
	case err == sql.ErrNoRows:
		return "" // not provisioned for this brand — stay neutral
	default:
		log.Printf("lookupBrandImageHost: lookup error for %s: %v", candidate, err)
		return ""
	}
}

// resolveImageHost is the per-profile cached wrapper over lookupBrandImageHost
// (one DB lookup per profile, like resolveTrackingURL). Empty results are cached
// too so an unprovisioned brand doesn't re-query on every message.
func (p *SendWorkerPool) resolveImageHost(ctx context.Context, profileID string) string {
	if profileID == "" {
		return ""
	}
	p.pihMu.RLock()
	if cached, ok := p.profileImageHostCache[profileID]; ok {
		p.pihMu.RUnlock()
		return cached
	}
	p.pihMu.RUnlock()

	host := lookupBrandImageHost(ctx, p.db, profileID)

	if p.profileImageHostCache != nil {
		p.pihMu.Lock()
		p.profileImageHostCache[profileID] = host
		p.pihMu.Unlock()
	}
	return host
}

func ensureHTTPSWorker(domainOrURL string) string {
	d := strings.TrimSpace(domainOrURL)
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "http") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
}

// Start begins the worker pool
func (p *SendWorkerPool) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.workCh = make(chan QueueItem, p.batchSize*2)
	p.mu.Unlock()

	log.Printf("SendWorkerPool: Starting %d channel workers (batch_size=%d, channel_buf=%d)", p.numWorkers, p.batchSize, p.batchSize*2)

	p.registerWorker()

	go p.heartbeatLoop()
	go p.throughputLogLoop()

	// ISP dispatch coordinator: claims items from DB and feeds workCh
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.ispDispatchLoop()
	}()

	// Channel consumer workers: read from workCh and call processItem
	for i := 0; i < p.numWorkers; i++ {
		p.wg.Add(1)
		go p.channelWorker(i)
	}
}

// Stop gracefully stops the worker pool
func (p *SendWorkerPool) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.cancel()
	p.mu.Unlock()

	log.Println("SendWorkerPool: Stopping workers...")
	p.wg.Wait()

	// Deregister worker
	p.deregisterWorker()

	log.Printf("SendWorkerPool: Stopped. Total sent: %d, failed: %d, skipped: %d",
		atomic.LoadInt64(&p.totalSent), atomic.LoadInt64(&p.totalFailed), atomic.LoadInt64(&p.totalSkipped))
}

// Stats returns current statistics
func (p *SendWorkerPool) Stats() map[string]int64 {
	return map[string]int64{
		"total_sent":    atomic.LoadInt64(&p.totalSent),
		"total_failed":  atomic.LoadInt64(&p.totalFailed),
		"total_skipped": atomic.LoadInt64(&p.totalSkipped),
	}
}

// channelWorker consumes QueueItems pushed by the ISP dispatch loop
// and processes them. This replaces the old flat-claim worker() which
// independently queried the DB.
func (p *SendWorkerPool) channelWorker(workerNum int) {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return
		case item, ok := <-p.workCh:
			if !ok {
				return
			}
			if err := p.processItem(item); err != nil {
				log.Printf("Worker %d: Error processing item %s: %v", workerNum, item.ID, err)
			}
		}
	}
}

// claimISPForOne claims up to `count` queued items for a specific campaign+ISP.
//
// In legacy mode (OutboxMode != "durable") the target status is 'sending',
// matching historical behavior exactly. In durable mode the target is
// 'submitting' — skipping the intermediate 'sending' state entirely so the
// state machine has exactly one row-UPDATE per claim (no perf regression vs
// legacy) and the reconciler's stuck-submitting query has a clean signal.
//
// The idempotency_key column is returned even when NULL (for legacy rows
// that predate the outbox migration) so scanQueueRows can safely scan every
// queue row regardless of origin path.
func (p *SendWorkerPool) claimISPForOne(ctx context.Context, campaignID, isp string, count int) ([]QueueItem, error) {
	targetStatus := "sending"
	eligibleStatuses := "q.status = 'queued'"
	if p.isDurable() {
		targetStatus = "submitting"
		// Durable mode also picks rows whose retry backoff has elapsed.
		// The next_attempt_at predicate ensures we only re-pick
		// failed_retryable rows after their exponential-backoff window
		// has passed; legacy 'queued' rows still get picked unconditionally
		// so first-time-send latency is unaffected.
		eligibleStatuses = `(
			q.status = 'queued'
			OR (q.status = 'failed_retryable' AND (q.next_attempt_at IS NULL OR q.next_attempt_at <= NOW()))
		)`
	}
	rows, err := p.db.QueryContext(ctx, `
		WITH claimed AS (
			UPDATE mailing_campaign_queue
			SET status = $5, worker_id = $1, locked_at = NOW()
			WHERE id IN (
				SELECT q.id FROM mailing_campaign_queue q
				JOIN mailing_campaigns camp ON camp.id = q.campaign_id
				WHERE q.campaign_id = $2
				  AND q.recipient_isp = $3
				  AND `+eligibleStatuses+`
				  AND camp.status = 'sending'
				  AND q.scheduled_at <= NOW()
				  AND (q.locked_at IS NULL OR q.locked_at < NOW() - INTERVAL '5 minutes')
				ORDER BY q.priority DESC, q.scheduled_at ASC
				LIMIT $4
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, campaign_id, subscriber_id, subject, html_content, plain_content, offer_id, creative_id, subject_line_id, from_name_id, idempotency_key, content_snapshot_id, wave_id
		)
		SELECT
			c.id, c.campaign_id, c.subscriber_id,
			s.email,
			COALESCE(c.subject, ''), COALESCE(c.html_content, ''), COALESCE(c.plain_content, ''),
			COALESCE(camp.preview_text, ''),
			COALESCE(camp.from_name, ''), COALESCE(camp.from_email, ''), COALESCE(camp.reply_to, ''),
			COALESCE(camp.sending_profile_id::text, ''), COALESCE(sp.vendor_type, 'ses'),
			COALESCE(s.first_name, ''), COALESCE(s.last_name, ''),
			s.custom_fields, COALESCE(s.engagement_score, 0),
			COALESCE(s.total_emails_received, 0), COALESCE(s.total_opens, 0), COALESCE(s.total_clicks, 0),
			s.last_open_at, s.last_click_at, s.last_email_at,
			s.optimal_send_hour_utc, COALESCE(s.timezone, ''),
			COALESCE(s.status, 'confirmed'), COALESCE(s.source, ''),
			COALESCE(s.subscribed_at, s.created_at), COALESCE(camp.name, ''),
			COALESCE(c.offer_id, '00000000-0000-0000-0000-000000000000'),
			COALESCE(c.creative_id, '00000000-0000-0000-0000-000000000000'),
			COALESCE(c.subject_line_id, '00000000-0000-0000-0000-000000000000'),
			COALESCE(c.from_name_id, '00000000-0000-0000-0000-000000000000'),
			COALESCE(c.idempotency_key, '00000000-0000-0000-0000-000000000000'),
			COALESCE(c.content_snapshot_id, '00000000-0000-0000-0000-000000000000'),
			COALESCE(c.wave_id, '00000000-0000-0000-0000-000000000000'),
			COALESCE(camp.partner_dataset_id, '00000000-0000-0000-0000-000000000000')
		FROM claimed c
		JOIN mailing_subscribers s ON s.id = c.subscriber_id
		JOIN mailing_campaigns camp ON camp.id = c.campaign_id
		LEFT JOIN mailing_sending_profiles sp ON sp.id = camp.sending_profile_id
	`, p.workerID, campaignID, isp, count, targetStatus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := p.scanQueueRows(rows)
	if err != nil {
		return nil, err
	}
	return p.resolveDatasetStamps(ctx, p.hydrateSnapshots(ctx, items)), nil
}

// hydrateSnapshots resolves queue rows that reference a content snapshot
// (set-based enqueue path) into ready-to-send bodies: load the wave's shared
// base creative through the LRU/singleflight cache, then apply the
// deterministic per-recipient fingerprint mutation + honeypot. Rows that
// already carry html_content inline (legacy enqueue, pre-cutover backlog)
// pass through untouched — this is the dual-read half of the migration.
//
// A row whose snapshot cannot be loaded is marked failed (retryable per
// markFailed's normal policy) and dropped from the batch so an empty body can
// never reach an ISP.
func (p *SendWorkerPool) hydrateSnapshots(ctx context.Context, items []QueueItem) []QueueItem {
	out := items[:0]
	for i := range items {
		item := items[i]
		if item.ContentSnapshotID == uuid.Nil || strings.TrimSpace(item.HTMLContent) != "" {
			out = append(out, item)
			continue
		}
		snap, err := p.snapshotCache.Get(ctx, p.db, item.ContentSnapshotID)
		if err != nil {
			log.Printf("[SendWorkerPool] snapshot hydrate failed item=%s snapshot=%s: %v", item.ID, item.ContentSnapshotID, err)
			if markErr := p.markFailed(ctx, item.ID, "snapshot_load_failed: "+err.Error()); markErr != nil {
				log.Printf("[SendWorkerPool] markFailed after snapshot miss item=%s: %v", item.ID, markErr)
			}
			continue
		}
		item.HTMLContent = renderSnapshotBody(snap, item.SubscriberID, item.WaveID.String())
		if strings.TrimSpace(item.TextContent) == "" {
			item.TextContent = snap.PlainContent
		}
		out = append(out, item)
	}
	return out
}

// messageDatasetStampDisabled is the REQ-034 kill switch: set
// DISABLE_MESSAGE_DATASET_STAMP=true to revert to no partner-dataset
// stamping without a redeploy (send-path kill-switch rule). It nulls the
// stamped VALUE only — the SQL still references the columns, which is why
// their DDL lives in criticalSendPathDDL, not behind this switch.
func messageDatasetStampDisabled() bool {
	return os.Getenv("DISABLE_MESSAGE_DATASET_STAMP") == "true"
}

// datasetStampColumnsReady reports whether partner_dataset_id exists on BOTH
// send-path write targets. Until it does, markSent uses the legacy INSERTs
// (no stamp) so a missing column can never break sending — the deploy is
// decoupled from a lock we may not win during an active send. The columns
// land via criticalSendPathDDL on a boot that wins the lock, or by an
// operator-applied ALTER in a calm window; this latch then flips within one
// probe TTL with no redeploy. Fails CLOSED (no stamp) on probe error.
func (p *SendWorkerPool) datasetStampColumnsReady(ctx context.Context) bool {
	p.datasetColsMu.Lock()
	defer p.datasetColsMu.Unlock()
	if p.datasetColsReady {
		return true
	}
	if time.Now().Before(p.datasetColsNextProbe) {
		return false
	}
	p.datasetColsNextProbe = time.Now().Add(datasetResolveProbeTTL)
	var n int
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE column_name = 'partner_dataset_id'
		  AND table_name IN ('mailing_message_log', 'mailing_tracking_events')
	`).Scan(&n)
	if err != nil || n < 2 {
		log.Printf("[SendWorkerPool] partner_dataset_id stamp DORMANT: columns present=%d/2 (err=%v) — sending with legacy INSERTs, re-probe in %s", n, err, datasetResolveProbeTTL)
		return false
	}
	p.datasetColsReady = true
	log.Printf("[SendWorkerPool] partner_dataset_id stamp ACTIVE (columns present on message_log + tracking_events)")
	return true
}

// datasetResolveProbeTTL bounds how often a not-yet-ready pool re-checks for
// idx_pcq_subscriber_id (built by ensureConcurrentIndexes AFTER workers start,
// in a calm IO window — possibly hours after the first boot of this binary).
const datasetResolveProbeTTL = 5 * time.Minute

// datasetResolveIndexReady reports whether the subscriber→dataset batch
// lookup may run. It requires the partial index idx_pcq_subscriber_id
// (cmd/server/main.go concurrentIndexSpecs) to exist and be valid; until
// then resolution is skipped entirely so the send hot path can never be
// held behind a partner_clean_queue seq scan. Result latches true.
func (p *SendWorkerPool) datasetResolveIndexReady(ctx context.Context) bool {
	p.datasetResolveMu.Lock()
	defer p.datasetResolveMu.Unlock()
	if p.datasetResolveReady {
		return true
	}
	if time.Now().Before(p.datasetResolveNextProbe) {
		return false
	}
	p.datasetResolveNextProbe = time.Now().Add(datasetResolveProbeTTL)
	var valid bool
	err := p.db.QueryRowContext(ctx, `
		SELECT i.indisvalid
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = 'idx_pcq_subscriber_id'
	`).Scan(&valid)
	if err != nil || !valid {
		log.Printf("[SendWorkerPool] dataset stamp resolution dormant: idx_pcq_subscriber_id not valid yet (err=%v valid=%v); campaign-level stamps still apply, re-probe in %s", err, valid, datasetResolveProbeTTL)
		return false
	}
	p.datasetResolveReady = true
	log.Printf("[SendWorkerPool] dataset stamp resolution ACTIVE (idx_pcq_subscriber_id valid)")
	return true
}

// pcqMembership is one (dataset, first-ingest) membership row for a
// subscriber in partner_clean_queue. A subscriber can be a member of
// multiple datasets — the per-vertical uniqueness constraint
// (uq_pcq_vertical_email_md5) means one row per vertical per email.
type pcqMembership struct {
	DatasetID  uuid.UUID
	IngestedAt time.Time
}

// pickDatasetID applies the REQ-034 deterministic attribution rule for one
// message, given the campaign's deploy-time stamp and the subscriber's
// partner_clean_queue memberships:
//
//  1. Campaign stamped AND the subscriber is a member of that dataset →
//     the campaign's dataset (the campaign context resolves cross-vertical
//     multi-membership).
//  2. Subscriber has memberships (campaign unstamped — sidecar/broadcast —
//     OR stamped with a dataset the subscriber is provably NOT in, the
//     vertical-keyed misattribution class of dataset 9502c7c4) → the
//     EARLIEST-ingested membership, ties broken by smallest dataset id:
//     first-touch acquisition attribution, fully deterministic.
//  3. Campaign stamped, subscriber has NO memberships (pcq link-back never
//     stamped subscriber_id) → the campaign's dataset.
//  4. Neither → Nil (not a partner send; column stays NULL).
func pickDatasetID(campaignDS uuid.UUID, memberships []pcqMembership) uuid.UUID {
	if campaignDS != uuid.Nil {
		for _, m := range memberships {
			if m.DatasetID == campaignDS {
				return campaignDS
			}
		}
	}
	if len(memberships) > 0 {
		best := memberships[0]
		for _, m := range memberships[1:] {
			if m.IngestedAt.Before(best.IngestedAt) ||
				(m.IngestedAt.Equal(best.IngestedAt) && m.DatasetID.String() < best.DatasetID.String()) {
				best = m
			}
		}
		return best.DatasetID
	}
	return campaignDS
}

// resolveDatasetStamps refines each claimed item's PartnerDatasetID from the
// campaign-level stamp (already in the claim SELECT) to per-message truth via
// ONE batched partner_clean_queue lookup per claim batch (≤100 subscriber
// ids — never a per-message query; see pickDatasetID for the rule). Fails
// open: on any error items pass through with the campaign-level stamp only.
func (p *SendWorkerPool) resolveDatasetStamps(ctx context.Context, items []QueueItem) []QueueItem {
	if len(items) == 0 || messageDatasetStampDisabled() {
		return items
	}
	if !p.datasetResolveIndexReady(ctx) {
		return items
	}
	seen := make(map[uuid.UUID]struct{}, len(items))
	ids := make([]string, 0, len(items))
	for i := range items {
		if items[i].SubscriberID == uuid.Nil {
			continue
		}
		if _, ok := seen[items[i].SubscriberID]; !ok {
			seen[items[i].SubscriberID] = struct{}{}
			ids = append(ids, items[i].SubscriberID.String())
		}
	}
	if len(ids) == 0 {
		return items
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT subscriber_id, dataset_id, MIN(ingested_at)
		FROM partner_clean_queue
		WHERE subscriber_id = ANY($1)
		GROUP BY subscriber_id, dataset_id
	`, pq.Array(ids))
	if err != nil {
		log.Printf("[SendWorkerPool] dataset stamp resolve failed (campaign-level stamps only for this batch): %v", err)
		return items
	}
	defer rows.Close()
	memberships := make(map[uuid.UUID][]pcqMembership)
	for rows.Next() {
		var subID, dsID uuid.UUID
		var ingested time.Time
		if err := rows.Scan(&subID, &dsID, &ingested); err != nil {
			log.Printf("[SendWorkerPool] dataset stamp resolve scan error: %v", err)
			continue
		}
		memberships[subID] = append(memberships[subID], pcqMembership{DatasetID: dsID, IngestedAt: ingested})
	}
	for i := range items {
		items[i].PartnerDatasetID = pickDatasetID(items[i].PartnerDatasetID, memberships[items[i].SubscriberID])
	}
	return items
}

// claimISPBatch claims items proportionally across ISPs for a single campaign.
// Claims are issued concurrently — one goroutine per ISP.
func (p *SendWorkerPool) claimISPBatch(campaignID string, batchCounts map[string]int) ([]QueueItem, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	type ispResult struct {
		isp   string
		items []QueueItem
	}
	ch := make(chan ispResult, len(batchCounts))
	var wg sync.WaitGroup

	for isp, count := range batchCounts {
		if count <= 0 {
			continue
		}
		wg.Add(1)
		go func(isp string, count int) {
			defer wg.Done()
			items, err := p.claimISPForOne(ctx, campaignID, isp, count)
			if err != nil {
				log.Printf("[ISPDispatch] claim error campaign=%s isp=%s: %v", campaignID, isp, err)
				return
			}
			ch <- ispResult{isp: isp, items: items}
		}(isp, count)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	var allItems []QueueItem
	for res := range ch {
		allItems = append(allItems, res.items...)
	}
	return allItems, nil
}

// ---------------------------------------------------------------------------
// ISP-Proportional Campaign Dispatcher
// ---------------------------------------------------------------------------

// ispCampaignState tracks the batch plan and remaining quotas for one campaign.
type ispCampaignState struct {
	campaignID    string
	plan          map[string]int // per-ISP per-batch target (from ComputeBatchPlan)
	remaining     map[string]int // per-ISP queued items left (from DB, refreshed periodically)
	lastRefreshOK bool           // whether the most recent DB refresh succeeded
	lastRefreshAt time.Time      // when remaining was last refreshed from DB
}

// ispDispatchLoop is a single coordinator goroutine that discovers ISP-quota
// campaigns, computes proportional batch plans, claims items concurrently
// across ISPs, and fans claimed items out to a bounded worker pool.
func (p *SendWorkerPool) ispDispatchLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	states := make(map[string]*ispCampaignState)
	removedCooldown := make(map[string]time.Time) // campaigns recently removed as fully dispatched

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.refreshISPCampaigns(states, removedCooldown)
			p.dispatchISPBatches(states, removedCooldown)
		}
	}
}

// refreshISPCampaigns discovers sending campaigns with ISP quotas, initializes
// batch plans, and queries the DB for actual remaining queued counts per ISP
// (surviving server restarts). Removes campaigns no longer sending.
func (p *SendWorkerPool) refreshISPCampaigns(states map[string]*ispCampaignState, removedCooldown map[string]time.Time) {
	// Expire stale cooldown entries
	for cid, t := range removedCooldown {
		if time.Since(t) > 60*time.Second {
			delete(removedCooldown, cid)
		}
	}
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, `
		SELECT id::text, COALESCE(esp_quotas::text, '{}')
		FROM mailing_campaigns
		WHERE status = 'sending'
		  AND esp_quotas IS NOT NULL
		  AND esp_quotas ? 'isp_quotas'
	`)
	if err != nil {
		log.Printf("[ISPDispatch] Error querying ISP campaigns: %v", err)
		return
	}
	defer rows.Close()

	activeCampaigns := make(map[string]bool)
	for rows.Next() {
		var campID, quotasJSON string
		if err := rows.Scan(&campID, &quotasJSON); err != nil {
			continue
		}
		activeCampaigns[campID] = true

		if st, exists := states[campID]; exists {
			if time.Since(st.lastRefreshAt) >= 15*time.Second {
				p.refreshRemainingFromDB(st)
			}
			continue
		}
		if _, onCooldown := removedCooldown[campID]; onCooldown {
			continue
		}

		var qCfg struct {
			ISPQuotas []struct {
				ISP    string `json:"isp"`
				Volume int    `json:"volume"`
			} `json:"isp_quotas"`
		}
		if err := json.Unmarshal([]byte(quotasJSON), &qCfg); err != nil || len(qCfg.ISPQuotas) == 0 {
			continue
		}

		quotas := make(map[string]int, len(qCfg.ISPQuotas))
		for _, q := range qCfg.ISPQuotas {
			if q.Volume > 0 {
				quotas[strings.ToLower(q.ISP)] = q.Volume
			}
		}
		if len(quotas) == 0 {
			// Derive quotas from actual queue distribution when volumes are unset
			qRows, qErr := p.db.QueryContext(ctx, `
				SELECT COALESCE(recipient_isp, 'other'), COUNT(*)
				FROM mailing_campaign_queue
				WHERE campaign_id = $1 AND status = 'queued'
				GROUP BY recipient_isp`, campID)
			if qErr == nil {
				for qRows.Next() {
					var isp string
					var cnt int
					if qRows.Scan(&isp, &cnt) == nil && cnt > 0 {
						quotas[isp] = cnt
					}
				}
				qRows.Close()
			}
			if len(quotas) == 0 {
				continue
			}
			log.Printf("[ISPDispatch] Auto-derived quotas for campaign %s from queue: %v", campID, quotas)
		}

		totalItems := 0
		for _, v := range quotas {
			totalItems += v
		}
		numBatches := (totalItems + p.batchSize - 1) / p.batchSize
		if numBatches < 1 {
			numBatches = 1
		}

		state := &ispCampaignState{
			campaignID: campID,
			plan:       ComputeBatchPlan(quotas, numBatches),
			remaining:  make(map[string]int, len(quotas)),
		}
		if !p.refreshRemainingFromDB(state) {
			log.Printf("[ISPDispatch] Skipping campaign %s on first discovery (refresh failed, will retry next tick)", campID)
			continue
		}

		states[campID] = state
		log.Printf("[ISPDispatch] Tracking campaign %s: quotas=%v plan=%v remaining=%v batches=%d",
			campID, quotas, state.plan, state.remaining, numBatches)
	}

	for campID := range states {
		if !activeCampaigns[campID] {
			log.Printf("[ISPDispatch] Campaign %s no longer sending, removing state", campID)
			delete(states, campID)
		}
	}

	p.warnOrphanedCampaigns(ctx)
}

// warnOrphanedCampaigns detects campaigns in "sending" status that lack ISP
// quotas. These campaigns cannot be processed — the flat claim path has been
// removed. Logs a warning so operators can investigate and fix the campaign.
func (p *SendWorkerPool) warnOrphanedCampaigns(ctx context.Context) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT id::text, COALESCE(name, '')
		FROM mailing_campaigns
		WHERE status = 'sending'
		  AND (esp_quotas IS NULL OR NOT (esp_quotas ? 'isp_quotas'))
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var campID, campName string
		if rows.Scan(&campID, &campName) == nil {
			log.Printf("[ISPDispatch] WARNING: Campaign %s (%s) is 'sending' but has NO ISP quotas — it will NOT be processed. "+
				"All campaigns must have esp_quotas with isp_quotas set.", campID, campName)
		}
	}
}

// refreshRemainingFromDB queries the actual queued item counts per ISP from
// the database, making the dispatcher resilient to server restarts.
// Returns true on success, false on error (so callers can avoid acting on stale data).
func (p *SendWorkerPool) refreshRemainingFromDB(state *ispCampaignState) bool {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, `
		SELECT COALESCE(LOWER(recipient_isp), 'other'), COUNT(*)
		FROM mailing_campaign_queue
		WHERE campaign_id = $1
		  AND status = 'queued'
		GROUP BY COALESCE(LOWER(recipient_isp), 'other')
	`, state.campaignID)
	if err != nil {
		log.Printf("[ISPDispatch] Error refreshing remaining for %s: %v", state.campaignID, err)
		state.lastRefreshOK = false
		return false
	}
	defer rows.Close()

	fresh := make(map[string]int)
	for rows.Next() {
		var isp string
		var count int
		if err := rows.Scan(&isp, &count); err != nil {
			continue
		}
		fresh[isp] = count
	}

	for isp := range state.plan {
		state.remaining[isp] = fresh[isp]
	}
	state.remaining["other"] = fresh["other"]
	state.lastRefreshOK = true
	state.lastRefreshAt = time.Now()
	return true
}

// dispatchISPBatches runs one batch cycle for every tracked ISP-quota campaign.
// Claimed items are processed concurrently using a bounded goroutine pool.
// dispatchISPBatches walks every active ISP-targeted campaign and tries to
// claim a fresh batch for each. Each campaign is processed by an independent
// goroutine bounded by DISPATCH_CAMPAIGN_PARALLELISM (default 8). With ~190
// concurrent 'sending' campaigns observed in production, the previous
// single-goroutine serial loop was the dominant throughput ceiling: one slow
// claimISPBatch query or a brief workCh blocking write would starve every
// other campaign for the rest of the tick. Parallelizing the per-campaign
// dispatch unblocks the work pool and lifts aggregate send rate without
// touching any rate-limit / cap / per-IP-allocation accounting (each campaign
// owns its own state and never reads another campaign's).
func (p *SendWorkerPool) dispatchISPBatches(states map[string]*ispCampaignState, removedCooldown map[string]time.Time) {
	parallelism := dispatchCampaignParallelism()

	// Snapshot keys so the parent map can be safely mutated post-wait.
	campIDs := make([]string, 0, len(states))
	for id := range states {
		campIDs = append(campIDs, id)
	}

	type removal struct {
		campID string
		at     time.Time
	}
	var (
		removalsMu sync.Mutex
		removals   []removal
	)

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup

	for _, id := range campIDs {
		select {
		case <-p.ctx.Done():
			break
		default:
		}

		state, ok := states[id]
		if !ok {
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-p.ctx.Done():
			break
		}

		wg.Add(1)
		go func(campID string, st *ispCampaignState) {
			defer wg.Done()
			defer func() { <-sem }()

			if remove, when := p.dispatchOneCampaign(campID, st); remove {
				removalsMu.Lock()
				removals = append(removals, removal{campID: campID, at: when})
				removalsMu.Unlock()
			}
		}(id, state)
	}

	wg.Wait()

	// Apply removals serially after all goroutines finish to keep the
	// states map and removedCooldown map free of concurrent mutation.
	for _, r := range removals {
		delete(states, r.campID)
		removedCooldown[r.campID] = r.at
	}
}

// dispatchOneCampaign claims and enqueues at most one batch for a single
// campaign. Returns (remove=true, when=now) when the campaign has no remaining
// work and the parent should evict it from the active states map. Safe to run
// concurrently across campaigns: every call operates on a distinct *ispCampaignState
// (no cross-campaign reads), and downstream sinks (claimISPBatch DB pool,
// rateRegistry, workCh) are concurrency-safe by contract.
func (p *SendWorkerPool) dispatchOneCampaign(campID string, state *ispCampaignState) (bool, time.Time) {
	select {
	case <-p.ctx.Done():
		return false, time.Time{}
	default:
	}

	batchCounts := AssembleBatch(state.plan, state.remaining)
	if _, inPlan := state.plan["other"]; !inPlan {
		if otherRem := state.remaining["other"]; otherRem > 0 {
			cap := p.batchSize
			if otherRem < cap {
				cap = otherRem
			}
			batchCounts["other"] = cap
		}
	}
	if BatchTotal(batchCounts) == 0 {
		if !state.lastRefreshOK {
			log.Printf("[ISPDispatch] Campaign %s batch=0 but last refresh failed — keeping (will retry)", campID)
			return false, time.Time{}
		}
		log.Printf("[ISPDispatch] Campaign %s fully dispatched, removing (cooldown 60s)", campID)
		return true, time.Now()
	}

	// Per-IP allocation map: isp -> hostname -> allowed count
	// Populated when per-IP rate limiting is enabled; nil otherwise.
	var ipAllocations map[string]map[string]int

	if p.rateRegistry != nil && !p.rateLimitingDisabled {
		if p.perIPEnabled {
			ipAllocations = make(map[string]map[string]int)
			for isp, count := range batchCounts {
				if count <= 0 {
					continue
				}
				if p.rateRegistry.HasIPListStr(isp) {
					perIPAlloc := p.rateRegistry.DistributeByIPStr(isp, count)
					totalAllowed := 0
					for _, n := range perIPAlloc {
						totalAllowed += n
					}
					if totalAllowed == 0 {
						delete(batchCounts, isp)
					} else {
						batchCounts[isp] = totalAllowed
						ipAllocations[isp] = perIPAlloc
					}
				} else {
					allowed := p.rateRegistry.AllowN(isp, count)
					if allowed == 0 {
						delete(batchCounts, isp)
					} else {
						batchCounts[isp] = allowed
					}
				}
			}
		} else {
			for isp, count := range batchCounts {
				if count <= 0 {
					continue
				}
				allowed := p.rateRegistry.AllowN(isp, count)
				if allowed == 0 {
					delete(batchCounts, isp)
				} else {
					batchCounts[isp] = allowed
				}
			}
		}
		if BatchTotal(batchCounts) == 0 {
			return false, time.Time{}
		}
	}

	items, err := p.claimISPBatch(campID, batchCounts)
	if err != nil {
		log.Printf("[ISPDispatch] Claim error for campaign %s: %v", campID, err)
		return false, time.Time{}
	}

	if len(items) == 0 {
		return false, time.Time{}
	}

	// Cross-brand daily cap filter. Subscribers already at/above the
	// configured cap today are marked 'skipped' with reason
	// 'cross_brand_cap_exceeded' and excluded from the wave. Reserve
	// is atomic against Redis; on Redis outage falls back to Postgres.
	if p.capChecker != nil {
		items = p.applyCrossBrandCap(items)
		if len(items) == 0 {
			return false, time.Time{}
		}
	}

	// Assign pre-determined VMTAs to claimed items when per-IP is active
	if ipAllocations != nil && len(ipAllocations) > 0 {
		assignVMTAsToItems(items, ipAllocations)
	}

	actualCounts := make(map[string]int)
	for _, item := range items {
		actualCounts[ClassifySubscriberISP(item.Email)]++
	}
	for isp, n := range actualCounts {
		state.remaining[isp] -= n
		if state.remaining[isp] < 0 {
			state.remaining[isp] = 0
		}
	}
	log.Printf("[ISPDispatch] Campaign %s: claimed %d items (plan=%v actual=%v)",
		campID, len(items), batchCounts, actualCounts)

	p.enqueueForProcessing(items)
	return false, time.Time{}
}

// dispatchCampaignParallelism returns how many campaigns the per-tick
// dispatcher fans out to in parallel. Default 8 — chosen to keep per-tick DB
// contention well below the 40-conn server pool while still unblocking the
// 190-campaign serial bottleneck observed in production. Operators can pin
// "1" via DISPATCH_CAMPAIGN_PARALLELISM to fall back to legacy serial behavior
// for an A/B during a sending incident; values >32 are clamped.
func dispatchCampaignParallelism() int {
	raw := strings.TrimSpace(os.Getenv("DISPATCH_CAMPAIGN_PARALLELISM"))
	if raw == "" {
		return 8
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 8
	}
	if n > 32 {
		return 32
	}
	return n
}

// enqueueForProcessing pushes claimed items into the work channel for
// consumption by channel workers. Blocks if the channel is full (backpressure).
func (p *SendWorkerPool) enqueueForProcessing(items []QueueItem) {
	for _, item := range items {
		select {
		case <-p.ctx.Done():
			return
		case p.workCh <- item:
		}
	}
}

// assignVMTAsToItems distributes pre-assigned VMTA hostnames to claimed items
// based on the per-IP allocation map from DistributeByIP. Items for each ISP
// are round-robined across the allocated IPs proportional to their budgets.
func assignVMTAsToItems(items []QueueItem, ipAllocations map[string]map[string]int) {
	for i := range items {
		isp := ClassifySubscriberISP(items[i].Email)
		alloc, ok := ipAllocations[isp]
		if !ok || len(alloc) == 0 {
			continue
		}

		// Build ordered IP list from allocation for deterministic assignment
		type ipBudget struct {
			hostname  string
			remaining int
		}
		var ips []ipBudget
		for h, n := range alloc {
			if n > 0 {
				ips = append(ips, ipBudget{h, n})
			}
		}
		if len(ips) == 0 {
			continue
		}

		// Find the first IP with remaining budget
		assigned := false
		for j := range ips {
			if ips[j].remaining > 0 {
				items[i].AssignedVMTA = vmtaShortName(ips[j].hostname)
				ips[j].remaining--
				assigned = true
				break
			}
		}
		if !assigned {
			continue
		}

		// Write remaining budgets back (for subsequent items in same ISP)
		for _, ib := range ips {
			alloc[ib.hostname] = ib.remaining
		}
	}
}

func (p *SendWorkerPool) scanQueueRows(rows *sql.Rows) ([]QueueItem, error) {
	var items []QueueItem
	for rows.Next() {
		var item QueueItem
		var profileID, espType string
		err := rows.Scan(
			&item.ID,
			&item.CampaignID,
			&item.SubscriberID,
			&item.Email,
			&item.Subject,
			&item.HTMLContent,
			&item.TextContent,
			&item.PreviewText,
			&item.FromName,
			&item.FromEmail,
			&item.ReplyTo,
			&profileID,
			&espType,
			&item.FirstName,
			&item.LastName,
			&item.CustomFields,
			&item.EngagementScore,
			&item.TotalEmailsReceived,
			&item.TotalOpens,
			&item.TotalClicks,
			&item.LastOpenAt,
			&item.LastClickAt,
			&item.LastEmailAt,
			&item.OptimalSendHourUTC,
			&item.Timezone,
			&item.SubscriberStatus,
			&item.SubscriberSource,
			&item.SubscribedAt,
			&item.CampaignName,
			&item.OfferID,
			&item.CreativeID,
			&item.SubjectLineID,
			&item.FromNameID,
			&item.IdempotencyKey,
			&item.ContentSnapshotID,
			&item.WaveID,
			&item.PartnerDatasetID,
		)
		if err != nil {
			log.Printf("SendWorkerPool: scan error: %v", err)
			continue
		}
		item.ProfileID = profileID
		item.ESPType = espType
		item.BrandRoot = brand.RootFromEmail(item.FromEmail)
		items = append(items, item)
	}
	return items, nil
}

// processItem processes a single queue item
func (p *SendWorkerPool) processItem(item QueueItem) error {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	// Cross-brand cap slot was reserved in applyCrossBrandCap before this
	// item was placed on the work channel. If the send does not actually
	// go out (suppression, validation fail, ESP error) we must release the
	// slot so the subscriber can be mailed later today. The happy-path
	// Send call flips capSent=true to skip the release.
	capSent := false
	if p.capChecker != nil {
		defer func() {
			if !capSent {
				p.capChecker.Release(ctx, item.SubscriberID.String())
			}
		}()
	}

	// Gate: skip if campaign was cancelled or paused after claim
	var campStatus string
	if p.db.QueryRowContext(ctx, "SELECT status FROM mailing_campaigns WHERE id=$1", item.CampaignID).Scan(&campStatus) == nil {
		if campStatus == "cancelled" || campStatus == "paused" {
			return p.markSkipped(ctx, item.ID, "campaign_"+campStatus)
		}
	}

	// Load campaign's suppression list IDs and check suppression
	suppressionListIDs, err := p.getCampaignSuppressionListIDs(ctx, item.CampaignID)
	if err != nil {
		log.Printf("Error loading suppression lists for campaign %s: %v", item.CampaignID, err)
	}
	if len(suppressionListIDs) > 0 {
		suppressed, err := p.checkSuppression(ctx, item.Email, suppressionListIDs)
		if err != nil {
			log.Printf("Suppression check error for %s: %v", logger.RedactEmail(item.Email), err)
		}
		if suppressed {
			atomic.AddInt64(&p.totalSkipped, 1)
			return p.markSkipped(ctx, item.ID, "suppressed")
		}
	}

	// Global + brand-scoped suppression — single source of truth, single
	// read lock covers both axes. When item.BrandRoot is empty (unknown
	// sending domain), this falls back to a pure global check.
	if hub := p.getGlobalSuppressionHub(); hub != nil && hub.IsSuppressedForBrand(item.Email, item.BrandRoot) {
		atomic.AddInt64(&p.totalSkipped, 1)
		if item.BrandRoot != "" {
			return p.markSkipped(ctx, item.ID, "brand_suppressed")
		}
		return p.markSkipped(ctx, item.ID, "global_suppressed")
	}

	// Offer-level suppression — Bloom filter first (O(1)), DB fallback for false positives
	if item.OfferID != uuid.Nil {
		offerIDStr := item.OfferID.String()
		skipOffer := false

		if p.offerSuppChecker != nil {
			if suppressed, avail := p.offerSuppChecker.IsSuppressed(offerIDStr, item.Email); avail {
				if suppressed {
					// Bloom says maybe-suppressed; verify with DB to avoid false positives
					var dbConfirmed bool
					p.db.QueryRowContext(ctx,
						`SELECT EXISTS(SELECT 1 FROM mailing_offer_suppressions WHERE offer_id=$1 AND subscriber_id=$2)`,
						item.OfferID, item.SubscriberID).Scan(&dbConfirmed)
					skipOffer = dbConfirmed
				}
				// Bloom says definitely not suppressed: skip DB query entirely
			} else {
				// No Bloom available for this offer: full DB check
				p.db.QueryRowContext(ctx,
					`SELECT EXISTS(SELECT 1 FROM mailing_offer_suppressions WHERE offer_id=$1 AND subscriber_id=$2)`,
					item.OfferID, item.SubscriberID).Scan(&skipOffer)
			}
		} else {
			p.db.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM mailing_offer_suppressions WHERE offer_id=$1 AND subscriber_id=$2)`,
				item.OfferID, item.SubscriberID).Scan(&skipOffer)
		}

		if skipOffer {
			atomic.AddInt64(&p.totalSkipped, 1)
			return p.markSkipped(ctx, item.ID, "offer_suppressed")
		}
	}

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
		return p.quarantineUnrenderable(ctx, item, rf.field, rf.err)
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
		return p.quarantineUnrenderable(ctx, item, "text_content", textErr)
	}

	// Detect unresolved template syntax in rendered subject (high signal)
	if strings.Contains(subject, "{{") || strings.Contains(subject, "{%") {
		log.Printf("[SendWorkerPool] UNRESOLVED_TOKEN campaign=%s field=subject rendered=%q", item.CampaignID, subject)
	}

	// Text/plain fallback: auto-generate from final HTML if text is empty
	if strings.TrimSpace(textContent) == "" && strings.TrimSpace(htmlContent) != "" {
		textContent = GenerateTextFromHTML(htmlContent)
	}

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
		prefsURL := fmt.Sprintf("%s/preferences?sid=%s", trackBase, item.SubscriberID.String())
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

	// Post-render validation: audit or enforce based on VALIDATOR_MODE env var.
	//
	// FATAL issues are the exception — they block in EVERY mode. Before
	// 2026-08-10 this gate was inert against the incident it was built for:
	// VALIDATOR_MODE is unset in prod (so mode == audit and this branch could
	// never fire), AND the raw-liquid-in-html finding was warning-severity, so
	// even VALIDATOR_MODE=enforce would have shipped it. raw_liquid_source is
	// now fatal and quarantines on the same terminal path as a render error.
	validatorMode := GetValidatorMode()
	messageClass := "promotional"
	validationIssues := ValidateRenderedMessage(msg, validatorMode, messageClass)
	if HasFatalIssues(validationIssues) {
		return p.quarantineUnrenderable(ctx, item, "validator",
			fmt.Errorf("fatal content validation: %s", firstFatalMessage(validationIssues)))
	}
	if validatorMode == ValidatorEnforce && HasBlockingIssues(validationIssues) {
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, "message validation failed: blocking issues detected")
	}

	// Ownership re-check immediately before transport submission (REQ-005
	// criterion 1; findings 2026-07-13-B §1). A claimed item can wait in the
	// work channel + dispatch batches long past QueueRecovery's stale-age
	// under a PMTA slowdown; if recovery requeued this row (and another
	// worker possibly re-claimed and sent it), submitting here would deliver
	// the same email twice. Abort WITHOUT touching the row — it belongs to
	// the recovery / re-claimer lifecycle now; the deferred cap release
	// fires (capSent stays false) so the subscriber's cross-brand slot is
	// returned.
	if !p.stillOwnsItem(ctx, item.ID) {
		atomic.AddInt64(&p.totalSkipped, 1)
		log.Printf("[SendWorkerPool] OWNERSHIP LOST pre-submit: item %s no longer owned by %s — skipping to prevent duplicate send", item.ID, p.workerID)
		return nil
	}

	// Select sender based on ESP type
	var sender ESPSender
	switch item.ESPType {
	case "sparkpost":
		sender = p.sparkpostSender
	case "ses":
		sender = p.sesSender
	case "mailgun":
		sender = p.mailgunSender
	case "sendgrid":
		sender = p.sendgridSender
	case "pmta", "kumo":
		// Both dispatch through the ProfileBasedSender, which reads vendor_type
		// and selects the PMTA-bridge or KumoMTA injector per profile.
		sender = p.pmtaSender
	default:
		sender = p.sesSender
	}

	if sender == nil {
		atomic.AddInt64(&p.totalFailed, 1)
		return p.markFailed(ctx, item.ID, "no sender configured for "+item.ESPType)
	}

	// Send the email
	result, err := sender.Send(ctx, msg)
	if err != nil || !result.Success {
		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
		}

		// Strict-pool exhaustion: defer with bounded backoff, never bounce or suppress.
		if strings.Contains(errMsg, "deferred_strict_pool") {
			return p.deferStrictPool(ctx, item, errMsg)
		}

		atomic.AddInt64(&p.totalFailed, 1)

		if isTransportError(errMsg) {
			log.Printf("[SendWorkerPool] TRANSPORT ERROR campaign=%s email=%s esp=%s err=%s",
				item.CampaignID, logger.RedactEmail(item.Email), item.ESPType, errMsg)
		} else {
			log.Printf("[SendWorkerPool] ISP REJECT campaign=%s email=%s esp=%s err=%s",
				item.CampaignID, logger.RedactEmail(item.Email), item.ESPType, errMsg)
			p.recordBounce(ctx, item, errMsg)
		}

		return p.markFailed(ctx, item.ID, errMsg)
	}

	// Mark as sent and update campaign stats
	atomic.AddInt64(&p.totalSent, 1)
	capSent = true
	if err := p.markSent(ctx, item, result.MessageID, result.VMTA); err != nil {
		log.Printf("Error marking sent: %v", err)
	}

	// Update campaign sent count and subscriber email count
	p.db.ExecContext(ctx, `UPDATE mailing_campaigns SET sent_count = COALESCE(sent_count, 0) + 1, updated_at = NOW() WHERE id = $1`, item.CampaignID)
	p.db.ExecContext(ctx, `UPDATE mailing_subscribers SET total_emails_received = COALESCE(total_emails_received, 0) + 1, updated_at = NOW() WHERE id = $1`, item.SubscriberID)

	return nil
}

// stillOwnsItem re-verifies, immediately before transport submission, that
// this worker still owns a queue row it dequeued from the work channel: the
// row must still carry our worker_id AND still be in the status our claim
// wrote ('sending' legacy / 'submitting' durable, claimISPForOne). If
// QueueRecovery requeued it (worker_id NULL, status 'queued') or another
// worker re-claimed it (different worker_id), we must not submit.
//
// Fails OPEN on query error: a DB blip must never halt sending — this check
// is a best-effort duplicate guard, and markSent's status guard still
// prevents a double state-flip (though not a double SMTP injection, which is
// exactly what this check exists to stop).
//
// Kill switch: DISABLE_SEND_OWNERSHIP_RECHECK=true restores the pre-check
// behavior without a redeploy.
func (p *SendWorkerPool) stillOwnsItem(ctx context.Context, itemID uuid.UUID) bool {
	if os.Getenv("DISABLE_SEND_OWNERSHIP_RECHECK") == "true" {
		return true
	}
	expectedStatus := "sending"
	if p.isDurable() {
		expectedStatus = "submitting"
	}
	var workerID sql.NullString
	var status string
	err := p.db.QueryRowContext(ctx, `
		SELECT worker_id, status FROM mailing_campaign_queue WHERE id = $1
	`, itemID).Scan(&workerID, &status)
	if err != nil {
		log.Printf("[SendWorkerPool] ownership re-check error for item %s (failing open): %v", itemID, err)
		return true
	}
	return workerID.Valid && workerID.String == p.workerID && status == expectedStatus
}

// checkSuppression checks if an email is suppressed against the given suppression lists.
// It always compares by MD5 hash, which handles both plaintext-email and MD5-only
// suppression entries (the md5_hash column is populated for all entries).
func (p *SendWorkerPool) checkSuppression(ctx context.Context, email string, suppressionListIDs []string) (bool, error) {
	if len(suppressionListIDs) == 0 {
		return false, nil
	}

	// Compute MD5 of the lowercased, trimmed email (matches import logic)
	cleaned := strings.ToLower(strings.TrimSpace(email))
	hash := md5.Sum([]byte(cleaned))
	emailMD5 := hex.EncodeToString(hash[:])

	var exists bool
	err := p.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mailing_suppression_entries
			WHERE md5_hash = $1
			  AND list_id = ANY($2)
		)
	`, emailMD5, pq.Array(suppressionListIDs)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check suppression: %w", err)
	}
	return exists, nil
}

// getCampaignSuppressionListIDs loads the suppression_list_ids JSONB column
// from the mailing_campaigns table for the given campaign.
func (p *SendWorkerPool) getCampaignSuppressionListIDs(ctx context.Context, campaignID uuid.UUID) ([]string, error) {
	var raw sql.NullString
	err := p.db.QueryRowContext(ctx, `
		SELECT suppression_list_ids::text FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("load campaign suppression lists: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "[]" || raw.String == "null" {
		return nil, nil
	}
	var listIDs []string
	if err := json.Unmarshal([]byte(raw.String), &listIDs); err != nil {
		return nil, fmt.Errorf("parse suppression_list_ids: %w", err)
	}
	return listIDs, nil
}

// markSent marks a queue item as sent and records the tracking event with VMTA metadata.
//
// In durable mode this is the single atomic commit point for the outbox state
// machine: submitting -> accepted carrying message_id, sent_at, submitted_at,
// and pmta_response in one row-UPDATE. The WHERE guard on the prior state
// makes the transition idempotent — a concurrent worker or a crash-recovery
// replay cannot flip a row twice.
//
// In legacy mode the behavior is byte-for-byte identical to the original
// pending->sending->sent flow: the status='sending' guard matches what
// claimISPForOne wrote in legacy mode, so no column is overwritten that the
// legacy rollback path would need.
func (p *SendWorkerPool) markSent(ctx context.Context, item QueueItem, messageID, vmta string) error {
	var err error
	if p.isDurable() {
		_, err = p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'accepted',
			    message_id = $2,
			    sent_at = NOW(),
			    submitted_at = NOW(),
			    pmta_response = $3,
			    html_content = NULL,
			    plain_content = NULL
			WHERE id = $1
			  AND status = 'submitting'
		`, item.ID, messageID, vmta)
	} else {
		_, err = p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'sent',
			    message_id = $2,
			    sent_at = NOW(),
			    html_content = NULL,
			    plain_content = NULL
			WHERE id = $1
		`, item.ID, messageID)
	}

	if err != nil {
		return err
	}

	// Partner-dataset attribution stamp (REQ-034): resolved at claim time
	// (claim SELECT campaign stamp + resolveDatasetStamps subscriber
	// refinement). NULL for non-partner sends and under the kill switch.
	var partnerDatasetID interface{}
	if item.PartnerDatasetID != uuid.Nil && !messageDatasetStampDisabled() {
		partnerDatasetID = item.PartnerDatasetID
	}

	// The stamp columns may not exist yet (see datasetStampColumnsReady) — a
	// send must never fail because attribution DDL could not get its lock.
	stampCols := p.datasetStampColumnsReady(ctx)

	// Log message for webhook correlation
	if stampCols {
		p.db.ExecContext(ctx, `
			INSERT INTO mailing_message_log (id, message_id, organization_id, campaign_id, subscriber_id, email, esp_type, partner_dataset_id, sent_at)
			SELECT gen_random_uuid(), $1, camp.organization_id, $2, $3, $4, $5, $6::uuid, NOW()
			FROM mailing_campaigns camp WHERE camp.id = $2
		`, messageID, item.CampaignID, item.SubscriberID, item.Email, item.ESPType, partnerDatasetID)
	} else {
		p.db.ExecContext(ctx, `
			INSERT INTO mailing_message_log (id, message_id, organization_id, campaign_id, subscriber_id, email, esp_type, sent_at)
			SELECT gen_random_uuid(), $1, camp.organization_id, $2, $3, $4, $5, NOW()
			FROM mailing_campaigns camp WHERE camp.id = $2
		`, messageID, item.CampaignID, item.SubscriberID, item.Email, item.ESPType)
	}

	// Extract sending and recipient domains
	sendingDomain := ""
	if atIdx := strings.LastIndex(item.FromEmail, "@"); atIdx >= 0 {
		sendingDomain = strings.ToLower(item.FromEmail[atIdx+1:])
	}
	recipientDomain := ""
	if atIdx := strings.LastIndex(item.Email, "@"); atIdx >= 0 {
		recipientDomain = strings.ToLower(item.Email[atIdx+1:])
	}

	meta := "{}"
	if vmta != "" {
		metaJSON, _ := json.Marshal(map[string]string{"vmta": vmta})
		meta = string(metaJSON)
	}
	// THIS IS THE CANONICAL `sent` WRITER (METRIC_CONTRACT.md §2.1, REQ-086).
	// One row per message, at submission, for EVERY transport (the ESP switch
	// above dispatches pmta/kumo/ses/sparkpost/mailgun/sendgrid all through
	// here). Nothing else may write event_type='sent' on this path — the SES
	// `Send` notification used to, and gave SES-relayed mail two rows per
	// message; it now records 'ses_accepted' (handlers_ses_events.go).
	//
	// offer_id/creative_id/subject_line_id carry the campaign's deploy-time
	// attribution stamp onto the sent event; NULL for unstamped campaigns.
	// partner_dataset_id mirrors the message-log stamp (REQ-034 parity).
	trackSQL := `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, sending_domain, recipient_domain, metadata, offer_id, creative_id, subject_line_id, partner_dataset_id)
		SELECT gen_random_uuid(), camp.organization_id, $1, $2, 'sent', NOW(), $3, $4, $5::jsonb, camp.offer_id, camp.creative_id, camp.subject_line_id, $6::uuid
		FROM mailing_campaigns camp WHERE camp.id = $1`
	trackArgs := []interface{}{item.CampaignID, item.SubscriberID, sendingDomain, recipientDomain, meta, partnerDatasetID}
	if !stampCols {
		trackSQL = `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, sending_domain, recipient_domain, metadata, offer_id, creative_id, subject_line_id)
		SELECT gen_random_uuid(), camp.organization_id, $1, $2, 'sent', NOW(), $3, $4, $5::jsonb, camp.offer_id, camp.creative_id, camp.subject_line_id
		FROM mailing_campaigns camp WHERE camp.id = $1`
		trackArgs = trackArgs[:5]
	}
	if _, trackErr := p.db.ExecContext(ctx, trackSQL, trackArgs...); trackErr != nil {
		log.Printf("[send_worker] tracking event INSERT failed for campaign=%s sub=%s: %v", item.CampaignID, item.SubscriberID, trackErr)
	}

	// Master List Migration P2 — shadow write to subscriber_domain_state.
	// Additive: legacy list_id selection still owns reads; this mirrors
	// the per-domain send state so P3 pilot can join against real data.
	mailing.UpsertSDSSend(ctx, p.db, item.SubscriberID, sendingDomain)

	// Per-domain throughput accounting (SA-7). One in-memory map insert,
	// no DB I/O. The successful submission is what we count — failures /
	// transport errors / suppressions are tracked elsewhere.
	if p.throughputTracker != nil {
		p.throughputTracker.RecordSend(sendingDomain)
	}

	// Upsert inbox profile for this recipient
	if recipientDomain != "" {
		eHash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ToLower(item.Email))))
		p.db.ExecContext(ctx, `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_sends, last_send_at, first_seen_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET
				total_sends = COALESCE(mailing_inbox_profiles.total_sends, 0) + 1,
				last_send_at = NOW(), updated_at = NOW()
		`, eHash, strings.ToLower(item.Email), recipientDomain)
	}

	return nil
}

// renderFailure pairs a recipient-visible message field with the error the
// Liquid engine returned for it.
type renderFailure struct {
	field string
	err   error
}

// firstRenderFailure returns the first field that failed to render, or nil.
func firstRenderFailure(candidates ...renderFailure) *renderFailure {
	for i := range candidates {
		if candidates[i].err != nil {
			return &candidates[i]
		}
	}
	return nil
}

// renderFailClosedDisabled is the kill switch for the 2026-08-10 fail-closed
// render guard: set DISABLE_RENDER_FAILCLOSED=1 to restore the previous
// log-and-ship-anyway behavior without a redeploy. Same idiom as
// DISABLE_BRAND_IMAGE_HOST_SWAP / DISABLE_SEND_OWNERSHIP_RECHECK.
func renderFailClosedDisabled() bool {
	return os.Getenv("DISABLE_RENDER_FAILCLOSED") == "1"
}

// quarantineUnrenderable is the terminal, non-retrying disposition for a queue
// row whose body could not be rendered.
//
// WHY dead_letter and not a retry: a Liquid parse failure is a property of the
// CREATIVE, not of this attempt. It fails identically for every recipient and
// on every retry, so 'failed_retryable' would spin the row through the full
// MaxRetryCount backoff ladder — pointless load, a misleading "transient"
// status, and a delayed terminal signal. dead_letter is the existing terminal
// bucket: it does not retry, it is already surfaced to operators
// (outbox_engine_status.go:420 dead_letter_24h, outbox_admin.go:298 browser,
// outbox_selfcheck.go:117 dead_letter_spike), and — critically — it already
// counts toward wave/campaign completion (campaign_scheduler.go:163), so
// quarantining does NOT strand the campaign in 'sending' forever.
//
// WHY NOT fail the whole campaign here: this runs per-message inside 25
// concurrent workers under FOR UPDATE SKIP LOCKED. A per-row actor mutating
// campaign-level state is a race and an autonomous-remediation violation
// (JAOS core §1.8). It is also unnecessary: when the creative is broken every
// row quarantines, failed == total, and checkCompletedCampaigns marks the
// campaign 'cancelled' with a log line (campaign_scheduler.go:194-198). The
// campaign-level signal arrives on its own, from the component that owns it.
//
// The status guard mirrors markFailed's: if QueueRecovery requeued this row
// mid-flight, it belongs to the re-claimer's lifecycle and we must not clobber
// it.
func (p *SendWorkerPool) quarantineUnrenderable(ctx context.Context, item QueueItem, field string, cause error) error {
	if renderFailClosedDisabled() {
		log.Printf("[SendWorkerPool] RENDER_FAILCLOSED_DISABLED campaign=%s field=%s — shipping unrendered body per DISABLE_RENDER_FAILCLOSED=1", item.CampaignID, field)
		return nil
	}

	atomic.AddInt64(&p.totalFailed, 1)
	reason := fmt.Sprintf("render_blocked: %s failed to render (body would have shipped as raw template source): %v", field, cause)

	// Loud, greppable, and carries everything an operator needs to find the
	// creative: this is the line that should have existed on 2026-08-10.
	log.Printf("[SendWorkerPool] RENDER_BLOCKED campaign=%s item=%s field=%s email=%s — NOT SENT, quarantined to dead_letter: %v",
		item.CampaignID, item.ID, field, logger.RedactEmail(item.Email), cause)

	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'dead_letter',
		    error_message = $2,
		    attempts = COALESCE(attempts, 0) + 1,
		    last_attempt_at = NOW()
		WHERE id = $1
		  AND status IN ('submitting', 'sending', 'claimed', 'failed_retryable')
	`, item.ID, reason)
	if err != nil {
		log.Printf("[SendWorkerPool] RENDER_BLOCKED quarantine write failed for item %s: %v", item.ID, err)
	}
	return err
}

// markFailed marks a queue item as failed, or as dead_letter if max retries exceeded.
//
// In durable mode the terminal-vs-retryable decision is the same as legacy —
// attempts+1 >= MaxRetryCount routes to dead_letter, everything else to
// failed_retryable with an exponential-backoff next_attempt_at so the claim
// query naturally re-picks the row when the backoff window elapses. The prior
// state guard accepts both 'submitting' (claim already flipped) and legacy
// 'sending' so a mid-flight mode flip between drains cannot strand rows.
//
// In legacy mode the behavior is byte-for-byte identical to the pre-outbox
// implementation.
func (p *SendWorkerPool) markFailed(ctx context.Context, itemID uuid.UUID, errMsg string) error {
	// PMTA-local transient errors (bridge unreachable, pmtad crash window,
	// resolver stall) must not burn recipient retry budget. Requeue with a
	// 10-minute scheduled_at cooldown so the dispatcher's `scheduled_at <=
	// NOW()` gate naturally re-picks the row after PMTA recovers. attempts
	// is intentionally left untouched. See IsPMTATransient for the
	// classification and the rationale.
	if IsPMTATransient(errMsg) {
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'queued',
			    error_message = $2,
			    last_attempt_at = NOW(),
			    scheduled_at = GREATEST(scheduled_at, NOW() + ($3 || ' minutes')::interval),
			    worker_id = NULL,
			    locked_at = NULL
			WHERE id = $1
		`, itemID, errMsg, fmt.Sprintf("%d", PMTATransientBackoffMinutes))
		if err == nil {
			log.Printf("[SendWorkerPool] PMTA transient: item %s requeued in %dm, attempts unchanged", itemID, PMTATransientBackoffMinutes)
		}
		return err
	}

	var attempts int
	_ = p.db.QueryRowContext(ctx, `
		SELECT COALESCE(attempts, 0) FROM mailing_campaign_queue WHERE id = $1
	`, itemID).Scan(&attempts)

	if p.isDurable() {
		if attempts+1 >= MaxRetryCount {
			_, err := p.db.ExecContext(ctx, `
				UPDATE mailing_campaign_queue
				SET status = 'dead_letter',
				    error_message = $2,
				    attempts = attempts + 1,
				    last_attempt_at = NOW()
				WHERE id = $1
				  AND status IN ('submitting', 'sending', 'claimed', 'failed_retryable')
			`, itemID, errMsg)
			if err == nil {
				log.Printf("[SendWorkerPool] Item %s moved to dead_letter after %d attempts (durable)", itemID, attempts+1)
			}
			return err
		}

		// Exponential backoff capped at 30 minutes, matching the plan's
		// centralized retry policy. Attempt count is the already-recorded
		// value; the next attempt_at is set relative to now so the claim
		// query's (next_attempt_at IS NULL OR next_attempt_at <= NOW())
		// predicate naturally re-picks this row at the right moment.
		backoffSeconds := (attempts + 1) * (attempts + 1) * 30
		if backoffSeconds > 1800 {
			backoffSeconds = 1800
		}
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'failed_retryable',
			    error_message = $2,
			    attempts = attempts + 1,
			    last_attempt_at = NOW(),
			    next_attempt_at = NOW() + ($3 || ' seconds')::interval
			WHERE id = $1
			  AND status IN ('submitting', 'sending', 'claimed', 'failed_retryable')
		`, itemID, errMsg, fmt.Sprintf("%d", backoffSeconds))
		return err
	}

	if attempts+1 >= MaxRetryCount {
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue 
			SET status = 'dead_letter', error_message = $2, attempts = attempts + 1, last_attempt_at = NOW()
			WHERE id = $1
		`, itemID, errMsg)
		if err == nil {
			log.Printf("[SendWorkerPool] Item %s moved to dead_letter after %d attempts", itemID, attempts+1)
		}
		return err
	}

	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue 
		SET status = 'failed', error_message = $2, attempts = attempts + 1, last_attempt_at = NOW()
		WHERE id = $1
	`, itemID, errMsg)
	return err
}

// deferStrictPool handles strict-isolation pool exhaustion by deferring the
// message with bounded exponential backoff. The recipient is NOT suppressed,
// NOT bounced, and NOT counted as a send failure. This is a capacity/availability
// constraint on the isolated IP, not a delivery failure.
func (p *SendWorkerPool) deferStrictPool(ctx context.Context, item QueueItem, errMsg string) error {
	var attempts int
	_ = p.db.QueryRowContext(ctx, `
		SELECT COALESCE(attempts, 0) FROM mailing_campaign_queue WHERE id = $1
	`, item.ID).Scan(&attempts)

	if attempts+1 >= MaxStrictPoolRetries {
		// Dead-letter with operator alert — NOT a suppression event.
		log.Printf("[STRICT_POOL_DEAD_LETTER] campaign=%s email=%s attempts=%d — operator action required: activate standby IP, increase daily limit, or reduce campaign volume",
			item.CampaignID, logger.RedactEmail(item.Email), attempts+1)
		_, err := p.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'dead_letter_strict', error_message = $2, attempts = attempts + 1, last_attempt_at = NOW()
			WHERE id = $1
		`, item.ID, errMsg)
		return err
	}

	// Exponential backoff: 30s, 60s, 120s, 240s, 300s, 300s, ...
	backoff := StrictPoolBackoffBase * time.Duration(1<<uint(attempts))
	if backoff > StrictPoolBackoffCap {
		backoff = StrictPoolBackoffCap
	}
	retryAt := time.Now().Add(backoff)

	if (attempts+1)%3 == 0 {
		log.Printf("[STRICT_POOL_EXHAUSTED] campaign=%s ISP=%s attempts=%d next_retry=%s — isolated IP may be at capacity",
			item.CampaignID, ClassifySubscriberISP(item.Email), attempts+1, retryAt.Format(time.RFC3339))
	}

	// Backoff via the same scheduled_at pushback the PMTA-transient branch of
	// markFailed uses (REQ-005 criterion 4; findings 2026-07-13-B §10). The
	// claim query already enforces `q.scheduled_at <= NOW()` (claimISPForOne),
	// so the row becomes claim-eligible exactly when the backoff expires — no
	// new claim predicate, no new index, zero claim-perf impact (the partial
	// index idx_queue_campaign_status_scheduled on (campaign_id, status,
	// scheduled_at) WHERE status='queued' keeps covering the claim verbatim).
	// The previous implementation wrote retry_after, which NO claim/recovery
	// predicate ever read, and left worker_id/locked_at intact — so the
	// intended 30s→300s ladder was actually a flat ~5-minute locked_at
	// shadow. worker_id/locked_at are now cleared so the computed backoff is
	// the only gate.
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'queued', error_message = $2, attempts = attempts + 1,
		    last_attempt_at = NOW(),
		    scheduled_at = GREATEST(scheduled_at, $3),
		    worker_id = NULL,
		    locked_at = NULL
		WHERE id = $1
	`, item.ID, errMsg, retryAt)
	return err
}

// recordBounce inserts a tracking event for the failed send and, for hard
// bounces, adds the address to the global suppression hub so it is never
// mailed again.
//
// GUARD: Before writing anything, we check that no 'delivered' event
// already exists for this (campaign, subscriber) pair in the last 24h.
// PMTA's accounting pipeline is authoritative — if PMTA already reported a
// successful delivery for this recipient on this campaign, any later
// "bounce" we observe is a submission-retry artifact and must be
// discarded. This protects against future classes of phantom-bounce bugs
// beyond the PMTA HTTP bridge 5xx case (which isTransportError already
// short-circuits).
func (p *SendWorkerPool) recordBounce(ctx context.Context, item QueueItem, errMsg string) {
	var alreadyDelivered bool
	if err := p.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mailing_tracking_events
			WHERE campaign_id = $1 AND subscriber_id = $2
			  AND event_type = 'delivered'
			  AND event_at > NOW() - INTERVAL '24 hours'
		)
	`, item.CampaignID, item.SubscriberID).Scan(&alreadyDelivered); err == nil && alreadyDelivered {
		log.Printf("[SendWorkerPool] skip phantom bounce — already delivered: campaign=%s sub=%s err=%s",
			item.CampaignID, item.SubscriberID, errMsg)
		return
	}

	bounceType := classifySendError(errMsg)
	sendingDomain := ""
	if atIdx := strings.LastIndex(item.FromEmail, "@"); atIdx >= 0 {
		sendingDomain = strings.ToLower(item.FromEmail[atIdx+1:])
	}
	recipientDomain := ""
	if atIdx := strings.LastIndex(item.Email, "@"); atIdx >= 0 {
		recipientDomain = strings.ToLower(item.Email[atIdx+1:])
	}

	bounceMeta := "{}"
	if item.AssignedVMTA != "" {
		metaJSON, _ := json.Marshal(map[string]string{"vmta": item.AssignedVMTA})
		bounceMeta = string(metaJSON)
	}
	_, dbErr := p.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events
			(id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, bounce_reason, event_at, sending_domain, recipient_domain, metadata)
		VALUES ($1, $2, $3, $4, 'bounced', $5, $6, NOW(), $7, $8, $9::jsonb)
	`, uuid.New(), p.orgID, item.CampaignID, item.SubscriberID,
		bounceType, errMsg, sendingDomain, recipientDomain, bounceMeta)
	if dbErr != nil {
		log.Printf("[SendWorkerPool] bounce tracking insert error: %v", dbErr)
	}

	p.db.ExecContext(ctx, `UPDATE mailing_campaigns SET bounce_count = COALESCE(bounce_count, 0) + 1, updated_at = NOW() WHERE id = $1`, item.CampaignID)
	if bounceType == "hard" {
		p.db.ExecContext(ctx, `UPDATE mailing_campaigns SET hard_bounce_count = COALESCE(hard_bounce_count, 0) + 1 WHERE id = $1`, item.CampaignID)
	} else {
		p.db.ExecContext(ctx, `UPDATE mailing_campaigns SET soft_bounce_count = COALESCE(soft_bounce_count, 0) + 1 WHERE id = $1`, item.CampaignID)
	}

	if suppressor := p.getGlobalSuppressionWriter(); bounceType == "hard" && suppressor != nil {
		ispGroup := recipientDomain
		if _, suppressErr := suppressor.Suppress(
			ctx, item.Email, "hard_bounce", "send_worker",
			ispGroup, "", errMsg, "", item.CampaignID.String(),
		); suppressErr != nil {
			log.Printf("[SendWorkerPool] global suppress error for %s: %v",
				logger.RedactEmail(item.Email), suppressErr)
		}
	}

	// Master List Migration P2 — shadow write to subscriber_domain_state.
	// Hard bounces stamp the per-domain SDS row AND mailing_subscribers
	// (global hard_bounced_at) per the agreed design.
	if bounceType == "hard" {
		mailing.UpsertSDSHardBounce(ctx, p.db, item.SubscriberID, sendingDomain)
	}

	// Upsert inbox profile for bounced recipient
	if recipientDomain != "" {
		eHash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ToLower(item.Email))))
		p.db.ExecContext(ctx, `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, total_bounces, last_bounce_at, first_seen_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, 1, NOW(), NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET
				total_bounces = COALESCE(mailing_inbox_profiles.total_bounces, 0) + 1,
				last_bounce_at = NOW(), updated_at = NOW()
		`, eHash, strings.ToLower(item.Email), recipientDomain)
	}
}

// pmtaHTTPStatus parses the HTTP status code embedded in a PMTA bridge error
// of the form "PMTA API error (HTTP <n>): ...". Returns -1 if not present.
// The PMTA HTTP bridge uses 5xx to signal its OWN failures (bridge crashed,
// upstream PMTA unreachable, etc) and 4xx to signal caller errors (bad
// envelope, missing recipient). 5xx/408/429 must never be treated as ISP
// rejections — the message was never handed to an ISP.
func pmtaHTTPStatus(errMsg string) int {
	lower := strings.ToLower(errMsg)
	const marker = "pmta api error (http "
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return -1
	}
	rest := errMsg[idx+len(marker):]
	end := strings.IndexByte(rest, ')')
	if end <= 0 {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:end]))
	if err != nil {
		return -1
	}
	return n
}

// isTransportError returns true when the error is an infrastructure/network
// failure (the message was never presented to any ISP) as opposed to an
// actual recipient rejection. Transport errors should be retried without
// inflating bounce counters.
//
// WHY THIS MATTERS:
// The PMTA HTTP bridge on :19099 is a local submission gateway. If it
// returns a 5xx, times out, or reports "out of connection slots", the
// message is still in our hand — PMTA never saw it. Previously these were
// being classified as ISP rejections and written as phantom soft-bounces,
// inflating the "bounce" metric while PMTA silently retried and delivered
// the message on the second submission.
func isTransportError(errMsg string) bool {
	lower := strings.ToLower(errMsg)

	// Any PMTA HTTP bridge 5xx / 408 / 429 is a bridge-side failure, not
	// an ISP rejection. The message hasn't been queued yet.
	if code := pmtaHTTPStatus(errMsg); code >= 500 || code == 408 || code == 429 {
		return true
	}

	transportIndicators := []string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"connection unexpectedly closed", // PMTA bridge drop mid-response
		"out of connection slots",        // PMTA local-VMTA submission congestion
		"i/o timeout",
		"no such host",
		"no route to host",
		"network is unreachable",
		"dial tcp",
		"dial udp",
		"tls handshake",
		"eof",
		"broken pipe",
		"context deadline exceeded",
		"context canceled",
		"http2",
		"server misbehaving",
		"marshal pmta payload",
		"create pmta request",
		"pmta api request to",
		"no sender configured",
		"all ips exhausted",
		"deferring send",
		"no sending ips configured",
		"refusing to send via default-pool",
		"no vmta routing available",
		"empty hostname",
	}
	for _, ind := range transportIndicators {
		if strings.Contains(lower, ind) {
			return true
		}
	}
	return false
}

// classifySendError determines hard vs soft bounce from SMTP error text.
func classifySendError(errMsg string) string {
	lower := strings.ToLower(errMsg)
	hardIndicators := []string{
		"550", "551", "552", "553", "554",
		"user unknown", "mailbox not found", "does not exist",
		"no such user", "invalid recipient", "rejected",
		"permanently", "disabled", "deactivated",
	}
	for _, ind := range hardIndicators {
		if strings.Contains(lower, ind) {
			return "hard"
		}
	}
	return "soft"
}

// markSkipped marks a queue item as skipped
func (p *SendWorkerPool) markSkipped(ctx context.Context, itemID uuid.UUID, reason string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue 
		SET status = 'skipped', error_message = $2
		WHERE id = $1
	`, itemID, reason)
	return err
}

// applyCrossBrandCap filters a claimed batch against the daily cap. Items
// that exceed the cap are marked 'skipped' with reason
// 'cross_brand_cap_exceeded' on the queue and dropped from the returned
// slice. Counters are updated so AnalyticsCenter can surface a wave-level
// skipped_cap_exceeded metric.
func (p *SendWorkerPool) applyCrossBrandCap(items []QueueItem) []QueueItem {
	if p.capChecker == nil || len(items) == 0 {
		return items
	}
	ctx := p.ctx
	kept := items[:0]
	for _, it := range items {
		allowed, _, err := p.capChecker.Reserve(ctx, p.orgID, it.SubscriberID.String())
		if err != nil {
			kept = append(kept, it)
			continue
		}
		if allowed {
			kept = append(kept, it)
			continue
		}
		atomic.AddInt64(&p.totalSkippedCap, 1)
		atomic.AddInt64(&p.totalSkipped, 1)
		if mErr := p.markSkipped(ctx, it.ID, "cross_brand_cap_exceeded"); mErr != nil {
			log.Printf("[cap] markSkipped failed for item=%s: %v", it.ID, mErr)
		}
	}
	return kept
}

// registerWorker registers this worker in the database
func (p *SendWorkerPool) registerWorker() {
	p.db.Exec(`
		INSERT INTO mailing_workers (id, worker_type, hostname, status, max_concurrent, started_at, last_heartbeat_at)
		VALUES ($1, 'campaign_sender', $2, 'running', $3, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			status = 'running',
			started_at = NOW(),
			last_heartbeat_at = NOW()
	`, p.workerID, getHostname(), p.numWorkers*p.batchSize)
}

// deregisterWorker removes this worker from the database
func (p *SendWorkerPool) deregisterWorker() {
	p.db.Exec(`UPDATE mailing_workers SET status = 'stopped' WHERE id = $1`, p.workerID)
}

// throughputLogLoop emits a single "[send_worker] throughput last_60s: ..."
// line every 60 seconds for operator visibility into per-sending-domain
// send rates. Skips emission when the snapshot is empty so quiet periods
// don't spam the log. Lifecycle-bound to p.ctx — Stop() cancels the loop.
func (p *SendWorkerPool) throughputLogLoop() {
	if p.throughputTracker == nil {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			snap := p.throughputTracker.Snapshot()
			if len(snap) == 0 {
				continue
			}
			log.Printf("[send_worker] throughput last_60s: %s", FormatThroughputLog(snap))
		}
	}
}

// heartbeatLoop sends periodic heartbeats
func (p *SendWorkerPool) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			stats := p.Stats()
			statsJSON, _ := json.Marshal(stats)
			p.db.Exec(`
				UPDATE mailing_workers 
				SET 
					last_heartbeat_at = NOW(),
					total_processed = $2,
					total_errors = $3,
					metadata = $4
				WHERE id = $1
			`, p.workerID, stats["total_sent"], stats["total_failed"], string(statsJSON))
		}
	}
}

func getHostname() string {
	// Would normally use os.Hostname()
	return "ignite-worker"
}

// ReplaceTrackingMergeTags replaces Everflow tracking link merge tags at send time.
// {{DATE_MMDDYYYY}} -> current date in mmddYYYY format
// {{MAILING_ID}} -> the campaign/mailing ID (subscriber-specific for tracking)
func ReplaceTrackingMergeTags(html string, campaignID string, subscriberID string, creativeID ...string) string {
	now := time.Now()
	dateStr := fmt.Sprintf("%02d%02d%d", now.Month(), now.Day(), now.Year())

	html = strings.ReplaceAll(html, "{{DATE_MMDDYYYY}}", dateStr)
	html = strings.ReplaceAll(html, "{{MAILING_ID}}", subscriberID)
	html = strings.ReplaceAll(html, "{{SUBSCRIBER_ID}}", subscriberID)
	html = strings.ReplaceAll(html, "{{CAMPAIGN_ID}}", campaignID)
	if len(creativeID) > 0 && creativeID[0] != "" {
		html = strings.ReplaceAll(html, "{{CREATIVE_ID}}", creativeID[0])
	}

	return html
}

// ---------------------------------------------------------------------------
// Tracking injection: open pixel, click redirects, unsubscribe URL
// ---------------------------------------------------------------------------

var linkRe = regexp.MustCompile(`href=["'](https?://[^"']+)["']`)

// TrackSign computes a truncated HMAC-SHA256 signature for tracking URLs.
func TrackSign(data, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (p *SendWorkerPool) trackSign(data string) string {
	return TrackSign(data, p.trackingSecret)
}

// InjectTrackingPixelAndLinks adds an open-tracking pixel and rewrites href
// links to click-tracking URLs. orgID and secret are passed explicitly so this
// function can be called outside of a SendWorkerPool context (e.g. proof sends).
//
// Pixel placement strategy (post 2026-04-28):
//   - One pixel near the TOP of <body> wrapped in a hidden DIV. Gmail's image
//     proxy walks INTO display:none wrappers but skips display:none directly
//     on an <img>. Top placement also survives Gmail's 102KB clipping rule
//     that drops anything past the clip point.
//   - One pixel before </body> with the same wrapper for redundancy.
//
// Both pixel SRCs are byte-identical so the consumer dedupes them via
// ON CONFLICT DO NOTHING (the partition PK includes the event id which is
// derived from emailID; the second fetch is a no-op at the DB level).
//
// Hiding uses position:absolute + opacity:0 (visually invisible) on the
// wrapper rather than display:none on the <img>, matching the SparkPost /
// HistoryFacts pattern that we have empirical evidence Gmail proxies
// reliably fetch.
func InjectTrackingPixelAndLinks(html, campaignID, subscriberID, emailID, baseURL, orgID, secret string) string {
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	sig := TrackSign(encoded, secret)

	// Wrap the <img> in a hidden DIV so Gmail's image proxy walks the DOM
	// into the wrapper and fetches the IMG. display:none directly on the
	// IMG element causes the proxy to skip the element entirely.
	pixelHTML := fmt.Sprintf(
		`<div style="display:none;font-size:1px;color:transparent;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;mso-hide:all;" aria-hidden="true"><img src="%s/track/open/%s/%s" width="1" height="1" border="0" alt="" /></div>`,
		baseURL, encoded, sig,
	)

	htmlLower := strings.ToLower(html)

	// Top placement — right after <body...> open tag. Survives Gmail
	// clipping when the email body is large.
	if bodyIdx := strings.Index(htmlLower, "<body"); bodyIdx >= 0 {
		// Find the matching '>' that closes the <body ...> tag.
		if closeIdx := strings.Index(html[bodyIdx:], ">"); closeIdx >= 0 {
			insertAt := bodyIdx + closeIdx + 1
			html = html[:insertAt] + pixelHTML + html[insertAt:]
			htmlLower = strings.ToLower(html)
		}
	}

	// Bottom placement — right before </body>. Redundant fallback.
	if idx := strings.LastIndex(htmlLower, "</body>"); idx >= 0 {
		html = html[:idx] + pixelHTML + html[idx:]
	} else {
		html += pixelHTML
	}

	return RewriteClickLinks(html, campaignID, subscriberID, emailID, baseURL, orgID, secret)
}

// RewriteClickLinks rewrites href links to signed /track/click redirect URLs
// with NO pixel injection — the links-only half of
// InjectTrackingPixelAndLinks, factored out (2026-07-13) so the SES relay
// path can restore click tracking without double-injecting the open pixel.
// Skip rules are inherited verbatim from the original loop:
//   - linkRe only matches absolute href="http(s)://..." values, so mailto:,
//     tel:, in-page anchors (#...) and relative links never match at all;
//   - URLs containing "/track/" (unsubscribe/open/already-wrapped click
//     links, including the pixel just injected) are left untouched;
//   - the "mailto:" substring check is kept as a defensive second layer.
//
// The token embeds the FULL original URL (org|campaign|subscriber|email|url),
// so query params — including Everflow sub1/sub2 attribution tags — survive
// the redirect byte-for-byte (internal/tracking/handler.go HandleClick reads
// parts[4] and 307-redirects to it).
func RewriteClickLinks(html, campaignID, subscriberID, emailID, baseURL, orgID, secret string) string {
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	return linkRe.ReplaceAllStringFunc(html, func(match string) string {
		parts := linkRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		origURL := parts[1]
		// A tracking-layer offer link (baseURL + "/o/<sub>/<hash>/<campaign>")
		// is ALREADY a tracking URL, so wrapping it into /track/click would
		// double-wrap the redirect. Match ONLY our own tracking host + /o/
		// prefix — a bare Contains(origURL, "/o/") would also skip unrelated
		// content links whose path happens to contain an "/o/" segment,
		// silently dropping their click tracking on live sends.
		if strings.Contains(origURL, "/track/") || strings.HasPrefix(origURL, baseURL+"/o/") || strings.Contains(origURL, "mailto:") {
			return match
		}
		// Wave-2 tracking-layer offer minting (opt-in: SMARTLINK_HASH_EMIT=true,
		// wired at pool init via SetOfferHashEmitter). NETWORK-AGNOSTIC: the
		// emitter's inverse dictionary (normalize(offer_url_template) -> hash) is
		// the SOLE gate — no host regex precondition. When the emitter is ENABLED
		// and origURL's normalized form is a SEEDED destination on ANY offer
		// network (cratoolpro, eos57ytf, k8k0hfdt, codefortwo, kj3rwth8trk, muqes,
		// or any future network — the source of truth is the mailing_smart_links
		// rows, seeded from agents/scheduling/offers.py money hosts), emit the
		// /o/<sub>/<hash>/<campaign> tracking URL instead of the /track/click
		// wrap. Every miss falls through to the wrap UNCHANGED:
		//   - emitter nil or disabled  → short-circuits on Enabled(), so a
		//     flag-off deploy is byte-identical to pre-Wave-2 (the whole guard
		//     is skipped before any lookup runs);
		//   - newsletter / unsub / non-money link → never seeded → Lookup misses;
		//   - money link with no seeded hash       → Lookup misses → existing wrap.
		// A Lookup miss NEVER drops the link — control falls through to the wrap
		// below. The emitted URL is on baseURL+"/o/", which the skip rule above
		// catches on any re-processing, so it can never be double-wrapped.
		if offerHashEmitter.Enabled() {
			if hash, ok := offerHashEmitter.Lookup(origURL); ok {
				// Brand-in-path /o/ mint (2026-07-22): the shared builder bakes the
				// sending brand (baseURL's apex) as the FIRST path segment so the
				// tracking service can derive sub2 even though CloudFront strips the
				// viewer Host. Emitted URL still starts with baseURL+"/o/", so the
				// skip rule above catches it on any re-processing (no double-wrap).
				return `href="` + moneylink.OfferTrackingURL(baseURL, subscriberID, hash, campaignID) + `"`
			}
		}
		linkData := fmt.Sprintf("%s|%s", data, origURL)
		linkEncoded := base64.URLEncoding.EncodeToString([]byte(linkData))
		linkSig := TrackSign(linkEncoded, secret)
		return fmt.Sprintf(`href="%s/track/click/%s/%s"`, baseURL, linkEncoded, linkSig)
	})
}

func (p *SendWorkerPool) injectTrackingPixelAndLinks(html, campaignID, subscriberID, emailID, baseURL string) string {
	return InjectTrackingPixelAndLinks(html, campaignID, subscriberID, emailID, baseURL, p.orgID, p.trackingSecret)
}

// InjectOpenPixel adds ONLY the open-tracking pixel (top + bottom placement,
// same construction as InjectTrackingPixelAndLinks) with NO link rewriting.
// Used on the SES relay path where click rewrites must stay off (SES may
// wrap links itself; double-wrapping risks broken redirects) but our open
// pixel is required: SES-native open tracking captures <1% of opens
// (measured 2026-07-02 — ~1.3k/day PG-visible SES opens on ~230k/day SES
// sends vs ~41% pixel-tracked on PMTA lanes), which starved the engaged-tier
// segments of ALL SES-routed engagement (partner-drip, gmail, yahoo-family).
// Deliberately self-contained rather than refactoring the proven hot-path
// function. Kill switch: DISABLE_SES_OPEN_PIXEL=true.
func InjectOpenPixel(html, campaignID, subscriberID, emailID, baseURL, orgID, secret string) string {
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	sig := TrackSign(encoded, secret)
	pixelHTML := fmt.Sprintf(
		`<div style="display:none;font-size:1px;color:transparent;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;mso-hide:all;" aria-hidden="true"><img src="%s/track/open/%s/%s" width="1" height="1" border="0" alt="" /></div>`,
		baseURL, encoded, sig,
	)
	htmlLower := strings.ToLower(html)
	if bodyIdx := strings.Index(htmlLower, "<body"); bodyIdx >= 0 {
		if closeIdx := strings.Index(html[bodyIdx:], ">"); closeIdx >= 0 {
			insertAt := bodyIdx + closeIdx + 1
			html = html[:insertAt] + pixelHTML + html[insertAt:]
			htmlLower = strings.ToLower(html)
		}
	}
	if idx := strings.LastIndex(htmlLower, "</body>"); idx >= 0 {
		html = html[:idx] + pixelHTML + html[idx:]
	} else {
		html += pixelHTML
	}
	return html
}

// applySESTracking is the tracking-injection step for the SES relay path
// (via_ses=true): open pixel (restored 2026-07-02) + t.em click-wrapping
// (restored 2026-07-13 — SES declines to click-wrap PMTA-DKIM-signed mail,
// which is 15 of 16 brands, so relying on SES left money-link clicks
// invisible). Each half has its own kill switch so either can be reverted
// to the historical skip independently:
//   - DISABLE_SES_OPEN_PIXEL=true  → no open pixel (pre-jul02 behavior)
//   - DISABLE_SES_CLICK_WRAP=true  → no click-wrapping (pre-jul13 behavior)
//
// Order matters only cosmetically: the pixel is injected first, matching
// InjectTrackingPixelAndLinks; RewriteClickLinks cannot touch it (the pixel
// is an <img>, not an href, and carries /track/ anyway). Both flags set
// returns html unchanged. Called per message at claim/send time — nothing
// upstream caches wrapped output (snapshots store the pre-injection base
// creative), so an env flip applies uniformly from the next process start.
func applySESTracking(html, campaignID, subscriberID, emailID, baseURL, orgID, secret string) string {
	if os.Getenv("DISABLE_SES_OPEN_PIXEL") != "true" {
		html = InjectOpenPixel(html, campaignID, subscriberID, emailID, baseURL, orgID, secret)
	}
	if os.Getenv("DISABLE_SES_CLICK_WRAP") != "true" {
		html = RewriteClickLinks(html, campaignID, subscriberID, emailID, baseURL, orgID, secret)
	}
	return html
}

// GenerateUnsubscribeURL builds a signed global one-click unsubscribe URL.
// Emits a 3-part token (org|campaign|subscriber) that HandleTrackUnsubscribe
// interprets as "suppress globally, flip subscriber status".
func GenerateUnsubscribeURL(orgID, campaignID, subscriberID, baseURL, secret string) string {
	return generateUnsubURL(orgID, campaignID, subscriberID, "", baseURL, secret)
}

// GenerateBrandUnsubscribeURL builds a signed brand-scoped unsubscribe URL.
// Emits a 4-part token (org|campaign|subscriber|brandRoot) on the SAME
// /track/unsubscribe path as the global URL — HandleTrackUnsubscribe branches
// on the token part count. When brandRoot is empty this falls back to the
// global 3-part shape for safety.
func GenerateBrandUnsubscribeURL(orgID, campaignID, subscriberID, brandRoot, baseURL, secret string) string {
	return generateUnsubURL(orgID, campaignID, subscriberID, brandRoot, baseURL, secret)
}

func generateUnsubURL(orgID, campaignID, subscriberID, brandRoot, baseURL, secret string) string {
	data := fmt.Sprintf("%s|%s|%s", orgID, campaignID, subscriberID)
	if brandRoot != "" {
		data += "|" + brandRoot
	}
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	sig := TrackSign(encoded, secret)
	return fmt.Sprintf("%s/track/unsubscribe/%s/%s", baseURL, encoded, sig)
}

func (p *SendWorkerPool) generateUnsubscribeURL(campaignID, subscriberID, baseURL string) string {
	return GenerateUnsubscribeURL(p.orgID, campaignID, subscriberID, baseURL, p.trackingSecret)
}

func (p *SendWorkerPool) generateBrandUnsubscribeURL(campaignID, subscriberID, brandRoot, baseURL string) string {
	return GenerateBrandUnsubscribeURL(p.orgID, campaignID, subscriberID, brandRoot, baseURL, p.trackingSecret)
}

// buildUnsubscribeHeaders computes the signed global + brand-scoped unsubscribe
// URLs for a send and — unless disabled — writes the RFC 8058 one-click
// List-Unsubscribe / List-Unsubscribe-Post headers into the provided map.
//
// It is transport-agnostic ON PURPOSE: BOTH the Dedicated/PMTA path and the SES
// relay path call it, so every send carries the Gmail/Yahoo-mandated one-click
// header. (Regression fixed 2026-07-14: the header construction used to live
// only in the !ViaSES branch of processItem, so every SES-relayed send — gmail,
// apple, the yahoo-family NL ramp — shipped without List-Unsubscribe.)
//
// The returned URLs are used by the caller to resolve in-body {{ system.*_url }}
// tokens, so they are ALWAYS computed — even when the header kill switch is set
// — so a send never ships a literal, unresolved unsubscribe token.
//
// Kill switch: DISABLE_LIST_UNSUB_HEADERS=true skips only the header writes
// (restores the pre-fix behavior) while still returning resolved URLs.
func (p *SendWorkerPool) buildUnsubscribeHeaders(campaignID, subscriberID, brandRoot, fromEmail, trackBase string, headers map[string]string) (unsubURL, brandUnsubURL string) {
	// Delegates to the package-level shared helper (list_unsub_headers.go) so
	// EVERY send path — broadcast, click-drip, proofs — emits the identical
	// RFC 8058 header pair. Extracted 2026-07-21 (Google Postmaster one-click
	// compliance); behavior is byte-for-byte the 2026-07-14 fix.
	return BuildListUnsubscribeHeaders(p.orgID, campaignID, subscriberID, brandRoot, fromEmail, trackBase, p.trackingSecret, headers)
}

// buildRenderContext constructs a full Liquid render context from a queue item,
// matching the schema produced by mailing.ContextBuilder.BuildContext but built
// from data already loaded in the claim query (no extra DB round-trips).
// trackBase is the per-profile tracking URL (e.g. https://trk.em.discountblog.com)
// so that {{ system.unsubscribe_url }} uses the brand domain, not the platform domain.
func (p *SendWorkerPool) buildRenderContext(item QueueItem, trackBase string) mailing.RenderContext {
	rc := make(mailing.RenderContext)

	// Top-level profile fields
	rc["first_name"] = item.FirstName
	rc["last_name"] = item.LastName
	rc["email"] = item.Email
	rc["full_name"] = strings.TrimSpace(item.FirstName + " " + item.LastName)
	if parts := strings.SplitN(item.Email, "@", 2); len(parts) == 2 {
		rc["email_local"] = parts[0]
		rc["email_domain"] = parts[1]
	}

	// Custom fields
	if item.CustomFields != nil {
		rc["custom"] = map[string]interface{}(item.CustomFields)
	} else {
		rc["custom"] = make(map[string]interface{})
	}

	// Brand root for Everflow sub2={{ brand.domain }} on offer-center creatives;
	// brand.name is the display label for partner-offer disclosures.
	if item.BrandRoot != "" {
		rc["brand"] = map[string]interface{}{
			"domain": item.BrandRoot,
			"name":   brand.Label(item.BrandRoot),
		}
	}

	// Engagement
	rc["engagement"] = map[string]interface{}{
		"score":             item.EngagementScore,
		"total_emails":      item.TotalEmailsReceived,
		"total_opens":       item.TotalOpens,
		"total_clicks":      item.TotalClicks,
		"subscribed_at":     item.SubscribedAt,
		"optimal_send_hour": item.OptimalSendHourUTC,
	}

	// System fields
	now := time.Now()
	// Dispatch-time stamp for express lanes, where "we received your form and
	// mailed you right now" is the whole message (operator 2026-08-09,
	// internal_auto_insurance). ADDITIVE — every pre-existing key below keeps
	// its exact prior value and formatting.
	//
	// Rendered in DENVER, not the server's UTC. The task runs in UTC, so
	// current_hour is already UTC and a naive "current_time" built from it
	// would tell a Boise recipient their quotes were prepared six hours in
	// the future. LoadLocation can fail on a container without tzdata, so it
	// degrades to UTC with an explicit label rather than silently lying about
	// the zone.
	dispatchAt, zoneLabel := now, "UTC"
	if den, err := time.LoadLocation("America/Denver"); err == nil {
		dispatchAt = now.In(den)
		zoneLabel = dispatchAt.Format("MST")
	}
	system := map[string]interface{}{
		"current_date":    now.Format("January 2, 2006"),
		"current_year":    now.Year(),
		"current_month":   now.Month().String(),
		"current_day":     now.Day(),
		"current_weekday": now.Weekday().String(),
		"current_hour":    now.Hour(),
		"timestamp":       now.Unix(),
		// e.g. "3:04 PM MDT" / "August 9, 2026 at 3:04 PM MDT"
		"current_time":  dispatchAt.Format("3:04 PM ") + zoneLabel,
		"dispatch_date": dispatchAt.Format("January 2, 2006"),
		"dispatched_at": dispatchAt.Format("January 2, 2006") + " at " + dispatchAt.Format("3:04 PM ") + zoneLabel,
	}
	tBase := trackBase
	if tBase == "" {
		tBase = p.trackingURL
	}
	if tBase != "" {
		system["unsubscribe_url"] = p.generateUnsubscribeURL(item.CampaignID.String(), item.SubscriberID.String(), tBase)
		// Brand-scoped unsubscribe URL — suppresses only for the brand root
		// (e.g. *.discountblog.com) so the subscriber remains mailable for
		// other brands. Populated when item.BrandRoot is known (i.e. whenever
		// the sending domain matches an owned brand). When BrandRoot is empty
		// this falls back to the same shape as the global URL for safety —
		// callers that embed it in a template still get a working link.
		system["brand_unsubscribe_url"] = p.generateBrandUnsubscribeURL(item.CampaignID.String(), item.SubscriberID.String(), item.BrandRoot, tBase)
		system["preferences_url"] = fmt.Sprintf("%s/preferences?sid=%s", tBase, item.SubscriberID.String())
		system["view_in_browser_url"] = fmt.Sprintf("%s/view?cid=%s&sid=%s", tBase, item.CampaignID.String(), item.SubscriberID.String())
		// Scheme+host of the SENDING profile's tracking domain, no trailing
		// slash (operator 2026-09-06: the brand resolves from the sending
		// domain, never hardcoded). A creative builds its own tracking-layer
		// link as {{ system.tracking_base }}/o/{{ brand.domain }}/... so the
		// host follows the profile (t.em.<apex> on PMTA, t.m.<apex> on SES)
		// and RewriteClickLinks recognises the href as its own
		// (baseURL+"/o/") instead of double-wrapping it in /track/click.
		// Absent when no tracking base is configured.
		system["tracking_base"] = tBase
	}
	rc["system"] = system
	rc["now"] = now
	rc["today"] = now.Format("January 2, 2006")
	rc["year"] = now.Year()

	// Campaign metadata
	rc["campaign"] = map[string]interface{}{
		"id":           item.CampaignID.String(),
		"name":         item.CampaignName,
		"subject":      item.Subject,
		"preview_text": item.PreviewText,
		"from_name":    item.FromName,
		"from_email":   item.FromEmail,
	}
	rc["campaignId"] = item.CampaignID.String()
	rc["campaign_name"] = item.CampaignName

	// Subscriber metadata
	rc["subscriber"] = map[string]interface{}{
		"id":            item.SubscriberID.String(),
		"status":        item.SubscriberStatus,
		"source":        item.SubscriberSource,
		"timezone":      item.Timezone,
		"subscribed_at": item.SubscribedAt,
	}

	return rc
}

// personalizeContent replaces all merge-field variations with subscriber data.
// Handles {{ field }}, {{field}}, and [FIELD] syntaxes with case-insensitive matching.
// Kept as fallback; the primary path now uses the full Liquid TemplateService.
func personalizeContent(content, email, firstName, lastName string) string {
	if content == "" {
		return content
	}

	fullName := strings.TrimSpace(firstName + " " + lastName)
	emailLocal := email
	emailDomain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		emailLocal = email[:at]
		emailDomain = email[at+1:]
	}

	replacements := []struct {
		tags  []string
		value string
	}{
		{[]string{"{{ first_name }}", "{{first_name}}", "[FIRST_NAME]"}, firstName},
		{[]string{"{{ last_name }}", "{{last_name}}", "[LAST_NAME]"}, lastName},
		{[]string{"{{ full_name }}", "{{full_name}}", "[FULL_NAME]"}, fullName},
		{[]string{"{{ email }}", "{{email}}", "[EMAIL]", "{{EMAIL}}", "{{ EMAIL }}"}, email},
		{[]string{"{{ FIRST_NAME }}", "{{ LAST_NAME }}", "{{ FULL_NAME }}"}, ""},
		{[]string{"{{ email_local }}", "{{email_local}}"}, emailLocal},
		{[]string{"{{ email_domain }}", "{{email_domain}}"}, emailDomain},
	}
	// Apply the concrete-value replacements first
	replacements[4].tags = nil // skip the empty row
	for _, r := range replacements {
		for _, tag := range r.tags {
			if strings.Contains(content, tag) {
				content = strings.ReplaceAll(content, tag, r.value)
			}
		}
	}
	// Uppercase variants
	content = strings.ReplaceAll(content, "{{ FIRST_NAME }}", firstName)
	content = strings.ReplaceAll(content, "{{ LAST_NAME }}", lastName)
	content = strings.ReplaceAll(content, "{{ FULL_NAME }}", fullName)
	content = strings.ReplaceAll(content, "{{FIRST_NAME}}", firstName)
	content = strings.ReplaceAll(content, "{{LAST_NAME}}", lastName)
	content = strings.ReplaceAll(content, "{{FULL_NAME}}", fullName)

	return content
}
