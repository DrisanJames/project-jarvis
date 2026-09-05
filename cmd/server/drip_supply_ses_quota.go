package main

// drip_supply_ses_quota.go — the SES half of the REQ-118 governor stack (D3).
//
// dripsupply.SESQuotaGovernor takes a plain function, not an *ses.Client, so
// the dripsupply package carries no AWS dependency (bucket.go:453). This is
// that function, and the only place in the tree that knows the drip
// reservation path is quota-bound.
//
// Doctrine: an SES 454 is CAPACITY, not deliverability (JAOS core §5). The
// governor reduces capacity and must never colour an ISP health band — and a
// read failure yields NO ceiling (never a zero), because failing closed on an
// AWS blip would stop the estate.

import (
	"context"
	"errors"
	"log"
	"sync"

	appconfig "github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// errNoSESSendQuota is returned when GetAccount succeeds but carries no
// SendQuota — "unknown", which must read as no ceiling, not as zero quota.
var errNoSESSendQuota = errors.New("ses: GetAccount returned no SendQuota")

// sesAccountQuotaReader returns the account's 24h quota reader, or nil when SES
// is not configured. nil is a supported input: NewSESQuotaGovernor(nil) is
// permanently inert and logs once, so a box without SES credentials boots with
// the same stack minus one ceiling instead of failing.
//
// The client is built LAZILY and once. Building it eagerly here would run
// inside the partner-drip starter closure, whose whole contract is "cheap to
// register"; and a client built at boot on a box with no credentials would log
// a scary error for a governor that will never be asked anything.
func sesAccountQuotaReader(ctx context.Context, cfg appconfig.SESConfig) dripsupply.SESQuotaFunc {
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		log.Printf("[DripSupply] SES credentials absent — governor %q will be inert (no quota ceiling on the drip reservation path)", "ses_quota")
		return nil
	}
	var (
		once    sync.Once
		client  *ses.Client
		initErr error
	)
	return func(callCtx context.Context) (float64, float64, error) {
		once.Do(func() { client, initErr = ses.NewClient(ctx, cfg) })
		if initErr != nil {
			return 0, 0, initErr
		}
		out, err := client.GetAccountStatistics(callCtx)
		if err != nil {
			return 0, 0, err
		}
		// A missing SendQuota must be an ERROR, never (0, 0). The governor
		// computes remaining = floor(max − sent) and treats 0 as a genuine
		// STOP (bucket.go:580), so returning zeros for "I could not read it"
		// would zero every SES-routed lane estate-wide. An error yields NO
		// ceiling and bumps ErrorCount(), which is the visible failure mode.
		if out == nil || out.SendQuota == nil {
			return 0, 0, errNoSESSendQuota
		}
		return out.SendQuota.Max24HourSend, out.SendQuota.SentLast24Hours, nil
	}
}
