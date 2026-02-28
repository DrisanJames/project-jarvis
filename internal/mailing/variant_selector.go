package mailing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"sync"
	"time"

	"github.com/google/uuid"
)

// VariantContent holds the subject/HTML override from an A/B variant.
type VariantContent struct {
	VariantID   uuid.UUID
	Subject     string
	HTMLContent string
	FromName    string
}

// variantCacheEntry stores loaded variants with a TTL.
type variantCacheEntry struct {
	variants  []ABVariant
	winner    *ABVariant
	fetchedAt time.Time
}

// VariantSelector deterministically assigns a subscriber to an A/B variant.
// It caches variant lists per campaign with a configurable TTL (H6).
type VariantSelector struct {
	db    *sql.DB
	cache map[uuid.UUID]*variantCacheEntry
	mu    sync.RWMutex
	ttl   time.Duration
}

func NewVariantSelector(db *sql.DB) *VariantSelector {
	return &VariantSelector{
		db:    db,
		cache: make(map[uuid.UUID]*variantCacheEntry),
		ttl:   5 * time.Minute,
	}
}

// SelectVariant returns the variant content for a given email+campaign.
// Uses SHA256(email + campaign_id) for deterministic assignment.
// If no A/B test exists for this campaign, returns nil.
// If the test has a declared winner, always returns the winner.
func (vs *VariantSelector) SelectVariant(ctx context.Context, campaignID uuid.UUID, email string) (*VariantContent, error) {
	variants, winner, err := vs.getVariants(ctx, campaignID)
	if err != nil || len(variants) == 0 {
		return nil, err
	}

	if winner != nil {
		return &VariantContent{
			VariantID:   winner.ID,
			Subject:     winner.VariantValue,
			HTMLContent: "",
			FromName:    "",
		}, nil
	}

	hash := sha256.Sum256([]byte(email + campaignID.String()))
	idx := int(binary.BigEndian.Uint64(hash[:8])) % len(variants)
	if idx < 0 {
		idx = -idx
	}

	v := variants[idx]
	return &VariantContent{
		VariantID:   v.ID,
		Subject:     v.VariantValue,
		HTMLContent: "",
		FromName:    "",
	}, nil
}

func (vs *VariantSelector) getVariants(ctx context.Context, campaignID uuid.UUID) ([]ABVariant, *ABVariant, error) {
	vs.mu.RLock()
	cached, ok := vs.cache[campaignID]
	vs.mu.RUnlock()

	if ok && time.Since(cached.fetchedAt) < vs.ttl {
		return cached.variants, cached.winner, nil
	}

	variants, err := vs.loadVariants(ctx, campaignID)
	if err != nil {
		if ok {
			return cached.variants, cached.winner, nil
		}
		return nil, nil, err
	}

	var winner *ABVariant
	for i := range variants {
		if variants[i].IsWinner {
			winner = &variants[i]
			break
		}
	}

	vs.mu.Lock()
	vs.cache[campaignID] = &variantCacheEntry{
		variants:  variants,
		winner:    winner,
		fetchedAt: time.Now(),
	}
	vs.mu.Unlock()

	return variants, winner, nil
}

func (vs *VariantSelector) loadVariants(ctx context.Context, campaignID uuid.UUID) ([]ABVariant, error) {
	rows, err := vs.db.QueryContext(ctx,
		`SELECT v.id, v.campaign_id, v.variant_name, COALESCE(v.variant_type,''), COALESCE(v.variant_value,''),
		        v.traffic_percentage, v.is_winner
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.campaign_id = v.campaign_id
		WHERE v.campaign_id = $1 AND t.status IN ('running', 'completed')
		ORDER BY v.variant_name`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var variants []ABVariant
	for rows.Next() {
		var v ABVariant
		if err := rows.Scan(&v.ID, &v.CampaignID, &v.VariantName, &v.VariantType, &v.VariantValue, &v.TrafficPercentage, &v.IsWinner); err != nil {
			continue
		}
		variants = append(variants, v)
	}
	return variants, rows.Err()
}
