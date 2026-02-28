package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BulkEnqueuer handles high-speed queue insertion using PostgreSQL COPY
// Designed for 50,000+ inserts/second
type BulkEnqueuer struct {
	db *sql.DB

	// Stats
	totalEnqueued int64
	totalFailed   int64
}

// NewBulkEnqueuer creates a new bulk enqueuer
func NewBulkEnqueuer(db *sql.DB) *BulkEnqueuer {
	return &BulkEnqueuer{db: db}
}

// Subscriber represents a subscriber to be enqueued
type EnqueueSubscriber struct {
	ID              uuid.UUID
	Email           string
	FirstName       string
	LastName        string
	CustomFields    map[string]interface{}
}

// EnqueueCampaign bulk-inserts subscribers into the queue using COPY
// This is 100x faster than individual INSERT statements
func (e *BulkEnqueuer) EnqueueCampaign(ctx context.Context, campaignID uuid.UUID, subscribers []EnqueueSubscriber, priority int) (int, error) {
	if len(subscribers) == 0 {
		return 0, nil
	}

	startTime := time.Now()
	log.Printf("[BulkEnqueuer] Starting enqueue of %d subscribers for campaign %s", len(subscribers), campaignID)

	// Use COPY for bulk insert
	txn, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer txn.Rollback()

	// Prepare COPY statement
	stmt, err := txn.Prepare(pq.CopyIn(
		"mailing_campaign_queue_v2",
		"id", "campaign_id", "subscriber_id", "email",
		"substitution_data", "status", "priority",
		"scheduled_at", "created_at",
	))
	if err != nil {
		return 0, fmt.Errorf("failed to prepare COPY: %w", err)
	}

	now := time.Now()
	successCount := 0

	for _, sub := range subscribers {
		// Build substitution data (personalization)
		substitutionData := map[string]interface{}{
			"first_name": sub.FirstName,
			"last_name":  sub.LastName,
			"email":      sub.Email,
		}
		// Merge custom fields
		for k, v := range sub.CustomFields {
			substitutionData[k] = v
		}

		subDataJSON, err := json.Marshal(substitutionData)
		if err != nil {
			log.Printf("[BulkEnqueuer] Warning: failed to marshal substitution data for %s: %v", logger.RedactEmail(sub.Email), err)
			subDataJSON = []byte("{}")
		}

		_, err = stmt.Exec(
			uuid.New(),     // id
			campaignID,     // campaign_id
			sub.ID,         // subscriber_id
			sub.Email,      // email
			string(subDataJSON), // substitution_data
			"queued",       // status
			priority,       // priority
			now,            // scheduled_at
			now,            // created_at
		)
		if err != nil {
			log.Printf("[BulkEnqueuer] Warning: failed to exec for %s: %v", logger.RedactEmail(sub.Email), err)
			atomic.AddInt64(&e.totalFailed, 1)
			continue
		}
		successCount++
	}

	// Flush the COPY
	_, err = stmt.Exec()
	if err != nil {
		return 0, fmt.Errorf("failed to flush COPY: %w", err)
	}

	err = stmt.Close()
	if err != nil {
		return 0, fmt.Errorf("failed to close COPY statement: %w", err)
	}

	// Commit transaction
	err = txn.Commit()
	if err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	atomic.AddInt64(&e.totalEnqueued, int64(successCount))

	elapsed := time.Since(startTime)
	rate := float64(successCount) / elapsed.Seconds()
	log.Printf("[BulkEnqueuer] Enqueued %d subscribers in %v (%.0f/sec)", successCount, elapsed, rate)

	return successCount, nil
}

// EnqueueCampaignFromQuery bulk-inserts subscribers from a SQL query result
// This is the most efficient method for large segments
func (e *BulkEnqueuer) EnqueueCampaignFromQuery(ctx context.Context, campaignID uuid.UUID, query string, args []interface{}, priority int) (int, error) {
	startTime := time.Now()
	log.Printf("[BulkEnqueuer] Starting query-based enqueue for campaign %s", campaignID)

	// Execute the subscriber query
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to execute subscriber query: %w", err)
	}
	defer rows.Close()

	// Start COPY transaction
	txn, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer txn.Rollback()

	stmt, err := txn.Prepare(pq.CopyIn(
		"mailing_campaign_queue_v2",
		"id", "campaign_id", "subscriber_id", "email",
		"substitution_data", "status", "priority",
		"scheduled_at", "created_at",
	))
	if err != nil {
		return 0, fmt.Errorf("failed to prepare COPY: %w", err)
	}

	now := time.Now()
	successCount := 0
	batchCount := 0

	for rows.Next() {
		var subID uuid.UUID
		var email, firstName, lastName string
		var customFieldsJSON []byte

		err := rows.Scan(&subID, &email, &firstName, &lastName, &customFieldsJSON)
		if err != nil {
			log.Printf("[BulkEnqueuer] Warning: failed to scan row: %v", err)
			continue
		}

		// Build substitution data
		substitutionData := map[string]interface{}{
			"first_name": firstName,
			"last_name":  lastName,
			"email":      email,
		}

		// Parse custom fields if present
		if len(customFieldsJSON) > 0 {
			var customFields map[string]interface{}
			if err := json.Unmarshal(customFieldsJSON, &customFields); err == nil {
				for k, v := range customFields {
					substitutionData[k] = v
				}
			}
		}

		subDataJSON, _ := json.Marshal(substitutionData)

		_, err = stmt.Exec(
			uuid.New(),
			campaignID,
			subID,
			email,
			string(subDataJSON),
			"queued",
			priority,
			now,
			now,
		)
		if err != nil {
			atomic.AddInt64(&e.totalFailed, 1)
			continue
		}

		successCount++
		batchCount++

		// Log progress every 50,000 records
		if batchCount >= 50000 {
			log.Printf("[BulkEnqueuer] Progress: %d records enqueued...", successCount)
			batchCount = 0
		}
	}

	// Check for row iteration errors
	if err := rows.Err(); err != nil {
		log.Printf("[BulkEnqueuer] Warning: row iteration error: %v", err)
	}

	// Flush the COPY
	_, err = stmt.Exec()
	if err != nil {
		return 0, fmt.Errorf("failed to flush COPY: %w", err)
	}

	err = stmt.Close()
	if err != nil {
		return 0, fmt.Errorf("failed to close statement: %w", err)
	}

	err = txn.Commit()
	if err != nil {
		return 0, fmt.Errorf("failed to commit: %w", err)
	}

	atomic.AddInt64(&e.totalEnqueued, int64(successCount))

	elapsed := time.Since(startTime)
	rate := float64(successCount) / elapsed.Seconds()
	log.Printf("[BulkEnqueuer] Completed: %d subscribers in %v (%.0f/sec)", successCount, elapsed, rate)

	return successCount, nil
}

// Stats returns current statistics
func (e *BulkEnqueuer) Stats() map[string]int64 {
	return map[string]int64{
		"total_enqueued": atomic.LoadInt64(&e.totalEnqueued),
		"total_failed":   atomic.LoadInt64(&e.totalFailed),
	}
}
