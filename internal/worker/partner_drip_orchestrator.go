package worker

// PartnerDripOrchestrator schedules 15-minute mini-campaigns from the
// partner_clean_queue. Each tick:
//   1. For each active vertical, compute wave_size from queue depth and
//      the dataset's flush_window_hours.
//   2. Round-robin pick a brand from BRANDS, skipping paused brands.
//   3. Resolve creative for (vertical, brand) from partner_drip_creatives.
//   4. Apply ISP-rate-limit deferral and per-dataset distribution overrides.
//   5. Atomically claim N FIFO records (status: ready -> claimed).
//   6. Promote claimed records to mailing_subscribers with full provenance
//      columns populated (the data_pipeline.go bug regression: that worker
//      forgot to populate source_system/source_detail/source_metadata).
//   7. Create a per-wave static segment containing exactly the claimed
//      record IDs so the existing audience finalizer picks them up.
//   8. Build a PMTACampaignInput mirroring deploy_may12_mature.py and call
//      DeployFn (in-process — no HTTP self-call).
//   9. Mark records mailed with the resulting campaign_id.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// Brands in deterministic round-robin order. The first 4 are the mature
// brands launched in 2026-04 (db/ht/mh/qf) — they have full per-ISP IP
// pool isolation and the operator's highest cap budgets. The remaining 12
// were brought online in 2026-05-08 / 05-09 / 05-09b and use general-pool
// IPs. Both tiers ship the same partner-drip creatives — the per-ISP cap
// table below is what controls cumulative volume to each ISP per wave.
//
// All 16 brands have:
//   - mailing_sending_profiles row (PMTA, status=active)
//   - per-brand newsletter creatives in docs/emails/ for both the welcome
//     family (amerisave/personal-loans/optima-tax-relief/renewal-by-andersen)
//     and the follow-up families (trugreen + the-capital-wallet)
//   - image-CDN domain entry in mailing_image_domains (verified)
//
// 2026-05-14 expansion ("expand our ingestion system across ALL properties"):
// adds the 12 new-brand codes after the 4 mature codes so the round-robin
// kicks off mature-first and only spills into newer-brand IPs once those
// come up in the rotation. Per-ISP caps still cap cumulative volume.
var dripBrands = []string{
	// Mature 4 — full per-ISP pool isolation since 2026-04.
	"db", "ht", "mh", "qf",
	// New-brand expansion 2026-05-14 — general-pool IPs, gentler caps.
	"bwp", "ci", "cp", "fc", "hws", "lpl", "mrd", "rb", "rru", "tot", "wfy", "yih",
}

// verticalBrandRoster overrides the default dripBrands rotation for specific
// verticals. samsclub_internal warms the new KumoMTA Colo1 domains
// (mpf/pmd/trb, fresh OVH IPs 51.81.135.220-222) by sending the Sam's Club
// digest ONLY from those three; the mature brands are freed from this vertical.
// These brands are intentionally absent from the global dripBrands slice so
// they send for NO other vertical (welcome AND follow-up). 2026-06-17 warm-up.
// 2026-06-22: KumoMTA Colo1 brands moved to scheduling-only (operator: "no
// longer use automations and ai for sending"). The samsclub_internal roster is
// removed so NO vertical auto-mails mpf/pmd/trb or the 5 new ISP-pooled brands
// (bcc/usf/yfb/hlj/fth) — they're absent from global dripBrands AND have no
// dedicated roster. Kumo sends are now driven only via the scheduler/send-day.
var verticalBrandRoster = map[string][]string{}

// governedBrands is the set of Kumo properties (em.<apex> on the 16.217.96.0/24
// per-ISP pools) governed by partner_property_governor. They are intentionally
// ABSENT from dripBrands (so no vertical auto-mails them by default) AND from
// verticalBrandRoster (so they are not warm-up-roster brands) AND from
// brandRosterFor's output (so the welcome pass can never select them). They ride
// a SEPARATE additive pass, tickGoverned (Option B), off their own
// '<vertical>:governed' rotation state. Every wave they ship is clamped by
// applyPropertyGovernor (per-(property,vertical,ISP) daily cap, gmail held,
// 6h-paced per-wave cap, floor gate). Membership is structural (code); the
// numbers and subscriptions are operator-settable in the table without a deploy.
//
// WELCOME-ONLY guarantee (enforced two ways): (1) absent from dripBrands, so
// pickNextFollowupBrand can never select them (no follow-ups); (2) absent from
// brandRosterFor's output, so the welcome pass can never select them. They fire
// ONLY in tickGoverned, which produces welcome (status='ready') waves.
var governedBrands = map[string]bool{
	"mpf": true, "pmd": true, "trb": true, "bcc": true, "usf": true,
	"yfb": true, "hlj": true, "fth": true, "htm": true,
}

// governedBrandsList is the deterministic rotation order for governed brands,
// walked by tickGoverned for a vertical's subscribed governed subset. Stable,
// append-only.
var governedBrandsList = []string{
	"mpf", "pmd", "trb", "bcc", "usf", "yfb", "hlj", "fth", "htm",
}

// brandRosterFor returns the brand rotation a vertical should walk: its
// dedicated roster if one exists, else the shared dripBrands.
//
// Governed (Kumo) brands are NEVER appended here. They ride a separate additive
// pass (tickGoverned) off their own '<vertical>:governed' rotation state — see
// Option B. Keeping this function byte-identical to its pre-P1 form guarantees
// the welcome + follow-up passes for the 16 normal brands are unchanged (zero
// side effects on existing brands).
func brandRosterFor(vertical string) []string {
	if r, ok := dynamicRosterFor(vertical); ok {
		return r
	}
	if r, ok := verticalBrandRoster[strings.ToLower(strings.TrimSpace(vertical))]; ok {
		return r
	}
	return dripBrands
}

// dynamicRosterFor returns the DB roster overlay for a vertical, if one is
// configured (partner_drip_vertical_roster). Both the welcome pass (via
// brandRosterFor) and the follow-up pass (pickNextFollowupBrand) consult it, so
// a DB lane gets the FULL multi-touch ladder, not just touch 1.
func dynamicRosterFor(vertical string) ([]string, bool) {
	lv := strings.ToLower(strings.TrimSpace(vertical))
	dynamicRosterMu.RLock()
	r, ok := dynamicRoster[lv]
	dynamicRosterMu.RUnlock()
	if ok && len(r) > 0 {
		return r, true
	}
	return nil, false
}

// dynamicRoster is the DB overlay on the compiled rosters, refreshed once per
// tick from partner_drip_vertical_roster. Deliberately NOT folded into
// warmupRosterBrands: that set is derived from the compiled verticalBrandRoster
// at init, so a DB lane never inherits the warm-up yahoo/aol-only ISP clamp.
var (
	dynamicRosterMu sync.RWMutex
	dynamicRoster   = map[string][]string{}
)

// setDynamicRoster swaps the roster overlay atomically.
func setDynamicRoster(m map[string][]string) {
	dynamicRosterMu.Lock()
	dynamicRoster = m
	dynamicRosterMu.Unlock()
}

// orderedSubscribedGoverned returns the governed brands subscribed to `vertical`
// in governedBrandsList order. This is the governed pass's rotation roster
// (distinct from brandRosterFor, which never includes governed brands).
func (po *PartnerDripOrchestrator) orderedSubscribedGoverned(vertical string) []string {
	subscribed := po.governedBrandsSubscribedTo(vertical)
	if len(subscribed) == 0 {
		return nil
	}
	out := make([]string, 0, len(subscribed))
	for _, b := range governedBrandsList {
		if subscribed[b] {
			out = append(out, b)
		}
	}
	return out
}

// governedBrandsSubscribedTo returns the set of governed brand codes whose
// governor row (active) lists `vertical` in subscribed_verticals. Reads the
// per-tick governor cache; if the cache is unloaded/empty (load error or no
// governors), returns an empty set so no governed brand joins any rotation
// (fail-safe — see loadGovernors).
func (po *PartnerDripOrchestrator) governedBrandsSubscribedTo(vertical string) map[string]bool {
	lv := strings.ToLower(strings.TrimSpace(vertical))
	po.governorMu.RLock()
	defer po.governorMu.RUnlock()
	if len(po.governorCache) == 0 {
		return nil
	}
	out := map[string]bool{}
	for code, g := range po.governorCache {
		if !g.active {
			continue
		}
		if g.subscribed[lv] {
			out[code] = true
		}
	}
	return out
}

// warmupRosterBrands is the set of every brand that appears in a
// verticalBrandRoster — fresh-IP warm-up brands. They are confined both to their
// vertical (via the roster) AND to that warm-up's ISP set: see processVertical,
// where they bypass the mature-brand allow-list and ship yahoo/aol only.
var warmupRosterBrands = func() map[string]bool {
	m := map[string]bool{}
	for _, brands := range verticalBrandRoster {
		for _, b := range brands {
			m[strings.ToLower(strings.TrimSpace(b))] = true
		}
	}
	return m
}()

// keepOnlyISPCaps returns a copy of caps with every ISP except the allowed set
// forced to 0, confining a wave to a fixed ISP set.
func keepOnlyISPCaps(caps map[string]int, allowed ...string) map[string]int {
	keep := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		keep[strings.ToLower(strings.TrimSpace(a))] = true
	}
	out := cloneISPCapMap(caps)
	for isp := range out {
		if !keep[strings.ToLower(strings.TrimSpace(isp))] {
			out[isp] = 0
		}
	}
	return out
}

// applyYahooNewsletterFollowupCaps enforces the yahoo-newsletter-only drip lane
// (operator 2026-07-14) on a follow-up wave's per-ISP caps. Two mirror-image
// branches guarantee yahoo offers and yahoo newsletters can never both fire:
//
//   - yahooNewsletter=true  → confine the claim to yahoo ONLY (the caller serves
//     the brand newsletter to those records).
//   - offer pass, flag on   → remove yahoo from the offer claim entirely.
//   - flag off               → return caps unchanged (exact legacy behavior; the
//     yahoo-newsletter pass is never invoked in this state either).
//
// The flag defaulting false makes this a no-op kill switch: the offer path is
// byte-identical to before when PARTNER_DRIP_YAHOO_NEWSLETTER_ONLY is unset.
func applyYahooNewsletterFollowupCaps(caps map[string]int, flagOn, yahooNewsletter bool) map[string]int {
	if yahooNewsletter {
		return keepOnlyISPCaps(caps, "yahoo")
	}
	if !flagOn {
		return caps
	}
	out := cloneISPCapMap(caps)
	out["yahoo"] = 0
	return out
}

var brandSendingDomain = map[string]string{
	// Mature 4
	"db": "em.discountblog.com",
	"ht": "em.historythinking.com",
	"mh": "em.myownhealth.net",
	"qf": "em.quizfiesta.com",
	// New-brand expansion 2026-05-14
	"bwp": "em.businessweeklypro.com",
	"ci":  "em.casainsure.com",
	"cp":  "em.consumerpro.net",
	"fc":  "em.financialcalculate.com",
	"hws": "em.homewarrantyservices.org",
	"lpl": "em.learnpersonalloans.com",
	"mrd": "em.myrepairdiy.com",
	"rb":  "em.ratesbazar.com",
	"rru": "em.refinanceratesusa.com",
	"tot": "em.thingoftheday.org",
	"wfy": "em.warrantyforyou.com",
	"yih": "em.yourinsurancehub.com",
	// KumoMTA Colo1 warm-up 2026-06-17 (fresh OVH IPs .220/.221/.222). NOT in
	// dripBrands — routed only via verticalBrandRoster[samsclub_internal].
	"mpf": "em.mypersonalfinancial.com",
	"pmd": "em.paymydebit.com",
	"trb": "em.theretirementblog.com",
	// KumoMTA Colo1 ISP-pool expansion 2026-06 (per-ISP IPs on 16.217.96.0/24).
	// These 6 had governor rows + active kumo profiles but were missing here, so
	// pausedBrandFn (no sending domain => paused) skipped them and only 3 of 9
	// governed brands warmed. Added so all 9 resolve a sending domain and send.
	"bcc": "em.bestcreditcare.com",
	"usf": "em.us-finance.com",
	"yfb": "em.yourfinancialblog.com",
	"hlj": "em.homeloansbyjaime.com",
	"fth": "em.firsttimebuyerhomeloan.com",
	"htm": "em.hometracmortgage.com",
}

// BrandSendingDomain exposes the orchestrator's brand → sending-domain
// mapping to other packages (notably cmd/server/main.go's
// PausedBrandPredicate wiring) so callers stay in sync with the canonical
// dripBrands list. Returns ("", false) for unknown brands. Adding a brand
// to the orchestrator means updating brandSendingDomain in this file
// only — no cross-package map duplication.
func BrandSendingDomain(brand string) (string, bool) {
	return resolveBrandSendingDomain(brand)
}

// DripIntroBrands exposes the canonical 16-code drip roster (dripBrands) to
// other packages — notably the Property Ledger API's brand validation
// (internal/api/property_ledger.go) and the intro-counter grid — so callers
// stay in sync with the orchestrator's roster instead of duplicating it.
// Returns a copy; governed (Kumo) brands are intentionally absent (they are
// excluded from the intro-budget ledger — see partner_drip_brand_budgets.go).
func DripIntroBrands() []string {
	out := make([]string, len(dripBrands))
	copy(out, dripBrands)
	return out
}

// dynamicBrandDomain is the DB overlay on brandSendingDomain, refreshed once
// per tick from mailing_brand_metadata (brand_code -> sending_domain). It lets
// an operator add a NEW drip lane/brand with no deploy. Empty = static map only.
var (
	dynamicBrandDomainMu sync.RWMutex
	dynamicBrandDomain   = map[string]string{}
)

// setDynamicBrandDomains swaps the overlay atomically. An empty/nil map clears
// it, so a failed load falls back to the compiled map rather than breaking sends.
func setDynamicBrandDomains(m map[string]string) {
	dynamicBrandDomainMu.Lock()
	dynamicBrandDomain = m
	dynamicBrandDomainMu.Unlock()
}

// resolveBrandSendingDomain prefers the DB overlay, then the compiled map.
func resolveBrandSendingDomain(brand string) (string, bool) {
	b := strings.ToLower(strings.TrimSpace(brand))
	dynamicBrandDomainMu.RLock()
	d, ok := dynamicBrandDomain[b]
	dynamicBrandDomainMu.RUnlock()
	if ok && strings.TrimSpace(d) != "" {
		return d, true
	}
	d, ok = brandSendingDomain[b]
	return d, ok
}

// followupTouchGapHours is the minimum gap between consecutive touches.
// Operator directive 2026-05-14: "campaigns should run daily" — 24h is
// the most defensible cadence (matches inbox-fatigue research and the
// industry's three-touch nurture cycle).
const followupTouchGapHours = 24

// MaxTouchCount is the terminal touch number. After this many touches a
// recipient is permanently retired from the drip rotation regardless of
// engagement state.
//
// 2026-07-08: raised 4→5 to support a per-touch-offer ladder (one vertical
// mailing up to 5 different offers, one per touch). Verticals that configure
// FEWER than MaxTouchCount touches are NOT broken by the higher ceiling: when a
// record advances to a touch that has no configured creative row for the
// vertical (no brand-specific row AND no vertical=NULL global row), the
// follow-up pass retires it as terminal instead of looping (see processFollowup
// + followupTouchConfigured).
const MaxTouchCount = 5

// dataPartnerMasterListID is seeded by startup migration dp_seed_master_list.
const dataPartnerMasterListID = "00000000-0000-0000-0000-0000d4ada4a7"

// CampaignDeployFn is the in-process call signature for HandleDeployCampaign.
// We accept a func so the worker package doesn't need to import internal/api.
type CampaignDeployFn func(ctx context.Context, input engine.PMTACampaignInput) (campaignID string, err error)

// PartnerDripOrchestratorConfig holds runtime knobs.
type PartnerDripOrchestratorConfig struct {
	OrganizationID       string
	TickInterval         time.Duration // default 15 minutes
	MinWaveSize          int           // default 25
	MaxWaveSize          int           // default 5000
	WindowHours          int           // PMTA wave window in hours (default 8 — bypass the source-field sanity check)
	CreativesDir         string        // path to docs/emails (defaults to "docs/emails")
	DeployFn             CampaignDeployFn
	PausedBrandPredicate func(ctx context.Context, brand string) bool // optional — return true to skip brand
	// PerISPCapPerWave caps how many records per ISP family one wave may
	// claim. Protects ISPs from cumulative drip + Welcome + Engager volume
	// overshooting the published caps (Gmail 5000/brand/day, Yahoo 500/brand/day).
	// Default mirrors the conservative drip allotment: gmail=150, yahoo=20, other=100.
	PerISPCapPerWave map[string]int
	// PerISPDrainDays stretches selected ISPs over N calendar days of waves
	// instead of the default 24h drain. Caps are recomputed each tick from
	// live queue depth: cap = min(PerISPCapPerWave, ceil(ready / (wavesPerVerticalPerDay * N))).
	// ISPs omitted from this map keep the static PerISPCapPerWave ceiling only.
	PerISPDrainDays map[string]int
	// NewRecordDailyISPCaps bounds how many NEW records (first touch) one
	// brand may mail per ISP per calendar day (America/Denver). Applied on
	// top of PerISPCapPerWave at claim time: wave cap = min(per-wave cap,
	// daily cap - already mailed today). Follow-up touches are NOT counted
	// or capped here ("everything else can flow as is" — operator
	// 2026-06-10). Override: PARTNER_DRIP_DAILY_ISP_CAPS="gmail=400,yahoo=100,aol=100".
	NewRecordDailyISPCaps map[string]int
	// NewRecordISPBrandAllow restricts which brands may receive NEW records
	// for an ISP at all (caps[isp]=0 for brands not listed). Operator
	// 2026-06-10: gmail new records only via db/ht/mh/qf.
	// Override: PARTNER_DRIP_GMAIL_NEW_BRANDS="db,ht,mh,qf".
	NewRecordISPBrandAllow map[string]map[string]bool
	// ThrottledISPRateThreshold (msgs_per_hour) below which an ISP is considered
	// in active backoff and that ISP's portion of the wave is deferred. Default 50.
	ThrottledISPRateThreshold float64
	// ThrottleDeferralDisabled skips mailing_isp_throttle_state deferrals entirely
	// (PARTNER_DRIP_THROTTLE_THRESHOLD=0). Do not rely on threshold=0 alone —
	// NewPartnerDripOrchestrator treats unset zero as "default to 50".
	ThrottleDeferralDisabled bool
	// BrandsPerTick fires up to N brands' welcome waves per vertical per
	// tick (in parallel). Default 4. With 16 brands round-robin and 4
	// brands per tick, the rotation completes one full cycle in 4 ticks
	// (1 hour at 15min cadence). This is what unlocks the "expand
	// ingestion across ALL properties = throughput up" property — without
	// it, adding 12 new brands stretches the rotation 4x and per-brand
	// throughput stays the same.
	BrandsPerTick int
	// FollowupBrandsPerTick mirrors BrandsPerTick but for the follow-up
	// touches. Default 4. The follow-up loop runs against a synthetic
	// 'followup' vertical so its brand-rotation index is independent of
	// the welcome-touch round-robin.
	FollowupBrandsPerTick int
	// MaxFollowupClaimPerVertical caps how many records the follow-up
	// loop will claim per vertical per tick (across all brands), as a
	// safety ceiling. Default 5000 (matches MaxWaveSize).
	MaxFollowupClaimPerVertical int
	// FollowupDisabled skips tickFollowups entirely (PARTNER_DRIP_FOLLOWUP_DISABLED=1).
	// Do not rely on MaxFollowupClaimPerVertical=0 alone — NewPartnerDripOrchestrator
	// treats unset zero as "default to MaxWaveSize".
	FollowupDisabled bool
	// YahooNewsletterOnlyDrip makes the yahoo-family a NEWSLETTER-ONLY drip lane
	// (operator 2026-07-14: "for our Yahoo drips we must mail newsletter only").
	// When true: (1) every OFFER follow-up claim excludes yahoo (perISPCaps["yahoo"]=0
	// in processFollowup, mirroring the live apple ban immediately below it), and
	// (2) a dedicated yahoo-confined follow-up pass serves the brand NEWSLETTER
	// (resolveCreative — the same creative touch-0/welcome ships) at touches 2..N,
	// so yahoo records keep climbing the ladder without ever receiving an offer.
	// Env: PARTNER_DRIP_YAHOO_NEWSLETTER_ONLY=1. Default false = exact legacy
	// behavior (yahoo receives offer follow-ups). This is the one-move kill switch.
	YahooNewsletterOnlyDrip bool
	// ClaimedJanitorMaxAge releases partner_clean_queue rows stuck in
	// 'claimed' after a crash between claim and promote/deploy. Only
	// rows with no subscriber_id and no mailed_campaign_id are touched.
	// Default 45m. Set to 0 to disable.
	ClaimedJanitorMaxAge time.Duration
	// GovernedDisabled skips tickGoverned entirely (the Kumo property governed
	// pass). Mirrors FollowupDisabled. Env: PARTNER_DRIP_GOVERNED_DISABLED=1.
	GovernedDisabled bool
	// GovernedBrandsPerTick (SAFEGUARD (c) — DEPLOY THROTTLE) bounds how many
	// governed brands tickGoverned fires per vertical per tick. Kept small
	// (default 2) and fired SEQUENTIALLY per brand so the governed pass adds at
	// most ~GovernedBrandsPerTick deploys/vertical/tick on top of the welcome +
	// follow-up passes — bounding the staging-burst exposure. Env:
	// PARTNER_DRIP_GOVERNED_BRANDS_PER_TICK.
	GovernedBrandsPerTick int
}

type PartnerDripOrchestrator struct {
	db  *sql.DB
	cfg PartnerDripOrchestratorConfig

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once

	// governorCache holds the partner_property_governor rows, refreshed once
	// per tick by loadGovernors. Keyed by brand_code. Guarded by governorMu.
	governorMu    sync.RWMutex
	governorCache map[string]propertyGovernor

	// ispCapCache holds partner_drip_isp_caps (isp -> per_wave_cap), refreshed
	// once per tick by loadISPCaps. It OVERLAYS cfg.PerISPCapPerWave, so an ISP
	// absent from the table keeps its compiled value and an unreadable table is
	// a no-op rather than an outage. This is what makes a cap change deploy-free.
	ispCapMu    sync.RWMutex
	ispCapCache map[string]int

	// brandBudgetCache holds partner_drip_brand_budgets (brand -> isp -> row),
	// refreshed once per tick by loadBrandBudgets. An absent (brand, isp) cell
	// means "unconstrained", so an empty cache is byte-identical to the
	// pre-ledger behavior. Guarded by brandBudgetMu.
	//
	// globalHold mirrors the property_ledger_flags 'global_hold' row (I-5,
	// property_ledger_doc.go), refreshed by the same loadBrandBudgets tick.
	// nil = never successfully read since process start — FAIL-CLOSED: the
	// welcome pass treats the estate as HELD until the first successful read.
	// Once warm, a read error keeps the previous value. Guarded by brandBudgetMu.
	brandBudgetMu    sync.RWMutex
	brandBudgetCache map[string]map[string]brandBudgetRow
	globalHold       *bool

	// throttleCache is the per-tick snapshot of mailing_isp_throttle_state
	// (fetchThrottledISPs), loaded once by loadThrottledISPs in tickOnce.
	// applyThroughputSafety runs once per WAVE (8-10+ waves per vertical per
	// tick across welcome + follow-up + governed passes), and before this cache
	// each of those calls re-read the table. throttleErr preserves the exact
	// pre-cache semantics: a failed read still surfaces to applyThroughputSafety,
	// whose callers proceed without deferral. throttleLoaded=false (a path that
	// never went through tickOnce, e.g. tests) falls back to a direct fetch.
	// Guarded by throttleMu.
	throttleMu     sync.RWMutex
	throttleCache  map[string]float64
	throttleErr    error
	throttleLoaded bool

	// stamp counts markMailed / recovery outcomes since process start. Before
	// this existed the ONLY evidence of a dropped ladder advance was a single
	// log line (2026-08-20). Snapshot it with StampStats(); the durable,
	// operator-queryable form is the partner_drip_stamp_failures table.
	stamp stampCounters

	// lastStampRecovery is the wall clock of the last recovery pass, so the
	// opt-in sweep runs on its own cadence rather than every tick.
	lastStampRecovery time.Time
}

// propertyGovernor is the in-memory form of a partner_property_governor row.
type propertyGovernor struct {
	brand          string
	perISPDaily    int             // per_isp_daily_cap (seed 500)
	windowHours    int             // window_hours (seed 6)
	gmailHeld      bool            // gmail_held (seed true)
	perISPOverride map[string]int  // per_isp_overrides JSONB (isp -> cap; replaces perISPDaily)
	subscribed     map[string]bool // subscribed_verticals (lowercased)
	active         bool
}

// DefaultPerISPCapPerWave returns the COMPILED per-ISP per-wave claim
// ceilings — the value NewPartnerDripOrchestrator installs when the caller
// supplies none (cmd/server does not). Extracted from the constructor so the
// Property Ledger throttle read can name the GLOBAL default an ISP without a
// partner_isp_distribution_overrides row actually rides, instead of
// re-declaring the map in internal/api and drifting from it.
//
// NOTE: this is the compiled BASE only. The live per-wave default is this map
// OVERLAID by active partner_drip_isp_caps rows (loadISPCaps -> basePerISPCaps),
// which the startup migration seeds with these exact values.
func DefaultPerISPCapPerWave() map[string]int {
	var out map[string]int
	// Per-ISP per-wave caps. With 4 waves/hour (15-min ticks) and 4
	// brands round-robin, each brand sees one wave per hour, so:
	//
	//   per_brand_per_day = cap * 24
	//
	// Caps below stay under each ISP's published per-brand-per-day
	// guidance with ~5–10% headroom:
	//
	//   gmail     200 -> 4,800/brand/day (Google: 5,000/sender/day)
	//   microsoft 100 -> 2,400/brand/day (operator 2026-06-02: 50% pullback from 200)
	//   apple     200 -> 4,800/brand/day (no published cap; warm reputation)
	//   yahoo     20  -> 480/brand/day  (Yahoo: 500/sender/day)
	//   aol       20  -> 480/brand/day  (matches Yahoo carve)
	//   other     150 -> 3,600/brand/day (mixed deliverability bucket)
	//
	// 2026-05-14 bump: gmail 150->200, microsoft/apple 100->200,
	// other 100->150, comcast/charter 60->100. Operator directive
	// "INCREASE THROUGHPUT. Especially for GMAIL." after observing
	// David Cal's Personal Loans feed shipping 235 gmail/day vs a
	// 2,334-record queue.
	//
	// Pairs with the per-ISP claim strategy in claimRecordsByISPCaps —
	// without that change, raising caps alone has near-zero effect
	// because the old oldest-first claim wastes wave slots on ISPs
	// (yahoo) whose share of the queue dominates and whose cap is
	// then defer-released.
	// 2026-06-05 REDUCTION (operator directive "do not stop them just reduce
	// the quotas"): DSN-truth analysis showed the drip's "14-17% hard bounce"
	// is ~98% ISP reputation BLOCKS (5.0.0 policy, 5.3.2 system-not-accepting,
	// 5.7.1 spam) on clean EO-verified addresses — i.e. a reputation/placement
	// problem, not bad data. The cold-mail VOLUME on the shared PMTA IPs is what
	// manufactures those blocks. Caps cut ~70% to relieve block pressure while
	// the engaged-only model rebuilds reputation; the channel keeps flowing.
	// Prior caps (gmail 200/yahoo 20/aol 20/microsoft 100/apple 200/comcast 100/
	// charter 100/att 60/sbcglobal 60/cox 60/verizon 60/other 150) are preserved
	// in git history; raise back once block rate (5.x.x reputation DSNs) recovers.
	// 2026-06-13 operator cap revision (Sam's data "fine, just drain faster"):
	//   gmail     0      HOLD all new gmail; focus on known engagers (follow-ups
	//                    + clicker rings continue; only new first-touch stops)
	//   yahoo     8->16  growing lane, doubled per operator
	//   apple     ~uncapped (100000; routed to mature-4 for placement)
	//   microsoft ~uncapped (100000; spreads all 16)
	//   att       50 ceiling, true rate set by NewRecordDailyISPCaps att=225/brand
	//             (×4 mature = ~900/day) — "loving the growth, 480->900/d"
	//   aol       30 ceiling, true rate set by NewRecordDailyISPCaps aol=56/brand
	//             (×16 brands = ~900/day)
	// 2026-06-29 operator "double the family draining and monitor": the partner
	// ready backlog is ~76% Yahoo-family (~1.26M ready) and SES is healthy (Yahoo
	// 99.1% / AOL 98.8% / ATT 99.6% accept, 0% complaints, ~100k headroom), so the
	// binding per-wave ceiling — NOT data or SES — is throttling the drain. Double
	// the Yahoo-family per-wave caps (yahoo 16->32, aol 30->60, att 50->100,
	// sbcglobal 20->40, verizon 20->40). Watch deferral/complaint and step again.
	out = map[string]int{
		"gmail":     0,
		"yahoo":     32,
		"aol":       60,
		"microsoft": 100000,
		"apple":     100000,
		"comcast":   30,
		"charter":   30,
		"att":       100,
		"sbcglobal": 40,
		"cox":       20,
		"verizon":   40,
		"other":     40,
	}
	return out
}

// DefaultNewRecordDailyISPCaps returns the GLOBAL per-(brand, ISP) daily
// new-record budgets the orchestrator installs by default, including the
// PARTNER_DRIP_DAILY_ISP_CAPS env override. Same reason as
// DefaultPerISPCapPerWave: one definition, read by both the orchestrator and
// the throttle read that reports which cap source is winning.
func DefaultNewRecordDailyISPCaps() map[string]int {
	var out map[string]int
	// Operator 2026-06-10: gmail 400/day (allow-listed brands only),
	// yahoo 100/day and aol 100/day across all brands. ISPs not listed
	// here have no daily new-record budget (per-wave caps only).
	// 2026-06-13 operator: gmail HELD to 0 (no new gmail). att/aol set to
	// hit ~900/day TOTAL via per-brand budgets (caps are per-brand/day):
	//   att 225/brand × 4 mature (routed) = ~900/day
	//   aol 225/brand × 4 mature (routed 2026-06-13) = ~900/day
	// yahoo kept at 100/brand × 4 mature = 400/day ceiling (per-wave 16 binds ~384).
	// Operator 2026-06-27: drain the non-gmail backlog GLOBALLY (shared across all
	// feeds). gmail HELD at 0; yahoo/aol/att daily budgets removed so the 7-day
	// drain-horizon (PerISPDrainDays below) paces the drain, not a flat per-brand
	// ceiling. ("Share across all feeds. Also expand across all brands.")
	out = map[string]int{"gmail": 0}
	if v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_DAILY_ISP_CAPS")); v != "" {
		parsed := map[string]int{}
		for _, pair := range strings.Split(v, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) != 2 {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(kv[1])); err == nil {
				parsed[strings.ToLower(strings.TrimSpace(kv[0]))] = n
			}
		}
		if len(parsed) > 0 {
			out = parsed
		}
	}
	return out
}

// DefaultNewRecordISPBrandAllow returns the brand-allow routing gate. It is
// the HARD ceiling a lane override cannot bypass (applyNewRecordDailyBudget
// zeroes the cap for a brand outside an ISP's allow set), so the throttle
// read must apply it before claiming any effective cap.
func DefaultNewRecordISPBrandAllow() map[string]map[string]bool {
	var out map[string]map[string]bool
	// Deliverability routing (operator 2026-06-13): the engagement-priced /
	// reputation-sensitive ISPs ship only from the warmed mature-4 domains
	// (db/ht/mh/qf own the isolated per-ISP IP pools). Per-ISP env override:
	// PARTNER_DRIP_<ISP>_NEW_BRANDS (e.g. PARTNER_DRIP_YAHOO_NEW_BRANDS).
	matureBrands := "db,ht,mh,qf"
	parseAllow := func(envKey, def string) map[string]bool {
		v := strings.TrimSpace(os.Getenv(envKey))
		if v == "" {
			v = def
		}
		m := map[string]bool{}
		for _, b := range strings.Split(v, ",") {
			if b = strings.ToLower(strings.TrimSpace(b)); b != "" {
				m[b] = true
			}
		}
		return m
	}
	// Operator 2026-06-27: "expand across all brands" — yahoo/att/aol now
	// ship from ALL 16 brands (removed from this map = unrestricted routing). Only
	// gmail stays mature-4-restricted, and it is held at per-wave cap 0 regardless.
	// Operator 2026-07-07 (term_life_apple/Liberty lane): apple excludes the two
	// Apple-banned brands lpl/wfy (HM08 — Apple hard-rejects those sending
	// domains regardless of offer). Applied on welcome AND follow-up passes.
	appleBrands := "db,ht,mh,qf,bwp,ci,cp,fc,hws,mrd,rb,rru,tot,yih"
	out = map[string]map[string]bool{
		"gmail": parseAllow("PARTNER_DRIP_GMAIL_NEW_BRANDS", matureBrands),
		"apple": parseAllow("PARTNER_DRIP_APPLE_NEW_BRANDS", appleBrands),
	}
	return out
}

func NewPartnerDripOrchestrator(db *sql.DB, cfg PartnerDripOrchestratorConfig) *PartnerDripOrchestrator {
	if cfg.OrganizationID == "" {
		cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 15 * time.Minute
	}
	if cfg.MinWaveSize <= 0 {
		cfg.MinWaveSize = 25
	}
	if cfg.MaxWaveSize <= 0 {
		cfg.MaxWaveSize = 5000
	}
	if cfg.WindowHours <= 0 {
		cfg.WindowHours = 8 // matches the wave_sanity check minimum
	}
	if cfg.CreativesDir == "" {
		cfg.CreativesDir = "docs/emails"
	}
	if cfg.PerISPCapPerWave == nil {
		cfg.PerISPCapPerWave = DefaultPerISPCapPerWave()
	}
	if cfg.NewRecordDailyISPCaps == nil {
		cfg.NewRecordDailyISPCaps = DefaultNewRecordDailyISPCaps()
	}
	if cfg.NewRecordISPBrandAllow == nil {
		cfg.NewRecordISPBrandAllow = DefaultNewRecordISPBrandAllow()
	}
	if cfg.PerISPDrainDays == nil {
		// Operator 2026-05-30: stretch high-volume / sensitive ISPs so a
		// refilling ingest queue drains over multiple days. Caps float with
		// live ready depth — see ispCapForDrainHorizon.
		// Operator 2026-06-27: 7-day drain horizon for the non-gmail sensitive ISPs —
		// spreads each vertical's backlog over ~7 days at the wave cadence (shared
		// across all feeds, all 16 brands). gmail held at cap 0 (drain-days moot).
		// Operator 2026-06-29 "double the Yahoo-family drain": the per-wave cap (raised
		// 16→32 in 41d1746) was NEVER the throttle — empirically Yahoo mailed ~8/wave,
		// because cap = min(PerISPCapPerWave, ceil(ready/(waves×drainDays))) and the
		// 7-day horizon binds at ~8. Halve the Yahoo-family horizon 7→3 (~2.3× the
		// per-wave drainCap to ~16-18, which now flows under the 32 cap instead of
		// re-clamping at 16). gmail stays 3 (cap 0 anyway). Monitor accept/bounce.
		// Operator 2026-06-30 "push it further": Yahoo/SES is healthy (99% accept, deferrals
		// negligible — the jun30 deferral pressure was PMTA-only, NOT SES/Yahoo) and ~1.26M
		// family records still ready, so cut the Yahoo-family horizon 3→2 (drainCap ~16→~24,
		// still under the 32 per-wave cap). Expect a MODEST further lift — many waves are
		// audience-limited (jun30 per-wave avg ~8-9 << the ~16 cap), so only cap-bound waves
		// gain. gmail stays 3. Monitor SES accept/deferral; next step 2→1 hits the 32 cap.
		cfg.PerISPDrainDays = map[string]int{
			"gmail":     3,
			"yahoo":     2,
			"sbcglobal": 2,
			"aol":       2,
			"att":       2,
		}
	}
	if !cfg.ThrottleDeferralDisabled && cfg.ThrottledISPRateThreshold <= 0 {
		cfg.ThrottledISPRateThreshold = 50.0
	}
	if cfg.BrandsPerTick <= 0 {
		cfg.BrandsPerTick = 4
	}
	if cfg.FollowupBrandsPerTick <= 0 {
		cfg.FollowupBrandsPerTick = 4
	}
	if cfg.FollowupDisabled {
		cfg.MaxFollowupClaimPerVertical = 0
	} else if cfg.MaxFollowupClaimPerVertical <= 0 {
		cfg.MaxFollowupClaimPerVertical = cfg.MaxWaveSize
	}
	if cfg.ClaimedJanitorMaxAge == 0 {
		cfg.ClaimedJanitorMaxAge = 45 * time.Minute
	}
	// SAFEGUARD (c): small default so the governed pass never bursts. Sequential
	// per-brand firing keeps added deploys bounded (~2/vertical/tick).
	if cfg.GovernedBrandsPerTick <= 0 {
		cfg.GovernedBrandsPerTick = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PartnerDripOrchestrator{db: db, cfg: cfg, ctx: ctx, cancel: cancel}
}

func (po *PartnerDripOrchestrator) Start() {
	po.startOnce.Do(func() {
		po.wg.Add(1)
		go po.run()
		log.Printf("[PartnerDripOrchestrator] started — tick=%s min_wave=%d max_wave=%d window_hours=%d brands_per_tick=%d followup_brands_per_tick=%d max_followup_claim=%d creatives_dir=%s brands=%d",
			po.cfg.TickInterval, po.cfg.MinWaveSize, po.cfg.MaxWaveSize, po.cfg.WindowHours,
			po.cfg.BrandsPerTick, po.cfg.FollowupBrandsPerTick, po.cfg.MaxFollowupClaimPerVertical,
			po.cfg.CreativesDir, len(dripBrands))
	})
}

func (po *PartnerDripOrchestrator) Stop() {
	po.stopOnce.Do(func() {
		po.cancel()
		po.wg.Wait()
		log.Println("[PartnerDripOrchestrator] stopped")
	})
}

func (po *PartnerDripOrchestrator) run() {
	defer po.wg.Done()
	t := time.NewTicker(po.cfg.TickInterval)
	defer t.Stop()
	// Don't wait an entire tick on first boot — drain any waiting backlog.
	po.tickOnce()
	for {
		select {
		case <-po.ctx.Done():
			return
		case <-t.C:
			po.tickOnce()
		}
	}
}

// tickOnce iterates over every active vertical with ready records and
// runs up to BrandsPerTick welcome waves per vertical per tick (in
// parallel brand selection — one wave per (vertical, brand) pair).
// Errors per-vertical are logged but do not stop the rest of the loop.
//
// After the welcome pass, runs the follow-up pass which scans for
// records whose next_touch_at has elapsed and ships them through one or
// more touches at FollowupBrandsPerTick per vertical.
func (po *PartnerDripOrchestrator) tickOnce() {
	if po.cfg.DeployFn == nil {
		log.Println("[PartnerDripOrchestrator] no DeployFn wired — skipping tick")
		return
	}
	// TICK LEADER LEASE (2026-08-22). The ECS service runs desiredCount=2, so
	// TWO full orchestrators have been ticking concurrently with no
	// coordination — every sweep, claim and deploy attempt ran twice, saved
	// only by SKIP LOCKED and by-name idempotency, and doubling per-tick DB
	// load (two identical reconcile UPDATEs were measured running side by
	// side). The non-leader skips the tick entirely; leadership migrates when
	// the holder's session dies.
	//
	// INCIDENT 2026-08-22 (~22:20 UTC): the first version of this lease used
	// distlock's PG-advisory fallback on the POOLED *sql.DB. pg_try_advisory_lock
	// is SESSION(connection)-scoped, so Acquire ran on one pooled connection and
	// Release's pg_advisory_unlock landed on a DIFFERENT pooled connection — a
	// silent no-op — leaving the lock held by an idle pooled connection. From
	// the second tick onward BOTH ECS tasks got acquired=false and skipped
	// every tick: ALL partner-drip sending stopped ~40 min after boot while
	// broadcast continued. Fixed by holding the lock on a DEDICATED connection
	// for the tick's duration (see acquireTickLease). Fail-OPEN on any
	// lock-infrastructure error: locking must never stop the drip. Kill
	// switch: PARTNER_DRIP_TICK_LOCK_DISABLED=1 restores dual-tick.
	release, lead := po.acquireTickLease()
	if !lead {
		return
	}
	defer release()
	// Refresh the property-governor cache once per tick so operator edits to
	// partner_property_governor (caps, subscriptions) take effect on the next
	// tick without a deploy. Fail-safe: on load error the cache is emptied so
	// no governed brand joins any rotation and any governed wave is cap-0.
	po.loadGovernors(po.ctx)
	// Same contract as the governor refresh: operator edits to the ISP-cap and
	// brand-domain tables take effect next tick, no deploy.
	po.loadISPCaps(po.ctx)
	po.loadBrandDomains(po.ctx)
	po.loadVerticalRosters(po.ctx)
	po.loadBrandBudgets(po.ctx)
	// Throttle snapshot for this tick — applyThroughputSafety runs once per WAVE
	// and used to re-read mailing_isp_throttle_state on every call.
	po.loadThrottledISPs(po.ctx)
	if po.cfg.ClaimedJanitorMaxAge > 0 {
		if n, err := po.releaseStaleClaims(po.ctx); err != nil {
			log.Printf("[PartnerDripOrchestrator] claimed janitor: %v", err)
		} else if n > 0 {
			log.Printf("[PartnerDripOrchestrator] claimed janitor released %d stale rows", n)
		}
	}
	// Ladder-stamp recovery (partner_drip_stamp_recovery.go) runs BEFORE the
	// residual reconcile below: it replays every durable stamp failure from
	// mailing_message_log (send truth), advancing the ladder to the EXACT
	// campaign that shipped. Always on since 2026-08-22 (opt-OUT kill switch
	// PARTNER_DRIP_STAMP_RECOVERY_DISABLED=1); rate-limited to its own cadence,
	// so a re-fired scheduler on every ECS bounce cannot turn it into a
	// per-tick sweep.
	po.maybeRunStampRecovery(po.ctx)
	// Residual reconcile for whatever recovery could not attribute. Since
	// 2026-08-22 this ALSO advances touch_count / next_touch_at (the bare status
	// flip was the 2026-08-17 triple-fire: a flipped-but-not-advanced row was
	// instantly re-claimable by claimFollowupRecordsByISPCaps and the same
	// people re-mailed every tick until a markMailed succeeded).
	if n, err := po.reconcileShippedClaims(po.ctx); err != nil {
		log.Printf("[PartnerDripOrchestrator] reconcile shipped claims: %v", err)
	} else if n > 0 {
		log.Printf("[PartnerDripOrchestrator] reconciled %d claimed rows to mailed + advanced their ladder (post-deploy markMailed miss)", n)
	}
	// Always-on health surface for the ladder stamp (mailing_worker_heartbeats
	// row 'partner_drip_ladder_stamp'). Flips to last_status='error' the moment a
	// stamp is dropped, so the loss is visible in the portal's worker-health view
	// instead of only in one log line.
	po.emitStampHeartbeat(po.ctx)
	// A gateway-query failure (e.g. statement_timeout under heavy RDS IO) must
	// NOT abort the whole tick — the follow-up pass below is an independent
	// path with its own gateway and should still get a chance to ship. Log and
	// fall through with an empty welcome set rather than returning early.
	verticals, err := po.activeVerticalsWithBacklog(po.ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] active_verticals: %v (continuing to follow-up pass)", err)
		verticals = nil
	}
	for _, v := range verticals {
		if po.ctx.Err() != nil {
			return
		}
		// AOL round-robin companion (Operating Plan v2): on the internal
		// verticals, AOL introductions ship under a rotated estate brand.
		if aolRotationActive(v.vertical) {
			if err := po.processAOLRotated(po.ctx, v); err != nil {
				log.Printf("[PartnerDripOrchestrator] aol_rotate vertical=%s: %v", v.vertical, err)
			}
		}
		// Fire up to BrandsPerTick welcome waves per vertical per tick.
		// Each call advances the brand round-robin pointer and processes
		// a fresh per-ISP-capped wave for that brand.
		brandsThisTick := po.cfg.BrandsPerTick
		if brandsThisTick > len(dripBrands) {
			brandsThisTick = len(dripBrands)
		}
		for i := 0; i < brandsThisTick; i++ {
			if po.ctx.Err() != nil {
				return
			}
			if err := po.processVertical(po.ctx, v); err != nil {
				log.Printf("[PartnerDripOrchestrator] welcome vertical=%s: %v", v.vertical, err)
				// Keep advancing brand index even on error — the next
				// brand's wave is independent.
			}
			// Re-read the vertical state for ready_count and brand_index
			// so the next call doesn't re-process the same brand. We
			// also bail out if no more ready records remain for this
			// vertical (the orchestrator's main goal is responsiveness).
			fresh, err := po.refreshVerticalState(po.ctx, v.vertical)
			if err != nil || fresh == nil || fresh.readyCount <= 0 {
				break
			}
			v = *fresh
		}
	}
	// Follow-up pass: independent of welcome pass, runs across the same
	// verticals (record-vertical, not brand-vertical). The 'followup'
	// drip-state row drives brand rotation for follow-ups.
	if !po.cfg.FollowupDisabled && po.cfg.MaxFollowupClaimPerVertical > 0 {
		po.tickFollowups(po.ctx)
	}
	// Governed pass (Option B): additive third pass for the Kumo properties.
	// Mirrors the follow-up pass's shape (own vertical enumeration + own
	// '<vertical>:governed' rotation state + reuses the welcome wave builder).
	// Runs LAST so it only claims what the welcome + follow-up passes leave, and
	// the floor gate ensures it only touches huge pools (zero side effect on the
	// 16 brands). Welcome-only: governed brands are absent from dripBrands AND
	// brandRosterFor, so neither prior pass can select them.
	if !po.cfg.GovernedDisabled {
		po.tickGoverned(po.ctx)
	}
}

// refreshVerticalState pulls the up-to-date state of a single vertical
// from the DB. Used between intra-tick calls to processVertical so the
// brand pointer + ready-count reflect the wave we just shipped.
func (po *PartnerDripOrchestrator) refreshVerticalState(ctx context.Context, vertical string) (*verticalState, error) {
	var (
		v     verticalState
		found bool
	)
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		// Phase 1 — cheap per-vertical aggregate via idx_pcq_status_ready.
		var (
			readyTotal sql.NullInt64
			oldestAt   sql.NullTime
		)
		err := tx.QueryRowContext(ctx, `
			SELECT s.next_brand_index, agg.ready_total, agg.oldest_at
			FROM partner_drip_state s
			JOIN (
				SELECT COUNT(*) AS ready_total, MIN(ingested_at) AS oldest_at
				FROM partner_clean_queue
				WHERE status = 'ready' AND vertical = $1
			) agg ON true
			WHERE s.vertical = $1
		`, vertical).Scan(&v.brandIndex, &readyTotal, &oldestAt)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		v.vertical = vertical
		if readyTotal.Valid {
			v.readyCount = int(readyTotal.Int64)
		}
		v.oldestIngest = oldestAt

		// Phase 2 — dominant-dataset metadata (oldest ready row).
		var (
			datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
			flushHours                                       sql.NullInt64
			offerID                                          string
		)
		err = tx.QueryRowContext(ctx, `
			SELECT d.id, d.slug, p.slug, p.name, d.flush_window_hours,
			       COALESCE(d.offer_id::text, '')
			FROM (
				SELECT dataset_id
				FROM partner_clean_queue
				WHERE vertical = $1 AND status = 'ready'
				ORDER BY ingested_at ASC
				LIMIT 1
			) AS dom
			LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
			LEFT JOIN data_partners p ON p.id = d.partner_id
		`, vertical).Scan(&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		v.offerID = offerID
		if datasetID.Valid {
			v.datasetID = datasetID.String
		}
		if datasetSlug.Valid {
			v.datasetSlug = datasetSlug.String
		}
		if partnerSlug.Valid {
			v.partnerSlug = partnerSlug.String
		}
		if partnerName.Valid {
			v.partnerName = partnerName.String
		}
		if flushHours.Valid {
			v.flushHours = int(flushHours.Int64)
		}
		if v.flushHours <= 0 {
			v.flushHours = 24
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &v, nil
}

type verticalState struct {
	vertical     string
	brandIndex   int
	readyCount   int
	oldestIngest sql.NullTime
	flushHours   int
	datasetID    string // dominant dataset id (for ISP overrides + flush window) — picked by oldest ingestion
	datasetSlug  string
	partnerSlug  string
	partnerName  string
	// OfferID is set when the dominant dataset for this vertical is bound to
	// a specific mailing_offers.id (vertical='direct_offer'). When set, the
	// orchestrator pulls creative + subject + from-name from the offer-center
	// tables and stamps OfferID on the deploy payload so the offer-suppression
	// Bloom filter applies and content_locked inherits from the offer. Empty
	// for the legacy 4-vertical drip pool.
	offerID string
}

// orchestratorQueryTimeout is the per-query statement_timeout the orchestrator
// applies to its full-queue scans. The app's main pool runs at 30s
// (main.go: statement_timeout=30000), but partner_clean_queue is >1M rows and
// its aggregate / window scans legitimately exceed 30s whenever RDS IO is
// saturated by concurrent audience/segment evaluation. When the gateway scan
// (activeVerticalsWithBacklog) is cancelled, tickOnce returns early and ships
// ZERO waves — the whole drip silently falls behind. Raising the ceiling for
// just these queries keeps the tick alive under load.
const orchestratorQueryTimeout = 120 * time.Second

// withDBTimeout runs fn inside a read-committed transaction whose
// statement_timeout is raised to orchestratorQueryTimeout via SET LOCAL.
// Because SET LOCAL is transaction-scoped, the pooled connection returns to
// the pool with the app's default 30s ceiling intact — no cross-query leak.
// This mirrors the established pattern in segment_refresh.go and
// mailing_admin_bulk_tag.go. The tx is read-committed and commits on success;
// it carries the orchestrator's claim UPDATEs too (FOR UPDATE SKIP LOCKED rows
// stay locked only until the immediate commit).
func (po *PartnerDripOrchestrator) withDBTimeout(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := po.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", orchestratorQueryTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// activeVerticalsWithBacklog returns every vertical that has ready records,
// the brand round-robin pointer, the ready count + oldest ingest, and the
// dataset/partner metadata of the dataset owning the oldest ready record.
//
// 2026-06-07: split into a cheap aggregate gateway + a per-vertical metadata
// fetch, all under withDBTimeout. The previous single query grouped the entire
// ready set by (vertical, dataset_id) AND ran a correlated LATERAL per
// drip-state row; on a >1M-row queue under IO pressure it blew past the 30s
// pool timeout every tick, aborting the whole drip. Phase 1 is a plain
// GROUP BY vertical satisfied by the partial index idx_pcq_status_ready
// (vertical, status, ingested_at) WHERE status='ready'. Phase 2 is a per-
// vertical LIMIT 1 index scan for the dominant (oldest-ingest) dataset.
func (po *PartnerDripOrchestrator) activeVerticalsWithBacklog(ctx context.Context) ([]verticalState, error) {
	var out []verticalState
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		// Phase 1 — cheap aggregate over the ready partial index.
		rows, err := tx.QueryContext(ctx, `
			SELECT s.vertical, s.next_brand_index, agg.ready_total, agg.oldest_at
			FROM partner_drip_state s
			JOIN (
				SELECT vertical, COUNT(*) AS ready_total, MIN(ingested_at) AS oldest_at
				FROM partner_clean_queue
				WHERE status = 'ready'
				GROUP BY vertical
			) agg ON agg.vertical = s.vertical
			ORDER BY s.vertical
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v verticalState
			var (
				readyTotal sql.NullInt64
				oldestAt   sql.NullTime
			)
			if err := rows.Scan(&v.vertical, &v.brandIndex, &readyTotal, &oldestAt); err != nil {
				continue
			}
			if !readyTotal.Valid || readyTotal.Int64 == 0 {
				continue
			}
			v.readyCount = int(readyTotal.Int64)
			v.oldestIngest = oldestAt
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Phase 2 — per-vertical dominant-dataset metadata (oldest ready row).
		for i := range out {
			var (
				datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
				flushHours                                       sql.NullInt64
				offerID                                          string
			)
			err := tx.QueryRowContext(ctx, `
				SELECT d.id, d.slug, p.slug, p.name, d.flush_window_hours,
				       COALESCE(d.offer_id::text, '')
				FROM (
					SELECT dataset_id
					FROM partner_clean_queue
					WHERE vertical = $1 AND status = 'ready'
					ORDER BY ingested_at ASC
					LIMIT 1
				) AS dom
				LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
				LEFT JOIN data_partners p ON p.id = d.partner_id
			`, out[i].vertical).Scan(&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			out[i].offerID = offerID
			if datasetID.Valid {
				out[i].datasetID = datasetID.String
			}
			if datasetSlug.Valid {
				out[i].datasetSlug = datasetSlug.String
			}
			if partnerSlug.Valid {
				out[i].partnerSlug = partnerSlug.String
			}
			if partnerName.Valid {
				out[i].partnerName = partnerName.String
			}
			if flushHours.Valid {
				out[i].flushHours = int(flushHours.Int64)
			}
			if out[i].flushHours <= 0 {
				out[i].flushHours = 24
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// appleBannedDripVerticals returns drip verticals barred from Apple/iCloud. Operator 2026-06-16:
// Apple HM08-rejects Fidelity term-life ~85-92% via BOTH PMTA and SES (content/domain policy — no
// ESP or whitelisting lever; proven against Sam's Club, which delivers to the same Apple audience
// at 0.04% hard). Zeroing the apple per-wave cap makes claim*ByISPCaps skip Apple for these
// verticals. Override the default via PARTNER_DRIP_APPLE_BANNED_VERTICALS (comma-separated list).
func appleBannedDripVerticals() map[string]bool {
	v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_APPLE_BANNED_VERTICALS"))
	if v == "" {
		v = "term_life"
	}
	m := map[string]bool{}
	for _, s := range strings.Split(v, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			m[s] = true
		}
	}
	return m
}

// brandSESSendingDomain maps a brand to its SES tenant sending domain (m.<apex>),
// used when a wave (or part of one) is pinned to that brand's SES tenant profile
// via dripBrandISPSESProfiles. Only the SES-tenant brands need an entry; brands
// without one keep their default em.<apex> PMTA domain.
var brandSESSendingDomain = map[string]string{
	"ht": "m.historythinking.com",
	"mh": "m.myownhealth.net",
}

// dripBrandISPSESProfiles returns per-(brand,ISP) SES tenant profile pins. When a
// drip wave for `brand` includes a pinned ISP, that ISP's records are peeled off
// into a SEPARATE campaign pinned to the brand's SES tenant profile (m.<apex>,
// via_ses=true) instead of the default em.<apex> PMTA route — routing a
// collapsing-reputation ISP pipe through the SES relay, mirroring the boards'
// ses_tight route and the standing Gmail-via-SES rule. Operator 2026-06-16:
// "route all HT Microsoft traffic through AWS SES" (HT rides the Server-B
// Microsoft pipe whose reputation throttle collapsed). Default: ht microsoft →
// HT SES tenant (c24a8455…). Override via PARTNER_DRIP_BRAND_ISP_SES_PROFILES
// (comma-separated brand=isp=profileUUID triples, env REPLACES the default).
// The PMTA route is the default for every (brand,ISP) NOT listed here.
func dripBrandISPSESProfiles() map[string]map[string]string {
	v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_BRAND_ISP_SES_PROFILES"))
	if v == "" {
		v = "ht=microsoft=c24a8455-e893-4895-a8ad-4556d9013003"
	}
	m := map[string]map[string]string{}
	for _, trip := range strings.Split(v, ",") {
		parts := strings.SplitN(strings.TrimSpace(trip), "=", 3)
		if len(parts) != 3 {
			continue
		}
		brand := strings.ToLower(strings.TrimSpace(parts[0]))
		isp := strings.ToLower(strings.TrimSpace(parts[1]))
		pid := strings.TrimSpace(parts[2])
		if brand == "" || isp == "" || pid == "" {
			continue
		}
		if m[brand] == nil {
			m[brand] = map[string]string{}
		}
		m[brand][isp] = pid
	}
	return m
}

// dripBrandSESProfiles maps each of the 16 drip brands to its m.<apex> SES tenant
// profile. When PARTNER_DRIP_ROUTE_ALL_SES is on (default), every claimed record
// that isn't otherwise pinned defaults to its brand's SES profile, so the ENTIRE
// partner-drip ships through the SES relay (operator 2026-06-27: "all of these
// drips should route through SES"). gmail is held at per-wave cap 0 and never
// claims, so it is unaffected. Override an id via PARTNER_DRIP_BRAND_SES_PROFILES
// (comma-separated brand=profileUUID).
func dripBrandSESProfiles() map[string]string {
	m := map[string]string{
		"db": "93938919-2df7-40c4-ba68-4f3e301e5b05", "ht": "c24a8455-e893-4895-a8ad-4556d9013003",
		"mh": "81b1e46b-b0cc-4f1a-9667-e09452ad6fa4", "qf": "85cb59ed-b45a-427b-b5c9-ad7a67142697",
		"bwp": "651cff33-beb5-49a9-bd75-70f11c4043a7", "ci": "80d254e6-4aa3-4e45-baed-353b5319efd5",
		"cp": "d34e6b74-7d92-47ab-bc10-9f929b2cc135", "fc": "d0e552fd-21d8-4aaa-85e5-358577874ab8",
		"hws": "c9e5e308-9c8b-481e-b6ee-5843a45fe148", "lpl": "d93a31f4-1daa-40a3-9c27-926a8e7e4c65",
		"mrd": "5b1aa1f5-e38e-4c79-8f81-45345b59f2d1", "rb": "c58d1939-eaf1-4f5e-9922-6f9b59d9be6d",
		"rru": "9a66ef56-0b35-4ab6-8ea1-af482671c441", "tot": "7da27070-d17a-4475-8564-1ded7bbaea8e",
		"wfy": "98888308-ebe2-4ba8-b618-eaa2215c26b1", "yih": "9fffe735-a57c-4d02-adca-d49bfeb94da6",
	}
	if v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_BRAND_SES_PROFILES")); v != "" {
		for _, pair := range strings.Split(v, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) == 2 {
				if b, id := strings.ToLower(strings.TrimSpace(kv[0])), strings.TrimSpace(kv[1]); b != "" && id != "" {
					m[b] = id
				}
			}
		}
	}
	return m
}

// dripRouteAllSES reports whether the whole drip defaults to the SES relay
// (operator 2026-06-27). Default ON; set PARTNER_DRIP_ROUTE_ALL_SES=false to
// restore the per-(brand,ISP)-pin PMTA-default behavior.
func dripRouteAllSES() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_ROUTE_ALL_SES"))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// claimGroup is a subset of a wave's claimed records routed to one campaign.
// recs and subIDs are index-aligned. profileID == "" means the default PMTA
// route (resolve by SendingDomain); a non-empty UUID pins an SES tenant profile.
type claimGroup struct {
	profileID string
	recs      []claimedRecord
	subIDs    []string
}

// partitionWaveBySESProfile splits a wave's claimed records (and index-aligned
// subscriber ids) into routing groups: one default-PMTA group plus one group per
// distinct SES tenant profile pinned for (brand, isp) in dripBrandISPSESProfiles.
// Brands/ISPs with no pin all fall into the "" (PMTA) group, so when nothing is
// pinned the result is a single group identical to the pre-split behavior.
// Groups are returned PMTA-first, then SES groups in deterministic profile order.
func partitionWaveBySESProfile(brand string, recs []claimedRecord, subIDs []string) []claimGroup {
	lb := strings.ToLower(strings.TrimSpace(brand))
	pins := dripBrandISPSESProfiles()[lb]
	defaultSES := ""
	if dripRouteAllSES() {
		defaultSES = dripBrandSESProfiles()[lb] // whole drip defaults to the brand's SES relay
	}
	byProfile := map[string]*claimGroup{}
	order := []string{}
	for i, r := range recs {
		if i >= len(subIDs) {
			break
		}
		pid := defaultSES
		if pins != nil {
			if p, ok := pins[strings.ToLower(strings.TrimSpace(r.ispFamily))]; ok {
				pid = p // explicit (brand,ISP) pin still wins
			}
		}
		g, ok := byProfile[pid]
		if !ok {
			g = &claimGroup{profileID: pid}
			byProfile[pid] = g
			order = append(order, pid)
		}
		g.recs = append(g.recs, r)
		g.subIDs = append(g.subIDs, subIDs[i])
	}
	sort.Strings(order) // "" sorts first → PMTA group leads, SES groups follow deterministically
	out := make([]claimGroup, 0, len(order))
	for _, pid := range order {
		out = append(out, *byProfile[pid])
	}
	return out
}

// deployWaveGroup ships ONE campaign for a routing group of a wave (see
// partitionWaveBySESProfile): builds the per-group segment, builds the campaign
// input, pins the SES profile + SES sending domain when profileID is set,
// deploys, stamps partner attribution, and marks the group's records mailed.
// nameSuffix (e.g. "[t2]") is appended to the campaign name when non-empty.
func (po *PartnerDripOrchestrator) deployWaveGroup(ctx context.Context, v verticalState, brand string, creative creativeRec, g claimGroup, nameSuffix string) (string, error) {
	segmentID, err := po.createWaveSegment(ctx, v, brand, g.recs, g.subIDs)
	if err != nil {
		return "", fmt.Errorf("create_wave_segment: %w", err)
	}
	input, err := po.buildCampaignInput(v, brand, creative, segmentID, tallyISPs(g.recs))
	if err != nil {
		return "", fmt.Errorf("build_input: %w", err)
	}
	if g.profileID != "" {
		// Pin the SES tenant profile — this bypasses the by-domain PMTA lookup
		// (see PMTACampaignInput.SendingProfileID). Match SendingDomain to the
		// SES tenant so footer/tracking-domain logic stays consistent.
		input.SendingProfileID = g.profileID
		if d, ok := brandSESSendingDomain[strings.ToLower(strings.TrimSpace(brand))]; ok {
			input.SendingDomain = d
		} else if strings.HasPrefix(input.SendingDomain, "em.") {
			// Every brand's SES tenant is m.<apex> (mirrors em.<apex> PMTA); derive it
			// so ALL brands route through the SES relay when pinned — not just the
			// two static ht/mh entries above (2026-06-25: relay ISPs were still
			// hitting PMTA because the SES profile was set but the domain stayed em.).
			input.SendingDomain = "m." + strings.TrimPrefix(input.SendingDomain, "em.")
		}
		// Disambiguate the name: PMTA + SES groups of one wave share the same
		// creative (and thus the same base name); tag SES groups so the two
		// campaigns are distinct (unique per profile within a wave).
		input.Name = fmt.Sprintf("%s [ses:%s]", input.Name, shortID(g.profileID))
	}
	if nameSuffix != "" {
		input.Name = fmt.Sprintf("%s %s", input.Name, nameSuffix)
	}
	campaignID, err := po.cfg.DeployFn(ctx, input)
	if err != nil {
		// ErrDeployNameReused means the idempotency guard converged on a DIFFERENT
		// wave's live campaign — by definition a campaign with this name already
		// exists, so the by-name recovery below must NOT run (it would stamp this
		// group's records onto another group's audience, the 2026-08-11 loss).
		// Keep the release path for it.
		if errors.Is(err, ErrDeployNameReused) {
			return "", fmt.Errorf("deploy: %w", err)
		}
		// 2026-08-21 double-send defect: HandleDeployCampaign COMMITS the campaign
		// row and only then runs a post-commit verification query; when that query
		// times out the handler returns 500 with the campaign already live.
		// Treating that as a failed deploy released the claim, and the next tick
		// re-claimed and RE-MAILED the same people (55 double-mailed). The name is
		// unique per wave group (buildCampaignInput folds in the per-group segment
		// nonce), so a live campaign under this exact name IS this deploy —
		// recover it and finish the bookkeeping instead of releasing.
		if id, ok := po.findDeployedCampaignByName(ctx, input.Name); ok {
			log.Printf("[PartnerDripOrchestrator] post-deploy-error campaign found by name — treating as deployed: campaign=%s name=%q deploy_err=%v",
				id, input.Name, err)
			campaignID = id
		} else {
			return "", fmt.Errorf("deploy: %w", err)
		}
	}
	if err := po.stampPartnerAttributionOnCampaign(ctx, campaignID, v.datasetID, v.partnerSlug, v.vertical); err != nil {
		log.Printf("[PartnerDripOrchestrator] stamp_attribution (campaign=%s dataset=%s): %v", campaignID, v.datasetID, err)
	}
	// markMailed does its own chunked retries, records any residual loss durably
	// in partner_drip_stamp_failures, and the always-on recovery pass replays it
	// from mailing_message_log — the campaign is already live, so the wave itself
	// is a success either way. Log the loss loudly with counts; never silent.
	if err := po.markMailed(ctx, g.recs, campaignID, brand); err != nil {
		log.Printf("[PartnerDripOrchestrator] ALERT mark_mailed failed post-deploy campaign=%s brand=%s rows=%d — loss durably recorded (partner_drip_stamp_failures), stamp recovery replays it from mailing_message_log: %v",
			campaignID, brand, len(g.recs), err)
	}
	return campaignID, nil
}

// findDeployedCampaignByName reports whether a campaign with this wave group's
// (unique-per-group, see buildCampaignInput) name exists in a live state —
// the post-deploy-error probe for the "handler committed, then 500'd on its
// post-commit verification" defect. Runs on a context detached from
// cancellation with its own short deadline: the deploy error may itself be a
// context timeout, and this one bounded read decides mailed-vs-release.
func (po *PartnerDripOrchestrator) findDeployedCampaignByName(ctx context.Context, name string) (string, bool) {
	if strings.TrimSpace(name) == "" || po.db == nil {
		return "", false
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var id string
	err := po.db.QueryRowContext(wctx, `
		SELECT id::text
		FROM mailing_campaigns
		WHERE organization_id = $1::uuid
		  AND name = $2
		  AND status NOT IN ('failed', 'cancelled', 'deleted')
		ORDER BY created_at DESC
		LIMIT 1
	`, po.cfg.OrganizationID, name).Scan(&id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[PartnerDripOrchestrator] post-deploy-error campaign-by-name lookup failed name=%q: %v — treating deploy as failed (release path)", name, err)
		}
		return "", false
	}
	return id, true
}

// routeLabel renders a routing group's route for logs: "kumo" for a governed
// (Kumo) brand's default by-domain route, "pmta" for every other default
// by-domain route, or "ses:<profile>" when pinned to an SES tenant profile.
// Log/label-only — nothing parses this (verified: the only consumers are the
// two deployWaveGroups log lines).
func routeLabel(profileID, brand string) string {
	if profileID == "" {
		if governedBrands[strings.ToLower(strings.TrimSpace(brand))] {
			return "kumo"
		}
		return "pmta"
	}
	return "ses:" + profileID
}

// deployWaveGroups partitions a wave's claimed records by SES routing and deploys
// each group as its own campaign (PMTA remainder + one campaign per pinned SES
// tenant). Returns the last campaign id and the total records actually deployed.
// A group whose deploy fails releases ONLY its own records (the next tick retries
// them) so a partial failure never unships the groups that already mailed.
func (po *PartnerDripOrchestrator) deployWaveGroups(ctx context.Context, v verticalState, brand string, creative creativeRec, claimed []claimedRecord, subscriberIDs []string, nameSuffix string) (lastCampaignID string, deployedCount int) {
	for _, g := range partitionWaveBySESProfile(brand, claimed, subscriberIDs) {
		cid, err := po.deployWaveGroup(ctx, v, brand, creative, g, nameSuffix)
		if err != nil {
			_ = po.releaseClaim(ctx, g.recs)
			log.Printf("[PartnerDripOrchestrator] wave group deploy failed: vertical=%s brand=%s route=%s size=%d: %v",
				v.vertical, brand, routeLabel(g.profileID, brand), len(g.recs), err)
			continue
		}
		lastCampaignID = cid
		deployedCount += len(g.recs)
		log.Printf("[PartnerDripOrchestrator] wave fired: vertical=%s brand=%s campaign=%s size=%d route=%s creative=%s",
			v.vertical, brand, cid, len(g.recs), routeLabel(g.profileID, brand), creative.filename)
	}
	return lastCampaignID, deployedCount
}

// passContext parameterizes processVerticalWith so one wave builder serves both
// the welcome pass (16 normal brands) and the additive governed pass (Kumo
// properties). It carries (a) the rotation roster the brand is picked from and
// (b) the partner_drip_state key the rotation pointer is read from / written to.
// The welcome pass uses {roster: brandRosterFor(v.vertical), stateKey: v.vertical}
// (byte-identical to the pre-refactor processVertical); the governed pass uses
// {roster: orderedSubscribedGoverned(v.vertical), stateKey: v.vertical+":governed"}
// so the two rotations never share a pointer.
type passContext struct {
	roster   []string // brands to round-robin
	stateKey string   // partner_drip_state.vertical key for the rotation pointer
}

// processVertical preserves the original entrypoint for the welcome pass: it
// builds the welcome passContext (roster + stateKey identical to today) and
// delegates to processVerticalWith. This keeps the 16-brand welcome path
// byte-identical after the Option B parameterization.
func (po *PartnerDripOrchestrator) processVertical(ctx context.Context, v verticalState) error {
	return po.processVerticalWith(ctx, v, passContext{
		roster:   brandRosterFor(v.vertical),
		stateKey: v.vertical,
	})
}

func (po *PartnerDripOrchestrator) processVerticalWith(ctx context.Context, v verticalState, pc passContext) error {
	waveSize := po.computeWaveSize(v)
	if waveSize <= 0 {
		return nil
	}
	brand, newIdx, err := po.pickNextBrand(ctx, v, pc.roster)
	if err != nil {
		return fmt.Errorf("pick_brand: %w", err)
	}
	creative, err := po.resolveCreativeForVertical(ctx, v, brand)
	if err != nil {
		return fmt.Errorf("resolve_creative: %w", err)
	}

	// ISP-aware claim: pull up to `cap` records per ISP-family from the
	// queue (oldest-first within each ISP), bounded by waveSize as a
	// safety ceiling. Replaces the old oldest-first-across-all-ISPs claim
	// that wasted wave slots on yahoo records that would later be
	// deferred and released. See claimRecordsByISPCaps for the full
	// rationale and the May 14 cap bump.
	perISPCaps, err := po.resolvePerISPCaps(ctx, v.vertical, v.datasetID, ispCapBacklogReady)
	if err != nil {
		return fmt.Errorf("resolve_isp_caps: %w", err)
	}
	if governedBrands[strings.ToLower(strings.TrimSpace(brand))] {
		// Property governor (Kumo properties): clamp each ISP's wave cap to the
		// property's remaining (property,vertical,ISP) daily budget, force gmail=0,
		// and apply 6h rate-limit pacing. A governed brand not subscribed to this
		// vertical yields an all-zero cap map (claims nothing) — belt-and-suspenders
		// against a roster-membership/subscription mismatch. Runs BEFORE warmup /
		// daily-budget / brand-routing so the governor is the sole ceiling for these
		// brands (it already min()s against resolvePerISPCaps' base, so platform-wide
		// ISP doctrine still bounds it from above).
		//
		// SAFEGUARD (a) — FLOOR GATE: only claim an ISP where the per-wave cap
		// BINDS (huge pool). When resolvePerISPCaps' drain-horizon cap for an ISP
		// is below the static PerISPCapPerWave base (thin pool → drain-horizon
		// binds → the normal brands' caps float with ready depth), governed
		// consumption would lower the normal brands' next caps. Force that ISP's
		// governed cap to 0 so the governed pass NEVER competes for a thin pool.
		// This is what makes the governed pass zero-side-effect on the 16 brands.
		perISPCaps = po.applyGovernedFloorGate(ctx, v.vertical, v.datasetID, perISPCaps)
		perISPCaps = po.applyPropertyGovernor(ctx, brand, v.vertical, perISPCaps)
	} else if warmupRosterBrands[strings.ToLower(strings.TrimSpace(brand))] {
		// Warm-up brands (fresh KumoMTA IPs on a dedicated verticalBrandRoster,
		// e.g. samsclub_internal -> mpf/pmd/trb) send ONLY yahoo+aol — the Sam's
		// Yahoo/AOL digest. Bypass the mature-brand allow-list (applyISPBrandRouting
		// would zero yahoo/aol for any non-mature brand, leaving only ungated ISPs
		// like sbcglobal) and the daily-budget gate; every non-yahoo/aol ISP is
		// zeroed so a fresh IP can never drift onto sbcglobal/att/etc. The yahoo/aol
		// caps stay at resolvePerISPCaps' value (the gentle dataset override).
		perISPCaps = keepOnlyISPCaps(perISPCaps, "yahoo", "aol")
	} else {
		// Per-brand daily new-record budget (operator 2026-06-10): clamp the
		// wave's per-ISP caps to what this brand may still mail TODAY. Gmail is
		// additionally brand-gated. Follow-ups bypass this (separate loop).
		perISPCaps = po.applyNewRecordDailyBudget(ctx, brand, v.datasetID, perISPCaps)
		// Deliverability routing (operator 2026-06-13): pin the hard,
		// engagement-priced ISPs to the warmed mature-4 domains. When a
		// non-allowed brand waves, those ISPs' caps go to 0 (claim skips any
		// non-positive cap), so they only ship from db/ht/mh/qf.
		perISPCaps = po.applyISPBrandRouting(brand, perISPCaps)
		// Per-(sending-domain × ISP) introduction budgets (operator 2026-08-15):
		// clamp each ISP's cap to the domain's remaining warming budget for
		// today (partner_drip_brand_budgets, welcome pass only). When the clamp
		// zeroes a wave that otherwise had capacity, SKIP the brand and advance
		// the rotation — without the advance, a budget-exhausted brand would be
		// re-picked every tick (updateDripState only runs after a deploy) and
		// pin the vertical for the rest of the day.
		preBudget := perISPCaps
		perISPCaps = po.applyBrandIntroBudgets(ctx, brand, perISPCaps)
		if capsAnyPositive(preBudget) && !capsAnyPositive(perISPCaps) {
			if err := po.advanceBrandRotation(ctx, pc.stateKey, newIdx); err != nil {
				log.Printf("[PartnerDripOrchestrator] advance rotation past budget-exhausted brand=%s vertical=%s: %v", brand, v.vertical, err)
			} else {
				log.Printf("[PartnerDripOrchestrator] brand=%s budget-exhausted for vertical=%s — skipped, rotation advanced", brand, v.vertical)
			}
			return nil
		}
	}
	// Apple-banned verticals (operator 2026-06-16): Fidelity term-life is HM08-rejected by Apple
	// ~85-92% (content policy — no ESP lever). Zero the apple cap so no Apple/iCloud records are
	// claimed for these verticals; every other ISP is unaffected.
	if appleBannedDripVerticals()[strings.ToLower(strings.TrimSpace(v.vertical))] {
		perISPCaps["apple"] = 0
	}
	// AOL round-robin: while the rotated companion pass owns AOL for this
	// vertical, the roster brand's own wave leaves AOL alone — one path, so
	// the lane's AOL volume spreads across domains instead of doubling.
	if aolRotationActive(v.vertical) {
		perISPCaps["aol"] = 0
	}
	claimed, err := po.claimRecordsByISPCaps(ctx, v.vertical, perISPCaps, waveSize)
	if err != nil {
		return fmt.Errorf("claim_records: %w", err)
	}
	if len(claimed) == 0 {
		return nil
	}

	// Apply throttle-based ISP deferral + per-ISP cap. Records cut from this
	// wave are released back to 'ready' so the next tick can revisit them
	// (potentially against a different brand whose throttle state differs).
	keep, deferred, deferralReasons, err := po.applyThroughputSafety(ctx, brand, claimed, perISPCaps)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] throughput_safety check failed for vertical=%s: %v — proceeding without deferral", v.vertical, err)
		keep = claimed
	}
	if len(deferred) > 0 {
		if relErr := po.releaseClaim(ctx, deferred); relErr != nil {
			log.Printf("[PartnerDripOrchestrator] release deferred: %v", relErr)
		}
		log.Printf("[PartnerDripOrchestrator] deferred %d records for vertical=%s reasons=%v",
			len(deferred), v.vertical, deferralReasons)
	}
	if len(keep) == 0 {
		log.Printf("[PartnerDripOrchestrator] vertical=%s: all claimed records deferred — skipping wave", v.vertical)
		return nil
	}
	claimed = keep

	subscriberIDs, err := po.promoteToSubscribers(ctx, v, claimed)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("promote_subscribers: %w", err)
	}

	// Split the wave by SES routing and deploy each group as its own campaign
	// (PMTA remainder + one campaign per pinned SES tenant — e.g. ht microsoft →
	// HT SES tenant). When nothing is pinned this is a single PMTA campaign,
	// identical to the pre-split behavior.
	lastCampaignID, deployedCount := po.deployWaveGroups(ctx, v, brand, creative, claimed, subscriberIDs, "")
	if deployedCount == 0 {
		return fmt.Errorf("all wave groups failed to deploy for vertical=%s brand=%s", v.vertical, brand)
	}
	if err := po.updateDripState(ctx, pc.stateKey, newIdx, brand, lastCampaignID, deployedCount); err != nil {
		log.Printf("[PartnerDripOrchestrator] update_state: %v", err)
	}
	return nil
}

// computeWaveSize divides remaining queue by waves remaining in the flush
// window, clamped to MIN/MAX.
//
// 2026-05-14 change: the historical "spread evenly across the remaining
// flush window" formula (readyCount / wavesRemaining) was the dominant
// throughput choke. Early in a dataset's life-cycle wavesRemaining was
// near 96 and the wave shrank to ~280 records, far below the per-ISP
// cap budget — so gmail/microsoft/apple records trickled while the
// queue grew. With per-ISP caps now enforced natively at claim time
// (claimRecordsByISPCaps), the wave is naturally smoothed to
// sum(per-ISP caps) regardless of waveSize. Returning MaxWaveSize lets
// the per-ISP caps act as the true throughput regulator and lets
// backlogs drain at the published per-ISP cadence. See operator note
// 2026-05-14 ("INCREASE THROUGHPUT. Especially for GMAIL.") for context.
func (po *PartnerDripOrchestrator) computeWaveSize(v verticalState) int {
	if v.readyCount <= 0 {
		return 0
	}
	size := po.cfg.MaxWaveSize
	if size > v.readyCount {
		size = v.readyCount
	}
	if size < po.cfg.MinWaveSize {
		size = po.cfg.MinWaveSize
	}
	return size
}

// pickNextBrand walks the given roster's round-robin starting at v.brandIndex
// and skips any brand the operator has paused (via PausedBrandPredicate).
// Returns the chosen brand + the next index to write to partner_drip_state.
// The roster is passed in (welcome pass: brandRosterFor; governed pass:
// orderedSubscribedGoverned) so one walker serves both rotations.
func (po *PartnerDripOrchestrator) pickNextBrand(ctx context.Context, v verticalState, roster []string) (string, int, error) {
	if len(roster) == 0 {
		return "", v.brandIndex, fmt.Errorf("empty roster — no brand available")
	}
	for offset := 0; offset < len(roster); offset++ {
		idx := (v.brandIndex + offset) % len(roster)
		brand := roster[idx]
		if po.cfg.PausedBrandPredicate != nil && po.cfg.PausedBrandPredicate(ctx, brand) {
			continue
		}
		next := (idx + 1) % len(roster)
		return brand, next, nil
	}
	return "", v.brandIndex, fmt.Errorf("all brands paused — no brand available")
}

type creativeRec struct {
	filename  string
	subject   string
	preheader string
	fromName  string
	htmlBody  string
	// offerID is the PER-TOUCH offer bound to this creative row (nullable
	// partner_drip_creatives.offer_id / partner_drip_followup_creatives.offer_id).
	// When non-empty it OVERRIDES the dataset's offerID for BOTH the deploy
	// payload's OfferID (so this touch scrubs THIS offer's DNM/offer-suppression)
	// and for offer/money attribution. Empty ("") = fall back to the dataset's
	// offerID exactly as before (no behavior change for existing single-offer drips).
	offerID string
}

// resolveCreativeForVertical returns the creative the orchestrator will use
// for the next wave. It dispatches between two backing stores:
//
//  1. Direct-offer datasets (verticalState.offerID set) — pull a creative
//     from mailing_offer_creatives + a subject from mailing_offer_subject_lines
//     + a from-name from mailing_offer_from_names. Subject + from-name rotate
//     deterministically by wave time so the partner sees the full pool. The
//     HTML lives in mailing_offer_creatives.html_content (already CAN-SPAM
//     footer-injected at upload time by appendUnsubDisclaimer — brand footer at
//     the bottom, never the corporate identity).
//
//  2. Drip-pool datasets (offerID empty) — legacy path, looks up
//     partner_drip_creatives keyed by (vertical, brand) and reads HTML from
//     docs/emails/<filename>.
//
// Either path returns a populated creativeRec with htmlBody pre-loaded so
// the caller doesn't need to know which path produced it.
func (po *PartnerDripOrchestrator) resolveCreativeForVertical(ctx context.Context, v verticalState, brand string) (creativeRec, error) {
	var c creativeRec
	var err error
	if v.offerID != "" {
		c, err = po.resolveOfferCreative(ctx, v.offerID, brand)
	} else {
		c, err = po.resolveCreative(ctx, v.vertical, brand)
	}
	if err != nil {
		return c, err
	}
	c.htmlBody = ensureMoneyLinkAttribution(c.htmlBody)
	return c, nil
}

// dripMoneyHostHrefRE matches money-network hrefs across every affiliate host
// the click enroller recognizes (moneySlugRes), excluding nothing by host —
// the /integration/ unsub URLs are filtered post-match, mirroring
// appendTrackingToCratoolpro (send_day_creative_resolve.go). Group 1 = URL
// through the path, group 2 = optional query string.
var dripMoneyHostHrefRE = regexp.MustCompile(
	`(?i)(https?://(?:www\.)?(?:cratoolpro|eos57ytf|xnonu|muqes|k8k0hfdt|codefortwo)\.com/[A-Za-z0-9_/.\-]+)(\?[^"'\s>]*)?`,
)

// dripMoneyAttributionSuffix carries the Everflow attribution params every
// money link must ship: sub1 = subscriber UUID, sub3 = campaign UUID. The
// UPPERCASE merge tags are substituted at send time by BOTH send paths —
// send_worker.ReplaceTrackingMergeTags for drip/broadcast campaigns and the
// click-drip reminder sender (which calls the same function). sub2 (brand
// domain) is deliberately omitted: drip creatives are brand-agnostic and the
// campaign-send path has no lowercase {{brand.domain}} substitution; sub3's
// campaign resolves the brand instead. The 2026-07-09/10 Empire conversions
// arrived sub-less and permanently unattributable because the creative links
// carried only creative_id — this guard makes that class impossible.
const dripMoneyAttributionSuffix = "source_id=email&sub1={{SUBSCRIBER_ID}}&sub3={{CAMPAIGN_ID}}"

// ensureMoneyLinkAttribution appends dripMoneyAttributionSuffix to every
// money-network URL in html that does not already carry source_id=.
// Idempotent; /integration/ unsub URLs (which carry ?_redir= tokens that must
// not be mutated) are left untouched.
func ensureMoneyLinkAttribution(html string) string {
	matches := dripMoneyHostHrefRE.FindAllStringSubmatchIndex(html, -1)
	if len(matches) == 0 {
		return html
	}
	var b strings.Builder
	b.Grow(len(html) + len(matches)*(len(dripMoneyAttributionSuffix)+1))
	cursor := 0
	for _, m := range matches {
		matchStart, matchEnd := m[0], m[1]
		base := html[m[2]:m[3]]
		qs := ""
		if m[4] >= 0 && m[5] >= 0 {
			qs = html[m[4]:m[5]]
		}
		if strings.Contains(base, "/integration/") || strings.Contains(qs, "source_id=") {
			b.WriteString(html[cursor:matchEnd])
			cursor = matchEnd
			continue
		}
		b.WriteString(html[cursor:matchStart])
		b.WriteString(base)
		if qs != "" {
			b.WriteString(qs)
			if !strings.HasSuffix(qs, "&") {
				b.WriteString("&")
			}
		} else {
			b.WriteString("?")
		}
		b.WriteString(dripMoneyAttributionSuffix)
		cursor = matchEnd
	}
	b.WriteString(html[cursor:])
	return b.String()
}

func (po *PartnerDripOrchestrator) resolveCreative(ctx context.Context, vertical, brand string) (creativeRec, error) {
	var c creativeRec
	var offerID sql.NullString
	err := po.db.QueryRowContext(ctx, `
		SELECT creative_filename, subject_line, COALESCE(preheader, ''), from_name, offer_id
		FROM partner_drip_creatives
		WHERE vertical = $1 AND brand = $2 AND active = true
	`, vertical, brand).Scan(&c.filename, &c.subject, &c.preheader, &c.fromName, &offerID)
	if err != nil {
		return c, fmt.Errorf("creative lookup (%s/%s): %w", vertical, brand, err)
	}
	if offerID.Valid {
		c.offerID = strings.TrimSpace(offerID.String)
	}
	// Per-touch offer WITHOUT a bespoke creative file: pull the creative content
	// from the offer-center tables for THIS touch's offer (mailing_offer_creatives
	// / subjects / from-names), but preserve c.offerID so the deploy stamps the
	// per-touch offer's suppression scrub + attribution. When the row DOES carry a
	// filename its copy takes precedence (disk-read path below) and we only inherit
	// offer_id for the scrub. Empty offer_id keeps the exact legacy behavior.
	if c.offerID != "" && strings.TrimSpace(c.filename) == "" {
		oc, err := po.resolveOfferCreative(ctx, c.offerID, brand)
		if err != nil {
			return c, err
		}
		oc.offerID = c.offerID
		return oc, nil
	}
	if subj, pre, fn, ok := po.rotateCopyLines(ctx, vertical, brand); ok {
		if subj != "" {
			c.subject = subj
		}
		if pre != "" {
			c.preheader = pre
		}
		if fn != "" {
			c.fromName = fn
		}
	}
	body, err := os.ReadFile(filepath.Join(po.cfg.CreativesDir, c.filename))
	if err != nil {
		return c, fmt.Errorf("read creative %s: %w", c.filename, err)
	}
	c.htmlBody = string(body)
	return c, nil
}

// rotateCopyLines picks subject, preheader, and from_name from
// partner_drip_copy_lines when seeded for a vertical. Each pool rotates
// independently so copy tests can mix strict from-names with subject lines.
func (po *PartnerDripOrchestrator) rotateCopyLines(ctx context.Context, vertical, brand string) (subject, preheader, fromName string, ok bool) {
	subjects, err := po.fetchCopyLines(ctx, vertical, "subject")
	if err != nil || len(subjects) == 0 {
		return "", "", "", false
	}
	preheaders, err := po.fetchCopyLines(ctx, vertical, "preheader")
	if err != nil {
		preheaders = nil
	}
	if len(preheaders) == 0 {
		preheaders = subjects
	}
	fromNames, err := po.fetchCopyLines(ctx, vertical, "from_name")
	if err != nil {
		fromNames = nil
	}
	bucket := time.Now().UTC().Truncate(5 * time.Minute).Unix()
	rotKey := fmt.Sprintf("%s|%s|%d", vertical, brand, bucket)
	rotSHA := sha256.Sum256([]byte(rotKey))
	subIdx := int(rotSHA[0])<<8 | int(rotSHA[1])
	preIdx := int(rotSHA[2])<<8 | int(rotSHA[3])
	fn := ""
	if len(fromNames) > 0 {
		fnIdx := int(rotSHA[4])<<8 | int(rotSHA[5])
		fn = fromNames[fnIdx%len(fromNames)]
	}
	return subjects[subIdx%len(subjects)], preheaders[preIdx%len(preheaders)], fn, true
}

func (po *PartnerDripOrchestrator) fetchCopyLines(ctx context.Context, vertical, kind string) ([]string, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT copy_text FROM partner_drip_copy_lines
		WHERE vertical = $1 AND line_kind = $2 AND active = true
		  AND COALESCE(copy_text, '') <> ''
		ORDER BY sort_order, id
	`, vertical, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 32)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// pairedCreative is one CREATIVE+SUBJECT(+FROM-NAME) pair: a
// mailing_offer_creatives row whose subject_line_id resolves to a live
// subject. Only rows that declare that link participate in paired rotation.
type pairedCreative struct {
	htmlBody string
	subject  string
	fromName string // "" when from_name_id is NULL — falls back to the pool
}

// fetchPairedCreatives returns the offer's DECLARED creative+subject pairs, in
// a stable order (id), or an empty slice when the offer declares none.
//
// A pair exists only when mailing_offer_creatives.subject_line_id points at a
// live subject row. That link is the operator's opt-in: with fewer than two
// pairs the caller keeps the historical single-creative behaviour exactly, so
// enabling this cannot disturb an offer nobody has paired. As of 2026-08-19
// ZERO of the 112 live creative rows carry the link, and 13 offers (Fidelity
// 35 creatives, CarShield 11, Metal Roofing 5, Empire Today 3 …) hold multiple
// creatives where an operator deliberately chose ONE canonical row — those must
// keep serving that row until they are paired on purpose.
func (po *PartnerDripOrchestrator) fetchPairedCreatives(ctx context.Context, offerID string) ([]pairedCreative, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT c.html_content, s.subject_line, COALESCE(f.from_name, '')
		FROM mailing_offer_creatives c
		JOIN mailing_offer_subject_lines s
		  ON s.id = c.subject_line_id
		 AND s.offer_id = c.offer_id
		 AND COALESCE(s.status, '') NOT IN ('archived','rejected')
		 AND COALESCE(s.subject_line, '') <> ''
		LEFT JOIN mailing_offer_from_names f
		  ON f.id = c.from_name_id
		 AND f.offer_id = c.offer_id
		 AND COALESCE(f.status, '') NOT IN ('archived','rejected')
		 AND COALESCE(f.from_name, '') <> ''
		WHERE c.offer_id = $1
		  AND c.subject_line_id IS NOT NULL
		  AND COALESCE(c.status, '') NOT IN ('archived','rejected')
		  AND COALESCE(c.html_content, '') <> ''
		ORDER BY c.id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]pairedCreative, 0, 8)
	for rows.Next() {
		var p pairedCreative
		if err := rows.Scan(&p.htmlBody, &p.subject, &p.fromName); err == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// resolveOfferCreative pulls a fresh creative+subject+from-name from the
// offer-center tables for a direct-offer-bound dataset. Selection rules:
//
//   - PAIRED ROTATION (opt-in, 2026-08-19). When the offer declares two or
//     more creative+subject pairs — a mailing_offer_creatives row carrying a
//     subject_line_id — the whole PAIR rotates together on the wave bucket, so
//     a creative always ships with the subject written for it. Rotating them
//     independently is what this exists to prevent.
//
//   - Otherwise (the historical path): pick the latest non-archived creative,
//     preferring status='approved' over 'generated'. We do NOT rotate
//     creatives — the operator decides which one is canonical — and the
//     subject rotates on its own from the offer's pool.
//
//   - Subject + from-name rotation hashes (offerID, brand, 5-min wave bucket)
//     into a stable index so successive waves get different items but a retry
//     of the same wave hits the same row.
//
// All three pools must be non-empty. If any are empty we fail loud — the
// orchestrator's caller releases the claim and the next tick retries.
func (po *PartnerDripOrchestrator) resolveOfferCreative(ctx context.Context, offerID, brand string) (creativeRec, error) {
	var c creativeRec
	c.filename = "offer:" + offerID

	// Hash bucket for rotation. Granularity 5 minutes — successive waves get
	// different rows; an immediate retry of the same wave is stable.
	bucket := time.Now().UTC().Truncate(5 * time.Minute).Unix()
	rotKey := fmt.Sprintf("%s|%s|%d", offerID, brand, bucket)
	rotSHA := sha256.Sum256([]byte(rotKey))
	rotIdx := int(rotSHA[0])<<8 | int(rotSHA[1])

	// Paired rotation first. Two or more declared pairs is the opt-in signal;
	// a single pair (or none) falls through to the historical path so the
	// offer behaves exactly as it did before it was paired.
	pairs, perr := po.fetchPairedCreatives(ctx, offerID)
	if perr != nil {
		return c, fmt.Errorf("offer_paired_creatives lookup (offer=%s): %w", offerID, perr)
	}
	if len(pairs) > 1 {
		p := pairs[rotIdx%len(pairs)]
		c.htmlBody = p.htmlBody
		c.subject = p.subject
		// Preheader still rotates off the subject pool — pairing governs the
		// creative+subject bond, which is what drives inbox consistency.
		subjects, err := po.fetchOfferSubjects(ctx, offerID)
		if err != nil {
			return c, fmt.Errorf("offer_subjects lookup: %w", err)
		}
		if len(subjects) > 0 {
			preIdx := int(rotSHA[2])<<8 | int(rotSHA[3])
			c.preheader = subjects[preIdx%len(subjects)]
		}
		fromNames, err := po.fetchOfferFromNames(ctx, offerID)
		if err != nil {
			return c, fmt.Errorf("offer_from_names lookup: %w", err)
		}
		if len(fromNames) == 0 {
			return c, fmt.Errorf("offer %s has no active from-names — operator must seed at least one before deploying", offerID)
		}
		switch {
		case p.fromName != "":
			c.fromName = p.fromName
		default:
			if matched, ok := po.matchBrandOfferFromName(ctx, brand, fromNames); ok {
				c.fromName = matched
			} else {
				c.fromName = fromNames[rotIdx%len(fromNames)]
			}
		}
		return c, nil
	}

	// Creative HTML — prefer 'approved' over 'generated' over anything else.
	if err := po.db.QueryRowContext(ctx, `
		SELECT html_content
		FROM mailing_offer_creatives
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		  AND COALESCE(html_content, '') <> ''
		ORDER BY (CASE COALESCE(status, '')
		            WHEN 'approved' THEN 0
		            WHEN 'generated' THEN 1
		            ELSE 2
		          END), updated_at DESC
		LIMIT 1
	`, offerID).Scan(&c.htmlBody); err != nil {
		return c, fmt.Errorf("offer_creative lookup (offer=%s): %w", offerID, err)
	}

	// Subject pool.
	subjects, err := po.fetchOfferSubjects(ctx, offerID)
	if err != nil {
		return c, fmt.Errorf("offer_subjects lookup: %w", err)
	}
	if len(subjects) == 0 {
		return c, fmt.Errorf("offer %s has no active subjects — operator must seed at least one before deploying", offerID)
	}
	c.subject = subjects[rotIdx%len(subjects)]
	preIdx := int(rotSHA[2])<<8 | int(rotSHA[3])
	c.preheader = subjects[preIdx%len(subjects)]

	// From-name pool — prefer the row matching this brand's blog persona when the
	// pool uses a "Partner - {friendly}" prefix (e.g. CarShield direct offers).
	fromNames, err := po.fetchOfferFromNames(ctx, offerID)
	if err != nil {
		return c, fmt.Errorf("offer_from_names lookup: %w", err)
	}
	if len(fromNames) == 0 {
		return c, fmt.Errorf("offer %s has no active from-names — operator must seed at least one before deploying", offerID)
	}
	if matched, ok := po.matchBrandOfferFromName(ctx, brand, fromNames); ok {
		c.fromName = matched
	} else {
		c.fromName = fromNames[rotIdx%len(fromNames)]
	}

	return c, nil
}

func (po *PartnerDripOrchestrator) fetchOfferSubjects(ctx context.Context, offerID string) ([]string, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT subject_line FROM mailing_offer_subject_lines
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		  AND COALESCE(subject_line, '') <> ''
		ORDER BY id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// matchBrandOfferFromName picks the from-name row that ends with this brand's
// mailing_brand_metadata.from_name when the pool uses a partner prefix pattern.
func (po *PartnerDripOrchestrator) matchBrandOfferFromName(ctx context.Context, brand string, fromNames []string) (string, bool) {
	domain, ok := resolveBrandSendingDomain(brand)
	if !ok || domain == "" {
		return "", false
	}
	var metaFrom string
	err := po.db.QueryRowContext(ctx, `
		SELECT from_name FROM mailing_brand_metadata
		WHERE sending_domain = $1
		LIMIT 1
	`, domain).Scan(&metaFrom)
	if err != nil || metaFrom == "" {
		return "", false
	}
	for _, fn := range fromNames {
		if fn == metaFrom {
			return fn, true
		}
	}
	suffix := " - " + metaFrom
	for _, fn := range fromNames {
		if strings.HasSuffix(fn, suffix) {
			return fn, true
		}
	}
	return "", false
}

func (po *PartnerDripOrchestrator) fetchOfferFromNames(ctx context.Context, offerID string) ([]string, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT from_name FROM mailing_offer_from_names
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		  AND COALESCE(from_name, '') <> ''
		ORDER BY id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

type claimedRecord struct {
	id        string
	email     string
	emailMD5  string
	ispFamily string
	datasetID string
	partnerID string
	batchID   string
	extra     []byte
}

// datasetNotEmergencyPausedSQL excludes queue rows whose dataset the operator
// has emergency-stopped (REQ-004; findings 2026-07-13-E §1). Before this
// predicate, the portal's Emergency Stop only halted the ingest slicer
// (partner_slicer.go claimNextBatch) — every already-sliced 'ready'/'mailed'
// row of a stopped dataset kept being claimed and MAILED on subsequent waves.
//
// Deliberately a correlated NOT EXISTS rather than a JOIN: partner_datasets
// never appears in the claim CTEs' FROM clause, so the FOR UPDATE SKIP LOCKED
// row locks stay scoped to partner_clean_queue only (a join would either lock
// the dataset row — serializing claims against the emergency-stop UPDATE
// itself — or require FOR UPDATE OF). Rows with a NULL/orphaned dataset_id
// remain claimable (NOT EXISTS is vacuously true), matching prior behavior.
const datasetNotEmergencyPausedSQL = `
			  AND NOT EXISTS (
			      SELECT 1 FROM partner_datasets d
			      WHERE d.id = partner_clean_queue.dataset_id
			        AND d.paused_emergency
			  )`

func (po *PartnerDripOrchestrator) claimRecords(ctx context.Context, vertical string, waveSize int) ([]claimedRecord, error) {
	rows, err := po.db.QueryContext(ctx, `
		WITH picked AS (
			SELECT id
			FROM partner_clean_queue
			WHERE status = 'ready' AND vertical = $1
			  -- Never-mailed only. Since 2026-07-26 a partner re-push can return
			  -- an already-mailed row to 'ready' (partner_slicer.bulkInsertSurvivors)
			  -- so that the stream router re-stages it as fresh signal. Those rows
			  -- keep their ORIGINAL ingested_at, so without this predicate they
			  -- would sort to the FRONT of this ORDER BY ingested_at ASC queue and
			  -- be claimed here — ahead of genuinely new records, and racing the
			  -- router for the same rows. Re-staging belongs to the router; the
			  -- drip ladder owns first-touch. No-op for pre-2026-07-26 behaviour:
			  -- a mailed row could not previously be 'ready' at all.
			  AND mailed_at IS NULL`+datasetNotEmergencyPausedSQL+`
			ORDER BY ingested_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW()
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, vertical, waveSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]claimedRecord, 0, waveSize)
	for rows.Next() {
		var r claimedRecord
		if err := rows.Scan(&r.id, &r.email, &r.emailMD5, &r.ispFamily, &r.datasetID, &r.partnerID, &r.batchID, &r.extra); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// applyISPBrandRouting enforces ISP→brand deliverability routing: for any ISP
// in NewRecordISPBrandAllow, if the waving brand is NOT in that ISP's allow
// set, its per-ISP cap is forced to 0 so the claim skips it (claimRecordsByISPCaps
// drops non-positive caps). This pins engagement-priced ISPs (gmail/yahoo/
// apple/att) to the warmed mature-4 domains regardless of daily-cap membership.
// Unlike applyNewRecordDailyBudget this is pure routing — it never imposes a
// volume throttle; allowed brands keep whatever cap resolvePerISPCaps produced.
func (po *PartnerDripOrchestrator) applyISPBrandRouting(brand string, caps map[string]int) map[string]int {
	if len(po.cfg.NewRecordISPBrandAllow) == 0 {
		return caps
	}
	out := cloneISPCapMap(caps)
	lb := strings.ToLower(strings.TrimSpace(brand))
	for isp, allow := range po.cfg.NewRecordISPBrandAllow {
		isp = strings.ToLower(strings.TrimSpace(isp))
		if len(allow) == 0 {
			continue
		}
		if !allow[lb] {
			out[isp] = 0
		}
	}
	return out
}

// applyFollowupGmailRouting zeroes ONLY the gmail per-ISP cap when `brand` is
// not in the gmail allow-set (NewRecordISPBrandAllow["gmail"], default
// db/ht/mh/qf). The welcome path runs the full applyISPBrandRouting; the
// follow-up path historically skipped all ISP→brand routing, so because the
// follow-up brand rotation is INDEPENDENT of a record's original mailed_brand,
// gmail touches could ship under any brand — including ones the operator has
// banned from gmail. Operator 2026-06-14: "BAN ALL GMAIL delivery for YIH,
// thingoftheday, learnpersonalloans, refinanceratesusa, consumerpro,
// businessweeklypro, financialcalculate — this includes any and all drips."
// This gates gmail ONLY (not yahoo/apple/att/aol) so existing follow-up volume
// for the other sensitive ISPs is unchanged — gmail follow-ups now ship solely
// from the warmed mature-4, where the gmail engager rings already live.
func (po *PartnerDripOrchestrator) applyFollowupGmailRouting(brand string, caps map[string]int) map[string]int {
	allow := po.cfg.NewRecordISPBrandAllow["gmail"]
	if len(allow) == 0 {
		return caps
	}
	lb := strings.ToLower(strings.TrimSpace(brand))
	if allow[lb] {
		return caps
	}
	out := cloneISPCapMap(caps)
	out["gmail"] = 0
	return out
}

// laneDailyCapsDisabled is the one-move kill switch for per-drip daily ISP
// budgets (drip-specific caps doctrine, core.md §14 2026-08-06). Set
// PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED=1 to ignore every daily_cap value in
// partner_isp_distribution_overrides and restore the pure per-brand env
// behavior — no data changes needed. Default false = enforce.
func laneDailyCapsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// datasetDailyISPCaps loads the lane-owned daily ISP budgets for one dataset
// (partner_isp_distribution_overrides.daily_cap, NULL = no override). Read
// fresh per budget application — like loadISPCaps, an operator change takes
// effect on the next wave with no deploy. Best-effort: any error returns nil
// and the caller falls back to the global per-brand env behavior.
func (po *PartnerDripOrchestrator) datasetDailyISPCaps(ctx context.Context, datasetID string) map[string]int {
	if datasetID == "" || laneDailyCapsDisabled() {
		return nil
	}
	var loaded map[string]int
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT LOWER(TRIM(isp)) AS isp, daily_cap
			FROM partner_isp_distribution_overrides
			WHERE dataset_id = $1::uuid AND daily_cap IS NOT NULL
		`, datasetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isp string
			var cap int
			if err := rows.Scan(&isp, &cap); err != nil {
				continue
			}
			if isp == "" {
				continue
			}
			if loaded == nil {
				loaded = map[string]int{}
			}
			loaded[isp] = cap
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] lane daily-cap load failed dataset=%s (%v) — using global daily caps", datasetID, err)
		return nil
	}
	return loaded
}

// applyNewRecordDailyBudget clamps a wave's per-ISP caps to the remaining
// daily new-record budget (America/Denver day). mailed_at is written exactly
// once (first touch), so counting rows with mailed_at >= local midnight counts
// NEW records only — follow-up touches never move mailed_at. Best-effort: on
// count error the static caps stand (consistent with resolvePerISPCaps
// degradation).
//
// Two budget scopes (drip-specific caps doctrine, core.md §14 2026-08-06),
// precedence per-drip override → global default:
//   - LANE: an ISP with a daily_cap row in partner_isp_distribution_overrides
//     for this wave's dataset is governed by the LANE's budget — counted per
//     (dataset, ISP) across ALL brands — and the shared per-brand env value is
//     not consulted for it. daily_cap=0 hard-suppresses the ISP for the lane.
//   - GLOBAL (legacy, byte-identical when no override row exists): ISPs in
//     NewRecordDailyISPCaps count per (brand, ISP) — the shared budget.
//
// The brand-allow gate (NewRecordISPBrandAllow) applies to BOTH scopes — it is
// deliverability routing (which brands may carry the ISP at all), the hard
// platform ceiling a lane override must not bypass.
func (po *PartnerDripOrchestrator) applyNewRecordDailyBudget(ctx context.Context, brand, datasetID string, caps map[string]int) map[string]int {
	laneCaps := po.datasetDailyISPCaps(ctx, datasetID)
	if len(po.cfg.NewRecordDailyISPCaps) == 0 && len(laneCaps) == 0 {
		return caps
	}
	out := cloneISPCapMap(caps)
	lb := strings.ToLower(strings.TrimSpace(brand))
	seen := map[string]bool{}
	for isp := range laneCaps {
		seen[isp] = true
	}
	for isp := range po.cfg.NewRecordDailyISPCaps {
		seen[strings.ToLower(strings.TrimSpace(isp))] = true
	}
	for isp := range seen {
		if allow, gated := po.cfg.NewRecordISPBrandAllow[isp]; gated && !allow[lb] {
			out[isp] = 0
			continue
		}
		daily, lane := laneCaps[isp]
		if !lane {
			var ok bool
			daily, ok = po.cfg.NewRecordDailyISPCaps[isp]
			if !ok {
				continue
			}
		}
		if lane && daily <= 0 {
			// Lane hard suppression: no DB round-trip (mirrors the follow-up
			// path's cap==0 semantics).
			out[isp] = 0
			continue
		}
		var used int
		err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			if lane {
				return tx.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM partner_clean_queue
					WHERE dataset_id = $1::uuid
					  AND LOWER(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
					  AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
				`, datasetID, isp).Scan(&used)
			}
			return tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM partner_clean_queue
				WHERE mailed_brand = $1
				  AND LOWER(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
				  AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
			`, lb, isp).Scan(&used)
		})
		if err != nil {
			log.Printf("[PartnerDripOrchestrator] daily-budget count failed brand=%s isp=%s lane=%v (%v) — keeping static cap", lb, isp, lane, err)
			continue
		}
		remaining := daily - used
		if remaining < 0 {
			remaining = 0
		}
		if out[isp] > remaining {
			out[isp] = remaining
		}
	}
	return out
}

// followupDailyCapsDisabled is the one-move kill switch for the follow-up daily
// ISP cap enforcement (applyFollowupDailyISPBudget). Set
// PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED=1 (or true/on/yes) to restore the
// pre-2026-07-15 behavior where follow-up touches bypassed NewRecordDailyISPCaps
// entirely. Default false = enforce.
func followupDailyCapsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// applyFollowupDailyISPBudget clamps a FOLLOW-UP wave's per-ISP caps by the same
// NewRecordDailyISPCaps (PARTNER_DRIP_DAILY_ISP_CAPS) daily ceiling the welcome
// path enforces via applyNewRecordDailyBudget. Bug fix 2026-07-15: without this,
// follow-up touches (t2..t5) escaped the daily cap completely — a gmail=0 cap
// blocked enrollment/touch-1 but every already-enrolled gmail recipient kept
// receiving follow-up drip mail (934 gmail follow-up deliveries observed under
// gmail=0). Semantics:
//
//   - ISP NOT in NewRecordDailyISPCaps  -> untouched (non-capped behavior exact).
//   - daily cap <= 0 (e.g. gmail=0)     -> per-wave cap forced to 0, so
//     claimFollowupRecordsByISPCaps drops the ISP from its VALUES list and NO
//     touch of any number ships to it. This is the core suppression the operator
//     relies on (broadcast owns gmail; the drip drains non-gmail).
//   - daily cap > 0                     -> clamp per-wave cap to the remaining
//     daily budget = cap - already-mailed-today (first touches via mailed_at +
//     follow-up touches via last-touch-today), counted per (brand, ISP) in the
//     America/Denver day, mirroring applyNewRecordDailyBudget's per-brand basis.
//
// Best-effort: on a count error the static per-wave cap stands (consistent with
// applyNewRecordDailyBudget). Kill switch: followupDailyCapsDisabled().
//
// Note on the positive-cap count: a terminal follow-up (touch_count reached
// MaxTouchCount, next_touch_at NULL) shipped today is not counted, so a positive
// cap can under-count by the terminal touch — this only ever makes the cap
// slightly more generous, never over-suppresses. The cap==0 path (the reported
// bug and the standing gmail rule) is exact and DB-free.
func (po *PartnerDripOrchestrator) applyFollowupDailyISPBudget(ctx context.Context, brand, datasetID string, caps map[string]int) map[string]int {
	if followupDailyCapsDisabled() {
		return caps
	}
	laneCaps := po.datasetDailyISPCaps(ctx, datasetID)
	if len(po.cfg.NewRecordDailyISPCaps) == 0 && len(laneCaps) == 0 {
		return caps
	}
	out := cloneISPCapMap(caps)
	lb := strings.ToLower(strings.TrimSpace(brand))
	seen := map[string]bool{}
	for isp := range laneCaps {
		seen[isp] = true
	}
	for isp := range po.cfg.NewRecordDailyISPCaps {
		seen[strings.ToLower(strings.TrimSpace(isp))] = true
	}
	for isp := range seen {
		// Brand-allow gate mirrors the welcome path: a gated ISP (e.g. gmail)
		// ships from allow-listed brands only — the platform ceiling; a lane
		// daily_cap override does not bypass it.
		if allow, gated := po.cfg.NewRecordISPBrandAllow[isp]; gated && !allow[lb] {
			out[isp] = 0
			continue
		}
		// Precedence per-drip override → global default (core.md §14
		// 2026-08-06): a lane daily_cap governs this dataset's touches counted
		// per (dataset, ISP) across all brands; otherwise the shared per-brand
		// env value applies exactly as before.
		daily, lane := laneCaps[isp]
		if !lane {
			var ok bool
			daily, ok = po.cfg.NewRecordDailyISPCaps[isp]
			if !ok {
				continue
			}
		}
		// Hard suppression: a daily cap of 0 zeroes the per-wave cap so the ISP
		// is dropped from the follow-up claim entirely (no DB round-trip).
		if daily <= 0 {
			out[isp] = 0
			continue
		}
		var used int
		err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			if lane {
				return tx.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM partner_clean_queue
					WHERE dataset_id = $1::uuid
					  AND LOWER(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
					  AND (`+introSpendTermSQL(`
					    mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
					    OR`)+`
					    (COALESCE(touch_count, 0) > 1
					       AND next_touch_at >= (date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver') + INTERVAL '24 hours')
					  )
				`, datasetID, isp).Scan(&used)
			}
			return tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM partner_clean_queue
				WHERE LOWER(COALESCE(NULLIF(isp_family, ''), 'other')) = $2
				  AND (`+introSpendTermSQL(`
				    (mailed_brand = $1
				       AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver')
				    OR`)+`
				    (last_touch_brand = $1
				       AND COALESCE(touch_count, 0) > 1
				       AND next_touch_at >= (date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver') + INTERVAL '24 hours')
				  )
			`, lb, isp).Scan(&used)
		})
		if err != nil {
			log.Printf("[PartnerDripOrchestrator] followup daily-budget count failed brand=%s isp=%s lane=%v (%v) — keeping static cap", lb, isp, lane, err)
			continue
		}
		remaining := daily - used
		if remaining < 0 {
			remaining = 0
		}
		if out[isp] > remaining {
			out[isp] = remaining
		}
	}
	return out
}

// loadISPCaps refreshes the per-wave ISP cap overlay from partner_drip_isp_caps.
// Called once per tick so an operator cap change takes effect on the next tick
// with NO deploy. Fail-safe differs from loadGovernors on purpose: a load error
// LEAVES the previous overlay in place (and an empty table means "no overlay"),
// because zeroing caps here would silently stop every lane.
func (po *PartnerDripOrchestrator) loadISPCaps(ctx context.Context) {
	loaded := map[string]int{}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT lower(btrim(isp)), per_wave_cap
			FROM partner_drip_isp_caps
			WHERE active
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isp string
			var cap int
			if err := rows.Scan(&isp, &cap); err != nil {
				return err
			}
			if isp != "" && cap >= 0 {
				loaded[isp] = cap
			}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] loadISPCaps failed (%v) — keeping previous cap overlay", err)
		return
	}
	po.ispCapMu.Lock()
	po.ispCapCache = loaded
	po.ispCapMu.Unlock()
}

// loadBrandDomains refreshes the brand_code -> sending_domain overlay from
// mailing_brand_metadata so a NEW drip lane needs only a DB row. On error the
// previous overlay stands; an empty result clears it back to the compiled map.
func (po *PartnerDripOrchestrator) loadBrandDomains(ctx context.Context) {
	loaded := map[string]string{}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT lower(btrim(brand_code)), btrim(sending_domain)
			FROM mailing_brand_metadata
			WHERE COALESCE(status, 'active') = 'active'
			  AND COALESCE(btrim(sending_domain), '') <> ''
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code, dom string
			if err := rows.Scan(&code, &dom); err != nil {
				return err
			}
			if code != "" {
				loaded[code] = dom
			}
		}
		return rows.Err()
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] loadBrandDomains failed (%v) — keeping previous domain overlay", err)
		return
	}
	setDynamicBrandDomains(loaded)
}

// loadVerticalRosters refreshes the vertical->brand roster overlay. Ordered by
// sort_order so an operator controls round-robin position. On error the previous
// overlay stands (same posture as the cap/domain overlays: never fail a lane shut).
func (po *PartnerDripOrchestrator) loadVerticalRosters(ctx context.Context) {
	// Weighted rotation (operator 2026-08-15): a brand with weight N appears N
	// times in the roster slice, so the existing round-robin distributes waves
	// proportionally with zero changes to pickNextBrand/pickNextFollowupBrand.
	// Weight is clamped to [1, 20] in SQL. If the weight column is missing
	// (migration not yet landed), fall back to the legacy unweighted query so
	// the overlay never freezes — a frozen-empty overlay would send
	// roster-restricted lanes (e.g. heloc→wcl) across all 16 dripBrands.
	loaded := map[string][]string{}
	scanRoster := func(tx *sql.Tx, query string, weighted bool) error {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v, b string
			w := 1
			if weighted {
				if err := rows.Scan(&v, &b, &w); err != nil {
					return err
				}
			} else if err := rows.Scan(&v, &b); err != nil {
				return err
			}
			if v == "" || b == "" {
				continue
			}
			for i := 0; i < w; i++ {
				loaded[v] = append(loaded[v], b)
			}
		}
		return rows.Err()
	}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		return scanRoster(tx, `
			SELECT lower(btrim(vertical)), lower(btrim(brand)),
			       GREATEST(1, LEAST(COALESCE(weight, 1), 20))
			FROM partner_drip_vertical_roster
			WHERE active
			ORDER BY vertical, sort_order, brand
		`, true)
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] weighted roster load failed (%v) — retrying unweighted", err)
		loaded = map[string][]string{}
		err = po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			return scanRoster(tx, `
				SELECT lower(btrim(vertical)), lower(btrim(brand))
				FROM partner_drip_vertical_roster
				WHERE active
				ORDER BY vertical, sort_order, brand
			`, false)
		})
	}
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] loadVerticalRosters failed (%v) — keeping previous roster overlay", err)
		return
	}
	setDynamicRoster(loaded)
}

// loadGovernors refreshes the in-memory partner_property_governor cache. Called
// once per tick (top of tickOnce) so operator edits to caps/subscriptions take
// effect within one tick without a deploy. Fail-safe: on any error (including a
// missing table) the cache is set EMPTY so no governed brand joins any rotation
// and applyPropertyGovernor returns all-zero caps — a missing/unreadable
// governor means "we don't know the ceiling", and these are fresh-IP properties,
// so the safe direction is to ship nothing rather than ship uncapped.
func (po *PartnerDripOrchestrator) loadGovernors(ctx context.Context) {
	loaded := map[string]propertyGovernor{}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT brand_code, per_isp_daily_cap, window_hours, gmail_held,
			       COALESCE(per_isp_overrides::text, '{}'),
			       COALESCE(subscribed_verticals, '{}'::text[]),
			       active
			FROM partner_property_governor
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				g            propertyGovernor
				overridesRaw string
				subs         []string
			)
			if err := rows.Scan(&g.brand, &g.perISPDaily, &g.windowHours, &g.gmailHeld,
				&overridesRaw, pq.Array(&subs), &g.active); err != nil {
				continue
			}
			g.brand = strings.ToLower(strings.TrimSpace(g.brand))
			g.perISPOverride = map[string]int{}
			if overridesRaw != "" && overridesRaw != "{}" {
				var m map[string]int
				if jsonErr := json.Unmarshal([]byte(overridesRaw), &m); jsonErr == nil {
					for k, v := range m {
						g.perISPOverride[strings.ToLower(strings.TrimSpace(k))] = v
					}
				}
			}
			g.subscribed = map[string]bool{}
			for _, s := range subs {
				if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
					g.subscribed[s] = true
				}
			}
			loaded[g.brand] = g
		}
		return rows.Err()
	})
	po.governorMu.Lock()
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] loadGovernors failed (%v) — governed brands held to cap-0 this tick", err)
		po.governorCache = map[string]propertyGovernor{}
	} else {
		po.governorCache = loaded
	}
	po.governorMu.Unlock()
}

// applyPropertyGovernor clamps a governed brand's per-ISP wave caps to:
//   - its subscription to this vertical (not subscribed -> all-zero, claim nothing),
//   - the per-ISP daily cap (per_isp_overrides[isp] else per_isp_daily_cap; gmail
//     forced to 0 when gmail_held),
//   - the remaining (property,vertical,ISP) daily budget (dailyCap - mailed-today),
//   - the 6h-paced per-wave cap (perWaveCapFromPacing).
//
// Final per-ISP cap = min(caps[isp], remaining, paceCap). It only ever clamps
// DOWN from the resolvePerISPCaps base passed in, so platform-wide ISP doctrine
// still bounds these brands from above. Best-effort on the daily count: a count
// error keeps the (paced, dailyCap-bounded) cap rather than failing the wave —
// consistent with applyNewRecordDailyBudget's degradation.
func (po *PartnerDripOrchestrator) applyPropertyGovernor(ctx context.Context, brand, vertical string, caps map[string]int) map[string]int {
	lb := strings.ToLower(strings.TrimSpace(brand))
	lv := strings.ToLower(strings.TrimSpace(vertical))

	po.governorMu.RLock()
	g, ok := po.governorCache[lb]
	po.governorMu.RUnlock()

	// Not governed, inactive, or not subscribed to this vertical -> claim nothing.
	if !ok || !g.active || !g.subscribed[lv] {
		return zeroAllCaps(caps)
	}

	governedWavesPerDay := po.governedWavesPerDay(lv)
	out := cloneISPCapMap(caps)
	for isp := range out {
		lisp := strings.ToLower(strings.TrimSpace(isp))
		dailyCap := g.perISPDaily
		if ov, has := g.perISPOverride[lisp]; has {
			dailyCap = ov
		}
		if g.gmailHeld && lisp == "gmail" {
			dailyCap = 0
		}
		if dailyCap <= 0 {
			out[isp] = 0
			continue
		}
		used := po.governedDailyCount(ctx, lb, lv, lisp)
		remaining := dailyCap - used
		if remaining < 0 {
			remaining = 0
		}
		paceCap := perWaveCapFromPacing(dailyCap, g.windowHours, governedWavesPerDay)
		// final = min(base, remaining, paceCap)
		final := out[isp]
		if remaining < final {
			final = remaining
		}
		if paceCap < final {
			final = paceCap
		}
		out[isp] = final
	}
	return out
}

// governedDailyCount returns how many NEW records (first touch) the property has
// mailed today (America/Denver) for (brand, vertical, ISP). Because governed
// brands fire WELCOME waves only (never follow-ups — they are absent from the
// dripBrands follow-up rotation), and mailed_at/mailed_brand are written exactly
// once on the first touch, this new-records count IS the clean per-day total for
// the governor. Best-effort: a count error returns 0 (the dailyCap/pace bounds
// still apply).
func (po *PartnerDripOrchestrator) governedDailyCount(ctx context.Context, brand, vertical, isp string) int {
	var used int
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM partner_clean_queue
			WHERE mailed_brand = $1
			  AND vertical = $2
			  AND LOWER(COALESCE(NULLIF(isp_family, ''), 'other')) = $3
			  AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
		`, brand, vertical, isp).Scan(&used)
	})
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] governor daily-count failed brand=%s vertical=%s isp=%s (%v) — treating used=0", brand, vertical, isp, err)
		return 0
	}
	return used
}

// governedWavesPerDay estimates how many WELCOME waves a single governed brand
// gets per day for `vertical` in the GOVERNED pass (Option B). The governed pass
// fires GovernedBrandsPerTick brands/vertical/tick over the vertical's subscribed
// governed roster, so a single brand's waves/day = ticksPerDay *
// GovernedBrandsPerTick / len(governedRoster). At least 1. (Independent of the
// welcome pass's BrandsPerTick / brandRosterFor — governed brands are no longer
// in brandRosterFor.)
func (po *PartnerDripOrchestrator) governedWavesPerDay(vertical string) int {
	interval := po.cfg.TickInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticksPerDay := int((24 * time.Hour) / interval)
	if ticksPerDay < 1 {
		ticksPerDay = 96
	}
	brandsPerTick := po.cfg.GovernedBrandsPerTick
	if brandsPerTick <= 0 {
		brandsPerTick = 2
	}
	rosterLen := len(po.orderedSubscribedGoverned(vertical))
	if rosterLen < 1 {
		rosterLen = 1
	}
	w := ticksPerDay * brandsPerTick / rosterLen
	if w < 1 {
		w = 1
	}
	return w
}

// perWaveCapFromPacing sizes a per-wave cap so dailyCap is spread evenly across
// windowHours of a single governed brand's waves. wavesInWindow = the brand's
// governed waves/day scaled to the window; the daily cap is divided across them
// (ceil). The remaining = dailyCap - used clamp in applyPropertyGovernor is the
// hard backstop — pacing only smooths, it never raises the day total.
func perWaveCapFromPacing(dailyCap, windowHours, governedWavesPerDay int) int {
	if dailyCap <= 0 {
		return 0
	}
	if windowHours <= 0 || windowHours > 24 {
		windowHours = 24
	}
	if governedWavesPerDay < 1 {
		governedWavesPerDay = 1
	}
	wavesInWindow := governedWavesPerDay * windowHours / 24
	if wavesInWindow < 1 {
		wavesInWindow = 1
	}
	perWave := (dailyCap + wavesInWindow - 1) / wavesInWindow // ceil
	if perWave < 1 {
		perWave = 1
	}
	return perWave
}

// zeroAllCaps returns a copy of caps with every ISP forced to 0.
func zeroAllCaps(caps map[string]int) map[string]int {
	out := make(map[string]int, len(caps))
	for isp := range caps {
		out[isp] = 0
	}
	return out
}

// claimRecordsByISPCaps claims oldest-first records from partner_clean_queue
// while honoring per-ISP-per-wave caps natively at the SQL level. This is
// the ISP-aware replacement for claimRecords + applyThroughputSafety's
// per-ISP cap branch.
//
// Why a separate function: the old claimRecords pulls oldest-first across
// ALL ISPs, regardless of cap. When yahoo dominates the queue (e.g. 49%
// of records but cap=20), the wave's claim slots get eaten by yahoo
// records that then get deferred and released back to 'ready'. Net effect:
// gmail/microsoft/apple records stuck behind yahoo trickle through far
// below their per-wave caps. Observed on David Cal's Personal Loans feed
// — gmail shipped 235/day against a 200/wave cap that should have shipped
// up to 4,800/day per brand.
//
// New strategy: rank queue rows BY ISP, take the oldest N per ISP up to
// each ISP's cap, then drain by global ingested_at. This guarantees every
// ISP gets its full per-wave budget before any other ISP overshoots.
//
// hardCap is a safety upper bound on total claim size (post-cap). Set to
// MaxWaveSize so a single wave can never exceed that operational ceiling.
func (po *PartnerDripOrchestrator) claimRecordsByISPCaps(ctx context.Context, vertical string, perISPCaps map[string]int, hardCap int) ([]claimedRecord, error) {
	if len(perISPCaps) == 0 {
		return nil, fmt.Errorf("perISPCaps is empty")
	}
	if hardCap <= 0 {
		hardCap = po.cfg.MaxWaveSize
	}

	// Build a VALUES list (isp, cap) for the caps CTE. Args:
	//   $1 = vertical, $2 = hardCap, then $3.. = isp, cap, isp, cap, ...
	args := []interface{}{vertical, hardCap}
	valueClauses := make([]string, 0, len(perISPCaps))
	idx := 3
	for ispName, capValue := range perISPCaps {
		if capValue <= 0 {
			continue
		}
		valueClauses = append(valueClauses, fmt.Sprintf("($%d::text, $%d::int)", idx, idx+1))
		args = append(args, ispName, capValue)
		idx += 2
	}
	if len(valueClauses) == 0 {
		return nil, fmt.Errorf("perISPCaps has no positive entries")
	}

	// caps is declared FIRST so ranked can bucket against it.
	//
	// The isp bucket falls back to 'other' for any isp_family with no caps row.
	// The join below is an INNER join, so before this an unrecognized
	// isp_family matched nothing and its records were NEVER claimable — they
	// sat in 'ready' forever, silently, with no cap to raise and no error.
	// Measured 2026-08-11 on internal_auto_insurance: protonmail 17 received,
	// 0 ever mailed, 14 stuck since morning. PerISPCapPerWave carries 12 keys
	// (gmail yahoo aol microsoft apple comcast charter att sbcglobal cox
	// verizon other); ANY class outside them starves — protonmail today, the
	// next unclassified domain family tomorrow.
	//
	// This mirrors baseISPCap(), which already falls back to perISPCaps["other"]
	// when RESOLVING a cap. The fallback existed on one side of the same concept
	// and not the other; now both agree.
	query := fmt.Sprintf(`
		WITH caps(isp, cap) AS (
			VALUES %s
		),
		ranked AS (
			SELECT id, isp_family, ingested_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY CASE
			               WHEN COALESCE(NULLIF(isp_family, ''), 'other') IN (SELECT isp FROM caps)
			               THEN COALESCE(NULLIF(isp_family, ''), 'other') ELSE 'other' END
			           ORDER BY ingested_at ASC
			       ) AS rn,
			       CASE
			           WHEN COALESCE(NULLIF(isp_family, ''), 'other') IN (SELECT isp FROM caps)
			           THEN COALESCE(NULLIF(isp_family, ''), 'other') ELSE 'other' END AS isp_bucket
			FROM partner_clean_queue
			WHERE status = 'ready' AND vertical = $1`+datasetNotEmergencyPausedSQL+`
		),
		eligible AS (
			SELECT r.id
			FROM ranked r
			JOIN caps c ON c.isp = r.isp_bucket
			WHERE r.rn <= c.cap
			ORDER BY r.ingested_at ASC
			LIMIT $2
		),
		picked AS (
			SELECT id FROM partner_clean_queue
			WHERE id IN (SELECT id FROM eligible)
			  AND status = 'ready'`+datasetNotEmergencyPausedSQL+`
			FOR UPDATE SKIP LOCKED
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW()
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, strings.Join(valueClauses, ", "))

	out := make([]claimedRecord, 0, hardCap)
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r claimedRecord
			if err := rows.Scan(&r.id, &r.email, &r.emailMD5, &r.ispFamily, &r.datasetID, &r.partnerID, &r.batchID, &r.extra); err != nil {
				continue
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// claimSweepBatchSize / claimSweepMaxBatchesPerTick bound the two per-tick
// janitor UPDATEs (releaseStaleClaims, reconcileShippedClaims). Both used to
// run as one unbounded statement over the whole 11.8M-row queue, holding row
// locks across the entire matching backlog for the statement's lifetime and
// contending with the claim CTEs. Each batch is now its own short transaction
// over at most claimSweepBatchSize rows, selected with FOR UPDATE SKIP LOCKED
// so a batch never blocks on (or is blocked by) a concurrent claim. The
// per-tick iteration cap keeps a pathological backlog from monopolizing a
// tick; the remainder is picked up next tick.
const claimSweepBatchSize = 5000
const claimSweepMaxBatchesPerTick = 10

// releaseStaleClaimsSQL — see releaseStaleClaims. $1 cutoff, $2 batch size.
const releaseStaleClaimsSQL = `
		UPDATE partner_clean_queue
		SET status = 'ready',
		    claimed_at = NULL
		WHERE id IN (
		    SELECT id
		    FROM partner_clean_queue
		    WHERE status = 'claimed'
		      AND claimed_at IS NOT NULL
		      AND claimed_at < $1
		      AND subscriber_id IS NULL
		      AND mailed_campaign_id IS NULL
		    LIMIT $2
		    FOR UPDATE SKIP LOCKED
		)
`

// releaseStaleClaims returns zombie 'claimed' rows to 'ready'. These appear
// when the process dies after claimRecordsByISPCaps but before promote/deploy
// completes (no subscriber_id, no mailed_campaign_id). Safe to re-queue.
// Batched — see claimSweepBatchSize.
func (po *PartnerDripOrchestrator) releaseStaleClaims(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-po.cfg.ClaimedJanitorMaxAge)
	var total int64
	for i := 0; i < claimSweepMaxBatchesPerTick; i++ {
		var n int64
		err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, releaseStaleClaimsSQL, cutoff, claimSweepBatchSize)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < claimSweepBatchSize {
			break
		}
	}
	return total, nil
}

// reconcileShippedClaimsSQL — see reconcileShippedClaims. $1 MaxTouchCount,
// $2 followupTouchGapHours, $3 batch size.
//
// The SET clause mirrors markMailedStampSQL's ladder transition: the EXISTS
// proves a campaign for these rows reached the sending stage, so the flip back
// to 'mailed' must ALSO advance touch_count / next_touch_at (NULL +
// terminal_reason='completed' at the MaxTouchCount ceiling). What it cannot do
// is move last_touch_campaign_id — the shipped campaign's id is exactly what
// markMailed failed to record — so the stamp-recovery pass (which replays the
// true campaign id from mailing_message_log, and runs FIRST in tickOnce)
// remains the precise repair; this is the coarse residual backstop that
// guarantees a row is never re-claimable without its ladder advanced.
const reconcileShippedClaimsSQL = `
		UPDATE partner_clean_queue q
		SET status = 'mailed',
		    claimed_at = NULL,
		    mailed_at = COALESCE(q.mailed_at, NOW()),
		    touch_count = LEAST(COALESCE(q.touch_count, 0) + 1, $1),
		    next_touch_at = CASE
		        WHEN COALESCE(q.touch_count, 0) + 1 < $1
		        THEN NOW() + make_interval(hours => $2::int)
		        ELSE NULL
		    END,
		    terminal_reason = CASE
		        WHEN COALESCE(q.touch_count, 0) + 1 >= $1 THEN 'completed'
		        ELSE q.terminal_reason
		    END
		WHERE q.id IN (
		    SELECT id
		    FROM partner_clean_queue
		    WHERE status = 'claimed'
		      AND last_touch_campaign_id IS NOT NULL
		      AND EXISTS (
		        SELECT 1 FROM mailing_campaigns c
		        WHERE c.id = partner_clean_queue.last_touch_campaign_id
		          AND c.status IN ('sending', 'sent', 'completed', 'completed_with_errors')
		      )
		    LIMIT $3
		    FOR UPDATE SKIP LOCKED
		)
`

// reconcileShippedClaims repairs rows left in status='claimed' after a wave
// deployed successfully but markMailed failed (logged, non-fatal). Without
// this, follow-up touches stay claimed and block claimFollowupRecordsByISPCaps
// (which only picks status='mailed').
//
// 2026-08-17 triple-fire fix: the old bare `SET status = 'mailed'` did NOT
// advance touch_count / next_touch_at, so a reconciled row (next_touch_at
// still in the past) was instantly re-claimed by claimFollowupRecordsByISPCaps
// and the SAME people were re-mailed every tick until a markMailed succeeded.
// The reconcile now advances the ladder for every row it flips (see
// reconcileShippedClaimsSQL). Batched — see claimSweepBatchSize.
func (po *PartnerDripOrchestrator) reconcileShippedClaims(ctx context.Context) (int64, error) {
	var total int64
	for i := 0; i < claimSweepMaxBatchesPerTick; i++ {
		var n int64
		err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, reconcileShippedClaimsSQL,
				MaxTouchCount, followupTouchGapHours, claimSweepBatchSize)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < claimSweepBatchSize {
			break
		}
	}
	return total, nil
}

// releaseClaim flips claimed records back to 'ready' so the next tick can
// retry them after a deploy / promote / segment-creation failure.
func (po *PartnerDripOrchestrator) releaseClaim(ctx context.Context, recs []claimedRecord) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	// A released FOLLOW-UP claim (touch_count >= 1, already t1-mailed) must go
	// back to 'mailed' — that is the pool the t2..tN pass claims from. Releasing
	// it to 'ready' ejects it from the reminder ladder AND exposes it to the
	// first-touch pass, which re-sends t1 to someone mid-sequence (2026-08-05
	// outage: 30k records ejected, WCL follow-ups collapsed 9,338 -> 122/day
	// while first-touches inflated with duplicates).
	_, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = CASE
		        WHEN COALESCE(touch_count, 0) >= 1 AND mailed_campaign_id IS NOT NULL
		        THEN 'mailed'
		        ELSE 'ready'
		    END,
		    claimed_at = NULL
		WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(ids, ",")+"}")
	return err
}

// namesFromExtra extracts personalization names from a clean-queue record's
// extra_metadata. Both ingest doors (partner API slicer, drivesource) write
// top-level first_name/last_name keys; empty string means "not provided"
// (the API door marshals absent fields as "") and the record mails generic.
// Names are trimmed and capped to the mailing_subscribers VARCHAR(100)
// columns — an oversized value must not fail the insert (a failed insert
// silently drops the record from the wave).
func namesFromExtra(extra []byte) (first, last string) {
	if len(extra) == 0 {
		return "", ""
	}
	var m struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	if err := json.Unmarshal(extra, &m); err != nil {
		return "", ""
	}
	return cap100(m.FirstName), cap100(m.LastName)
}

// cap100 trims a personalization value and caps it to the 100-rune width of the
// mailing_subscribers VARCHAR(100) name columns. Shared by namesFromExtra and
// personaFieldsFromExtra so both doors truncate identically — an oversized
// value must never fail the promote insert (a failed insert silently drops the
// record from the wave).
func cap100(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 100 {
		return string(r[:100])
	}
	return s
}

// personaFieldsFromExtra extracts the geo/vehicle personalization a clean-queue
// record carries beyond first/last name, for mailing_subscribers.custom_fields
// (JSONB — migrations/001_mailing_schema.sql:94). That is the existing
// per-subscriber custom-field mechanism: the send worker's claim query already
// selects s.custom_fields (send_worker.go:872) into QueueItem.CustomFields
// (send_worker.go:326) and buildRenderContext exposes it to Liquid as
// rc["custom"] (send_worker.go:3134) — i.e. {{ custom.city }}, NOT {{ city }}.
//
// Two shapes have to be read because the two ingest doors disagree on nesting:
// the partner-API slicer writes state/zip at the TOP level (partner_slicer.go
// :445-453), while the drivesource door routes city and the lead-detail fields
// into the nested "metadata" object (agents/drivesource/pull.py NESTED_MAP).
// Top level wins when a key appears in both.
//
// Keys are emitted under the portal's canonical import names — city / state /
// postal_code (mailing_import.go:358-361) — so a drip-promoted subscriber is
// segmentable by the same custom_fields->>'city' the CSV door produces.
//
// Hardened against a bad payload on every axis, because promoteToSubscribers
// runs one prepared statement per record and a single insert error drops that
// recipient from the wave: only non-empty values are emitted, every value is
// trimmed and capped to 100 runes, a "metadata" value that is not an object is
// ignored WITHOUT discarding the top-level fields, and an unparseable payload
// yields an empty (not nil) map, which merges as a no-op.
func personaFieldsFromExtra(extra []byte) map[string]interface{} {
	out := map[string]interface{}{}
	if len(extra) == 0 {
		return out
	}
	// Metadata is decoded separately (RawMessage) so a partner sending
	// "metadata": "n/a" cannot fail the whole struct and cost us city/state.
	var m struct {
		City       string          `json:"city"`
		State      string          `json:"state"`
		PostalCode string          `json:"postal_code"`
		Zip        string          `json:"zip"`
		Vehicle    string          `json:"vehicle"`
		Address1   string          `json:"address_1"`
		SignupURL  string          `json:"signup_url"`
		SignupAt   string          `json:"signup_date"`
		TID        string          `json:"tid"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(extra, &m); err != nil {
		return out
	}
	var meta map[string]interface{}
	if len(m.Metadata) > 0 {
		_ = json.Unmarshal(m.Metadata, &meta)
	}
	// A zip posted as a JSON number is still a zip.
	nested := func(key string) string {
		switch v := meta[key].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	put := func(key string, candidates ...string) {
		for _, c := range candidates {
			if v := cap100(c); v != "" {
				out[key] = v
				return
			}
		}
	}
	put("city", m.City, nested("city"))
	put("state", m.State, nested("state"))
	put("postal_code", m.PostalCode, m.Zip, nested("postal_code"), nested("zip"))
	put("vehicle", m.Vehicle, nested("vehicle"))
	put("address_1", m.Address1, nested("address_1"))
	put("signup_url", m.SignupURL, nested("signup_url"))
	put("signup_date", m.SignupAt, nested("signup_date"))
	// Lead-detail fields the mortgage/HELOC feeds carry. Measured 2026-08-19 on
	// the WCL Mortgage 08-18-26 drop: partner_clean_queue held loan_type and
	// property_type on every record, and of 6,071 promoted subscribers exactly
	// 2 had loan_type in custom_fields and ZERO had property_type — the values
	// arrived intact at the door and died in this allow-list. The v6 HELOC
	// creatives render {{ custom.loan_type }} / {{ custom.property_type }}, so
	// without these two lines that personalization is blank for the whole feed
	// while every render test still passes on a synthetic persona.
	// Additive and non-empty-only: a feed that carries neither is unchanged.
	put("loan_type", nested("loan_type"))
	put("property_type", nested("property_type"))
	put("loan_purpose", nested("loan_purpose"))
	// equity_estimate — the single most useful number a HELOC creative can say,
	// and it is DERIVED, so nothing upstream ships it. Measured on the WCL
	// Mortgage 08-18-26 drop: property_value and loan_balance are present on
	// 100% of records and 75.5% clear $10k of equity (median $307,895), while
	// the creatives said nothing about it.
	//
	// Rounded DOWN to the nearest $10k, comma-grouped for display. Two
	// deliberate choices: down, so the figure is never larger than what the
	// recipient's own stated numbers support; and coarse, because "$307,895"
	// reads as an appraisal we did not perform. Copy must frame it as an
	// estimate from their own form values.
	//
	// Emitted only when both inputs parse and the result clears $10k — below
	// that a HELOC pitch is not credible and the key stays absent, so the
	// creative's `{% if %}` drops the clause instead of promising nothing.
	if eq, ok := equityEstimate(nested("property_value"), nested("loan_balance")); ok {
		out["equity_estimate"] = eq
	}
	// tid is the PER-USER money-link token. It must reach custom_fields or the
	// creative's {{ custom.tid }} renders empty and EVERY recipient gets the
	// same untracked link — attribution silently dies for the whole feed while
	// the send looks healthy. Partners spell its parent object "data" (merged
	// into metadata at the door) as well as "metadata".
	put("tid", m.TID, nested("tid"))
	// emd5 — the recipient's email-md5, passed by the Attribits gmail feeds in
	// metadata and required in their money link as s11= (vendor spec
	// 2026-08-24). Same failure shape as tid: absent here, the creative's
	// {{ custom.emd5 }} renders s11 empty for the whole feed while every send
	// looks healthy. Existing subscribers were backfilled with
	// md5(lower(trim(email))) on 2026-08-24; this carries it for new claims.
	put("emd5", nested("emd5"))
	return out
}

// equityEstimate derives available home equity from the two values the mortgage
// feeds carry, rounded DOWN to the nearest $10,000 and returned COMMA-GROUPED
// and ready to display (e.g. "300,000"). Copy supplies the currency symbol.
//
// Formatting happens HERE because the template engine cannot do it: verified
// 2026-08-19 that `{{ n | number_with_delimiter }}` is not a supported filter
// and renders the raw digits with NO error — a silent passthrough that would
// have shipped "$320000" to every recipient and looked like working copy.
//
// Returns ok=false — and the caller emits no key — when either input is
// unparseable, non-positive, or the difference is under $10,000. A missing key
// makes the creative's guard drop the whole clause, which is the only correct
// behaviour: an equity figure we cannot stand behind must not be shown at all.
func equityEstimate(propertyValue, loanBalance string) (string, bool) {
	pv, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(propertyValue), ",", ""), 64)
	if err != nil || pv <= 0 {
		return "", false
	}
	lb, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(loanBalance), ",", ""), 64)
	if err != nil || lb < 0 {
		return "", false
	}
	eq := pv - lb
	if eq < 10000 {
		return "", false
	}
	return groupThousands(strconv.FormatInt(int64(eq/10000)*10000, 10)), true
}

// groupThousands inserts commas into a non-negative integer string.
func groupThousands(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// promoteToSubscribers inserts each claimed email into mailing_subscribers
// (under the Data Partners Master list) with full provenance columns. Returns
// the subscriber IDs in the same order as recs (empty string for inserts that
// failed).
func (po *PartnerDripOrchestrator) promoteToSubscribers(ctx context.Context, v verticalState, recs []claimedRecord) ([]string, error) {
	out := make([]string, len(recs))
	tx, err := po.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO mailing_subscribers
		    (id, organization_id, list_id, email, email_hash,
		     status, source, engagement_score,
		     source_system, source_detail, source_metadata, imported_at,
		     first_name, last_name, custom_fields)
		VALUES ($1, $2, $3, $4, $5,
		        'confirmed', $6, 50.0,
		        $7, $8, $9::jsonb, NOW(),
		        NULLIF($10, ''), NULLIF($11, ''), $12::jsonb)
		ON CONFLICT (list_id, email) DO UPDATE SET
		    source = EXCLUDED.source,
		    source_system = EXCLUDED.source_system,
		    source_detail = EXCLUDED.source_detail,
		    source_metadata = EXCLUDED.source_metadata,
		    -- follow-up touches re-run this statement: never let a nameless
		    -- re-promote blank out a name some earlier door populated
		    first_name = COALESCE(NULLIF(EXCLUDED.first_name, ''), mailing_subscribers.first_name),
		    last_name  = COALESCE(NULLIF(EXCLUDED.last_name, ''),  mailing_subscribers.last_name),
		    -- same rule for the geo/vehicle fields, expressed as a JSONB merge
		    -- (the datanorm importer idiom): keys this promote carries win,
		    -- every key some other door wrote (provenance, domain_group, ...)
		    -- survives, and an empty object is a no-op rather than a wipe.
		    custom_fields = COALESCE(mailing_subscribers.custom_fields, '{}'::jsonb) || EXCLUDED.custom_fields,
		    updated_at = NOW()
		RETURNING id
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i, r := range recs {
		sourceSystem := "data_partner_" + safeIdent(v.partnerSlug)
		sourceDetail := v.datasetSlug + ":" + r.batchID
		metadata := map[string]interface{}{
			"partner_slug": v.partnerSlug,
			"partner_name": v.partnerName,
			"dataset_id":   v.datasetID,
			"dataset_slug": v.datasetSlug,
			"vertical":     v.vertical,
			"batch_id":     r.batchID,
			"clean_q_id":   r.id,
		}
		metaJSON, _ := json.Marshal(metadata)
		firstName, lastName := namesFromExtra(r.extra)
		// Geo/vehicle personalization -> custom_fields, read by templates as
		// {{ custom.city }}. Marshal cannot fail for a map[string]string-shaped
		// value, but a defensive fallback keeps a hypothetical error from
		// binding an empty string to a ::jsonb parameter (which WOULD fail the
		// insert and drop this recipient from the wave).
		customJSON, cfErr := json.Marshal(personaFieldsFromExtra(r.extra))
		if cfErr != nil || len(customJSON) == 0 {
			customJSON = []byte("{}")
		}
		var subID string
		err := stmt.QueryRowContext(ctx,
			uuid.New().String(),
			po.cfg.OrganizationID,
			dataPartnerMasterListID,
			r.email,
			r.emailMD5,
			"data_partner",
			sourceSystem,
			sourceDetail,
			string(metaJSON),
			firstName,
			lastName,
			string(customJSON),
		).Scan(&subID)
		if err != nil {
			log.Printf("[PartnerDripOrchestrator] insert subscriber %s: %v", r.email, err)
			continue
		}
		out[i] = subID
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Write subscriber_id back to partner_clean_queue so the engagement
	// marker can flip engaged_at when a tracking event arrives. Best-
	// effort — failure here doesn't unship the wave, but the recipient
	// will silently keep getting follow-ups even if they engage. We log
	// loud on any failure so an operator can audit.
	po.linkSubscriberIDsToQueue(ctx, recs, out)
	return out, nil
}

// linkSubscriberIDsToQueue stamps subscriber_id onto each partner_clean_queue
// row. The engagement marker uses this column to flip engaged_at when a
// new tracking event arrives — without it, every follow-up touch fires
// regardless of whether the recipient already engaged.
func (po *PartnerDripOrchestrator) linkSubscriberIDsToQueue(ctx context.Context, recs []claimedRecord, subIDs []string) {
	if len(recs) == 0 || len(subIDs) == 0 {
		return
	}
	pairs := make([]string, 0, len(recs))
	args := make([]interface{}, 0, len(recs)*2)
	posIdx := 1
	for i, r := range recs {
		if i >= len(subIDs) || subIDs[i] == "" {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("($%d::uuid, $%d::uuid)", posIdx, posIdx+1))
		args = append(args, r.id, subIDs[i])
		posIdx += 2
	}
	if len(pairs) == 0 {
		return
	}
	q := fmt.Sprintf(`
		UPDATE partner_clean_queue q
		SET subscriber_id = pairs.subscriber_id
		FROM (VALUES %s) AS pairs(queue_id, subscriber_id)
		WHERE q.id = pairs.queue_id
		  AND q.subscriber_id IS DISTINCT FROM pairs.subscriber_id
	`, strings.Join(pairs, ","))
	if _, err := po.db.ExecContext(ctx, q, args...); err != nil {
		log.Printf("[PartnerDripOrchestrator] link_subscriber_ids: %v", err)
	}
}

// safeSegmentNonce renders the short, name-safe uniqueness token appended to a
// wave campaign's name. Empty/short segment ids degrade to whatever is there
// rather than panicking — a wave with no segment is already an error path, and
// losing the nonce must not lose the deploy.
func safeSegmentNonce(segmentID string) string {
	s := strings.TrimSpace(segmentID)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// createWaveSegment builds a one-shot static segment named after the wave so
// the audience finalizer pulls exactly the records we claimed (no more, no
// less). Returns the segment_id.
func (po *PartnerDripOrchestrator) createWaveSegment(ctx context.Context, v verticalState, brand string, recs []claimedRecord, subIDs []string) (string, error) {
	segID := uuid.New().String()
	name := fmt.Sprintf("data-partner-wave-%s-%s-%s", v.vertical, brand, time.Now().UTC().Format("20060102T150405"))

	if _, err := po.db.ExecContext(ctx, `
		INSERT INTO mailing_segments (id, organization_id, list_id, name, description, segment_type, conditions, subscriber_count, last_calculated_at, status)
		VALUES ($1, $2, $3, $4, $5, 'static', '[]'::jsonb, $6, NOW(), 'active')
	`, segID, po.cfg.OrganizationID, dataPartnerMasterListID, name,
		fmt.Sprintf("auto-generated by partner_drip_orchestrator for %s/%s wave", v.vertical, brand),
		len(recs)); err != nil {
		return "", fmt.Errorf("insert segment: %w", err)
	}

	const cols = 3
	args := make([]interface{}, 0, len(recs)*cols)
	placeholders := make([]string, 0, len(recs))
	rowIndex := 0
	for i, r := range recs {
		if i >= len(subIDs) || subIDs[i] == "" {
			continue
		}
		offset := rowIndex * cols
		placeholders = append(placeholders, fmt.Sprintf("($%d::uuid, $%d::uuid, $%d)", offset+1, offset+2, offset+3))
		args = append(args, segID, subIDs[i], r.email)
		rowIndex++
	}
	if rowIndex == 0 {
		po.upsertWaveSegmentLedger(ctx, segID, 0)
		return segID, nil
	}
	q := fmt.Sprintf(`
		INSERT INTO mailing_segment_members (segment_id, subscriber_id, email)
		VALUES %s
		ON CONFLICT DO NOTHING
	`, strings.Join(placeholders, ","))
	res, err := po.db.ExecContext(ctx, q, args...)
	if err != nil {
		return "", fmt.Errorf("insert segment members: %w", err)
	}
	// The segment is brand new, so rows-affected (ON CONFLICT DO NOTHING
	// skips none on a fresh segment_id) IS the resulting member count — no
	// COUNT(*) needed.
	memberCount := int64(rowIndex)
	if n, raErr := res.RowsAffected(); raErr == nil {
		memberCount = n
	}
	po.upsertWaveSegmentLedger(ctx, segID, memberCount)
	return segID, nil
}

// upsertWaveSegmentLedger best-effort records the wave segment's membership
// in mailing_segment_build_ledger so the v2 segments list shows an
// authoritative count for partner-drip wave segments instead of a stale
// "never built" row.
//
// The SQL is inlined here (rather than calling api.UpsertSegmentLedger)
// because internal/api already imports internal/worker — a worker→api import
// would be a cycle. internal/api/segment_ledger.go is the CANONICAL
// implementation; keep this statement in sync with it (notably:
// last_built_at / subscriber_count only advance on status 'ok', which this
// write always is).
func (po *PartnerDripOrchestrator) upsertWaveSegmentLedger(ctx context.Context, segmentID string, count int64) {
	if po.db == nil {
		return
	}
	if _, err := po.db.ExecContext(ctx, `
		INSERT INTO mailing_segment_build_ledger
			(segment_id, subscriber_count, last_built_at, build_source,
			 last_build_status, last_build_ms, last_delta_pct, last_error, updated_at)
		VALUES ($1::uuid, $2, NOW(), 'partner-drip', 'ok', 0, 0, NULL, NOW())
		ON CONFLICT (segment_id) DO UPDATE SET
			subscriber_count  = EXCLUDED.subscriber_count,
			last_built_at     = NOW(),
			build_source      = EXCLUDED.build_source,
			last_build_status = EXCLUDED.last_build_status,
			last_build_ms     = EXCLUDED.last_build_ms,
			last_delta_pct    = EXCLUDED.last_delta_pct,
			last_error        = NULL,
			updated_at        = NOW()
	`, segmentID, count); err != nil {
		log.Printf("[PartnerDripOrchestrator] segment ledger upsert failed for %s (continuing): %v", segmentID, err)
	}
}

const (
	ispCapBacklogReady    = "ready"
	ispCapBacklogFollowup = "followup"
)

// ispCapForDrainHorizon returns the per-wave claim cap for one ISP given live
// backlog depth and a multi-day drain target. Result is clamped to baseCap.
func ispCapForDrainHorizon(readyCount, baseCap, drainDays, wavesPerVerticalPerDay int) int {
	if baseCap <= 0 {
		return 0
	}
	if drainDays <= 0 {
		drainDays = 1
	}
	if wavesPerVerticalPerDay <= 0 || readyCount <= 0 {
		return 0
	}
	horizonWaves := wavesPerVerticalPerDay * drainDays
	drainCap := (readyCount + horizonWaves - 1) / horizonWaves
	if drainCap < 1 {
		drainCap = 1
	}
	if drainCap > baseCap {
		drainCap = baseCap
	}
	return drainCap
}

func (po *PartnerDripOrchestrator) wavesPerVerticalPerDay(followup bool) int {
	interval := po.cfg.TickInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticksPerDay := int((24 * time.Hour) / interval)
	if ticksPerDay < 1 {
		ticksPerDay = 96
	}
	brands := po.cfg.BrandsPerTick
	if followup {
		brands = po.cfg.FollowupBrandsPerTick
	}
	if brands <= 0 {
		brands = 4
	}
	return ticksPerDay * brands
}

func cloneISPCapMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func baseISPCap(perISPCaps map[string]int, isp string) int {
	if cap, ok := perISPCaps[isp]; ok {
		return cap
	}
	return perISPCaps["other"]
}

// resolvePerISPCaps merges static PerISPCapPerWave ceilings with drain-horizon
// caps for ISPs listed in PerISPDrainDays. Recomputed every wave from live DB
// counts so caps rise if the ingest queue refills during the drain window.
//
// Dataset-scoped overrides: any per-ISP max_per_wave row in
// partner_isp_distribution_overrides for this wave's dominant dataset REPLACES
// the global PerISPCapPerWave ceiling for that ISP — for this dataset only.
// This lets one Yahoo-heavy list run at a raised Yahoo cap without lifting the
// reputation-protective global caps for every other vertical/brand. The
// override becomes the base the drain-horizon clamp is computed against, so a
// raised cap still floats down on a tiny backlog. Best-effort: any lookup
// error falls back to the global caps.
// basePerISPCaps returns the compiled per-wave caps with the DB overlay applied.
// Overlay-not-replace: a partial table cannot silently zero an ISP that the
// operator never configured.
func (po *PartnerDripOrchestrator) basePerISPCaps() map[string]int {
	out := cloneISPCapMap(po.cfg.PerISPCapPerWave)
	po.ispCapMu.RLock()
	defer po.ispCapMu.RUnlock()
	for isp, cap := range po.ispCapCache {
		out[strings.ToLower(strings.TrimSpace(isp))] = cap
	}
	return out
}

func (po *PartnerDripOrchestrator) resolvePerISPCaps(ctx context.Context, vertical, datasetID, backlogKind string) (map[string]int, error) {
	base := po.basePerISPCaps()
	if datasetID != "" {
		po.applyDatasetISPCapOverrides(ctx, datasetID, base)
	}
	caps := cloneISPCapMap(base)
	if len(po.cfg.PerISPDrainDays) == 0 {
		return caps, nil
	}

	// EXPRESS datasets are exempt from the multi-day drain horizon.
	//
	// The horizon exists to spread a large COLD backlog over N days so a
	// refilling ingest queue can't dump on a reputation-sensitive ISP. An
	// express dataset is the opposite shape: partner-validated form fills
	// arriving in real time, whose creative says "your quotes are ready" —
	// their value decays in minutes, and the operator set express_dispatch
	// precisely to mail on arrival.
	//
	// Leaving the horizon on silently defeats that. Measured 2026-08-11 on
	// internal_auto_insurance: AOL (drain_days=2) drained at a FLAT 16/hour
	// against 60-100/hour of inflow, so the queue grew ~+45..+84/hour to 290
	// records aged 4h, while every ISP NOT in PerISPDrainDays cleared in ~11
	// minutes. Raising the cap could not fix it — both the DB overlay and the
	// per-dataset max_per_wave override are the BASE the horizon clamps down
	// from, so the horizon always won.
	//
	// Scope: express_dispatch is 1 of 29 datasets, so this changes exactly the
	// lane it is set on. The per-wave base cap still applies — this removes the
	// multi-day SPREAD, not the ceiling.
	if datasetID != "" && po.datasetIsExpress(ctx, datasetID) {
		return caps, nil
	}

	followup := backlogKind == ispCapBacklogFollowup
	query := `
		SELECT COALESCE(NULLIF(isp_family, ''), 'other') AS isp, COUNT(*)
		FROM partner_clean_queue
		WHERE vertical = $1 AND status = 'ready'
		GROUP BY 1
	`
	if followup {
		query = `
			SELECT COALESCE(NULLIF(isp_family, ''), 'other') AS isp, COUNT(*)
			FROM partner_clean_queue
			WHERE vertical = $1
			  AND status = 'mailed'
			  AND next_touch_at IS NOT NULL
			  AND next_touch_at <= NOW()
			GROUP BY 1
		`
	}

	readyByISP := make(map[string]int)
	if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, vertical)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isp string
			var n int
			if err := rows.Scan(&isp, &n); err != nil {
				continue
			}
			readyByISP[strings.ToLower(strings.TrimSpace(isp))] = n
		}
		return rows.Err()
	}); err != nil {
		// Graceful degradation: the drain-horizon recompute is an optimization
		// (it widens caps when the queue is small). If it can't run — e.g. the
		// per-ISP COUNT loses the IO race under a finalization storm — DO NOT
		// fail the whole wave. Fall back to the static PerISPCapPerWave
		// ceilings so the orchestrator still ships at the conservative caps
		// instead of shipping nothing. (2026-06-07 incident.)
		log.Printf("[PartnerDripOrchestrator] resolvePerISPCaps vertical=%s: drain-horizon recompute failed (%v) — falling back to static caps", vertical, err)
		return caps, nil
	}

	wavesPerDay := po.wavesPerVerticalPerDay(followup)
	for isp, drainDays := range po.cfg.PerISPDrainDays {
		isp = strings.ToLower(strings.TrimSpace(isp))
		caps[isp] = ispCapForDrainHorizon(readyByISP[isp], baseISPCap(base, isp), drainDays, wavesPerDay)
	}
	return caps, nil
}

// readyCountByISP returns the ready-record count per ISP family for a vertical.
// Same query shape resolvePerISPCaps uses for its drain-horizon recompute,
// factored out so the governed floor gate can reuse it without duplicating SQL.
func (po *PartnerDripOrchestrator) readyCountByISP(ctx context.Context, vertical string) (map[string]int, error) {
	out := make(map[string]int)
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT COALESCE(NULLIF(isp_family, ''), 'other') AS isp, COUNT(*)
			FROM partner_clean_queue
			WHERE vertical = $1 AND status = 'ready'
			GROUP BY 1
		`, vertical)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isp string
			var n int
			if err := rows.Scan(&isp, &n); err != nil {
				continue
			}
			out[strings.ToLower(strings.TrimSpace(isp))] = n
		}
		return rows.Err()
	})
	return out, err
}

// governedFloorGateBinds decides SAFEGUARD (a) for one ISP: returns true (=> the
// governed pass MUST NOT claim this ISP, force cap 0) when the drain-horizon cap
// is below the static per-wave base — i.e. the pool is thin and the normal
// brands' caps float with ready depth, so any governed consumption would lower
// their next cap. When the static per-wave cap binds (huge pool, drainCap >=
// base), governed claiming is side-effect-free and the gate allows it.
//
// ISPs NOT in PerISPDrainDays have no drain-horizon clamp at all (their cap is
// always the static base), so they always bind => never gated.
func (po *PartnerDripOrchestrator) governedFloorGateBinds(vertical, isp string, base int, readyByISP map[string]int) bool {
	lisp := strings.ToLower(strings.TrimSpace(isp))
	drainDays, drained := po.cfg.PerISPDrainDays[lisp]
	if !drained {
		return false // no drain-horizon => static cap always binds => allow
	}
	if base <= 0 {
		return true // nothing to claim anyway; treat as gated
	}
	drainCap := ispCapForDrainHorizon(readyByISP[lisp], base, drainDays, po.wavesPerVerticalPerDay(false))
	// Gate (force 0) when the drain horizon binds BELOW the static base.
	return drainCap < base
}

// applyGovernedFloorGate zeroes the governed per-ISP cap for every ISP where the
// drain-horizon binds below the static base (SAFEGUARD (a)). Reads ready depth
// once per call (reused across all ISPs). Best-effort: if the count fails, the
// SAFE direction is to gate ALL drain-managed ISPs (the governed pass claims
// nothing on a vertical it can't measure) rather than risk competing for a thin
// pool — so on error every PerISPDrainDays ISP is zeroed.
func (po *PartnerDripOrchestrator) applyGovernedFloorGate(ctx context.Context, vertical, datasetID string, caps map[string]int) map[string]int {
	out := cloneISPCapMap(caps)
	// Recompute the static base the gate compares against (mirrors
	// resolvePerISPCaps' base: global caps + any per-dataset max_per_wave
	// override). The drain-horizon clamp in `caps` is what we're guarding
	// against, so we compare to the UN-drained base here.
	base := po.basePerISPCaps()
	if datasetID != "" {
		po.applyDatasetISPCapOverrides(ctx, datasetID, base)
	}
	readyByISP, err := po.readyCountByISP(ctx, vertical)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] governed floor-gate vertical=%s: ready-count failed (%v) — gating all drain-managed ISPs", vertical, err)
		for isp := range out {
			if _, drained := po.cfg.PerISPDrainDays[strings.ToLower(strings.TrimSpace(isp))]; drained {
				out[isp] = 0
			}
		}
		return out
	}
	for isp := range out {
		if po.governedFloorGateBinds(vertical, isp, baseISPCap(base, isp), readyByISP) {
			out[isp] = 0
		}
	}
	return out
}

// applyDatasetISPCapOverrides overlays per-ISP max_per_wave ceilings from
// partner_isp_distribution_overrides for one dataset onto base, mutating base
// in place. Only rows with a positive max_per_wave override the global cap;
// pct_override (distribution shaping) is ignored here. Best-effort — on any
// error the caller keeps the global caps. dataset_id is a UUID column, so the
// param is cast explicitly.
// datasetIsExpress reports whether the dataset is flagged express_dispatch
// (mail-on-arrival). Fail-SAFE: any lookup error returns false, which keeps the
// conservative drain-horizon clamp in place rather than removing a
// reputation guard because a query lost an IO race.
func (po *PartnerDripOrchestrator) datasetIsExpress(ctx context.Context, datasetID string) bool {
	var express bool
	if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE(express_dispatch, FALSE)
			FROM partner_datasets WHERE id = $1::uuid
		`, datasetID).Scan(&express)
	}); err != nil {
		log.Printf("[PartnerDripOrchestrator] express lookup (dataset=%s) failed (%v) — keeping drain horizon", datasetID, err)
		return false
	}
	return express
}

func (po *PartnerDripOrchestrator) applyDatasetISPCapOverrides(ctx context.Context, datasetID string, base map[string]int) {
	if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT LOWER(TRIM(isp)) AS isp, max_per_wave
			FROM partner_isp_distribution_overrides
			WHERE dataset_id = $1::uuid
			  AND max_per_wave IS NOT NULL
			  AND max_per_wave > 0
		`, datasetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isp string
			var mpw int
			if err := rows.Scan(&isp, &mpw); err != nil {
				continue
			}
			if isp == "" {
				continue
			}
			base[isp] = mpw
		}
		return rows.Err()
	}); err != nil {
		log.Printf("[PartnerDripOrchestrator] dataset ISP cap overrides (dataset=%s) failed (%v) — using global caps", datasetID, err)
	}
}

// applyThroughputSafety filters claimed records based on:
//  1. Active ISP backoff in mailing_isp_throttle_state (rate < threshold).
//  2. Per-ISP cap per wave (resolved caps passed by caller).
//
// Returns (keep, deferred, reasons-by-isp).
func (po *PartnerDripOrchestrator) applyThroughputSafety(ctx context.Context, brand string, recs []claimedRecord, perISPCaps map[string]int) ([]claimedRecord, []claimedRecord, map[string]string, error) {
	throttled, err := po.throttledISPs(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	// SES-pinned (brand,isp) pairs route through the SES relay, NOT the PMTA
	// pipe whose reputation/throttle state mailing_isp_throttle_state tracks —
	// so the PMTA throttle deferral must NOT gate them (otherwise a 0-msgs/hr
	// ISP like a collapsed Microsoft pipe would defer every record we are
	// deliberately re-routing to SES, and they'd never ship). Operator 2026-06-16.
	sesPins := dripBrandISPSESProfiles()[strings.ToLower(strings.TrimSpace(brand))]
	keep := make([]claimedRecord, 0, len(recs))
	deferred := make([]claimedRecord, 0)
	counts := make(map[string]int)
	reasons := make(map[string]string)
	for _, r := range recs {
		isp := r.ispFamily
		if isp == "" {
			isp = "other"
		}
		_, sesRouted := sesPins[isp]
		if rate, ok := throttled[isp]; ok && !sesRouted {
			if existing, exists := reasons[isp]; !exists || existing == "" {
				reasons[isp] = fmt.Sprintf("throttled (rate=%.1f)", rate)
			}
			deferred = append(deferred, r)
			continue
		}
		cap, hasCap := perISPCaps[isp]
		if !hasCap {
			cap = perISPCaps["other"]
		}
		if cap > 0 && counts[isp] >= cap {
			if existing, exists := reasons[isp]; !exists || existing == "" {
				reasons[isp] = fmt.Sprintf("per-wave cap reached (%d)", cap)
			}
			deferred = append(deferred, r)
			continue
		}
		counts[isp]++
		keep = append(keep, r)
	}
	return keep, deferred, reasons, nil
}

// partnerDripTickLockKey is the tick-lease key. The advisory-lock id below is
// derived from it exactly the way distlock.NewPGAdvisoryLock derives its id
// (fnv64a of the key string → int64) so this lease contends with any other
// holder of the same key. Pinned here rather than calling into distlock:
// distlock only exposes pooled-*sql.DB locks (the bug — see acquireTickLease),
// and extending it is out of scope for this hotfix.
const partnerDripTickLockKey = "partner-drip-tick"

// partnerDripTickLockID mirrors distlock.NewPGAdvisoryLock's id derivation.
func partnerDripTickLockID() int64 {
	h := fnv.New64a()
	h.Write([]byte(partnerDripTickLockKey))
	return int64(h.Sum64())
}

// acquireTickLease takes the single-leader lease for one tick. Returns a
// release func and whether this instance leads. See the tickOnce comment for
// why this exists (desiredCount=2 ran two uncoordinated orchestrators).
//
// INCIDENT 2026-08-22 (~22:20 UTC): the first version used distlock's PG
// advisory fallback on the POOLED *sql.DB. pg_try_advisory_lock is
// SESSION(connection)-scoped: Acquire ran on one pooled connection while
// Release's pg_advisory_unlock landed on a DIFFERENT pooled connection — a
// silent no-op (returns false + SQL WARNING) — so the lock stayed held by an
// idle pooled connection forever. From the second tick onward BOTH ECS tasks
// got acquired=false and skipped every tick; ALL partner-drip sending stopped
// ~40 min after boot. (The comment that stood here claimed the different-conn
// no-op benignly "pins leadership" — it wedged every tick on both tasks
// instead.)
//
// The fix: hold the advisory lock on a DEDICATED connection for the tick's
// duration and unlock ON THAT SAME connection. If the task is killed or
// crashes mid-tick, the dedicated session dies with it and Postgres frees the
// lock automatically, so leadership migrates to the surviving task.
//
// Fail-OPEN on any lock-infrastructure error (conn checkout or acquire query):
// locking must never stop the drip — worst case is the pre-lease dual-tick
// behavior. Kill switch: PARTNER_DRIP_TICK_LOCK_DISABLED=1 restores dual-tick.
func (po *PartnerDripOrchestrator) acquireTickLease() (func(), bool) {
	if v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_TICK_LOCK_DISABLED")); v == "1" || strings.EqualFold(v, "true") {
		return func() {}, true
	}
	conn, err := po.db.Conn(po.ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] tick lease conn checkout error (fail-open, ticking anyway): %v", err)
		return func() {}, true
	}
	lockID := partnerDripTickLockID()
	var acquired bool
	if err := conn.QueryRowContext(po.ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired); err != nil {
		conn.Close()
		log.Printf("[PartnerDripOrchestrator] tick lease acquire error (fail-open, ticking anyway): %v", err)
		return func() {}, true
	}
	if !acquired {
		conn.Close()
		log.Printf("[PartnerDripOrchestrator] tick lease held by the other instance — skipping tick")
		return nil, false
	}
	return func() {
		// Unlock on the SAME dedicated connection that acquired the lock —
		// this is the whole fix. Background context: release must run even
		// when po.ctx is being torn down.
		var released bool
		if rerr := conn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", lockID).Scan(&released); rerr != nil {
			log.Printf("[PartnerDripOrchestrator] tick lease release error: %v", rerr)
		} else if !released {
			log.Printf("[PartnerDripOrchestrator] WARNING: tick lease pg_advisory_unlock returned false — lock was not held by this session")
		}
		if cerr := conn.Close(); cerr != nil {
			log.Printf("[PartnerDripOrchestrator] tick lease conn close error: %v", cerr)
		}
	}, true
}

// loadThrottledISPs refreshes the per-tick throttle snapshot. Called once from
// tickOnce alongside the other per-tick loads; every wave's
// applyThroughputSafety then reads the cache instead of re-querying
// mailing_isp_throttle_state (previously 8-10+ reads per vertical per tick).
// The error is cached too so a failed read keeps the exact pre-cache
// semantics: applyThroughputSafety surfaces it and its callers proceed
// without deferral for this tick.
func (po *PartnerDripOrchestrator) loadThrottledISPs(ctx context.Context) {
	m, err := po.fetchThrottledISPs(ctx)
	po.throttleMu.Lock()
	po.throttleCache, po.throttleErr, po.throttleLoaded = m, err, true
	po.throttleMu.Unlock()
}

// throttledISPs returns the tick's throttle snapshot. Falls back to a direct
// fetch when no snapshot has been loaded (a call path that never went through
// tickOnce — e.g. tests), preserving the pre-cache behavior exactly.
func (po *PartnerDripOrchestrator) throttledISPs(ctx context.Context) (map[string]float64, error) {
	po.throttleMu.RLock()
	if po.throttleLoaded {
		m, err := po.throttleCache, po.throttleErr
		po.throttleMu.RUnlock()
		return m, err
	}
	po.throttleMu.RUnlock()
	return po.fetchThrottledISPs(ctx)
}

// fetchThrottledISPs returns isp -> msgs_per_hour for any ISPs whose rate is
// below ThrottledISPRateThreshold. These are skipped from the upcoming wave.
// Per-tick callers should go through throttledISPs (the cached snapshot); the
// ThrottleDeferralDisabled short-circuit and the missing-table = no-throttling
// behavior live HERE so both the cached and fallback paths share them.
func (po *PartnerDripOrchestrator) fetchThrottledISPs(ctx context.Context) (map[string]float64, error) {
	if po.cfg.ThrottleDeferralDisabled {
		return map[string]float64{}, nil
	}
	rows, err := po.db.QueryContext(ctx, `
		SELECT isp, msgs_per_hour
		FROM mailing_isp_throttle_state
		WHERE msgs_per_hour < $1
	`, po.cfg.ThrottledISPRateThreshold)
	if err != nil {
		// Table may not be populated; treat as no throttling.
		if strings.Contains(err.Error(), "does not exist") {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var isp string
		var rate float64
		if err := rows.Scan(&isp, &rate); err == nil {
			out[strings.ToLower(strings.TrimSpace(isp))] = rate
		}
	}
	return out, nil
}

func tallyISPs(recs []claimedRecord) map[string]int {
	out := make(map[string]int)
	for _, r := range recs {
		isp := r.ispFamily
		if isp == "" {
			isp = "other"
		}
		out[isp]++
	}
	return out
}

// buildCampaignInput mirrors deploy_may12_mature.py's build_payload but
// expressed as the typed engine.PMTACampaignInput. Window is anchored to
// scheduled_at + cfg.WindowHours so the wave_sanity_check 8h floor is
// satisfied and time_spans[*].source = "duration-calc".
func (po *PartnerDripOrchestrator) buildCampaignInput(v verticalState, brand string, creative creativeRec, segmentID string, ispCounts map[string]int) (engine.PMTACampaignInput, error) {
	domain, ok := resolveBrandSendingDomain(brand)
	if !ok {
		return engine.PMTACampaignInput{}, fmt.Errorf("unknown brand %q", brand)
	}
	startAt := time.Now().UTC().Add(2 * time.Minute)
	endAt := startAt.Add(time.Duration(po.cfg.WindowHours) * time.Hour)
	span := engine.PMTATimeSpanInput{
		Type:     "absolute",
		StartAt:  &startAt,
		EndAt:    &endAt,
		Timezone: "America/Denver",
		Source:   "duration-calc", // mandatory per sending-throttle.mdc
	}

	plans := make([]engine.PMTAISPScheduleInput, 0, len(ispCounts))
	quotas := make([]engine.ISPQuota, 0, len(ispCounts))
	targetISPs := make([]engine.ISP, 0, len(ispCounts))
	for ispName, count := range ispCounts {
		ispISP := engine.ISP(ispName)
		quotas = append(quotas, engine.ISPQuota{ISP: ispName, Volume: count})
		targetISPs = append(targetISPs, ispISP)
		plans = append(plans, engine.PMTAISPScheduleInput{
			ISP:               ispName,
			Quota:             count,
			RandomizeAudience: false,
			ThrottleStrategy:  "gentle",
			Timezone:          "America/Denver",
			Cadence: engine.PMTACadenceInput{
				Mode:         "interval",
				EveryMinutes: 15,
				BatchSize:    0,
			},
			TimeSpans: []engine.PMTATimeSpanInput{span},
		})
	}

	useMaster := false
	contentLocked := true
	scheduledAt := startAt
	htmlSHA := sha256.Sum256([]byte(creative.htmlBody))
	// The name MUST be unique per wave group. HandleDeployCampaign has a
	// by-(org,name) idempotency guard (handlers_pmta_campaign.go:1305) that
	// converges an id-less deploy onto any live campaign with the same name —
	// so a colliding name does not mint a second campaign, it RETURNS THE
	// FIRST ONE, and this wave's records get stamped onto a campaign whose
	// audience is some other wave's segment.
	//
	// The old format used MINUTE precision plus the creative hash, so two wave
	// groups of the same (vertical, brand, creative) deploying in the same
	// minute produced a byte-identical name. Measured 2026-08-11 on
	// internal_auto_insurance: two groups fired at 21:15:01 and 21:15:02, both
	// got campaign caa1f42d back, and the second group's 349 records were
	// marked mailed against a campaign that only ever had the first group's 2
	// recipients — 349 real subscribers terminally burned, their 349-member
	// segment orphaned with zero campaigns referencing it. ~1,118 records were
	// lost this way in a single day before it was caught.
	//
	// segmentID is minted per wave group (createWaveSegment) and is therefore
	// the correct uniqueness key; seconds are added so the name stays readable
	// and sortable. Deterministic within one deploy call, which is what the
	// idempotency guard legitimately needs for genuine retries.
	name := fmt.Sprintf("[partner-drip] %s %s %s %s %s",
		v.vertical, brand,
		time.Now().UTC().Format("20060102T150405"),
		hex.EncodeToString(htmlSHA[:4]),
		safeSegmentNonce(segmentID))

	// Per-touch offer wins: when this touch's creative row carries its own
	// offer_id, that offer drives the deploy's OfferID (offer-suppression Bloom +
	// DNM scrub + content_locked inheritance + offer/money attribution) so each
	// touch scrubs ITS OWN offer, not the dataset's. Empty creative.offerID falls
	// back to the dataset's offerID exactly as before (single-offer drips unchanged).
	offerID := v.offerID
	if creative.offerID != "" {
		offerID = creative.offerID
	}

	return engine.PMTACampaignInput{
		Name: name,
		// OfferID drives the offer-suppression Bloom + DNM scrub and content_locked
		// inheritance. It is the PER-TOUCH offer (creative.offerID) when the touch's
		// creative row binds one, else the dataset's offer (v.offerID) — set only for
		// direct-offer datasets, empty for the legacy drip pool. content_locked is
		// passed true explicitly for safety regardless.
		OfferID:       offerID,
		TargetISPs:    targetISPs,
		SendingDomain: domain,
		Variants: []engine.ContentVariant{{
			VariantName:  "A",
			FromName:     creative.fromName,
			Subject:      creative.subject,
			PreviewText:  creative.preheader,
			HTMLContent:  creative.htmlBody,
			PlainContent: "",
			SplitPercent: 100,
		}},
		ISPPlans:           plans,
		InclusionSegments:  []string{segmentID},
		InclusionLists:     []string{},
		SendPriority:       []engine.PriorityItem{{ID: segmentID, Type: "segment"}},
		ExclusionSegments:  []string{},
		ExclusionLists:     []string{},
		Timezone:           "America/Denver",
		ThrottleStrategy:   "gentle",
		ISPQuotas:          quotas,
		RandomizeAudience:  false,
		SendMode:           "scheduled",
		ScheduledAt:        &scheduledAt,
		MinRemailHours:     0,
		UseMasterSelection: &useMaster,
		ContentLocked:      &contentLocked,
	}, nil
}

// stampPartnerAttributionOnCampaign writes partner_drip_tag and
// partner_dataset_id onto the freshly-deployed campaign row. These columns
// are the join key the analytics dashboard uses to slice campaigns by feed.
// HandleDeployCampaign doesn't accept these fields directly, so we patch
// post-deploy. Errors are logged but not fatal — the wave still mails;
// analytics will just be missing this attribution row.
func (po *PartnerDripOrchestrator) stampPartnerAttributionOnCampaign(ctx context.Context, campaignID, datasetID, partnerSlug, vertical string) error {
	if campaignID == "" {
		return nil
	}
	tag := fmt.Sprintf("data_partner:%s/%s", safeIdent(partnerSlug), vertical)
	var datasetArg interface{}
	if datasetID != "" {
		datasetArg = datasetID
	}
	_, err := po.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET partner_drip_tag = $2,
		    partner_dataset_id = $3,
		    updated_at = NOW()
		WHERE id = $1
	`, campaignID, tag, datasetArg)
	return err
}

// markMailed stamps the queue rows with the wave's outcome and advances
// the touch state machine:
//
//   - touch_count -> touch_count + 1
//   - last_touch_brand -> brand
//   - last_touch_campaign_id -> campaignID
//   - next_touch_at -> NOW() + 24h while touch_count < MaxTouchCount, else NULL
//   - terminal_reason -> 'completed' once touch_count reaches MaxTouchCount
//
// status remains 'mailed' on every successful wave so existing analytics
// and dashboard SUM(status='mailed') queries continue to give a useful
// "at least one wave shipped" count. The follow-up orchestrator queries
// touch_count + next_touch_at + engaged_at to find the next-due record.
//
// On the FIRST touch (touch_count was 0) we also stamp mailed_at +
// mailed_campaign_id + mailed_brand so the legacy fields stay populated
// for backwards-compatible dashboards.
//
// DURABILITY (2026-08-20 incident, campaign ff01ad90-e3fc-4d0e-bc55-239e8ce35d69):
// this used to be ONE unbounded `WHERE id = ANY($1::uuid[])` on the app pool at
// the global 30s statement_timeout. A wave carries up to cfg.MaxWaveSize (5000)
// ids; that array against an 11.8M-row / 10GB partner_clean_queue under IO load
// hit `canceling statement due to statement timeout`, and because the caller only
// LOGS the error the ladder advance was lost permanently: 314 people were mailed
// with touch_count still 1, next_touch_at still in the past, and status stuck at
// 'claimed' — exactly the shape that re-mailed 4,104 people three times on
// 2026-08-17. The stamp is now chunked, retried, and its loss is recorded.
//
// Re-application is safe: the WHERE clause carries
// `last_touch_campaign_id IS DISTINCT FROM $2::uuid`, so a chunk that already
// landed is a zero-row no-op on retry (one campaign == one touch, so this can
// never suppress a legitimate second advance).
func (po *PartnerDripOrchestrator) markMailed(ctx context.Context, recs []claimedRecord, campaignID, brand string) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	gap := time.Duration(followupTouchGapHours) * time.Hour
	// Computed ONCE for the whole wave so a retried chunk writes the same
	// next_touch_at as its first attempt — the stamp stays deterministic.
	nextTouchAt := time.Now().UTC().Add(gap)

	var stamped, lost int64
	var firstErr error
	for start := 0; start < len(ids); start += markMailedChunkSize {
		end := start + markMailedChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		n, err := po.stampMailedChunk(ctx, chunk, campaignID, brand, nextTouchAt)
		stamped += n
		if err != nil {
			lost += int64(len(chunk))
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				// Shutdown / cancelled tick: every remaining chunk would fail the
				// same way. Stop grinding, but still account for what was lost so
				// the recovery pass can pick it up.
				lost += int64(len(ids) - end)
				break
			}
		}
	}

	po.stamp.rowsStamped.Add(stamped)
	if lost == 0 {
		return nil
	}
	po.stamp.rowsLost.Add(lost)
	po.stamp.wavesFailed.Add(1)
	po.stamp.lastFailureUnix.Store(time.Now().UTC().Unix())
	po.recordStampFailure(ctx, campaignID, brand, lost, firstErr)
	log.Printf("[PartnerDripOrchestrator] ALERT stamp_lost campaign=%s brand=%s rows_lost=%d rows_stamped=%d — ladder NOT advanced; recover with PARTNER_DRIP_STAMP_RECOVERY=apply (see partner_drip_stamp_failures): %v",
		campaignID, brand, lost, stamped, firstErr)
	return fmt.Errorf("mark_mailed: %d/%d rows unstamped: %w", lost, len(ids), firstErr)
}

// tickFollowups runs the follow-up touch loop across all verticals
// represented in the queue. For each vertical it picks the next 'followup'
// brand off the round-robin and claims up to per-ISP-capped records that
// satisfy: status='mailed', touch_count IN (1,2,3), next_touch_at <= NOW(),
// engaged_at IS NULL, terminal_reason IS NULL.
//
// The follow-up loop uses a SHARED 'followup' brand round-robin
// (partner_drip_state row vertical='followup') so brand rotation is
// independent of the welcome pipeline. This keeps welcome and follow-up
// traffic spread evenly across all 16 brands.
func (po *PartnerDripOrchestrator) tickFollowups(ctx context.Context) {
	verticals, err := po.followupVerticalsWithDueRecords(ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] followup_verticals: %v", err)
		return
	}
	if len(verticals) == 0 {
		return
	}
	state, err := po.followupBrandIndex(ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] followup_brand_index: %v", err)
		return
	}

	brandsThisTick := po.cfg.FollowupBrandsPerTick
	if brandsThisTick > len(dripBrands) {
		brandsThisTick = len(dripBrands)
	}

	for _, v := range verticals {
		// Verticals with a dedicated warm-up roster (e.g. samsclub_internal on
		// the new KumoMTA domains) send WELCOME touches only: no multi-touch on
		// fresh IPs, and the mature brands stay off this vertical entirely.
		if _, dedicated := verticalBrandRoster[strings.ToLower(strings.TrimSpace(v.vertical))]; dedicated {
			continue
		}
		// EXPRESS verticals bypass the blind brand round-robin (operator
		// 2026-08-16: "this must drain, it is our money maker"). The shared
		// 16-brand rotation gives a vertical FollowupBrandsPerTick blind picks
		// per tick, so a lane whose due records live on ONE brand (measured:
		// internal_auto_insurance, 32,711 due — 100% on db, db-only creative
		// roster) lands a productive wave ~once per hour while 15/16 picks
		// resolve no creative and ship nothing. Express records are real-time
		// form fills whose value decays in minutes — for them, fire a wave for
		// every brand that actually HOLDS due records, every tick. Cold lanes
		// keep the rotation (and its reputation pacing) unchanged, and the
		// shared rotation pointer is not consumed by the express path.
		if v.datasetID != "" && po.datasetIsExpress(ctx, v.datasetID) {
			limit := brandsThisTick * 2
			if limit < 8 {
				limit = 8
			}
			loaded, lerr := po.expressFollowupBrands(ctx, v.vertical, limit)
			if lerr != nil {
				log.Printf("[PartnerDripOrchestrator] express followup brands vertical=%s: %v — falling back to rotation", v.vertical, lerr)
			} else if len(loaded) > 0 {
				for _, brand := range loaded {
					if ctx.Err() != nil {
						return
					}
					if err := po.processFollowup(ctx, v, brand); err != nil {
						log.Printf("[PartnerDripOrchestrator] express followup vertical=%s brand=%s: %v", v.vertical, brand, err)
					}
					if po.cfg.YahooNewsletterOnlyDrip {
						if err := po.processFollowupYahooNewsletter(ctx, v, brand); err != nil {
							log.Printf("[PartnerDripOrchestrator] express followup-yahoo-nl vertical=%s brand=%s: %v", v.vertical, brand, err)
						}
					}
				}
				continue
			}
			// No loaded brands (or lookup error): fall through to the rotation
			// so an express vertical is never worse off than a cold one.
		}
		// Each vertical is processed independently. The follow-up brand
		// rotation is shared across verticals — every (vertical, brand)
		// combination gets a wave at most once per tick.
		for i := 0; i < brandsThisTick; i++ {
			if ctx.Err() != nil {
				return
			}
			brand, err := po.pickNextFollowupBrand(ctx, v.vertical, state)
			if err != nil {
				log.Printf("[PartnerDripOrchestrator] followup pick_brand: %v", err)
				return
			}
			if err := po.processFollowup(ctx, v, brand); err != nil {
				log.Printf("[PartnerDripOrchestrator] followup vertical=%s brand=%s: %v", v.vertical, brand, err)
			}
			// Yahoo newsletter-only (operator 2026-07-14): the offer pass above
			// excluded yahoo; serve yahoo its brand newsletter for this touch here.
			if po.cfg.YahooNewsletterOnlyDrip {
				if err := po.processFollowupYahooNewsletter(ctx, v, brand); err != nil {
					log.Printf("[PartnerDripOrchestrator] followup-yahoo-nl vertical=%s brand=%s: %v", v.vertical, brand, err)
				}
			}
			// Advance the round-robin index AFTER every brand attempt,
			// regardless of success. This guarantees we don't get stuck
			// on a single brand if its claim fails.
			state.brandIndex = (state.brandIndex + 1) % len(dripBrands)
			if err := po.persistFollowupBrandIndex(ctx, state.brandIndex); err != nil {
				log.Printf("[PartnerDripOrchestrator] followup persist_brand_index: %v", err)
			}
		}
	}
}

// expressFollowupBrands returns the brands that actually hold due follow-up
// records for an express vertical, heaviest backlog first. This is the express
// path's replacement for the blind rotation pick: a brand appears here iff a
// processFollowup wave for it can claim at least one record right now.
func (po *PartnerDripOrchestrator) expressFollowupBrands(ctx context.Context, vertical string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 8
	}
	var out []string
	// Same engagement gate as the claim, so the express pass only fires waves
	// for brands that still hold ELIGIBLE due records on a gated vertical.
	engGate := engagementGateSQL(vertical, "")
	engagedExit := engagedExitSQL(vertical, "")
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT COALESCE(NULLIF(last_touch_brand, ''), mailed_brand) AS brand, COUNT(*) AS due
			FROM partner_clean_queue
			WHERE vertical = $1
			  AND status = 'mailed'`+engagedExit+`
			  AND terminal_reason IS NULL
			  AND next_touch_at IS NOT NULL
			  AND next_touch_at <= NOW()
			  AND touch_count BETWEEN 1 AND $2
			  AND COALESCE(NULLIF(last_touch_brand, ''), mailed_brand) IS NOT NULL`+engGate+`
			GROUP BY 1
			ORDER BY due DESC
			LIMIT $3
		`, vertical, MaxTouchCount-1, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var brand string
			var due int
			if err := rows.Scan(&brand, &due); err != nil {
				continue
			}
			if b := strings.ToLower(strings.TrimSpace(brand)); b != "" {
				out = append(out, b)
			}
		}
		return rows.Err()
	})
	return out, err
}

// followupState is a tiny in-memory mirror of the 'followup' partner_drip_state
// row used to track the brand round-robin pointer between intra-tick calls.
type followupState struct {
	brandIndex int
}

func (po *PartnerDripOrchestrator) followupBrandIndex(ctx context.Context) (*followupState, error) {
	var idx int
	err := po.db.QueryRowContext(ctx,
		`SELECT next_brand_index FROM partner_drip_state WHERE vertical = 'followup'`,
	).Scan(&idx)
	if err == sql.ErrNoRows {
		// Seeded by startup migration but defensive fallback.
		_, _ = po.db.ExecContext(ctx,
			`INSERT INTO partner_drip_state (vertical, next_brand_index) VALUES ('followup', 0) ON CONFLICT (vertical) DO NOTHING`)
		return &followupState{brandIndex: 0}, nil
	}
	if err != nil {
		return nil, err
	}
	return &followupState{brandIndex: idx % len(dripBrands)}, nil
}

func (po *PartnerDripOrchestrator) persistFollowupBrandIndex(ctx context.Context, idx int) error {
	_, err := po.db.ExecContext(ctx, `
		INSERT INTO partner_drip_state (vertical, next_brand_index, updated_at)
		VALUES ('followup', $1, NOW())
		ON CONFLICT (vertical) DO UPDATE SET
		    next_brand_index = EXCLUDED.next_brand_index,
		    updated_at = NOW()
	`, idx)
	return err
}

// =========================================================================
// GOVERNED PASS (Option B) — additive third pass for the Kumo properties
// =========================================================================
//
// tickGoverned mirrors tickFollowups: it enumerates the verticals that (a) have
// governed subscribers and (b) have ready records, and for each walks ONLY that
// vertical's subscribed governed brands off a SEPARATE '<vertical>:governed'
// rotation state row, firing WELCOME (status='ready') waves via the shared
// processVerticalWith builder. It is strictly additive: it runs after the
// welcome + follow-up passes, claims only what they leave, writes only to
// '<vertical>:governed' state rows, and the floor gate keeps it off thin pools
// so the 16 normal brands' caps are never lowered.
//
// SAFEGUARD (c) — DEPLOY THROTTLE: at most GovernedBrandsPerTick brands per
// vertical per tick, fired SEQUENTIALLY (no burst). Post-deploy, VERIFY governed
// campaigns persisted BY NAME (staging-burst footgun: /pmta-campaign deploy can
// return HTTP 200 with a wrong/empty campaign_id under concurrent burst).
func (po *PartnerDripOrchestrator) tickGoverned(ctx context.Context) {
	verticals, err := po.governedVerticalsWithBacklog(ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] governed_verticals: %v", err)
		return
	}
	if len(verticals) == 0 {
		return
	}
	brandsThisTick := po.cfg.GovernedBrandsPerTick
	if brandsThisTick <= 0 {
		brandsThisTick = 2
	}
	for _, v := range verticals {
		if ctx.Err() != nil {
			return
		}
		roster := po.orderedSubscribedGoverned(v.vertical)
		if len(roster) == 0 {
			continue // no active governed subscribers for this vertical
		}
		stateKey := governedStateKey(v.vertical)
		// Each governed wave advances the '<vertical>:governed' pointer; cap the
		// per-tick brand count at the roster length (no point cycling past it).
		n := brandsThisTick
		if n > len(roster) {
			n = len(roster)
		}
		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				return
			}
			pc := passContext{roster: roster, stateKey: stateKey}
			if err := po.processVerticalWith(ctx, v, pc); err != nil {
				log.Printf("[PartnerDripOrchestrator] governed vertical=%s: %v", v.vertical, err)
				// Keep advancing even on error — the next governed brand's wave
				// is independent.
			}
			// Re-read the governed state (brand pointer) + ready count so the next
			// call picks the next governed brand and bails when the pool drains.
			fresh, err := po.refreshGovernedVerticalState(ctx, v.vertical)
			if err != nil || fresh == nil || fresh.readyCount <= 0 {
				break
			}
			v = *fresh
		}
	}
}

// governedStateKey returns the partner_drip_state rotation-pointer key for a
// vertical's governed pass. Distinct from the bare vertical (welcome pass) and
// 'followup' (follow-up pass) keys so the three rotations never collide.
func governedStateKey(vertical string) string {
	return strings.ToLower(strings.TrimSpace(vertical)) + ":governed"
}

// governedVerticalsWithBacklog is activeVerticalsWithBacklog restricted to the
// verticals that have ≥1 active governed subscriber (orderedSubscribedGoverned
// non-empty). It loads each vertical's READY backlog + dominant-dataset metadata
// exactly like the welcome enumerator, but reads the brand-rotation pointer from
// the '<vertical>:governed' state row (NOT the bare-vertical row), so the
// governed rotation is fully independent of the welcome rotation.
func (po *PartnerDripOrchestrator) governedVerticalsWithBacklog(ctx context.Context) ([]verticalState, error) {
	var out []verticalState
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		// Phase 1 — ready aggregate per BARE vertical (the shared pool the
		// governed pass claims), joined to the '<vertical>:governed' pointer.
		// LEFT JOIN so a vertical with ready records but no governed state row
		// yet still surfaces (brandIndex defaults 0; the row is auto-seeded by
		// refreshGovernedVerticalState / governedBrandIndex on first use).
		rows, err := tx.QueryContext(ctx, `
			SELECT agg.vertical, COALESCE(gs.next_brand_index, 0), agg.ready_total, agg.oldest_at
			FROM (
				SELECT vertical, COUNT(*) AS ready_total, MIN(ingested_at) AS oldest_at
				FROM partner_clean_queue
				WHERE status = 'ready'
				GROUP BY vertical
			) agg
			LEFT JOIN partner_drip_state gs ON gs.vertical = agg.vertical || ':governed'
			ORDER BY agg.vertical
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v verticalState
			var (
				readyTotal sql.NullInt64
				oldestAt   sql.NullTime
			)
			if err := rows.Scan(&v.vertical, &v.brandIndex, &readyTotal, &oldestAt); err != nil {
				continue
			}
			if !readyTotal.Valid || readyTotal.Int64 == 0 {
				continue
			}
			// Filter: only verticals with active governed subscribers.
			if len(po.governedBrandsSubscribedTo(v.vertical)) == 0 {
				continue
			}
			v.readyCount = int(readyTotal.Int64)
			v.oldestIngest = oldestAt
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Phase 2 — per-vertical dominant-dataset metadata (oldest ready row).
		// Identical to activeVerticalsWithBacklog Phase 2.
		for i := range out {
			var (
				datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
				flushHours                                       sql.NullInt64
				offerID                                          string
			)
			err := tx.QueryRowContext(ctx, `
				SELECT d.id, d.slug, p.slug, p.name, d.flush_window_hours,
				       COALESCE(d.offer_id::text, '')
				FROM (
					SELECT dataset_id
					FROM partner_clean_queue
					WHERE vertical = $1 AND status = 'ready'
					ORDER BY ingested_at ASC
					LIMIT 1
				) AS dom
				LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
				LEFT JOIN data_partners p ON p.id = d.partner_id
			`, out[i].vertical).Scan(&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			out[i].offerID = offerID
			if datasetID.Valid {
				out[i].datasetID = datasetID.String
			}
			if datasetSlug.Valid {
				out[i].datasetSlug = datasetSlug.String
			}
			if partnerSlug.Valid {
				out[i].partnerSlug = partnerSlug.String
			}
			if partnerName.Valid {
				out[i].partnerName = partnerName.String
			}
			if flushHours.Valid {
				out[i].flushHours = int(flushHours.Int64)
			}
			if out[i].flushHours <= 0 {
				out[i].flushHours = 24
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// refreshGovernedVerticalState re-reads a single vertical's READY backlog +
// dataset metadata + the '<vertical>:governed' brand pointer between intra-tick
// governed waves. Mirrors refreshVerticalState but reads the governed state row.
func (po *PartnerDripOrchestrator) refreshGovernedVerticalState(ctx context.Context, vertical string) (*verticalState, error) {
	var (
		v     verticalState
		found bool
	)
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		var (
			readyTotal sql.NullInt64
			oldestAt   sql.NullTime
		)
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(gs.next_brand_index, 0), agg.ready_total, agg.oldest_at
			FROM (
				SELECT COUNT(*) AS ready_total, MIN(ingested_at) AS oldest_at
				FROM partner_clean_queue
				WHERE status = 'ready' AND vertical = $1
			) agg
			LEFT JOIN partner_drip_state gs ON gs.vertical = $1 || ':governed'
		`, vertical).Scan(&v.brandIndex, &readyTotal, &oldestAt)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		v.vertical = vertical
		if readyTotal.Valid {
			v.readyCount = int(readyTotal.Int64)
		}
		v.oldestIngest = oldestAt

		var (
			datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
			flushHours                                       sql.NullInt64
			offerID                                          string
		)
		err = tx.QueryRowContext(ctx, `
			SELECT d.id, d.slug, p.slug, p.name, d.flush_window_hours,
			       COALESCE(d.offer_id::text, '')
			FROM (
				SELECT dataset_id
				FROM partner_clean_queue
				WHERE vertical = $1 AND status = 'ready'
				ORDER BY ingested_at ASC
				LIMIT 1
			) AS dom
			LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
			LEFT JOIN data_partners p ON p.id = d.partner_id
		`, vertical).Scan(&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		v.offerID = offerID
		if datasetID.Valid {
			v.datasetID = datasetID.String
		}
		if datasetSlug.Valid {
			v.datasetSlug = datasetSlug.String
		}
		if partnerSlug.Valid {
			v.partnerSlug = partnerSlug.String
		}
		if partnerName.Valid {
			v.partnerName = partnerName.String
		}
		if flushHours.Valid {
			v.flushHours = int(flushHours.Int64)
		}
		if v.flushHours <= 0 {
			v.flushHours = 24
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &v, nil
}

func (po *PartnerDripOrchestrator) pickNextFollowupBrand(ctx context.Context, vertical string, state *followupState) (string, error) {
	// DB-roster lane (partner_drip_vertical_roster): walk the lane's own roster
	// WITHOUT writing the shared dripBrands rotation index — a one-brand lane
	// must not reset the 16-brand rotation for every other vertical each tick.
	if roster, ok := dynamicRosterFor(vertical); ok {
		for offset := 0; offset < len(roster); offset++ {
			brand := roster[(state.brandIndex+offset)%len(roster)]
			if po.cfg.PausedBrandPredicate != nil && po.cfg.PausedBrandPredicate(ctx, brand) {
				continue
			}
			return brand, nil
		}
		return "", fmt.Errorf("all brands in the %s roster paused — no follow-up brand available", vertical)
	}
	for offset := 0; offset < len(dripBrands); offset++ {
		idx := (state.brandIndex + offset) % len(dripBrands)
		brand := dripBrands[idx]
		if po.cfg.PausedBrandPredicate != nil && po.cfg.PausedBrandPredicate(ctx, brand) {
			continue
		}
		state.brandIndex = idx
		return brand, nil
	}
	return "", fmt.Errorf("all brands paused — no follow-up brand available")
}

// followupVerticalsWithDueRecords returns the verticals that have at
// least one queue row whose next_touch_at has elapsed and isn't engaged
// or terminal yet. We pull dataset attribution from whichever queue row
// is oldest-due so the wave inherits dataset metadata for the analytics
// stamp.
func (po *PartnerDripOrchestrator) followupVerticalsWithDueRecords(ctx context.Context) ([]verticalState, error) {
	var out []verticalState
	// Gated verticals only surface as "having work" when the due rows are
	// ENGAGEMENT-ELIGIBLE, so a fully-cold internal lane stops being visited
	// every tick only to claim nothing.
	engGate := engagementGateAnyVerticalSQL("q")
	engagedExitAny := engagedExitAnyVerticalSQL("q")
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
		WITH due AS (
			SELECT q.vertical, q.dataset_id,
			       MIN(q.next_touch_at) AS oldest_due,
			       COUNT(*) FILTER (
			           WHERE q.status = 'mailed'
			             AND q.next_touch_at IS NOT NULL
			             AND q.next_touch_at <= NOW()`+engagedExitAny+`
			             AND q.terminal_reason IS NULL
			             AND q.touch_count BETWEEN 1 AND $1
			       ) AS due_total
			FROM partner_clean_queue q
			WHERE q.status = 'mailed'`+engagedExitAny+`
			  AND q.terminal_reason IS NULL
			  AND q.touch_count BETWEEN 1 AND $1`+engGate+`
			GROUP BY q.vertical, q.dataset_id
		)
		SELECT v.vertical,
		       (SELECT SUM(due_total) FROM due WHERE due.vertical = v.vertical) AS due_count,
		       d.id, d.slug, p.slug, p.name, d.flush_window_hours,
		       COALESCE(d.offer_id::text, '')
		FROM (SELECT DISTINCT vertical FROM due WHERE due_total > 0) v
		LEFT JOIN LATERAL (
			SELECT du.dataset_id
			FROM due du
			WHERE du.vertical = v.vertical AND due_total > 0
			ORDER BY oldest_due ASC NULLS LAST
			LIMIT 1
		) AS dom ON true
		LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
		LEFT JOIN data_partners p ON p.id = d.partner_id
		ORDER BY v.vertical
	`, MaxTouchCount-1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v verticalState
			var (
				datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
				flushHours                                       sql.NullInt64
				dueCount                                         sql.NullInt64
				offerID                                          string
			)
			if err := rows.Scan(&v.vertical, &dueCount,
				&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID); err != nil {
				continue
			}
			v.offerID = offerID
			if !dueCount.Valid || dueCount.Int64 == 0 {
				continue
			}
			v.readyCount = int(dueCount.Int64)
			if datasetID.Valid {
				v.datasetID = datasetID.String
			}
			if datasetSlug.Valid {
				v.datasetSlug = datasetSlug.String
			}
			if partnerSlug.Valid {
				v.partnerSlug = partnerSlug.String
			}
			if partnerName.Valid {
				v.partnerName = partnerName.String
			}
			if flushHours.Valid {
				v.flushHours = int(flushHours.Int64)
			}
			if v.flushHours <= 0 {
				v.flushHours = 24
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// processFollowup ships one follow-up wave for one (vertical, brand)
// pair. Records are claimed by ISP up to per-ISP caps, promoted to
// mailing_subscribers (idempotent — most are already there from the
// welcome wave), wrapped in a per-wave segment, and shipped via
// DeployFn. On success markMailed advances touch_count and stamps the
// next_touch_at clock.
func (po *PartnerDripOrchestrator) processFollowup(ctx context.Context, v verticalState, brand string) error {
	return po.processFollowupImpl(ctx, v, brand, false)
}

// processFollowupYahooNewsletter is the yahoo-confined companion to
// processFollowup: it claims ONLY yahoo-family follow-up records for the
// (vertical, brand) and serves them the brand NEWSLETTER instead of the offer
// creative, so the yahoo drip lane is newsletter-only at every touch while still
// advancing the ladder (operator 2026-07-14). Gated by cfg.YahooNewsletterOnlyDrip
// at the call site (tickFollowups); a no-op via that gate when the flag is off.
func (po *PartnerDripOrchestrator) processFollowupYahooNewsletter(ctx context.Context, v verticalState, brand string) error {
	return po.processFollowupImpl(ctx, v, brand, true)
}

// processFollowupImpl ships one follow-up wave for one (vertical, brand). In the
// default (offer) mode it serves the per-touch offer creative. In yahooNewsletter
// mode it confines the claim to the yahoo family and serves the brand newsletter.
func (po *PartnerDripOrchestrator) processFollowupImpl(ctx context.Context, v verticalState, brand string, yahooNewsletter bool) error {
	if MaxTouchCount-1 < 1 {
		return nil
	}
	hardCap := po.cfg.MaxFollowupClaimPerVertical
	if hardCap <= 0 || hardCap > po.cfg.MaxWaveSize {
		hardCap = po.cfg.MaxWaveSize
	}

	perISPCaps, err := po.resolvePerISPCaps(ctx, v.vertical, v.datasetID, ispCapBacklogFollowup)
	if err != nil {
		return fmt.Errorf("resolve_isp_caps: %w", err)
	}
	// ISP brand-ban on follow-ups (operator 2026-06-14 gmail; 2026-07-07 apple).
	// The follow-up brand rotation is independent of a record's original
	// mailed_brand, so without this a touch could ship under a brand banned
	// from that ISP (gmail -> mature-4; apple -> lpl/wfy HM08 ban). Run the
	// same full brand routing as the welcome path.
	perISPCaps = po.applyISPBrandRouting(brand, perISPCaps)
	// Apple-banned verticals (operator 2026-06-16): same Fidelity term-life → Apple ban as the
	// welcome path — follow-up touches must not ship term-life to Apple either.
	if appleBannedDripVerticals()[strings.ToLower(strings.TrimSpace(v.vertical))] {
		perISPCaps["apple"] = 0
	}
	perISPCaps = applyYahooNewsletterFollowupCaps(perISPCaps, po.cfg.YahooNewsletterOnlyDrip, yahooNewsletter)
	// Per-ISP DAILY cap on follow-ups (bug fix 2026-07-15): the welcome path
	// clamps first touches by NewRecordDailyISPCaps (PARTNER_DRIP_DAILY_ISP_CAPS)
	// via applyNewRecordDailyBudget, but the follow-up path historically skipped
	// it entirely — so a gmail=0 cap suppressed enrollment/touch-1 yet let t2..t5
	// keep shipping to already-enrolled gmail recipients (934 gmail follow-up
	// deliveries observed under gmail=0). Enforce the same daily ceiling here so a
	// cap of 0 for an ISP suppresses ALL touches to that ISP. Kill switch:
	// PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED=1 restores the legacy behavior.
	perISPCaps = po.applyFollowupDailyISPBudget(ctx, brand, v.datasetID, perISPCaps)
	claimed, err := po.claimFollowupRecordsByISPCaps(ctx, v.vertical, perISPCaps, hardCap)
	if err != nil {
		return fmt.Errorf("claim_followup: %w", err)
	}
	if len(claimed) == 0 {
		return nil
	}

	keep, deferred, deferralReasons, err := po.applyThroughputSafety(ctx, brand, claimed, perISPCaps)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] followup throughput_safety failed for vertical=%s: %v — proceeding without deferral", v.vertical, err)
		keep = claimed
	}
	if len(deferred) > 0 {
		if relErr := po.releaseClaim(ctx, deferred); relErr != nil {
			log.Printf("[PartnerDripOrchestrator] followup release deferred: %v", relErr)
		}
		log.Printf("[PartnerDripOrchestrator] followup deferred %d records for vertical=%s reasons=%v",
			len(deferred), v.vertical, deferralReasons)
	}
	if len(keep) == 0 {
		log.Printf("[PartnerDripOrchestrator] followup vertical=%s: all claimed records deferred — skipping wave", v.vertical)
		return nil
	}
	claimed = keep

	// Determine touch_number for the resolveFollowupCreative call. All
	// claimed records share the same target touch (touch_count + 1)
	// because the claim query partitions by touch_count and only takes
	// records whose current touch is the same.
	var touchNum int
	if err := po.db.QueryRowContext(ctx, `
		SELECT MAX(touch_count) + 1 FROM partner_clean_queue WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(claimedRecordIDs(claimed), ",")+"}").Scan(&touchNum); err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("compute_touch_number: %w", err)
	}
	if touchNum < 2 || touchNum > MaxTouchCount {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("invalid touch_number %d for vertical=%s", touchNum, v.vertical)
	}

	// Yahoo newsletter-only pass: serve the brand NEWSLETTER (the same creative the
	// welcome/touch-0 path ships via resolveCreative) instead of the per-touch offer.
	// resolveCreative is configured for every (vertical, brand) — a lookup miss is a
	// data gap, not ladder-complete, so we release (never retire) and let a later
	// wave under a configured brand serve it.
	if yahooNewsletter {
		creative, err := po.resolveCreative(ctx, v.vertical, brand)
		if err != nil {
			_ = po.releaseClaim(ctx, claimed)
			if errors.Is(err, sql.ErrNoRows) {
				log.Printf("[PartnerDripOrchestrator] followup-yahoo-nl vertical=%s brand=%s touch=%d has no newsletter for this brand — released", v.vertical, brand, touchNum)
				return nil
			}
			return fmt.Errorf("resolve_yahoo_newsletter_creative: %w", err)
		}
		subscriberIDs, err := po.promoteToSubscribers(ctx, v, claimed)
		if err != nil {
			_ = po.releaseClaim(ctx, claimed)
			return fmt.Errorf("promote_followup_subscribers: %w", err)
		}
		_, deployedCount := po.deployWaveGroups(ctx, v, brand, creative, claimed, subscriberIDs, fmt.Sprintf("[t%d]", touchNum))
		if deployedCount == 0 {
			return fmt.Errorf("all yahoo-nl followup wave groups failed to deploy for vertical=%s brand=%s", v.vertical, brand)
		}
		log.Printf("[PartnerDripOrchestrator] followup-yahoo-nl wave complete: vertical=%s brand=%s touch=%d deployed=%d creative=%s",
			v.vertical, brand, touchNum, deployedCount, creative.filename)
		return nil
	}

	creative, err := po.resolveFollowupCreative(ctx, v.vertical, brand, touchNum)
	if errors.Is(err, sql.ErrNoRows) {
		// No creative row for this (vertical, brand, touch). Two cases:
		//   1. The touch is unconfigured for the WHOLE vertical (no brand-specific
		//      row AND no vertical=NULL global) — the record has run past the end of
		//      this vertical's configured ladder (e.g. a 4-touch vertical under the
		//      new 5-touch ceiling). Retire it as terminal so it never loops.
		//   2. Some OTHER brand configures this touch but the shared follow-up brand
		//      rotation landed on a brand that doesn't — release the claim so a later
		//      wave under a configured brand serves it.
		if configured, cErr := po.followupTouchConfigured(ctx, v.vertical, touchNum); cErr == nil && !configured {
			if rErr := po.retireRecordsTerminal(ctx, claimed, "ladder_complete"); rErr != nil {
				log.Printf("[PartnerDripOrchestrator] followup retire-terminal failed (vertical=%s touch=%d): %v", v.vertical, touchNum, rErr)
				_ = po.releaseClaim(ctx, claimed)
			} else {
				log.Printf("[PartnerDripOrchestrator] followup vertical=%s touch=%d unconfigured — retired %d records as terminal (ladder_complete)", v.vertical, touchNum, len(claimed))
			}
			return nil
		}
		_ = po.releaseClaim(ctx, claimed)
		log.Printf("[PartnerDripOrchestrator] followup vertical=%s brand=%s touch=%d has no creative for this brand — released for a configured brand", v.vertical, brand, touchNum)
		return nil
	}
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("resolve_followup_creative: %w", err)
	}

	// Promote subscribers — idempotent because the ON CONFLICT branch in
	// promoteToSubscribers updates source_metadata + returns the
	// existing subscriber id.
	subscriberIDs, err := po.promoteToSubscribers(ctx, v, claimed)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("promote_followup_subscribers: %w", err)
	}

	// Split by SES routing (e.g. ht microsoft → HT SES tenant) and deploy each
	// group as its own follow-up campaign. The "[t%d]" suffix tags the touch so
	// analytics distinguishes welcome vs follow-up waves at a glance.
	_, deployedCount := po.deployWaveGroups(ctx, v, brand, creative, claimed, subscriberIDs, fmt.Sprintf("[t%d]", touchNum))
	if deployedCount == 0 {
		return fmt.Errorf("all followup wave groups failed to deploy for vertical=%s brand=%s", v.vertical, brand)
	}
	log.Printf("[PartnerDripOrchestrator] followup wave complete: vertical=%s brand=%s touch=%d deployed=%d creative=%s",
		v.vertical, brand, touchNum, deployedCount, creative.filename)
	return nil
}

func claimedRecordIDs(recs []claimedRecord) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	return ids
}

// claimFollowupRecordsByISPCaps claims oldest-due-first records from
// partner_clean_queue for a given vertical, partitioned by ISP and
// capped by perISPCaps. Records must be:
//
//   - status = 'mailed'
//   - touch_count BETWEEN 1 AND MaxTouchCount-1 (welcome shipped, < max)
//   - next_touch_at <= NOW() (gap window elapsed)
//   - engaged_at IS NULL (recipient hasn't engaged with prior touches)
//   - terminal_reason IS NULL (operator hasn't manually retired the row)
//
// Within a single wave we constrain claims to a single touch_count value
// so the resolveFollowupCreative step can pick the right family without
// running per-record. We pick whichever touch_count has the most
// outstanding due rows for this vertical at claim time — this keeps the
// rotation balanced between touch 2/3/4 in a steady-state queue.
//
// Returns the claimed records with status flipped to 'claimed' (so they
// won't be re-picked by a concurrent tick) and the wave_segment + deploy
// pipeline can carry on. On failure the caller releases via releaseClaim.
func (po *PartnerDripOrchestrator) claimFollowupRecordsByISPCaps(ctx context.Context, vertical string, perISPCaps map[string]int, hardCap int) ([]claimedRecord, error) {
	if len(perISPCaps) == 0 {
		return nil, fmt.Errorf("perISPCaps is empty")
	}
	if hardCap <= 0 {
		hardCap = po.cfg.MaxWaveSize
	}

	// Engagement gate (operator 2026-08-24): on gated verticals only openers and
	// clickers earn the next touch. Empty string on ungated verticals, so their
	// SQL is byte-identical to the pre-gate behavior. See
	// partner_drip_engagement_gate.go.
	engGate := engagementGateSQL(vertical, "")
	// Clicker continuation (operator 2026-08-25): on internal feeds a money-
	// clicker NEVER exits — engagedExit is empty there, the exit predicate
	// elsewhere. See engagedExitSQL.
	engagedExit := engagedExitSQL(vertical, "")

	// First pick the dominant touch_count for this vertical.
	// On engagement-gated verticals the count is taken over ELIGIBLE rows only,
	// so a lane whose largest pool has gone cold does not pin every wave on a
	// touch that can then claim nothing.
	//
	// TOUCH ROTATION (operator 2026-08-24, "the subsequent touches are performed
	// as well … the ladder should be automated, no manual intervention"). The
	// legacy pick was `ORDER BY COUNT(*) DESC LIMIT 1` — one touch per wave,
	// always the biggest pool. On any lane whose T1 inflow outruns its T2 drain
	// that pool NEVER changes hands, so touches 3+ are structurally unreachable:
	// measured 2026-08-24, internal_auto_insurance_v7 shipped ZERO t3 and ZERO
	// t4 for eight consecutive days with 11,580 records parked at t2, and
	// gmail_v1/v5 the same. Selecting by oldest-due instead only moves the lock
	// (v7 t3's oldest is 56h vs t2's 105h — t2 still wins for days).
	//
	// So we rotate: every eligible touch_count takes its turn, one per tick.
	// This does NOT dilute introductions — touch 1 ships from the separate
	// welcome pass (processVerticalWith) and never appears here; this rotation
	// only shares out the FOLLOW-UP waves the lane was already firing.
	//
	// Stateless on purpose. The index comes from the wall clock, so both service
	// instances agree without a shared pointer, there is no extra row to write
	// per wave, and a restart cannot lose the position.
	// Kill switch: PARTNER_DRIP_TOUCH_ROTATION_DISABLED=1 restores the
	// largest-pool pick exactly.
	var targetTouchCount int
	noRows := false
	if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT touch_count, COUNT(*) AS due
			FROM partner_clean_queue
			WHERE status = 'mailed'
			  AND vertical = $1
			  AND touch_count BETWEEN 1 AND $2
			  AND next_touch_at <= NOW()`+engagedExit+`
			  AND terminal_reason IS NULL`+datasetNotEmergencyPausedSQL+engGate+`
			GROUP BY touch_count
			ORDER BY touch_count ASC
		`, vertical, MaxTouchCount-1)
		if err != nil {
			return err
		}
		defer rows.Close()
		var eligible []touchPool
		for rows.Next() {
			var tp touchPool
			if err := rows.Scan(&tp.touchCount, &tp.due); err != nil {
				return err
			}
			eligible = append(eligible, tp)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(eligible) == 0 {
			noRows = true
			return nil
		}
		targetTouchCount = pickFollowupTouch(eligible, time.Now())
		return nil
	}); err != nil {
		return nil, fmt.Errorf("pick_dominant_touch: %w", err)
	}
	if noRows {
		return nil, nil
	}

	// Build VALUES list for caps CTE.
	args := []interface{}{vertical, hardCap, targetTouchCount}
	valueClauses := make([]string, 0, len(perISPCaps))
	idx := 4
	for ispName, capValue := range perISPCaps {
		if capValue <= 0 {
			continue
		}
		valueClauses = append(valueClauses, fmt.Sprintf("($%d::text, $%d::int)", idx, idx+1))
		args = append(args, ispName, capValue)
		idx += 2
	}
	if len(valueClauses) == 0 {
		return nil, fmt.Errorf("perISPCaps has no positive entries")
	}

	query := fmt.Sprintf(`
		WITH ranked AS (
			SELECT id, isp_family, next_touch_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY COALESCE(NULLIF(isp_family, ''), 'other')
			           ORDER BY next_touch_at ASC NULLS LAST
			       ) AS rn
			FROM partner_clean_queue
			WHERE status = 'mailed'
			  AND vertical = $1
			  AND touch_count = $3
			  AND next_touch_at <= NOW()`+engagedExit+`
			  AND terminal_reason IS NULL`+datasetNotEmergencyPausedSQL+engGate+`
		),
		caps(isp, cap) AS (
			VALUES %s
		),
		eligible AS (
			SELECT r.id
			FROM ranked r
			JOIN caps c ON c.isp = COALESCE(NULLIF(r.isp_family, ''), 'other')
			WHERE r.rn <= c.cap
			ORDER BY r.next_touch_at ASC NULLS LAST
			LIMIT $2
		),
		picked AS (
			SELECT id FROM partner_clean_queue
			WHERE id IN (SELECT id FROM eligible)
			  AND status = 'mailed'
			  AND touch_count = $3
			  AND next_touch_at <= NOW()`+engagedExit+`
			  AND terminal_reason IS NULL`+datasetNotEmergencyPausedSQL+engGate+`
			FOR UPDATE SKIP LOCKED
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW()
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, strings.Join(valueClauses, ", "))

	out := make([]claimedRecord, 0, hardCap)
	if err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r claimedRecord
			if err := rows.Scan(&r.id, &r.email, &r.emailMD5, &r.ispFamily, &r.datasetID, &r.partnerID, &r.batchID, &r.extra); err != nil {
				continue
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// resolveFollowupCreative looks up the follow-up creative for
// (vertical, brand, touch_number) in partner_drip_followup_creatives and
// reads the HTML body from disk. Vertical-specific rows win; rows with
// vertical IS NULL are the shared/global fallback chain (pre-2026-06-11
// behavior), so funnels without a bespoke ladder keep working unchanged.
// Errors out loud — the caller releases the claim if this fails so the
// records get retried on the next tick.
func (po *PartnerDripOrchestrator) resolveFollowupCreative(ctx context.Context, vertical, brand string, touchNumber int) (creativeRec, error) {
	var c creativeRec
	var offerID sql.NullString
	err := po.db.QueryRowContext(ctx, `
		SELECT creative_filename, subject_line, COALESCE(preheader, ''), from_name, offer_id
		FROM partner_drip_followup_creatives
		WHERE brand = $1 AND touch_number = $2 AND active = true
		  AND (vertical = $3 OR vertical IS NULL)
		ORDER BY (vertical = $3) DESC NULLS LAST
		LIMIT 1
	`, brand, touchNumber, vertical).Scan(&c.filename, &c.subject, &c.preheader, &c.fromName, &offerID)
	if err != nil {
		// sql.ErrNoRows is preserved via %w so processFollowup can distinguish an
		// unconfigured touch (ladder shorter than MaxTouchCount → retire terminal)
		// from a genuine lookup/read failure.
		return c, fmt.Errorf("followup_creative lookup (%s/%s/t%d): %w", vertical, brand, touchNumber, err)
	}
	if offerID.Valid {
		c.offerID = strings.TrimSpace(offerID.String)
	}
	// Per-touch offer WITHOUT a bespoke creative file → pull the creative content
	// from the offer-center tables for THIS touch's offer, preserving c.offerID for
	// the deploy's per-touch suppression scrub + attribution. A configured filename
	// takes precedence (disk read below). Empty offer_id keeps legacy behavior.
	if c.offerID != "" && strings.TrimSpace(c.filename) == "" {
		oc, err := po.resolveOfferCreative(ctx, c.offerID, brand)
		if err != nil {
			return c, err
		}
		oc.offerID = c.offerID
		return oc, nil
	}
	body, err := os.ReadFile(filepath.Join(po.cfg.CreativesDir, c.filename))
	if err != nil {
		return c, fmt.Errorf("read followup_creative %s: %w", c.filename, err)
	}
	c.htmlBody = string(body)
	return c, nil
}

// followupTouchConfigured reports whether ANY active follow-up creative row is
// configured for (vertical, touchNumber) — either a vertical-specific row or the
// shared vertical=NULL global fallback, for any brand. Used to decide whether a
// record that has advanced past the vertical's configured ladder should be
// retired as terminal (touch entirely unconfigured) versus merely re-queued for a
// brand that does configure it. Brand-agnostic on purpose: the follow-up brand
// rotation is independent of a record's origin, so "no row for THIS brand" alone
// must not retire records another brand could still serve.
func (po *PartnerDripOrchestrator) followupTouchConfigured(ctx context.Context, vertical string, touchNumber int) (bool, error) {
	var exists bool
	err := po.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM partner_drip_followup_creatives
			WHERE touch_number = $1 AND active = true
			  AND (vertical = $2 OR vertical IS NULL)
		)
	`, touchNumber, vertical).Scan(&exists)
	return exists, err
}

// retireRecordsTerminal permanently retires claimed records: it flips them back
// to status='mailed' (so mailed-count analytics stay intact), clears
// next_touch_at, and stamps terminal_reason (first-writer-wins). Retired rows are
// excluded from every future follow-up claim (which filters terminal_reason IS
// NULL), so this is the graceful terminal state for records that have exhausted a
// vertical's configured touch ladder.
func (po *PartnerDripOrchestrator) retireRecordsTerminal(ctx context.Context, recs []claimedRecord, reason string) error {
	if len(recs) == 0 {
		return nil
	}
	ids := claimedRecordIDs(recs)
	_, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'mailed',
		    next_touch_at = NULL,
		    terminal_reason = COALESCE(terminal_reason, $2)
		WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(ids, ",")+"}", reason)
	return err
}

func (po *PartnerDripOrchestrator) updateDripState(ctx context.Context, vertical string, nextIdx int, brand, campaignID string, waveSize int) error {
	_, err := po.db.ExecContext(ctx, `
		INSERT INTO partner_drip_state (vertical, next_brand_index, last_wave_at, last_wave_campaign_id, last_wave_brand, last_wave_size, updated_at)
		VALUES ($1, $2, NOW(), $3::uuid, $4, $5, NOW())
		ON CONFLICT (vertical) DO UPDATE SET
		    next_brand_index = EXCLUDED.next_brand_index,
		    last_wave_at = EXCLUDED.last_wave_at,
		    last_wave_campaign_id = EXCLUDED.last_wave_campaign_id,
		    last_wave_brand = EXCLUDED.last_wave_brand,
		    last_wave_size = EXCLUDED.last_wave_size,
		    updated_at = NOW()
	`, vertical, nextIdx, campaignID, brand, waveSize)
	return err
}

// safeIdent returns a slug suitable for use inside source_system labels.
// Only [a-z0-9_] characters retained, trims duplicate underscores.
func safeIdent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(s))
	last := byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			out = append(out, ch)
			last = ch
		default:
			if last != '_' {
				out = append(out, '_')
				last = '_'
			}
		}
	}
	return strings.Trim(string(out), "_")
}

// =========================================================================
// HandleDeployCampaign adapter
// =========================================================================
//
// WrapPMTACampaignDeploy turns the existing http.HandlerFunc-style HandleDeploy
// into a typed in-process call. We use httptest.NewRecorder + http.NewRequest
// so we don't have to refactor the existing handler. The handler reads the
// body as JSON, persists the campaign row, and queues the audience finalizer
// goroutine. The 202 response carries the campaign_id we extract here.

type deployHandlerSig func(http.ResponseWriter, *http.Request)

// ErrDeployNameReused reports that HandleDeployCampaign's by-(org,name)
// idempotency guard returned an EXISTING campaign rather than creating one.
// A wave group that sees this must NOT mark its records mailed — the returned
// campaign carries a different group's audience.
var ErrDeployNameReused = errors.New("deploy converged on an existing campaign by name (records NOT mailed)")

// WrapPMTACampaignDeploy adapts an HTTP handler into a typed CampaignDeployFn.
// The handler is invoked in-process via httptest, so no real network call.
func WrapPMTACampaignDeploy(h deployHandlerSig) CampaignDeployFn {
	return func(ctx context.Context, input engine.PMTACampaignInput) (string, error) {
		body, err := json.Marshal(input)
		if err != nil {
			return "", fmt.Errorf("marshal input: %w", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Caller", "partner_drip_orchestrator")
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code >= 400 {
			respBody, _ := io.ReadAll(rr.Body)
			return "", fmt.Errorf("HandleDeployCampaign returned %d: %s", rr.Code, string(respBody))
		}
		var out struct {
			CampaignID     string `json:"campaign_id"`
			ID             string `json:"id"`
			AlreadyExisted bool   `json:"already_existed"`
			Error          string `json:"error,omitempty"`
		}
		respBody, _ := io.ReadAll(rr.Body)
		if err := json.Unmarshal(respBody, &out); err != nil {
			return "", fmt.Errorf("unmarshal response: %w (raw=%s)", err, string(respBody))
		}
		if out.Error != "" {
			return "", fmt.Errorf("HandleDeployCampaign error: %s", out.Error)
		}
		// already_existed means the by-(org,name) idempotency guard converged
		// this deploy onto a PRE-EXISTING campaign instead of minting one. For
		// a partner-drip wave that is never correct: this group's segment was
		// not the returned campaign's audience, so marking the group mailed
		// would terminally burn records that no campaign will ever send (the
		// 2026-08-11 internal_auto_insurance loss — 349 records in one wave,
		// ~1,118 in a day). Surface it as an error so deployWaveGroups takes
		// the releaseClaim path and the next tick retries the records under a
		// freshly-minted, now-unique name.
		if out.AlreadyExisted {
			id := out.CampaignID
			if id == "" {
				id = out.ID
			}
			return "", fmt.Errorf("%w (converged on campaign %s)", ErrDeployNameReused, id)
		}
		if out.CampaignID != "" {
			return out.CampaignID, nil
		}
		if out.ID != "" {
			return out.ID, nil
		}
		return "", fmt.Errorf("HandleDeployCampaign returned no campaign_id (status=%d)", rr.Code)
	}
}
