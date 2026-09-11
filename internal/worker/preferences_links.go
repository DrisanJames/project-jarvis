package worker

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

// PreferenceGate is the call-site entry of *preferences.Hub. Gate returns
// skip=true only under PREFERENCES_MODE=enforce; in shadow it only counts.
type PreferenceGate interface {
	Gate(site, email, brandRoot, topic string, now time.Time) (skip bool, reason string)
}

// GeneratePreferencesURL builds the recipient preference-page link on the
// sending profile's tracking host:
//
//	{trackBase}/track/preferences?t=<v1 token>
//
// /track/* is the one path family the ALB forwards to the tracking service on
// EVERY host (a bare /preferences on a trk./t.em. host is 403'd at the edge —
// verified 2026-09-11), and cmd/tracking redirects /track/preferences to the
// API page. Keeping the link on the brand's own host avoids stamping the
// platform domain into every brand's mail. brandRoot "" mints an all-brands
// token. With no secret the link carries no token and lands on the neutral page.
func GeneratePreferencesURL(orgID, subscriberID, brandRoot, trackBase, secret string, now time.Time) string {
	scope := preferences.ScopeBrand
	if strings.TrimSpace(brandRoot) == "" {
		scope = preferences.ScopeAll
	}
	u := strings.TrimRight(trackBase, "/") + "/track/preferences"
	if tok := preferences.Mint(orgID, subscriberID, brandRoot, scope, now, secret); tok != "" {
		return u + "?t=" + tok
	}
	return u
}

// clickDripSuppressionCheckDisabled is the kill switch for the click-drip
// send-time suppression check (compliance fix 2026-09-11). Default: check ON.
func clickDripSuppressionCheckDisabled() bool {
	return os.Getenv("DISABLE_CLICKDRIP_SUPPRESSION_CHECK") == "true"
}

var clickDripSendTimeCounters struct {
	skippedGlobal, skippedBrand, skippedPreference, deferredUnattached atomic.Int64
}

// ClickDripSuppressionStatus is the /health "clickdrip_suppression" block.
func ClickDripSuppressionStatus() map[string]interface{} {
	c := &clickDripSendTimeCounters
	return map[string]interface{}{
		"check_enabled":       !clickDripSuppressionCheckDisabled(),
		"skipped_global":      c.skippedGlobal.Load(),
		"skipped_brand":       c.skippedBrand.Load(),
		"skipped_preference":  c.skippedPreference.Load(),
		"deferred_unattached": c.deferredUnattached.Load(),
	}
}

// SetSuppressionSource wires the per-send global suppression hub lookup. In
// production this is SendWorkerPool.GlobalSuppressionHub, so the click-drip
// path sees the hub the moment the pool's late-pass wiring attaches it.
func (s *JourneyClickDripSender) SetSuppressionSource(f func() GlobalSuppressionChecker) {
	s.suppressionSource = f
}

// SetPreferencesGate wires the subscriber-preference hub.
func (s *JourneyClickDripSender) SetPreferencesGate(g PreferenceGate) {
	s.prefsGate = g
}

// sendTimeSkip decides whether this touch must not go out.
//
//   - suppressed globally or for the sending brand -> skip (nil error)
//   - source configured but hub not attached yet (boot window) -> TRANSIENT
//     error: journey retry backs off 5m instead of mailing unchecked. The
//     message avoids the word "suppressed" (a terminal marker that would
//     eject the enrollment).
//   - no source configured at all (legacy construction) -> logged once,
//     cannot check.
//   - preference gate (shadow by default) -> skip only when enforcing.
func (s *JourneyClickDripSender) sendTimeSkip(p ClickDripSendParams) (bool, error) {
	brandRoot := brand.RootFromEmail(p.FromEmail)
	if !clickDripSuppressionCheckDisabled() {
		switch {
		case s.suppressionSource == nil:
			s.unconfiguredWarned.Do(func() {
				log.Printf("ERROR: JourneyClickDripSender: no suppression source configured — click-drip sends are NOT suppression-checked")
			})
		default:
			hub := s.suppressionSource()
			if hub == nil {
				n := clickDripSendTimeCounters.deferredUnattached.Add(1)
				return false, fmt.Errorf("click-drip sender: global suppression hub not attached yet (boot) — deferring send (deferred_total=%d)", n)
			}
			if hub.IsSuppressed(p.SubscriberEmail) {
				n := clickDripSendTimeCounters.skippedGlobal.Add(1)
				log.Printf("JourneyClickDripSender: SKIP global suppression step=%d offer=%s to %s (skipped_global_total=%d)",
					p.ReminderSeq, p.EverflowOfferID, logger.RedactEmail(p.SubscriberEmail), n)
				return true, nil
			}
			if hub.IsSuppressedForBrand(p.SubscriberEmail, brandRoot) {
				n := clickDripSendTimeCounters.skippedBrand.Add(1)
				log.Printf("JourneyClickDripSender: SKIP brand suppression brand=%s step=%d offer=%s to %s (skipped_brand_total=%d)",
					brandRoot, p.ReminderSeq, p.EverflowOfferID, logger.RedactEmail(p.SubscriberEmail), n)
				return true, nil
			}
		}
	}
	if s.prefsGate != nil {
		if skip, reason := s.prefsGate.Gate("clickdrip", p.SubscriberEmail, brandRoot, "", time.Now()); skip {
			n := clickDripSendTimeCounters.skippedPreference.Add(1)
			log.Printf("JourneyClickDripSender: SKIP preference=%s step=%d offer=%s to %s (skipped_preference_total=%d)",
				reason, p.ReminderSeq, p.EverflowOfferID, logger.RedactEmail(p.SubscriberEmail), n)
			return true, nil
		}
	}
	return false, nil
}
