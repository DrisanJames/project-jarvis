package api

// Campaign Manager — NEWSLETTERS mode (all 27 sending domains).
//
// WHAT THIS FILE IS FOR
// ---------------------
// The operator wants one scheduled newsletter send spanning every sending
// domain. The Warm-up flow (warmup_requests.go) is the pattern and the
// state-ownership protocol is REUSED wholesale — same table, same status
// vocabulary, same FOR UPDATE transition guard, same never-DELETE rule. What
// lives here is only what newsletters add:
//
//  1. THE CANONICAL CREATIVE SELECTION (the SEV-1 fix). Preview and sender
//     must resolve the SAME row, or the operator approves one creative and a
//     different one mails. Today they do not:
//       preview  internal/api/warmup_requests.go   filters filename ~ 'kumo-digest'
//       sender   agents/scheduling/kumo_warm.py:518-523  has NO filename filter,
//                so for em.discountblog.com it resolves to a live auto-insurance
//                CPC offer creative and a NEWSLETTER SEND SHIPS AN OFFER.
//     Both now go through newsletterCreativeWhere/Order below.
//
//  2. THE BYTE PIN. agents/jobs/kumo_newsletter_stage.py:88-100 UPDATEs
//     html_content on the SAME row id (the upsert is keyed on
//     (organization_id, filename)), so pinning creative_id does NOT pin what
//     ships. Preview at 09:00, build at 11:00 -> different articles under the
//     approved subject, with the audit trail unchanged. The request row
//     therefore carries creative_sha256 and the API refuses a drifted pin.
//
//  3. THE KIND DISCRIMINATOR. The live-slot unique index was
//     (organization_id, sending_domain, denver_day) with no discriminator, and
//     the 11 kumo domains are BOTH warm-up and newsletter domains — so the
//     second request for a domain+day raised a unique violation surfaced as a
//     bare 500. Re-cut below to include `kind`.
//
// HARD BOUNDARY — identical to warmup_requests.go: nothing here creates a
// campaign, enqueues, or sends. It records intent and reads Creative Studio.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

// =============================================================================
// Request kinds
// =============================================================================

// The `kind` column discriminates the two request programmes that share
// mailing_kumo_warmup_requests. THE TABLE IS DELIBERATELY NOT RENAMED:
// ALTER TABLE ... RENAME TO has no IF EXISTS form and is not one of the shapes
// migrationSkipProbe recognizes (cmd/server/migration_skip.go:40-46), so it
// would execute and ERROR on every boot after the first. The name is legacy;
// this column is the truth.
const (
	RequestKindKumoWarmup = "kumo_warmup"
	RequestKindNewsletter = "newsletter"
)

// campaignRequestKinds is a CLOSED allow-list. An unknown kind is a 400, never
// a silently-stored value that no builder polls for.
var campaignRequestKinds = map[string]bool{
	RequestKindKumoWarmup: true,
	RequestKindNewsletter: true,
}

// =============================================================================
// Producer stamps — the discriminator that stops an offer shipping as a newsletter
// =============================================================================

// NewsletterProducerStamps are the mailing_creatives.approved_by values written
// by the two newsletter producers. This is the ONLY safe discriminator:
//
//	approved_by  <- USE THIS. Real per-producer values on prod:
//	                'kumo_newsletter_stage' (the 11 kumo digests),
//	                'claude-2026-08-20-auto-cpc-golive' (24 CPC OFFER rows),
//	                'remarketing_seed_test', 'operator-...'.
//	source       <- USELESS. June newsletters are split across 'studio' AND
//	                'forge'; 'manual' holds 24 approved non-newsletters.
//	template_id  <- NULL on all 11.
//	forge_brand_key <- just mirrors the apex.
//	filename     <- FRAGILE. 'nl-<slug>-kumo-digest.html' is a naming
//	                convention with no constraint behind it, and it is exactly
//	                what made the preview and the sender diverge.
//
// 'legacy_newsletter_stage' is the stamp the Python builder writes for the 16
// legacy (PMTA/SES) brands. THAT EXACT STRING IS THE CONTRACT between the two
// sides — changing it here silently un-selects every legacy newsletter.
var NewsletterProducerStamps = []string{
	"kumo_newsletter_stage",
	"legacy_newsletter_stage",
}

// newsletterStaleAfter mirrors the wizard's WARMUP_STALE_MS. A creative older
// than this has almost certainly not been re-registered by today's stage run,
// so sending it re-sends yesterday's articles — the exact failure the
// daily-fresh programme exists to prevent (CLAUDE.md §13.1).
const newsletterStaleAfter = 24 * time.Hour

// =============================================================================
// Schema — FOUR SEPARATE MIGRATION ENTRIES, plus a scoped lock_timeout pair
// =============================================================================
//
// WHY SEPARATE: migrationSkipProbe classifies an entry by its LEADING keyword
// (cmd/server/migration_skip.go:40). A single string holding CREATE TABLE then
// CREATE INDEX is probed as CREATE TABLE, so once the table exists the probe
// skips the WHOLE entry and the index silently never lands.
//
// WHY THIS IS SAFE ON TODAY'S DB (RDS ~99% CPU, EBSIOBalance% ~0, and a no-op
// ADD COLUMN in this same slice sat 216s in a lock queue earlier today):
//   - mailing_kumo_warmup_requests is EMPTY (0 rows, 32 kB — verified in the
//     2026-08-20 data audit) and brand new. Nothing polls it, no worker reads
//     it, it has no dependent views, no triggers, no FKs.
//   - Both ALTERs are ADD COLUMN IF NOT EXISTS with a constant default, which
//     on PG 11+ is a catalog-only change: no table rewrite even if it were
//     populated.
//   - The index is built over ZERO tuples.
//   - The 216s incident was a lock QUEUE on a hot table (mailing_campaigns),
//     not statement work. The only lock contender here would be another writer
//     of this table, and there is none — which is exactly why this is the free
//     moment to re-cut the index.
//   - lock_timeout is nevertheless SET for these statements so that if that
//     reasoning is ever wrong, the DDL bails in 2s instead of barricading the
//     lock queue for the runner's full 5s statement budget.

// CampaignRequestLockTimeoutDDL bounds the lock wait for the entries that
// follow it. runStartupMigrations holds ONE dedicated connection for the whole
// slice (cmd/server/main.go, `conn, connErr := db.Conn(...)`, then
// `execSQL("SET statement_timeout = '5s'")`), so a SET here persists — which
// is why CampaignRequestLockTimeoutResetDDL must be registered after the last
// of these statements. Without the reset, every LATER migration in the slice
// would silently inherit a 2s lock budget.
const CampaignRequestLockTimeoutDDL = `SET lock_timeout = '2s'`

// CampaignRequestLockTimeoutResetDDL restores the connection default.
const CampaignRequestLockTimeoutResetDDL = `RESET lock_timeout`

// CampaignRequestKindDDL adds the mode discriminator. Single-action
// ADD COLUMN IF NOT EXISTS with no top-level comma, so reMigAddColumn
// (migration_skip.go:46) recognizes it and the probe skips it on every boot
// after the first — it takes a lock exactly once, ever.
const CampaignRequestKindDDL = `
ALTER TABLE mailing_kumo_warmup_requests
	ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'kumo_warmup'`

// CampaignRequestCreativeShaDDL pins the BYTES the operator approved.
// creative_id alone does not: kumo_newsletter_stage.py rewrites html_content
// in place on the same row id every morning.
const CampaignRequestCreativeShaDDL = `
ALTER TABLE mailing_kumo_warmup_requests
	ADD COLUMN IF NOT EXISTS creative_sha256 TEXT`

// CampaignRequestDropKindBlindSlotDDL retires the kind-blind live slot that
// KumoWarmupRequestsLiveSlotDDL used to create. It is NOT one of the shapes
// migrationSkipProbe recognizes (there is no DROP INDEX pattern —
// migration_skip.go:40-46), so it re-executes on every boot. That is
// deliberate and free: DROP INDEX IF EXISTS on an absent index is a catalog
// lookup that takes no table lock and no measurable time.
//
// ORDERING CONTRACT: this must be registered BEFORE the entry that creates
// idx_campaign_requests_live_slot_v2 (KumoWarmupRequestsLiveSlotDDL, re-cut in
// warmup_requests.go), and the `kind` column must exist before that index can
// reference it. See the registration block in cmd/server/main.go.
const CampaignRequestDropKindBlindSlotDDL = `DROP INDEX IF EXISTS idx_kumo_warmup_requests_live_slot`

// =============================================================================
// THE CANONICAL CREATIVE SELECTION
// =============================================================================
//
// One predicate, one total order, composed into both the single-domain and the
// all-domains query so the two CANNOT drift. Do not inline either query
// elsewhere; call SelectNewsletterCreative / selectNewsletterCreatives.
//
// PYTHON SENDER — CONVERGENCE REQUIRED (filed, not done here; the Python
// builder owns that file this session): agents/scheduling/kumo_warm.py:518-523
// must be repointed at this exact shape. Today it runs
//
//	SELECT subject, html_content FROM mailing_creatives
//	WHERE upper(brand_code) = upper(%s) AND approval_status = 'approved'
//	  AND COALESCE(html_content,'') <> ''
//	ORDER BY updated_at DESC, generated_at DESC LIMIT 1
//
// which differs from this in four ways that each change WHICH ROW IS SENT:
// no approved_by producer gate (an offer creative wins), no organization_id
// filter, no offer_key guard, and no id DESC tie-break.

// newsletterCreativeCols — html_content is returned as `html`; the sha is
// computed LIVE from the bytes rather than read from mailing_creatives.sha256,
// so it can never disagree with what would actually mail.
const newsletterCreativeCols = `
		id,
		brand_code,
		filename,
		COALESCE(subject, '')                                        AS subject,
		COALESCE(preheader, '')                                      AS preheader,
		COALESCE(html_content, '')                                   AS html,
		updated_at,
		generated_at,
		approval_status,
		COALESCE(approved_by, '')                                    AS approved_by,
		length(html_content)::bigint                                 AS html_length,
		encode(sha256(convert_to(html_content, 'UTF8')), 'hex')      AS live_sha256`

// newsletterCreativeWhere — $1 organization_id, $2 text[] of lowercase brand
// keys (empty array = every brand), $3 text[] of producer stamps, $4 an
// OPTIONAL extra filename regex (empty string = no extra narrowing; used only
// by the legacy brand_slug lookup, never by the newsletter contract).
//
// brand_code carries the APEX for newsletter rows (bestcreditcare.com) but the
// SENDING domain for the CPC offer rows (em.discountblog.com) — matching
// lowercase on the apex plus the producer gate keeps those apart.
const newsletterCreativeWhere = `
		organization_id = $1
		AND (array_length($2::text[], 1) IS NULL OR lower(brand_code) = ANY($2::text[]))
		AND approval_status = 'approved'
		AND approved_by = ANY($3::text[])
		AND COALESCE(html_content, '') <> ''
		AND COALESCE(offer_key, '') = ''
		AND ($4 = '' OR filename ~ $4)`

// newsletterCreativeOrder is a TOTAL order. id DESC is load-bearing, not
// cosmetic: all 11 kumo digest rows carry an identical updated_at, so an
// order without a unique final key lets preview and sender pick different
// rows from the same tie — the divergence this whole file exists to close.
const newsletterCreativeOrder = `updated_at DESC, generated_at DESC, id DESC`

// CanonicalNewsletterCreativeSQL selects ONE creative. Exported so the shape
// is quotable by the Python sender (see the convergence note above).
var CanonicalNewsletterCreativeSQL = `
	SELECT` + newsletterCreativeCols + `
	FROM mailing_creatives
	WHERE` + newsletterCreativeWhere + `
	ORDER BY ` + newsletterCreativeOrder + `
	LIMIT 1`

// CanonicalNewsletterCreativeBatchSQL selects the winning creative for EVERY
// brand key in one pass. DISTINCT ON + the identical predicate and tie-break,
// so a batch read and a single read can never disagree.
//
// One scan of mailing_creatives (167 rows / 2400 kB) instead of 27. Do NOT add
// an index for this — a seq scan of that table is correct and free, and index
// DDL against today's DB is the expensive thing.
var CanonicalNewsletterCreativeBatchSQL = `
	SELECT DISTINCT ON (lower(brand_code))` + newsletterCreativeCols + `
	FROM mailing_creatives
	WHERE` + newsletterCreativeWhere + `
	ORDER BY lower(brand_code), ` + newsletterCreativeOrder

// NewsletterCreative is the approved creative a newsletter request pins.
type NewsletterCreative struct {
	CreativeID     string
	BrandCode      string
	Filename       string
	Subject        string
	Preheader      string
	HTML           string
	UpdatedAt      time.Time
	GeneratedAt    time.Time
	ApprovalStatus string
	ApprovedBy     string
	HTMLLength     int64
	SHA256         string
}

type newsletterCreativeScanner interface {
	Scan(dest ...interface{}) error
}

func scanNewsletterCreative(sc newsletterCreativeScanner) (*NewsletterCreative, error) {
	var c NewsletterCreative
	if err := sc.Scan(&c.CreativeID, &c.BrandCode, &c.Filename, &c.Subject, &c.Preheader,
		&c.HTML, &c.UpdatedAt, &c.GeneratedAt, &c.ApprovalStatus, &c.ApprovedBy,
		&c.HTMLLength, &c.SHA256); err != nil {
		return nil, err
	}
	return &c, nil
}

// SelectNewsletterCreative resolves the single creative that a newsletter (or
// warm-up) send for brandKey would ship. brandKey is the lowercase apex;
// filenameRe is an optional extra narrowing and should be "" for anything on
// the newsletter contract. Returns (nil, nil) when there is no approved,
// producer-stamped creative — an ABSENCE, never a fabricated row.
func SelectNewsletterCreative(ctx context.Context, db *sql.DB, orgID, brandKey, filenameRe string) (*NewsletterCreative, error) {
	keys := []string{}
	if k := strings.ToLower(strings.TrimSpace(brandKey)); k != "" {
		keys = append(keys, k)
	}
	// An empty brand key AND an empty filename regex would select the newest
	// approved newsletter of ANY brand — i.e. mail one property's creative
	// under another's from-name. Refuse rather than return a plausible row.
	if len(keys) == 0 && strings.TrimSpace(filenameRe) == "" {
		return nil, fmt.Errorf("newsletter creative lookup needs a brand key or a filename pattern")
	}
	row := db.QueryRowContext(ctx, CanonicalNewsletterCreativeSQL,
		orgID, pq.Array(keys), pq.Array(NewsletterProducerStamps), filenameRe)
	c, err := scanNewsletterCreative(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// selectNewsletterCreatives resolves the winning creative for many brand keys
// in one scan, keyed by lowercase brand_code. Keys with no approved creative
// are simply absent from the map — the caller renders them as "missing".
func selectNewsletterCreatives(ctx context.Context, db *sql.DB, orgID string, brandKeys []string) (map[string]*NewsletterCreative, error) {
	keys := make([]string, 0, len(brandKeys))
	for _, k := range brandKeys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			keys = append(keys, k)
		}
	}
	out := map[string]*NewsletterCreative{}
	rows, err := db.QueryContext(ctx, CanonicalNewsletterCreativeBatchSQL,
		orgID, pq.Array(keys), pq.Array(NewsletterProducerStamps), "")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, sErr := scanNewsletterCreative(rows)
		if sErr != nil {
			return nil, sErr
		}
		out[strings.ToLower(c.BrandCode)] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// =============================================================================
// The sha drift refusal
// =============================================================================

// NewsletterShaMismatchReason is THE operator-facing sentence for a creative
// that was legitimately refreshed between preview and submit. It is a single
// exported function so the 409 body, the request row's drift flag, and the
// build_note the Python builder writes all say the same thing — a drift must
// never read as a silent drop or a generic 500.
//
// Both shas are truncated to 12 hex chars: enough to be quotable in Slack,
// short enough to read.
func NewsletterShaMismatchReason(sendingDomain, approvedSHA, liveSHA string, liveUpdatedAt time.Time) string {
	return fmt.Sprintf(
		"the newsletter creative for %s changed after it was approved "+
			"(approved bytes %s, live bytes %s, creative last refreshed %s). "+
			"This is what a normal daily refresh looks like — it is refused, not sent, "+
			"because the articles that would ship are not the ones that were reviewed. "+
			"Re-open the preview, approve today's creative, and resubmit.",
		sendingDomain, shortSHA(approvedSHA), shortSHA(liveSHA),
		liveUpdatedAt.UTC().Format(time.RFC3339))
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(none)"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// newsletterBrandSlug derives the property slug. The registered daily creative
// is nl-<slug>-<something>.html, which is the only DB-resident apex->slug
// mapping (there is no mechanical rule: bestcreditcare.com -> bcc,
// firsttimebuyerhomeloan.com -> fth). Falls back to the apex's first label,
// which is honest and stable but is NOT the estate short code.
func newsletterBrandSlug(filename, apex string) string {
	if strings.HasPrefix(filename, "nl-") {
		if parts := strings.Split(filename, "-"); len(parts) >= 2 && reWarmupSlug.MatchString(parts[1]) {
			return parts[1]
		}
	}
	return strings.Split(apex, ".")[0]
}

// =============================================================================
// GET /api/mailing/pmta-campaign/newsletter/preview
// =============================================================================

// newsletterPreviewDomain is the FIXED response contract. Field names are
// coordinated with the wizard build and must not be renamed casually.
type newsletterPreviewDomain struct {
	SendingDomain string `json:"sending_domain"`
	Apex          string `json:"apex"`
	BrandSlug     string `json:"brand_slug"`
	// from_name / from_email come from mailing_sending_profiles — the DOMAIN,
	// never the creative row. DKIM alignment: kumo_warm.py:513/:530 forces the
	// same thing (the 2026-08-02 guard).
	FromName  string `json:"from_name"`
	FromEmail string `json:"from_email"`

	CreativeID string `json:"creative_id"`
	// CreativeSHA256 is what the operator is approving. It is computed live
	// from html_content and is the value the request row must carry.
	CreativeSHA256 string `json:"creative_sha256"`
	Filename       string `json:"filename"`
	Subject        string `json:"subject"`
	Preheader      string `json:"preheader"`
	// UpdatedAt IS THE FRESHNESS FIELD; generated_at is frozen at first insert
	// and reads days stale on a creative refreshed this morning. Nil when the
	// creative is missing — never a zero timestamp dressed up as data.
	UpdatedAt      *time.Time `json:"updated_at"`
	ApprovalStatus string     `json:"approval_status"`
	// HTML is present only when include_html=1.
	HTML string `json:"html,omitempty"`

	// Status: ready | missing | stale. Reason carries an operator-facing
	// sentence whenever Status != ready.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

const (
	newsletterStatusReady   = "ready"
	newsletterStatusMissing = "missing"
	newsletterStatusStale   = "stale"
)

// newsletterPreviewProfilesSQL — every eligible sending domain.
//
// DISTINCT ON is a guard, not decoration: mailing_sending_profiles.sending_domain
// is NOT unique (m.discountblog.com alone carries four active rows with four
// different from_names), so an unordered read can hand back a non-deterministic
// friendly-from. Restricting to em.<apex> is what makes it one row per property
// (verified: exactly 27, zero duplicates), and DISTINCT ON pins the choice if
// that ever stops being true.
const newsletterPreviewProfilesSQL = `
	SELECT DISTINCT ON (sending_domain)
	       sending_domain,
	       COALESCE(from_name, ''),
	       COALESCE(from_email, '')
	FROM mailing_sending_profiles
	WHERE organization_id = $1
	  AND status = 'active'
	  AND sending_domain LIKE 'em.%'
	  AND ($2 = '' OR sending_domain = $2)
	ORDER BY sending_domain, COALESCE(is_default, FALSE) DESC, created_at DESC`

// HandleNewsletterPreview answers "exactly what will send, per domain, today".
//
//	GET /api/mailing/pmta-campaign/newsletter/preview?include_html=0|1[&sending_domain=...]
//
// Omitting sending_domain returns ALL eligible sending domains — that is the
// default the wizard uses, because a newsletter day is one scheduled time
// across every domain.
//
// A domain with no approved creative is returned with status "missing" and a
// reason. It is NEVER omitted and NEVER fabricated: the operator is auditing
// what will send, so an absent creative has to be visibly absent.
func (s *PMTACampaignService) HandleNewsletterPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// getOrgID wraps GetOrgIDFromRequest (handlers_pmta.go:1339) and adds the
	// documented fallback chain; it is what every sibling handler on this
	// service uses.
	orgID := getOrgID(r)

	includeHTML := newsletterTruthy(r.URL.Query().Get("include_html"))
	only := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sending_domain")))

	rows, err := s.db.QueryContext(ctx, newsletterPreviewProfilesSQL, orgID, only)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "sending profile query failed")
		return
	}
	defer rows.Close()

	out := []newsletterPreviewDomain{}
	apexes := []string{}
	for rows.Next() {
		var d newsletterPreviewDomain
		if err := rows.Scan(&d.SendingDomain, &d.FromName, &d.FromEmail); err != nil {
			respondError(w, http.StatusInternalServerError, "sending profile scan failed")
			return
		}
		d.Apex = kumoApexFromSendingDomain(d.SendingDomain)
		apexes = append(apexes, d.Apex)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "sending profile query failed")
		return
	}

	creatives := map[string]*NewsletterCreative{}
	if len(apexes) > 0 {
		creatives, err = selectNewsletterCreatives(ctx, s.db, orgID, apexes)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "creative lookup failed")
			return
		}
	}

	now := time.Now()
	var ready, missing, stale int
	for i := range out {
		d := &out[i]
		c := creatives[d.Apex]
		if c == nil {
			d.Status = newsletterStatusMissing
			d.BrandSlug = newsletterBrandSlug("", d.Apex)
			d.Reason = "no approved newsletter creative is registered for " + d.Apex +
				" — the daily stage has not published one, or it is not approved. " +
				"This domain will NOT be included in the send until Creative Studio holds an " +
				"approved creative stamped by a newsletter producer (approved_by in " +
				strings.Join(NewsletterProducerStamps, ", ") + ")."
			missing++
			continue
		}
		d.CreativeID = c.CreativeID
		d.CreativeSHA256 = c.SHA256
		d.Filename = c.Filename
		d.Subject = c.Subject
		d.Preheader = c.Preheader
		upd := c.UpdatedAt
		d.UpdatedAt = &upd
		d.ApprovalStatus = c.ApprovalStatus
		d.BrandSlug = newsletterBrandSlug(c.Filename, d.Apex)
		if includeHTML {
			d.HTML = c.HTML
		}
		if age := now.Sub(c.UpdatedAt); age > newsletterStaleAfter {
			d.Status = newsletterStatusStale
			d.Reason = fmt.Sprintf(
				"this creative was last refreshed %s ago (%s). The daily stage has not "+
					"re-registered it, so sending now re-sends the same articles. Run the "+
					"daily newsletter stage for %s before scheduling.",
				age.Round(time.Hour), c.UpdatedAt.UTC().Format(time.RFC3339), d.Apex)
			stale++
			continue
		}
		d.Status = newsletterStatusReady
		ready++
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"day":             time.Now().In(propertyLedgerLoc).Format("2006-01-02"),
		"domains":         out,
		"count":           len(out),
		"ready":           ready,
		"missing":         missing,
		"stale":           stale,
		"include_html":    includeHTML,
		"producer_stamps": NewsletterProducerStamps,
		"stale_after":     newsletterStaleAfter.String(),
	})
}

// newsletterTruthy accepts the query-string spellings a client may send.
// Absent or "0" is false; the wizard sends include_html=1.
func newsletterTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}
