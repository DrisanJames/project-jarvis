package ongage

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// fetchVolumeViaContactActivity uses the Ongage Contact Activity async API to
// get exact per-DATA_SET send volumes. This is the most accurate strategy
// because it queries at the contact level with data_set as a selected field,
// rather than relying on segment or list naming conventions.
//
// Flow:
//  1. POST to create an async report filtered by data_set notempty
//  2. Poll GET until status=2 (Completed)
//  3. GET the CSV export
//  4. Parse CSV, aggregate sent counts by data_set value
//  5. DELETE the report (cleanup)
func (c *Collector) fetchVolumeViaContactActivity(ctx context.Context, from, to time.Time) (map[string]int64, error) {
	// Step 1: Create the contact activity report
	req := ContactActivityRequest{
		Title:          fmt.Sprintf("DS Volume %s to %s", from.Format("2006-01-02"), to.Format("2006-01-02")),
		SelectedFields: []string{"DATA_SET", "sent"},
		Filters: ContactActivityFilters{
			Criteria: []ContactActivityCriterion{
				{
					FieldName:     "DATA_SET",
					Type:          "string",
					Operator:      "notempty",
					Operand:       []string{},
					CaseSensitive: 0, // notempty operator does not allow case_sensitive
					Condition:     "and",
				},
			},
			UserType: "all",
			FromDate: from.Unix(),
			ToDate:   to.Add(24*time.Hour - time.Second).Unix(), // End of day
		},
		CombinedAsAnd: true,
	}

	reportID, err := c.client.CreateContactActivityReport(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create contact activity report: %w", err)
	}
	log.Printf("Ongage: Created contact activity report %s for %s to %s",
		reportID, from.Format("2006-01-02"), to.Format("2006-01-02"))

	// Always attempt cleanup when we're done, regardless of outcome.
	// Use a fresh background context for deletion in case the main ctx was cancelled.
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if delErr := c.client.DeleteContactActivityReport(cleanupCtx, reportID); delErr != nil {
			log.Printf("Ongage: Warning: failed to delete contact activity report %s: %v", reportID, delErr)
		} else {
			log.Printf("Ongage: Deleted contact activity report %s (cleanup)", reportID)
		}
	}()

	// Step 2: Poll for completion (every 30s, up to 30 minutes)
	// This is a once-daily job running in a background goroutine. The report can
	// take 15-20 minutes to build for large lists. We poll slowly (30s) to avoid
	// hitting Ongage's 1000 calls/min rate limit.
	pollInterval := 30 * time.Second
	maxWait := 30 * time.Minute
	deadline := time.Now().Add(maxWait)
	rateLimitRetries := 0

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("contact activity report %s timed out after %s", reportID, maxWait)
		}

		status, err := c.client.GetContactActivityStatus(ctx, reportID)
		if err != nil {
			// Handle rate limiting: wait longer and retry
			if strings.Contains(err.Error(), "429") {
				rateLimitRetries++
				backoff := time.Duration(rateLimitRetries) * 60 * time.Second
				if backoff > 3*time.Minute {
					backoff = 3 * time.Minute
				}
				log.Printf("Ongage: Rate limited polling report %s, backing off %s (retry %d)",
					reportID, backoff, rateLimitRetries)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("poll contact activity status for %s: %w", reportID, err)
		}
		rateLimitRetries = 0

		if status == 2 {
			log.Printf("Ongage: Contact activity report %s completed", reportID)
			break
		}

		log.Printf("Ongage: Contact activity report %s status=%d (pending), waiting %s...",
			reportID, status, pollInterval)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while waiting for report %s: %w", reportID, ctx.Err())
		case <-time.After(pollInterval):
			// continue polling
		}
	}

	// Step 3: Export the CSV
	csvData, err := c.client.ExportContactActivityCSV(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("export contact activity CSV for %s: %w", reportID, err)
	}
	log.Printf("Ongage: Exported contact activity CSV (%d bytes)", len(csvData))

	// Step 4: Parse CSV and aggregate sent by data_set
	reader := csv.NewReader(bytes.NewReader(csvData))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse contact activity CSV: %w", err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("contact activity CSV has no data rows (only %d records)", len(records))
	}

	// Find column indices (case-insensitive)
	header := records[0]
	dsCol := -1
	sentCol := -1
	for i, col := range header {
		switch strings.ToLower(strings.TrimSpace(col)) {
		case "data_set":
			dsCol = i
		case "sent":
			sentCol = i
		}
	}
	if dsCol < 0 {
		return nil, fmt.Errorf("contact activity CSV missing 'data_set' column (headers: %v)", header)
	}
	if sentCol < 0 {
		return nil, fmt.Errorf("contact activity CSV missing 'sent' column (headers: %v)", header)
	}

	result := make(map[string]int64)
	var totalVolume int64
	var skippedRows int

	for _, row := range records[1:] {
		if len(row) <= dsCol || len(row) <= sentCol {
			skippedRows++
			continue
		}

		dsValue := strings.ToUpper(strings.TrimSpace(row[dsCol]))
		if dsValue == "" {
			skippedRows++
			continue
		}

		sentValue, err := strconv.ParseInt(strings.TrimSpace(row[sentCol]), 10, 64)
		if err != nil || sentValue <= 0 {
			skippedRows++
			continue
		}

		result[dsValue] += sentValue
		totalVolume += sentValue
	}

	// Step 5: (cleanup is handled by defer above)

	log.Printf("Ongage: Contact activity CSV parsed: %d data-set codes, %d total volume, %d rows skipped",
		len(result), totalVolume, skippedRows)

	return result, nil
}
