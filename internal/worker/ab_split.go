package worker

// Wave-path A/B split (operator 2026-07-03; plan docs/plans/AB_SPLIT_WAVE_PATH_PLAN.md).
//
// When a campaign has rows in mailing_ab_tests/mailing_ab_variants (the SAME tables the
// campaign-builder A/B UI reads), the set-based wave enqueue assigns each recipient a
// variant deterministically by subscriber hash and stamps the queue row with the
// variant's content_snapshot_id + creative_id (= the ab_variant id). Winner metric is
// raw open rate — the operator's inboxing meter (MPP/proxy fetch fires only on inboxed
// mail): opens join back to queue rows per (campaign_id, subscriber_id) → creative_id.
//
// Safety properties:
//   - campaigns with NO variants take the pre-existing code path byte-identically;
//   - assignment is deterministic in subscriber_id (re-dispatch/re-enqueue assigns the
//     same variant; idempotency_key already dedups the row itself);
//   - any malformed variant set (0/1 usable variants, empty html) disables the split
//     for that campaign — fail OPEN to variant A behavior, never drop a send;
//   - kill switch DISABLE_WAVE_AB_SPLIT=true disables globally without redeploy.

import (
	"context"
	"database/sql"
	"hash/fnv"
	"log"
	"os"
	"sort"

	"github.com/google/uuid"
)

type waveABVariant struct {
	CreativeID uuid.UUID // mailing_ab_variants.id — stamped into queue.creative_id
	SnapshotID uuid.UUID // per-variant content snapshot
	SplitPct   int
}

func waveABSplitDisabled() bool {
	return os.Getenv("DISABLE_WAVE_AB_SPLIT") == "true"
}

// loadWaveABVariants resolves a campaign's A/B variants into per-variant content
// snapshots. Returns nil (split disabled) unless there are >=2 usable variants.
// Variant subjects/from_names are deliberately IGNORED on this path — the operator's
// test isolates the creative (same subject both arms).
func loadWaveABVariants(ctx context.Context, db *sql.DB, campaignID uuid.UUID, waveID, plainContent string, locked bool) []waveABVariant {
	if waveABSplitDisabled() {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT v.id, COALESCE(v.html_content, ''), COALESCE(v.split_percent, 0)
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1 AND COALESCE(t.status,'') NOT IN ('cancelled','completed')
		ORDER BY v.variant_name ASC
	`, campaignID)
	if err != nil {
		log.Printf("[ab-split] variant lookup failed campaign=%s: %v — sending single-variant", campaignID, err)
		return nil
	}
	defer rows.Close()

	type raw struct {
		id    uuid.UUID
		html  string
		split int
	}
	var raws []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.id, &r.html, &r.split); err != nil {
			log.Printf("[ab-split] variant scan failed campaign=%s: %v — sending single-variant", campaignID, err)
			return nil
		}
		if r.html != "" {
			raws = append(raws, r)
		}
	}
	if len(raws) < 2 || len(raws) > 4 {
		return nil
	}

	// Normalize splits: any non-positive or non-100 total → equal split.
	total := 0
	for _, r := range raws {
		if r.split <= 0 {
			total = -1
			break
		}
		total += r.split
	}
	if total != 100 {
		for i := range raws {
			raws[i].split = 100 / len(raws)
		}
		raws[0].split += 100 - (100/len(raws))*len(raws) // remainder to A
	}

	out := make([]waveABVariant, 0, len(raws))
	for _, r := range raws {
		snapID, serr := ensureContentSnapshot(ctx, db, campaignID, waveID, r.html, plainContent, locked)
		if serr != nil {
			log.Printf("[ab-split] variant snapshot failed campaign=%s variant=%s: %v — sending single-variant", campaignID, r.id, serr)
			return nil
		}
		out = append(out, waveABVariant{CreativeID: r.id, SnapshotID: snapID, SplitPct: r.split})
	}
	// Deterministic order regardless of DB ordering quirks.
	sort.Slice(out, func(i, j int) bool { return out[i].CreativeID.String() < out[j].CreativeID.String() })
	return out
}

// pickWaveABVariant deterministically maps a subscriber to a variant index by
// cumulative split. Stable across processes and re-runs (fnv-1a of the uuid string).
func pickWaveABVariant(subscriberID uuid.UUID, variants []waveABVariant) int {
	h := fnv.New32a()
	h.Write([]byte(subscriberID.String()))
	bucket := int(h.Sum32() % 100)
	cum := 0
	for i, v := range variants {
		cum += v.SplitPct
		if bucket < cum {
			return i
		}
	}
	return len(variants) - 1
}
