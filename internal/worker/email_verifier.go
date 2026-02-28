package worker

import (
	"context"
	"database/sql"
	"log"
	"net"
	"strings"
	"time"
)

// EmailVerificationProvider abstracts third-party email verification (H5).
type EmailVerificationProvider interface {
	Verify(ctx context.Context, email string) (VerificationResult, error)
}

// VerificationResult holds the outcome from a verification provider.
type VerificationResult struct {
	Status string  // "valid", "invalid", "catch-all", "unknown"
	Score  float64 // mapped quality score
}

// EmailVerifier runs background email validation on imported subscribers.
// It uses local MX lookup as a free pre-filter, then delegates to a
// third-party provider for definitive verification.
type EmailVerifier struct {
	db           *sql.DB
	provider     EmailVerificationProvider
	batchSize    int
	interval     time.Duration
	ratePerMin   int
	ctx          context.Context
	cancel       context.CancelFunc
	lastRunAt    time.Time
	healthy      bool
}

func NewEmailVerifier(db *sql.DB, provider EmailVerificationProvider) *EmailVerifier {
	return &EmailVerifier{
		db:         db,
		provider:   provider,
		batchSize:  50,
		interval:   1 * time.Minute,
		ratePerMin: 100,
		healthy:    true,
	}
}

func (v *EmailVerifier) Start() {
	v.ctx, v.cancel = context.WithCancel(context.Background())
	go func() {
		log.Println("[EmailVerifier] Starting email verification worker")
		time.Sleep(30 * time.Second) // initial delay
		v.runOnce()

		ticker := time.NewTicker(v.interval)
		defer ticker.Stop()
		for {
			select {
			case <-v.ctx.Done():
				log.Println("[EmailVerifier] Stopped")
				return
			case <-ticker.C:
				v.runOnce()
			}
		}
	}()
}

func (v *EmailVerifier) Stop() {
	if v.cancel != nil {
		v.cancel()
	}
}

func (v *EmailVerifier) IsHealthy() bool { return v.healthy }
func (v *EmailVerifier) LastRunAt() time.Time { return v.lastRunAt }

func (v *EmailVerifier) runOnce() {
	v.lastRunAt = time.Now()
	v.healthy = true
	ctx := v.ctx

	// Phase 1: MX pre-filter for unverified subscribers (score < 0.25)
	v.verifyMXBatch(ctx)

	// Phase 2: Third-party API for MX-valid subscribers (score = 0.25)
	if v.provider != nil {
		v.verifyAPIBatch(ctx)
	}
}

func (v *EmailVerifier) verifyMXBatch(ctx context.Context) {
	rows, err := v.db.QueryContext(ctx,
		`SELECT id, email FROM mailing_subscribers
		WHERE data_quality_score < 0.25 AND status = 'confirmed'
		ORDER BY created_at ASC LIMIT $1`, v.batchSize)
	if err != nil {
		log.Printf("[EmailVerifier] MX batch query error: %v", err)
		v.healthy = false
		return
	}
	defer rows.Close()

	for rows.Next() {
		if ctx.Err() != nil {
			return
		}
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			continue
		}

		if v.checkMX(email) {
			v.db.ExecContext(ctx,
				`UPDATE mailing_subscribers SET data_quality_score = 0.25, verification_status = 'mx_valid', verified_at = NOW(), updated_at = NOW()
				WHERE id = $1 AND data_quality_score < 0.25`, id)
		} else {
			v.db.ExecContext(ctx,
				`UPDATE mailing_subscribers SET data_quality_score = 0.00, verification_status = 'mx_failed', verified_at = NOW(), updated_at = NOW()
				WHERE id = $1`, id)
		}
	}
}

func (v *EmailVerifier) verifyAPIBatch(ctx context.Context) {
	rows, err := v.db.QueryContext(ctx,
		`SELECT id, email FROM mailing_subscribers
		WHERE data_quality_score = 0.25 AND verification_status = 'mx_valid' AND status = 'confirmed'
		ORDER BY created_at ASC LIMIT $1`, v.batchSize)
	if err != nil {
		log.Printf("[EmailVerifier] API batch query error: %v", err)
		return
	}
	defer rows.Close()

	processed := 0
	for rows.Next() {
		if ctx.Err() != nil || processed >= v.ratePerMin {
			return
		}
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			continue
		}

		result, err := v.provider.Verify(ctx, email)
		if err != nil {
			log.Printf("[EmailVerifier] API error for %s: %v", email, err)
			continue
		}

		status := "api_" + result.Status
		score := result.Score

		// H5: Score mapping
		switch result.Status {
		case "valid":
			score = 0.50
		case "catch-all":
			score = 0.30
		case "invalid":
			score = 0.00
			// Suppress invalid emails immediately
			v.db.ExecContext(ctx,
				`UPDATE mailing_subscribers SET status = 'bounced', data_quality_score = 0.00, verification_status = $1, verified_at = NOW(), updated_at = NOW()
				WHERE id = $2`, status, id)
			processed++
			continue
		default:
			score = 0.25 // unknown — keep at MX level
		}

		v.db.ExecContext(ctx,
			`UPDATE mailing_subscribers SET data_quality_score = GREATEST(data_quality_score, $1), verification_status = $2, verified_at = NOW(), updated_at = NOW()
			WHERE id = $3`, score, status, id)
		processed++
	}
}

func (v *EmailVerifier) checkMX(email string) bool {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resolver := &net.Resolver{}
	records, err := resolver.LookupMX(ctx, parts[1])
	return err == nil && len(records) > 0
}
