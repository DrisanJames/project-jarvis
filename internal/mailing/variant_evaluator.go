package mailing

import (
	"context"
	"database/sql"
	"log"
	"math"
	"time"

	"github.com/google/uuid"
)

// VariantEvaluator is a background worker that monitors A/B test performance
// and declares winners when statistical significance is reached (H18).
type VariantEvaluator struct {
	db            *sql.DB
	interval      time.Duration
	minSampleSize int
	maxWaitHours  int
	pThreshold    float64
	minEffect     float64
	ctx           context.Context
	cancel        context.CancelFunc
	lastRunAt     time.Time
	healthy       bool
}

func NewVariantEvaluator(db *sql.DB) *VariantEvaluator {
	return &VariantEvaluator{
		db:            db,
		interval:      5 * time.Minute,
		minSampleSize: 200,
		maxWaitHours:  24,
		pThreshold:    0.05,
		minEffect:     0.005, // 0.5 percentage points
		healthy:       true,
	}
}

func (ve *VariantEvaluator) Start() {
	ve.ctx, ve.cancel = context.WithCancel(context.Background())
	go func() {
		log.Println("[VariantEvaluator] Starting A/B test evaluator")
		time.Sleep(2 * time.Minute)
		ve.runOnce()

		ticker := time.NewTicker(ve.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ve.ctx.Done():
				log.Println("[VariantEvaluator] Stopped")
				return
			case <-ticker.C:
				ve.runOnce()
			}
		}
	}()
}

func (ve *VariantEvaluator) Stop() {
	if ve.cancel != nil {
		ve.cancel()
	}
}

func (ve *VariantEvaluator) IsHealthy() bool  { return ve.healthy }
func (ve *VariantEvaluator) LastRunAt() time.Time { return ve.lastRunAt }

func (ve *VariantEvaluator) runOnce() {
	ve.lastRunAt = time.Now()
	ve.healthy = true
	ctx := ve.ctx

	rows, err := ve.db.QueryContext(ctx,
		`SELECT id, campaign_id, created_at FROM mailing_ab_tests WHERE status = 'running'`)
	if err != nil {
		log.Printf("[VariantEvaluator] query error: %v", err)
		ve.healthy = false
		return
	}
	defer rows.Close()

	for rows.Next() {
		var testID, campaignID uuid.UUID
		var createdAt time.Time
		if err := rows.Scan(&testID, &campaignID, &createdAt); err != nil {
			continue
		}
		if err := ve.evaluateTest(ctx, testID, campaignID, createdAt); err != nil {
			log.Printf("[VariantEvaluator] evaluate test %s error: %v", testID, err)
		}
	}
}

type variantMetrics struct {
	variantID  uuid.UUID
	sends      int
	opens      int
	clicks     int
	openRate   float64
	clickRate  float64
}

func (ve *VariantEvaluator) evaluateTest(ctx context.Context, testID, campaignID uuid.UUID, testStarted time.Time) error {
	vRows, err := ve.db.QueryContext(ctx,
		`SELECT id FROM mailing_ab_variants WHERE test_id = $1`, testID)
	if err != nil {
		return err
	}
	defer vRows.Close()

	var metrics []variantMetrics
	for vRows.Next() {
		var vid uuid.UUID
		if err := vRows.Scan(&vid); err != nil {
			continue
		}
		m, err := ve.computeVariantMetrics(ctx, vid)
		if err != nil {
			continue
		}
		metrics = append(metrics, m)
	}

	if len(metrics) < 2 {
		return nil
	}

	// Check if all variants have minimum sample
	allReady := true
	for _, m := range metrics {
		if m.sends < ve.minSampleSize {
			allReady = false
			break
		}
	}

	// H18: Force decision after maxWaitHours even if not significant
	timedOut := time.Since(testStarted) > time.Duration(ve.maxWaitHours)*time.Hour

	if !allReady && !timedOut {
		return nil
	}

	// Find best performer by open rate
	bestIdx := 0
	for i, m := range metrics {
		if m.openRate > metrics[bestIdx].openRate {
			bestIdx = i
		}
	}

	// Check statistical significance between best and second best
	significant := false
	if len(metrics) == 2 {
		significant = ve.isStatisticallySignificant(
			metrics[0].openRate, metrics[1].openRate,
			metrics[0].sends, metrics[1].sends,
		)
	}

	// H18: Early stopping — 3+ standard deviations ahead after 500 samples
	earlyStop := false
	if len(metrics) == 2 && metrics[0].sends >= 500 && metrics[1].sends >= 500 {
		best := metrics[bestIdx]
		other := metrics[1-bestIdx]
		if best.sends > 0 && other.sends > 0 {
			pooledSE := math.Sqrt(best.openRate*(1-best.openRate)/float64(best.sends) +
				other.openRate*(1-other.openRate)/float64(other.sends))
			if pooledSE > 0 && math.Abs(best.openRate-other.openRate)/pooledSE >= 3.0 {
				earlyStop = true
			}
		}
	}

	if !significant && !timedOut && !earlyStop {
		return nil
	}

	// H18: Don't declare winner for trivial differences
	if len(metrics) == 2 && math.Abs(metrics[0].openRate-metrics[1].openRate) < ve.minEffect && !timedOut {
		return nil
	}

	winner := metrics[bestIdx]

	// Declare winner
	ve.db.ExecContext(ctx,
		`UPDATE mailing_ab_variants SET is_winner = TRUE WHERE id = $1`, winner.variantID)
	ve.db.ExecContext(ctx,
		`UPDATE mailing_ab_tests SET status = 'completed', winner_variant_id = $1, completed_at = NOW() WHERE id = $2`,
		winner.variantID, testID)

	// Record learning
	ve.recordLearning(ctx, testID, campaignID, winner)

	log.Printf("[VariantEvaluator] Test %s: winner=%s open_rate=%.4f (significant=%v timeout=%v early=%v)",
		testID, winner.variantID, winner.openRate, significant, timedOut, earlyStop)

	return nil
}

func (ve *VariantEvaluator) computeVariantMetrics(ctx context.Context, variantID uuid.UUID) (variantMetrics, error) {
	m := variantMetrics{variantID: variantID}

	ve.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriber_events WHERE variant_id = $1 AND event_type = 'send'`,
		variantID).Scan(&m.sends)
	ve.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriber_events WHERE variant_id = $1 AND event_type = 'open'`,
		variantID).Scan(&m.opens)
	ve.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriber_events WHERE variant_id = $1 AND event_type = 'click'`,
		variantID).Scan(&m.clicks)

	if m.sends > 0 {
		m.openRate = float64(m.opens) / float64(m.sends)
		m.clickRate = float64(m.clicks) / float64(m.sends)
	}
	return m, nil
}

// isStatisticallySignificant performs a Z-test for two proportions.
func (ve *VariantEvaluator) isStatisticallySignificant(rateA, rateB float64, nA, nB int) bool {
	if nA == 0 || nB == 0 {
		return false
	}
	pooled := (rateA*float64(nA) + rateB*float64(nB)) / float64(nA+nB)
	if pooled <= 0 || pooled >= 1 {
		return false
	}
	se := math.Sqrt(pooled * (1 - pooled) * (1.0/float64(nA) + 1.0/float64(nB)))
	if se == 0 {
		return false
	}
	z := math.Abs(rateA-rateB) / se
	// p < 0.05 corresponds to |z| > 1.96
	return z > 1.96
}

func (ve *VariantEvaluator) recordLearning(ctx context.Context, testID, campaignID uuid.UUID, winner variantMetrics) {
	ve.db.ExecContext(ctx,
		`INSERT INTO content_learnings (organization_id, campaign_id, ab_test_id, variant_id, sample_size, open_rate, click_rate, is_winner)
		SELECT t.organization_id, $1, $2, $3, $4, $5, $6, TRUE
		FROM mailing_ab_tests t WHERE t.id = $2`,
		campaignID, testID, winner.variantID, winner.sends, winner.openRate, winner.clickRate,
	)
}
