package worker

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SuppressionListWorker runs nightly to build brand-specific sunset suppression
// lists. For each sending brand, it finds subscribers who received 3+ campaign
// deliveries within a 90-day window but opened zero messages. Those emails are
// MD5-hashed and inserted into mailing_suppression_entries for consumption by
// the SuppressionMatcher at send time.
//
// Brand-specificity is critical: a subscriber exhausted on Discount Blog can
// still be mailed by Quiz Fiesta. Suppression is per-brand, not global.
type SuppressionListWorker struct {
	db               *sql.DB
	interval         time.Duration
	minSends         int
	lookbackDays     int
	circuitBreakerPct float64
}

func NewSuppressionListWorker(db *sql.DB, interval time.Duration) *SuppressionListWorker {
	return &SuppressionListWorker{
		db:                db,
		interval:          interval,
		minSends:          3,
		lookbackDays:      90,
		circuitBreakerPct: 3.0,
	}
}

func (w *SuppressionListWorker) Start(ctx context.Context) {
	go func() {
		time.Sleep(90 * time.Second)
		w.run(ctx)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.run(ctx)
			case <-ctx.Done():
				log.Println("[suppression-list] stopped")
				return
			}
		}
	}()
}

const suppressionWorkerLockID int64 = 867530901

func (w *SuppressionListWorker) run(ctx context.Context) {
	runStart := time.Now()
	log.Println("[suppression-list] run starting")

	var acquired bool
	if err := w.db.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", suppressionWorkerLockID).Scan(&acquired); err != nil {
		log.Printf("[suppression-list] advisory lock failed: %v", err)
		return
	}
	if !acquired {
		log.Println("[suppression-list] another instance holds the lock — skipping")
		return
	}
	defer func() {
		if _, err := w.db.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", suppressionWorkerLockID); err != nil {
			log.Printf("[suppression-list] WARNING: failed to release advisory lock: %v", err)
		}
	}()

	brands, err := w.discoverBrands(ctx)
	if err != nil {
		log.Printf("[suppression-list] failed to discover brands: %v", err)
		return
	}
	if len(brands) == 0 {
		log.Println("[suppression-list] no brands found (is base_domain populated on sending profiles?) — skipping")
		return
	}

	log.Printf("[suppression-list] processing %d brands: %v", len(brands), brandNames(brands))

	for _, brand := range brands {
		if ctx.Err() != nil {
			log.Println("[suppression-list] context cancelled — aborting remaining brands")
			return
		}
		w.processBrand(ctx, brand)
	}

	log.Printf("[suppression-list] run complete in %v", time.Since(runStart))
}

type brandInfo struct {
	baseDomain     string
	sendingDomains []string
	orgID          string
}

func (w *SuppressionListWorker) discoverBrands(ctx context.Context) ([]brandInfo, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT organization_id::text, base_domain, ARRAY_AGG(sending_domain)
		FROM mailing_sending_profiles
		WHERE status = 'active'
		  AND base_domain IS NOT NULL AND base_domain != ''
		  AND sending_domain IS NOT NULL AND sending_domain != ''
		GROUP BY organization_id, base_domain
	`)
	if err != nil {
		return nil, fmt.Errorf("query sending profiles: %w", err)
	}
	defer rows.Close()

	var brands []brandInfo
	for rows.Next() {
		var b brandInfo
		var domainsStr string
		if err := rows.Scan(&b.orgID, &b.baseDomain, &domainsStr); err != nil {
			log.Printf("[suppression-list] scan error: %v", err)
			continue
		}
		b.sendingDomains = parsePostgresTextArray(domainsStr)
		if len(b.sendingDomains) > 0 {
			brands = append(brands, b)
		}
	}
	return brands, rows.Err()
}

func (w *SuppressionListWorker) processBrand(ctx context.Context, brand brandInfo) {
	start := time.Now()
	listName := fmt.Sprintf("Sunset Suppression - %s", brand.baseDomain)
	log.Printf("[suppression-list] %s: starting", brand.baseDomain)

	runID := uuid.New().String()
	w.db.ExecContext(ctx, `
		INSERT INTO suppression_list_runs (id, brand, started_at, status)
		VALUES ($1, $2, NOW(), 'running')
	`, runID, brand.baseDomain)

	completeRun := func(status string, count int, errMsg string) {
		if _, err := w.db.ExecContext(ctx, `
			UPDATE suppression_list_runs
			SET completed_at = NOW(), status = $1, suppressed_count = $2, error_message = $3
			WHERE id = $4
		`, status, count, errMsg, runID); err != nil {
			log.Printf("[suppression-list] %s: failed to update run record %s: %v", brand.baseDomain, runID, err)
		}
	}

	exhaustedEmails, err := w.findExhaustedEmails(ctx, brand.sendingDomains)
	if err != nil {
		log.Printf("[suppression-list] %s: query error: %v", brand.baseDomain, err)
		completeRun("failed", 0, err.Error())
		return
	}

	if len(exhaustedEmails) == 0 {
		log.Printf("[suppression-list] %s: no exhausted emails found", brand.baseDomain)
		completeRun("completed", 0, "")
		return
	}

	if !w.circuitBreakerCheck(ctx, brand.baseDomain, len(exhaustedEmails)) {
		completeRun("circuit_breaker", 0, fmt.Sprintf("new count %d exceeds threshold", len(exhaustedEmails)))
		return
	}

	listID, err := w.ensureSuppressionList(ctx, brand.orgID, listName)
	if err != nil {
		log.Printf("[suppression-list] %s: list creation failed: %v", brand.baseDomain, err)
		completeRun("failed", 0, err.Error())
		return
	}

	inserted, err := w.insertHashes(ctx, listID, exhaustedEmails)
	if err != nil {
		log.Printf("[suppression-list] %s: insert error: %v", brand.baseDomain, err)
		completeRun("failed", inserted, err.Error())
		return
	}

	elapsed := time.Since(start)
	log.Printf("[suppression-list] %s: suppressed %d emails (%d new) in %v", brand.baseDomain, len(exhaustedEmails), inserted, elapsed)
	completeRun("completed", inserted, "")
}

func (w *SuppressionListWorker) findExhaustedEmails(ctx context.Context, sendingDomains []string) ([]string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	domainParams := make([]string, len(sendingDomains))
	args := make([]any, 0, len(sendingDomains)+1)
	for i, d := range sendingDomains {
		domainParams[i] = fmt.Sprintf("$%d", i+1)
		args = append(args, d)
	}
	args = append(args, w.minSends)
	minSendsParam := fmt.Sprintf("$%d", len(sendingDomains)+1)

	query := fmt.Sprintf(`
		WITH delivered AS (
			SELECT LOWER(s.email) AS email,
			       COUNT(DISTINCT te.campaign_id) FILTER (WHERE te.campaign_id IS NOT NULL) AS send_count
			FROM mailing_tracking_events te
			JOIN mailing_subscribers s ON s.id = te.subscriber_id
			WHERE te.event_type = 'delivered'
			  AND te.sending_domain IN (%s)
			  AND te.event_at >= NOW() - INTERVAL '%d days'
			GROUP BY LOWER(s.email)
			HAVING COUNT(DISTINCT te.campaign_id) FILTER (WHERE te.campaign_id IS NOT NULL) >= %s
		),
		opened AS (
			SELECT DISTINCT LOWER(s.email) AS email
			FROM mailing_tracking_events te
			JOIN mailing_subscribers s ON s.id = te.subscriber_id
			WHERE te.event_type = 'opened'
			  AND te.sending_domain IN (%s)
			  AND te.event_at >= NOW() - INTERVAL '%d days'
		)
		SELECT d.email FROM delivered d
		LEFT JOIN opened o ON o.email = d.email
		WHERE o.email IS NULL
	`, strings.Join(domainParams, ","), w.lookbackDays, minSendsParam,
		strings.Join(domainParams, ","), w.lookbackDays)

	rows, err := w.db.QueryContext(queryCtx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("exhausted query: %w", err)
	}
	defer rows.Close()

	var emails []string
	for rows.Next() {
		var email string
		if rows.Scan(&email) == nil {
			emails = append(emails, email)
		}
	}
	return emails, rows.Err()
}

func (w *SuppressionListWorker) circuitBreakerCheck(ctx context.Context, brand string, newCount int) bool {
	var lastCount int
	err := w.db.QueryRowContext(ctx, `
		SELECT COALESCE(suppressed_count, 0)
		FROM suppression_list_runs
		WHERE brand = $1 AND status = 'completed'
		ORDER BY completed_at DESC
		LIMIT 1
	`, brand).Scan(&lastCount)

	if err != nil {
		// First run — no baseline, allow it
		log.Printf("[suppression-list] %s: first run, no baseline — proceeding with %d", brand, newCount)
		return true
	}

	if lastCount == 0 {
		return true
	}

	growthPct := float64(newCount-lastCount) / float64(lastCount) * 100
	maxGrowthPct := w.circuitBreakerPct * 100
	if growthPct > maxGrowthPct {
		log.Printf("[suppression-list] %s: CIRCUIT BREAKER TRIPPED — new=%d, last=%d, growth=%.1f%% exceeds %.0f%% threshold. Skipping this brand.",
			brand, newCount, lastCount, growthPct, maxGrowthPct)
		return false
	}

	return true
}

func (w *SuppressionListWorker) ensureSuppressionList(ctx context.Context, orgID, name string) (string, error) {
	listID := uuid.New().String()

	var actualID string
	err := w.db.QueryRowContext(ctx, `
		INSERT INTO mailing_lists (id, organization_id, name, list_type, description, created_at, updated_at)
		VALUES ($1, $2, $3, 'suppression', 'Auto-generated sunset suppression list', NOW(), NOW())
		ON CONFLICT (organization_id, name) DO UPDATE SET updated_at = NOW()
		RETURNING id::text
	`, listID, orgID, name).Scan(&actualID)
	if err != nil {
		return "", fmt.Errorf("ensure suppression list %q: %w", name, err)
	}

	return actualID, nil
}

func (w *SuppressionListWorker) insertHashes(ctx context.Context, listID string, emails []string) (int, error) {
	const batchSize = 500
	var totalInserted int

	for i := 0; i < len(emails); i += batchSize {
		if ctx.Err() != nil {
			return totalInserted, ctx.Err()
		}

		end := i + batchSize
		if end > len(emails) {
			end = len(emails)
		}
		batch := emails[i:end]

		tx, err := w.db.BeginTx(ctx, nil)
		if err != nil {
			return totalInserted, fmt.Errorf("begin tx: %w", err)
		}

		batchInserted := 0
		for _, email := range batch {
			hash := md5.Sum([]byte(email))
			md5Hex := hex.EncodeToString(hash[:])
			entryID := uuid.New().String()

			result, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source, created_at)
				VALUES ($1, $2, $3, $4, 'sunset_exhausted', 'suppression_worker', NOW())
				ON CONFLICT (list_id, md5_hash) DO NOTHING
			`, entryID, listID, email, md5Hex)
			if err != nil {
				if rbErr := tx.Rollback(); rbErr != nil {
					log.Printf("[suppression-list] rollback failed: %v (original error: %v)", rbErr, err)
				}
				return totalInserted, fmt.Errorf("insert hash (batch offset %d): %w", i, err)
			}
			if rows, _ := result.RowsAffected(); rows > 0 {
				batchInserted++
			}
		}

		if err := tx.Commit(); err != nil {
			return totalInserted, fmt.Errorf("commit (batch offset %d): %w", i, err)
		}
		totalInserted += batchInserted
	}

	return totalInserted, nil
}

// parsePostgresTextArray parses a PostgreSQL text array literal like {a,b,c}.
func parsePostgresTextArray(s string) []string {
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(p, "\" ")
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func brandNames(brands []brandInfo) []string {
	names := make([]string, len(brands))
	for i, b := range brands {
		names[i] = b.baseDomain
	}
	return names
}
