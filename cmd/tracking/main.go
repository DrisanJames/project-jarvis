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

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/ignite/sparkpost-monitor/internal/buildinfo"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
	_ "github.com/lib/pq"
)

// openTrackingDB opens the optional Postgres handle the smart-link dictionary
// reads from. DATABASE_URL is OPTIONAL: if unset — or if the open/ping fails —
// the service logs a clear warning and runs with a nil-db dictionary, still
// serving /track/* and the /o/ brand-root fallback exactly as before. The
// tracking service must never fail to boot over a missing/degraded DB.
func openTrackingDB() *sql.DB {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Printf("DATABASE_URL not set — smart-link dictionary runs empty (offer /o/ links fall back to brand root)")
		return nil
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Printf("WARNING: DATABASE_URL set but sql.Open failed (%v) — smart-link dictionary runs empty", err)
		return nil
	}
	// Small read-only pool: this handle only serves the ~60s dictionary reload.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Printf("WARNING: DATABASE_URL set but ping failed (%v) — smart-link dictionary runs empty", err)
		db.Close()
		return nil
	}
	log.Printf("smart-link dictionary DB connected")
	return db
}

func main() {
	bi := buildinfo.Current()
	log.Printf("tracking build info: version=%s git_sha=%s image_digest=%s build_time=%s", bi.Version, bi.GitSHA, bi.ImageDigest, bi.BuildTime)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	queueURL := os.Getenv("SQS_TRACKING_QUEUE_URL")
	if queueURL == "" {
		log.Fatal("SQS_TRACKING_QUEUE_URL is required")
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}

	sqsClient := sqs.NewFromConfig(awsCfg)
	pub := tracking.NewPublisher(sqsClient, queueURL)

	// Optional smart-link dictionary. Graceful default: db may be nil (no
	// DATABASE_URL / open failed), in which case the dictionary is an empty
	// no-op and /o/ links fall back to brand root.
	db := openTrackingDB()
	if db != nil {
		defer db.Close()
	}
	dict := tracking.NewSmartLinkDictionary(db, 60*time.Second)
	defer dict.Close() // cancels the background refresh goroutine on shutdown

	// Offer gateway IP classification (internal/tracking/gateway.go), reading
	// ignite_ip_classification through the SAME optional handle: db may be nil,
	// in which case the classifier is an empty no-op and every click forwards.
	// SHIPPED OFF — even fully loaded it withholds nothing unless GATEWAY_ENFORCE
	// is armed; GATEWAY_DISABLED turns it off entirely. Both are read per
	// request, so either flips without a deploy.
	//
	// Gate 2 (session-shape fanout) rides the same object and is ALSO shipped
	// off: GATEWAY_FANOUT_ENABLED arms it, GATEWAY_FANOUT_LINKS (default 4) and
	// GATEWAY_FANOUT_WINDOW_S (default 60) tune it, all read per request. Its
	// counter is process-local — see the scaling note in gateway.go.
	ipc := tracking.NewIPClassifier(db, 15*time.Minute)
	defer ipc.Close()

	handler := tracking.NewHandlerWithClassifier(pub, dict, ipc)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler.Routes(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("tracking service listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down tracking service...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
