package api

// Durable per-conversion revenue ledger (REQ-037, 2026-07-13).
//
// Before this file, an Everflow conversion survived in PG only as a
// mailing_offer_suppressions row (reason='converted', no payout) plus counter
// bumps into mailing_offer_deployments whose join never matches — so payout
// dollars existed nowhere per-conversion, blank-sub1 conversions were dropped
// entirely, and per-dataset revenue was uncomputable (findings G2 §3.3, G3 §4).
//
// Two ingest paths write mailing_everflow_conversions (DDL in
// cmd/server/main.go runStartupMigrations, "create_everflow_conversions"):
//
//  1. POSTBACK enrichment — recordEverflowConversion() is called by both
//     Everflow entry points (HandlePostback and the click endpoint's
//     conversion branch) BEFORE any skip logic, so blank-sub1 conversions
//     persist too. Deduped on transaction_id among postback-shaped rows
//     (conversion_id='') via a partial unique index.
//
//  2. EXPORT pull — POST /api/admin/everflow-conversions-sync pulls the
//     Everflow conversions report (internal/everflow client, the same
//     X-Eflow-API-Key auth the creatives integration uses) and upserts
//     source='export' rows keyed by conversion_id. A postback row with the
//     same transaction_id is ENRICHED in place (gains conversion_id, payout,
//     status) rather than duplicated.
//
// sub1/sub2/sub3 are stored RAW: sub2 semantics conflict between handlers
// (creative UUID vs brand domain — G2 §3.3), so no parse is trusted here.
// Per-dataset revenue is a read-time join:
//
//	SELECT q.dataset_id, COUNT(*), SUM(c.payout)
//	FROM mailing_everflow_conversions c
//	JOIN partner_clean_queue q ON q.subscriber_id = c.subscriber_id
//	GROUP BY 1;

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// insertEverflowConversionSQL persists one postback-shaped conversion row.
// The ON CONFLICT arbiter is the partial unique index
// uniq_everflow_conversions_txn (transaction_id WHERE transaction_id <> ''
// AND conversion_id = ''), so a re-fired postback is a no-op while an export
// row for the same transaction (which carries a conversion_id) is unaffected.
const insertEverflowConversionSQL = `
	INSERT INTO mailing_everflow_conversions
		(id, organization_id, transaction_id, everflow_offer_id, subscriber_id, campaign_id,
		 sub1, sub2, sub3, payout, source, converted_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'postback', NOW())
	ON CONFLICT (transaction_id) WHERE transaction_id <> '' AND conversion_id = '' DO NOTHING`

// recordEverflowConversion writes the durable per-conversion row for a live
// postback. Best-effort by contract: both callers always 200 to Everflow, so
// this logs and never propagates. subscriberID/campaignID may be uuid.Nil
// (stored as NULL) — blank-sub1 conversions are kept, not dropped; they remain
// joinable later by transaction_id via the export pull.
//
// Returns TRUE only when this call actually inserted a new row. Everflow
// RETRIES postbacks, so the same conversion arrives at this endpoint several
// times; the ON CONFLICT above absorbs the repeats. Callers use the return
// value to gate side effects that must fire ONCE PER CONVERSION rather than
// once per delivery — notably the Slack alert, which on 2026-07-31 announced a
// single $50 conversion (txn 42ba0910) five times and made the day's revenue
// look like $400 instead of $200.
//
// A false return means "already recorded", NOT "failed" — errors also return
// false, and are logged. That is the safe direction for an alert gate: a
// missed alert is recoverable from the durable row, a phantom one is not.
func recordEverflowConversion(ctx context.Context, db *sql.DB, orgID string,
	txnID, efOfferID string, subscriberID, campaignID uuid.UUID,
	sub1, sub2, sub3 string, payout float64) bool {
	var subArg, campArg interface{}
	if subscriberID != uuid.Nil {
		subArg = subscriberID
	}
	if campaignID != uuid.Nil {
		campArg = campaignID
	}
	res, err := db.ExecContext(ctx, insertEverflowConversionSQL,
		uuid.New(), orgID, strings.TrimSpace(txnID), strings.TrimSpace(efOfferID),
		subArg, campArg, sub1, sub2, sub3, payout)
	if err != nil {
		log.Printf("[EverflowConversions] ERROR inserting durable conversion row (txn=%s offer=%s): %v",
			txnID, efOfferID, err)
		return false
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Driver could not report the count — treat as "already recorded" so a
		// retry storm cannot fan out into repeat alerts.
		log.Printf("[EverflowConversions] RowsAffected unavailable (txn=%s): %v", txnID, err)
		return false
	}
	if n == 0 {
		log.Printf("[EverflowConversions] duplicate postback absorbed (txn=%s offer=%s) — no new row, no alert",
			txnID, efOfferID)
		return false
	}
	return true
}

// enrichEverflowConversionSQL upgrades a postback row (conversion_id='') with
// the export's authoritative fields. Once conversion_id is set the row leaves
// the txn-dedupe index's domain, so a second export event on the same
// transaction inserts its own row keyed by its own conversion_id.
const enrichEverflowConversionSQL = `
	UPDATE mailing_everflow_conversions
	SET conversion_id = $1,
	    payout        = $2,
	    status        = $3,
	    event_name    = $4,
	    source        = 'export',
	    updated_at    = NOW()
	WHERE transaction_id = $5 AND transaction_id <> '' AND conversion_id = ''`

// upsertEverflowConversionSQL inserts an export row, updating payout/status on
// re-pull (Everflow statuses move pending→approved/rejected; payout can be
// adjusted after the fact).
const upsertEverflowConversionSQL = `
	INSERT INTO mailing_everflow_conversions
		(id, organization_id, conversion_id, transaction_id, everflow_offer_id,
		 subscriber_id, campaign_id, sub1, sub2, sub3, payout, status, event_name,
		 source, converted_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'export', $14)
	ON CONFLICT (conversion_id) WHERE conversion_id <> ''
	DO UPDATE SET payout = EXCLUDED.payout, status = EXCLUDED.status,
	              event_name = EXCLUDED.event_name, updated_at = NOW()`

// EverflowSyncStats reports one export-pull's outcome.
type EverflowSyncStats struct {
	Fetched  int
	Enriched int
	Upserted int
	Skipped  int
	Failed   int
}

// RunEverflowConversionsSync pulls the Everflow conversions report for a date
// range and upserts durable rows — the core shared by the admin endpoint and
// the boot-started scheduler (Empire flooring diagnosis F5, 2026-08-06: the
// endpoint existed but nothing called it, so any conversion that missed the
// live postback never landed and lane governors optimized on conversions=0).
// Idempotent by construction: enrich-then-upsert keyed on conversion_id /
// transaction_id, safe on double-fire.
func RunEverflowConversionsSync(ctx context.Context, db *sql.DB, from, to time.Time) (EverflowSyncStats, error) {
	// Same key resolution as the Everflow creatives integration
	// (server_routes_mailing.go RegisterEverflowCreativeRoutes call site).
	apiKey := os.Getenv("EVERFLOW_API_KEY")
	if apiKey == "" {
		apiKey = "Pn9S4t76TWezyTJ5iwtQbQ" // Default from config
	}
	baseURL := os.Getenv("EVERFLOW_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.eflow.team"
	}
	client := everflow.NewClient(everflow.Config{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		TimezoneID: 90,
		CurrencyID: "USD",
	})

	var stats EverflowSyncStats
	records, err := client.GetConversions(ctx,
		from.Format("2006-01-02"), to.Format("2006-01-02"), nil, false)
	if err != nil {
		return stats, err
	}
	stats.Fetched = len(records)

	orgID := "00000000-0000-0000-0000-000000000001"
	for _, rec := range records {
		convID := strings.TrimSpace(rec.ConversionID)
		txnID := strings.TrimSpace(rec.TransactionID)
		if convID == "" && txnID == "" {
			stats.Skipped++
			continue
		}

		// Enrich an existing postback row for this transaction first —
		// it predates the export row and must not be double-counted.
		if convID != "" && txnID != "" {
			res, uErr := db.ExecContext(ctx, enrichEverflowConversionSQL,
				convID, rec.Payout, rec.Status, rec.EventName, txnID)
			if uErr != nil {
				log.Printf("[EverflowConversionsSync] ERROR enriching txn=%s: %v", txnID, uErr)
				stats.Failed++
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.Enriched++
				continue
			}
		}

		subscriberID, _ := uuid.Parse(strings.TrimSpace(rec.Sub1))
		campaignID, _ := uuid.Parse(strings.TrimSpace(rec.Sub3))
		var subArg, campArg interface{}
		if subscriberID != uuid.Nil {
			subArg = subscriberID
		}
		if campaignID != uuid.Nil {
			campArg = campaignID
		}
		convertedAt := time.Now()
		if rec.ConversionUnixTimestamp > 0 {
			convertedAt = time.Unix(rec.ConversionUnixTimestamp, 0)
		}
		efOfferID := strings.TrimSpace(rec.OfferID)
		if rec.Relationship != nil && rec.Relationship.Offer != nil && rec.Relationship.Offer.NetworkOfferID > 0 {
			efOfferID = strconv.Itoa(rec.Relationship.Offer.NetworkOfferID)
		}

		if _, iErr := db.ExecContext(ctx, upsertEverflowConversionSQL,
			uuid.New(), orgID, convID, txnID, efOfferID,
			subArg, campArg, rec.Sub1, rec.Sub2, rec.Sub3,
			rec.Payout, rec.Status, rec.EventName, convertedAt); iErr != nil {
			log.Printf("[EverflowConversionsSync] ERROR upserting conversion=%s: %v", convID, iErr)
			stats.Failed++
			continue
		}
		stats.Upserted++
	}

	log.Printf("[EverflowConversionsSync] %s → %s: fetched=%d enriched=%d upserted=%d skipped=%d failed=%d",
		from.Format("2006-01-02"), to.Format("2006-01-02"), stats.Fetched, stats.Enriched, stats.Upserted, stats.Skipped, stats.Failed)
	return stats, nil
}

// HandleEverflowConversionsSync is the admin HTTP wrapper around
// RunEverflowConversionsSync. Registered behind the X-Admin-Key gate (same as
// /api/admin/attribution-backfill). Params: days (default 7, max 90) or
// from/to (YYYY-MM-DD). Backfills payout onto historical conversions the
// postback path stored at $0 or missed entirely.
func HandleEverflowConversionsSync(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 90 {
			days = v
		}
		to := time.Now()
		from := to.AddDate(0, 0, -days)
		if v := r.URL.Query().Get("from"); v != "" {
			if t, err := time.Parse("2006-01-02", v); err == nil {
				from = t
			}
		}
		if v := r.URL.Query().Get("to"); v != "" {
			if t, err := time.Parse("2006-01-02", v); err == nil {
				to = t
			}
		}

		stats, err := RunEverflowConversionsSync(r.Context(), db, from, to)
		if err != nil {
			respondJSON(w, 502, map[string]string{"error": "everflow fetch failed: " + err.Error()})
			return
		}
		respondJSON(w, 200, map[string]interface{}{
			"from":     from.Format("2006-01-02"),
			"to":       to.Format("2006-01-02"),
			"fetched":  stats.Fetched,
			"enriched": stats.Enriched,
			"upserted": stats.Upserted,
			"skipped":  stats.Skipped,
			"failed":   stats.Failed,
		})
	}
}

// StartEverflowConversionsSyncScheduler runs the export pull on a fixed
// cadence so conversions that miss the live postback still land without an
// operator remembering to call the admin endpoint (Empire flooring diagnosis
// F5: 417791's two July $180 conversions never landed and the lane governor
// optimized on conversions=0). Every 6h it re-pulls the trailing 3 days —
// wide enough to catch late postback misses and Everflow status moves
// (pending→approved) on recent conversions. Double-fire across ECS tasks is
// safe: the sync is idempotent (enrich/upsert keyed on conversion_id /
// transaction_id), the duplicate cost is only a repeated report pull.
// Kill switch: EVERFLOW_CONVERSIONS_SYNC_DISABLED=1.
func StartEverflowConversionsSyncScheduler(db *sql.DB) {
	if os.Getenv("EVERFLOW_CONVERSIONS_SYNC_DISABLED") == "1" {
		log.Printf("[EverflowConversionsSync] scheduler disabled via EVERFLOW_CONVERSIONS_SYNC_DISABLED")
		return
	}
	go func() {
		// First pull shortly after boot (let the pool settle), then every 6h.
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		for {
			<-timer.C
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			to := time.Now()
			from := to.AddDate(0, 0, -3)
			if _, err := RunEverflowConversionsSync(ctx, db, from, to); err != nil {
				log.Printf("[EverflowConversionsSync] scheduled pull failed: %v", err)
			}
			cancel()
			timer.Reset(6 * time.Hour)
		}
	}()
}
