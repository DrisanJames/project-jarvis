package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	_ "github.com/lib/pq"
)

func main() {
	log.Println("Starting IGNITE Send Worker...")

	// Database connection
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ignite:ignite_dev_password@localhost:5432/ignite?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Configure connection pool for 50M/day capacity
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(1 * time.Minute)

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}
	log.Println("Connected to database")

	// Initialize services
	sparkpostKey := os.Getenv("SPARKPOST_API_KEY")
	trackingURL := os.Getenv("TRACKING_URL")
	signingKey := os.Getenv("SIGNING_KEY")
	
	if trackingURL == "" {
		trackingURL = "https://track.ignite.media"
	}
	if signingKey == "" {
		signingKey = "ignite-signing-key-dev"
	}

	// Initialize email sender
	emailSender := mailing.NewEmailSender(db, sparkpostKey, trackingURL, signingKey)
	log.Println("Email sender initialized")

	// Create cancellable context
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	// Initialize journey executor
	journeyExecutor := worker.NewJourneyExecutor(db)
	journeyExecutor.SetEmailSender(func(ctx context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
		// Wrap the EmailSender to match the expected signature
		// Use a nil UUID for journey-triggered emails (no campaign association)
		result, err := emailSender.SendEmail(ctx, email, fromEmail, fromName, subject, htmlContent, "", uuid.Nil)
		if err != nil {
			return err
		}
		if !result.Success {
			if result.Suppressed {
				log.Printf("JourneyExecutor: Email suppressed for %s: %s", email, result.Reason)
				return nil // Don't treat suppression as error
			}
			if result.Throttled {
				return fmt.Errorf("email throttled: %s", result.Reason)
			}
			return fmt.Errorf("email send failed: %s", result.Reason)
		}
		log.Printf("JourneyExecutor: Email sent to %s (ID: %s)", email, result.EmailID)
		return nil
	})
	go journeyExecutor.Start()
	log.Println("Journey executor started")

	// Start Queue Recovery Worker (reclaims stuck items from crashed workers)
	queueRecovery := worker.NewQueueRecoveryWorker(db)
	go queueRecovery.Start(ctx)
	log.Println("Queue Recovery Worker started (scans every 2m for stuck items, max 5 retries)")

	// Start Data Cleanup Worker (removes old queue items, tracking events, agent decisions)
	dataCleanup := worker.NewDataCleanupWorker(db)
	go dataCleanup.Start(ctx)
	log.Println("Data Cleanup Worker started (runs every 1h, batch deletes old data)")

	// Simplified worker loop for demo (heartbeat)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Println("Worker heartbeat - services running...")
			}
		}
	}()

	log.Println("Worker running...")

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down worker...")
	cancel()

	// Stop journey executor gracefully
	log.Println("Stopping journey executor...")
	journeyExecutor.Stop()

	// Give any remaining operations time to finish
	time.Sleep(2 * time.Second)

	log.Println("Worker stopped")
}
