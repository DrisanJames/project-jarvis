package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

func main() {
	log.Println("╔════════════════════════════════════════════════════════════╗")
	log.Println("║  WARNING: This is a STUB API for local testing ONLY.      ║")
	log.Println("║  All responses are HARDCODED placeholders.                ║")
	log.Println("║                                                           ║")
	log.Println("║  For the REAL server, run:                                ║")
	log.Println("║    go run cmd/server/main.go                              ║")
	log.Println("╚════════════════════════════════════════════════════════════╝")
	log.Println("")
	log.Println("Starting IGNITE STUB API (hardcoded responses)...")

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

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}
	log.Println("Connected to database")

	// Create HTTP mux
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"healthy","service":"ignite-stub-api","warning":"THIS IS A STUB - responses are hardcoded"}`))
	})

	// Metrics endpoint (placeholder)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("# HELP ignite_api_requests_total Total API requests\n"))
		w.Write([]byte("# TYPE ignite_api_requests_total counter\n"))
		w.Write([]byte("ignite_api_requests_total 0\n"))
	})

	// Initialize mailing service
	// store := mailing.NewStore(db)
	// sparkpostKey := os.Getenv("SPARKPOST_API_KEY")
	// trackingURL := os.Getenv("TRACKING_URL")
	// trackingSecret := os.Getenv("TRACKING_SECRET")
	// sender := mailing.NewSender(store, sparkpostKey)
	// tracking := mailing.NewTrackingService(store, trackingSecret, trackingURL)
	// ai := mailing.NewAIService(store)
	// handlers := mailing.NewHandlers(store, sender, tracking, ai)
	// handlers.RegisterRoutes(mux)

	// API routes placeholder
	mux.HandleFunc("GET /api/mailing/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"overview": {
				"total_subscribers": 125000,
				"total_lists": 12,
				"total_campaigns": 45,
				"daily_capacity": 500000,
				"daily_used": 125000
			},
			"performance": {
				"total_sent": 1250000,
				"total_opens": 187500,
				"total_clicks": 31250,
				"total_revenue": 15625.00,
				"open_rate": 15.0,
				"click_rate": 2.5
			},
			"recent_campaigns": []
		}`))
	})

	mux.HandleFunc("GET /api/mailing/lists", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"lists": [], "total": 0}`))
	})

	mux.HandleFunc("GET /api/mailing/campaigns", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"campaigns": [], "total": 0}`))
	})

	mux.HandleFunc("GET /api/mailing/sending-plans", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"plans": [
				{
					"time_period": "morning",
					"name": "Morning Focus",
					"description": "Concentrated morning send targeting high-engagement subscribers",
					"recommended_volume": 50000,
					"time_slots": [],
					"audience_breakdown": [],
					"offer_recommendations": [],
					"predictions": {
						"estimated_opens": 8500,
						"estimated_clicks": 1275,
						"estimated_revenue": 637.50,
						"estimated_bounce_rate": 1.2,
						"estimated_complaint_rate": 0.03,
						"revenue_range": [510.00, 765.00],
						"confidence_interval": 0.85
					},
					"confidence_score": 0.88,
					"ai_explanation": "Morning sends historically show 30% higher engagement. This plan focuses on your most engaged subscribers during peak morning hours.",
					"warnings": [],
					"recommendations": ["Ideal for time-sensitive offers", "Best for high-value content"]
				}
			],
			"target_date": "2026-02-01",
			"generated_at": "2026-02-01T10:00:00Z"
		}`))
	})

	// CORS middleware
	handler := corsMiddleware(mux)

	// Create server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:         "0.0.0.0:" + port,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		log.Printf("Server listening on :%s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server stopped")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Organization-ID")
		w.Header().Set("X-Server-Identity", "ignite-stub-api")
		w.Header().Set("X-Server-Warning", "STUB - hardcoded responses only")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
