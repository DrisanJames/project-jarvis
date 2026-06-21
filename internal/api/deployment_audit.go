package api

// Per-deploy audit write. Records one immutable row per PMTA wave campaign at
// deploy time: the html_sha256 actually shipped, the money links it carried
// (url+slug), the sending domain, and the planned audience total. This is a
// forensic breadcrumb for incidents like the jun21 dead-link salvage — given a
// campaign you can prove exactly which creative bytes / money slugs went out.
//
// ⚠️ FAIL-SAFE CONTRACT (non-negotiable):
//   This helper is called AFTER the deploy transaction has committed, with the
//   committed *sql.DB handle (never the tx). It returns nothing, gates nothing,
//   and CANNOT break a deploy. Its entire body is wrapped in a defer/recover and
//   every error (nil db, marshal, DB) is logged and swallowed. The INSERT is
//   ON CONFLICT (campaign_id) DO NOTHING so a re-run (e.g. a retried finalize)
//   is idempotent. Nothing the caller does with this function's (absent) outcome
//   may alter deploy success.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
)

// moneyLinkAudit is one extracted money link stored in the money_links JSONB array.
type moneyLinkAudit struct {
	URL  string `json:"url"`
	Slug string `json:"slug"`
}

// writeDeploymentAudit best-effort records one mailing_pmta_deployment_audits row.
// It NEVER returns an error and NEVER panics out to the caller — any failure is
// recovered + logged + swallowed. Safe to call with a nil db (no-ops). Call it
// AFTER the deploy tx commits, passing the committed *sql.DB (not the tx).
func writeDeploymentAudit(ctx context.Context, db *sql.DB, orgID, campaignID, offerKey, html, sendingDomain string, totalRecipients int) {
	// Outermost guard: anything below — including a nil-map/marshal/driver panic —
	// is caught here and turned into a log line. Cannot crash the finalizer goroutine.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[deploy-audit] recovered panic for campaign %s: %v", campaignID, r)
		}
	}()

	if db == nil {
		log.Printf("[deploy-audit] skip campaign %s: nil db handle", campaignID)
		return
	}

	sum := sha256.Sum256([]byte(html))
	htmlSHA := hex.EncodeToString(sum[:])

	links := make([]moneyLinkAudit, 0)
	for _, u := range extractMoneyHrefs(html) {
		links = append(links, moneyLinkAudit{URL: u, Slug: slugFromMoneyURL(u)})
	}
	moneyJSON, err := json.Marshal(links)
	if err != nil {
		log.Printf("[deploy-audit] marshal money links failed for %s: %v", campaignID, err)
		moneyJSON = []byte("[]")
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO mailing_pmta_deployment_audits
			(campaign_id, organization_id, offer_key, html_sha256, money_links,
			 sending_domain, total_recipients, deployed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (campaign_id) DO NOTHING`,
		campaignID, orgID, offerKey, htmlSHA, moneyJSON, sendingDomain, totalRecipients,
	); err != nil {
		log.Printf("[deploy-audit] insert failed for campaign %s (swallowed): %v", campaignID, err)
		return
	}
}
