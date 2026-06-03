package worker

// JourneyClickTrackingEnroller is the INTERNAL-click producer for the
// click-drip queue. It exists because Everflow's per-offer click postbacks
// don't reliably reach us (they fire on conversion and macro substitution is
// flaky), so the real click stream — tens of thousands of internal click
// events per day in mailing_tracking_events — never entered the drip.
//
// Pipeline position:
//
//	mailing_tracking_events (event_type='clicked', cratoolpro money URL)
//	      │  this worker: resolve offer from the trailing slug
//	      ▼
//	mailing_journey_event_triggers (source='internal_click_tracking')
//	      │  JourneyEventEnroller (existing) drains + gates + enrolls
//	      ▼
//	mailing_journey_enrollments → JourneyExecutor → reminder emails
//
// By writing into the SAME trigger queue the Everflow path uses, every
// downstream guarantee is reused unchanged: journey-map gating (offer mapped /
// enabled / non-CPC), already-converted suppression skip, 24h idempotency,
// sending-profile resolution from the original campaign, and the
// ON CONFLICT (journey_id, subscriber_email) enrollment dedup. This worker only
// adds the missing inlet.
//
// Offer identity: mailing_tracking_events.offer_id is 100% NULL in production,
// but the offer is encoded in link_url as the cratoolpro slug
// (https://www.cratoolpro.com/BJB4Q5BF/<SLUG>/). mailing_offer_slug_map is the
// verified slug → everflow_offer_id dictionary; a click whose slug is not in
// the map (or maps to an offer with no journey) is skipped.
//
// Multi-instance safety: the server runs on multiple ECS tasks, so this worker
// runs N times concurrently. A single shared cursor row
// (mailing_clickdrip_enroller_cursor) is advanced under SELECT ... FOR UPDATE,
// which serializes time-slice ownership across instances — only one task
// processes any given slice. A per-(subscriber, offer) 24h trigger-dedup guards
// the boundary case.

import (
	"context"
	"database/sql"
	"log"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// Tunables. Click-drip's first reminder is +1h, so enrollment latency of a few
// seconds is irrelevant; we favor a calm cadence and a bounded per-tick scan.
const (
	DefaultClickTrackingInterval   = 30 * time.Second
	DefaultClickTrackingBatchLimit = 500
	// clickTrackingMaxSliceMinutes bounds how much wall-clock time a single
	// tick scans, so a worker that fell behind (deploy, downtime) catches up
	// over several small scans instead of one giant ILIKE table scan.
	clickTrackingMaxSliceMinutes = 15
)

// cratoolproSlugRe extracts the trailing offer slug from a cratoolpro money URL.
// Example: https://www.cratoolpro.com/BJB4Q5BF/KW3Q1DJ/?source_id=email...
var cratoolproSlugRe = regexp.MustCompile(`cratoolpro\.com/BJB4Q5BF/([A-Za-z0-9_-]+)`)

// JourneyClickTrackingEnroller is the worker handle.
type JourneyClickTrackingEnroller struct {
	db         *sql.DB
	interval   time.Duration
	batchLimit int
	stopChan   chan struct{}
	stopOnce   sync.Once

	totalScanned int64
	totalQueued  int64
	totalSkipped int64
	totalErrors  int64
}

// NewJourneyClickTrackingEnroller wires the worker.
func NewJourneyClickTrackingEnroller(db *sql.DB) *JourneyClickTrackingEnroller {
	return &JourneyClickTrackingEnroller{
		db:         db,
		interval:   DefaultClickTrackingInterval,
		batchLimit: DefaultClickTrackingBatchLimit,
		stopChan:   make(chan struct{}),
	}
}

// WithInterval overrides the poll cadence (tests / incidents).
func (w *JourneyClickTrackingEnroller) WithInterval(d time.Duration) *JourneyClickTrackingEnroller {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithBatchLimit overrides the per-tick cap.
func (w *JourneyClickTrackingEnroller) WithBatchLimit(n int) *JourneyClickTrackingEnroller {
	if n > 0 {
		w.batchLimit = n
	}
	return w
}

// Start runs the polling loop until ctx is cancelled or Stop() is called.
func (w *JourneyClickTrackingEnroller) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneyClickTrackingEnroller: started (interval=%s, batchLimit=%d)", w.interval, w.batchLimit)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneyClickTrackingEnroller: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneyClickTrackingEnroller: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the polling loop. Idempotent.
func (w *JourneyClickTrackingEnroller) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats returns lifetime counters for observability.
func (w *JourneyClickTrackingEnroller) Stats() (scanned, queued, skipped, errors int64) {
	return atomic.LoadInt64(&w.totalScanned),
		atomic.LoadInt64(&w.totalQueued),
		atomic.LoadInt64(&w.totalSkipped),
		atomic.LoadInt64(&w.totalErrors)
}

// clickCandidate is one money-URL click row from the cursor slice.
type clickCandidate struct {
	subscriberID  string
	eventAt       time.Time
	linkURL       string
	sendingDomain string
	campaignID    string
}

// tick processes one bounded slice of internal clicks under a cursor lock.
func (w *JourneyClickTrackingEnroller) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Ensure the singleton cursor row exists. Default last_event_at=NOW() means
	// a fresh deploy starts go-forward (no retroactive enrollment of historical
	// clickers, which would blast reminders at everyone at once).
	if _, err := w.db.ExecContext(tickCtx,
		`INSERT INTO mailing_clickdrip_enroller_cursor (id) VALUES (1) ON CONFLICT (id) DO NOTHING`); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: ensure cursor: %v", err)
		return
	}

	// Load the slug dictionary once per tick (8 rows; trivial).
	slugToOffer, err := w.loadSlugMap(tickCtx)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: load slug map: %v", err)
		return
	}
	if len(slugToOffer) == 0 {
		return // nothing mapped yet
	}

	tx, err := w.db.BeginTx(tickCtx, nil)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: begin tx: %v", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Serialize slice ownership across ECS tasks.
	var cursor time.Time
	if err := tx.QueryRowContext(tickCtx,
		`SELECT last_event_at FROM mailing_clickdrip_enroller_cursor WHERE id=1 FOR UPDATE`).Scan(&cursor); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: lock cursor: %v", err)
		return
	}

	// Upper bound: at most clickTrackingMaxSliceMinutes past the cursor, never
	// beyond NOW().
	now := time.Now().UTC()
	upper := cursor.Add(clickTrackingMaxSliceMinutes * time.Minute)
	if upper.After(now) {
		upper = now
	}
	if !upper.After(cursor) {
		// Cursor already at/after the cap; nothing to do, release the lock.
		committed = true
		_ = tx.Commit()
		return
	}

	rows, err := tx.QueryContext(tickCtx, `
		SELECT t.subscriber_id::text, t.event_at, t.link_url,
		       COALESCE(t.sending_domain,''), COALESCE(t.campaign_id::text,'')
		FROM mailing_tracking_events t
		WHERE t.event_type='clicked'
		  AND t.event_at > $1 AND t.event_at <= $2
		  AND t.link_url ILIKE '%cratoolpro.com/BJB4Q5BF/%'
		  AND NOT EXISTS (
		        SELECT 1 FROM mailing_campaigns c
		        WHERE c.id = t.campaign_id AND c.campaign_type = 'click_drip'
		      )
		ORDER BY t.event_at ASC
		LIMIT $3
	`, cursor, upper, w.batchLimit)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: scan clicks: %v", err)
		return
	}

	var cands []clickCandidate
	for rows.Next() {
		var c clickCandidate
		if err := rows.Scan(&c.subscriberID, &c.eventAt, &c.linkURL, &c.sendingDomain, &c.campaignID); err != nil {
			rows.Close()
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyClickTrackingEnroller: scan row: %v", err)
			return
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: rows err: %v", err)
		return
	}

	atomic.AddInt64(&w.totalScanned, int64(len(cands)))

	queued := 0
	for _, c := range cands {
		offerID, ok := resolveOfferFromLink(c.linkURL, slugToOffer)
		if !ok {
			atomic.AddInt64(&w.totalSkipped, 1)
			continue
		}

		// Per-(subscriber, offer) 24h dedup — keeps the trigger queue tidy and
		// guards the cursor-boundary reprocess case. (The downstream enroller
		// also dedups, but suppressing here avoids noise.)
		var dup int
		if err := tx.QueryRowContext(tickCtx, `
			SELECT 1 FROM mailing_journey_event_triggers
			WHERE subscriber_id=$1 AND everflow_offer_id=$2
			  AND received_at > NOW() - INTERVAL '24 hours'
			LIMIT 1
		`, c.subscriberID, offerID).Scan(&dup); err == nil {
			atomic.AddInt64(&w.totalSkipped, 1)
			continue
		} else if err != sql.ErrNoRows {
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyClickTrackingEnroller: dedup check: %v", err)
			continue
		}

		var campaignArg interface{}
		if c.campaignID != "" {
			campaignArg = c.campaignID
		} else {
			campaignArg = ""
		}

		if _, err := tx.ExecContext(tickCtx, `
			INSERT INTO mailing_journey_event_triggers (
				id, source, everflow_offer_id, subscriber_id, subscriber_email,
				sub2_brand, sub3_campaign_id, click_id, sending_profile_id,
				sending_domain, click_url, status, received_at
			) VALUES (
				gen_random_uuid(), 'internal_click_tracking', $1, $2, NULL,
				$3, $4, '', NULL,
				$5, $6, 'pending', NOW()
			)
		`,
			offerID,
			c.subscriberID,
			brand.Root(c.sendingDomain),
			campaignArg,
			c.sendingDomain,
			c.linkURL,
		); err != nil {
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyClickTrackingEnroller: insert trigger (offer=%s sub=%s): %v", offerID, c.subscriberID, err)
			continue
		}
		queued++
	}

	// Advance the cursor. If we hit the batch limit there may be more clicks in
	// (lastEventAt, upper]; resume from the last row's event_at next tick.
	// Otherwise advance to the slice upper bound.
	newCursor := upper
	if len(cands) >= w.batchLimit && len(cands) > 0 {
		newCursor = cands[len(cands)-1].eventAt
	}
	if _, err := tx.ExecContext(tickCtx,
		`UPDATE mailing_clickdrip_enroller_cursor SET last_event_at=$1, updated_at=NOW() WHERE id=1`,
		newCursor); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: advance cursor: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: commit: %v", err)
		return
	}
	committed = true

	atomic.AddInt64(&w.totalQueued, int64(queued))
	if queued > 0 || len(cands) > 0 {
		log.Printf("JourneyClickTrackingEnroller: slice (%s, %s] scanned=%d queued=%d (cursor→%s)",
			cursor.Format(time.RFC3339), upper.Format(time.RFC3339), len(cands), queued, newCursor.Format(time.RFC3339))
	}
}

// loadSlugMap returns enabled slug → everflow_offer_id mappings.
func (w *JourneyClickTrackingEnroller) loadSlugMap(ctx context.Context) (map[string]string, error) {
	rows, err := w.db.QueryContext(ctx,
		`SELECT cratoolpro_slug, everflow_offer_id FROM mailing_offer_slug_map WHERE enabled=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var slug, offer string
		if err := rows.Scan(&slug, &offer); err != nil {
			return nil, err
		}
		out[normalizeSlug(slug)] = offer
	}
	return out, rows.Err()
}

// resolveOfferFromLink extracts the cratoolpro slug from a link and maps it to
// an everflow offer id. ok=false when the link carries no recognizable slug or
// the slug is not in the dictionary.
func resolveOfferFromLink(linkURL string, slugToOffer map[string]string) (string, bool) {
	m := cratoolproSlugRe.FindStringSubmatch(linkURL)
	if len(m) < 2 {
		return "", false
	}
	offer, ok := slugToOffer[normalizeSlug(m[1])]
	return offer, ok
}

// normalizeSlug upper-cases and trims a slug so map lookups are case-stable.
func normalizeSlug(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
