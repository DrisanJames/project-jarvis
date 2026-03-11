// +build ignore

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping: %v", err)
	}

	ctx := context.Background()

	// 1. Clear all generated forecasts
	result1, err := db.ExecContext(ctx, `DELETE FROM agent_campaign_recommendations`)
	if err != nil {
		log.Fatalf("Failed to clear forecasts: %v", err)
	}
	n1, _ := result1.RowsAffected()
	fmt.Printf("Cleared %d forecast recommendations\n", n1)

	// 2. Cancel scheduled campaigns for tomorrow (UTC)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	result2, err := db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
		WHERE status IN ('scheduled', 'preparing')
		  AND scheduled_at IS NOT NULL
		  AND (scheduled_at AT TIME ZONE 'UTC')::date = $1::date
	`, tomorrow)
	if err != nil {
		log.Fatalf("Failed to cancel tomorrow's campaigns: %v", err)
	}
	n2, _ := result2.RowsAffected()
	fmt.Printf("Cancelled %d campaigns scheduled for %s\n", n2, tomorrow)

	// Also cancel queued items for those campaigns
	result3, err := db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue
		SET status = 'cancelled', updated_at = NOW()
		WHERE campaign_id IN (
			SELECT id FROM mailing_campaigns
			WHERE status = 'cancelled' AND completed_at > NOW() - INTERVAL '1 minute'
		)
		AND status IN ('queued', 'paused')
	`)
	if err != nil {
		log.Printf("Warning: failed to cancel queue items: %v", err)
	} else {
		n3, _ := result3.RowsAffected()
		if n3 > 0 {
			fmt.Printf("Cancelled %d queued items\n", n3)
		}
	}

	fmt.Println("Done.")
}
