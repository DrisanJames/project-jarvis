package tracking

// /track/* token signature verification — SHADOW by default (2026-09-11).
//
// The public tracking service decoded /track/{open,click,unsubscribe}/{data}/{sig}
// but never checked {sig}, and /track/click 307s to whatever URL sits in the
// token. This file adds the check behind a mode switch so real traffic can be
// MEASURED before anything is blocked:
//
//	TRACKING_SIG_MODE = off | shadow | enforce   (unset/unknown = shadow)
//	  off      no verification, no counters.
//	  shadow   verify + count; the response is NEVER changed.
//	  enforce  /track/click with a bad or absent signature -> neutral 400
//	           ("bad link", same as a malformed token), no event, no redirect.
//	           open/unsubscribe stay count-only in every mode (see below).
//
// Key: TRACKING_SECRET — the SAME env var every live generator signs with
// (cmd/server/main.go SetTrackingConfig, internal/api/mailing_handlers_full.go,
// internal/api/offer_proof_send.go). Comma-separated for rotation ("new,old").
// The unsplit raw value is always tried too, so a key that itself contains a
// comma still verifies.
//
// Signed-bytes formats (every generator of /track/* links in the repo):
//
//	enc  hex(HMAC-SHA256(key, base64url(raw)))[:16]
//	     worker.TrackSign(encoded, …) — RewriteClickLinks, InjectTrackingPixelAndLinks,
//	     InjectOpenPixel, generateUnsubURL (send worker, SES relay, click-drip,
//	     proof sends) and api.injectTrackingWithURL (campaign-builder sync send).
//	raw  hex(HMAC-SHA256(key, raw))[:16]
//	     mailing.TrackingService.sign (no live caller) and the API transactional
//	     open pixel (internal/api/mailing_sending.go). Accepted on /track/open
//	     ONLY — no live generator signs a click or unsubscribe this way.
//
// "raw" is the pipe-joined org|campaign|subscriber|email[|url] string BEFORE
// base64; for clicks the url is the href attribute value verbatim (HTML
// entities such as &amp; are NOT decoded before signing).
//
// Open/unsubscribe are never blocked here: an unsubscribe must never fail
// (RFC 8058 compliance) and an open is telemetry with no redirect. Blocking
// forged ones is a separate, later decision informed by these counters.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

const (
	// SigModeEnv selects off | shadow | enforce. Read per request.
	SigModeEnv = "TRACKING_SIG_MODE"
	// SigKeyEnv is the signing key env var shared with the generators.
	SigKeyEnv = "TRACKING_SECRET"

	sigSummaryEvery = 5 * time.Minute
	sigHexLen       = 16
)

type sigMode int

const (
	sigModeShadow sigMode = iota
	sigModeOff
	sigModeEnforce
)

func (m sigMode) String() string {
	switch m {
	case sigModeOff:
		return "off"
	case sigModeEnforce:
		return "enforce"
	default:
		return "shadow"
	}
}

func currentSigMode() sigMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SigModeEnv))) {
	case "off":
		return sigModeOff
	case "enforce":
		return sigModeEnforce
	default:
		return sigModeShadow
	}
}

// Signature result classes.
const (
	SigValid       = "valid"
	SigInvalid     = "invalid"
	SigNoSig       = "no_sig"
	SigMissingKey  = "missing_key"
	SigUndecodable = "undecodable"
)

// Signed-bytes formats.
const (
	SigFormatEncoded = "enc"
	SigFormatRaw     = "raw"
)

// SigResult is the outcome of verifying one token.
type SigResult struct {
	Class  string // SigValid | SigInvalid | SigNoSig | SigMissingKey | SigUndecodable
	Format string // SigFormatEncoded | SigFormatRaw when Class == SigValid, else ""
}

// ParseSigKeys turns the env value into candidate keys: the raw value exactly
// as set (what the server signs with — it does not split), then each
// comma-separated, trimmed part. Duplicates and empties dropped.
func ParseSigKeys(v string) [][]byte {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out [][]byte
	add := func(k string) {
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, []byte(k))
	}
	add(v)
	for _, k := range strings.Split(v, ",") {
		add(strings.TrimSpace(k))
	}
	return out
}

func loadSigKeysFromEnv() [][]byte { return ParseSigKeys(os.Getenv(SigKeyEnv)) }

// Accepted formats per endpoint — only formats a LIVE generator emits:
//   - click/unsub: enc only. The only raw click generator is
//     mailing.TrackingService (referenced solely from a commented-out stub);
//     measured 2026-09-11 on 11,413 real lake click links: 100% enc, 0% raw.
//   - open: enc + raw. raw is live for the API transactional open pixel
//     (internal/api/mailing_sending.go pixelData HMAC before base64).
var (
	sigFormatsEncOnly = []string{SigFormatEncoded}
	sigFormatsOpen    = []string{SigFormatEncoded, SigFormatRaw}
)

// SignTrackingMsg is hex(HMAC-SHA256(key, msg))[:16] — the one algorithm every
// generator uses; only the signed bytes differ by format.
func SignTrackingMsg(key, msg []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return hex.EncodeToString(mac.Sum(nil))[:sigHexLen]
}

// VerifyTrackingToken checks sig against every key × each requested format.
// encoded is the {data} path segment EXACTLY as received — no re-encoding and
// no padding normalization (the enc format signs those bytes).
func VerifyTrackingToken(encoded, sig string, keys [][]byte, formats []string) SigResult {
	if sig == "" {
		return SigResult{Class: SigNoSig}
	}
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return SigResult{Class: SigUndecodable}
	}
	if len(keys) == 0 {
		return SigResult{Class: SigMissingKey}
	}
	if len(sig) != sigHexLen {
		return SigResult{Class: SigInvalid}
	}
	got := []byte(sig)
	for _, k := range keys {
		for _, f := range formats {
			msg := []byte(encoded)
			if f == SigFormatRaw {
				msg = raw
			}
			if hmac.Equal([]byte(SignTrackingMsg(k, msg)), got) {
				return SigResult{Class: SigValid, Format: f}
			}
		}
	}
	return SigResult{Class: SigInvalid}
}

// blocksInEnforce: only a present-but-wrong or absent signature blocks.
// missing_key fails OPEN (a key-less task must not black-hole every click);
// undecodable is already rejected by the pre-existing bad-link branch.
func (r SigResult) blocksInEnforce() bool {
	return r.Class == SigInvalid || r.Class == SigNoSig
}

// tokenHashPrefix is the ONLY token-derived value ever logged.
func tokenHashPrefix(encoded string) string {
	s := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(s[:4])
}

// -----------------------------------------------------------------------------
// Destination check (shadow-only, all modes except off): is the /track/click
// redirect target an owned domain or a registered money host?
// -----------------------------------------------------------------------------

const (
	DestOwned       = "owned"
	DestMoney       = "money"
	DestUnknown     = "unknown"
	DestBadScheme   = "bad_scheme"
	DestUnparseable = "unparseable"
)

// platformOwnedHosts are platform hosts that are not brand roots.
var platformOwnedHosts = []string{"projectjarvis.io"}

func normHost(h string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "www.")
}

func hostUnder(host string, roots []string) bool {
	for _, r := range roots {
		r = normHost(r)
		if r != "" && (host == r || strings.HasSuffix(host, "."+r)) {
			return true
		}
	}
	return false
}

// deadLinkRemapHosts are the remap TARGET hosts — hosts we redirect to by our
// own code, so by definition registered.
func deadLinkRemapHosts() []string {
	out := make([]string, 0, len(deadLinkRemap))
	for _, m := range deadLinkRemap {
		if u, err := url.Parse(m.to); err == nil {
			out = append(out, u.Hostname())
		}
	}
	return out
}

// classifyDestination: owned = brand.Domains() (compile-time OwnedDomains ∪
// mailing_owned_domains when the refresher is wired) or a platform host;
// money = a host of an active mailing_smart_links.offer_url_template (the
// in-memory dictionary) or a dead-link remap target; else unknown.
func classifyDestination(target string, dict *SmartLinkDictionary) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return DestUnparseable
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return DestBadScheme
	}
	host := normHost(u.Hostname())
	if hostUnder(host, brand.Domains()) || hostUnder(host, platformOwnedHosts) {
		return DestOwned
	}
	if dict.HasHost(host) || hostUnder(host, deadLinkRemapHosts()) {
		return DestMoney
	}
	return DestUnknown
}

// -----------------------------------------------------------------------------
// Counters: cumulative (served on /health) + a 5-minute window ("TRACKSIG
// SUMMARY" log line). Keys never carry request content.
// -----------------------------------------------------------------------------

type sigStats struct {
	mu     sync.Mutex
	since  time.Time
	total  map[string]int64
	window map[string]int64
}

func newSigStats() *sigStats {
	return &sigStats{since: time.Now(), total: map[string]int64{}, window: map[string]int64{}}
}

var sigCounters = newSigStats()

func (s *sigStats) inc(key string) {
	s.mu.Lock()
	s.total[key]++
	s.window[key]++
	s.mu.Unlock()
}

func (s *sigStats) snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.total))
	for k, v := range s.total {
		out[k] = v
	}
	return out
}

func (s *sigStats) flush(window time.Duration, mode sigMode, nkeys int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := make([]string, 0, len(s.window))
	var total int64
	for k, v := range s.window {
		parts = append(parts, k+"="+strconv.FormatInt(v, 10))
		if strings.Contains(k, ".sig.") {
			total += v
		}
	}
	sort.Strings(parts)
	line := "TRACKSIG SUMMARY window=" + window.String() + " since=" + s.since.UTC().Format(time.RFC3339) +
		" mode=" + mode.String() + " keys=" + strconv.Itoa(nkeys) + " checked=" + strconv.FormatInt(total, 10) +
		" counts[" + strings.Join(parts, " ") + "]"
	s.since = time.Now()
	s.window = map[string]int64{}
	return line
}

// checkSig verifies and counts one request. endpoint is click|open|unsub.
func (h *Handler) checkSig(endpoint, encoded, sig string) SigResult {
	formats := sigFormatsEncOnly
	if endpoint == "open" {
		formats = sigFormatsOpen
	}
	res := VerifyTrackingToken(encoded, sig, h.sigKeys, formats)
	sigCounters.inc(endpoint + ".sig." + res.Class)
	if res.Format != "" {
		sigCounters.inc(endpoint + ".fmt." + res.Format)
	}
	return res
}

// SigKeyCount is the number of candidate keys loaded (never the keys).
func (h *Handler) SigKeyCount() int { return len(h.sigKeys) }

// StartSigSummary logs one TRACKSIG SUMMARY line every 5 minutes until ctx ends.
func (h *Handler) StartSigSummary(ctx context.Context) {
	go func() {
		t := time.NewTicker(sigSummaryEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Print(sigCounters.flush(sigSummaryEvery, currentSigMode(), len(h.sigKeys)))
			}
		}
	}()
}
