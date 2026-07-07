package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/api"
	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/ignite/sparkpost-monitor/internal/azure"
	"github.com/ignite/sparkpost-monitor/internal/buildinfo"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/datainjections"
	"github.com/ignite/sparkpost-monitor/internal/datanorm"
	"github.com/ignite/sparkpost-monitor/internal/domainagent"
	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/financial"
	"github.com/ignite/sparkpost-monitor/internal/intelligence"
	"github.com/ignite/sparkpost-monitor/internal/kanban"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/notify"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/snowflake"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
	"github.com/ignite/sparkpost-monitor/internal/worker"

	"github.com/google/uuid"
	_ "github.com/lib/pq" // PostgreSQL driver
	"github.com/redis/go-redis/v9"
)

// checkPortAvailable verifies that the target port is not already in use.
// This prevents confusion from stale/stub processes occupying the port.
func checkPortAvailable(host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %d is already in use (addr %s): %v\n"+
			"  Hint: Run 'lsof -i :%d' to find the blocking process,\n"+
			"  or use 'scripts/kill-port.sh %d' to kill it", port, addr, err, port, port)
	}
	ln.Close()
	return nil
}

func extractHost(dsn string) string {
	at := strings.Index(dsn, "@")
	if at < 0 {
		return "(unknown)"
	}
	rest := dsn[at+1:]
	slash := strings.Index(rest, "/")
	if slash >= 0 {
		rest = rest[:slash]
	}
	return rest
}

//go:embed embed/suppress_besmed_bounces.sql
var besmedSuppressionSQL string

func main() {
	log.Println("╔════════════════════════════════════════════════════════════╗")
	log.Println("║  Project Jarvis Production Server (cmd/server/main.go)            ║")
	log.Println("║  Real database-backed API with full ESP integrations      ║")
	log.Println("╚════════════════════════════════════════════════════════════╝")
	bi := buildinfo.Current()
	log.Printf("Build info: version=%s git_sha=%s image_digest=%s build_time=%s", bi.Version, bi.GitSHA, bi.ImageDigest, bi.BuildTime)

	// Load configuration
	cfg, err := config.LoadFromEnv("config/config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if os.Getenv("DATABASE_URL") != "" {
		log.Println("[config] DATABASE_URL env override active")
	}

	// Pre-flight check: verify the target port is available
	host := cfg.Server.GetHost()
	port := cfg.Server.Port
	if port == 0 {
		port = 8080
	}
	if err := checkPortAvailable(host, port); err != nil {
		log.Fatalf("Pre-flight check FAILED: %v", err)
	}
	log.Printf("Pre-flight check passed: port %d is available", port)

	// Initialize storage
	store, err := storage.New(cfg.Storage)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// Initialize SparkPost client
	spClient := sparkpost.NewClient(cfg.SparkPost)

	// Initialize learning agent
	learningAgent := agent.New(cfg.Agent, store)

	// Initialize SparkPost metrics collector
	spCollector := sparkpost.NewCollector(spClient, store, learningAgent, cfg.Polling)

	ctx, cancel := context.WithCancel(context.Background())

	// Initialize authentication manager if enabled
	var authManager *auth.AuthManager
	if cfg.Auth.Enabled && cfg.Auth.GoogleClientID != "" {
		// Determine base URL for OAuth callbacks
		baseURL := fmt.Sprintf("http://%s:%d", cfg.Server.GetHost(), cfg.Server.Port)
		// On ECS, use the production URL
		if os.Getenv("ECS_CONTAINER_METADATA_URI") != "" {
			baseURL = "https://projectjarvis.io"
		}
		// Allow override via environment variable
		if envURL := os.Getenv("AUTH_BASE_URL"); envURL != "" {
			baseURL = envURL
		}

		authManager = auth.NewAuthManager(&cfg.Auth, baseURL)

		// Pre-flight: validate OAuth credentials against Google before accepting traffic.
		// This prevents silent misconfiguration from surfacing only at user login time.
		log.Println("Validating Google OAuth credentials...")
		if err := authManager.ValidateCredentials(context.Background()); err != nil {
			log.Fatalf("OAuth pre-flight FAILED: %v", err)
		}
		log.Println("Google OAuth credentials validated successfully")

		authManager.CleanupExpiredSessions()
		log.Printf("Google OAuth enabled for domain: %s (callback: %s/auth/callback)", cfg.Auth.AllowedDomain, baseURL)
	} else {
		log.Println("Authentication disabled")
	}

	// Initialize and start API server
	var server *api.Server
	if authManager != nil {
		server = api.NewServerWithAuth(cfg.Server, spClient, store, learningAgent, spCollector, authManager)
	} else {
		server = api.NewServer(cfg.Server, spClient, store, learningAgent, spCollector)
	}

	// Set the full config on server for handlers that need it (e.g., IP pool types)
	server.SetConfig(cfg)

	// Start HTTP server IMMEDIATELY so ALB health checks pass while the
	// heavy platform init (AWS credential fetch via IMDS, Redis, S3) runs.
	// Chi v5 does NOT compile/lock the route tree — routes added later are
	// visible to subsequent requests, so mailing routes can be registered
	// after the listener is already accepting connections.
	server.RegisterHealthRoutes()
	log.Println("Health check routes registered: /health, /health/live, /health/ready")

	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Server.GetHost(), cfg.Server.Port)
		log.Printf("Starting server on %s (health routes ready, platform init continues)", addr)
		if err := server.ListenAndServe(addr); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()
	time.Sleep(100 * time.Millisecond)

	var mailingDB *sql.DB
	var redisClient *redis.Client
	dbReachable := false

	if cfg.Mailing.Enabled && cfg.Mailing.DatabaseURL != "" {
		log.Println("Initializing Mailing Platform with PostgreSQL...")

		dbURL := cfg.Mailing.DatabaseURL
		sep := "?"
		if strings.Contains(dbURL, "?") {
			sep = "&"
		}
		if !strings.Contains(dbURL, "connect_timeout") {
			dbURL += sep + "connect_timeout=5"
			sep = "&"
		}
		dbURL += sep + "options=-c%20statement_timeout%3D30000%20-c%20idle_in_transaction_session_timeout%3D30000"
		log.Printf("DB URL host portion: ...@%s/...", extractHost(dbURL))
		var err error
		mailingDB, err = sql.Open("postgres", dbURL)
		if err != nil {
			log.Printf("Warning: Failed to connect to mailing database: %v", err)
			mailingDB = nil
		}
	}

	if mailingDB != nil {
		server.SetOpenAIConfig(cfg.OpenAI)

		{
			imgBucket := os.Getenv("JARVIS_S3_BUCKET")
			if imgBucket == "" && cfg.Storage.S3Bucket != "" {
				imgBucket = cfg.Storage.S3Bucket
			}
			if imgBucket != "" {
				imgRegion := os.Getenv("JARVIS_S3_REGION")
				if imgRegion == "" {
					imgRegion = cfg.Storage.AWSRegion
				}
				if imgRegion == "" {
					imgRegion = "us-west-2"
				}
				awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(imgRegion))
				if err != nil {
					log.Printf("WARNING: Failed to load AWS config for Image CDN: %v", err)
				} else {
					imgS3Client := s3.NewFromConfig(awsCfg)
					imgCDNDomain := os.Getenv("IMAGE_CDN_DOMAIN")
					server.SetImageCDNConfig(imgS3Client, imgBucket, imgCDNDomain, imgRegion)
					log.Printf("Image CDN initialized: bucket=%s, region=%s, cdn=%s", imgBucket, imgRegion, imgCDNDomain)
				}
			}
		}

		{
			suppRegion := "us-west-2"
			suppCfg, suppErr := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(suppRegion))
			if suppErr != nil {
				log.Printf("WARNING: Failed to load AWS config for suppression S3: %v", suppErr)
			} else {
				suppS3Client := s3.NewFromConfig(suppCfg)
				server.SetSuppressionS3Client(api.NewSuppressionS3Client(suppS3Client))
				log.Printf("Suppression S3 initialized: bucket=jarvis-offer-suppressions, region=%s", suppRegion)
			}
		}

		// Partner-Ingest S3 client. Uses PARTNER_INGEST_S3_BUCKET / REGION env
		// vars (defaults: jarvis-partner-ingest / us-west-2). Drives the data
		// partner ingestion pipeline (POST /api/partner-ingest/v1/records).
		{
			partnerRegion := os.Getenv("PARTNER_INGEST_S3_REGION")
			if partnerRegion == "" {
				partnerRegion = "us-west-2"
			}
			partnerCfg, partnerErr := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(partnerRegion))
			if partnerErr != nil {
				log.Printf("WARNING: Failed to load AWS config for partner-ingest S3: %v", partnerErr)
			} else {
				partnerS3Client := s3.NewFromConfig(partnerCfg)
				partnerIngestClient := api.NewPartnerIngestS3Client(partnerS3Client)
				server.SetPartnerIngestS3Client(partnerIngestClient)
				log.Printf("Partner-Ingest S3 initialized: bucket=%s, region=%s", partnerIngestClient.Bucket(), partnerIngestClient.Region())
			}
		}

		redisURL := os.Getenv("REDIS_URL")
		if redisURL == "" {
			redisURL = os.Getenv("REDIS_ADDR")
		}
		if redisURL != "" {
			opts, err := redis.ParseURL(redisURL)
			if err != nil {
				redisClient = redis.NewClient(&redis.Options{Addr: redisURL})
			} else {
				redisClient = redis.NewClient(opts)
			}
			pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
			if err := redisClient.Ping(pingCtx).Err(); err != nil {
				log.Printf("Warning: Redis connection failed (%s): %v — falling back to PG advisory locks", redisURL, err)
				redisClient.Close()
				redisClient = nil
			} else {
				server.SetRedisClient(redisClient)
				if authManager != nil {
					authManager.SetRedisClient(redisClient)
				}
				log.Printf("Redis connected: %s (distributed locking + persistent sessions enabled)", redisURL)
			}
			pingCancel()
		} else {
			log.Println("Redis not configured (REDIS_URL not set) — using PG advisory locks for distributed locking")
		}

		mailingDB.SetMaxOpenConns(40)
		mailingDB.SetMaxIdleConns(25)
		mailingDB.SetConnMaxLifetime(5 * time.Minute)
		mailingDB.SetConnMaxIdleTime(2 * time.Minute)

		server.SetShutdownContext(ctx)
		// Wire partner drip factory before SetMailingDB — route registration can
		// take >10 minutes; the orchestrator starts from SetMailingDB as soon as
		// PMTACampaignService exists (see startPartnerDripOrchestrator).
		if server.GetPartnerIngestS3Client() != nil {
			server.SetPartnerDripStarter(func(db *sql.DB, pmta *api.PMTACampaignService) interface{ Stop() } {
				pausedBrandFn := func(ctx context.Context, brand string) bool {
					brandDomain, ok := worker.BrandSendingDomain(brand)
					if !ok || brandDomain == "" {
						return true
					}
					var n int
					_ = db.QueryRowContext(ctx,
						`SELECT COUNT(*) FROM mailing_sending_profiles
						 WHERE sending_domain = $1 AND status = 'active' AND vendor_type = 'pmta'`,
						brandDomain).Scan(&n)
					return n == 0
				}
				followupDisabled := os.Getenv("PARTNER_DRIP_FOLLOWUP_DISABLED") == "1"
				followupMax := 5000
				if followupDisabled {
					followupMax = 0
				}
				throttleDeferralDisabled := false
				throttleThreshold := 50.0
				if v := strings.TrimSpace(os.Getenv("PARTNER_DRIP_THROTTLE_THRESHOLD")); v != "" {
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						throttleThreshold = f
						if f <= 0 {
							throttleDeferralDisabled = true
						}
					}
				}
				creativesDir := strings.TrimSpace(os.Getenv("PARTNER_DRIP_CREATIVES_DIR"))
				if creativesDir == "" {
					creativesDir = "docs/emails"
				}
				orch := worker.NewPartnerDripOrchestrator(db, worker.PartnerDripOrchestratorConfig{
					OrganizationID:              "00000000-0000-0000-0000-000000000001",
					DeployFn:                    worker.WrapPMTACampaignDeploy(pmta.HandleDeployCampaign),
					PausedBrandPredicate:        pausedBrandFn,
					MaxFollowupClaimPerVertical: followupMax,
					FollowupDisabled:            followupDisabled,
					ThrottledISPRateThreshold:   throttleThreshold,
					ThrottleDeferralDisabled:    throttleDeferralDisabled,
					CreativesDir:                creativesDir,
				})
				orch.Start()
				log.Printf("[PartnerDripOrchestrator] started (followup_disabled=%v followup_max=%d throttle_deferral_disabled=%v throttle_threshold=%.0f creatives_dir=%s)",
					followupDisabled, followupMax, throttleDeferralDisabled, throttleThreshold, creativesDir)
				return orch
			})
		}
		// Route registration is heavy (many handlers + synchronous service
		// init). Do not block send-worker startup on it — workers only need
		// the *sql.DB handle, which is already open above.
		go func() {
			server.SetMailingDB(mailingDB)
			log.Println("Mailing Platform routes registered")
		}()
	}

	// Read replica pool for analytical workloads (SegmentRefreshWorker).
	// Falls back to primary when READ_REPLICA_URL is not configured.
	var readDB *sql.DB
	replicaConfigured := strings.TrimSpace(cfg.Mailing.ReadReplicaURL) != ""
	if replicaConfigured {
		rURL := cfg.Mailing.ReadReplicaURL
		sep := "?"
		if strings.Contains(rURL, "?") {
			sep = "&"
		}
		if !strings.Contains(rURL, "connect_timeout") {
			rURL += sep + "connect_timeout=5"
			sep = "&"
		}
		rURL += sep + "options=-c%20statement_timeout%3D120000"
		var err error
		readDB, err = sql.Open("postgres", rURL)
		if err != nil {
			log.Printf("READ_REPLICA_URL set but unreachable — falling back to primary (open failed: %v)", err)
			readDB = nil
		} else {
			readDB.SetMaxOpenConns(15)
			readDB.SetMaxIdleConns(8)
			readDB.SetConnMaxLifetime(5 * time.Minute)
			readDB.SetConnMaxIdleTime(2 * time.Minute)
			pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
			pingErr := readDB.PingContext(pingCtx)
			pingCancel()
			if pingErr != nil {
				log.Printf("READ_REPLICA_URL set but unreachable — falling back to primary (ping failed: %v)", pingErr)
				readDB.Close()
				readDB = nil
			} else {
				log.Println("Read replica pool initialized — replica healthy")
			}
		}
	}
	if readDB == nil {
		readDB = mailingDB
		if mailingDB != nil {
			if replicaConfigured {
				log.Println("READ_REPLICA_URL set but unreachable — falling back to primary")
			} else {
				log.Println("READ_REPLICA_URL not set — SegmentRefreshWorker using primary")
			}
		}
	}

	// Kafka event-backbone handle (SA-6). Stays nil/dark until wireEventBus runs
	// below; its Stop() is registered on the shutdown path. A nil handle Stops
	// safely, so the dark default (KAFKA_BROKERS unset) needs no special casing.
	var eventBus *eventBusHandle

	// ── Run migrations and start workers (server is already serving) ──
	if mailingDB != nil {
		pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
		if err := mailingDB.PingContext(pingCtx); err != nil {
			pingCancel()
			log.Printf("Warning: Mailing database ping failed: %v — workers skipped", err)
		} else {
			pingCancel()
			dbReachable = true
			log.Println("Mailing Platform database connected successfully")
			// Migrations can include multi-minute CREATE INDEX CONCURRENTLY on
			// million-row tables. Run them in the background so worker startup
			// (send path, wave dispatcher) is not blocked during deploys.
			// Send-path-critical schema lands SYNCHRONOUSLY, before any
			// worker goroutine exists — the claim SQL references it
			// unconditionally, so the binary must never outrun it
			// (2026-06-10 AAR action item 4).
			ensureSendPathSchema(mailingDB)
			go func() {
				runAdminMigrations()
				// Drift guard: compare the LIVE verdict-function bodies against
				// the committed source BEFORE runStartupMigrations CREATE OR
				// REPLACEs them, so a boot never silently reverts a hot-patch.
				checkVerdictFunctionDrift(mailingDB)
				runStartupMigrations(mailingDB)
				seedProcessDefaultOrgID(mailingDB)
				// Long-running CONCURRENTLY builds live outside the
				// 5s-per-statement migration runner: they wait for a
				// calm-IO window and never block writes.
				ensureConcurrentIndexes(mailingDB)
				// Materialize click_verdict for historical clicked rows in
				// calm-IO-gated windows (never the 5s slice). Idempotent,
				// resumable, kill: DISABLE_CLICK_VERDICT_BACKFILL=1.
				backfillClickVerdict(ctx, mailingDB)
			}()
		}

		// Load offer suppression Bloom filters from S3 at startup.
		if server.OfferSuppMgr != nil {
			go func() {
				loadCtx, loadCancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer loadCancel()
				if err := server.OfferSuppMgr.LoadActiveOffers(loadCtx); err != nil {
					log.Printf("WARNING: Bloom filter startup load failed: %v — falling back to DB queries", err)
				}
			}()
		}

		if dbReachable {

			// Start Backpressure Monitor
			backpressure := worker.NewBackpressureMonitor(mailingDB, 100000)
			go backpressure.Start(ctx)
			worker.SetPackageBackpressure(backpressure)
			log.Println("Backpressure Monitor started (threshold: 100,000, check every 30s)")

			// Start Campaign Scheduler Worker (polls for scheduled campaigns and enqueues them)
			campaignScheduler := worker.NewCampaignScheduler(mailingDB)
			campaignScheduler.SetBackpressure(backpressure)
			if redisClient != nil {
				campaignScheduler.SetRedisClient(redisClient)
			}
			if err := campaignScheduler.Start(); err != nil {
				log.Printf("Warning: Failed to start Campaign Scheduler: %v", err)
			} else {
				log.Println("Campaign Scheduler Worker started (polls every 30s for scheduled campaigns)")
			}

			// Start PMTA ISP wave scheduler / consumer.
			pmtaWaveQueueURL := os.Getenv("SQS_PMTA_WAVE_QUEUE_URL")
			var pmtaWaveSQSClient *sqs.Client
			// Hoisted outside the if-block so the cross-brand cap
			// wiring downstream can reach it via SetCapChecker.
			var pmtaWaveConsumer *worker.PMTAWaveConsumer
			if pmtaWaveQueueURL != "" {
				awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
				if err != nil {
					log.Printf("Warning: AWS config for PMTA wave SQS failed: %v", err)
				} else {
					pmtaWaveSQSClient = sqs.NewFromConfig(awsCfg)
					pmtaWaveConsumer = worker.NewPMTAWaveConsumer(pmtaWaveSQSClient, pmtaWaveQueueURL, mailingDB)
					pmtaWaveConsumer.Start(ctx)
					log.Printf("PMTA wave consumer started (queue=%s)", pmtaWaveQueueURL)
				}
			}
			pmtaWaveScheduler := worker.NewPMTAWaveScheduler(mailingDB, pmtaWaveSQSClient, pmtaWaveQueueURL)
			if redisClient != nil {
				pmtaWaveScheduler.SetRedisClient(redisClient)
			}
			if err := pmtaWaveScheduler.Start(); err != nil {
				log.Printf("Warning: Failed to start PMTA wave scheduler: %v", err)
			} else if pmtaWaveQueueURL != "" {
				log.Printf("PMTA wave scheduler started (queue=%s)", pmtaWaveQueueURL)
			} else {
				log.Println("PMTA wave scheduler started (direct DB enqueue fallback)")
			}

			// Start Send Worker Pool (processes the queue and sends emails)
			sendWorkerPool := worker.NewSendWorkerPool(mailingDB, 25)
			profileSender := worker.NewProfileBasedSender(mailingDB)
			sendWorkerPool.SetESPSenders(profileSender, profileSender, profileSender, profileSender)
			sendWorkerPool.SetPMTASender(profileSender)

			trackURL := os.Getenv("TRACKING_URL")
			if trackURL == "" {
				trackURL = "http://localhost:8080"
			}
			trackSecret := os.Getenv("TRACKING_SECRET")
			if trackSecret == "" {
				trackSecret = "ignite-tracking-secret-dev"
			}
			sendWorkerPool.SetTrackingConfig(trackURL, trackSecret, "00000000-0000-0000-0000-000000000001")

			// Start SQS tracking event consumer
			var trackingConsumer *tracking.Consumer
			if sqsQueueURL := os.Getenv("SQS_TRACKING_QUEUE_URL"); sqsQueueURL != "" {
				awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
				if err != nil {
					log.Printf("Warning: AWS config for SQS consumer failed: %v", err)
				} else {
					sqsClient := sqs.NewFromConfig(awsCfg)
					trackingConsumer = tracking.NewConsumer(sqsClient, sqsQueueURL, mailingDB)
					trackingConsumer.Start(ctx)
					log.Printf("SQS Tracking Consumer started (queue=%s)", sqsQueueURL)
				}
			}

			// Wire global suppression hub to send worker pool for bounce recording
			if hub, ok := server.GlobalHub.(worker.GlobalSuppressionChecker); ok {
				sendWorkerPool.SetGlobalSuppressionHub(hub)
			}
			if suppressor, ok := server.GlobalHub.(worker.GlobalSuppressionSuppressor); ok {
				sendWorkerPool.SetGlobalSuppressionWriter(suppressor)
			}

			if rr := server.GetRateRegistry(); rr != nil {
				sendWorkerPool.SetRateRegistry(rr)
				perIPEnabled := os.Getenv("ENABLE_PER_IP_RATE_LIMITING") == "true"
				sendWorkerPool.SetPerIPRateLimiting(perIPEnabled)
				disableRateLimiting := os.Getenv("DISABLE_ISP_RATE_LIMITING") == "true"
				sendWorkerPool.SetRateLimitingDisabled(disableRateLimiting)
				profileSender.SetIPChangeCallback(func(_ string, ispGroups map[string][]worker.VMTAInfo) {
					for poolSuffix, entries := range ispGroups {
						isp := engine.ISP(poolSuffix)
						ips := make([]engine.IPEntry, len(entries))
						for i, e := range entries {
							ips[i] = engine.IPEntry{
								Hostname:         e.Hostname,
								Status:           e.Status,
								WarmupDailyLimit: e.WarmupDailyLimit,
							}
						}
						rr.SetIPList(isp, ips)
					}
				})
				log.Printf("ISP rate registry wired to send worker pool (per-IP rate limiting: %v, rate limiting disabled: %v)", perIPEnabled, disableRateLimiting)
			}

			// Wire offer suppression Bloom checker to send worker
			if server.OfferSuppMgr != nil {
				sendWorkerPool.SetOfferSuppressionChecker(server.OfferSuppMgr)
				log.Println("Offer suppression Bloom filter wired to send worker pool")
			}

			// Cross-brand daily cap enforcer (P5a). Redis fast path +
			// Postgres fallback. Default cap=2; org settings override.
			// Disable entirely with DISABLE_CROSS_BRAND_CAP=true.
			//
			// The same CapChecker is also wired into the PMTA wave
			// scheduler (cap-aware reserve pool, Slice 4) so the
			// dispatcher can Peek the cap and substitute reserves
			// before the worker layer ever sees over-cap subscribers.
			if os.Getenv("DISABLE_CROSS_BRAND_CAP") != "true" {
				capChecker := mailing.NewCapChecker(mailingDB, redisClient, 2)
				sendWorkerPool.SetCapChecker(capChecker)
				pmtaWaveScheduler.SetCapChecker(capChecker)
				if pmtaWaveConsumer != nil {
					pmtaWaveConsumer.SetCapChecker(capChecker)
				}
				if redisClient != nil {
					log.Println("Cross-brand daily cap wired to send worker pool + PMTA wave dispatcher (Redis fast path + PG fallback)")
				} else {
					log.Println("Cross-brand daily cap wired to send worker pool + PMTA wave dispatcher (PG-only, not race-safe under high concurrency)")
				}
			} else {
				log.Println("Cross-brand daily cap DISABLED via DISABLE_CROSS_BRAND_CAP env var")
			}

			// Durable injection outbox state machine. Controlled by
			// mailing.outbox_mode in config.yaml (or OUTBOX_MODE env var).
			// "legacy" preserves historic pending->sending->sent flow;
			// "durable" activates queued->submitting->accepted with atomic
			// state guards and the X-Ignite-Idempotency-Key header. This
			// is the one-line rollback: flip the flag, restart, done.
			outboxMode := strings.ToLower(strings.TrimSpace(cfg.Mailing.OutboxMode))
			if outboxMode == "" {
				outboxMode = "legacy"
			}
			sendWorkerPool.SetOutboxMode(outboxMode)
			log.Printf("Send worker pool outbox mode: %s", outboxMode)

			sendWorkerPool.Start()

			// SA-7: pure observability — register the wave-processor
			// status endpoint AFTER the send worker pool exists so the
			// handler can read its in-memory throughput. Mounted on
			// the root router (bypasses /api auth) following the same
			// pattern as /api/outbox/summary so operators can curl it
			// during live incidents without auth dance.
			server.RegisterWaveProcessorStatusRoute(sendWorkerPool)
			log.Println("[wave_processor_status] GET /api/wave-processor/status registered")

			// Start Queue Recovery Worker (reclaims stuck items from crashed workers)
			queueRecovery := worker.NewQueueRecoveryWorker(mailingDB)
			go queueRecovery.Start(ctx)
			log.Println("Queue Recovery Worker started (scans every 2m for stuck items, max 5 retries)")

			// Durable-outbox reconciler. Only meaningful when OutboxMode=durable
			// because nothing ever writes 'submitting' in legacy mode. Starting
			// it unconditionally is cheap (single indexed SELECT, no writes if
			// no submitting rows exist) and leaves a safety net in place if
			// durable mode is flipped on without a restart.
			outboxReconciler := worker.NewOutboxReconciler(mailingDB)
			go outboxReconciler.Start(ctx)
			log.Println("Outbox Reconciler started (60s interval, 10m grace, commits crash-window sends + requeues stranded rows)")

			// Outbox summary refresher. Keeps /api/outbox/summary on a warm
			// in-memory cache so dashboard polls never block on the aggregate
			// scans that previously took ~15s against a 1M+ row queue table.
			api.StartOutboxSummaryRefresher(ctx, mailingDB)
			log.Println("Outbox Summary Refresher started (30s cache, decouples dashboard from DB load)")

			// ── Kafka event backbone (SA-6) — DARK BY DEFAULT ──────────────
			// wireEventBus is a no-op unless KAFKA_BROKERS is set (and even
			// then each producer flow + consumer is independently flag-gated,
			// defaults OFF; the suppression projector runs in SHADOW mode and
			// does NOT mutate the live hub). It NEVER touches the send queue,
			// send worker, or schedulers. When KAFKA_BROKERS is unset (today's
			// prod) it logs one "disabled" line and returns a dark handle, and
			// the package-level Publish* taps stay nil — byte-identical
			// behavior. Wired here, after the DB + redis client exist.
			eventBus = wireEventBus(ctx, mailingDB, redisClient)

			// Operational-alert transport. Twilio SMS was retired 2026-06-07
			// (dead credentials → http 401); each operational pager now posts
			// to its OWN Slack channel via the shared SLACK_BOT_TOKEN. The
			// channel is overridable per-pager via env, defaulting to the
			// dedicated channels created 2026-06-07.
			outboxNotifier := notify.SlackChannelFromEnv("SLACK_OUTBOX_CHANNEL", "#outbox-self-check")
			storageNotifier := notify.SlackChannelFromEnv("SLACK_STORAGE_GUARD_CHANNEL", "#storage-guard")

			// Outbox self-check. Evaluates durable-outbox invariants every
			// 5 minutes and routes breaches to #outbox-self-check.
			outboxSelfCheck := worker.NewOutboxSelfCheck(mailingDB)
			if _, noop := outboxNotifier.(notify.NoopNotifier); !noop {
				outboxSelfCheck.SetAlerter(worker.NewSlackAlerterTiered(outboxNotifier, "OUTBOX", notify.TierAlert), []string{"slack"})
				log.Printf("Outbox Self-Check Slack alerts ENABLED (transport=%s)", outboxNotifier.Name())
			} else {
				log.Println("Outbox Self-Check Slack alerts DISABLED (no Slack transport configured)")
			}
			go outboxSelfCheck.Start(ctx)
			log.Println("Outbox Self-Check started (5m interval, 30m re-alert suppression)")

			// Storage guard — state-aware replication slot / WAL / queue / acct monitoring.
			storageGuard := worker.NewStorageGuard(mailingDB, replicaConfigured)
			if _, noop := storageNotifier.(notify.NoopNotifier); !noop {
				storageGuard.SetAlerter(worker.NewSlackAlerterTiered(storageNotifier, "STORAGE", notify.TierAlert), []string{"slack"})
				log.Printf("Storage Guard Slack alerts ENABLED (transport=%s)", storageNotifier.Name())
			}
			go storageGuard.Start(ctx)
			server.SetStorageGuard(storageGuard)
			log.Println("Storage Guard started (5m interval, state-aware slot/WAL/queue/acct checks)")

			// Start Data Cleanup Worker (removes old queue items, tracking events, agent decisions)
			dataCleanup := worker.NewDataCleanupWorker(mailingDB)
			go dataCleanup.Start(ctx)
			log.Println("Data Cleanup Worker started (runs every 1h, batch deletes old data)")

			// Domain Agent scorecard worker — rebuilds the per-domain × ISP
			// daily scorecard (mailing_domain_agent_scorecard) for the trailing
			// 3 days, for every org with active sending profiles. Backs the
			// /api/mailing/domain-agent endpoints.
			domainAgentScorecard := domainagent.NewScorecardWorker(mailingDB, redisClient)
			domainAgentScorecard.Start(ctx)
			log.Println("Domain Agent Scorecard Worker started (first run in 60s, then every 6h, 3-day rollup window)")

			// Start Engine Signals Archiver. Keeps mailing_engine_signals
			// at a 14-day hot window; everything older lands in
			// s3://$ENGINE_S3_BUCKET/engine-signals/dt=YYYY-MM-DD/isp=<isp>/
			// with a pointer row in mailing_engine_signals_archive_index.
			// Cold reads go through internal/engine/signal_archive.go.
			{
				engineBucket := os.Getenv("ENGINE_S3_BUCKET")
				if engineBucket == "" {
					engineBucket = "ignite-pmta-engine"
				}
				engineRegion := os.Getenv("ENGINE_S3_REGION")
				if engineRegion == "" {
					engineRegion = "us-west-2"
				}
				engAwsCfg, engErr := awsconfig.LoadDefaultConfig(
					context.Background(),
					awsconfig.WithRegion(engineRegion),
				)
				if engErr != nil {
					log.Printf("WARNING: Engine Signals Archiver disabled — AWS config failed: %v", engErr)
				} else {
					engS3 := s3.NewFromConfig(engAwsCfg)
					archiver := worker.NewEngineSignalsArchiver(mailingDB, engS3, engineBucket)
					go archiver.Start(ctx)
					log.Printf("Engine Signals Archiver started (hot=14d, interval=6h, bucket=%s, region=%s)",
						engineBucket, engineRegion)
				}
			}

			// Analytics event lake (Phase 1) — best-effort fan-out of
			// per-recipient delivery/engagement events to Firehose ->
			// s3://ignite-analytics-lake (JSON->Parquet, Glue+Athena).
			// DISABLED unless ANALYTICS_FIREHOSE_STREAM is set, so this
			// ships dark and is enabled later via env (no code change).
			// Never blocks the send/ingest hot path; lossy by design.
			{
				lakeStream := os.Getenv("ANALYTICS_FIREHOSE_STREAM")
				lakeRegion := os.Getenv("ANALYTICS_FIREHOSE_REGION")
				if lakeRegion == "" {
					lakeRegion = "us-west-2"
				}
				if err := analytics.Init(context.Background(), lakeStream, lakeRegion); err != nil {
					log.Printf("WARNING: analytics event lake init failed: %v", err)
				}

				// READ side of the same lake (Athena-backed). Disabled
				// unless ANALYTICS_ATHENA_OUTPUT is set, so this also ships
				// dark. Best-effort: never fail boot.
				if err := analytics.InitReader(context.Background(), os.Getenv("ANALYTICS_ATHENA_DATABASE"), os.Getenv("ANALYTICS_ATHENA_WORKGROUP"), os.Getenv("ANALYTICS_ATHENA_OUTPUT"), lakeRegion); err != nil {
					log.Printf("WARNING: analytics lake reader init failed: %v", err)
				}
			}

			// Start ISP Backfill Worker (classifies mailing_subscribers.isp
			// for rows with empty/NULL isp). The PMTA campaign planner's
			// per-ISP cold-fallback stripe relies on this column being
			// populated, so we eagerly backfill on startup and then hourly.
			// Uses the canonical isp.SQLCaseFromEmail classifier so the SQL
			// and Go classifiers never drift.
			ispBackfill := worker.NewISPBackfillWorker(mailingDB)
			go ispBackfill.Start(ctx)
			log.Println("ISP Backfill Worker started (classifies mailing_subscribers.isp hourly)")

			// Start Warmup Graduation Worker (P5b): nightly sweep that
			// promotes warming→engaged and demotes engaged→dormant on
			// the SDS table. First pass runs 2m after boot, then every
			// 24h aligned to 02:00 UTC.
			warmupGraduator := mailing.NewWarmupGraduator(mailingDB)
			warmupGraduator.Start(ctx)
			log.Println("Warmup Graduation Worker started (nightly sweep on mailing_subscriber_domain_state)")

			// Start Segment Refresh Worker (recalculates dynamic segment subscriber counts).
			// Reads heavy COUNT queries from readDB (replica when configured, primary otherwise).
			// Writes subscriber_count updates back to mailingDB (always the primary).
			segRefresh := worker.NewSegmentRefreshWorkerWithConcurrency(readDB, mailingDB, 30*time.Minute, 2)
			segRefresh.Start(ctx)
			log.Println("Segment Refresh Worker started (recalculates dynamic segments every 30m, concurrency=2)")

			// Start Segment Cleanup Worker. This was previously built but
			// never wired into main, which is why ~19.8k segments
			// accumulated. It hard-deletes single-use static snapshots
			// (segment + member rows, no FK cascade) 7d after last_used,
			// and runs the warn→grace→archive→delete lifecycle for unused
			// dynamic segments per mailing_segment_cleanup_settings.
			// emailSender is nil: warnings are recorded in-DB (and surfaced
			// in the UI) without sending email. Stops on ctx cancellation.
			segCleanup := worker.NewSegmentCleanupWorker(mailingDB, nil)
			segCleanup.Start()
			go func() {
				<-ctx.Done()
				segCleanup.Stop()
			}()
			log.Println("Segment Cleanup Worker started (static snapshots hard-deleted 7d after last_used; dynamic on warn/grace/archive)")

			// Start Worker Health Monitor. Scans mailing_worker_heartbeats
			// and alerts (Slack if SLACK_BOT_TOKEN set, else logs) when any
			// instrumented worker misses 3× its declared cycle interval.
			// Surfaced in the UI via /api/worker-health.
			workerStallNotifier := notify.SlackChannelFromEnv("SLACK_WORKER_STALL_CHANNEL", "#worker-stall")
			workerHealthMonitor := worker.NewWorkerHealthMonitor(mailingDB, workerStallNotifier)
			go workerHealthMonitor.Start(ctx)
			log.Printf("Worker Health Monitor started (5m scan, transport=%s)", workerStallNotifier.Name())

			// Sam's Club internal drip digest. Posts a 6-hourly progress
			// snapshot (queue depth by ISP, sent last 24h, drain ETA, live
			// status) for the samsclub_internal vertical — the 1.4M
			// Yahoo/AOL net-new engaged load — to #yahoo-aol-sams-drip
			// (override: SLACK_SAMSCLUB_DRIP_CHANNEL). Posts via the shared
			// SLACK_BOT_TOKEN; logs only if no Slack transport is configured.
			samsDripNotifier := notify.SlackChannelFromEnv("SLACK_SAMSCLUB_DRIP_CHANNEL", "#yahoo-aol-sams-drip")
			samsDripDigest := worker.NewPartnerDripDigestMonitor(mailingDB, samsDripNotifier, "samsclub_internal", "Yahoo/AOL Engaged")
			go samsDripDigest.Start(ctx)
			log.Printf("Sam's Club drip digest started (vertical=samsclub_internal, 6h, transport=%s)", samsDripNotifier.Name())

			// Partner engagement marker — the missing WRITER for
			// partner_clean_queue.engaged_at. Stamps engaged_at when a
			// record's subscriber CLICKS one of that dataset's drip
			// campaigns (partner_dataset_id match, clicks-only). Fixes the
			// drip's engaged-exit (stop re-mailing proven clickers) AND the
			// Activation/Churn metrics that were stuck at 0/100% because the
			// column was read everywhere but written nowhere. Backfills all
			// history once on boot, then sweeps recent clicks every 3m.
			// Kill: DISABLE_PARTNER_ENGAGEMENT_MARKER=1.
			engagementMarker := worker.NewPartnerEngagementMarker(mailingDB)
			go engagementMarker.Start(ctx)
			log.Printf("Partner engagement marker started (clicks->engaged_at, 3m; kill: DISABLE_PARTNER_ENGAGEMENT_MARKER)")

			// Journey Segment Enroller — auto-enrolls subscribers from
			// segment-triggered journeys (Welcome Series Phase 2). Uses the
			// segmentation engine for saved segments, and the
			// "__preset_cleaned_never_mailed__" preset for the UI's
			// cleaned-never-mailed shortcut. Idempotent via UNIQUE
			// (journey_id, subscriber_email) ON CONFLICT DO NOTHING.
			journeySegmentEnroller := worker.NewJourneySegmentEnroller(mailingDB, segmentation.NewEngine(mailingDB))
			journeySegmentEnroller.Start(ctx)
			log.Println("Journey Segment Enroller started (auto-enrolls segment-triggered journeys every 5m)")

			// Click-Drip Journey workers (2026-06-01). These run IN-PROCESS in
			// the server because there is no separate worker deployment in
			// production (cmd/worker is not deployed; only cmd/server runs as
			// ignite-service). Safe to run here: the only active journey in the
			// system is the click-drip journey, so the executor never touches
			// Welcome-series sending (which goes through PMTA waves, not the
			// journey tables).
			//
			//  1. JourneyEventEnroller drains mailing_journey_event_triggers
			//     (queued by EverflowClickPostbackHandler) into
			//     mailing_journey_enrollments every 5s.
			//  2. JourneyExecutor advances enrollments through the delay/email
			//     nodes; click-drip email nodes dispatch via JourneyClickDripSender
			//     which reuses the in-process profileSender (PMTA HTTP bridge +
			//     per-ISP VMTA pool routing) so reminders go out on the exact
			//     brand profile the subscriber originally clicked from, with
			//     open/click tracking and a mailing_message_log row.
			journeyEventEnroller := worker.NewJourneyEventEnroller(mailingDB)
			journeyEventEnroller.Start(ctx)
			log.Println("Journey Event Enroller started (drains click-drip trigger queue every 5s)")

			// JourneyClickTrackingEnroller is the INTERNAL-click inlet: it
			// reads real click events from mailing_tracking_events, resolves
			// the offer from the money-URL slug (cratoolpro or affiliate PS####;
			// mailing_offer_slug_map), and writes them into the SAME
			// mailing_journey_event_triggers queue the Everflow path uses.
			// This is what actually feeds the drip, since Everflow's per-offer
			// click postbacks don't reliably reach us.
			journeyClickTrackingEnroller := worker.NewJourneyClickTrackingEnroller(mailingDB)
			journeyClickTrackingEnroller.Start(ctx)
			log.Println("Journey Click-Tracking Enroller started (enrolls internal money-URL clicks every 30s)")

			journeyClickDripSender := worker.NewJourneyClickDripSender(mailingDB, profileSender, trackURL, trackSecret)
			journeyExecutor := worker.NewJourneyExecutor(mailingDB)
			journeyExecutor.SetClickDripSender(journeyClickDripSender)
			journeyExecutor.Start()
			log.Println("Journey Executor started (advances click-drip enrollments; reminders sent via PMTA on original profile)")

			ghostVisitorWorker := worker.NewGhostVisitorWorker(mailingDB, 4*time.Hour)
			ghostVisitorWorker.Start(ctx)
			log.Println("Ghost Visitor Worker started (tags ghost visitors every 4h)")

			// Start List Refresh Worker (updates subscriber_count and mailed_to on lists)
			listRefresh := worker.NewListRefreshWorker(mailingDB, 1*time.Hour)
			listRefresh.Start(ctx)
			log.Println("List Refresh Worker started (updates list counts every 1h)")

			suppressionListWorker := worker.NewSuppressionListWorker(mailingDB, 24*time.Hour)
			suppressionListWorker.Start(ctx)
			log.Println("Suppression List Worker started (brand-specific sunset suppression, runs daily)")

			healthMonitor := worker.NewCampaignHealthMonitor(mailingDB)

			// Wire the campaign-lateness pager to Slack (Twilio SMS retired
			// 2026-06-07). Feature is off by default; enable via
			// alerting.campaign_lateness.enabled (yaml) or
			// ALERT_CAMPAIGN_LATENESS_ENABLED=1 (env). Delivers to
			// #campaign-lateness-pager (override: SLACK_CAMPAIGN_LATENESS_CHANNEL).
			latenessNotifier := notify.SlackChannelFromEnv("SLACK_CAMPAIGN_LATENESS_CHANNEL", "#campaign-lateness-pager")
			if _, noop := latenessNotifier.(notify.NoopNotifier); cfg.Alerting.CampaignLateness.Enabled && !noop {
				threshold := time.Duration(cfg.Alerting.CampaignLateness.ThresholdMinutes) * time.Minute
				reAlert := time.Duration(cfg.Alerting.CampaignLateness.ReAlertAfterHours) * time.Hour
				healthMonitor.SetLatenessAlerter(
					worker.NewSlackAlerterTiered(latenessNotifier, "Campaign lateness", notify.TierAlert),
					[]string{"slack"},
					threshold,
					reAlert,
				)
				log.Printf("Campaign lateness Slack alerts ENABLED: threshold=%s realert=%s transport=%s",
					threshold, reAlert, latenessNotifier.Name())
			} else {
				log.Println("Campaign lateness Slack alerts DISABLED (alerting.campaign_lateness.enabled=false or no Slack transport)")
			}

			healthMonitor.Start()
			defer healthMonitor.Stop()
			log.Println("Campaign Health Monitor started (per-ISP threshold auto-pause, checks every 60s)")

			// Start Content Refresh Worker (pre-generates wave email content nightly)
			contentRefresh := worker.NewContentRefreshWorker(mailingDB, 24*time.Hour)
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "discountblog", BlogDomain: "discountblog.com",
				SendingDomain: "em.discountblog.com", BrandName: "Discount Blog",
				CampaignType: "newsletter",
				Voice:        `You are writing as "Jamie" from Discount Blog — a relatable, practical person who genuinely loves saving money and sharing what works. First-person storytelling. You say things like "My wife and I tried this..." and "Here's what actually worked for our family." Pull specific numbers from the articles — dollar amounts, percentages, timeframes. Never generic. Every sentence should teach or reveal something useful. Tone: warm, honest, slightly conspiratorial (like sharing a secret with a friend). NOT salesy, NOT clickbaity, NOT corporate. Think: personal finance blog meets friendly text message.`,
				Audience:     `Budget-conscious families, young professionals figuring out adulting, and savvy deal hunters. They're busy, skeptical of hype, and want actionable advice — not listicles. They read Discount Blog because it feels real, not manufactured.`,
				HTMLTemplate: api.DiscountBlogHTMLTemplate,
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "quizfiesta", BlogDomain: "quizfiesta.com",
				SendingDomain: "em.quizfiesta.com", BrandName: "QuizFiesta",
				CampaignType: "trivia",
				Voice:        `You are the voice of QuizFiesta — a retro-arcade trivia platform. Write like an arcade machine that gained sentience and got really into hyping people up. Short punchy sentences. Direct challenges to the reader. Arcade/gaming lingo: "INSERT COIN", "GAME OVER", "player", "high score", "streak", "level up." Competitive but encouraging — you want them to play, not feel bad. Use ALL CAPS sparingly for emphasis on key words (one per paragraph max). Think: the text on an arcade cabinet's attract screen mixed with a friend trash-talking you at game night.`,
				Audience:     `Trivia lovers, casual gamers, friend groups who want game night content, and competitive players chasing leaderboard spots. They're on their phone, they have 2 minutes, and you need to get them excited enough to tap PLAY.`,
				HTMLTemplate: api.QuizFiestaHTMLTemplate,
				FallbackContent: []mailing.BlogExcerpt{
					{Title: "Classic Mode — Test Your Knowledge", Excerpt: "15 questions. 30 seconds each. Streak multipliers and adaptive difficulty. How high can you score?", URL: "https://quizfiesta.com/play"},
					{Title: "Survival Mode — One Wrong Answer and It's Over", Excerpt: "3 lives. Questions get harder the longer you last. Stack streak bonuses for massive multipliers. Current record: 47 questions.", URL: "https://quizfiesta.com/play"},
					{Title: "Speed Run — 30 Seconds. Go.", Excerpt: "The clock is ticking. Answer as many as you can before time runs out. Every second wasted is points lost.", URL: "https://quizfiesta.com/play"},
					{Title: "AI Challenge — A.P.E.X. Is Watching", Excerpt: "Race the machine through 20 levels. Our AI adapts to your skill in real-time. It studies your weaknesses.", URL: "https://quizfiesta.com/play"},
					{Title: "Multiplayer Duels — Settle It in Real-Time", Excerpt: "Share a room code. Go head-to-head. Real-time trivia battles where every millisecond matters.", URL: "https://quizfiesta.com/play"},
					{Title: "Party Mode — Up to 8 Players. Total Chaos.", Excerpt: "Create a room. Share the code. Jackbox-style trivia with friends. Perfect for game night.", URL: "https://quizfiesta.com/play"},
					{Title: "Weekly Leaderboard — The Arcade's Finest", Excerpt: "New leaderboard resets every Monday. Current top 3 average 94% accuracy. Where do you rank?", URL: "https://quizfiesta.com/leaderboard"},
				},
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "discountblog", BlogDomain: "discountblog.com",
				SendingDomain: "em.discountblog.com", BrandName: "Discount Blog",
				CampaignType: "welcome",
				Voice:        `You are writing as "Jamie" from Discount Blog — a relatable, practical person who genuinely loves saving money and sharing what works. This is a WELCOME email for brand-new subscribers. Your job is to prove — with real numbers and real examples — that this newsletter will save them money. First-person storytelling: "My wife and I tried this..." Pull specific dollar amounts, percentages, and store names from the articles. Feature 2-3 of the best current deals as proof of value. Include a clear CTA to browse deals. Tone: warm, honest, slightly conspiratorial (like sharing a secret with a friend). NOT salesy, NOT clickbaity, NOT corporate. The first impression must feel real, not manufactured.`,
				Audience:     `Brand-new subscribers who just signed up. Curious but uncommitted — this is the first impression. They're skeptical of yet another email list and need immediate proof this one is different. Show them a real deal they would have missed.`,
				HTMLTemplate: api.DiscountBlogWelcomeHTMLTemplate,
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "quizfiesta", BlogDomain: "quizfiesta.com",
				SendingDomain: "em.quizfiesta.com", BrandName: "QuizFiesta",
				CampaignType: "welcome",
				Voice:        `You are the voice of QuizFiesta — a retro-arcade trivia platform. This is a WELCOME email for brand-new players. Your job is to get them to tap PLAY within 60 seconds of opening. Short punchy sentences. Direct challenges: "Think you know science? Prove it." Arcade/gaming lingo: "INSERT COIN", "PLAYER ONE", "high score", "streak." Highlight 2-3 game modes with specific hooks — mention the leaderboard record, mention the AI opponent, mention multiplayer. Competitive but encouraging — trash-talk with a wink. Think: the attract screen on an arcade cabinet that makes you dig for quarters.`,
				Audience:     `Brand-new players who just signed up. They haven't played yet. They're one tap away from becoming addicted or unsubscribing. This email decides which. Hit them with the most exciting game mode, a challenge they can't ignore, and a CTA that feels like pressing START.`,
				HTMLTemplate: api.QuizFiestaWelcomeHTMLTemplate,
				FallbackContent: []mailing.BlogExcerpt{
					{Title: "Classic Mode — Test Your Knowledge", Excerpt: "15 questions. 30 seconds each. Streak multipliers and adaptive difficulty.", URL: "https://quizfiesta.com/play"},
					{Title: "Survival Mode — One Wrong Answer and It's Over", Excerpt: "3 lives. Questions get harder the longer you last.", URL: "https://quizfiesta.com/play"},
					{Title: "Speed Run — 30 Seconds. Go.", Excerpt: "The clock is ticking. Answer as many as you can before time runs out.", URL: "https://quizfiesta.com/play"},
					{Title: "Multiplayer Duels — Settle It in Real-Time", Excerpt: "Share a room code. Go head-to-head. Real-time trivia battles.", URL: "https://quizfiesta.com/play"},
				},
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "historythinking", BlogDomain: "historythinking.com",
				SendingDomain: "em.historythinking.com", BrandName: "History Thinking",
				CampaignType: "newsletter",
				Voice:        `You are the voice of History Thinking — a scholarly yet accessible history publication. Write like a brilliant professor who tells stories at dinner parties that leave everyone speechless. Every article must surprise: reveal hidden connections, debunk myths, or reframe well-known events. Use vivid period detail and primary sources. Tone: authoritative but never dry, passionate about making history feel alive and relevant. Think: a documentary narrator who can't help but lean in when the story gets good.`,
				Audience:     `History enthusiasts who want surprising, well-researched stories that go beyond textbook summaries. They're curious, educated, and love "I didn't know that" moments.`,
				HTMLTemplate: api.HistoryThinkingHTMLTemplate,
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "historythinking", BlogDomain: "historythinking.com",
				SendingDomain: "em.historythinking.com", BrandName: "History Thinking",
				CampaignType: "welcome",
				Voice:        `You are the voice of History Thinking — a scholarly yet accessible history publication. This is a WELCOME email for brand-new subscribers. Your job is to prove that this newsletter will consistently surprise and educate them. Write like a brilliant professor telling stories at a dinner party that leave everyone speechless. Lead with the most jaw-dropping historical fact you can find from the blog. Feature 2-3 articles with vivid period detail: dates, names, consequences. Every article summary should end on a hook that makes them want to click through. Tone: authoritative but never dry, passionate about making history feel alive and relevant.`,
				Audience:     `Brand-new subscribers who just signed up. They're curious about history but haven't committed — this is the email that proves History Thinking is different from boring history textbooks. They want "I didn't know that" moments they can share with friends.`,
				HTMLTemplate: api.HistoryThinkingWelcomeHTMLTemplate,
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "myownhealth", BlogDomain: "myownhealth.net",
				SendingDomain: "em.myownhealth.net", BrandName: "My Own Health",
				CampaignType: "newsletter",
				Voice:        `You are the voice of My Own Health — a no-nonsense, evidence-based health publication. Write like a sports medicine doctor who also lifts. Direct, actionable, zero fluff. Every claim needs a mechanism: don't just say "drink water," explain what dehydration does to cortisol and recovery. Use specific numbers: grams, reps, minutes, studies. Challenge bro-science and wellness hype with actual data. Tone: confident, slightly irreverent, respects the reader's intelligence. Think: a coach who reads PubMed for fun.`,
				Audience:     `Health-conscious adults who want actionable tips without fluff. They're tired of vague "eat clean, train hard" advice and want specific protocols backed by evidence.`,
				HTMLTemplate: api.MyOwnHealthHTMLTemplate,
			})
			contentRefresh.RegisterBrand(worker.ContentBrand{
				Key: "myownhealth", BlogDomain: "myownhealth.net",
				SendingDomain: "em.myownhealth.net", BrandName: "My Own Health",
				CampaignType: "welcome",
				Voice:        `You are the voice of My Own Health — a no-nonsense, evidence-based health publication. This is a WELCOME email for brand-new subscribers. Your job is to prove in 30 seconds of reading that this newsletter is different from every other wellness spam in their inbox. Lead with a specific, surprising health fact backed by a mechanism. Feature 2-3 articles with concrete numbers — grams, percentages, study sizes, timeframes. Challenge conventional wisdom: if a popular belief is wrong, say so and explain why. Mention the free tools (BMI, macro, calorie calculators) as proof of value. Tone: confident like a sports medicine doctor who also lifts, slightly irreverent, zero fluff, respects the reader's intelligence.`,
				Audience:     `Brand-new subscribers who just signed up. They're tired of vague "eat clean, train hard" wellness newsletters and want evidence-based content with specific protocols. This email must immediately prove we're different — hit them with the most counterintuitive, well-sourced health insight from the blog.`,
				HTMLTemplate: api.MyOwnHealthWelcomeHTMLTemplate,
			})
			contentRefresh.Start(ctx)
			log.Println("Content Refresh Worker started (generates fresh wave content every 24h)")

			// Start Offer Suppression Worker (fatigue suppression + performance rollups nightly)
			offerSuppWorker := worker.NewOfferSuppressionWorker(mailingDB, 24*time.Hour)
			offerSuppWorker.Start(ctx)
			log.Println("Offer Suppression Worker started (fatigue suppression + performance rollups every 24h)")

			// Start S3 Data Normalizer (imports from jvc-email-data bucket)
			datanormCfg := datanorm.Config{
				Bucket:     cfg.DataNorm.S3Bucket,
				Region:     cfg.DataNorm.S3Region,
				AWSProfile: cfg.DataNorm.AWSProfile,
				OrgID:      "00000000-0000-0000-0000-000000000001",
				ListID:     cfg.DataNorm.DefaultListID,
				Interval:   time.Duration(cfg.DataNorm.IntervalMinutes) * time.Minute,
			}
			var normalizer *datanorm.Normalizer
			if cfg.DataNorm.Enabled {
				var err error
				normalizer, err = datanorm.NewNormalizer(mailingDB, datanormCfg)
				if err != nil {
					log.Printf("Warning: Data normalizer init failed: %v", err)
				} else {
					normalizer.Start()
					server.SetNormalizer(normalizer)
					log.Printf("S3 Data Normalizer started (bucket: %s, interval: %dm)", cfg.DataNorm.S3Bucket, cfg.DataNorm.IntervalMinutes)
				}
			}

			// Initialize EventWriter for subscriber_events table
			eventWriter := datanorm.NewEventWriter(mailingDB)
			_ = eventWriter // will be wired to handlers in subsequent phases

			// Start Data Pipeline (nightly S3 ingestion, EO validation, list replenishment)
			var dataPipeline *worker.DataPipeline
			log.Printf("[DataPipeline] Config: enabled=%v bucket=%q prefix=%q eo_token_set=%v",
				cfg.DataPipeline.Enabled, cfg.DataPipeline.S3Bucket, cfg.DataPipeline.S3Prefix, cfg.DataPipeline.EOAPIToken != "")
			if cfg.DataPipeline.Enabled {
				var err error
				dataPipeline, err = worker.NewDataPipeline(mailingDB, cfg.DataPipeline, "00000000-0000-0000-0000-000000000001")
				if err != nil {
					log.Printf("Warning: Data Pipeline init failed: %v", err)
				} else {
					// Wire notifications via alerter with SES auth
					sesUser := os.Getenv("SES_SMTP_USER")
					sesSecret := os.Getenv("SES_SMTP_SECRET")
					sesRegion := os.Getenv("SES_REGION")
					if sesRegion == "" {
						sesRegion = "us-east-1"
					}
					if sesUser != "" && sesSecret != "" {
						pipelineAlerter := engine.NewAlerter(engine.AlerterConfig{
							SMTPHost: fmt.Sprintf("email-smtp.%s.amazonaws.com", sesRegion),
							SMTPPort: 587,
							SMTPUser: sesUser,
							SMTPPass: sesSecret,
							From:     "noreply@projectjarvis.io",
							To:       []string{cfg.DataPipeline.AdminEmail},
						})
						dataPipeline.Notifier = &pipelineAlerterAdapter{alerter: pipelineAlerter}
					}

					dataPipeline.Start()
					server.DataPipeline = dataPipeline
					log.Printf("Data Pipeline started (bucket: %s, prefix: %s, run_hour_utc: %d)",
						cfg.DataPipeline.S3Bucket, cfg.DataPipeline.S3Prefix, cfg.DataPipeline.RunHourUTC)
				}
			}

			// Data Partner Ingestion workers (slicer, validator, drip orchestrator).
			// Each is a polled goroutine — see internal/worker/partner_*.go.
			// They start AFTER server.GlobalHub has been wired by SetMailingDB.
			var partnerSlicer *worker.PartnerSlicer
			var partnerValidator *worker.PartnerValidator
			{
				partnerS3 := server.GetPartnerIngestS3Client()
				partnerEnabled := partnerS3 != nil
				if !partnerEnabled {
					log.Println("[PartnerIngest] disabled — no partner-ingest S3 client wired")
				} else {
					partnerRegion := partnerS3.Region()
					awsCfg, awsErr := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(partnerRegion))
					if awsErr != nil {
						log.Printf("[PartnerIngest] aws config: %v — workers skipped", awsErr)
					} else {
						s3RawClient := s3.NewFromConfig(awsCfg)

						// Slicer: drains S3 NDJSON into partner_clean_queue with global suppression check.
						hubAdapter := worker.SuppressionHub(nil)
						if hub, ok := server.GlobalHub.(*engine.GlobalSuppressionHub); ok {
							hubAdapter = worker.AdaptGlobalHub(hub)
						}
						if hubAdapter == nil {
							log.Println("[PartnerSlicer] WARNING: GlobalHub not yet ready — slicer will operate without suppression check")
						}
						partnerSlicer = worker.NewPartnerSlicer(mailingDB, s3RawClient, partnerS3.Bucket(), hubAdapter, worker.PartnerSlicerConfig{})
						partnerSlicer.Start()
						server.SetPartnerSlicer(partnerSlicer)

						// Validator: drains pending_eo with EmailOversight.
						eoToken := cfg.DataPipeline.EOAPIToken
						if eoToken == "" {
							eoToken = os.Getenv("EO_API_TOKEN")
						}
						if eoToken == "" {
							log.Println("[PartnerValidator] WARNING: no EO API token — partner records will queue indefinitely until configured")
						} else {
							eoListID := cfg.DataPipeline.EOListID
							if eoListID <= 0 {
								eoListID = 1
							}
							eoConcurrency := cfg.DataPipeline.EOConcurrency
							if eoConcurrency <= 0 {
								eoConcurrency = 10
							}
							eoClient := emailoversight.New(eoToken, eoListID, eoConcurrency)
							partnerValidator = worker.NewPartnerValidator(mailingDB, eoClient, worker.PartnerValidatorConfig{
								BatchSize:      500,
								Concurrency:    eoConcurrency,
								OrganizationID: "00000000-0000-0000-0000-000000000001",
							})
							partnerValidator.Start()
							server.SetPartnerValidator(partnerValidator)
						}
					}
				}
			}

			// Ensure workers stop on shutdown (H12)
			go func() {
				<-ctx.Done()
				campaignScheduler.Stop()
				sendWorkerPool.Stop()
				offerSuppWorker.Stop()
				server.StopDeltaSyncWorker()
				if trackingConsumer != nil {
					trackingConsumer.Stop()
				}
				if normalizer != nil {
					normalizer.Stop()
				}
				if dataPipeline != nil {
					dataPipeline.Stop()
				}
				if partnerSlicer != nil {
					partnerSlicer.Stop()
				}
				if partnerValidator != nil {
					partnerValidator.Stop()
				}
				if orch := server.GetPartnerDripOrchestrator(); orch != nil {
					orch.Stop()
				}
				if redisClient != nil {
					redisClient.Close()
				}
			}()
		}
	}
	if mailingDB == nil {
		log.Println("Mailing Platform not configured (disabled or missing database_url)")
	}

	// Initialize Mailgun - always run if API key is configured
	if cfg.Mailgun.APIKey != "" && len(cfg.Mailgun.Domains) > 0 {
		log.Println("Initializing Mailgun integration...")

		mgClient := mailgun.NewClient(cfg.Mailgun)
		mgCollector := mailgun.NewCollector(mgClient, store, learningAgent, cfg.Polling)

		// Set Mailgun collector on server
		server.SetMailgunCollector(mgCollector)

		// Start Mailgun collector in background
		go mgCollector.Start(ctx)

		log.Printf("Mailgun integration started with %d domains", len(cfg.Mailgun.Domains))
	} else {
		log.Println("Mailgun integration not configured (missing API key or domains)")
	}

	// Initialize SES - always run if credentials are configured
	if cfg.SES.AccessKey != "" && cfg.SES.SecretKey != "" {
		log.Println("Initializing AWS SES integration...")

		sesClient, err := ses.NewClient(ctx, cfg.SES)
		if err != nil {
			log.Printf("Warning: Failed to initialize SES client: %v", err)
		} else {
			sesCollector := ses.NewCollector(sesClient, store, learningAgent, cfg.Polling)

			// Set SES collector on server
			server.SetSESCollector(sesCollector)

			// Start SES collector in background
			go sesCollector.Start(ctx)

			log.Printf("SES integration started with %d ISPs configured", len(cfg.SES.DefaultISPs()))
		}
	} else {
		log.Println("SES integration not configured (missing AWS credentials)")
	}

	// Initialize Ongage - campaign management platform
	var ongageClient *ongage.Client
	if cfg.Ongage.Enabled && cfg.Ongage.BaseURL != "" && cfg.Ongage.Username != "" {
		log.Println("Initializing Ongage integration...")

		ongageConfig := ongage.Config{
			BaseURL:     cfg.Ongage.BaseURL,
			Username:    cfg.Ongage.Username,
			Password:    cfg.Ongage.Password,
			AccountCode: cfg.Ongage.AccountCode,
			ListID:      cfg.Ongage.ListID,
		}
		ongageClient = ongage.NewClient(ongageConfig)

		// Calculate fetch interval (use polling interval or default to 2 minutes)
		fetchInterval := time.Duration(cfg.Polling.IntervalSeconds) * time.Second
		if fetchInterval < 2*time.Minute {
			fetchInterval = 2 * time.Minute
		}

		ongageCollector := ongage.NewCollector(ongageClient, fetchInterval, cfg.Ongage.LookbackDays)

		// Configure S3-backed persistence for Contact Activity volume data.
		// The once-daily report takes 15-30 min; S3 ensures results survive restarts.
		ongageS3Bucket := os.Getenv("ONGAGE_S3_BUCKET")
		if ongageS3Bucket == "" {
			ongageS3Bucket = "ignite-ongage-reports"
		}
		s3Region := cfg.Storage.AWSRegion
		if s3Region == "" {
			s3Region = "us-east-1"
		}
		s3VolCache, s3Err := ongage.NewS3VolumeCache(ctx, ongageS3Bucket, s3Region)
		if s3Err != nil {
			log.Printf("Warning: S3 volume cache init failed (Contact Activity results won't persist across restarts): %v", s3Err)
		} else {
			ongageCollector.SetS3Cache(s3VolCache)
			log.Printf("S3 volume cache configured (bucket: %s, region: %s)", ongageS3Bucket, s3Region)
		}

		// Set Ongage collector on server
		server.SetOngageCollector(ongageCollector)

		// Start Ongage collector in background
		ongageCollector.Start()

		log.Printf("Ongage integration started with %d days lookback", cfg.Ongage.LookbackDays)
	} else {
		log.Println("Ongage integration not configured (missing credentials or disabled)")
	}

	// Initialize Everflow - revenue tracking integration
	var efCollector *everflow.Collector
	if cfg.Everflow.Enabled && cfg.Everflow.APIKey != "" && len(cfg.Everflow.AffiliateIDs) > 0 {
		log.Println("Initializing Everflow integration...")

		efConfig := everflow.Config{
			APIKey:       cfg.Everflow.APIKey,
			BaseURL:      cfg.Everflow.BaseURL,
			TimezoneID:   cfg.Everflow.TimezoneID,
			CurrencyID:   cfg.Everflow.CurrencyID,
			Enabled:      cfg.Everflow.Enabled,
			AffiliateIDs: cfg.Everflow.AffiliateIDs,
		}
		efClient := everflow.NewClient(efConfig)

		// Calculate fetch interval (use polling interval or default to 5 minutes)
		fetchInterval := time.Duration(cfg.Polling.IntervalSeconds) * time.Second
		if fetchInterval < 5*time.Minute {
			fetchInterval = 5 * time.Minute
		}

		efCollector = everflow.NewCollector(efClient, fetchInterval, cfg.Everflow.LookbackDays)

		// Set up campaign enricher if Ongage is configured
		if ongageClient != nil {
			campaignEnricher := everflow.NewCampaignEnricher(ongageClient)
			// Also set the Ongage collector to access pre-fetched stats
			if ongageCollector := server.GetOngageCollector(); ongageCollector != nil {
				campaignEnricher.SetOngageCollector(ongageCollector)
			}
			efCollector.SetCampaignEnricher(campaignEnricher)
			log.Println("Everflow campaign enricher configured with Ongage integration")
		}

		// Set up cost calculator if ESP contracts are configured
		log.Printf("DEBUG: ESP contracts in config: %d", len(cfg.ESPContracts))
		if len(cfg.ESPContracts) > 0 {
			contracts := make([]everflow.ESPContractInfo, 0, len(cfg.ESPContracts))
			for i, c := range cfg.ESPContracts {
				log.Printf("DEBUG: Contract %d: Name=%q, Enabled=%v, Monthly=%d, Fee=%.2f",
					i, c.ESPName, c.Enabled, c.MonthlyIncluded, c.MonthlyFee)
				if c.Enabled {
					contracts = append(contracts, everflow.ESPContractInfo{
						ESPName:            c.ESPName,
						MonthlyIncluded:    c.MonthlyIncluded,
						MonthlyFee:         c.MonthlyFee,
						OverageRatePer1000: c.OverageRatePer1000,
					})
					log.Printf("ESP Contract loaded: %s - %d emails/mo for $%.2f, overage $%.4f/1000",
						c.ESPName, c.MonthlyIncluded, c.MonthlyFee, c.OverageRatePer1000)
				}
			}
			if len(contracts) > 0 {
				costCalculator := everflow.NewCostCalculator(contracts)
				efCollector.SetCostCalculator(costCalculator)
				log.Printf("ESP cost calculator initialized with %d contract(s)", len(contracts))
			}
		} else {
			log.Println("DEBUG: No ESP contracts found in config")
		}

		// Set Everflow collector on server
		server.SetEverflowCollector(efCollector)

		// Start Everflow collector in background
		efCollector.Start()

		// Start Network Intelligence Collector (network-wide data, no affiliate filter)
		// This background worker continuously processes the entire Everflow network
		// to build audience profiles and AI recommendations for campaign creation
		networkIntelInterval := 15 * time.Minute
		if fetchInterval > networkIntelInterval {
			networkIntelInterval = fetchInterval
		}
		networkIntelCollector := everflow.NewNetworkIntelligenceCollector(efClient, networkIntelInterval)
		networkIntelCollector.Start()
		server.SetNetworkIntelligenceCollector(networkIntelCollector)
		log.Println("Network Intelligence Collector started (network-wide offer analytics + audience profiling)")

		log.Printf("Everflow integration started with %d affiliate(s) and %d days lookback",
			len(cfg.Everflow.AffiliateIDs), cfg.Everflow.LookbackDays)
	} else {
		log.Println("Everflow integration not configured (missing API key, affiliates, or disabled)")
	}

	// Create enrichment service if both Everflow and Ongage are configured
	if efCollector != nil {
		enrichmentService := everflow.NewEnrichmentService(efCollector, ongageClient)
		// Set Ongage collector for accessing cached campaign stats
		if ongageCollector := server.GetOngageCollector(); ongageCollector != nil {
			enrichmentService.SetOngageCollector(ongageCollector)
		}
		server.SetEnrichmentService(enrichmentService)
		log.Println("Everflow enrichment service initialized (Ongage linked:", ongageClient != nil, ")")
	}

	// Initialize Knowledge Base for the AI agent
	// Check for S3 storage configuration (preferred) or fall back to local
	var knowledgeBase *agent.KnowledgeBase

	s3Bucket := os.Getenv("JARVIS_S3_BUCKET")
	s3Prefix := os.Getenv("JARVIS_S3_PREFIX")
	s3EncKey := os.Getenv("JARVIS_S3_ENCRYPTION_KEY") // Base64-encoded 32-byte AES-256 key
	useAWSOnly := os.Getenv("JARVIS_USE_AWS_ONLY") == "true"

	// Use S3 bucket from config if available
	if s3Bucket == "" && cfg.Storage.S3Bucket != "" {
		s3Bucket = cfg.Storage.S3Bucket
	}

	if s3Bucket != "" {
		// Use S3 storage for knowledge base (keeps data on AWS)
		if s3Prefix == "" {
			s3Prefix = "ignite/knowledge/"
		}

		kbConfig := agent.KnowledgeBaseConfig{
			LocalPath:       "data/knowledge_base.json", // Fallback
			S3Bucket:        s3Bucket,
			S3Prefix:        s3Prefix,
			S3Region:        cfg.Storage.AWSRegion,
			S3EncryptionKey: s3EncKey,
			S3Compress:      true,
		}

		knowledgeBase = agent.NewKnowledgeBaseWithConfig(kbConfig)
		log.Printf("Knowledge Base initialized with S3 storage: s3://%s/%s", s3Bucket, s3Prefix)
	} else {
		// Fall back to local file storage
		knowledgeBasePath := "data/knowledge_base.json"
		knowledgeBase = agent.NewKnowledgeBase(knowledgeBasePath)
		log.Println("Knowledge Base initialized with local file storage")
	}

	// Determine which AI backend to use
	// Priority: IGNITE_USE_AWS_ONLY -> OpenAI (if configured)
	var openaiAgent *agent.OpenAIAgent
	var bedrockAgent *agent.BedrockAgent

	if useAWSOnly {
		// Use AWS Bedrock (data stays on AWS)
		log.Println("Initializing AWS Bedrock agent (AWS-only mode)...")
		var err error
		bedrockAgent, err = agent.NewBedrockAgent("", learningAgent, knowledgeBase)
		if err != nil {
			log.Printf("Warning: Failed to initialize Bedrock agent: %v", err)
		} else {
			log.Printf("AWS Bedrock agent initialized (model: %s, region: %s)",
				bedrockAgent.GetModelID(), bedrockAgent.GetRegion())
		}
	} else if cfg.OpenAI.Enabled && cfg.OpenAI.APIKey != "" {
		// Use OpenAI
		openaiAgent = agent.NewOpenAIAgent(cfg.OpenAI.APIKey, cfg.OpenAI.Model, learningAgent, knowledgeBase)
		server.SetOpenAIAgent(openaiAgent)
		log.Printf("OpenAI conversational agent initialized (model: %s)", cfg.OpenAI.Model)
	} else {
		log.Println("No AI agent configured - using keyword-based chat fallback")
	}

	// Start hourly learning cycle in background (if any AI agent is configured)
	if openaiAgent != nil || bedrockAgent != nil {
		go func() {
			// Wait for initial data collection
			time.Sleep(30 * time.Second)

			// Run initial learning cycle
			log.Println("Knowledge Base: Running initial learning cycle...")
			if err := knowledgeBase.RunLearningCycle(ctx, learningAgent); err != nil {
				log.Printf("Knowledge Base: Initial learning cycle error: %v", err)
			}
			if err := knowledgeBase.Save(); err != nil {
				log.Printf("Knowledge Base: Save error: %v", err)
			}

			// Run hourly learning cycles
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					// Save before shutdown
					knowledgeBase.Save()
					return
				case <-ticker.C:
					log.Println("Knowledge Base: Running hourly learning cycle...")
					if err := knowledgeBase.RunLearningCycle(ctx, learningAgent); err != nil {
						log.Printf("Knowledge Base: Learning cycle error: %v", err)
					}
					if err := knowledgeBase.Save(); err != nil {
						log.Printf("Knowledge Base: Save error: %v", err)
					}
				}
			}
		}()

		// Start the Agentic Self-Learning Loop
		if s3Bucket != "" && useAWSOnly {
			// Use full AWS configuration
			loopConfig := agent.AgenticLoopConfig{
				S3Bucket:            s3Bucket,
				S3Prefix:            s3Prefix,
				S3Region:            cfg.Storage.AWSRegion,
				S3EncryptionKey:     s3EncKey,
				S3Compress:          true,
				UseAWSOnly:          true,
				LearningInterval:    5 * time.Minute,
				OptimizationEnabled: true,
			}
			agenticLoop, err := agent.NewAgenticLoopWithConfig(server.GetMailingDB(), knowledgeBase, learningAgent, loopConfig)
			if err != nil {
				log.Printf("Warning: Failed to create agentic loop with full config: %v", err)
			} else {
				if bedrockAgent != nil {
					agenticLoop.SetBedrockAgent(bedrockAgent)
				}
				agenticLoop.Start()
				server.SetAgenticLoop(agenticLoop)
				log.Println("Agentic self-learning loop started (AWS-only mode, S3 storage)")
			}
		} else {
			// Use basic configuration with OpenAI
			agenticLoop := agent.NewAgenticLoop(server.GetMailingDB(), knowledgeBase, openaiAgent)
			agenticLoop.Start()
			server.SetAgenticLoop(agenticLoop)
			log.Println("Agentic self-learning loop started (5-minute intervals)")
		}
	}

	// Initialize Data Injections monitoring (Azure Table Storage + Snowflake + Ongage Imports)
	var azureCollector *azure.Collector
	var snowflakeCollector *snowflake.Collector

	// Initialize Azure Table Storage collector if configured
	if cfg.Azure.Enabled && cfg.Azure.ConnectionString != "" {
		log.Println("Initializing Azure Table Storage integration...")

		azureCfg := azure.Config{
			ConnectionString:  cfg.Azure.ConnectionString,
			TableName:         cfg.Azure.TableName,
			GapThresholdHours: cfg.Azure.GapThresholdHours,
			Enabled:           cfg.Azure.Enabled,
		}

		azureClient, err := azure.NewClient(azureCfg)
		if err != nil {
			log.Printf("Warning: Failed to initialize Azure client: %v", err)
		} else {
			azureCollector = azure.NewCollector(azureClient, azureCfg)
			go azureCollector.Start(ctx)
			log.Printf("Azure Table Storage integration started (table: %s)", cfg.Azure.TableName)

			// Wire volume providers into Everflow data-partner analytics.
			if efCollector != nil {
				oc := server.GetOngageCollector() // may be nil if Ongage not configured
				ac := azureCollector              // capture for closure

				// 1. Per-data-set volume provider (for direct lookup by code)
				efCollector.SetVolumeProvider(func() map[string]int64 {
					if oc != nil {
						if ongageSends := oc.GetListSendsByDataSetCode(); len(ongageSends) > 0 {
							return ongageSends
						}
					}
					// Fall back to Azure injection RecordCount
					metrics := ac.GetDataSetMetrics()
					result := make(map[string]int64, len(metrics))
					for _, m := range metrics {
						result[m.DataSetCode] = m.RecordCount
					}
					return result
				})

				// 2. Date-range-aware total sends (fresh Ongage query for the exact date window)
				if oc != nil {
					efCollector.SetTotalSendsForDateRange(func(ctx context.Context, from, to time.Time) int64 {
						return oc.GetTotalSendsForDateRange(ctx, from, to)
					})
				}

				// 3. Date-range-aware sub2 entity report (clicks by data partner)
				// The collector fetches this directly via its own client (c.client).
				efCollector.SetSub2ReportForDateRange(func(ctx context.Context, from, to time.Time) *everflow.EntityReportResponse {
					report, err := efCollector.FetchSub2ReportForRange(ctx, from, to)
					if err != nil {
						log.Printf("Everflow: Failed to fetch sub2 report for %s to %s: %v",
							from.Format("2006-01-02"), to.Format("2006-01-02"), err)
						return nil
					}
					return report
				})

				// 4. Date-range-aware per-DS volume provider
				if oc != nil {
					efCollector.SetVolumeProviderForDateRange(func(ctx context.Context, from, to time.Time) map[string]int64 {
						return oc.GetListSendsByDataSetCodeForDateRange(ctx, from, to)
					})
				}

				log.Println("Volume providers wired: per-DS + total sends + sub2 clicks + volume-for-date (all date-aware)")
			}
		}
	} else {
		log.Println("Azure Table Storage integration not configured (disabled or missing connection string)")
	}

	// Initialize Snowflake collector if configured
	if cfg.Snowflake.Enabled && (cfg.Snowflake.User != "" || cfg.Snowflake.ConnectionString != "") {
		log.Println("Initializing Snowflake integration...")

		snowflakeCfg := snowflake.Config{
			Account:   cfg.Snowflake.Account,
			User:      cfg.Snowflake.User,
			Password:  cfg.Snowflake.Password,
			Database:  cfg.Snowflake.Database,
			Schema:    cfg.Snowflake.Schema,
			Warehouse: cfg.Snowflake.Warehouse,
			Enabled:   cfg.Snowflake.Enabled,
		}

		// If using connection string, parse it
		if cfg.Snowflake.ConnectionString != "" {
			parsedCfg := snowflake.ParseConnectionString(cfg.Snowflake.ConnectionString)
			if snowflakeCfg.Account == "" {
				snowflakeCfg.Account = parsedCfg.Account
			}
			if snowflakeCfg.User == "" {
				snowflakeCfg.User = parsedCfg.User
			}
			if snowflakeCfg.Password == "" {
				snowflakeCfg.Password = parsedCfg.Password
			}
			if snowflakeCfg.Database == "" {
				snowflakeCfg.Database = parsedCfg.Database
			}
			if snowflakeCfg.Schema == "" {
				snowflakeCfg.Schema = parsedCfg.Schema
			}
		}

		snowflakeClient, err := snowflake.NewClient(snowflakeCfg)
		if err != nil {
			log.Printf("Warning: Failed to initialize Snowflake client: %v", err)
		} else {
			snowflakeCollector = snowflake.NewCollector(snowflakeClient, snowflakeCfg)
			go snowflakeCollector.Start(ctx)
			log.Printf("Snowflake integration started (database: %s.%s)", snowflakeCfg.Database, snowflakeCfg.Schema)
		}
	} else {
		log.Println("Snowflake integration not configured (disabled or missing credentials)")
	}

	// Initialize Data Injections service if any data source is available
	// This service monitors partner data flow: Ingestion (Azure) -> Validation (Snowflake) -> Import (Ongage)
	// Track data injections service for Kanban
	var dataInjectionsService *datainjections.Service
	if azureCollector != nil || snowflakeCollector != nil || ongageClient != nil {
		log.Println("Initializing Data Injections monitoring service...")

		dataInjectionsService = datainjections.NewService(azureCollector, snowflakeCollector, ongageClient)
		go dataInjectionsService.Start(ctx)
		server.SetDataInjectionsService(dataInjectionsService)

		log.Printf("Data Injections service started (Azure: %v, Snowflake: %v, Ongage: %v)",
			azureCollector != nil, snowflakeCollector != nil, ongageClient != nil)
	} else {
		log.Println("Data Injections service not initialized (no data sources configured)")
	}

	// Initialize Kanban task management
	if cfg.Kanban.Enabled || cfg.Storage.DynamoDBTable != "" {
		log.Println("Initializing Kanban task management...")

		// Use Kanban-specific table or fallback to storage table
		tableName := cfg.Kanban.DynamoDBTable
		if tableName == "" {
			tableName = cfg.Storage.DynamoDBTable
		}

		if tableName != "" {
			kanbanClient, err := kanban.NewClient(ctx, tableName, cfg.Storage.AWSRegion, cfg.Storage.AWSProfile)
			if err != nil {
				log.Printf("Warning: Failed to initialize Kanban client: %v", err)
			} else {
				// Create Kanban config
				kanbanConfig := kanban.Config{
					Enabled:           true,
					MaxActiveTasks:    cfg.Kanban.MaxActiveTasks,
					MaxNewTasksPerRun: cfg.Kanban.MaxNewTasksPerRun,
					AIRunInterval:     time.Duration(cfg.Kanban.AIRunIntervalMins) * time.Minute,
				}
				if kanbanConfig.MaxActiveTasks == 0 {
					kanbanConfig.MaxActiveTasks = 20
				}
				if kanbanConfig.MaxNewTasksPerRun == 0 {
					kanbanConfig.MaxNewTasksPerRun = 3
				}
				if kanbanConfig.AIRunInterval == 0 {
					kanbanConfig.AIRunInterval = 1 * time.Hour
				}

				// Create services
				kanbanService := kanban.NewService(kanbanClient, kanbanConfig)
				server.SetKanbanService(kanbanService)

				// Create AI analyzer with collectors
				collectors := &kanban.CollectorSet{
					SparkPost:      spCollector,
					Everflow:       efCollector,
					DataInjections: dataInjectionsService,
				}
				kanbanAIAnalyzer := kanban.NewAIAnalyzer(kanbanService, collectors, kanbanConfig)
				server.SetKanbanAIAnalyzer(kanbanAIAnalyzer)

				// Create archival service
				kanbanArchival := kanban.NewArchivalService(kanbanClient, kanbanService)
				server.SetKanbanArchival(kanbanArchival)

				// Start scheduler (AI analysis, weekly cleanup, monthly reports)
				kanbanScheduler := kanban.NewScheduler(kanbanAIAnalyzer, kanbanArchival, kanbanConfig)
				go kanbanScheduler.Start(ctx)

				// Start service
				go kanbanService.Start(ctx)

				log.Println("Kanban task management started")
			}
		} else {
			log.Println("Kanban not initialized (no DynamoDB table configured)")
		}
	}

	// Initialize Revenue Model Service for financial dashboard
	if cfg.RevenueModel.Enabled {
		revenueModelService := financial.NewRevenueModelService(&cfg.RevenueModel, cfg.ESPContracts)
		if efCollector != nil {
			revenueModelService.SetEverflowCollector(efCollector)
		}
		server.SetRevenueModelService(revenueModelService)
		log.Println("Revenue model service initialized for financial dashboard")
	}

	// Initialize Intelligence Service for AI-powered learning
	intelligenceService := intelligence.NewService(
		server.GetOngageCollector(),
		efCollector,
		store,
		cfg.Storage.S3Bucket,
		"intelligence",
	)
	intelligenceService.Start()
	server.SetIntelligenceService(intelligenceService)
	log.Println("Intelligence service initialized with continuous learning")

	// Setup graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	log.Println("All services initialized — server is ready")

	<-done
	log.Println("Shutting down...")

	// Cancel background tasks
	cancel()

	// Stop the Kafka event backbone (consumers, flag pollers, then flush+close
	// the producer taps). Safe no-op on a nil/dark handle.
	eventBus.Stop()

	// Flush any buffered analytics events to the lake (no-op when disabled).
	analytics.Shutdown()

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	log.Println("Server stopped")
}

// seedProcessDefaultOrgID configures the process-wide fallback organization id
// used by api.GetOrgIDFromRequest when no other source supplies one. This
// guards every read endpoint that scopes by organization_id from silently
// returning empty results when an auth context fails to hydrate transiently.
//
// Resolution order:
//  1. Explicit DEFAULT_ORG_ID environment variable, if set.
//  2. Auto-discovery: if exactly one organization exists in the `organizations`
//     table, use it. (Production is single-tenant — James Ventures Corp.)
//
// Logs every outcome so the operational team can confirm what was wired.
func seedProcessDefaultOrgID(db *sql.DB) {
	if envID := os.Getenv("DEFAULT_ORG_ID"); envID != "" {
		if id, err := uuid.Parse(envID); err == nil {
			api.SetProcessDefaultOrgID(id)
			log.Printf("[OrgContext] process default org_id set from DEFAULT_ORG_ID env: %s", id)
			return
		}
		log.Printf("[OrgContext] DEFAULT_ORG_ID env set but unparseable: %q", envID)
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	if err := db.QueryRowContext(probeCtx, `SELECT COUNT(*) FROM organizations`).Scan(&count); err != nil {
		log.Printf("[OrgContext] could not probe organizations table for single-tenant default: %v", err)
		return
	}
	if count != 1 {
		log.Printf("[OrgContext] organizations table has %d rows; auto-default skipped (set DEFAULT_ORG_ID env to override)", count)
		return
	}

	var idStr string
	if err := db.QueryRowContext(probeCtx, `SELECT id::text FROM organizations LIMIT 1`).Scan(&idStr); err != nil {
		log.Printf("[OrgContext] failed to read single-tenant org id: %v", err)
		return
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		log.Printf("[OrgContext] single-tenant org id %q failed to parse: %v", idStr, err)
		return
	}
	api.SetProcessDefaultOrgID(id)
	log.Printf("[OrgContext] process default org_id auto-discovered (single tenant): %s", id)
}

// criticalSendPathDDL is schema the send path cannot run without: the send
// workers' claim SQL and the wave dispatcher reference these objects
// UNCONDITIONALLY (not behind any env kill switch), so the binary must never
// come up ahead of them. Applied synchronously at boot, BEFORE any worker
// starts, by ensureSendPathSchema — the deep fix for the 2026-06-10 outage,
// where the new binary went live while these two statements sat unexecuted
// at the back of the migration slice behind a lock storm (AAR action item 4).
// Statements must be idempotent; with the migrationSkipProbe fast path they
// cost ~1ms on every boot after the first.
var criticalSendPathDDL = []struct {
	name string
	sql  string
}{
	{"create_content_snapshots", `CREATE TABLE IF NOT EXISTS mailing_content_snapshots (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		content_hash TEXT NOT NULL UNIQUE,
		campaign_id UUID,
		wave_id UUID,
		html_content TEXT NOT NULL,
		plain_content TEXT NOT NULL DEFAULT '',
		content_locked BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`},
	{"add_queue_content_snapshot_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS content_snapshot_id UUID`},
	// Per-vertical partner-drip follow-up ladders (operator 2026-06-11):
	// NULL vertical = the shared/global fallback chain.
	{"add_followup_creatives_vertical", `ALTER TABLE partner_drip_followup_creatives ADD COLUMN IF NOT EXISTS vertical TEXT`},
	// The original PK (brand, touch_number) blocks per-vertical rows; replace
	// with a vertical-aware unique index ('' = the NULL/global chain).
	{"drop_followup_creatives_pk", `ALTER TABLE partner_drip_followup_creatives DROP CONSTRAINT IF EXISTS partner_drip_followup_creatives_pkey`},
	{"followup_creatives_vertical_uq", `CREATE UNIQUE INDEX IF NOT EXISTS partner_drip_followup_creatives_uq ON partner_drip_followup_creatives (brand, touch_number, COALESCE(vertical,''))`},
	// routing_mode distinguishes the KumoMTA HTTP injector from the PMTA HTTP
	// bridge for vendor_type='pmta' profiles. ProfileBasedSender reads it
	// UNCONDITIONALLY per send, so the column must exist before any worker starts.
	{"add_profile_routing_mode", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS routing_mode TEXT`},
	// ---- Kafka send-queue ledger (migration 050, SK-4) ----
	// Additive: NEW table only — no existing table is altered. It sits on the
	// send path (the QueueWriterConsumer's optional ledger record + the
	// LedgerReconciler scan reference it), so per migration 050's note it lives
	// in criticalSendPathDDL: schema must land BEFORE the binary that references
	// it. All statements are idempotent (IF NOT EXISTS), ~1ms after first boot.
	// Ships DARK: nothing writes here unless KAFKA_SEND_QUEUE_* routing is on.
	{"create_send_ledger", `CREATE TABLE IF NOT EXISTS mailing_send_ledger (
		idempotency_key UUID PRIMARY KEY,
		campaign_id     UUID,
		subscriber_id   UUID,
		wave_id         UUID,
		status          TEXT NOT NULL DEFAULT 'claimed',
		message_id      TEXT,
		worker_id       TEXT,
		attempts        INT  NOT NULL DEFAULT 0,
		claimed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		sent_at         TIMESTAMPTZ
	)`},
	{"add_send_ledger_command", `ALTER TABLE mailing_send_ledger ADD COLUMN IF NOT EXISTS command JSONB`},
	{"idx_send_ledger_claimed", `CREATE INDEX IF NOT EXISTS idx_send_ledger_claimed ON mailing_send_ledger (status, claimed_at) WHERE status='claimed'`},
	{"idx_send_ledger_redrive", `CREATE INDEX IF NOT EXISTS idx_send_ledger_redrive ON mailing_send_ledger (status, claimed_at) WHERE status IN ('claimed','failed','requeued')`},
	// ---- end Kafka send-queue ledger ----
	// NOT send-path, but here for the SAME ordering guarantee: cpmDealSelect
	// references d.end_date UNCONDITIONALLY, so the column must exist before the
	// binary serves the CPM planner (in runStartupMigrations it applied in a
	// background goroutine AFTER serving began → deploy-window 500s). Tiny,
	// idempotent, NULL-tolerant read (sql.NullTime).
	{"add_cpm_deals_end_date", `ALTER TABLE mailing_cpm_deals ADD COLUMN IF NOT EXISTS end_date DATE`},
	// ---- Attribution stamping (Offer Alignment PART A, 2026-07-07) ----
	// The wave dispatcher's queue-enqueue INSERTs and the send worker's
	// sent-event INSERT reference mailing_campaigns.{offer_id,creative_id,
	// subject_line_id} via scalar subqueries UNCONDITIONALLY (not behind any
	// kill switch), so these columns must exist before any worker starts.
	// offer_key/attribution_source ride along: the deploy-time stamp
	// (stampCampaignAttribution) writes them as soon as the API serves.
	// The dim tables the stamp UPSERTs into live in runStartupMigrations —
	// the stamp is log-and-continue, so the background slice is safe there.
	{"add_campaigns_offer_key", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS offer_key TEXT`},
	{"add_campaigns_creative_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS creative_id UUID`},
	{"add_campaigns_subject_line_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
	{"add_campaigns_attribution_source", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS attribution_source TEXT`},
	// The columns below PRE-EXIST in prod (added long ago via
	// runStartupMigrations, main.go add_campaigns_offer_id / add_queue_* /
	// add_tracking_* entries) but are ALSO referenced unconditionally by the
	// same enqueue/sent-event SQL. Duplicating them here is a no-op on prod
	// (skip-probe sees them) and closes the fresh-DB/DR boot race where a
	// worker could enqueue before the background migration goroutine lands
	// them. Keep both copies — the background entries backfill older paths.
	{"add_campaigns_offer_id_critical", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS offer_id UUID`},
	{"add_queue_offer_id_critical", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS offer_id UUID`},
	{"add_queue_creative_id_critical", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS creative_id UUID`},
	{"add_queue_subject_line_id_critical", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
	{"add_tracking_offer_id_critical", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS offer_id UUID`},
	{"add_tracking_creative_id_critical", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS creative_id UUID`},
	{"add_tracking_subject_line_id_critical", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
}

// ensureSendPathSchema applies criticalSendPathDDL synchronously with bounded
// retries. Returns true when every statement is verified applied. On false,
// workers still start (matching the platform's boot-resilience posture) but
// the failure is logged at CRITICAL so the first triage glance explains any
// claim/enqueue errors that follow.
func ensureSendPathSchema(db *sql.DB) bool {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		allApplied := true
		for _, m := range criticalSendPathDDL {
			if migrationSkipProbe(db, m.sql) {
				continue
			}
			conn, err := db.Conn(context.Background())
			if err != nil {
				log.Printf("[SendPathSchema] %s: connection failed: %v", m.name, err)
				allApplied = false
				continue
			}
			// Bounded lock wait: long enough to slot between claim batches,
			// short enough not to wedge boot behind a saturated primary.
			if _, err := conn.ExecContext(context.Background(), `SET lock_timeout = '8s'; SET statement_timeout = '20s'`); err != nil {
				log.Printf("[SendPathSchema] %s: session setup failed: %v", m.name, err)
			}
			if _, err := conn.ExecContext(context.Background(), m.sql); err != nil {
				log.Printf("[SendPathSchema] %s: attempt %d/%d failed: %v", m.name, attempt, attempts, err)
				allApplied = false
			} else {
				log.Printf("[SendPathSchema] %s: applied", m.name)
			}
			conn.Close()
		}
		if allApplied {
			return true
		}
		time.Sleep(time.Duration(attempt) * 5 * time.Second)
	}
	log.Printf("[SendPathSchema] CRITICAL: send-path-critical schema could not be verified after %d attempts — claims/enqueues WILL fail until it lands (see CAMPAIGN_QUEUE_STORAGE_REDESIGN.md §9)", attempts)
	return false
}

// concurrentIndexSpecs are indexes too large to build inside the 5s-per-
// statement migration runner. Each is built with CREATE INDEX CONCURRENTLY
// (never blocks writes) on a dedicated connection with statement_timeout=0,
// only after the primary's IO-wait backend count drops below
// concurrentIndexIOWaitMax — the same gate the queue HTML slimmer uses
// (worker/data_cleanup.go), per CAMPAIGN_QUEUE_STORAGE_REDESIGN.md §4.1
// ("build in a calmer IO window").
var concurrentIndexSpecs = []struct {
	name string
	sql  string
}{
	// Event-ingestion lookups (engine/ingest.go) filter
	// mailing_message_log on LOWER(email) and order by sent_at DESC.
	// Without this expression index each lookup is a ~13M-row seq scan
	// (~17s under load) — the dominant read amplifier in the 2026-06-09
	// IO-saturation incident.
	{"idx_message_log_lower_email_sent", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_message_log_lower_email_sent ON mailing_message_log (LOWER(email), sent_at DESC)`},
	// Partial index over snapshot-bearing queue rows: drives the snapshot
	// retention NOT EXISTS probe in data_cleanup.go without a queue scan.
	// Lives here (not the migrations slice) because even a partial-index
	// build scans the full multi-GB queue heap — far beyond the runner's
	// 5s statement budget.
	{"idx_queue_content_snapshot_id", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_queue_content_snapshot_id ON mailing_campaign_queue (content_snapshot_id) WHERE content_snapshot_id IS NOT NULL`},
	// Dashboard audience-growth card (dashboard_console_audience_growth.go v1.1):
	// acquisition = COUNT(created_at >= floor) per org, churn = COUNT(
	// unsubscribed_at >= floor). Without these the counts seq-scan the ~13M-row
	// subscriber table (~25s) and 500 on the 8s handler timeout. Composite
	// (org, ts) → range scan within the org; the churn index is partial (only
	// the small set of unsubscribed rows).
	{"idx_subscribers_org_created", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_subscribers_org_created ON mailing_subscribers (organization_id, created_at)`},
	{"idx_subscribers_churn_updated", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_subscribers_churn_updated ON mailing_subscribers (organization_id, updated_at) WHERE status IN ('bounced','complained','blacklisted','unsubscribed')`},
	// Dashboard Audience Growth "Activated" pool (2026-07-02): early-funnel
	// subscribers mailed ≤4 times who have opened or clicked. Partial index over
	// just that ~19k subset so the count is an index scan, not a 13M seq scan.
	{"idx_subscribers_activated", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_subscribers_activated ON mailing_subscribers (organization_id) WHERE total_emails_received BETWEEN 1 AND 4 AND (total_opens > 0 OR total_clicks > 0)`},

	// ── Repairs for the five INVALID leftovers from interrupted ad-hoc
	// CONCURRENTLY builds (prod inventory 2026-06-10, AAR action item 5;
	// ~3 GB of dead weight maintained on every write, the largest being
	// idx_engine_signals_recorded_isp at 1.9 GB). Their original builders
	// use IF NOT EXISTS, which no-ops against an invalid index forever —
	// this path drops the invalid leftover and rebuilds. Ordered smallest
	// first to bank wins between calm-window waits; definitions are
	// byte-identical to the originals elsewhere in this file.
	{"idx_mcq_dead_letter_recent", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mcq_dead_letter_recent ON mailing_campaign_queue (COALESCE(last_attempt_at, created_at) DESC) WHERE status IN ('dead_letter','dead_letter_strict')`},
	{"idx_sds_state_domain", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_sds_state_domain ON mailing_subscriber_domain_state (sending_domain, state)`},
	{"idx_mcq_accepted_html", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mcq_accepted_html ON mailing_campaign_queue (id) WHERE status = 'accepted' AND html_content IS NOT NULL`},
	{"idx_pmta_acct_raw_unprocessed", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pmta_acct_raw_unprocessed ON pmta_acct_raw (processed, received_at, id) WHERE processed = FALSE`},
	{"idx_engine_signals_recorded_isp", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_engine_signals_recorded_isp ON mailing_engine_signals (recorded_at, isp)`},

	// Email→subscriber resolution for manual offer-suppression uploads
	// (offer_suppression_upload_handlers.go) and the various
	// LOWER(email)-keyed lookups (unsubscribe, automation enrolment).
	// Without it each lookup seq-scans the full subscriber heap and dies on
	// the 30s connection statement_timeout (observed 2026-06-10 on the
	// offer-suppression upload endpoint). Expression must stay byte-identical
	// to the handlers' LOWER(TRIM(email)) predicate.
	{"idx_subscribers_lower_email", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_subscribers_lower_email ON mailing_subscribers (LOWER(TRIM(email)))`},

	// SAFEGUARD (b) — supporting index for the Kumo governed pass's per-
	// (property,vertical,ISP) daily count (governedDailyCount in
	// partner_drip_orchestrator.go). That COUNT runs once per positive-cap ISP
	// per governed wave on the >1M-row partner_clean_queue; without this it is a
	// full heap scan. Predicate: mailed_brand=$1 AND vertical=$2 AND isp_family
	// match AND mailed_at >= local-midnight. Partial on mailed_at IS NOT NULL
	// (only first-touched rows carry a mailed_at). Lives here (not the 5s
	// migration slice) because the build scans the full queue heap. Idempotent
	// (IF NOT EXISTS); additive — no existing query depends on it.
	{"idx_pcq_governed_daily_count", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pcq_governed_daily_count ON partner_clean_queue (mailed_brand, vertical, isp_family, mailed_at) WHERE mailed_at IS NOT NULL`},
}

const concurrentIndexIOWaitMax = 8

// ensureConcurrentIndexes builds the indexes above, skipping ones already
// valid and dropping invalid leftovers from interrupted CONCURRENTLY builds
// (an invalid index is both useless and write-amplifying). Failures are
// logged and retried on the next boot — never fatal.
func ensureConcurrentIndexes(db *sql.DB) {
	for _, spec := range concurrentIndexSpecs {
		var valid bool
		err := db.QueryRow(`
			SELECT i.indisvalid
			FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			WHERE c.relname = $1
		`, spec.name).Scan(&valid)
		switch {
		case err == nil && valid:
			continue // already built
		case err == nil && !valid:
			log.Printf("[ConcurrentIndex] %s exists but is INVALID (interrupted build) — dropping for rebuild", spec.name)
			if _, dropErr := db.Exec(`DROP INDEX IF EXISTS ` + spec.name); dropErr != nil {
				log.Printf("[ConcurrentIndex] drop of invalid %s failed: %v — skipping", spec.name, dropErr)
				continue
			}
		case err != sql.ErrNoRows:
			log.Printf("[ConcurrentIndex] validity probe for %s failed: %v — skipping", spec.name, err)
			continue
		}

		// Wait for a calm-IO window: the build itself is two full scans of
		// the table, and the whole point is not to pile onto saturation.
		// Bounded wait (~6h in 10-minute probes); give up until next boot.
		calm := false
		for attempt := 0; attempt < 36; attempt++ {
			var ioWait int
			probeErr := db.QueryRow(`
				SELECT COUNT(*)::int
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND state = 'active'
				  AND wait_event_type = 'IO'
				  AND pid <> pg_backend_pid()
			`).Scan(&ioWait)
			if probeErr == nil && ioWait < concurrentIndexIOWaitMax {
				calm = true
				break
			}
			log.Printf("[ConcurrentIndex] deferring %s — primary busy (io_wait_backends=%d, probe_err=%v); retry in 10m", spec.name, ioWait, probeErr)
			time.Sleep(10 * time.Minute)
		}
		if !calm {
			log.Printf("[ConcurrentIndex] %s: no calm IO window found — will retry next boot", spec.name)
			continue
		}

		conn, err := db.Conn(context.Background())
		if err != nil {
			log.Printf("[ConcurrentIndex] %s: dedicated connection failed: %v", spec.name, err)
			continue
		}
		if _, err := conn.ExecContext(context.Background(), `SET statement_timeout = 0`); err != nil {
			log.Printf("[ConcurrentIndex] %s: reset statement_timeout failed: %v", spec.name, err)
		}
		start := time.Now()
		log.Printf("[ConcurrentIndex] building %s (CONCURRENTLY — writes unblocked) ...", spec.name)
		if _, err := conn.ExecContext(context.Background(), spec.sql); err != nil {
			// An interrupted CONCURRENTLY build leaves an INVALID index;
			// the validity probe above cleans it up on the next boot.
			log.Printf("[ConcurrentIndex] %s build FAILED after %s: %v", spec.name, time.Since(start).Round(time.Second), err)
		} else {
			log.Printf("[ConcurrentIndex] %s built in %s", spec.name, time.Since(start).Round(time.Second))
		}
		conn.Close()
	}
}

// runStartupMigrations applies critical schema fixes that must run before
// the scheduler and send workers start. These are idempotent and safe to
// re-run on every boot. Uses a PostgreSQL advisory lock so only one ECS
// task runs migrations at a time during rolling deployments.
func runStartupMigrations(db *sql.DB) {
	const migrationLockID = 8675309 // arbitrary but stable
	// Hold the advisory lock on a DEDICATED connection pinned for the entire
	// run. The previous implementation took the lock through the pool: the
	// lock bound to whichever backend session served that one query, and the
	// connection went straight back to the pool (and could be closed —
	// silently releasing the lock). That is how two ECS tasks ran this slice
	// concurrently during the 2026-06-10 deploy and compounded the boot lock
	// storm; the deferred unlock also ran on a different pooled session and
	// leaked the lock. (AAR action item 1.)
	lockConn, lockConnErr := db.Conn(context.Background())
	acquired := true
	if lockConnErr != nil {
		log.Printf("[StartupMigration] dedicated lock connection failed: %v — running migrations anyway", lockConnErr)
		lockConn = nil
	} else {
		defer lockConn.Close()
		if err := lockConn.QueryRowContext(context.Background(),
			"SELECT pg_try_advisory_lock($1)", migrationLockID).Scan(&acquired); err != nil {
			log.Printf("[StartupMigration] advisory lock query failed: %v — running migrations anyway", err)
			acquired = true
		}
	}
	if !acquired {
		log.Println("[StartupMigration] Another instance holds the migration lock — skipping")
		return
	}
	if lockConn != nil {
		defer lockConn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)
	}
	migrations := []struct {
		name string
		sql  string
	}{
		// Worker heartbeat table — backs WorkerHealthMonitor stall detection
		// and the /api/worker-health UI. Placed first (new table, no deps,
		// fast) so heartbeats can land on the first cycle after boot.
		{"create_worker_heartbeats", `CREATE TABLE IF NOT EXISTS mailing_worker_heartbeats (
			worker_name               TEXT PRIMARY KEY,
			last_beat_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_status               TEXT NOT NULL DEFAULT 'ok',
			last_error                TEXT,
			cycle_count               BIGINT NOT NULL DEFAULT 0,
			expected_interval_seconds INTEGER NOT NULL DEFAULT 3600,
			stalled                   BOOLEAN NOT NULL DEFAULT FALSE,
			stalled_since             TIMESTAMPTZ,
			last_alerted_at           TIMESTAMPTZ,
			updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		// Run-level worker observability — one row per completed worker cycle
		// (written by RecordWorkerRun, internal/worker/worker_health.go).
		// Complements the heartbeat upsert above: heartbeats answer "is it
		// alive?", runs answer "what did each cycle actually do?". Surfaced by
		// GET /api/mailing/v2/segments/workers. Placed near the top of this
		// slice (new table, no deps, fast) because end-of-slice entries can be
		// skipped when the migration runner exhausts its boot time budget;
		// ops also applies this DDL manually, so it must stay idempotent.
		{"create_worker_runs", `CREATE TABLE IF NOT EXISTS mailing_worker_runs (
			id BIGSERIAL PRIMARY KEY,
			worker_name TEXT NOT NULL,
			started_at TIMESTAMPTZ NOT NULL,
			finished_at TIMESTAMPTZ NOT NULL,
			duration_ms INTEGER NOT NULL,
			status TEXT NOT NULL,
			items_processed INTEGER NOT NULL DEFAULT 0,
			items_failed INTEGER NOT NULL DEFAULT 0,
			detail TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_worker_runs_name_time", `CREATE INDEX IF NOT EXISTS idx_worker_runs_name_time ON mailing_worker_runs (worker_name, started_at DESC)`},
		// Consciousness hourly assessments — one row per scope (platform +
		// per-ISP family) per assessment interval, written by the engine's
		// assessment loop (internal/engine/consciousness_assessment.go) and
		// read by /api/mailing/consciousness/assessments|recommendations.
		// Replaces per-event thought persistence (Round-3 §3). Placed near the
		// top of this slice (new table, no deps, fast).
		{"create_consciousness_assessments", `CREATE TABLE IF NOT EXISTS mailing_consciousness_assessments (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			hour            TIMESTAMPTZ NOT NULL,
			scope           TEXT NOT NULL,
			posture         TEXT NOT NULL DEFAULT '',
			body            JSONB NOT NULL DEFAULT '{}',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_consciousness_assessments_org_scope_time", `CREATE INDEX IF NOT EXISTS idx_consciousness_assessments_org_scope_time ON mailing_consciousness_assessments (organization_id, scope, created_at DESC)`},
		// CPM Planner manual conversions — operator-uploaded conversion ground
		// truth for deals whose offers have no Everflow postback wiring.
		// source='csv' rows come from Everflow conversion-report exports
		// (deduped on conversion_id via the partial unique index below);
		// source='manual' rows are quick-adds ("N conversions on date D",
		// conversion_id=''). Read by cpm_planner_handlers.go loadProgress /
		// loadDailySeries to blend with tracked (postback) conversions.
		// New table, no deps, fast.
		{"create_cpm_manual_conversions", `CREATE TABLE IF NOT EXISTS mailing_cpm_manual_conversions (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			deal_id         UUID NOT NULL,
			converted_at    TIMESTAMPTZ NOT NULL,
			count           INTEGER NOT NULL DEFAULT 1,
			revenue         NUMERIC(12,2) NOT NULL DEFAULT 0,
			sub1            TEXT NOT NULL DEFAULT '',
			sub2            TEXT NOT NULL DEFAULT '',
			conversion_id   TEXT NOT NULL DEFAULT '',
			source          TEXT NOT NULL DEFAULT 'manual',
			note            TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_cpm_manual_conversions_org_deal_time", `CREATE INDEX IF NOT EXISTS idx_cpm_manual_conversions_org_deal_time ON mailing_cpm_manual_conversions (organization_id, deal_id, converted_at)`},
		{"uniq_cpm_manual_conversions_dedupe", `CREATE UNIQUE INDEX IF NOT EXISTS uniq_cpm_manual_conversions_dedupe ON mailing_cpm_manual_conversions (deal_id, conversion_id) WHERE conversion_id <> ''`},
		// NOTE: mailing_cpm_deals.end_date (the deal DEADLINE / "finish sooner"
		// lever) is applied SYNCHRONOUSLY in criticalSendPathDDL — cpmDealSelect
		// reads it unconditionally, so it must land before the binary serves
		// (moved out of this async slice 2026-07-02 to close a deploy-window 500).
		// Creative registry — browse/preview surface for the send-day creative
		// archive (ReviewForge phase 2). Synced from operator tooling via
		// /api/admin/creatives-sync; read by the portal Creative Studio tab.
		{"create_mailing_creatives", `CREATE TABLE IF NOT EXISTS mailing_creatives (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			offer_key       TEXT NOT NULL DEFAULT '',
			brand_code      TEXT NOT NULL,
			filename        TEXT NOT NULL,
			subject         TEXT NOT NULL DEFAULT '',
			preheader       TEXT NOT NULL DEFAULT '',
			html_content    TEXT NOT NULL,
			money_urls      INTEGER NOT NULL DEFAULT 0,
			tagged          BOOLEAN NOT NULL DEFAULT FALSE,
			source          TEXT NOT NULL DEFAULT 'manual',
			forge_brand_key TEXT NOT NULL DEFAULT '',
			sha256          TEXT NOT NULL DEFAULT '',
			generated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (organization_id, filename)
		)`},
		{"idx_mailing_creatives_offer", `CREATE INDEX IF NOT EXISTS idx_mailing_creatives_org_offer_brand ON mailing_creatives (organization_id, offer_key, brand_code, generated_at DESC)`},
		// Creative Studio v2: registry rows link to their Content Library
		// template so copy edits stay in sync; conversations for the embedded
		// creative agent live in their own tables (EDITH's list stays clean).
		{"add_mailing_creatives_template_id", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS template_id UUID`},
		// Creative Studio approval + money-link-test surface (creatives_registry.go
		// approve/reject/money-link-check endpoints). approval_status ∈
		// pending|approved|rejected; money_link_status ∈ untested|pass|warn|dead.
		// All idempotent ADD COLUMN IF NOT EXISTS — re-run-safe on every boot.
		{"add_mailing_creatives_approval_status", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS approval_status TEXT NOT NULL DEFAULT 'pending'`},
		{"add_mailing_creatives_approved_by", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS approved_by TEXT NOT NULL DEFAULT ''`},
		{"add_mailing_creatives_approved_at", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ`},
		{"add_mailing_creatives_money_link_status", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS money_link_status TEXT NOT NULL DEFAULT 'untested'`},
		{"add_mailing_creatives_money_link_checked_at", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS money_link_checked_at TIMESTAMPTZ`},
		{"add_mailing_creatives_money_link_detail", `ALTER TABLE mailing_creatives ADD COLUMN IF NOT EXISTS money_link_detail TEXT NOT NULL DEFAULT ''`},
		// PMTA deployment audit ledger — created here so it exists at boot; a
		// separate builder (the deploy path) WRITES the row at deploy time. One
		// immutable row per deployed campaign (campaign_id UNIQUE) capturing the
		// exact html sha256 + money links + recipient count that shipped.
		{"create_mailing_pmta_deployment_audits", `CREATE TABLE IF NOT EXISTS mailing_pmta_deployment_audits (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id      UUID NOT NULL UNIQUE,
			organization_id  UUID NOT NULL,
			offer_key        TEXT NOT NULL DEFAULT '',
			html_sha256      TEXT NOT NULL DEFAULT '',
			money_links      JSONB NOT NULL DEFAULT '[]',
			sending_domain   TEXT NOT NULL DEFAULT '',
			total_recipients INT NOT NULL DEFAULT 0,
			deployed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_pmta_audit_campaign", `CREATE INDEX IF NOT EXISTS idx_pmta_audit_campaign ON mailing_pmta_deployment_audits(campaign_id)`},
		{"idx_pmta_audit_org", `CREATE INDEX IF NOT EXISTS idx_pmta_audit_org ON mailing_pmta_deployment_audits(organization_id, deployed_at DESC)`},
		{"create_creative_agent_conversations", `CREATE TABLE IF NOT EXISTS creative_agent_conversations (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			title           TEXT NOT NULL DEFAULT '',
			message_count   INTEGER NOT NULL DEFAULT 0,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"create_creative_agent_messages", `CREATE TABLE IF NOT EXISTS creative_agent_messages (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			conversation_id UUID NOT NULL,
			role            TEXT NOT NULL,
			content         TEXT,
			tool_calls      JSONB,
			tool_call_id    TEXT,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_creative_agent_messages_convo", `CREATE INDEX IF NOT EXISTS idx_creative_agent_messages_convo ON creative_agent_messages (conversation_id, created_at)`},
		// Offer Proofs — Creative Studio "Offers" sub-view. Operator uploads a
		// network-approved HTML creative; the server rehosts its images to our CDN
		// and injects our footer/unsub, then the proof carries a manual approval
		// lifecycle (pending|approved|rejected), an active/inactive flag, and the
		// approved sending domains / ISPs / subject+preheader variants / from-names
		// that the operator signs off on. v1 is a REGISTRY (the send-day reads it);
		// nothing here gates HandleDeployCampaign. All additive + idempotent; off
		// the critical send path, so it lives in the normal migration slice.
		{"create_mailing_offer_proofs", `CREATE TABLE IF NOT EXISTS mailing_offer_proofs (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id  UUID NOT NULL,
			name             TEXT NOT NULL,
			offer_key        TEXT NOT NULL DEFAULT '',
			html_content     TEXT NOT NULL,
			html_raw         TEXT NOT NULL DEFAULT '',
			images_rehosted  INTEGER NOT NULL DEFAULT 0,
			approval_status  TEXT NOT NULL DEFAULT 'pending',
			is_active        BOOLEAN NOT NULL DEFAULT TRUE,
			approved_by      TEXT NOT NULL DEFAULT '',
			approved_at      TIMESTAMPTZ,
			variants         JSONB NOT NULL DEFAULT '[]',
			from_names       JSONB NOT NULL DEFAULT '[]',
			approved_domains JSONB NOT NULL DEFAULT '[]',
			approved_isps    JSONB NOT NULL DEFAULT '[]',
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (organization_id, name)
		)`},
		{"idx_mailing_offer_proofs_org", `CREATE INDEX IF NOT EXISTS idx_mailing_offer_proofs_org ON mailing_offer_proofs (organization_id, created_at DESC)`},
		// Account-manager contacts a proof can be emailed to for review. External
		// reviewers (name + email), NOT platform logins — kept separate from users.
		{"create_mailing_proof_recipients", `CREATE TABLE IF NOT EXISTS mailing_proof_recipients (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id  UUID NOT NULL,
			name             TEXT NOT NULL,
			email            TEXT NOT NULL,
			is_active        BOOLEAN NOT NULL DEFAULT TRUE,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (organization_id, email)
		)`},
		// Per-send audit of proof emails (which AM got which proof, when, status).
		{"create_mailing_proof_sends", `CREATE TABLE IF NOT EXISTS mailing_proof_sends (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id  UUID NOT NULL,
			proof_id         UUID NOT NULL,
			sending_domain   TEXT NOT NULL DEFAULT '',
			recipient_email  TEXT NOT NULL DEFAULT '',
			recipient_name   TEXT NOT NULL DEFAULT '',
			subject          TEXT NOT NULL DEFAULT '',
			from_name        TEXT NOT NULL DEFAULT '',
			status           TEXT NOT NULL DEFAULT '',
			message_id       TEXT NOT NULL DEFAULT '',
			error            TEXT NOT NULL DEFAULT '',
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_mailing_proof_sends_proof", `CREATE INDEX IF NOT EXISTS idx_mailing_proof_sends_proof ON mailing_proof_sends (organization_id, proof_id, created_at DESC)`},
		// Ensure tracking events table has all required columns
		// Ensure partition exists for current month
		{"create_tracking_partition_mar26", `CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_03 PARTITION OF mailing_tracking_events FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')`},
		{"ensure_tracking_email_col", `DO $$
		DECLARE
			part regclass;
		BEGIN
			BEGIN
				ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT;
			EXCEPTION WHEN OTHERS THEN
				NULL; -- partitioned table: column must be added per-partition
			END;
			FOR part IN
				SELECT inhrelid::regclass
				FROM pg_inherits
				WHERE inhparent = 'public.mailing_tracking_events'::regclass
			LOOP
				EXECUTE format('ALTER TABLE %s ADD COLUMN IF NOT EXISTS email TEXT', part);
			END LOOP;
		END $$`},
		{"add_tracking_event_time_col", `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS event_time TIMESTAMPTZ DEFAULT NOW(); EXCEPTION WHEN OTHERS THEN NULL; END $$`},
		{"add_tracking_metadata_col", `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}'; EXCEPTION WHEN OTHERS THEN NULL; END $$`},
		{"add_tracking_is_unique_col", `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS is_unique BOOLEAN DEFAULT false; EXCEPTION WHEN OTHERS THEN NULL; END $$`},
		// Drop restrictive event_type constraint so hard_bounce, soft_bounce, delivered etc. can be stored
		{"drop_tracking_evt_chk", `ALTER TABLE mailing_tracking_events DROP CONSTRAINT IF EXISTS mailing_tracking_events_event_type_check`},
		// Ensure inbox profiles has email and last_bounce_at columns
		{"add_inbox_email_col", `ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS email TEXT`},
		{"add_inbox_last_bounce_col", `ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS last_bounce_at TIMESTAMPTZ`},
		{"drop_status_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`},
		{"drop_type_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_campaign_type_check`},
		{"drop_send_type_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_send_type_check`},
		{"widen_status_col", `DO $$ BEGIN ALTER TABLE mailing_campaigns ALTER COLUMN status TYPE TEXT; EXCEPTION WHEN OTHERS THEN NULL; END $$`},
		// readd_status_chk whitelist MUST include 'finalizing_audience' because
		// reserveCampaignForDeploy() inserts new campaigns with that status
		// (see handlers_pmta_campaign.go). Every boot this runs after
		// drop_status_chk, so the end state is always the whitelist coded
		// here. Later v2 migrations (drop_status_chk_v2, readd_status_chk_v2,
		// drop_status_check_constraint) live ~2000 lines deeper and during
		// the Apr 20 2026 deploy they were unreachable — intermediate
		// migrations hit 5s statement_timeout and the runner stalled before
		// ever reaching them. Result: prod was stuck on the old whitelist
		// and every PMTA campaign POST threw 23514. Adding 'finalizing_audience'
		// here makes the early-boot path self-sufficient regardless of how
		// far down the migration list the runner actually gets.
		{"readd_status_chk", `ALTER TABLE mailing_campaigns ADD CONSTRAINT mailing_campaigns_status_check CHECK (status IN ('draft','scheduled','preparing','finalizing_audience','sending','paused','completed','completed_with_errors','cancelled','failed','deleted','sent'))`},
		{"add_queued_count", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS queued_count INTEGER DEFAULT 0`},
		{"add_list_ids", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS list_ids JSONB DEFAULT '[]'`},
		{"add_suppression_list_ids", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS suppression_list_ids JSONB DEFAULT '[]'`},
		{"add_suppression_segment_ids", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS suppression_segment_ids JSONB DEFAULT '[]'`},
		{"add_isp_quotas", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS isp_quotas JSONB DEFAULT '{}'`},
		{"add_execution_mode", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS execution_mode TEXT DEFAULT 'standard'`},
		{"drop_execution_mode_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_execution_mode_check`},
		{"readd_execution_mode_chk", `ALTER TABLE mailing_campaigns ADD CONSTRAINT mailing_campaigns_execution_mode_check CHECK (execution_mode IN ('standard', 'pmta_isp_wave'))`},
		{"add_hard_bounce_count", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS hard_bounce_count INTEGER DEFAULT 0`},
		{"add_soft_bounce_count", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS soft_bounce_count INTEGER DEFAULT 0`},
		{"create_automation_workflows", `CREATE TABLE IF NOT EXISTS mailing_automation_workflows (
			id UUID PRIMARY KEY,
			organization_id UUID NOT NULL,
			name TEXT NOT NULL,
			description TEXT DEFAULT '',
			trigger_type TEXT DEFAULT 'enrollment',
			trigger_config JSONB DEFAULT '{}',
			list_id UUID,
			status TEXT DEFAULT 'draft',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"create_automation_steps", `CREATE TABLE IF NOT EXISTS mailing_automation_steps (
			id UUID PRIMARY KEY,
			workflow_id UUID NOT NULL REFERENCES mailing_automation_workflows(id) ON DELETE CASCADE,
			step_order INTEGER DEFAULT 0,
			step_type TEXT NOT NULL,
			template_id UUID,
			wait_duration INTEGER DEFAULT 0,
			config JSONB DEFAULT '{}',
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"create_automation_enrollments", `CREATE TABLE IF NOT EXISTS mailing_automation_enrollments (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workflow_id UUID NOT NULL REFERENCES mailing_automation_workflows(id) ON DELETE CASCADE,
			subscriber_id UUID,
			email TEXT,
			current_step_id UUID,
			current_step INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			enrolled_at TIMESTAMPTZ DEFAULT NOW(),
			next_step_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			UNIQUE(workflow_id, subscriber_id)
		)`},
		{"add_automation_total_enrolled", `ALTER TABLE mailing_automation_workflows ADD COLUMN IF NOT EXISTS total_enrolled INTEGER DEFAULT 0`},
		{"add_automation_total_completed", `ALTER TABLE mailing_automation_workflows ADD COLUMN IF NOT EXISTS total_completed INTEGER DEFAULT 0`},
		{"add_enrollment_step_id", `ALTER TABLE mailing_automation_enrollments ADD COLUMN IF NOT EXISTS current_step_id UUID`},
		{"add_enrollment_unique", `CREATE UNIQUE INDEX IF NOT EXISTS idx_enrollment_workflow_sub ON mailing_automation_enrollments(workflow_id, subscriber_id)`},
		{"create_campaign_queue", `CREATE TABLE IF NOT EXISTS mailing_campaign_queue (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
			subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
			subject VARCHAR(500),
			html_content TEXT,
			plain_content TEXT,
			scheduled_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			priority INTEGER DEFAULT 5,
			predicted_open_prob DECIMAL(5,4),
			predicted_click_prob DECIMAL(5,4),
			predicted_revenue DECIMAL(10,4),
			status VARCHAR(20) DEFAULT 'queued',
			attempts INTEGER DEFAULT 0,
			last_attempt_at TIMESTAMPTZ,
			error_message TEXT,
			message_id VARCHAR(255),
			delivery_server_id UUID,
			sent_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			locked_at TIMESTAMPTZ,
			worker_id VARCHAR(100),
			isp_plan_id UUID,
			wave_id UUID,
			recipient_isp VARCHAR(50),
			selection_rank INTEGER,
			audience_source_type VARCHAR(30),
			audience_source_id UUID
		)`},
		{"create_campaign_queue_indexes", `
			CREATE INDEX IF NOT EXISTS idx_queue_campaign ON mailing_campaign_queue(campaign_id);
			CREATE INDEX IF NOT EXISTS idx_queue_status ON mailing_campaign_queue(status);
			CREATE INDEX IF NOT EXISTS idx_queue_scheduled ON mailing_campaign_queue(scheduled_at) WHERE status = 'queued';
			CREATE INDEX IF NOT EXISTS idx_queue_subscriber ON mailing_campaign_queue(subscriber_id)
		`},
		{"add_queue_locked_at", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ`},
		{"add_queue_worker_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS worker_id VARCHAR(100)`},
		{"add_queue_isp_plan_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS isp_plan_id UUID`},
		{"add_queue_wave_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS wave_id UUID`},
		{"add_queue_recipient_isp", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS recipient_isp VARCHAR(50)`},
		{"add_queue_selection_rank", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS selection_rank INTEGER`},
		{"add_queue_audience_source_type", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS audience_source_type VARCHAR(30)`},
		{"add_queue_audience_source_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS audience_source_id UUID`},
		// Durable injection outbox columns (2026-04-23). Additive only.
		// idempotency_key is nullable on existing rows and gets a unique partial
		// index so new inserts with a populated key are de-duplicated while
		// legacy rows without a key are untouched.
		{"outbox_add_idempotency_key", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS idempotency_key UUID`},
		{"outbox_add_next_attempt_at", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ`},
		{"outbox_add_submitted_at", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ`},
		{"outbox_add_pmta_response", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS pmta_response TEXT`},
		{"outbox_idempotency_unique_idx", `CREATE UNIQUE INDEX IF NOT EXISTS uq_mcq_idempotency_key ON mailing_campaign_queue (idempotency_key) WHERE idempotency_key IS NOT NULL`},
		{"outbox_stuck_submitting_idx", `CREATE INDEX IF NOT EXISTS idx_mcq_stuck_submitting ON mailing_campaign_queue (locked_at) WHERE status = 'submitting'`},
		{"outbox_next_attempt_idx", `CREATE INDEX IF NOT EXISTS idx_mcq_next_attempt ON mailing_campaign_queue (priority, created_at) WHERE status = 'failed_retryable' AND next_attempt_at IS NOT NULL`},
		{"outbox_message_id_idx", `CREATE INDEX IF NOT EXISTS idx_mcq_message_id ON mailing_campaign_queue (message_id) WHERE message_id IS NOT NULL`},
		// Expand the status CHECK constraint to accept the durable-outbox
		// state-machine values (submitting, accepted, failed_retryable,
		// failed_permanent) alongside every value currently in production use.
		// This runs BEFORE any code writes these states — the state machine is
		// gated behind OutboxMode=durable which ships legacy by default — so
		// expanding the constraint now is harmless and unblocks the flip.
		{"outbox_drop_old_status_chk", `ALTER TABLE mailing_campaign_queue DROP CONSTRAINT IF EXISTS mailing_campaign_queue_status_check`},
		{"outbox_add_expanded_status_chk", `ALTER TABLE mailing_campaign_queue ADD CONSTRAINT mailing_campaign_queue_status_check CHECK (
			status::text IN (
				'queued','claimed','sending','sent','failed','skipped','dead_letter',
				'submitting','accepted','failed_retryable','failed_permanent',
				'dead_letter_strict','pending','processing'
			)
		)`},
		{"create_pmta_isp_plans", `CREATE TABLE IF NOT EXISTS mailing_campaign_isp_plans (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
			organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			isp VARCHAR(50) NOT NULL,
			sending_domain VARCHAR(255) NOT NULL,
			sending_profile_id UUID REFERENCES mailing_sending_profiles(id) ON DELETE SET NULL,
			quota INTEGER DEFAULT 0,
			randomize_audience BOOLEAN DEFAULT FALSE,
			throttle_strategy VARCHAR(50) DEFAULT 'auto',
			selection_strategy VARCHAR(50) DEFAULT 'priority_first',
			priority_strategy VARCHAR(50) DEFAULT 'selection_rank',
			timezone VARCHAR(80) DEFAULT 'UTC',
			status VARCHAR(30) DEFAULT 'planned',
			audience_estimated_count INTEGER DEFAULT 0,
			audience_selected_count INTEGER DEFAULT 0,
			enqueued_count INTEGER DEFAULT 0,
			sent_count INTEGER DEFAULT 0,
			failed_count INTEGER DEFAULT 0,
			config_snapshot JSONB DEFAULT '{}',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"create_pmta_isp_plan_indexes", `CREATE INDEX IF NOT EXISTS idx_campaign_isp_plans_campaign ON mailing_campaign_isp_plans(campaign_id); CREATE INDEX IF NOT EXISTS idx_campaign_isp_plans_status ON mailing_campaign_isp_plans(status, isp)`},
		{"create_pmta_isp_spans", `CREATE TABLE IF NOT EXISTS mailing_campaign_isp_time_spans (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			isp_plan_id UUID NOT NULL REFERENCES mailing_campaign_isp_plans(id) ON DELETE CASCADE,
			campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
			span_type VARCHAR(20) DEFAULT 'absolute',
			day_of_week VARCHAR(20),
			start_hour INTEGER,
			end_hour INTEGER,
			start_at TIMESTAMPTZ NOT NULL,
			end_at TIMESTAMPTZ NOT NULL,
			timezone VARCHAR(80) DEFAULT 'UTC',
			source VARCHAR(50) DEFAULT 'manual',
			sort_order INTEGER DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"create_pmta_isp_spans_index", `CREATE INDEX IF NOT EXISTS idx_campaign_isp_time_spans_plan ON mailing_campaign_isp_time_spans(isp_plan_id, sort_order)`},
		{"create_pmta_waves", `CREATE TABLE IF NOT EXISTS mailing_campaign_waves (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
			isp_plan_id UUID NOT NULL REFERENCES mailing_campaign_isp_plans(id) ON DELETE CASCADE,
			wave_number INTEGER NOT NULL,
			scheduled_at TIMESTAMPTZ NOT NULL,
			window_start_at TIMESTAMPTZ NOT NULL,
			window_end_at TIMESTAMPTZ NOT NULL,
			cadence_minutes INTEGER DEFAULT 0,
			batch_size INTEGER DEFAULT 0,
			planned_recipients INTEGER DEFAULT 0,
			enqueued_recipients INTEGER DEFAULT 0,
			status VARCHAR(30) DEFAULT 'planned',
			idempotency_key VARCHAR(255) NOT NULL,
			sqs_message_id VARCHAR(255),
			started_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			failed_at TIMESTAMPTZ,
			last_error TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE (isp_plan_id, wave_number),
			UNIQUE (idempotency_key)
		)`},
		{"create_pmta_waves_indexes", `CREATE INDEX IF NOT EXISTS idx_campaign_waves_due ON mailing_campaign_waves(status, scheduled_at); CREATE INDEX IF NOT EXISTS idx_campaign_waves_campaign ON mailing_campaign_waves(campaign_id, isp_plan_id)`},
		{"create_pmta_plan_recipients", `CREATE TABLE IF NOT EXISTS mailing_campaign_plan_recipients (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
			isp_plan_id UUID NOT NULL REFERENCES mailing_campaign_isp_plans(id) ON DELETE CASCADE,
			wave_id UUID REFERENCES mailing_campaign_waves(id) ON DELETE SET NULL,
			subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
			email VARCHAR(255) NOT NULL,
			recipient_isp VARCHAR(50) NOT NULL,
			selection_rank INTEGER NOT NULL,
			audience_source_type VARCHAR(30) NOT NULL,
			audience_source_id UUID,
			status VARCHAR(20) DEFAULT 'selected',
			queued_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE (isp_plan_id, subscriber_id)
		)`},
		{"create_pmta_plan_recipients_indexes", `CREATE INDEX IF NOT EXISTS idx_campaign_plan_recipients_plan ON mailing_campaign_plan_recipients(isp_plan_id, status, selection_rank); CREATE INDEX IF NOT EXISTS idx_campaign_plan_recipients_wave ON mailing_campaign_plan_recipients(wave_id)`},
		{"create_queue_wave_indexes", `CREATE INDEX IF NOT EXISTS idx_queue_wave_id ON mailing_campaign_queue(wave_id); CREATE INDEX IF NOT EXISTS idx_queue_plan_id ON mailing_campaign_queue(isp_plan_id); CREATE INDEX IF NOT EXISTS idx_queue_campaign_wave_schedule ON mailing_campaign_queue(campaign_id, wave_id, scheduled_at)`},
		{"create_suppressions_table", `CREATE TABLE IF NOT EXISTS mailing_suppressions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			email VARCHAR(255) NOT NULL,
			reason TEXT,
			source VARCHAR(50),
			active BOOLEAN DEFAULT true,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			CONSTRAINT mailing_suppressions_email_key UNIQUE (email)
		)`},
		{"create_suppressions_index", `CREATE INDEX IF NOT EXISTS idx_suppressions_active_email ON mailing_suppressions(email) WHERE active = true`},
		// Brand-scoped suppression — subscribers who unsubscribe from the TOP
		// link of a brand's email (e.g. *.discountblog.com) stay mailable for
		// other brands. Keyed by (org, email_hash, brand_root) with the same
		// MD5 convention as mailing_global_suppressions for hub parity.
		{"create_domain_suppressions_table", `CREATE TABLE IF NOT EXISTS mailing_domain_suppressions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			email VARCHAR(255) NOT NULL,
			email_hash VARCHAR(64) NOT NULL,
			brand_root VARCHAR(253) NOT NULL,
			reason VARCHAR(50) NOT NULL,
			source VARCHAR(50),
			isp VARCHAR(50),
			source_ip INET,
			campaign_id UUID,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			CONSTRAINT mailing_domain_suppressions_unique UNIQUE (organization_id, email_hash, brand_root)
		)`},
		{"create_domain_suppressions_lookup_idx", `CREATE INDEX IF NOT EXISTS idx_domsupp_lookup ON mailing_domain_suppressions (brand_root, email_hash)`},
		{"create_domain_suppressions_org_idx", `CREATE INDEX IF NOT EXISTS idx_domsupp_org ON mailing_domain_suppressions (organization_id, created_at DESC)`},
		// Supporting partial indexes so complete_finished_campaigns' (and reset_orphaned_sending's)
		// anti-joins are tiny index probes instead of scanning every queue/wave row per campaign.
		// Without them the cleanup cost ~79k and timed out every boot (and on the old shared-conn
		// runner that silently blocked later migrations). Partial = only the ~78k active queue rows /
		// ~20k active wave rows out of 10.9M / 1.9M total. Built CONCURRENTLY on prod out-of-band.
		//
		// Guarded by a pg_indexes catalog check, NOT plain CREATE INDEX IF NOT EXISTS: the latter
		// grabs a SHARE lock on the table BEFORE checking existence, so on these constantly-written
		// tables even the no-op path blocked on the hot-table lock and got cancelled at 5s every boot.
		// The catalog check takes no table lock on the skip path (prod), and only CREATEs on a fresh
		// DB where the tables are empty and the brief lock is free.
		{"idx_queue_active_by_campaign", `DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_queue_active_by_campaign') THEN
				CREATE INDEX idx_queue_active_by_campaign ON mailing_campaign_queue(campaign_id) WHERE status IN ('queued','sending','claimed');
			END IF;
		END $$`},
		{"idx_waves_active_by_campaign", `DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_waves_active_by_campaign') THEN
				CREATE INDEX idx_waves_active_by_campaign ON mailing_campaign_waves(campaign_id) WHERE status IN ('planned','enqueuing','dispatched');
			END IF;
		END $$`},
		// Mark genuinely-finished 'sending' campaigns 'sent': no active queue rows, no active waves,
		// and >=1 completed wave. With the indexes above the plan is ~48ms. FOR UPDATE ... SKIP LOCKED
		// so we never block on a campaign row the live app is mutating — those are simply skipped and
		// caught on a later boot. Without it the UPDATE waited >5s on row locks and got cancelled
		// every boot (the "canceling statement due to user request" we saw); idempotent + eventually
		// consistent across boots.
		{"complete_finished_campaigns", `UPDATE mailing_campaigns SET status = 'sent', completed_at = COALESCE(completed_at, NOW()), updated_at = NOW()
			WHERE id IN (
				SELECT c.id FROM mailing_campaigns c
				WHERE c.status = 'sending'
				AND NOT EXISTS (SELECT 1 FROM mailing_campaign_queue q WHERE q.campaign_id = c.id AND q.status IN ('queued','sending','claimed'))
				AND NOT EXISTS (SELECT 1 FROM mailing_campaign_waves w WHERE w.campaign_id = c.id AND w.status IN ('planned','enqueuing','dispatched'))
				AND EXISTS (SELECT 1 FROM mailing_campaign_waves w2 WHERE w2.campaign_id = c.id AND w2.status = 'completed')
				FOR UPDATE OF c SKIP LOCKED
			)`},
		{"reset_orphaned_sending_v3", `UPDATE mailing_campaigns SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
			WHERE status = 'sending'
			AND NOT EXISTS (SELECT 1 FROM mailing_campaign_queue q WHERE q.campaign_id = mailing_campaigns.id AND q.status IN ('queued','sending','claimed'))
			AND NOT EXISTS (SELECT 1 FROM mailing_campaign_waves w WHERE w.campaign_id = mailing_campaigns.id AND w.status IN ('planned','enqueuing','dispatched'))
			AND started_at < NOW() - INTERVAL '24 hours'`},
		{"unstick_locked_queue_items", `UPDATE mailing_campaign_queue SET status = 'queued', worker_id = NULL, locked_at = NULL WHERE status = 'sending' AND locked_at < NOW() - INTERVAL '10 minutes'`},
		// Seed IP pools and warmup IPs (originally in migration 030, may not exist in production RDS)
		{"seed_warmup_pool", `INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status)
			VALUES ('00000000-0000-0000-0000-000000000001', 'warmup-pool', '4 IPs for warmup rotation (.177-.180)', 'warmup', 'active')
			ON CONFLICT (organization_id, name) DO UPDATE SET description = '4 IPs for warmup rotation (.177-.180)', updated_at = NOW()`},
		{"seed_default_pool", `INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status)
			VALUES ('00000000-0000-0000-0000-000000000001', 'default-pool', 'All 16 IPs — production sending', 'dedicated', 'active')
			ON CONFLICT (organization_id, name) DO NOTHING`},
		{"seed_warmup_ips", `DO $$
		DECLARE
			org_id UUID := '00000000-0000-0000-0000-000000000001';
			wp_id UUID;
		BEGIN
			SELECT id INTO wp_id FROM mailing_ip_pools WHERE organization_id = org_id AND name = 'warmup-pool';
			IF wp_id IS NOT NULL THEN
				INSERT INTO mailing_ip_addresses (organization_id, ip_address, hostname, pool_id, acquisition_type, hosting_provider, cidr_block, status, warmup_stage, warmup_day, warmup_daily_limit, rdns_verified, reputation_score)
				VALUES
					(org_id, '15.204.22.176', 'mta1.mail.projectjarvis.io', wp_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
					(org_id, '15.204.22.177', 'mta2.mail.projectjarvis.io', wp_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
					(org_id, '15.204.22.178', 'mta3.mail.projectjarvis.io', wp_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0),
					(org_id, '15.204.22.179', 'mta4.mail.projectjarvis.io', wp_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0)
				ON CONFLICT DO NOTHING;
			END IF;
		END $$`},
		// Ensure api_endpoint column exists before referencing it
		{"add_api_endpoint_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS api_endpoint VARCHAR(500)`},
		// One-time fix: profiles seeded with wrong host 178.128.215.13 → correct OVH PMTA server
		{"fix_pmta_host_178_to_ovh", `UPDATE mailing_sending_profiles SET smtp_host = '15.204.101.125', api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE vendor_type = 'pmta' AND smtp_host = '178.128.215.13'`},
		// Ensure PMTA profiles with correct host also have api_endpoint set
		{"set_api_endpoint_for_ovh", `UPDATE mailing_sending_profiles SET api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE vendor_type = 'pmta' AND smtp_host = '15.204.101.125' AND (api_endpoint IS NULL OR api_endpoint = '')`},
		{"fix_pmta_bridge_port_19099", `UPDATE mailing_sending_profiles SET api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE vendor_type = 'pmta' AND smtp_host = '15.204.101.125' AND api_endpoint != 'http://15.204.101.125:19099'`},
		// Seed quizfiesta.com profile if not present
		{"seed_quizfiesta_profile", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'QuizFiesta PMTA', 'pmta', 'QuizFiesta', 'hello@em.quizfiesta.com', 'reply@em.quizfiesta.com', 'em.quizfiesta.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.quizfiesta.com', 3200, 25000, 'warmup-pool', 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.quizfiesta.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		// === KumoMTA Colo1 (OVH 40.160.129.116) infrastructure + per-brand profiles ===
		// Server + per-brand single-IP pools + IPs + sending profiles. Profiles are
		// vendor_type='pmta' (so every deploy/preflight/wave gate accepts them) with
		// routing_mode='kumo' so ProfileBasedSender uses the KumoMTA injector
		// (X-Virtual-MTA content header). pool_prefix='<brand>' -> '<brand>-general-pool'
		// (one dedicated IP) -> vmtaShortName(hostname)=mtaN -> Kumo egress source mtaN.
		{"seed_kumo_server", `INSERT INTO mailing_pmta_servers (id, organization_id, name, host, smtp_port, mgmt_port, provider, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'KumoMTA Colo1 (OVH)', '40.160.129.116', 25, 19099, 'OVH-Kumo', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_pmta_servers WHERE host = '40.160.129.116' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_kumo_pool_mpf", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mpf-general-pool', 'KumoMTA em.mypersonalfinancial.com', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mpf-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_kumo_pool_pmd", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'pmd-general-pool', 'KumoMTA em.paymydebit.com', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'pmd-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_kumo_pool_trb", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'trb-general-pool', 'KumoMTA em.theretirementblog.com', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'trb-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_kumo_ip_mpf", `INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, acquisition_type, hosting_provider, pmta_server_id, pool_id, status, warmup_stage, warmup_daily_limit, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', '51.81.135.220'::inet, 'mta1.mail.em.mypersonalfinancial.com', 'leased', 'OVH', (SELECT id FROM mailing_pmta_servers WHERE host = '40.160.129.116' AND organization_id = '00000000-0000-0000-0000-000000000001' LIMIT 1), (SELECT id FROM mailing_ip_pools WHERE name = 'mpf-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001' LIMIT 1), 'warmup', 'warming', 200, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_addresses WHERE ip_address = '51.81.135.220'::inet)`},
		{"seed_kumo_ip_pmd", `INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, acquisition_type, hosting_provider, pmta_server_id, pool_id, status, warmup_stage, warmup_daily_limit, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', '51.81.135.221'::inet, 'mta2.mail.em.paymydebit.com', 'leased', 'OVH', (SELECT id FROM mailing_pmta_servers WHERE host = '40.160.129.116' AND organization_id = '00000000-0000-0000-0000-000000000001' LIMIT 1), (SELECT id FROM mailing_ip_pools WHERE name = 'pmd-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001' LIMIT 1), 'warmup', 'warming', 200, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_addresses WHERE ip_address = '51.81.135.221'::inet)`},
		{"seed_kumo_ip_trb", `INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, acquisition_type, hosting_provider, pmta_server_id, pool_id, status, warmup_stage, warmup_daily_limit, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', '51.81.135.222'::inet, 'mta3.mail.em.theretirementblog.com', 'leased', 'OVH', (SELECT id FROM mailing_pmta_servers WHERE host = '40.160.129.116' AND organization_id = '00000000-0000-0000-0000-000000000001' LIMIT 1), (SELECT id FROM mailing_ip_pools WHERE name = 'trb-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001' LIMIT 1), 'warmup', 'warming', 200, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_addresses WHERE ip_address = '51.81.135.222'::inet)`},
		{"seed_kumo_profile_mpf", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, pool_prefix, routing_mode, via_ses, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'MyPersonalFinancial (KumoMTA)', 'pmta', 'My Personal Financial', 'hello@em.mypersonalfinancial.com', 'reply@em.mypersonalfinancial.com', 'em.mypersonalfinancial.com', '40.160.129.116', 587, 'http://40.160.129.116:19099', 'projectjarvis.io', 3200, 25000, 'mpf', 'kumo', false, 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.mypersonalfinancial.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_kumo_profile_pmd", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, pool_prefix, routing_mode, via_ses, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'PayMyDebit (KumoMTA)', 'pmta', 'PayMyDebit', 'hello@em.paymydebit.com', 'reply@em.paymydebit.com', 'em.paymydebit.com', '40.160.129.116', 587, 'http://40.160.129.116:19099', 'projectjarvis.io', 3200, 25000, 'pmd', 'kumo', false, 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.paymydebit.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_kumo_profile_trb", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, pool_prefix, routing_mode, via_ses, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'TheRetirementBlog (KumoMTA)', 'pmta', 'The Retirement Blog', 'hello@em.theretirementblog.com', 'reply@em.theretirementblog.com', 'em.theretirementblog.com', '40.160.129.116', 587, 'http://40.160.129.116:19099', 'projectjarvis.io', 3200, 25000, 'trb', 'kumo', false, 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.theretirementblog.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},

		// === Kumo property governor (partner-drip per-property volume governor) ===
		// The 9 Kumo properties subscribe to the 3 Sam's offer-backed verticals and
		// take a GOVERNED slice of each vertical's existing audience: 500 records per
		// ISP per vertical per day, gmail held (cap 0), paced over a 6h window via the
		// orchestrator's per-wave cap. Operator-settable WITHOUT a deploy (the
		// orchestrator reloads this table every tick): change per_isp_daily_cap,
		// window_hours, gmail_held, per_isp_overrides (JSONB isp->cap), or
		// subscribed_verticals (TEXT[]) per brand and it takes effect next tick.
		// App-user-owned table; idempotent INSERT...WHERE NOT EXISTS never clobbers
		// operator edits on re-run.
		{"create_partner_property_governor", `CREATE TABLE IF NOT EXISTS partner_property_governor (
			brand_code           TEXT PRIMARY KEY,
			per_isp_daily_cap    INTEGER NOT NULL DEFAULT 500,
			window_hours         INTEGER NOT NULL DEFAULT 6,
			gmail_held           BOOLEAN NOT NULL DEFAULT TRUE,
			per_isp_overrides    JSONB NOT NULL DEFAULT '{}'::jsonb,
			subscribed_verticals TEXT[] NOT NULL DEFAULT ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[],
			active               BOOLEAN NOT NULL DEFAULT TRUE,
			updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"seed_governor_mpf", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'mpf', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'mpf')`},
		{"seed_governor_pmd", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'pmd', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'pmd')`},
		{"seed_governor_trb", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'trb', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'trb')`},
		{"seed_governor_bcc", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'bcc', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'bcc')`},
		{"seed_governor_usf", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'usf', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'usf')`},
		{"seed_governor_yfb", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'yfb', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'yfb')`},
		{"seed_governor_hlj", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'hlj', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'hlj')`},
		{"seed_governor_fth", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'fth', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'fth')`},
		{"seed_governor_htm", `INSERT INTO partner_property_governor (brand_code, per_isp_daily_cap, window_hours, gmail_held, subscribed_verticals, active) SELECT 'htm', 500, 6, TRUE, ARRAY['samsclub_internal','direct_offer','clickers_samsclub']::text[], TRUE WHERE NOT EXISTS (SELECT 1 FROM partner_property_governor WHERE brand_code = 'htm')`},
		// Governed-pass rotation pointers (Option B): one '<vertical>:governed'
		// partner_drip_state row per governed-subscribed vertical, so the governed
		// brand round-robin starts deterministically at index 0 and is fully
		// independent of the bare-vertical (welcome) and 'followup' pointers.
		// Idempotent: ON CONFLICT (vertical) DO NOTHING never clobbers a live
		// pointer. (refreshGovernedVerticalState/updateDripState also create/advance
		// these rows defensively, so the seed is belt-and-suspenders.)
		{"seed_governed_state_samsclub_internal", `INSERT INTO partner_drip_state (vertical, next_brand_index) VALUES ('samsclub_internal:governed', 0) ON CONFLICT (vertical) DO NOTHING`},
		{"seed_governed_state_direct_offer", `INSERT INTO partner_drip_state (vertical, next_brand_index) VALUES ('direct_offer:governed', 0) ON CONFLICT (vertical) DO NOTHING`},
		{"seed_governed_state_clickers_samsclub", `INSERT INTO partner_drip_state (vertical, next_brand_index) VALUES ('clickers_samsclub:governed', 0) ON CONFLICT (vertical) DO NOTHING`},
		// Seed em.discountblog.com PMTA profile (mirrors em.quizfiesta.com setup)
		{"seed_pmta_discountblog_profile", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'DiscountBlog PMTA (em)', 'pmta', 'Jamie @ Discount Blog', 'hello@em.discountblog.com', 'reply@em.discountblog.com', 'em.discountblog.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.discountblog.com', 3200, 25000, 'warmup-pool', 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.discountblog.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},

		// === SES Tenant-Aware Routing (additive — Dedicated stays default) ===
		// Per the SES Config Sets + Tenants plan (jun 2026). These columns
		// are NULL on every existing row (default false / null) so all
		// current send paths are unchanged. Only profiles with
		// via_ses=true cause send_worker.go to inject X-SES-* headers
		// and strip internal /track/click + /track/open rewrites.
		{"add_via_ses_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS via_ses BOOLEAN NOT NULL DEFAULT FALSE`},
		{"add_ses_configuration_set_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS ses_configuration_set TEXT`},
		{"add_ses_tenant_name_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS ses_tenant_name TEXT`},

		// Discount Blog (SES Tenant) — SECOND profile for em.discountblog.com
		// brand. Keyed off NAME (not sending_domain) so it coexists with
		// the legacy SES Relay profile at the same m.discountblog.com
		// sending_domain. is_default=false + sending_domain=m.* keeps it
		// out of the by-domain auto-resolve race per the 2026-05-30 hijack
		// fix; campaign deploys must pin sending_profile_id explicitly.
		{"seed_pmta_db_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Discount Blog (SES Tenant)', 'pmta', 'Jamie @ Discount Blog',
				'hello@em.discountblog.com', 'reply@em.discountblog.com',
				'm.discountblog.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.discountblog.com',
				1000, 25000, 'db-ses-pool', 'active', false,
				true, 'discountblog', 'discountblog',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Discount Blog (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},

		// Refinance Rates USA (SES Tenant) — first SES-route profile for RR.
		// No legacy m.refinanceratesusa.com profile exists yet; this seed
		// also creates the m.* sending domain row.
		{"seed_pmta_rr_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Refinance Rates USA (SES Tenant)', 'pmta', 'Frank @ Refinance Rates USA',
				'hello@em.refinanceratesusa.com', 'reply@em.refinanceratesusa.com',
				'm.refinanceratesusa.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.refinanceratesusa.com',
				1000, 25000, 'rr-ses-pool', 'active', false,
				true, 'refinanceratesusa', 'refinanceratesusa',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Refinance Rates USA (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},

		// Sending-domain rows for the SES tenant routes. These are needed
		// because the campaign builder validates sending_domain against
		// mailing_sending_domains. m.discountblog.com is already seeded
		// above (seed_ses_discountblog_domain) — only the RR row is new.
		{"seed_ses_rr_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.refinanceratesusa.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.refinanceratesusa.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},

		// History Thinking (SES Tenant) — SECOND profile for em.historythinking.com brand.
		// PMTA Server B (15.204.107.107). Keeps existing trk.em.* tracking host
		// for unsubscribe links; SES handles open/click via redirect domain
		// s.em.historythinking.com (set on the SES configuration set, not here).
		{"seed_pmta_ht_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'History Thinking (SES Tenant)', 'pmta', 'History Thinking',
				'hello@em.historythinking.com', 'reply@em.historythinking.com',
				'm.historythinking.com', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 'trk.em.historythinking.com',
				1000, 25000, 'ht-ses-pool', 'active', false,
				true, 'historythinking', 'historythinking',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'History Thinking (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},

		// My Own Health (SES Tenant) — SECOND profile for em.myownhealth.net brand.
		// PMTA Server B (15.204.107.107). Keeps existing trk.em.* tracking host.
		{"seed_pmta_mh_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'My Own Health (SES Tenant)', 'pmta', 'Arnold @ My Own Health',
				'hello@em.myownhealth.net', 'reply@em.myownhealth.net',
				'm.myownhealth.net', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 'trk.em.myownhealth.net',
				1000, 25000, 'mh-ses-pool', 'active', false,
				true, 'myownhealth', 'myownhealth',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'My Own Health (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},

		// Corrective re-pin: the HT/MH SES-Tenant profiles were created (Jun-03
		// cutover) with ip_pool='ses-relay-b', but PMTA Server B has NO virtual-mta
		// or virtual-mta-pool named 'ses-relay-b' — only the per-brand tenant pools
		// 'ht-ses-pool' / 'mh-ses-pool' (and a generic 'ses-relay-pool'). The send
		// worker's bare-pool path (esp_profile.go) passes ip_pool verbatim as the
		// PMTA VMTA, so every message was rejected with
		//   554 5.5.0 specified Virtual MTA 'ses-relay-b' does not exist
		// stranding ~23k recipients/day with zero delivery. The seed INSERTs above
		// can't fix an already-existing row (WHERE NOT EXISTS), so this UPDATE
		// corrects the live rows to the tenant pools that actually exist on Server B
		// (matching the 6 sibling Server-B brands that deliver fine). Idempotent.
		{"fix_ht_mh_ses_tenant_ip_pool", `UPDATE mailing_sending_profiles
			SET ip_pool = CASE sending_domain
				WHEN 'm.historythinking.com' THEN 'ht-ses-pool'
				WHEN 'm.myownhealth.net'     THEN 'mh-ses-pool'
			END, updated_at = NOW()
			WHERE sending_domain IN ('m.historythinking.com','m.myownhealth.net')
			  AND via_ses = TRUE
			  AND ip_pool = 'ses-relay-b'
			  AND organization_id = '00000000-0000-0000-0000-000000000001'`},

		// Quiz Fiesta (SES Tenant) — SECOND profile for em.quizfiesta.com brand.
		// PMTA Server A (15.204.101.125), same as DB.
		{"seed_pmta_qf_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Quiz Fiesta (SES Tenant)', 'pmta', 'Quiz Master',
				'hello@em.quizfiesta.com', 'reply@em.quizfiesta.com',
				'm.quizfiesta.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.quizfiesta.com',
				1000, 25000, 'qf-ses-pool', 'active', false,
				true, 'quizfiesta', 'quizfiesta',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Quiz Fiesta (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},

		// Business Weekly Pro / Financial Calculate / Consumer Pro (SES Tenant) — jun05 cutover.
		// SECOND profiles for the em.<brand> brands. PMTA Server A (15.204.101.125).
		// SES side (config-set <brand_root> + SNS event dest + tenant + identity/cs assoc)
		// provisioned jun05 via .scratch/jun05_ses_wire_brands.py. PMTA VMTA relay pools
		// <prefix>-ses-pool appended to Server A /etc/pmta/config in the same cutover.
		// is_default=false + sending_domain=m.* keeps these out of the by-domain auto-resolve
		// race; campaign deploys pin sending_profile_id explicitly.
		{"seed_pmta_bw_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Business Weekly Pro (SES Tenant)', 'pmta', 'Marcus @ Business Weekly Pro',
				'hello@em.businessweeklypro.com', 'reply@em.businessweeklypro.com',
				'm.businessweeklypro.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.businessweeklypro.com',
				1000, 25000, 'bw-ses-pool', 'active', false,
				true, 'businessweeklypro', 'businessweeklypro',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Business Weekly Pro (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_fc_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Financial Calculate (SES Tenant)', 'pmta', 'Eleanor @ Financial Calculate',
				'hello@em.financialcalculate.com', 'reply@em.financialcalculate.com',
				'm.financialcalculate.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.financialcalculate.com',
				1000, 25000, 'fc-ses-pool', 'active', false,
				true, 'financialcalculate', 'financialcalculate',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Financial Calculate (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_cp_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Consumer Pro (SES Tenant)', 'pmta', 'Diane @ Consumer Pro',
				'hello@em.consumerpro.net', 'reply@em.consumerpro.net',
				'm.consumerpro.net', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.consumerpro.net',
				1000, 25000, 'cp-ses-pool', 'active', false,
				true, 'consumerpro', 'consumerpro',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Consumer Pro (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// Sending-domain rows for the new SES tenant routes (campaign builder validates
		// sending_domain against mailing_sending_domains). m.<brand> need not be SES-verified
		// — SES authorizes against the From identity em.<brand> (verified jun03/earlier).
		{"seed_ses_bw_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.businessweeklypro.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.businessweeklypro.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_fc_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.financialcalculate.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.financialcalculate.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_cp_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.consumerpro.net', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.consumerpro.net' AND organization_id = '00000000-0000-0000-0000-000000000001')`},

		// ===== Warming brands SES Tenant cutover (jun05) — all 8 remaining brands =====
		// "Everything via SES" directive (2026-06-05 PM). SES side (config-set + SNS event
		// dest + tenant + identity/cs assoc) provisioned via .scratch/jun05_ses_wire_warming.py;
		// easyDKIM identities verified/reset jun05; PMTA VMTA relay pools <prefix>-ses-pool
		// appended to Server A (mr,ci) + Server B (hw,tt,yi,lp,rb,wf). m.<brand> need not be
		// SES-verified — SES authorizes against the From identity em.<brand>.
		{"seed_pmta_hw_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Home Warranty Services (SES Tenant)', 'pmta', 'Hank @ Home Warranty Services',
				'hello@em.homewarrantyservices.org', 'reply@em.homewarrantyservices.org',
				'm.homewarrantyservices.org', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 't.em.homewarrantyservices.org',
				1000, 25000, 'hw-ses-pool', 'active', false,
				true, 'homewarrantyservices', 'homewarrantyservices',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Home Warranty Services (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_tt_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Thing of the Day (SES Tenant)', 'pmta', 'Olivia @ Thing of the Day',
				'hello@em.thingoftheday.org', 'reply@em.thingoftheday.org',
				'm.thingoftheday.org', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 't.em.thingoftheday.org',
				1000, 25000, 'tt-ses-pool', 'active', false,
				true, 'thingoftheday', 'thingoftheday',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Thing of the Day (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_yi_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Your Insurance Hub (SES Tenant)', 'pmta', 'Carl @ Your Insurance Hub',
				'hello@em.yourinsurancehub.com', 'reply@em.yourinsurancehub.com',
				'm.yourinsurancehub.com', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 't.em.yourinsurancehub.com',
				1000, 25000, 'yi-ses-pool', 'active', false,
				true, 'yourinsurancehub', 'yourinsurancehub',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Your Insurance Hub (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_lp_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Learn Personal Loans (SES Tenant)', 'pmta', 'Linda @ Learn Personal Loans',
				'hello@em.learnpersonalloans.com', 'reply@em.learnpersonalloans.com',
				'm.learnpersonalloans.com', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 't.em.learnpersonalloans.com',
				1000, 25000, 'lp-ses-pool', 'active', false,
				true, 'learnpersonalloans', 'learnpersonalloans',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Learn Personal Loans (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_rb_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Rates Bazar (SES Tenant)', 'pmta', 'Sam @ Rates Bazar',
				'hello@em.ratesbazar.com', 'reply@em.ratesbazar.com',
				'm.ratesbazar.com', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 't.em.ratesbazar.com',
				1000, 25000, 'rb-ses-pool', 'active', false,
				true, 'ratesbazar', 'ratesbazar',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Rates Bazar (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_wf_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Warranty For You (SES Tenant)', 'pmta', 'Greg @ Warranty For You',
				'hello@em.warrantyforyou.com', 'reply@em.warrantyforyou.com',
				'm.warrantyforyou.com', '15.204.107.107', 587,
				'http://15.204.107.107:19099', 't.em.warrantyforyou.com',
				1000, 25000, 'wf-ses-pool', 'active', false,
				true, 'warrantyforyou', 'warrantyforyou',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Warranty For You (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_mr_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'My Repair DIY (SES Tenant)', 'pmta', 'Bob @ My Repair DIY',
				'hello@em.myrepairdiy.com', 'reply@em.myrepairdiy.com',
				'm.myrepairdiy.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.myrepairdiy.com',
				1000, 25000, 'mr-ses-pool', 'active', false,
				true, 'myrepairdiy', 'myrepairdiy',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'My Repair DIY (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_pmta_ci_ses_tenant_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default,
			 via_ses, ses_configuration_set, ses_tenant_name,
			 created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Casa Insure (SES Tenant)', 'pmta', 'Maria @ Casa Insure',
				'hello@em.casainsure.com', 'reply@em.casainsure.com',
				'm.casainsure.com', '15.204.101.125', 587,
				'http://15.204.101.125:19099', 't.em.casainsure.com',
				1000, 25000, 'ci-ses-pool', 'active', false,
				true, 'casainsure', 'casainsure',
				NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE name = 'Casa Insure (SES Tenant)'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_ses_hw_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.homewarrantyservices.org', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.homewarrantyservices.org' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_tt_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.thingoftheday.org', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.thingoftheday.org' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_yi_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.yourinsurancehub.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.yourinsurancehub.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_lp_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.learnpersonalloans.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.learnpersonalloans.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_rb_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.ratesbazar.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.ratesbazar.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_wf_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.warrantyforyou.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.warrantyforyou.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_mr_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.myrepairdiy.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.myrepairdiy.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_ses_ci_sending_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.casainsure.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.casainsure.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},

		// EVERYTHING-via-SES (jun05): QF/HT/MH SES tenant profiles were seeded
		// inactive during the jun03 cutover (only DB/RR went live then). The
		// all-SES directive needs them active so pinned-profile preflight passes.
		// Activated via API jun05; this UPDATE keeps them active across deploys.
		{"activate_qf_ht_mh_ses_tenant_profiles", `UPDATE mailing_sending_profiles
			SET status = 'active', updated_at = NOW()
			WHERE organization_id = '00000000-0000-0000-0000-000000000001'
			  AND name IN ('Quiz Fiesta (SES Tenant)', 'History Thinking (SES Tenant)', 'My Own Health (SES Tenant)')
			  AND status <> 'active'`},
		// Ensure seed/test subscribers have first_name populated
		{"set_test_subscriber_names", `UPDATE mailing_subscribers SET first_name = 'Drisan', last_name = 'James', updated_at = NOW() WHERE email IN ('drisanjames@gmail.com','drisanjames@yahoo.com','drisanjames@outlook.com','drisanjames@att.net') AND (first_name IS NULL OR first_name = '')`},
		// --- AWS SES via PMTA relay: m.discountblog.com ---
		// Required DNS records for m.discountblog.com:
		//   DKIM: 3 CNAMEs provided by SES (o2c4nzw6..., q25q7twp..., zq53za2a...)
		//   SPF:  m.discountblog.com TXT "v=spf1 include:amazonses.com ~all"
		//   DMARC: _dmarc.m.discountblog.com TXT "v=DMARC1; p=quarantine; rua=mailto:dmarc@discountblog.com"
		//   MX:   m.discountblog.com MX 10 feedback-smtp.us-west-1.amazonses.com
		{"seed_ses_discountblog_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint,
			 hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'DiscountBlog SES', 'pmta', 'DiscountBlog', 'hello@m.discountblog.com', 'reply@m.discountblog.com',
				'm.discountblog.com', '15.204.101.125', 587, 'http://15.204.101.125:19099',
				1000, 10000, 'warmup-pool', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE sending_domain = 'm.discountblog.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_ses_discountblog_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.discountblog.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.discountblog.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// --- AWS SES via PMTA relay: m.quizfiesta.com ---
		// DKIM: 3 CNAMEs configured in SES (already verified)
		// SPF:  m.quizfiesta.com TXT "v=spf1 include:amazonses.com ~all"
		// DMARC: _dmarc.m.quizfiesta.com TXT "v=DMARC1; p=reject; rua=mailto:dmarc@quizfiesta.com"
		// BIMI: default._bimi.m.quizfiesta.com TXT "v=BIMI1; l=https://d3j30mnhwt8cov.cloudfront.net/bimi/quizfiesta.svg; a=;"
		// Relay block on Server A (15.204.101.125) → email-smtp.us-west-1.amazonaws.com:587
		{"seed_ses_quizfiesta_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint,
			 hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'QuizFiesta SES', 'pmta', 'Quiz Fiesta', 'hello@m.quizfiesta.com', 'reply@m.quizfiesta.com',
				'm.quizfiesta.com', '15.204.101.125', 587, 'http://15.204.101.125:19099',
				1000, 10000, 'warmup-pool', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE sending_domain = 'm.quizfiesta.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_ses_quizfiesta_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.quizfiesta.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.quizfiesta.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// --- AWS SES via PMTA relay: m.historythinking.com ---
		// DKIM: 3 CNAMEs configured via GoDaddy API (54pbgywbl5..., trprauojv7..., heyfh5ayh...)
		// SPF:  m.historythinking.com TXT "v=spf1 include:amazonses.com ~all"
		// DMARC: _dmarc.m.historythinking.com TXT "v=DMARC1; p=reject; rua=mailto:dmarc@historythinking.com"
		// BIMI: default._bimi.m.historythinking.com TXT "v=BIMI1; l=https://d3j30mnhwt8cov.cloudfront.net/bimi/historythinking.svg; a=;"
		// Relay block on Server B (15.204.107.107) → email-smtp.us-west-1.amazonaws.com:587
		{"seed_ses_historythinking_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint,
			 hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'HistoryThinking SES', 'pmta', 'History Thinking', 'hello@m.historythinking.com', 'reply@m.historythinking.com',
				'm.historythinking.com', '15.204.107.107', 587, 'http://15.204.107.107:19099',
				1000, 10000, 'warmup-pool', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE sending_domain = 'm.historythinking.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_ses_historythinking_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.historythinking.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.historythinking.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// --- AWS SES via PMTA relay: m.myownhealth.net ---
		// DKIM: 3 CNAMEs configured via GoDaddy API (jw25zdlcq4..., zwdem6fdz..., 5xevi2nj5...)
		// SPF:  m.myownhealth.net TXT "v=spf1 include:amazonses.com ~all"
		// DMARC: _dmarc.m.myownhealth.net TXT "v=DMARC1; p=reject; rua=mailto:dmarc@myownhealth.net"
		// BIMI: default._bimi.m.myownhealth.net TXT "v=BIMI1; l=https://d3j30mnhwt8cov.cloudfront.net/bimi/myownhealth.svg; a=;"
		// Relay block on Server B (15.204.107.107) → email-smtp.us-west-1.amazonaws.com:587
		{"seed_ses_myownhealth_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint,
			 hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'MyOwnHealth SES', 'pmta', 'My Own Health', 'hello@m.myownhealth.net', 'reply@m.myownhealth.net',
				'm.myownhealth.net', '15.204.107.107', 587, 'http://15.204.107.107:19099',
				1000, 10000, 'warmup-pool', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE sending_domain = 'm.myownhealth.net'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_ses_myownhealth_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'm.myownhealth.net', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'm.myownhealth.net'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// --- em.historythinking.com (PMTA direct) ---
		// DNS records configured via GoDaddy API 2026-03-16:
		//   A:     em.historythinking.com → 15.204.101.125
		//   SPF:   em.historythinking.com TXT "v=spf1 ip4:15.204.22.176/28 -all"
		//   DKIM:  dkim._domainkey.em.historythinking.com TXT "v=DKIM1; k=rsa; p=..."
		//   DMARC: _dmarc.em.historythinking.com TXT "v=DMARC1; p=quarantine; adkim=r; aspf=r; pct=100"
		//   MX:    em.historythinking.com MX 10 em.historythinking.com
		{"seed_pmta_historythinking_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'HistoryThinking PMTA', 'pmta', 'History Thinking', 'hello@em.historythinking.com', 'reply@em.historythinking.com',
				'em.historythinking.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.historythinking.com',
				3200, 25000, 'warmup-pool', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE sending_domain = 'em.historythinking.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_historythinking_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'em.historythinking.com', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'em.historythinking.com'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// --- em.myownhealth.net (PMTA direct) ---
		// DNS records configured via GoDaddy API 2026-03-16:
		//   A:     em.myownhealth.net → 15.204.101.125
		//   SPF:   em.myownhealth.net TXT "v=spf1 ip4:15.204.22.176/28 -all"
		//   DKIM:  dkim._domainkey.em.myownhealth.net TXT "v=DKIM1; k=rsa; p=..."
		//   DMARC: _dmarc.em.myownhealth.net TXT "v=DMARC1; p=quarantine; adkim=r; aspf=r; pct=100"
		//   MX:    em.myownhealth.net MX 10 em.myownhealth.net
		{"seed_pmta_myownhealth_profile", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'MyOwnHealth PMTA', 'pmta', 'My Own Health', 'hello@em.myownhealth.net', 'reply@em.myownhealth.net',
				'em.myownhealth.net', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.myownhealth.net',
				3200, 25000, 'warmup-pool', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_profiles
				WHERE sending_domain = 'em.myownhealth.net'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		{"seed_myownhealth_domain", `INSERT INTO mailing_sending_domains
			(id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'em.myownhealth.net', true, true, true, 'verified', NOW(), NOW()
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_sending_domains
				WHERE domain = 'em.myownhealth.net'
				  AND organization_id = '00000000-0000-0000-0000-000000000001'
			)`},
		// Fix list_ids that contain list names instead of UUIDs (campaigns stuck as scheduled)
		{"fix_list_ids_names_to_uuids", `
			UPDATE mailing_campaigns c
			SET list_ids = (
				SELECT jsonb_agg(l.id::text)
				FROM mailing_lists l,
				     jsonb_array_elements_text(c.list_ids) AS name_val
				WHERE l.organization_id = c.organization_id
				  AND l.name = name_val
			), updated_at = NOW()
			WHERE c.status IN ('scheduled','preparing')
			  AND jsonb_typeof(c.list_ids) = 'array'
			  AND jsonb_array_length(c.list_ids) > 0
			  AND (c.list_ids->>0) !~ '^[0-9a-f]{8}-'
		`},
		{"reset_emergency_agents", `UPDATE mailing_engine_agent_state SET status = 'active', updated_at = NOW() WHERE agent_type = 'emergency' AND status = 'firing'`},
		{"add_tracking_sending_domain", `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_domain VARCHAR(255); EXCEPTION WHEN OTHERS THEN NULL; END $$`},
		{"add_tracking_sending_ip", `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_ip VARCHAR(45); EXCEPTION WHEN OTHERS THEN NULL; END $$`},
		{"idx_tracking_sending_domain", `CREATE INDEX IF NOT EXISTS idx_tracking_sending_domain ON mailing_tracking_events(sending_domain)`},
		{"idx_tracking_sending_ip", `CREATE INDEX IF NOT EXISTS idx_tracking_sending_ip ON mailing_tracking_events(sending_ip)`},
		{"backfill_sending_domain_startup", `
			UPDATE mailing_tracking_events t
			SET sending_domain = LOWER(SPLIT_PART(c.from_email, '@', 2))
			FROM mailing_campaigns c
			WHERE t.campaign_id = c.id
			  AND (t.sending_domain IS NULL OR t.sending_domain = '')
			  AND c.from_email IS NOT NULL
			  AND c.from_email LIKE '%@%'
		`},
		{"create_auto_fill_sending_domain_fn_startup", `DO $$ BEGIN
			CREATE OR REPLACE FUNCTION auto_fill_sending_domain()
			RETURNS TRIGGER AS $fn$
			BEGIN
			  IF NEW.sending_domain IS NULL OR NEW.sending_domain = '' THEN
			    SELECT LOWER(SPLIT_PART(c.from_email, '@', 2)) INTO NEW.sending_domain
			    FROM mailing_campaigns c
			    WHERE c.id = NEW.campaign_id
			      AND c.from_email IS NOT NULL AND c.from_email LIKE '%@%';
			  END IF;
			  RETURN NEW;
			END;
			$fn$ LANGUAGE plpgsql;
		EXCEPTION WHEN OTHERS THEN NULL; END $$
		`},
		{"create_auto_fill_trigger_startup", `
			DO $$ BEGIN
			  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_auto_fill_sending_domain') THEN
			    CREATE TRIGGER trg_auto_fill_sending_domain
			    BEFORE INSERT ON mailing_tracking_events
			    FOR EACH ROW EXECUTE FUNCTION auto_fill_sending_domain();
			  END IF;
			END $$
		`},
		{"consolidate_suppression_entries", `DO $$ BEGIN
			INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
			SELECT gen_random_uuid(),
				COALESCE(
					(SELECT organization_id FROM mailing_suppression_lists WHERE id = e.list_id LIMIT 1),
					(SELECT id FROM organizations LIMIT 1)
				),
				LOWER(TRIM(e.email)), e.md5_hash,
				COALESCE(e.reason, e.category, 'manual'),
				COALESCE(e.source, 'legacy_migration'),
				COALESCE(e.created_at, NOW())
			FROM mailing_suppression_entries e
			WHERE e.is_global = TRUE AND e.md5_hash IS NOT NULL AND e.md5_hash != ''
			AND NOT EXISTS (SELECT 1 FROM mailing_global_suppressions g WHERE g.md5_hash = e.md5_hash)
			ON CONFLICT DO NOTHING;
		EXCEPTION WHEN OTHERS THEN NULL; END $$
		`},
		{"consolidate_suppression_legacy", `
			INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
			SELECT gen_random_uuid(), (SELECT id FROM organizations LIMIT 1),
				LOWER(TRIM(s.email)), MD5(LOWER(TRIM(s.email))),
				COALESCE(s.reason, 'manual'), COALESCE(s.source, 'legacy_migration'),
				COALESCE(s.created_at, NOW())
			FROM mailing_suppressions s WHERE s.active = TRUE
			AND NOT EXISTS (SELECT 1 FROM mailing_global_suppressions g WHERE g.md5_hash = MD5(LOWER(TRIM(s.email))))
			ON CONFLICT DO NOTHING
		`},
		{"cleanup_global_entries_from_legacy", `DELETE FROM mailing_suppression_entries WHERE is_global = TRUE`},
		// System segments: companion table (owned by ignite) to avoid ALTER on apex_admin-owned mailing_segments
		{"create_system_segments_table", `CREATE TABLE IF NOT EXISTS mailing_system_segments (
			segment_id UUID PRIMARY KEY,
			system_query TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		// Seed bot detection segment: first create in mailing_segments, then link in companion table
		{"seed_bot_segment_row", `
			INSERT INTO mailing_segments (
				id, organization_id, name, description, segment_type, conditions,
				calculation_mode, include_suppressed, status, created_at, updated_at
			)
			SELECT gen_random_uuid(), id,
				'Bot Clickers (System)',
				'Auto-detected bot clickers: high click frequency (5+/campaign), clicks without opens, inhuman click speed (<2s).',
				'dynamic', '{"logic_operator":"OR","conditions":[]}'::jsonb,
				'batch', false, 'active', NOW(), NOW()
			FROM organizations
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_segments WHERE name = 'Bot Clickers (System)' AND organization_id = organizations.id
			)
		`},
		{"seed_bot_segment_query", `
			INSERT INTO mailing_system_segments (segment_id, system_query)
			SELECT ms.id,
				'SELECT COUNT(DISTINCT s.id) FROM mailing_subscribers s WHERE s.organization_id = $1 AND s.status = ''confirmed'' AND (
					EXISTS (
						SELECT 1 FROM mailing_tracking_events e
						WHERE e.subscriber_id = s.id AND e.event_type = ''clicked''
						AND e.event_at > NOW() - INTERVAL ''30 days''
						GROUP BY e.campaign_id HAVING COUNT(*) >= 5
					)
					OR (
						EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = s.id AND e.event_type = ''clicked'' AND e.event_at > NOW() - INTERVAL ''30 days'')
						AND NOT EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = s.id AND e.event_type = ''opened'' AND e.event_at > NOW() - INTERVAL ''30 days'')
					)
					OR EXISTS (
						SELECT 1 FROM mailing_tracking_events click
						JOIN mailing_tracking_events deliver ON deliver.subscriber_id = click.subscriber_id
							AND deliver.campaign_id = click.campaign_id AND deliver.event_type = ''delivered''
						WHERE click.subscriber_id = s.id AND click.event_type = ''clicked''
						AND click.event_at > NOW() - INTERVAL ''30 days''
						AND click.event_at - deliver.event_at < INTERVAL ''2 seconds''
					)
				)'
			FROM mailing_segments ms
			WHERE ms.name = 'Bot Clickers (System)'
			AND NOT EXISTS (SELECT 1 FROM mailing_system_segments WHERE segment_id = ms.id)
		`},
		{"backfill_sent_from_queue_v2", `
			INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, event_type, event_at, sending_domain)
			SELECT gen_random_uuid(), c.organization_id, q.campaign_id, q.subscriber_id,
			       'sent', q.sent_at,
			       COALESCE(LOWER(SPLIT_PART(c.from_email, '@', 2)), '')
			FROM mailing_campaign_queue q
			JOIN mailing_campaigns c ON c.id = q.campaign_id
			WHERE q.status = 'sent'
			  AND q.sent_at IS NOT NULL
			  AND q.sent_at >= NOW() - INTERVAL '14 days'
			  AND NOT EXISTS (
			      SELECT 1 FROM mailing_tracking_events te
			      WHERE te.campaign_id = q.campaign_id
			        AND te.subscriber_id = q.subscriber_id
			        AND te.event_type = 'sent'
			  )
			ON CONFLICT DO NOTHING
		`},
		{"add_is_machine_open_col", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS is_machine_open BOOLEAN DEFAULT FALSE`},
		{"add_idx_mte_machine_open", `CREATE INDEX IF NOT EXISTS idx_mte_machine_open ON mailing_tracking_events (campaign_id, is_machine_open) WHERE event_type = 'opened' AND is_machine_open = TRUE`},
		{"backfill_mpp_opens_sent_v3", `
			UPDATE mailing_tracking_events o
			SET is_machine_open = TRUE
			FROM mailing_tracking_events s
			WHERE o.event_type = 'opened'
			  AND o.is_machine_open = FALSE
			  AND s.event_type = 'sent'
			  AND o.subscriber_id = s.subscriber_id
			  AND o.campaign_id = s.campaign_id
			  AND o.event_at >= s.event_at
			  AND o.event_at <= s.event_at + INTERVAL '120 seconds'
		`},

		{"create_wave_content_cache", `CREATE TABLE IF NOT EXISTS mailing_wave_content_cache (
			id            SERIAL PRIMARY KEY,
			brand_key     TEXT NOT NULL,
			wave_index    INT NOT NULL,
			subject       TEXT NOT NULL,
			preview_text  TEXT NOT NULL DEFAULT '',
			from_name     TEXT NOT NULL DEFAULT '',
			html_content  TEXT NOT NULL,
			diagnostics   JSONB,
			generated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			used_at       TIMESTAMPTZ,
			version       TEXT NOT NULL DEFAULT ''
		)`},
		{"idx_wave_cache_brand_unused", `CREATE INDEX IF NOT EXISTS idx_wave_cache_brand_unused ON mailing_wave_content_cache (brand_key, generated_at DESC) WHERE used_at IS NULL`},
		{"add_cache_campaign_type", `ALTER TABLE mailing_wave_content_cache ADD COLUMN IF NOT EXISTS campaign_type TEXT NOT NULL DEFAULT 'newsletter'`},
		{"add_cache_editorial_json", `ALTER TABLE mailing_wave_content_cache ADD COLUMN IF NOT EXISTS editorial_json JSONB`},
		{"idx_wave_cache_brand_type_unused", `CREATE INDEX IF NOT EXISTS idx_wave_cache_brand_type_unused ON mailing_wave_content_cache (brand_key, campaign_type, generated_at DESC) WHERE used_at IS NULL`},

		{"create_isp_throttle_state", `CREATE TABLE IF NOT EXISTS mailing_isp_throttle_state (
			isp          TEXT PRIMARY KEY,
			msgs_per_hour DOUBLE PRECISION NOT NULL,
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},

		{"create_engine_convictions", `CREATE TABLE IF NOT EXISTS mailing_engine_convictions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			isp VARCHAR(50) NOT NULL,
			agent_type VARCHAR(30) NOT NULL,
			agent_id VARCHAR(100) NOT NULL,
			verdict VARCHAR(10) NOT NULL,
			statement TEXT NOT NULL,
			effective_rate DOUBLE PRECISION,
			micro_context JSONB NOT NULL DEFAULT '{}',
			outcome JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_engine_convictions_isp_agent", `CREATE INDEX IF NOT EXISTS idx_engine_convictions_isp_agent ON mailing_engine_convictions(isp, agent_type, created_at DESC)`},
		{"idx_engine_convictions_created", `CREATE INDEX IF NOT EXISTS idx_engine_convictions_created ON mailing_engine_convictions(created_at DESC)`},

		{"add_list_subscriber_count", `ALTER TABLE mailing_lists ADD COLUMN IF NOT EXISTS subscriber_count INT DEFAULT 0`},
		{"add_list_active_count", `ALTER TABLE mailing_lists ADD COLUMN IF NOT EXISTS active_count INT DEFAULT 0`},
		{"add_list_mailed_to", `ALTER TABLE mailing_lists ADD COLUMN IF NOT EXISTS mailed_to INT DEFAULT 0`},
		{"add_list_last_refreshed_at", `ALTER TABLE mailing_lists ADD COLUMN IF NOT EXISTS last_refreshed_at TIMESTAMPTZ`},
		{"add_list_is_visible", `ALTER TABLE mailing_lists ADD COLUMN IF NOT EXISTS is_visible BOOLEAN DEFAULT true`},
		{"hide_duplicate_lists", `
			UPDATE mailing_lists SET is_visible = false
			WHERE id IN (
				'482e2ac2-06ee-4bf0-9c4c-4596c035292b',
				'7863cda8-a5c8-4bf3-9277-39f154e472d7',
				'a0379e54-11ba-4b57-9787-4da4c2eb223b',
				'580cea39-9560-4753-a078-78b0aa080fcd',
				'ed49d5cf-50f2-4200-9821-41bde46950dd',
				'da2970f4-b652-4251-aa19-19e7e7309ca9',
				'45a37870-dd25-4c7c-986e-8f42425720df',
				'2902c0f5-b9a3-4771-93a9-2387f1a5d10b'
			)`},

		// Fast O(1) subscriber count trigger — replaces the old O(n) COUNT(*) trigger.
		// Moved here from the runtime goroutine in NewMailingService so schema
		// mutations go through the startup migration system, not background DDL.
		{"create_fast_list_counts_fn", `
			CREATE OR REPLACE FUNCTION update_list_counts_fast() RETURNS TRIGGER AS $$
			BEGIN
				IF TG_OP = 'INSERT' THEN
					UPDATE mailing_lists SET subscriber_count = subscriber_count + 1,
						active_count = CASE WHEN NEW.status = 'confirmed' THEN active_count + 1 ELSE active_count END,
						updated_at = NOW() WHERE id = NEW.list_id;
				ELSIF TG_OP = 'DELETE' THEN
					UPDATE mailing_lists SET subscriber_count = GREATEST(subscriber_count - 1, 0),
						active_count = CASE WHEN OLD.status = 'confirmed' THEN GREATEST(active_count - 1, 0) ELSE active_count END,
						updated_at = NOW() WHERE id = OLD.list_id;
				ELSIF TG_OP = 'UPDATE' AND OLD.status != NEW.status THEN
					UPDATE mailing_lists SET
						active_count = active_count
							+ CASE WHEN NEW.status = 'confirmed' THEN 1 ELSE 0 END
							- CASE WHEN OLD.status = 'confirmed' THEN 1 ELSE 0 END,
						updated_at = NOW() WHERE id = NEW.list_id;
				END IF;
				RETURN NULL;
			END;
			$$ LANGUAGE plpgsql`},
		{"create_fast_list_counts_trigger", `
			DO $$ BEGIN
				IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trigger_update_list_counts') THEN
					CREATE TRIGGER trigger_update_list_counts
						AFTER INSERT OR UPDATE OR DELETE ON mailing_subscribers
						FOR EACH ROW EXECUTE FUNCTION update_list_counts_fast();
				END IF;
			END $$`},
		{"drop_old_list_counts_fn", `DROP FUNCTION IF EXISTS update_list_counts() CASCADE`},

		{"add_subscriber_isp_col", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS isp VARCHAR(20) DEFAULT ''`},
		{"idx_subscriber_isp", `CREATE INDEX IF NOT EXISTS idx_subscribers_isp ON mailing_subscribers(isp) WHERE isp != ''`},
		{"add_subscriber_bot_cols", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS is_bot BOOLEAN NOT NULL DEFAULT false; ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS bot_detected_at TIMESTAMPTZ`},
		{"idx_subscriber_is_bot", `CREATE INDEX IF NOT EXISTS idx_subscribers_is_bot ON mailing_subscribers(is_bot) WHERE is_bot = true`},

		// Bot-backstop migrations (Apr 2026): every audience-selection query
		// now carries AND is_bot = false (pmta_campaign_planner.go,
		// campaign_builder_send_async.go, mailing_sending.go,
		// handlers_pmta_campaign.go, worker/campaign_processor.go,
		// segmentation/query_builder.go, mailing_segments.BuildSegmentWhereClause).
		// These three migrations clean up state that was populated before
		// the filters were in place. All three are idempotent and bounded
		// by the partial index idx_subscribers_is_bot so they stay inside
		// the 5s startup statement_timeout.
		//
		// 1) Unsubscribe honeypot-flagged bots as a final backstop even if
		//    some new selection site is added later without the filter.
		{"bot_backstop_unsubscribe", `
			UPDATE mailing_subscribers
			SET status = 'unsubscribed', updated_at = NOW()
			WHERE is_bot = true
			  AND status IN ('active','confirmed')
		`},
		// 2) Purge bots from already-materialised segment members. Segments
		//    re-materialise nightly via segment_materializer.go which now
		//    excludes bots at the BuildSegmentWhereClause layer, so this
		//    is just a one-time cleanup of state accumulated before the
		//    filter existed.
		{"bot_backstop_purge_segment_members", `
			DELETE FROM mailing_segment_members sm
			USING mailing_subscribers s
			WHERE sm.subscriber_id = s.id
			  AND s.is_bot = true
		`},
		// 3) Zero out lingering inflated score_local values on SDS rows for
		//    bots. RecomputeSDSScoreLocal will continue to write 0 for bots
		//    on every future engagement event, but historical scores need
		//    to be explicitly cleared or the planner's ORDER BY
		//    sds.score_local DESC will keep picking them until something
		//    else triggers a recompute on that (subscriber, domain) pair.
		{"bot_backstop_reset_score_local", `
			UPDATE mailing_subscriber_domain_state sds
			SET score_local = 0, updated_at = NOW()
			FROM mailing_subscribers s
			WHERE s.id = sds.subscriber_id
			  AND s.is_bot = true
			  AND sds.score_local > 0
		`},

		{"add_subscriber_eo_validated_at", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS eo_validated_at TIMESTAMPTZ`},

		{"sync_bounced_to_global_supp", `
			INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
			SELECT gen_random_uuid(), s.organization_id, s.email, MD5(LOWER(TRIM(s.email))), 'hard_bounce', 'status_sync', NOW()
			FROM mailing_subscribers s
			WHERE s.status = 'bounced'
			AND NOT EXISTS (
				SELECT 1 FROM mailing_global_suppressions g
				WHERE g.organization_id = s.organization_id AND g.md5_hash = MD5(LOWER(TRIM(s.email)))
			)
			ON CONFLICT (organization_id, md5_hash) DO NOTHING
		`},
		{"sync_complained_to_global_supp", `
			INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
			SELECT gen_random_uuid(), s.organization_id, s.email, MD5(LOWER(TRIM(s.email))), 'complaint', 'status_sync', NOW()
			FROM mailing_subscribers s
			WHERE s.status = 'complained'
			AND NOT EXISTS (
				SELECT 1 FROM mailing_global_suppressions g
				WHERE g.organization_id = s.organization_id AND g.md5_hash = MD5(LOWER(TRIM(s.email)))
			)
			ON CONFLICT (organization_id, md5_hash) DO NOTHING
		`},
		{"sync_bot_clickers_to_global_supp", `
			INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
			SELECT DISTINCT gen_random_uuid(), s.organization_id, s.email, MD5(LOWER(TRIM(s.email))), 'bot_clicker', 'status_sync', NOW()
			FROM mailing_subscribers s
			WHERE s.status = 'confirmed'
			AND EXISTS (
				SELECT 1 FROM mailing_tracking_events e
				WHERE e.subscriber_id = s.id AND e.event_type = 'opened' AND e.is_machine_open = TRUE
			)
			AND NOT EXISTS (
				SELECT 1 FROM mailing_tracking_events e2
				WHERE e2.subscriber_id = s.id AND e2.event_type = 'opened' AND (e2.is_machine_open = FALSE OR e2.is_machine_open IS NULL)
			)
			AND NOT EXISTS (
				SELECT 1 FROM mailing_global_suppressions g
				WHERE g.organization_id = s.organization_id AND g.md5_hash = MD5(LOWER(TRIM(s.email)))
			)
			ON CONFLICT (organization_id, md5_hash) DO NOTHING
		`},

		{"startup_warmup_limits_10k", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 10000 WHERE warmup_daily_limit < 10000 AND status IN ('active', 'warmup')`},
		{"drop_ip_status_check", `ALTER TABLE mailing_ip_addresses DROP CONSTRAINT IF EXISTS mailing_ip_addresses_status_check`},
		{"startup_mta1_cold", `UPDATE mailing_ip_addresses SET status = 'cold', warmup_stage = 'paused' WHERE (hostname LIKE 'mta1%.mail.projectjarvis.io' OR ip_address::text LIKE '15.204.22.176%') AND status != 'cold'`},

		{"create_offer_verticals", `CREATE TABLE IF NOT EXISTS mailing_offer_verticals (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			name VARCHAR(255) NOT NULL,
			description TEXT DEFAULT '',
			sort_order INT DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"idx_offer_verticals_org_name", `CREATE UNIQUE INDEX IF NOT EXISTS idx_offer_verticals_org_name ON mailing_offer_verticals(organization_id, name)`},

		{"create_offers", `CREATE TABLE IF NOT EXISTS mailing_offers (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			vertical_id UUID REFERENCES mailing_offer_verticals(id) ON DELETE SET NULL,
			brand_name VARCHAR(255) DEFAULT '',
			name VARCHAR(255) NOT NULL,
			description TEXT DEFAULT '',
			everflow_offer_id VARCHAR(50) DEFAULT '',
			everflow_creative_id VARCHAR(50) DEFAULT '',
			tracking_link_template TEXT DEFAULT '',
			optizmo_link TEXT DEFAULT '',
			web_property VARCHAR(50) DEFAULT '',
			landing_page_slug VARCHAR(255) DEFAULT '',
			landing_page_url TEXT DEFAULT '',
			landing_page_html TEXT DEFAULT '',
			original_html_creative TEXT DEFAULT '',
			payout DECIMAL(10,2) DEFAULT 0,
			payout_type VARCHAR(20) DEFAULT '',
			optizmo_status VARCHAR(20) DEFAULT 'not_scrubbed',
			optizmo_last_scrubbed_at TIMESTAMPTZ,
			status VARCHAR(20) DEFAULT 'draft',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"ensure_offers_vertical_id", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS vertical_id UUID`},
		{"ensure_offers_brand_name", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS brand_name VARCHAR(255) DEFAULT ''`},
		{"ensure_offers_description", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS description TEXT DEFAULT ''`},
		{"ensure_offers_ef_offer_id", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS everflow_offer_id VARCHAR(50) DEFAULT ''`},
		{"ensure_offers_ef_creative_id", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS everflow_creative_id VARCHAR(50) DEFAULT ''`},
		{"ensure_offers_tracking_link", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS tracking_link_template TEXT DEFAULT ''`},
		{"ensure_offers_optizmo_link", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS optizmo_link TEXT DEFAULT ''`},
		{"ensure_offers_web_property", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS web_property VARCHAR(50) DEFAULT ''`},
		{"ensure_offers_lp_slug", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS landing_page_slug VARCHAR(255) DEFAULT ''`},
		{"ensure_offers_lp_url", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS landing_page_url TEXT DEFAULT ''`},
		{"ensure_offers_lp_html", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS landing_page_html TEXT DEFAULT ''`},
		{"ensure_offers_orig_html", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS original_html_creative TEXT DEFAULT ''`},
		{"ensure_offers_payout", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS payout DECIMAL(10,2) DEFAULT 0`},
		{"ensure_offers_payout_type", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS payout_type VARCHAR(20) DEFAULT ''`},
		{"ensure_offers_optizmo_status", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS optizmo_status VARCHAR(20) DEFAULT 'not_scrubbed'`},
		{"ensure_offers_optizmo_scrubbed", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS optizmo_last_scrubbed_at TIMESTAMPTZ`},
		{"ensure_offers_status", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS status VARCHAR(20) DEFAULT 'draft'`},
		{"idx_offers_org", `CREATE INDEX IF NOT EXISTS idx_offers_org ON mailing_offers(organization_id)`},
		{"idx_offers_vertical", `CREATE INDEX IF NOT EXISTS idx_offers_vertical ON mailing_offers(vertical_id)`},
		{"idx_offers_status", `CREATE INDEX IF NOT EXISTS idx_offers_status ON mailing_offers(status)`},

		{"create_offer_subject_lines", `CREATE TABLE IF NOT EXISTS mailing_offer_subject_lines (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			subject_line TEXT NOT NULL,
			status VARCHAR(20) DEFAULT 'draft',
			performance_score DECIMAL(5,2) DEFAULT 0,
			total_sends INT DEFAULT 0,
			total_opens INT DEFAULT 0,
			open_rate DECIMAL(5,4) DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"idx_offer_subjects_offer", `CREATE INDEX IF NOT EXISTS idx_offer_subjects_offer ON mailing_offer_subject_lines(offer_id)`},

		{"create_offer_from_names", `CREATE TABLE IF NOT EXISTS mailing_offer_from_names (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			from_name VARCHAR(255) NOT NULL,
			status VARCHAR(20) DEFAULT 'draft',
			performance_score DECIMAL(5,2) DEFAULT 0,
			total_sends INT DEFAULT 0,
			total_opens INT DEFAULT 0,
			complaint_rate DECIMAL(5,4) DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"idx_offer_from_names_offer", `CREATE INDEX IF NOT EXISTS idx_offer_from_names_offer ON mailing_offer_from_names(offer_id)`},

		{"create_offer_creatives", `CREATE TABLE IF NOT EXISTS mailing_offer_creatives (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			version INT DEFAULT 1,
			html_content TEXT NOT NULL DEFAULT '',
			subject_line_id UUID REFERENCES mailing_offer_subject_lines(id) ON DELETE SET NULL,
			from_name_id UUID REFERENCES mailing_offer_from_names(id) ON DELETE SET NULL,
			status VARCHAR(20) DEFAULT 'generated',
			approval_notes TEXT DEFAULT '',
			total_sends INT DEFAULT 0,
			total_clicks INT DEFAULT 0,
			total_opens INT DEFAULT 0,
			total_conversions INT DEFAULT 0,
			click_rate DECIMAL(5,4) DEFAULT 0,
			open_rate DECIMAL(5,4) DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"idx_offer_creatives_offer", `CREATE INDEX IF NOT EXISTS idx_offer_creatives_offer ON mailing_offer_creatives(offer_id)`},

		{"create_offer_suppressions", `CREATE TABLE IF NOT EXISTS mailing_offer_suppressions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			subscriber_id UUID NOT NULL,
			email_hash VARCHAR(32) DEFAULT '',
			reason VARCHAR(50) DEFAULT '',
			source VARCHAR(50) DEFAULT '',
			everflow_conversion_id VARCHAR(255) DEFAULT '',
			suppressed_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"idx_offer_suppressions_offer_sub", `CREATE UNIQUE INDEX IF NOT EXISTS idx_offer_suppressions_offer_sub ON mailing_offer_suppressions(offer_id, subscriber_id)`},
		{"idx_offer_suppressions_hash", `CREATE INDEX IF NOT EXISTS idx_offer_suppressions_hash ON mailing_offer_suppressions(offer_id, email_hash)`},

		{"create_offer_deployments", `CREATE TABLE IF NOT EXISTS mailing_offer_deployments (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			campaign_id UUID,
			creative_id UUID,
			subject_line_id UUID,
			from_name_id UUID,
			audience_list_ids JSONB DEFAULT '[]',
			deployed_at TIMESTAMPTZ DEFAULT NOW(),
			total_sent INT DEFAULT 0,
			total_conversions INT DEFAULT 0,
			revenue DECIMAL(12,2) DEFAULT 0
		)`},
		{"idx_offer_deployments_offer", `CREATE INDEX IF NOT EXISTS idx_offer_deployments_offer ON mailing_offer_deployments(offer_id)`},

		{"create_optizmo_scrub_jobs", `CREATE TABLE IF NOT EXISTS mailing_optizmo_scrub_jobs (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			audience_file_path TEXT DEFAULT '',
			audience_count INT DEFAULT 0,
			result_file_path TEXT DEFAULT '',
			suppressed_count INT DEFAULT 0,
			status VARCHAR(20) DEFAULT 'pending',
			error_message TEXT DEFAULT '',
			requested_at TIMESTAMPTZ DEFAULT NOW(),
			started_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ
		)`},
		{"idx_optizmo_scrub_jobs_offer", `CREATE INDEX IF NOT EXISTS idx_optizmo_scrub_jobs_offer ON mailing_optizmo_scrub_jobs(offer_id)`},
		{"add_scrub_jobs_file_count", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS file_count INT DEFAULT 0`},
		{"add_scrub_jobs_valid_md5_count", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS valid_md5_count INT DEFAULT 0`},
		{"add_scrub_jobs_non_md5_count", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS non_md5_count INT DEFAULT 0`},
		{"add_scrub_jobs_scrub_type", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS scrub_type VARCHAR(20) DEFAULT 'manual'`},
		{"add_scrub_jobs_progress", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS progress_pct INT DEFAULT 0`},
		{"add_scrub_jobs_progress_msg", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS progress_message TEXT DEFAULT ''`},
		{"add_scrub_jobs_s3_hash_key", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS s3_hash_key TEXT DEFAULT ''`},
		{"add_scrub_jobs_s3_bloom_key", `ALTER TABLE mailing_optizmo_scrub_jobs ADD COLUMN IF NOT EXISTS s3_bloom_key TEXT DEFAULT ''`},

		{"add_offers_suppression_sync_enabled", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS suppression_sync_enabled BOOLEAN DEFAULT FALSE`},
		{"add_offers_last_sync_at", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS last_suppression_sync_at TIMESTAMPTZ`},
		{"add_offers_last_sync_error", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS last_suppression_sync_error TEXT DEFAULT ''`},
		{"idx_offers_sync_enabled", `CREATE INDEX IF NOT EXISTS idx_offers_sync_enabled ON mailing_offers(suppression_sync_enabled) WHERE suppression_sync_enabled = TRUE`},

		{"add_campaign_queue_offer_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS offer_id UUID`},
		{"add_campaign_queue_creative_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS creative_id UUID`},
		{"add_campaign_queue_subject_line_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
		{"add_campaign_queue_from_name_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS from_name_id UUID`},

		{"add_tracking_events_offer_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS offer_id UUID`},
		{"add_tracking_events_creative_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS creative_id UUID`},
		{"add_tracking_events_subject_line_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
		{"add_tracking_events_from_name_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS from_name_id UUID`},

		{"add_campaigns_offer_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS offer_id UUID`},

		// Attribution-stamping dim tables (Offer Alignment PART A, 2026-07-07).
		// The companion mailing_campaigns columns (offer_key/creative_id/
		// subject_line_id/attribution_source) live in criticalSendPathDDL —
		// enqueue + sent-event SQL references them before workers start. These
		// dims are only written by the deploy-time stampCampaignAttribution,
		// which is log-and-continue, so the background slice is safe for them.
		// Deliberately NOT reusing mailing_offer_subject_lines/_creatives:
		// their offer_id is a NOT NULL FK, which breaks when the offer is
		// unresolvable, and they are the offer-center authoring pool with a
		// different lifecycle.
		{"create_creative_identities", `CREATE TABLE IF NOT EXISTS mailing_creative_identities (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			content_md5 TEXT NOT NULL,
			offer_key TEXT,
			subject TEXT,
			from_name TEXT,
			first_seen_at TIMESTAMPTZ DEFAULT NOW(),
			last_used_at TIMESTAMPTZ DEFAULT NOW(),
			campaign_count INT DEFAULT 1,
			sample_campaign_id UUID
		)`},
		{"uq_creative_identities_org_md5", `CREATE UNIQUE INDEX IF NOT EXISTS uq_creative_identities_org_md5 ON mailing_creative_identities(organization_id, content_md5)`},
		{"create_subject_identities", `CREATE TABLE IF NOT EXISTS mailing_subject_identities (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			subject_md5 TEXT NOT NULL,
			subject TEXT NOT NULL,
			first_seen_at TIMESTAMPTZ DEFAULT NOW(),
			last_used_at TIMESTAMPTZ DEFAULT NOW(),
			campaign_count INT DEFAULT 1
		)`},
		{"uq_subject_identities_org_md5", `CREATE UNIQUE INDEX IF NOT EXISTS uq_subject_identities_org_md5 ON mailing_subject_identities(organization_id, subject_md5)`},

		// =====================================================================
		// Phase 6: Two-PMTA Multi-Server Infrastructure
		// =====================================================================

		// Step 6.1: Add pool_prefix column for ISP-aware pool routing
		{"add_pool_prefix_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS pool_prefix TEXT DEFAULT ''`},

		// Step 6.2: Seed PMTA servers
		{"seed_pmta_server_a", `INSERT INTO mailing_pmta_servers (id, organization_id, name, host, smtp_port, mgmt_port, mgmt_api_key, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'PMTA Server A (OVH Original)', '15.204.101.125', 587, 19000, '', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_pmta_servers WHERE host = '15.204.101.125')`},
		{"seed_pmta_server_b", `INSERT INTO mailing_pmta_servers (id, organization_id, name, host, smtp_port, mgmt_port, mgmt_api_key, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'PMTA Server B (OVH New)', '15.204.107.107', 587, 19000, '', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_pmta_servers WHERE host = '15.204.107.107')`},

		// Step 6.3: Seed ISP sub-pools (10 ISPs x 4 brands = 40 pools).
		// AOL is a separate pool from Yahoo — see internal/pkg/isp/isp.go for rationale.
		{"seed_pool_db_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-gmail-pool', 'DiscountBlog Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-yahoo-pool', 'DiscountBlog Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_aol", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-aol-pool', 'DiscountBlog AOL ISP pool — separate from Yahoo for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-aol-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-msft-pool', 'DiscountBlog Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-apple-pool', 'DiscountBlog Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-comcast-pool', 'DiscountBlog Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-att-pool', 'DiscountBlog ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_sbcglobal", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-sbcglobal-pool', 'DiscountBlog SBC Global/BellSouth ISP pool — separate from ATT for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-sbcglobal-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-cox-pool', 'DiscountBlog Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-charter-pool', 'DiscountBlog Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-general-pool', 'DiscountBlog General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-gmail-pool', 'QuizFiesta Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-yahoo-pool', 'QuizFiesta Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_aol", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-aol-pool', 'QuizFiesta AOL ISP pool — separate from Yahoo for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-aol-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-msft-pool', 'QuizFiesta Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-apple-pool', 'QuizFiesta Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-comcast-pool', 'QuizFiesta Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-att-pool', 'QuizFiesta ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_sbcglobal", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-sbcglobal-pool', 'QuizFiesta SBC Global/BellSouth ISP pool — separate from ATT for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-sbcglobal-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-cox-pool', 'QuizFiesta Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-charter-pool', 'QuizFiesta Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-general-pool', 'QuizFiesta General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-gmail-pool', 'HistoryThinking Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-yahoo-pool', 'HistoryThinking Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_aol", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-aol-pool', 'HistoryThinking AOL ISP pool — separate from Yahoo for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-aol-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-msft-pool', 'HistoryThinking Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-apple-pool', 'HistoryThinking Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-comcast-pool', 'HistoryThinking Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-att-pool', 'HistoryThinking ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_sbcglobal", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-sbcglobal-pool', 'HistoryThinking SBC Global/BellSouth ISP pool — separate from ATT for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-sbcglobal-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-cox-pool', 'HistoryThinking Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-charter-pool', 'HistoryThinking Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-general-pool', 'HistoryThinking General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-gmail-pool', 'MyOwnHealth Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-yahoo-pool', 'MyOwnHealth Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_aol", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-aol-pool', 'MyOwnHealth AOL ISP pool — separate from Yahoo for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-aol-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-msft-pool', 'MyOwnHealth Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-apple-pool', 'MyOwnHealth Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-comcast-pool', 'MyOwnHealth Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-att-pool', 'MyOwnHealth ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_sbcglobal", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-sbcglobal-pool', 'MyOwnHealth SBC Global/BellSouth ISP pool — separate from ATT for independent reputation tracking', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-sbcglobal-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-cox-pool', 'MyOwnHealth Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-charter-pool', 'MyOwnHealth Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-general-pool', 'MyOwnHealth General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},

		// Step 6.4: Pause mta1 (.176) which is SBL-listed
		{"phase6_pause_mta1", `UPDATE mailing_ip_addresses SET status = 'paused', updated_at = NOW() WHERE ip_address = '15.204.22.176'::inet AND status NOT IN ('paused', 'cold')`},

		// Step 6.4b: Seed /28 warm IPs into ISP sub-pools and all /25 IPs
		{"phase6_seed_all_ips", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
    server_id_val UUID;
    vmta_num INT;
    ip_octet INT;
    ip_text TEXT;
    full_hostname TEXT;
    i INT;
BEGIN
    -- /28 warm IPs: reassign from warmup-pool to ISP sub-pools
    FOR rec IN
        SELECT * FROM (VALUES
            ('15.204.22.177', 'mta-a-shrd1.mail.em.discountblog.com', 'db-gmail-pool', '15.204.101.125'),
            ('15.204.22.178', 'mta-a-shrd2.mail.em.discountblog.com', 'db-yahoo-pool', '15.204.101.125'),
            ('15.204.22.179', 'mta-a-shrd3.mail.em.discountblog.com', 'qf-gmail-pool', '15.204.101.125'),
            ('15.204.22.180', 'mta-a-shrd4.mail.em.discountblog.com', 'qf-yahoo-pool', '15.204.101.125'),
            ('15.204.22.181', 'mta-a-shrd5.mail.em.discountblog.com', 'db-msft-pool', '15.204.101.125'),
            ('15.204.22.182', 'mta-a-shrd6.mail.em.discountblog.com', 'qf-msft-pool', '15.204.101.125'),
            ('15.204.22.183', 'mta-a-shrd7.mail.em.discountblog.com', 'db-apple-pool', '15.204.101.125'),
            ('15.204.22.184', 'mta-a-shrd8.mail.em.discountblog.com', 'qf-apple-pool', '15.204.101.125'),
            ('15.204.22.185', 'mta-a-shrd9.mail.em.quizfiesta.com', 'db-comcast-pool', '15.204.101.125'),
            ('15.204.22.186', 'mta-a-shrd10.mail.em.quizfiesta.com', 'qf-comcast-pool', '15.204.101.125'),
            ('15.204.22.187', 'mta-a-shrd11.mail.em.quizfiesta.com', 'db-att-pool', '15.204.101.125'),
            ('15.204.22.188', 'mta-a-shrd12.mail.em.quizfiesta.com', 'qf-att-pool', '15.204.101.125'),
            ('15.204.22.189', 'mta-a-shrd13.mail.em.quizfiesta.com', 'db-cox-pool', '15.204.101.125'),
            ('15.204.22.190', 'mta-a-shrd14.mail.em.quizfiesta.com', 'qf-charter-pool', '15.204.101.125'),
            ('15.204.22.191', 'mta-a-shrd15.mail.em.quizfiesta.com', 'db-general-pool', '15.204.101.125')
        ) AS t(ip_addr, hostname, pool_name, server_host)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        SELECT id INTO server_id_val FROM mailing_pmta_servers WHERE host = rec.server_host;
        INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, status, pool_id, pmta_server_id,
            warmup_stage, warmup_day, warmup_daily_limit, warmup_started_at, hosting_provider, acquisition_type,
            cidr_block, rdns_verified, reputation_score, created_at, updated_at)
        VALUES (gen_random_uuid(), org_id, rec.ip_addr::inet, rec.hostname, 'warmup', pool_id_val, server_id_val,
            'warming', 1, 10000, NOW(), 'OVH', 'purchased', '15.204.22.176/28', true, 50.0, NOW(), NOW())
        ON CONFLICT (ip_address) DO UPDATE SET
            pool_id = pool_id_val, pmta_server_id = server_id_val, hostname = rec.hostname,
            status = CASE WHEN mailing_ip_addresses.status = 'paused' THEN 'paused' ELSE 'warmup' END,
            warmup_started_at = COALESCE(mailing_ip_addresses.warmup_started_at, NOW()), updated_at = NOW();
    END LOOP;

    -- /25 IPs: algorithmically generated from canonical block definitions
    -- (start_octet, count, prefix, isp_code, pool_suffix, mail_domain, server_host, cidr, vmta_start_num)
    FOR rec IN
        SELECT * FROM (VALUES
            (0,7,'db','gm','gmail','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (7,7,'db','yh','yahoo','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (14,7,'db','ms','msft','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (21,6,'db','ap','apple','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (27,6,'db','cc','comcast','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (33,6,'db','at','att','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (39,6,'db','cx','cox','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (45,6,'db','ch','charter','em.discountblog.com','15.204.101.125','144.225.178.0/25',1),
            (51,5,'db','gn','general','em.discountblog.com','15.204.101.125','144.225.178.0/25',2),
            (64,7,'qf','gm','gmail','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (71,7,'qf','yh','yahoo','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (78,7,'qf','ms','msft','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (85,6,'qf','ap','apple','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (91,6,'qf','cc','comcast','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (97,6,'qf','at','att','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (103,7,'qf','cx','cox','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',1),
            (110,5,'qf','ch','charter','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',2),
            (115,6,'qf','gn','general','em.quizfiesta.com','15.204.101.125','144.225.178.0/25',1),
            (128,8,'ht','gm','gmail','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (136,8,'ht','yh','yahoo','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (144,8,'ht','ms','msft','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (152,7,'ht','ap','apple','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (159,7,'ht','cc','comcast','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (166,7,'ht','at','att','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (173,7,'ht','cx','cox','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (180,6,'ht','ch','charter','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (186,6,'ht','gn','general','em.historythinking.com','15.204.107.107','144.225.178.128/25',1),
            (192,8,'mh','gm','gmail','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (200,8,'mh','yh','yahoo','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (208,8,'mh','ms','msft','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (216,7,'mh','ap','apple','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (223,7,'mh','cc','comcast','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (230,7,'mh','at','att','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (237,7,'mh','cx','cox','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (244,6,'mh','ch','charter','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1),
            (250,6,'mh','gn','general','em.myownhealth.net','15.204.107.107','144.225.178.128/25',1)
        ) AS t(start_octet, ip_count, prefix, isp_code, pool_suffix, mail_domain, server_host, cidr, vmta_start)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools
            WHERE name = rec.prefix || '-' || rec.pool_suffix || '-pool' AND organization_id = org_id;
        SELECT id INTO server_id_val FROM mailing_pmta_servers WHERE host = rec.server_host;
        vmta_num := rec.vmta_start;
        FOR i IN 0..rec.ip_count-1 LOOP
            ip_octet := rec.start_octet + i;
            ip_text := '144.225.178.' || ip_octet;
            full_hostname := 'mta-' || rec.prefix || '-' || rec.isp_code || vmta_num || '.mail.' || rec.mail_domain;
            INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, status, pool_id, pmta_server_id,
                warmup_stage, warmup_day, warmup_daily_limit, warmup_started_at, hosting_provider, acquisition_type,
                cidr_block, rdns_verified, created_at, updated_at)
            VALUES (gen_random_uuid(), org_id, ip_text::inet, full_hostname, 'warmup', pool_id_val, server_id_val,
                'warming', 1, 50, NOW(), 'ovh', 'leased', rec.cidr, false, NOW(), NOW())
            ON CONFLICT (ip_address) DO UPDATE SET
                pool_id = pool_id_val, pmta_server_id = server_id_val, hostname = full_hostname,
                warmup_started_at = COALESCE(mailing_ip_addresses.warmup_started_at, NOW()), updated_at = NOW();
            vmta_num := vmta_num + 1;
        END LOOP;
    END LOOP;
END $$`},

		// Step 6.5: Update sending profiles for multi-PMTA
		{"phase6_ht_mh_to_server_b", `UPDATE mailing_sending_profiles SET smtp_host = '15.204.107.107', api_endpoint = 'http://15.204.107.107:19099', updated_at = NOW() WHERE sending_domain IN ('em.historythinking.com', 'em.myownhealth.net') AND vendor_type = 'pmta' AND smtp_host = '15.204.101.125'`},
		// 2026-05-30: added NOT LIKE 'ses-relay%' guard. The original
		// `NOT LIKE 'shared-%'` clause was intended to skip already-routed
		// warm profiles (ip_pool=shared-a) but had the side effect of
		// catching the SES-relay profile (ip_pool=ses-relay-a) and
		// clobbering its pool_prefix='' back to 'db' on every boot. With
		// pool_prefix='db' the SES profile builds db-<isp>-pool VMTAs
		// and routes via OVH warm IPs instead of the <virtual-mta-pool
		// ses-relay-a> block — silently bypassing the SES relay.
		{"phase6_pool_prefix_db", `UPDATE mailing_sending_profiles SET pool_prefix = 'db' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%' AND COALESCE(ip_pool,'') NOT LIKE 'ses-relay%'`},
		{"phase6_pool_prefix_qf", `UPDATE mailing_sending_profiles SET pool_prefix = 'qf' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_pool_prefix_ht", `UPDATE mailing_sending_profiles SET pool_prefix = 'ht' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_pool_prefix_mh", `UPDATE mailing_sending_profiles SET pool_prefix = 'mh' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_ip_pool_db", `UPDATE mailing_sending_profiles SET ip_pool = 'db-gmail-pool' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},
		{"phase6_ip_pool_qf", `UPDATE mailing_sending_profiles SET ip_pool = 'qf-gmail-pool' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},
		{"phase6_ip_pool_ht", `UPDATE mailing_sending_profiles SET ip_pool = 'ht-gmail-pool' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},
		{"phase6_ip_pool_mh", `UPDATE mailing_sending_profiles SET ip_pool = 'mh-gmail-pool' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},

		// Step 6.6: Seed AOL pools with IPs.
		//
		// For each brand, move the highest-numbered IP from the Yahoo pool into the
		// AOL pool. This is safe because:
		//   1. AOL and Yahoo share MX infrastructure — the IP's reputation carries over.
		//   2. Yahoo pools have 7-8 IPs; losing 1 still leaves ample capacity.
		//   3. AOL volume is much smaller than Yahoo proper.
		//   4. This is idempotent: the WHERE clause only matches IPs still in the Yahoo pool.
		//
		// The query picks MAX(ip_address) from the brand's yahoo pool so it's deterministic
		// across restarts and doesn't depend on a specific IP being present.
		{"seed_aol_ip_db", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    yahoo_pool_id UUID;
    aol_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO yahoo_pool_id FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = org_id;
    SELECT id INTO aol_pool_id FROM mailing_ip_pools WHERE name = 'db-aol-pool' AND organization_id = org_id;
    IF yahoo_pool_id IS NOT NULL AND aol_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = yahoo_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = aol_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = yahoo_pool_id;
        END IF;
    END IF;
END $$`},
		{"seed_aol_ip_qf", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    yahoo_pool_id UUID;
    aol_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO yahoo_pool_id FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = org_id;
    SELECT id INTO aol_pool_id FROM mailing_ip_pools WHERE name = 'qf-aol-pool' AND organization_id = org_id;
    IF yahoo_pool_id IS NOT NULL AND aol_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = yahoo_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = aol_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = yahoo_pool_id;
        END IF;
    END IF;
END $$`},
		{"seed_aol_ip_ht", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    yahoo_pool_id UUID;
    aol_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO yahoo_pool_id FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = org_id;
    SELECT id INTO aol_pool_id FROM mailing_ip_pools WHERE name = 'ht-aol-pool' AND organization_id = org_id;
    IF yahoo_pool_id IS NOT NULL AND aol_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = yahoo_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = aol_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = yahoo_pool_id;
        END IF;
    END IF;
END $$`},
		{"seed_aol_ip_mh", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    yahoo_pool_id UUID;
    aol_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO yahoo_pool_id FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = org_id;
    SELECT id INTO aol_pool_id FROM mailing_ip_pools WHERE name = 'mh-aol-pool' AND organization_id = org_id;
    IF yahoo_pool_id IS NOT NULL AND aol_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = yahoo_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = aol_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = yahoo_pool_id;
        END IF;
    END IF;
END $$`},

		// Step 6.7: Seed SBC Global/BellSouth pools with IPs.
		//
		// Same pattern as AOL: for each brand, move the highest-numbered IP from the
		// ATT pool into the SBC Global pool. SBC Global/BellSouth shares ATT MX
		// infrastructure, so the IP's reputation carries over.
		{"seed_sbcglobal_ip_db", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    att_pool_id UUID;
    sbc_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO att_pool_id FROM mailing_ip_pools WHERE name = 'db-att-pool' AND organization_id = org_id;
    SELECT id INTO sbc_pool_id FROM mailing_ip_pools WHERE name = 'db-sbcglobal-pool' AND organization_id = org_id;
    IF att_pool_id IS NOT NULL AND sbc_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = att_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = sbc_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = att_pool_id;
        END IF;
    END IF;
END $$`},
		{"seed_sbcglobal_ip_qf", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    att_pool_id UUID;
    sbc_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO att_pool_id FROM mailing_ip_pools WHERE name = 'qf-att-pool' AND organization_id = org_id;
    SELECT id INTO sbc_pool_id FROM mailing_ip_pools WHERE name = 'qf-sbcglobal-pool' AND organization_id = org_id;
    IF att_pool_id IS NOT NULL AND sbc_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = att_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = sbc_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = att_pool_id;
        END IF;
    END IF;
END $$`},
		{"seed_sbcglobal_ip_ht", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    att_pool_id UUID;
    sbc_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO att_pool_id FROM mailing_ip_pools WHERE name = 'ht-att-pool' AND organization_id = org_id;
    SELECT id INTO sbc_pool_id FROM mailing_ip_pools WHERE name = 'ht-sbcglobal-pool' AND organization_id = org_id;
    IF att_pool_id IS NOT NULL AND sbc_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = att_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = sbc_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = att_pool_id;
        END IF;
    END IF;
END $$`},
		{"seed_sbcglobal_ip_mh", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    att_pool_id UUID;
    sbc_pool_id UUID;
    target_ip_id UUID;
BEGIN
    SELECT id INTO att_pool_id FROM mailing_ip_pools WHERE name = 'mh-att-pool' AND organization_id = org_id;
    SELECT id INTO sbc_pool_id FROM mailing_ip_pools WHERE name = 'mh-sbcglobal-pool' AND organization_id = org_id;
    IF att_pool_id IS NOT NULL AND sbc_pool_id IS NOT NULL THEN
        SELECT id INTO target_ip_id FROM mailing_ip_addresses
            WHERE pool_id = att_pool_id AND organization_id = org_id
            ORDER BY ip_address DESC LIMIT 1;
        IF target_ip_id IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = sbc_pool_id, updated_at = NOW()
                WHERE id = target_ip_id AND pool_id = att_pool_id;
        END IF;
    END IF;
END $$`},

		{"drop_subscriber_events_if_stale", `DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'subscriber_events' AND column_name = 'subscriber_id'
			) AND EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_name = 'subscriber_events'
			) THEN
				DROP TABLE subscriber_events CASCADE;
			END IF;
		END $$`},
		{"create_subscriber_events_v2", `CREATE TABLE IF NOT EXISTS subscriber_events (
			id BIGSERIAL PRIMARY KEY,
			email_hash VARCHAR(64) NOT NULL,
			event_type VARCHAR(50) NOT NULL DEFAULT 'page_view',
			campaign_id UUID,
			variant_id UUID,
			source VARCHAR(50) DEFAULT 'site',
			metadata JSONB DEFAULT '{}',
			event_at TIMESTAMPTZ DEFAULT NOW(),
			subscriber_id UUID,
			subscriber_email VARCHAR(255),
			ip_address VARCHAR(45),
			user_agent TEXT
		)`},
		{"idx_sub_events_hash_v2", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_hash ON subscriber_events(email_hash)`},
		{"idx_sub_events_type_v2", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_type ON subscriber_events(event_type)`},
		{"idx_sub_events_at_v2", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_at ON subscriber_events(event_at)`},
		{"idx_sub_events_source_v2", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_source ON subscriber_events(source)`},
		{"idx_sub_events_domain_v2", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_domain ON subscriber_events((metadata->>'domain'))`},
		{"idx_sub_events_subscriber_v2", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_subscriber ON subscriber_events(subscriber_id)`},
		{"grant_subscriber_events_v2", `DO $$
		BEGIN
			GRANT ALL ON TABLE subscriber_events TO ignite;
			GRANT ALL ON SEQUENCE subscriber_events_id_seq TO ignite;
		EXCEPTION WHEN OTHERS THEN NULL;
		END $$`},

		{"purge_besmed_tracking_events", `DELETE FROM mailing_tracking_events WHERE LOWER(sending_domain) LIKE '%besmed%'`},
		{"purge_besmed_queue", `DELETE FROM mailing_campaign_queue WHERE campaign_id IN (SELECT id FROM mailing_campaigns WHERE LOWER(from_email) LIKE '%besmed%')`},
		{"purge_besmed_campaigns", `DELETE FROM mailing_campaigns WHERE LOWER(from_email) LIKE '%besmed%'`},
		{"purge_besmed_sending_profiles", `DELETE FROM mailing_sending_profiles WHERE LOWER(sending_domain) LIKE '%besmed%'`},

		{"ensure_pgcrypto", `CREATE EXTENSION IF NOT EXISTS pgcrypto`},

		{"backfill_inbox_profile_counts_v1", `
			UPDATE mailing_inbox_profiles p SET
				total_sends = COALESCE(agg.delivered, 0),
				total_bounces = COALESCE(agg.bounced, 0),
				total_complaints = COALESCE(agg.complained, 0),
				updated_at = NOW()
			FROM (
				SELECT LOWER(s.email) as email,
					COUNT(*) FILTER (WHERE e.event_type = 'delivered') as delivered,
					COUNT(*) FILTER (WHERE e.event_type = 'bounced') as bounced,
					COUNT(*) FILTER (WHERE e.event_type = 'complained') as complained
				FROM mailing_tracking_events e
				JOIN mailing_subscribers s ON e.subscriber_id = s.id
				WHERE s.email IS NOT NULL
				GROUP BY LOWER(s.email)
			) agg
			WHERE LOWER(p.email) = agg.email
		`},

		{"backfill_inbox_profile_engagement_v1", `
			UPDATE mailing_inbox_profiles SET
				engagement_score = LEAST(
					CASE WHEN COALESCE(total_sends, 0) = 0 THEN 0.50
					ELSE
						(CAST(COALESCE(total_opens, 0) AS FLOAT) / total_sends * 0.6) +
						(CAST(COALESCE(total_clicks, 0) AS FLOAT) / total_sends * 0.4) +
						CASE WHEN last_open_at IS NOT NULL AND last_open_at > NOW() - INTERVAL '7 days' THEN 0.20
							 WHEN last_open_at IS NOT NULL AND last_open_at > NOW() - INTERVAL '30 days' THEN 0.10
							 ELSE 0 END
					END,
					1.0
				)
			WHERE total_sends > 0
		`},

		// Image CDN: hosted images storage
		{"create_mailing_hosted_images", `CREATE TABLE IF NOT EXISTS mailing_hosted_images (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id TEXT NOT NULL,
			filename TEXT NOT NULL DEFAULT '',
			original_filename TEXT NOT NULL DEFAULT '',
			content_type TEXT NOT NULL DEFAULT '',
			size BIGINT DEFAULT 0,
			width INT DEFAULT 0,
			height INT DEFAULT 0,
			s3_key TEXT NOT NULL DEFAULT '',
			s3_key_thumbnail TEXT DEFAULT '',
			s3_key_medium TEXT DEFAULT '',
			s3_key_large TEXT DEFAULT '',
			cdn_url TEXT NOT NULL DEFAULT '',
			cdn_url_thumbnail TEXT DEFAULT '',
			cdn_url_medium TEXT DEFAULT '',
			cdn_url_large TEXT DEFAULT '',
			checksum TEXT DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},

		// Image CDN: per-domain CloudFront distributions
		{"create_mailing_image_domains", `CREATE TABLE IF NOT EXISTS mailing_image_domains (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id TEXT NOT NULL,
			domain TEXT NOT NULL,
			verified BOOLEAN DEFAULT false,
			verification_token TEXT DEFAULT '',
			verification_method TEXT DEFAULT 'dns_txt',
			ssl_status TEXT DEFAULT 'pending',
			s3_bucket TEXT DEFAULT '',
			cloudfront_distribution_id TEXT DEFAULT '',
			cloudfront_domain TEXT DEFAULT '',
			acm_cert_arn TEXT DEFAULT '',
			last_verified_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`},

		// Offer creative assets: links hosted images to offers with placement metadata
		{"create_mailing_offer_creative_assets", `CREATE TABLE IF NOT EXISTS mailing_offer_creative_assets (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			offer_id UUID NOT NULL REFERENCES mailing_offers(id) ON DELETE CASCADE,
			hosted_image_id UUID NOT NULL REFERENCES mailing_hosted_images(id) ON DELETE CASCADE,
			asset_role VARCHAR(50) NOT NULL DEFAULT 'content',
			label TEXT NOT NULL DEFAULT '',
			width INT NOT NULL DEFAULT 0,
			height INT NOT NULL DEFAULT 0,
			cdn_url TEXT NOT NULL DEFAULT '',
			cdn_url_medium TEXT DEFAULT '',
			cdn_url_thumbnail TEXT DEFAULT '',
			original_filename TEXT NOT NULL DEFAULT '',
			file_size BIGINT DEFAULT 0,
			mime_type VARCHAR(50) DEFAULT '',
			sort_order INT DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(offer_id, hosted_image_id)
		)`},
		{"idx_offer_creative_assets_offer", `CREATE INDEX IF NOT EXISTS idx_offer_creative_assets_offer ON mailing_offer_creative_assets(offer_id)`},

		// Approved ad copy and taglines storage for offers
		{"add_offers_approved_ad_copy", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS approved_ad_copy TEXT DEFAULT ''`},
		{"add_offers_approved_taglines", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS approved_taglines TEXT DEFAULT ''`},

		// Offer-level opt-out link (advertiser unsub, distinct from Optizmo suppression link)
		{"add_offers_optout_link", `ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS offer_optout_link TEXT DEFAULT ''`},

		// Seed image domains for each sending domain (idempotent via ON CONFLICT)
		{"seed_img_domain_projectjarvis", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000001', '00000000-0000-0000-0000-000000000001', 'img.projectjarvis.io', true, 'issued', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_quizfiesta", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000002', '00000000-0000-0000-0000-000000000001', 'img.quizfiesta.com', true, 'issued', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_discountblog", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000003', '00000000-0000-0000-0000-000000000001', 'img.discountblog.com', true, 'issued', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_historythinking", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000004', '00000000-0000-0000-0000-000000000001', 'img.historythinking.com', true, 'issued', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_myownhealth", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000005', '00000000-0000-0000-0000-000000000001', 'img.myownhealth.net', true, 'issued', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},

		// Mark all image domains as verified with CloudFront details (deployed 2026-03-19)
		{"verify_img_projectjarvis", `UPDATE mailing_image_domains
			SET verified = true, ssl_status = 'issued',
			    cloudfront_distribution_id = 'E1Q4ZUVTMC8135', cloudfront_domain = 'd3j30mnhwt8cov.cloudfront.net',
			    acm_cert_arn = 'arn:aws:acm:us-east-1:146361001621:certificate/14f19cd1-9f0b-40ee-be9e-185b329ecd79',
			    last_verified_at = NOW(), updated_at = NOW()
			WHERE domain = 'img.projectjarvis.io' AND verified = false`},
		{"verify_img_discountblog", `UPDATE mailing_image_domains
			SET verified = true, ssl_status = 'issued',
			    cloudfront_distribution_id = 'EIRUDR4162AFE', cloudfront_domain = 'd1gsmudv691ro6.cloudfront.net',
			    acm_cert_arn = 'arn:aws:acm:us-east-1:146361001621:certificate/4a690e09-e3d5-4e62-a677-72e0bab3062d',
			    last_verified_at = NOW(), updated_at = NOW()
			WHERE domain = 'img.discountblog.com' AND verified = false`},
		{"verify_img_quizfiesta", `UPDATE mailing_image_domains
			SET verified = true, ssl_status = 'issued',
			    cloudfront_distribution_id = 'EX9UHM32M77QA', cloudfront_domain = 'd253b1ujl076fp.cloudfront.net',
			    acm_cert_arn = 'arn:aws:acm:us-east-1:146361001621:certificate/1212bd64-5673-4395-bb55-fad8b6486f36',
			    last_verified_at = NOW(), updated_at = NOW()
			WHERE domain = 'img.quizfiesta.com' AND verified = false`},
		{"verify_img_historythinking", `UPDATE mailing_image_domains
			SET verified = true, ssl_status = 'issued',
			    cloudfront_distribution_id = 'E1MIZEFJJJYE48', cloudfront_domain = 'd31ok8yi8usnbc.cloudfront.net',
			    acm_cert_arn = 'arn:aws:acm:us-east-1:146361001621:certificate/e327f18d-0960-4e25-81f6-c7bbdba53ed5',
			    last_verified_at = NOW(), updated_at = NOW()
			WHERE domain = 'img.historythinking.com' AND verified = false`},
		{"verify_img_myownhealth", `UPDATE mailing_image_domains
			SET verified = true, ssl_status = 'issued',
			    cloudfront_distribution_id = 'E2JKJ1EU6CA650', cloudfront_domain = 'd6x9gyfp63ht8.cloudfront.net',
			    acm_cert_arn = 'arn:aws:acm:us-east-1:146361001621:certificate/11a43487-2731-40fc-91f7-ce6e159b18de',
			    last_verified_at = NOW(), updated_at = NOW()
			WHERE domain = 'img.myownhealth.net' AND verified = false`},

		// =====================================================================
		// Phase 7: Shared OVH pools (must run AFTER Phase 6 IP seeding)
		// =====================================================================

		// 7.1: Create shared pools
		{"phase7_startup_create_shared_a", `INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status, created_at, updated_at)
			VALUES ('00000000-0000-0000-0000-000000000001', 'shared-a', 'Shared /28 OVH IPs on Server A (15.204.22.177-.191)', 'shared', 'active', NOW(), NOW())
			ON CONFLICT (organization_id, name) DO NOTHING`},
		{"phase7_startup_create_shared_b", `INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status, created_at, updated_at)
			VALUES ('00000000-0000-0000-0000-000000000001', 'shared-b', 'Shared /28 OVH IPs on Server B (15.204.38.160-.175)', 'shared', 'active', NOW(), NOW())
			ON CONFLICT (organization_id, name) DO NOTHING`},

		// 7.2: Move Server A /28 OVH IPs to shared-a pool
		{"phase7_startup_move_ips_shared_a", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
    rec RECORD;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'shared-a' AND organization_id = org_id;
    IF pool_id_val IS NULL THEN RAISE NOTICE 'shared-a pool not found'; RETURN; END IF;
    FOR rec IN
        SELECT * FROM (VALUES
            ('15.204.22.177','mta-a-shrd1.mail.em.discountblog.com'),('15.204.22.178','mta-a-shrd2.mail.em.discountblog.com'),
            ('15.204.22.179','mta-a-shrd3.mail.em.discountblog.com'),('15.204.22.180','mta-a-shrd4.mail.em.discountblog.com'),
            ('15.204.22.181','mta-a-shrd5.mail.em.discountblog.com'),('15.204.22.182','mta-a-shrd6.mail.em.discountblog.com'),
            ('15.204.22.183','mta-a-shrd7.mail.em.discountblog.com'),('15.204.22.184','mta-a-shrd8.mail.em.discountblog.com'),
            ('15.204.22.185','mta-a-shrd9.mail.em.quizfiesta.com'),('15.204.22.186','mta-a-shrd10.mail.em.quizfiesta.com'),
            ('15.204.22.187','mta-a-shrd11.mail.em.quizfiesta.com'),('15.204.22.188','mta-a-shrd12.mail.em.quizfiesta.com'),
            ('15.204.22.189','mta-a-shrd13.mail.em.quizfiesta.com'),('15.204.22.190','mta-a-shrd14.mail.em.quizfiesta.com'),
            ('15.204.22.191','mta-a-shrd15.mail.em.quizfiesta.com')
        ) AS t(ip_addr, new_hostname)
    LOOP
        UPDATE mailing_ip_addresses SET pool_id = pool_id_val, hostname = rec.new_hostname, updated_at = NOW()
        WHERE ip_address = rec.ip_addr::inet;
    END LOOP;
END $$`},

		// 7.3: Seed Server B /28 OVH IPs into shared-b pool
		{"phase7_startup_seed_shared_b", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
    server_id_val UUID;
    i INT;
    ip_text TEXT;
    hostname_val TEXT;
    shrd_num INT;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'shared-b' AND organization_id = org_id;
    SELECT id INTO server_id_val FROM mailing_pmta_servers WHERE host = '15.204.107.107';
    IF pool_id_val IS NULL OR server_id_val IS NULL THEN RAISE NOTICE 'shared-b pool or server B not found'; RETURN; END IF;
    FOR i IN 0..15 LOOP
        ip_text := '15.204.38.' || (160 + i);
        shrd_num := i + 1;
        IF shrd_num <= 8 THEN
            hostname_val := 'mta-b-shrd' || shrd_num || '.mail.em.historythinking.com';
        ELSE
            hostname_val := 'mta-b-shrd' || shrd_num || '.mail.em.myownhealth.net';
        END IF;
        INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, status, pool_id, pmta_server_id,
            warmup_stage, warmup_day, warmup_daily_limit, warmup_started_at, hosting_provider, acquisition_type,
            cidr_block, rdns_verified, reputation_score, created_at, updated_at)
        VALUES (gen_random_uuid(), org_id, ip_text::inet, hostname_val, 'warmup', pool_id_val, server_id_val,
            'warming', 1, 10000, NOW(), 'OVH', 'purchased', '15.204.38.160/28', false, 50.0, NOW(), NOW())
        ON CONFLICT (ip_address) DO UPDATE SET
            pool_id = pool_id_val, pmta_server_id = server_id_val, hostname = hostname_val,
            warmup_started_at = COALESCE(mailing_ip_addresses.warmup_started_at, NOW()), updated_at = NOW();
    END LOOP;
END $$`},

		// 7.4: (superseded by phase 8 below — kept as no-ops for history)

		// 7.5: Force-correct shared-a pool assignment (plain SQL, no PL/pgSQL)
		{"phase7_force_shared_a", `UPDATE mailing_ip_addresses
SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'shared-a' AND organization_id = '00000000-0000-0000-0000-000000000001'),
    updated_at = NOW()
WHERE ip_address IN ('15.204.22.177'::inet,'15.204.22.178'::inet,'15.204.22.179'::inet,'15.204.22.180'::inet,
    '15.204.22.181'::inet,'15.204.22.182'::inet,'15.204.22.183'::inet,'15.204.22.184'::inet,
    '15.204.22.185'::inet,'15.204.22.186'::inet,'15.204.22.187'::inet,'15.204.22.188'::inet,
    '15.204.22.189'::inet,'15.204.22.190'::inet,'15.204.22.191'::inet)
AND (pool_id IS NULL OR pool_id != (SELECT id FROM mailing_ip_pools WHERE name = 'shared-a' AND organization_id = '00000000-0000-0000-0000-000000000001'))`},

		// 7.6: Force-correct shared-b pool assignment (plain SQL, no PL/pgSQL)
		{"phase7_force_shared_b", `UPDATE mailing_ip_addresses
SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'shared-b' AND organization_id = '00000000-0000-0000-0000-000000000001'),
    updated_at = NOW()
WHERE ip_address IN ('15.204.38.160'::inet,'15.204.38.161'::inet,'15.204.38.162'::inet,'15.204.38.163'::inet,
    '15.204.38.164'::inet,'15.204.38.165'::inet,'15.204.38.166'::inet,'15.204.38.167'::inet,
    '15.204.38.168'::inet,'15.204.38.169'::inet,'15.204.38.170'::inet,'15.204.38.171'::inet,
    '15.204.38.172'::inet,'15.204.38.173'::inet,'15.204.38.174'::inet,'15.204.38.175'::inet)
AND (pool_id IS NULL OR pool_id != (SELECT id FROM mailing_ip_pools WHERE name = 'shared-b' AND organization_id = '00000000-0000-0000-0000-000000000001'))`},

		// =====================================================================
		// Phase 8: Switch to ISP-dedicated pools (IPXO IPs)
		// Runs in startup migrations (guaranteed to execute on every boot).
		// =====================================================================
		// 2026-05-30: added NOT LIKE 'ses-relay%' guard so this catch-all
		// reseed (no pool_prefix-state filter) does not clobber the SES
		// relay profile. Same root cause as phase6 — see comment there.
		{"phase8_startup_route_db", `UPDATE mailing_sending_profiles SET pool_prefix = 'db', smtp_host = '15.204.101.125', api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND status = 'active' AND COALESCE(ip_pool,'') NOT LIKE 'ses-relay%'`},
		{"phase8_startup_route_qf", `UPDATE mailing_sending_profiles SET pool_prefix = 'qf', smtp_host = '15.204.101.125', api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND status = 'active'`},
		{"phase8_startup_route_ht", `UPDATE mailing_sending_profiles SET pool_prefix = 'ht', smtp_host = '15.204.107.107', api_endpoint = 'http://15.204.107.107:19099', updated_at = NOW() WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND status = 'active'`},
		{"phase8_startup_route_mh", `UPDATE mailing_sending_profiles SET pool_prefix = 'mh', smtp_host = '15.204.107.107', api_endpoint = 'http://15.204.107.107:19099', updated_at = NOW() WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND status = 'active'`},
		{"phase8_startup_ipxo_warmup", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 1000, updated_at = NOW() WHERE cidr_block IN ('144.225.178.0/25', '144.225.178.128/25') AND warmup_daily_limit < 1000`},

		// =====================================================================
		// May 8 2026: Seven new sending domains.
		// Harvest 21 IPs from existing brand general pools (db, qf, ht, mh)
		// and assign them to new <prefix>-general-pool per new domain.
		// IPs themselves are kept on the same physical PMTA server; only
		// pool_id, hostname, and the corresponding sending profile change.
		// PMTA config on Server A and Server B is updated separately and
		// the new VMTAs (mta-bw-gn1..3, etc.) already exist in PMTA.
		// =====================================================================
		{"may08_create_new_general_pools", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at)
SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', t.pool_name, t.descr, 'dedicated', 'active', NOW(), NOW()
FROM (VALUES
    ('bw-general-pool', 'BusinessWeeklyPro general ISP pool'),
    ('fc-general-pool', 'FinancialCalculate general ISP pool'),
    ('cp-general-pool', 'ConsumerPro general ISP pool'),
    ('hw-general-pool', 'HomeWarrantyServices general ISP pool'),
    ('rr-general-pool', 'RefinanceRatesUSA general ISP pool'),
    ('tt-general-pool', 'ThingOfTheDay general ISP pool'),
    ('yi-general-pool', 'YourInsuranceHub general ISP pool')
) AS t(pool_name, descr)
WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools p WHERE p.name = t.pool_name AND p.organization_id = '00000000-0000-0000-0000-000000000001')`},

		{"may08_reassign_harvested_ips", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            -- Server A harvests
            ('144.225.178.51',  'mta-bw-gn1.mail.em.businessweeklypro.com',     'bw-general-pool'),
            ('144.225.178.52',  'mta-bw-gn2.mail.em.businessweeklypro.com',     'bw-general-pool'),
            ('144.225.178.53',  'mta-bw-gn3.mail.em.businessweeklypro.com',     'bw-general-pool'),
            ('144.225.178.115', 'mta-fc-gn1.mail.em.financialcalculate.com',    'fc-general-pool'),
            ('144.225.178.116', 'mta-fc-gn2.mail.em.financialcalculate.com',    'fc-general-pool'),
            ('144.225.178.117', 'mta-fc-gn3.mail.em.financialcalculate.com',    'fc-general-pool'),
            ('144.225.178.54',  'mta-cp-gn1.mail.em.consumerpro.net',           'cp-general-pool'),
            ('144.225.178.118', 'mta-cp-gn2.mail.em.consumerpro.net',           'cp-general-pool'),
            ('144.225.178.119', 'mta-cp-gn3.mail.em.consumerpro.net',           'cp-general-pool'),
            -- Server B harvests
            ('144.225.178.186', 'mta-hw-gn1.mail.em.homewarrantyservices.org',  'hw-general-pool'),
            ('144.225.178.187', 'mta-hw-gn2.mail.em.homewarrantyservices.org',  'hw-general-pool'),
            ('144.225.178.188', 'mta-hw-gn3.mail.em.homewarrantyservices.org',  'hw-general-pool'),
            ('144.225.178.189', 'mta-rr-gn1.mail.em.refinanceratesusa.com',     'rr-general-pool'),
            ('144.225.178.190', 'mta-rr-gn2.mail.em.refinanceratesusa.com',     'rr-general-pool'),
            ('144.225.178.191', 'mta-rr-gn3.mail.em.refinanceratesusa.com',     'rr-general-pool'),
            ('144.225.178.250', 'mta-tt-gn1.mail.em.thingoftheday.org',         'tt-general-pool'),
            ('144.225.178.251', 'mta-tt-gn2.mail.em.thingoftheday.org',         'tt-general-pool'),
            ('144.225.178.252', 'mta-tt-gn3.mail.em.thingoftheday.org',         'tt-general-pool'),
            ('144.225.178.253', 'mta-yi-gn1.mail.em.yourinsurancehub.com',      'yi-general-pool'),
            ('144.225.178.254', 'mta-yi-gn2.mail.em.yourinsurancehub.com',      'yi-general-pool'),
            ('144.225.178.255', 'mta-yi-gn3.mail.em.yourinsurancehub.com',      'yi-general-pool')
        ) AS t(ip_addr, hostname, pool_name)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses
               SET pool_id = pool_id_val,
                   hostname = rec.hostname,
                   status = 'active',
                   warmup_stage = COALESCE(NULLIF(warmup_stage, ''), 'early'),
                   updated_at = NOW()
             WHERE ip_address = rec.ip_addr::inet;
        END IF;
    END LOOP;
END $$`},

		{"may08_seed_sending_profiles", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', t.name, 'pmta', t.from_name, t.from_email, t.reply_email, t.sending_domain, t.smtp_host, 587, t.api_endpoint, t.tracking_domain, 1500, 12000, t.ip_pool, t.prefix, 'active', false, NOW(), NOW()
FROM (VALUES
    ('BusinessWeeklyPro PMTA',  'Business Weekly Pro',     'hello@em.businessweeklypro.com',     'reply@em.businessweeklypro.com',     'em.businessweeklypro.com',     '15.204.101.125', 'http://15.204.101.125:19099', 't.em.businessweeklypro.com',     'bw-general-pool', 'bw'),
    ('FinancialCalculate PMTA', 'Financial Calculate',     'hello@em.financialcalculate.com',    'reply@em.financialcalculate.com',    'em.financialcalculate.com',    '15.204.101.125', 'http://15.204.101.125:19099', 't.em.financialcalculate.com',    'fc-general-pool', 'fc'),
    ('ConsumerPro PMTA',        'Consumer Pro',            'hello@em.consumerpro.net',           'reply@em.consumerpro.net',           'em.consumerpro.net',           '15.204.101.125', 'http://15.204.101.125:19099', 't.em.consumerpro.net',           'cp-general-pool', 'cp'),
    ('HomeWarrantyServices PMTA','Home Warranty Services', 'hello@em.homewarrantyservices.org',  'reply@em.homewarrantyservices.org',  'em.homewarrantyservices.org',  '15.204.107.107', 'http://15.204.107.107:19099', 't.em.homewarrantyservices.org',  'hw-general-pool', 'hw'),
    ('RefinanceRatesUSA PMTA',  'Refinance Rates USA',     'hello@em.refinanceratesusa.com',     'reply@em.refinanceratesusa.com',     'em.refinanceratesusa.com',     '15.204.107.107', 'http://15.204.107.107:19099', 't.em.refinanceratesusa.com',     'rr-general-pool', 'rr'),
    ('ThingOfTheDay PMTA',      'Thing of the Day',        'hello@em.thingoftheday.org',         'reply@em.thingoftheday.org',         'em.thingoftheday.org',         '15.204.107.107', 'http://15.204.107.107:19099', 't.em.thingoftheday.org',         'tt-general-pool', 'tt'),
    ('YourInsuranceHub PMTA',   'Your Insurance Hub',      'hello@em.yourinsurancehub.com',      'reply@em.yourinsurancehub.com',      'em.yourinsurancehub.com',      '15.204.107.107', 'http://15.204.107.107:19099', 't.em.yourinsurancehub.com',      'yi-general-pool', 'yi')
) AS t(name, from_name, from_email, reply_email, sending_domain, smtp_host, api_endpoint, tracking_domain, ip_pool, prefix)
WHERE NOT EXISTS (
    SELECT 1 FROM mailing_sending_profiles p
    WHERE p.sending_domain = t.sending_domain
      AND p.organization_id = '00000000-0000-0000-0000-000000000001'
)`},

		// Idempotent re-anchor: if profiles existed but pointing at wrong server / missing pool_prefix, fix them.
		{"may08_reassert_pool_prefix", `UPDATE mailing_sending_profiles SET pool_prefix = sub.prefix, smtp_host = sub.smtp_host, api_endpoint = sub.api_endpoint, ip_pool = sub.ip_pool, tracking_domain = sub.tracking_domain, updated_at = NOW()
FROM (VALUES
    ('em.businessweeklypro.com',  'bw', '15.204.101.125', 'http://15.204.101.125:19099', 'bw-general-pool', 't.em.businessweeklypro.com'),
    ('em.financialcalculate.com', 'fc', '15.204.101.125', 'http://15.204.101.125:19099', 'fc-general-pool', 't.em.financialcalculate.com'),
    ('em.consumerpro.net',        'cp', '15.204.101.125', 'http://15.204.101.125:19099', 'cp-general-pool', 't.em.consumerpro.net'),
    ('em.homewarrantyservices.org','hw','15.204.107.107', 'http://15.204.107.107:19099', 'hw-general-pool', 't.em.homewarrantyservices.org'),
    ('em.refinanceratesusa.com',  'rr', '15.204.107.107', 'http://15.204.107.107:19099', 'rr-general-pool', 't.em.refinanceratesusa.com'),
    ('em.thingoftheday.org',      'tt', '15.204.107.107', 'http://15.204.107.107:19099', 'tt-general-pool', 't.em.thingoftheday.org'),
    ('em.yourinsurancehub.com',   'yi', '15.204.107.107', 'http://15.204.107.107:19099', 'yi-general-pool', 't.em.yourinsurancehub.com')
) AS sub(sending_domain, prefix, smtp_host, api_endpoint, ip_pool, tracking_domain)
WHERE mailing_sending_profiles.sending_domain = sub.sending_domain
  AND mailing_sending_profiles.vendor_type = 'pmta'
  AND mailing_sending_profiles.organization_id = '00000000-0000-0000-0000-000000000001'
  AND (
        COALESCE(mailing_sending_profiles.pool_prefix, '') != sub.prefix
        OR COALESCE(mailing_sending_profiles.smtp_host, '') != sub.smtp_host
        OR COALESCE(mailing_sending_profiles.api_endpoint, '') != sub.api_endpoint
        OR COALESCE(mailing_sending_profiles.ip_pool, '') != sub.ip_pool
        OR COALESCE(mailing_sending_profiles.tracking_domain, '') != sub.tracking_domain
  )`},

		// Persona aliases for the May 2026 seven-brand expansion.
		// Mirrors the existing convention ("Jamie @ Discount Blog",
		// "Arnold @ My Own Health"). Idempotent: only updates rows that
		// are still on the bare brand name (the original seed value).
		{"may08_seed_brand_persona_from_names", `UPDATE mailing_sending_profiles
SET from_name = sub.persona, updated_at = NOW()
FROM (VALUES
    ('em.businessweeklypro.com',     'Marcus @ Business Weekly Pro',     'Business Weekly Pro'),
    ('em.financialcalculate.com',    'Eleanor @ Financial Calculate',    'Financial Calculate'),
    ('em.consumerpro.net',           'Diane @ Consumer Pro',             'Consumer Pro'),
    ('em.homewarrantyservices.org',  'Hank @ Home Warranty Services',    'Home Warranty Services'),
    ('em.refinanceratesusa.com',     'Frank @ Refinance Rates USA',      'Refinance Rates USA'),
    ('em.thingoftheday.org',         'Olivia @ Thing of the Day',        'Thing of the Day'),
    ('em.yourinsurancehub.com',      'Carl @ Your Insurance Hub',        'Your Insurance Hub')
) AS sub(sending_domain, persona, bare_name)
WHERE mailing_sending_profiles.sending_domain = sub.sending_domain
  AND mailing_sending_profiles.vendor_type = 'pmta'
  AND mailing_sending_profiles.organization_id = '00000000-0000-0000-0000-000000000001'
  AND COALESCE(mailing_sending_profiles.from_name, '') = sub.bare_name`},

		// Image-CDN seeds for the seven new brands. Each row creates an
		// idempotent mailing_image_domains entry that initially POINTS AT
		// the shared org-default CloudFront (img.projectjarvis.io). Once a
		// dedicated img.<brand> CloudFront is provisioned, the seed-row
		// can be UPDATEd to point at the brand-specific distribution
		// without touching application code.
		// Seed values reflect the dedicated per-brand CloudFront distros provisioned 2026-05-08.
		// All 7 share OAC E1SXZG33G45K6R, S3 origin jarvis-image-cdn.s3.us-west-2, Managed-CachingOptimized
		// cache policy 658327ea-f89d-4fab-a63d-7e88639e58f6, ACM certs in us-east-1.
		// Production rows already have these values; INSERT is a no-op via ON CONFLICT (id).
		// If the table is ever re-seeded from scratch, these values are correct as of 2026-05-08.
		{"may08_seed_img_domains_new_brands", `INSERT INTO mailing_image_domains
(id, org_id, domain, verified, ssl_status, s3_bucket, cloudfront_distribution_id, cloudfront_domain, acm_cert_arn, last_verified_at, created_at, updated_at)
VALUES
    ('d0000000-0000-0000-0001-000000000010', '00000000-0000-0000-0000-000000000001', 'img.businessweeklypro.com',    true, 'active', 'jarvis-image-cdn', 'E1MBWGFE62ND38', 'd7z6gova229dh.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/083c1055-25a5-49c8-af67-8a9d897be278', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000011', '00000000-0000-0000-0000-000000000001', 'img.financialcalculate.com',   true, 'active', 'jarvis-image-cdn', 'E2ON5XDX85OJ3',  'd3wzevxq8a765.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/75ebddb7-7589-4382-a7b2-e10f501c8e16', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000012', '00000000-0000-0000-0000-000000000001', 'img.consumerpro.net',          true, 'active', 'jarvis-image-cdn', 'E2AR0753VUOS08', 'd2iwzeqyecpo3h.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/3cb8db0b-5ce8-41fb-9dea-e7cefaf8e32d', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000013', '00000000-0000-0000-0000-000000000001', 'img.homewarrantyservices.org', true, 'active', 'jarvis-image-cdn', 'E12VZ45O0AJ082', 'drinizukkgd9n.cloudfront.net',  'arn:aws:acm:us-east-1:146361001621:certificate/c3d7dff8-ca7b-4a74-8e3c-32d51eac4555', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000014', '00000000-0000-0000-0000-000000000001', 'img.refinanceratesusa.com',    true, 'active', 'jarvis-image-cdn', 'E2HVU91OSSZUNV', 'd5h8617hpxs2x.cloudfront.net',  'arn:aws:acm:us-east-1:146361001621:certificate/db03ebd3-2113-4e17-9cf1-e9ca113f55c9', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000015', '00000000-0000-0000-0000-000000000001', 'img.thingoftheday.org',        true, 'active', 'jarvis-image-cdn', 'E1TNTZWMK4SNBK', 'd16geycs7mna9k.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/0fc97356-88da-4eb6-8161-0f3ca1904aec', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000016', '00000000-0000-0000-0000-000000000001', 'img.yourinsurancehub.com',     true, 'active', 'jarvis-image-cdn', 'E2CLOW0KBLMBE1', 'd138yhttpn3r8q.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/236ec74a-a26a-4069-840c-85b007145e2b', NOW(), NOW(), NOW())
ON CONFLICT (id) DO NOTHING`},

		// Brand metadata table — single source of truth for footer/SES/persona
		// substitutions across deploy scripts. Replaces the per-deploy-script
		// BRANDS / publisher_name / physical_address dictionaries. Deploy
		// scripts can SELECT from this table at runtime to derive footer
		// values without hardcoding.
		{"may08_create_brand_metadata", `CREATE TABLE IF NOT EXISTS mailing_brand_metadata (
			brand_root        TEXT PRIMARY KEY,
			brand_code        TEXT NOT NULL UNIQUE,
			brand_label       TEXT NOT NULL,
			sending_domain    TEXT NOT NULL,
			tracking_domain   TEXT NOT NULL,
			image_domain      TEXT NOT NULL,
			from_name         TEXT NOT NULL,
			from_email        TEXT NOT NULL,
			reply_email       TEXT NOT NULL,
			physical_address  TEXT NOT NULL,
			organization_id   UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
			status            TEXT NOT NULL DEFAULT 'active',
			notes             TEXT,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},

		// Seed all 11 brands (4 existing + 7 new). Idempotent — re-run is a no-op.
		{"may08_seed_brand_metadata", `INSERT INTO mailing_brand_metadata
(brand_root, brand_code, brand_label, sending_domain, tracking_domain, image_domain, from_name, from_email, reply_email, physical_address, notes)
VALUES
    ('discountblog.com',         'DB',  'Discount Blog',           'em.discountblog.com',          't.em.discountblog.com',          'img.discountblog.com',          'Jamie @ Discount Blog',            'hello@em.discountblog.com',          'reply@em.discountblog.com',          '784 S. Clearwater Loop, Ste. R, Post Falls, ID 83854', 'Existing brand; verified persona pattern'),
    ('historythinking.com',      'HT',  'History Thinking',        'em.historythinking.com',       'trk.em.historythinking.com',     'img.historythinking.com',       'History Thinking',                 'hello@em.historythinking.com',       'reply@em.historythinking.com',       '1309 Coffeen Ave STE 1200, Sheridan, WY 82801',        'Existing brand; brand-name from-name (no persona)'),
    ('myownhealth.net',          'MH',  'My Own Health',           'em.myownhealth.net',           'trk.em.myownhealth.net',         'img.myownhealth.net',           'Arnold @ My Own Health',           'hello@em.myownhealth.net',           'reply@em.myownhealth.net',           '784 S. Clearwater Loop, Ste. R, Post Falls, ID 83854', 'Existing brand; verified persona pattern'),
    ('quizfiesta.com',           'QF',  'Quiz Fiesta',             'em.quizfiesta.com',            't.em.quizfiesta.com',            'img.quizfiesta.com',            'Quiz Master',                      'hello@em.quizfiesta.com',            'reply@em.quizfiesta.com',            '1309 Coffeen Avenue STE 1200, Sheridan, WY 82801',     'Existing brand; non-personal from-name'),
    ('businessweeklypro.com',    'BW',  'Business Weekly Pro',     'em.businessweeklypro.com',     't.em.businessweeklypro.com',     'img.businessweeklypro.com',     'Marcus @ Business Weekly Pro',     'hello@em.businessweeklypro.com',     'reply@em.businessweeklypro.com',     '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion; img CDN shared with org default until dedicated CloudFront provisioned'),
    ('financialcalculate.com',   'FC',  'Financial Calculate',     'em.financialcalculate.com',    't.em.financialcalculate.com',    'img.financialcalculate.com',    'Eleanor @ Financial Calculate',    'hello@em.financialcalculate.com',    'reply@em.financialcalculate.com',    '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion'),
    ('consumerpro.net',          'CP',  'Consumer Pro',            'em.consumerpro.net',           't.em.consumerpro.net',           'img.consumerpro.net',           'Diane @ Consumer Pro',             'hello@em.consumerpro.net',           'reply@em.consumerpro.net',           '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion'),
    ('homewarrantyservices.org', 'HW',  'Home Warranty Services',  'em.homewarrantyservices.org',  't.em.homewarrantyservices.org',  'img.homewarrantyservices.org',  'Hank @ Home Warranty Services',    'hello@em.homewarrantyservices.org',  'reply@em.homewarrantyservices.org',  '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion'),
    ('refinanceratesusa.com',    'RR',  'Refinance Rates USA',     'em.refinanceratesusa.com',     't.em.refinanceratesusa.com',     'img.refinanceratesusa.com',     'Frank @ Refinance Rates USA',      'hello@em.refinanceratesusa.com',     'reply@em.refinanceratesusa.com',     '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion'),
    ('thingoftheday.org',        'TT',  'Thing of the Day',        'em.thingoftheday.org',         't.em.thingoftheday.org',         'img.thingoftheday.org',         'Olivia @ Thing of the Day',        'hello@em.thingoftheday.org',         'reply@em.thingoftheday.org',         '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion'),
    ('yourinsurancehub.com',     'YI',  'Your Insurance Hub',      'em.yourinsurancehub.com',      't.em.yourinsurancehub.com',      'img.yourinsurancehub.com',      'Carl @ Your Insurance Hub',        'hello@em.yourinsurancehub.com',      'reply@em.yourinsurancehub.com',      '30 N Gould St Ste R, Sheridan, WY 82801',              'May 2026 expansion')
ON CONFLICT (brand_root) DO NOTHING`},

		// =====================================================================
		// May 9 2026: Four additional sending domains.
		// Same pattern as may08 batch — harvest 12 IPs from existing legacy
		// brand ISP-tier pools (db/qf apple+comcast+charter on Server A,
		// ht/mh apple+comcast+charter on Server B) and assign to new
		// <prefix>-general-pool per new domain. Donor pools each shed 1 IP
		// (apple/comcast/charter pools drop from 7-8 IPs each to 6-7);
		// donor brands keep majority warmup capacity intact.
		// PMTA config on Server A and Server B already updated.
		// =====================================================================
		{"may09_create_new_general_pools", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at)
SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', t.pool_name, t.descr, 'dedicated', 'active', NOW(), NOW()
FROM (VALUES
    ('mr-general-pool', 'MyRepairDIY general ISP pool'),
    ('ci-general-pool', 'CasaInsure general ISP pool'),
    ('lp-general-pool', 'LearnPersonalLoans general ISP pool'),
    ('rb-general-pool', 'RatesBazar general ISP pool')
) AS t(pool_name, descr)
WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools p WHERE p.name = t.pool_name AND p.organization_id = '00000000-0000-0000-0000-000000000001')`},

		{"may09_reassign_harvested_ips", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            -- Server A harvests (donor: db/qf apple+comcast+charter)
            ('144.225.178.26',  'mta-mr-gn1.mail.em.myrepairdiy.com',        'mr-general-pool'),
            ('144.225.178.32',  'mta-mr-gn2.mail.em.myrepairdiy.com',        'mr-general-pool'),
            ('144.225.178.50',  'mta-mr-gn3.mail.em.myrepairdiy.com',        'mr-general-pool'),
            ('144.225.178.90',  'mta-ci-gn1.mail.em.casainsure.com',         'ci-general-pool'),
            ('144.225.178.96',  'mta-ci-gn2.mail.em.casainsure.com',         'ci-general-pool'),
            ('144.225.178.114', 'mta-ci-gn3.mail.em.casainsure.com',         'ci-general-pool'),
            -- Server B harvests (donor: ht/mh apple+comcast+charter)
            ('144.225.178.158', 'mta-lp-gn1.mail.em.learnpersonalloans.com', 'lp-general-pool'),
            ('144.225.178.165', 'mta-lp-gn2.mail.em.learnpersonalloans.com', 'lp-general-pool'),
            ('144.225.178.185', 'mta-lp-gn3.mail.em.learnpersonalloans.com', 'lp-general-pool'),
            ('144.225.178.222', 'mta-rb-gn1.mail.em.ratesbazar.com',         'rb-general-pool'),
            ('144.225.178.229', 'mta-rb-gn2.mail.em.ratesbazar.com',         'rb-general-pool'),
            ('144.225.178.249', 'mta-rb-gn3.mail.em.ratesbazar.com',         'rb-general-pool')
        ) AS t(ip_addr, hostname, pool_name)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses
               SET pool_id = pool_id_val,
                   hostname = rec.hostname,
                   status = 'active',
                   warmup_stage = COALESCE(NULLIF(warmup_stage, ''), 'early'),
                   updated_at = NOW()
             WHERE ip_address = rec.ip_addr::inet;
        END IF;
    END LOOP;
END $$`},

		{"may09_seed_sending_profiles", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', t.name, 'pmta', t.from_name, t.from_email, t.reply_email, t.sending_domain, t.smtp_host, 587, t.api_endpoint, t.tracking_domain, 1500, 12000, t.ip_pool, t.prefix, 'active', false, NOW(), NOW()
FROM (VALUES
    ('MyRepairDIY PMTA',         'My Repair DIY',          'hello@em.myrepairdiy.com',        'reply@em.myrepairdiy.com',        'em.myrepairdiy.com',        '15.204.101.125', 'http://15.204.101.125:19099', 't.em.myrepairdiy.com',        'mr-general-pool', 'mr'),
    ('CasaInsure PMTA',          'Casa Insure',            'hello@em.casainsure.com',         'reply@em.casainsure.com',         'em.casainsure.com',         '15.204.101.125', 'http://15.204.101.125:19099', 't.em.casainsure.com',         'ci-general-pool', 'ci'),
    ('LearnPersonalLoans PMTA',  'Learn Personal Loans',   'hello@em.learnpersonalloans.com', 'reply@em.learnpersonalloans.com', 'em.learnpersonalloans.com', '15.204.107.107', 'http://15.204.107.107:19099', 't.em.learnpersonalloans.com', 'lp-general-pool', 'lp'),
    ('RatesBazar PMTA',          'Rates Bazar',            'hello@em.ratesbazar.com',         'reply@em.ratesbazar.com',         'em.ratesbazar.com',         '15.204.107.107', 'http://15.204.107.107:19099', 't.em.ratesbazar.com',         'rb-general-pool', 'rb')
) AS t(name, from_name, from_email, reply_email, sending_domain, smtp_host, api_endpoint, tracking_domain, ip_pool, prefix)
WHERE NOT EXISTS (
    SELECT 1 FROM mailing_sending_profiles p
    WHERE p.sending_domain = t.sending_domain
      AND p.organization_id = '00000000-0000-0000-0000-000000000001'
)`},

		// Idempotent re-anchor (mirrors may08_reassert_pool_prefix).
		{"may09_reassert_pool_prefix", `UPDATE mailing_sending_profiles SET pool_prefix = sub.prefix, smtp_host = sub.smtp_host, api_endpoint = sub.api_endpoint, ip_pool = sub.ip_pool, tracking_domain = sub.tracking_domain, updated_at = NOW()
FROM (VALUES
    ('em.myrepairdiy.com',        'mr', '15.204.101.125', 'http://15.204.101.125:19099', 'mr-general-pool', 't.em.myrepairdiy.com'),
    ('em.casainsure.com',         'ci', '15.204.101.125', 'http://15.204.101.125:19099', 'ci-general-pool', 't.em.casainsure.com'),
    ('em.learnpersonalloans.com', 'lp', '15.204.107.107', 'http://15.204.107.107:19099', 'lp-general-pool', 't.em.learnpersonalloans.com'),
    ('em.ratesbazar.com',         'rb', '15.204.107.107', 'http://15.204.107.107:19099', 'rb-general-pool', 't.em.ratesbazar.com')
) AS sub(sending_domain, prefix, smtp_host, api_endpoint, ip_pool, tracking_domain)
WHERE mailing_sending_profiles.sending_domain = sub.sending_domain
  AND mailing_sending_profiles.vendor_type = 'pmta'
  AND mailing_sending_profiles.organization_id = '00000000-0000-0000-0000-000000000001'
  AND (
        COALESCE(mailing_sending_profiles.pool_prefix, '') != sub.prefix
        OR COALESCE(mailing_sending_profiles.smtp_host, '') != sub.smtp_host
        OR COALESCE(mailing_sending_profiles.api_endpoint, '') != sub.api_endpoint
        OR COALESCE(mailing_sending_profiles.ip_pool, '') != sub.ip_pool
        OR COALESCE(mailing_sending_profiles.tracking_domain, '') != sub.tracking_domain
  )`},

		// Persona aliases for the May 9 2026 four-brand expansion (mirrors may08 pattern).
		{"may09_seed_brand_persona_from_names", `UPDATE mailing_sending_profiles
SET from_name = sub.persona, updated_at = NOW()
FROM (VALUES
    ('em.myrepairdiy.com',        'Bob @ My Repair DIY',          'My Repair DIY'),
    ('em.casainsure.com',         'Maria @ Casa Insure',          'Casa Insure'),
    ('em.learnpersonalloans.com', 'Linda @ Learn Personal Loans', 'Learn Personal Loans'),
    ('em.ratesbazar.com',         'Sam @ Rates Bazar',            'Rates Bazar')
) AS sub(sending_domain, persona, bare_name)
WHERE mailing_sending_profiles.sending_domain = sub.sending_domain
  AND mailing_sending_profiles.vendor_type = 'pmta'
  AND mailing_sending_profiles.organization_id = '00000000-0000-0000-0000-000000000001'
  AND COALESCE(mailing_sending_profiles.from_name, '') = sub.bare_name`},

		// Image-CDN seeds for the four new brands. Each brand now has its OWN
		// dedicated CloudFront distribution + ACM cert in us-east-1 (provisioned
		// 2026-05-09 via .scratch/may09_new_domains/provision_img_distros.py).
		// CNAMEs img.<apex> -> <distro>.cloudfront.net are live in GoDaddy.
		// Distros are S3-OAC backed (jarvis-image-cdn bucket) — same storage
		// surface as established brands; per-brand distro avoids cross-brand
		// image fingerprinting at the CDN edge.
		{"may09_seed_img_domains_new_brands", `INSERT INTO mailing_image_domains
(id, org_id, domain, verified, ssl_status, s3_bucket, cloudfront_distribution_id, cloudfront_domain, acm_cert_arn, last_verified_at, created_at, updated_at)
VALUES
    ('d0000000-0000-0000-0001-000000000020', '00000000-0000-0000-0000-000000000001', 'img.myrepairdiy.com',        true, 'active', 'jarvis-image-cdn', 'E33V9NXEKUQBCV', 'dv7yu72awfglb.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/cc285053-7ef9-4bde-8932-4688f5bd83ca', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000021', '00000000-0000-0000-0000-000000000001', 'img.casainsure.com',         true, 'active', 'jarvis-image-cdn', 'E24SJM35AP27KP', 'dbq2ggpb746yz.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/5d843dc4-c778-4c01-b24b-a98ea72b83cc', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000022', '00000000-0000-0000-0000-000000000001', 'img.learnpersonalloans.com', true, 'active', 'jarvis-image-cdn', 'E3AMI6B4PQ744B', 'd2c66sxkzm772s.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/a569f75a-7fda-4010-99dc-5495d7576743', NOW(), NOW(), NOW()),
    ('d0000000-0000-0000-0001-000000000023', '00000000-0000-0000-0000-000000000001', 'img.ratesbazar.com',         true, 'active', 'jarvis-image-cdn', 'E3V9MOBIFGXHYV', 'd1zq3wbfdoaz7a.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/86d7c095-8df0-4905-9666-f8c52914867a', NOW(), NOW(), NOW())
ON CONFLICT (id) DO NOTHING`},

		// Idempotent re-anchor for image_domains. If the prior placeholder rows
		// (which pointed at the shared org-default CloudFront) had already been
		// seeded by an earlier server boot, this UPDATE migrates them to the
		// per-brand dedicated distros. Also sets verified=true / ssl_status='active'.
		{"may09_reassert_img_domains_dedicated", `UPDATE mailing_image_domains
SET cloudfront_distribution_id = sub.distro,
    cloudfront_domain          = sub.cf_domain,
    acm_cert_arn               = sub.acm_arn,
    verified                   = true,
    ssl_status                 = 'active',
    last_verified_at           = NOW(),
    updated_at                 = NOW()
FROM (VALUES
    ('img.myrepairdiy.com',        'E33V9NXEKUQBCV', 'dv7yu72awfglb.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/cc285053-7ef9-4bde-8932-4688f5bd83ca'),
    ('img.casainsure.com',         'E24SJM35AP27KP', 'dbq2ggpb746yz.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/5d843dc4-c778-4c01-b24b-a98ea72b83cc'),
    ('img.learnpersonalloans.com', 'E3AMI6B4PQ744B', 'd2c66sxkzm772s.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/a569f75a-7fda-4010-99dc-5495d7576743'),
    ('img.ratesbazar.com',         'E3V9MOBIFGXHYV', 'd1zq3wbfdoaz7a.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/86d7c095-8df0-4905-9666-f8c52914867a')
) AS sub(domain, distro, cf_domain, acm_arn)
WHERE mailing_image_domains.domain = sub.domain
  AND (
        COALESCE(mailing_image_domains.cloudfront_distribution_id, '') != sub.distro
     OR COALESCE(mailing_image_domains.cloudfront_domain, '')          != sub.cf_domain
     OR COALESCE(mailing_image_domains.acm_cert_arn, '')               != sub.acm_arn
     OR COALESCE(mailing_image_domains.verified, false)                != true
     OR COALESCE(mailing_image_domains.ssl_status, '')                 != 'active'
  )`},

		// Seed the 4 new brands into mailing_brand_metadata. Idempotent.
		{"may09_seed_brand_metadata", `INSERT INTO mailing_brand_metadata
(brand_root, brand_code, brand_label, sending_domain, tracking_domain, image_domain, from_name, from_email, reply_email, physical_address, notes)
VALUES
    ('myrepairdiy.com',        'MR', 'My Repair DIY',          'em.myrepairdiy.com',        't.em.myrepairdiy.com',        'img.myrepairdiy.com',        'Bob @ My Repair DIY',          'hello@em.myrepairdiy.com',        'reply@em.myrepairdiy.com',        '30 N Gould St Ste R, Sheridan, WY 82801', 'May 9 2026 expansion; img CDN initially shares org-default CloudFront until dedicated provisioned'),
    ('casainsure.com',         'CI', 'Casa Insure',            'em.casainsure.com',         't.em.casainsure.com',         'img.casainsure.com',         'Maria @ Casa Insure',          'hello@em.casainsure.com',         'reply@em.casainsure.com',         '30 N Gould St Ste R, Sheridan, WY 82801', 'May 9 2026 expansion'),
    ('learnpersonalloans.com', 'LP', 'Learn Personal Loans',   'em.learnpersonalloans.com', 't.em.learnpersonalloans.com', 'img.learnpersonalloans.com', 'Linda @ Learn Personal Loans', 'hello@em.learnpersonalloans.com', 'reply@em.learnpersonalloans.com', '30 N Gould St Ste R, Sheridan, WY 82801', 'May 9 2026 expansion'),
    ('ratesbazar.com',         'RB', 'Rates Bazar',            'em.ratesbazar.com',         't.em.ratesbazar.com',         'img.ratesbazar.com',         'Sam @ Rates Bazar',            'hello@em.ratesbazar.com',         'reply@em.ratesbazar.com',         '30 N Gould St Ste R, Sheridan, WY 82801', 'May 9 2026 expansion')
ON CONFLICT (brand_root) DO NOTHING`},

		// =====================================================================
		// May 9 2026 (B): warrantyforyou.com (single-brand follow-up to may09).
		// Same surgical pattern: 3 IPs harvested from over-provisioned mh-* ISP-tier
		// pools (Server B donor: mh apple/comcast/charter), renamed to
		// mta-wf-gn[1-3], placed in wf-general-pool. PMTA config on Server B
		// already updated (DKIM key + VMTA renames + new pool + reload).
		// Tracking + img CDN dedicated CloudFront distros provisioned in
		// us-east-1 (E19T9Q3SSOV4ET tracking, E3KPT8YQNLASCR img). SES inbound
		// MX swap + SES identity + receipt-rule recipient already live.
		// =====================================================================
		{"may09b_create_wf_general_pool", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at)
SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'wf-general-pool', 'WarrantyForYou general ISP pool', 'dedicated', 'active', NOW(), NOW()
WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools p WHERE p.name = 'wf-general-pool' AND p.organization_id = '00000000-0000-0000-0000-000000000001')`},

		{"may09b_reassign_wf_harvested_ips", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            ('144.225.178.202', 'mta-wf-gn1.mail.em.warrantyforyou.com', 'wf-general-pool'),
            ('144.225.178.203', 'mta-wf-gn2.mail.em.warrantyforyou.com', 'wf-general-pool'),
            ('144.225.178.205', 'mta-wf-gn3.mail.em.warrantyforyou.com', 'wf-general-pool')
        ) AS t(ip_addr, hostname, pool_name)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses
               SET pool_id = pool_id_val,
                   hostname = rec.hostname,
                   status = 'active',
                   warmup_stage = COALESCE(NULLIF(warmup_stage, ''), 'early'),
                   updated_at = NOW()
             WHERE ip_address = rec.ip_addr::inet;
        END IF;
    END LOOP;
END $$`},

		{"may09b_seed_wf_sending_profile", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'WarrantyForYou PMTA', 'pmta', 'Warranty For You', 'hello@em.warrantyforyou.com', 'reply@em.warrantyforyou.com', 'em.warrantyforyou.com', '15.204.107.107', 587, 'http://15.204.107.107:19099', 't.em.warrantyforyou.com', 1500, 12000, 'wf-general-pool', 'wf', 'active', false, NOW(), NOW()
WHERE NOT EXISTS (
    SELECT 1 FROM mailing_sending_profiles p
    WHERE p.sending_domain = 'em.warrantyforyou.com'
      AND p.organization_id = '00000000-0000-0000-0000-000000000001'
)`},

		// Idempotent re-anchor (mirrors may09_reassert_pool_prefix).
		{"may09b_reassert_wf_pool_prefix", `UPDATE mailing_sending_profiles SET pool_prefix = 'wf', smtp_host = '15.204.107.107', api_endpoint = 'http://15.204.107.107:19099', ip_pool = 'wf-general-pool', tracking_domain = 't.em.warrantyforyou.com', updated_at = NOW()
WHERE sending_domain = 'em.warrantyforyou.com'
  AND vendor_type = 'pmta'
  AND organization_id = '00000000-0000-0000-0000-000000000001'
  AND (
        COALESCE(pool_prefix, '') != 'wf'
        OR COALESCE(smtp_host, '') != '15.204.107.107'
        OR COALESCE(api_endpoint, '') != 'http://15.204.107.107:19099'
        OR COALESCE(ip_pool, '') != 'wf-general-pool'
        OR COALESCE(tracking_domain, '') != 't.em.warrantyforyou.com'
  )`},

		// Persona alias for WF.
		{"may09b_seed_wf_persona_from_name", `UPDATE mailing_sending_profiles
SET from_name = 'Greg @ Warranty For You', updated_at = NOW()
WHERE sending_domain = 'em.warrantyforyou.com'
  AND vendor_type = 'pmta'
  AND organization_id = '00000000-0000-0000-0000-000000000001'
  AND COALESCE(from_name, '') = 'Warranty For You'`},

		// Image-CDN seed for WF — dedicated CloudFront distro from day one.
		{"may09b_seed_wf_img_domain", `INSERT INTO mailing_image_domains
(id, org_id, domain, verified, ssl_status, s3_bucket, cloudfront_distribution_id, cloudfront_domain, acm_cert_arn, last_verified_at, created_at, updated_at)
VALUES
    ('d0000000-0000-0000-0001-000000000024', '00000000-0000-0000-0000-000000000001', 'img.warrantyforyou.com', true, 'active', 'jarvis-image-cdn', 'E3KPT8YQNLASCR', 'dfe0srr0ch1xd.cloudfront.net', 'arn:aws:acm:us-east-1:146361001621:certificate/9b7aca67-9866-480e-82b5-c0ec40a3ca33', NOW(), NOW(), NOW())
ON CONFLICT (id) DO NOTHING`},

		// Idempotent re-anchor for WF image_domains row.
		{"may09b_reassert_wf_img_domain_dedicated", `UPDATE mailing_image_domains
SET cloudfront_distribution_id = 'E3KPT8YQNLASCR',
    cloudfront_domain          = 'dfe0srr0ch1xd.cloudfront.net',
    acm_cert_arn               = 'arn:aws:acm:us-east-1:146361001621:certificate/9b7aca67-9866-480e-82b5-c0ec40a3ca33',
    verified                   = true,
    ssl_status                 = 'active',
    last_verified_at           = NOW(),
    updated_at                 = NOW()
WHERE domain = 'img.warrantyforyou.com'
  AND (
        COALESCE(cloudfront_distribution_id, '') != 'E3KPT8YQNLASCR'
     OR COALESCE(cloudfront_domain, '')          != 'dfe0srr0ch1xd.cloudfront.net'
     OR COALESCE(acm_cert_arn, '')               != 'arn:aws:acm:us-east-1:146361001621:certificate/9b7aca67-9866-480e-82b5-c0ec40a3ca33'
     OR COALESCE(verified, false)                != true
     OR COALESCE(ssl_status, '')                 != 'active'
  )`},

		// Seed WF brand metadata.
		{"may09b_seed_wf_brand_metadata", `INSERT INTO mailing_brand_metadata
(brand_root, brand_code, brand_label, sending_domain, tracking_domain, image_domain, from_name, from_email, reply_email, physical_address, notes)
VALUES
    ('warrantyforyou.com', 'WF', 'Warranty For You', 'em.warrantyforyou.com', 't.em.warrantyforyou.com', 'img.warrantyforyou.com', 'Greg @ Warranty For You', 'hello@em.warrantyforyou.com', 'reply@em.warrantyforyou.com', '30 N Gould St Ste R, Sheridan, WY 82801', 'May 9 2026 (B) follow-up expansion to may09 batch')
ON CONFLICT (brand_root) DO NOTHING`},

		// Phase 9: ThrottleAgent state persistence
		{"create_throttle_agent_state", `CREATE TABLE IF NOT EXISTS mailing_engine_throttle_agent_state (
			isp TEXT PRIMARY KEY,
			current_rate_adj DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			original_rate INT NOT NULL DEFAULT 0,
			last_stable_rate DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			backoff_count INT NOT NULL DEFAULT 0,
			in_recovery BOOLEAN NOT NULL DEFAULT FALSE,
			recovery_started TIMESTAMPTZ,
			last_backoff_at TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"add_escalation_cols_throttle_state", `DO $$ BEGIN
			ALTER TABLE mailing_engine_throttle_agent_state ADD COLUMN IF NOT EXISTS escalation_adj DOUBLE PRECISION NOT NULL DEFAULT 1.0;
			ALTER TABLE mailing_engine_throttle_agent_state ADD COLUMN IF NOT EXISTS escalation_cooldown_until TIMESTAMPTZ;
			ALTER TABLE mailing_engine_throttle_agent_state ADD COLUMN IF NOT EXISTS last_escalation_at TIMESTAMPTZ;
		END $$`},
		{"add_max_escalation_multiplier_isp_config", `ALTER TABLE mailing_engine_isp_config ADD COLUMN IF NOT EXISTS max_escalation_multiplier DOUBLE PRECISION NOT NULL DEFAULT 1.5`},
		{"create_ip_throttle_state", `CREATE TABLE IF NOT EXISTS mailing_isp_ip_throttle_state (
			isp TEXT NOT NULL,
			ip_hostname TEXT NOT NULL,
			msgs_per_hour DOUBLE PRECISION NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (isp, ip_hostname)
		)`},
		{"create_data_pipeline_runs", `CREATE TABLE IF NOT EXISTS data_pipeline_runs (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at TIMESTAMPTZ,
			status TEXT NOT NULL DEFAULT 'running',
			files_processed INT DEFAULT 0,
			emails_total INT DEFAULT 0,
			emails_verified INT DEFAULT 0,
			emails_suppressed INT DEFAULT 0,
			emails_deduped INT DEFAULT 0,
			details JSONB DEFAULT '{}',
			error_message TEXT,
			notification_sent BOOLEAN DEFAULT false
		)`},
		{"create_data_pipeline_files", `CREATE TABLE IF NOT EXISTS data_pipeline_files (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			s3_key TEXT NOT NULL UNIQUE,
			isp_folder TEXT NOT NULL,
			isp_normalized TEXT NOT NULL,
			row_count INT,
			status TEXT NOT NULL DEFAULT 'available',
			run_id UUID,
			sending_domain TEXT,
			target_list_id UUID,
			verified_count INT DEFAULT 0,
			suppressed_count INT DEFAULT 0,
			deduped_count INT DEFAULT 0,
			processed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`},
		{"create_data_pipeline_domain_lists", `CREATE TABLE IF NOT EXISTS data_pipeline_domain_lists (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			sending_domain TEXT NOT NULL,
			isp TEXT NOT NULL,
			list_id UUID NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(sending_domain, isp)
		)`},
		{"seed_pipeline_domain_lists", `
			INSERT INTO data_pipeline_domain_lists (sending_domain, isp, list_id)
			SELECT v.sending_domain, v.isp, l.id
			FROM (VALUES
				('em.discountblog.com', 'gmail', 'Google - Active - 1 03052026'),
				('em.discountblog.com', 'yahoo', 'Yahoo - Clickers - 0 03052026'),
				('em.discountblog.com', 'microsoft', 'Microsoft - Active - 1 03052026'),
				('em.discountblog.com', 'apple', 'Apple - Active - 1 03052026'),
				('em.discountblog.com', 'comcast', 'Comcast - Active - 1 03052026'),
				('em.discountblog.com', 'charter', 'Charter - Active - 2 03092026'),
				('em.discountblog.com', 'att', 'ATT - Active -1 03052026'),
				('em.discountblog.com', 'cox', 'Cox - Active - 1 03052026')
			) AS v(sending_domain, isp, list_name)
			JOIN mailing_lists l ON l.name = v.list_name
			WHERE NOT EXISTS (
				SELECT 1 FROM data_pipeline_domain_lists dl
				WHERE dl.sending_domain = v.sending_domain AND dl.isp = v.isp
			)
		`},
		{"seed_pipeline_domain_lists_mh_ht", `
			INSERT INTO data_pipeline_domain_lists (sending_domain, isp, list_id)
			SELECT v.sending_domain, v.isp, l.id
			FROM (VALUES
				('em.ownmyhealth.net', 'yahoo', 'MH Yahoo'),
				('em.ownmyhealth.net', 'gmail', 'MH Gmail'),
				('em.ownmyhealth.net', 'microsoft', 'MH Msft'),
				('em.ownmyhealth.net', 'apple', 'MH Apple'),
				('em.ownmyhealth.net', 'comcast', 'MH Comcast'),
				('em.ownmyhealth.net', 'charter', 'MH Charter'),
				('em.ownmyhealth.net', 'att', 'MH Att'),
				('em.ownmyhealth.net', 'cox', 'MH Cox'),
				('em.ownmyhealth.net', 'other', 'MH Other'),
				('em.historythinking.com', 'yahoo', 'HT Yahoo'),
				('em.historythinking.com', 'gmail', 'HT Gmail'),
				('em.historythinking.com', 'microsoft', 'HT Msft'),
				('em.historythinking.com', 'apple', 'HT Apple'),
				('em.historythinking.com', 'comcast', 'HT Comcast'),
				('em.historythinking.com', 'charter', 'HT Charter'),
				('em.historythinking.com', 'att', 'HT Att'),
				('em.historythinking.com', 'cox', 'HT Cox'),
				('em.historythinking.com', 'other', 'HT Other'),
				('em.quizfiesta.com', 'yahoo', 'QF Yahoo - Active - 2 03092026'),
				('em.quizfiesta.com', 'gmail', 'QF Google - Active - 2 03092026'),
				('em.quizfiesta.com', 'microsoft', 'QF Microsoft - Active - 2 03092026'),
				('em.quizfiesta.com', 'apple', 'QF Apple - Active - 2 03092026'),
				('em.quizfiesta.com', 'comcast', 'QF Comcast - Active - 2 03092026'),
				('em.quizfiesta.com', 'charter', 'QF Charter - Active - 2 03092026'),
				('em.quizfiesta.com', 'cox', 'QF Cox - Active - 2 03092026')
			) AS v(sending_domain, isp, list_name)
			JOIN mailing_lists l ON l.name = v.list_name
			WHERE NOT EXISTS (
				SELECT 1 FROM data_pipeline_domain_lists dl
				WHERE dl.sending_domain = v.sending_domain AND dl.isp = v.isp
			)
		`},
		{"mark_used_charter_s3_files", `
			INSERT INTO data_pipeline_files (id, s3_key, isp_folder, isp_normalized, status, row_count, processed_at, created_at)
			VALUES
				(gen_random_uuid(), 'warmup-data/2026/Charter/Charter_001.csv', 'Charter', 'charter', 'processed', 10000, NOW(), NOW()),
				(gen_random_uuid(), 'warmup-data/2026/Charter/Charter_002.csv', 'Charter', 'charter', 'processed', 10000, NOW(), NOW())
			ON CONFLICT (s3_key) DO UPDATE SET status = 'processed', processed_at = NOW()
		`},
		{"update_isp_rates_8h_window_v2", `
			UPDATE mailing_engine_isp_config SET max_msg_rate = 2659 WHERE isp = 'microsoft' AND max_msg_rate < 2659;
			UPDATE mailing_engine_isp_config SET max_msg_rate = 1980 WHERE isp = 'yahoo'     AND max_msg_rate < 1980;
			UPDATE mailing_engine_isp_config SET max_msg_rate = 1610 WHERE isp = 'gmail'     AND max_msg_rate < 1610;
			UPDATE mailing_engine_isp_config SET max_msg_rate = 1040 WHERE isp = 'apple'     AND max_msg_rate < 1040;
		`},
		{"set_isp_rates_6h_spread_v1", `
			UPDATE mailing_engine_isp_config SET max_msg_rate = 870  WHERE isp = 'microsoft';
			UPDATE mailing_engine_isp_config SET max_msg_rate = 650  WHERE isp = 'yahoo';
			UPDATE mailing_engine_isp_config SET max_msg_rate = 525  WHERE isp = 'gmail';
			UPDATE mailing_engine_isp_config SET max_msg_rate = 340  WHERE isp = 'apple';
		`},
		{"fix_all_isp_pool_ips_warmup_v2", `
			UPDATE mailing_ip_addresses
			SET status = 'warmup',
			    warmup_daily_limit = GREATEST(COALESCE(warmup_daily_limit, 0), 10000),
			    warmup_stage = COALESCE(NULLIF(warmup_stage, ''), 'warming'),
			    updated_at = NOW()
			WHERE pool_id IN (
				SELECT id FROM mailing_ip_pools
				WHERE (name LIKE 'ht-%' OR name LIKE 'mh-%' OR name LIKE 'db-%' OR name LIKE 'qf-%')
				AND status = 'active'
			)
			AND status NOT IN ('active', 'warmup')
		`},

		// List dashboard performance indexes
		{"idx_subscribers_list_status", `CREATE INDEX IF NOT EXISTS idx_subscribers_list_status ON mailing_subscribers (list_id, status)`},
		{"idx_lists_visible_created", `CREATE INDEX IF NOT EXISTS idx_lists_visible_created ON mailing_lists (created_at DESC) WHERE COALESCE(is_visible, true) = true`},
		{"idx_subscribers_audience_scan", `CREATE INDEX IF NOT EXISTS idx_subscribers_audience_scan ON mailing_subscribers (list_id, status, created_at, id) WHERE status IN ('active','confirmed')`},

		{"create_audience_metrics_table", `
			CREATE TABLE IF NOT EXISTS mailing_audience_metrics (
				id                   INTEGER PRIMARY KEY DEFAULT 1,
				active_audience_60d  INTEGER NOT NULL DEFAULT 0,
				global_churn_pct     NUMERIC(6,3) NOT NULL DEFAULT 0,
				global_intro_pct     NUMERIC(6,3) NOT NULL DEFAULT 0,
				computed_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				CHECK (id = 1)
			)`},
		{"seed_audience_metrics_row", `
			INSERT INTO mailing_audience_metrics (id) VALUES (1) ON CONFLICT DO NOTHING`},

		// Per-cell audience cadence snapshot (2026-05-05). Materialised by
		// AdvancedMailingService.RefreshAudienceCadenceSnapshot every 15min.
		// Backs the /api/mailing/analytics/audience-cadence-by-cell endpoint
		// and the Audience Cadence frontend tab. See SCHEDULING_INTEGRITY_PLAYBOOK §15.
		{"create_audience_cadence_snapshot_table", `
			CREATE TABLE IF NOT EXISTS mailing_audience_cadence_snapshot (
				brand                   TEXT NOT NULL,
				sending_domain          TEXT NOT NULL,
				isp                     TEXT NOT NULL,
				touched_total           INTEGER NOT NULL DEFAULT 0,
				welcome_eligible_1_3    INTEGER NOT NULL DEFAULT 0,
				welcome_eligible_4_7    INTEGER NOT NULL DEFAULT 0,
				saturated               INTEGER NOT NULL DEFAULT 0,
				opener_pool             INTEGER NOT NULL DEFAULT 0,
				cold_never_opened       INTEGER NOT NULL DEFAULT 0,
				unsubscribed_30d        INTEGER NOT NULL DEFAULT 0,
				hard_bounced_30d        INTEGER NOT NULL DEFAULT 0,
				complained_30d          INTEGER NOT NULL DEFAULT 0,
				daily_unique_burn_7d    INTEGER NOT NULL DEFAULT 0,
				refreshed_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY (sending_domain, isp)
			)`},
		{"idx_audience_cadence_snapshot_brand", `CREATE INDEX IF NOT EXISTS idx_audience_cadence_snapshot_brand ON mailing_audience_cadence_snapshot (brand)`},
		{"idx_audience_cadence_snapshot_refreshed", `CREATE INDEX IF NOT EXISTS idx_audience_cadence_snapshot_refreshed ON mailing_audience_cadence_snapshot (refreshed_at DESC)`},

		// =====================================================================
		// 2026-05-05 v2: Global per-ISP audience cadence snapshot.
		// =====================================================================
		// Replaces the per-(brand × ISP) snapshot above. Operator clarified that
		// the welcome pool is a SINGLE shared bucket across all four brands —
		// the deploy scripts hand the same six inclusion segments to every
		// brand and the planner shuffles by ISP across brands as it picks
		// recipients. The per-cell snapshot above double-counted subscribers
		// touched by multiple brands (one row per subscriber × sending_domain
		// in mailing_subscriber_domain_state). Real exhaustion question is
		// global per ISP. New table groups directly on subscriber → ISP and
		// pulls prior_sends from mailing_message_log (same source the planner
		// uses to enforce welcomeSaturationThreshold). Drop the old table to
		// prevent confusion — the new one is the only source of truth.
		{"drop_legacy_audience_cadence_snapshot", `DROP TABLE IF EXISTS mailing_audience_cadence_snapshot`},
		{"create_audience_cadence_isp_snapshot", `
			CREATE TABLE IF NOT EXISTS mailing_audience_cadence_isp_snapshot (
				isp                     TEXT PRIMARY KEY,
				pool_total              INTEGER NOT NULL DEFAULT 0,
				exempt_opened           INTEGER NOT NULL DEFAULT 0,
				retired_saturated       INTEGER NOT NULL DEFAULT 0,
				never_mailed            INTEGER NOT NULL DEFAULT 0,
				one_send_away           INTEGER NOT NULL DEFAULT 0,
				two_to_three_away       INTEGER NOT NULL DEFAULT 0,
				four_to_five_away       INTEGER NOT NULL DEFAULT 0,
				six_to_eight_away       INTEGER NOT NULL DEFAULT 0,
				daily_unique_burn_7d    INTEGER NOT NULL DEFAULT 0,
				unsubscribed_30d        INTEGER NOT NULL DEFAULT 0,
				hard_bounced_30d        INTEGER NOT NULL DEFAULT 0,
				complained_30d          INTEGER NOT NULL DEFAULT 0,
				refreshed_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`},
		{"idx_audience_cadence_isp_snapshot_refreshed", `CREATE INDEX IF NOT EXISTS idx_audience_cadence_isp_snapshot_refreshed ON mailing_audience_cadence_isp_snapshot (refreshed_at DESC)`},

		// =====================================================================
		// Phase 12: Force-correct OVH→Yahoo and IPXO→non-Yahoo pool assignments.
		// Phase 10 migrations didn't take effect because of ordering issues.
		// =====================================================================
		{"phase12_force_ovh_to_db_yahoo", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.22.177'::inet, '15.204.22.178'::inet, '15.204.22.179'::inet, '15.204.22.180'::inet,
			                     '15.204.22.181'::inet, '15.204.22.182'::inet, '15.204.22.183'::inet, '15.204.22.184'::inet)`},
		{"phase12_force_ovh_to_qf_yahoo", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.22.185'::inet, '15.204.22.186'::inet, '15.204.22.187'::inet, '15.204.22.188'::inet,
			                     '15.204.22.189'::inet, '15.204.22.190'::inet, '15.204.22.191'::inet)`},
		{"phase12_force_ovh_to_ht_yahoo", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.38.160'::inet, '15.204.38.161'::inet, '15.204.38.162'::inet, '15.204.38.163'::inet,
			                     '15.204.38.164'::inet, '15.204.38.165'::inet, '15.204.38.166'::inet, '15.204.38.167'::inet)`},
		{"phase12_force_ovh_to_mh_yahoo", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.38.168'::inet, '15.204.38.169'::inet, '15.204.38.170'::inet, '15.204.38.171'::inet,
			                     '15.204.38.172'::inet, '15.204.38.173'::inet, '15.204.38.174'::inet, '15.204.38.175'::inet)`},
		{"phase12_ipxo_out_of_yahoo", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            ('144.225.178.7',   'db-gmail-pool'),
            ('144.225.178.8',   'db-msft-pool'),
            ('144.225.178.9',   'db-apple-pool'),
            ('144.225.178.10',  'db-comcast-pool'),
            ('144.225.178.11',  'db-general-pool'),
            ('144.225.178.12',  'db-charter-pool'),
            -- .13 reserved for db-yahoo-pool (Phase 14)
            ('144.225.178.71',  'qf-gmail-pool'),
            ('144.225.178.72',  'qf-msft-pool'),
            ('144.225.178.73',  'qf-apple-pool'),
            ('144.225.178.74',  'qf-comcast-pool'),
            ('144.225.178.75',  'qf-general-pool'),
            ('144.225.178.76',  'qf-charter-pool'),
            -- .77 reserved for qf-yahoo-pool (Phase 14)
            ('144.225.178.136', 'ht-gmail-pool'),
            ('144.225.178.137', 'ht-msft-pool'),
            ('144.225.178.138', 'ht-apple-pool'),
            ('144.225.178.139', 'ht-comcast-pool'),
            ('144.225.178.140', 'ht-general-pool'),
            ('144.225.178.141', 'ht-charter-pool'),
            ('144.225.178.142', 'ht-general-pool'),
            -- .143 reserved for ht-yahoo-pool (Phase 14)
            ('144.225.178.200', 'mh-gmail-pool'),
            ('144.225.178.201', 'mh-msft-pool'),
            ('144.225.178.202', 'mh-apple-pool'),
            ('144.225.178.203', 'mh-comcast-pool'),
            ('144.225.178.204', 'mh-general-pool'),
            ('144.225.178.205', 'mh-charter-pool'),
            ('144.225.178.206', 'mh-general-pool')
            -- .207 reserved for mh-yahoo-pool (Phase 14)
        ) AS t(ip_addr, pool_name)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
            WHERE ip_address = rec.ip_addr::inet;
        END IF;
    END LOOP;
END $$`},

		// Phase 13: Pause all OVH IPs — all production sending now uses IPXO only.
		{"phase13_pause_all_ovh_ips", `UPDATE mailing_ip_addresses
			SET status = 'paused', updated_at = NOW()
			WHERE ip_address <<= '15.204.22.176/28'::inet
			  AND status NOT IN ('paused', 'cold')`},
		{"phase13_pause_all_ovh_ips_block2", `UPDATE mailing_ip_addresses
			SET status = 'paused', updated_at = NOW()
			WHERE ip_address <<= '15.204.38.160/28'::inet
			  AND status NOT IN ('paused', 'cold')`},

		// Phase 14: Assign 1 IPXO IP per brand to each Yahoo pool.
		// Phase 12 moved all IPXO Yahoo IPs to non-Yahoo pools; Phase 13 paused the
		// OVH IPs that replaced them. Net result: Yahoo pools had 0 active IPs.
		// This restores 1 dedicated Yahoo IP per brand from the general pool.
		{"phase14_yahoo_ip_db", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    updated_at = NOW()
			WHERE ip_address = '144.225.178.13'::inet
			  AND pool_id != (SELECT id FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"phase14_yahoo_ip_qf", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    updated_at = NOW()
			WHERE ip_address = '144.225.178.77'::inet
			  AND pool_id != (SELECT id FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"phase14_yahoo_ip_ht", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    updated_at = NOW()
			WHERE ip_address = '144.225.178.143'::inet
			  AND pool_id != (SELECT id FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"phase14_yahoo_ip_mh", `UPDATE mailing_ip_addresses
			SET pool_id = (SELECT id FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'),
			    updated_at = NOW()
			WHERE ip_address = '144.225.178.207'::inet
			  AND pool_id != (SELECT id FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},

		// Phase 15: Raise Yahoo pool IP warmup limits to 5000/day for seed campaigns.
		{"phase15_yahoo_warmup_db", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000, updated_at = NOW() WHERE ip_address = '144.225.178.13'::inet AND warmup_daily_limit < 5000`},
		{"phase15_yahoo_warmup_qf", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000, updated_at = NOW() WHERE ip_address = '144.225.178.77'::inet AND warmup_daily_limit < 5000`},
		{"phase15_yahoo_warmup_ht", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000, updated_at = NOW() WHERE ip_address = '144.225.178.143'::inet AND warmup_daily_limit < 5000`},
		{"phase15_yahoo_warmup_mh", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000, updated_at = NOW() WHERE ip_address = '144.225.178.207'::inet AND warmup_daily_limit < 5000`},

		// Phase 16: Fix welcome campaigns missing ISP quotas.
		// Welcome campaigns created dynamically lack the isp_quotas key in esp_quotas,
		// making them invisible to the ISP dispatch loop (send_worker.go:550-556).
		// After 10 min idle, the orphan check marks them 'failed'.
		// Fix: add proper ISP quotas (8 ISPs, no Gmail/Yahoo — 3rd party warming),
		// reset failed → scheduled for immediate pickup.

		// Apr 10 (Day 3, factor 1.331): all failed Welcome campaigns → reset & send now
		{"phase16_welcome_quotas_apr10", `UPDATE mailing_campaigns
			SET esp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":2660},{"isp":"Apple","volume":2200},{"isp":"Comcast","volume":870},{"isp":"Charter","volume":700},{"isp":"ATT","volume":470},{"isp":"AOL","volume":470},{"isp":"Cox","volume":470},{"isp":"Other","volume":700}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    isp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":2660},{"isp":"Apple","volume":2200},{"isp":"Comcast","volume":870},{"isp":"Charter","volume":700},{"isp":"ATT","volume":470},{"isp":"AOL","volume":470},{"isp":"Cox","volume":470},{"isp":"Other","volume":700}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    status = 'scheduled',
			    scheduled_at = NOW(),
			    started_at = NULL,
			    completed_at = NULL,
			    updated_at = NOW()
			WHERE name ILIKE '%Welcome%'
			  AND status = 'failed'
			  AND (esp_quotas IS NULL OR NOT (esp_quotas ? 'isp_quotas'))`},

		// Phase 16b: Re-schedule welcome campaigns stuck with scheduled_at in the past.
		// markCampaignsAsPreparing requires scheduled_at > NOW(). Phase 16 set NOW()
		// which is already past by the time the scheduler polls.
		{"phase16b_welcome_reschedule", `UPDATE mailing_campaigns
			SET scheduled_at = NOW() + INTERVAL '2 minutes',
			    updated_at = NOW()
			WHERE name ILIKE '%Welcome%'
			  AND status = 'scheduled'
			  AND scheduled_at <= NOW()
			  AND esp_quotas ? 'isp_quotas'`},

		// Apr 11 (Day 4, factor 1.464): scheduled Welcome campaigns for Apr 11
		{"phase16_welcome_quotas_apr11", `UPDATE mailing_campaigns
			SET esp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":2930},{"isp":"Apple","volume":2420},{"isp":"Comcast","volume":950},{"isp":"Charter","volume":770},{"isp":"ATT","volume":510},{"isp":"AOL","volume":510},{"isp":"Cox","volume":510},{"isp":"Other","volume":770}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    isp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":2930},{"isp":"Apple","volume":2420},{"isp":"Comcast","volume":950},{"isp":"Charter","volume":770},{"isp":"ATT","volume":510},{"isp":"AOL","volume":510},{"isp":"Cox","volume":510},{"isp":"Other","volume":770}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    updated_at = NOW()
			WHERE name ILIKE '%Welcome%'
			  AND status IN ('scheduled', 'failed')
			  AND scheduled_at::date = '2026-04-11'
			  AND (esp_quotas IS NULL OR NOT (esp_quotas ? 'isp_quotas'))`},

		// Apr 12 (Day 5, factor 1.611): scheduled Welcome campaigns for Apr 12
		{"phase16_welcome_quotas_apr12", `UPDATE mailing_campaigns
			SET esp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":3220},{"isp":"Apple","volume":2660},{"isp":"Comcast","volume":1050},{"isp":"Charter","volume":850},{"isp":"ATT","volume":560},{"isp":"AOL","volume":560},{"isp":"Cox","volume":560},{"isp":"Other","volume":850}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    isp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":3220},{"isp":"Apple","volume":2660},{"isp":"Comcast","volume":1050},{"isp":"Charter","volume":850},{"isp":"ATT","volume":560},{"isp":"AOL","volume":560},{"isp":"Cox","volume":560},{"isp":"Other","volume":850}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    updated_at = NOW()
			WHERE name ILIKE '%Welcome%'
			  AND status IN ('scheduled', 'failed')
			  AND scheduled_at::date = '2026-04-12'
			  AND (esp_quotas IS NULL OR NOT (esp_quotas ? 'isp_quotas'))`},

		// Apr 13 (Day 6, factor 1.772): scheduled Welcome campaigns for Apr 13
		{"phase16_welcome_quotas_apr13", `UPDATE mailing_campaigns
			SET esp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":3540},{"isp":"Apple","volume":2920},{"isp":"Comcast","volume":1150},{"isp":"Charter","volume":930},{"isp":"ATT","volume":620},{"isp":"AOL","volume":620},{"isp":"Cox","volume":620},{"isp":"Other","volume":930}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    isp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":3540},{"isp":"Apple","volume":2920},{"isp":"Comcast","volume":1150},{"isp":"Charter","volume":930},{"isp":"ATT","volume":620},{"isp":"AOL","volume":620},{"isp":"Cox","volume":620},{"isp":"Other","volume":930}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    updated_at = NOW()
			WHERE name ILIKE '%Welcome%'
			  AND status IN ('scheduled', 'failed')
			  AND scheduled_at::date = '2026-04-13'
			  AND (esp_quotas IS NULL OR NOT (esp_quotas ? 'isp_quotas'))`},

		// Apr 14 (Day 7, factor 1.949): scheduled Welcome campaigns for Apr 14
		{"phase16_welcome_quotas_apr14", `UPDATE mailing_campaigns
			SET esp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":3900},{"isp":"Apple","volume":3220},{"isp":"Comcast","volume":1270},{"isp":"Charter","volume":1020},{"isp":"ATT","volume":680},{"isp":"AOL","volume":680},{"isp":"Cox","volume":680},{"isp":"Other","volume":1020}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    isp_quotas = '{"isp_quotas":[{"isp":"Microsoft","volume":3900},{"isp":"Apple","volume":3220},{"isp":"Comcast","volume":1270},{"isp":"Charter","volume":1020},{"isp":"ATT","volume":680},{"isp":"AOL","volume":680},{"isp":"Cox","volume":680},{"isp":"Other","volume":1020}],"target_isps":["microsoft","apple","comcast","charter","att","aol","cox","other"],"execution_mode":"wave"}'::jsonb,
			    updated_at = NOW()
			WHERE name ILIKE '%Welcome%'
			  AND status IN ('scheduled', 'failed')
			  AND scheduled_at::date = '2026-04-14'
			  AND (esp_quotas IS NULL OR NOT (esp_quotas ? 'isp_quotas'))`},

		// === GHOST VISITOR TRACKING INFRASTRUCTURE ===
		{"create_mailing_subscriber_tags", `CREATE TABLE IF NOT EXISTS mailing_subscriber_tags (
			subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
			tag TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (subscriber_id, tag)
		)`},
		{"idx_subscriber_tags_tag", `CREATE INDEX IF NOT EXISTS idx_subscriber_tags_tag ON mailing_subscriber_tags (tag)`},

		{"idx_subscriber_events_recon", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_recon ON subscriber_events (subscriber_id, source, event_at) WHERE source IN ('site','site_beacon')`},
		{"idx_tracking_events_recon", `CREATE INDEX IF NOT EXISTS idx_tracking_events_recon ON mailing_tracking_events (subscriber_id, event_type, event_at)`},

		{"seed_ghost_visitor_segment_row", `
			INSERT INTO mailing_segments (
				id, organization_id, name, description, segment_type, conditions,
				status, subscriber_count, created_at, updated_at
			)
			SELECT gen_random_uuid(), id, 'Ghost Visitors (System)',
				'Subscribers with confirmed site visits but zero ISP-reported email clicks in the last 30 days. Strong signal of ISP metric suppression — prime re-engagement candidates.',
				'system', '[]'::jsonb, 'active', 0, NOW(), NOW()
			FROM organizations
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_segments WHERE name = 'Ghost Visitors (System)' AND organization_id = organizations.id
			)
		`},
		{"seed_ghost_visitor_segment_query", `
			INSERT INTO mailing_system_segments (segment_id, system_query)
			SELECT ms.id,
				'SELECT COUNT(DISTINCT s.id) FROM mailing_subscribers s WHERE s.organization_id = $1 AND s.status = ''confirmed'' AND EXISTS (SELECT 1 FROM subscriber_events se WHERE se.subscriber_id = s.id AND se.source IN (''site'',''site_beacon'') AND se.event_type = ''page_view'' AND se.event_at > NOW() - INTERVAL ''30 days'') AND NOT EXISTS (SELECT 1 FROM mailing_tracking_events te WHERE te.subscriber_id = s.id AND te.event_type = ''clicked'' AND te.event_at > NOW() - INTERVAL ''45 days'')'
			FROM mailing_segments ms
			WHERE ms.name = 'Ghost Visitors (System)'
				AND NOT EXISTS (SELECT 1 FROM mailing_system_segments WHERE segment_id = ms.id)
		`},

		// Deliverability remediation: add revision numbering and compliance metadata to mailing_templates
		{"add_template_version_col", `ALTER TABLE mailing_templates ADD COLUMN IF NOT EXISTS version INTEGER DEFAULT 1`},
		{"add_template_compliance_status_col", `ALTER TABLE mailing_templates ADD COLUMN IF NOT EXISTS compliance_status TEXT DEFAULT 'unchecked'`},
		{"add_template_compliance_warnings_col", `ALTER TABLE mailing_templates ADD COLUMN IF NOT EXISTS compliance_warnings JSONB DEFAULT '[]'`},

		// Brand-aligned tracking domains: replace projectjarvis.io with per-brand subdomains
		{"tracking_domain_discountblog", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.discountblog.com', updated_at = NOW() WHERE sending_domain = 'em.discountblog.com' AND tracking_domain = 'projectjarvis.io'`},
		{"tracking_domain_historythinking", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.historythinking.com', updated_at = NOW() WHERE sending_domain = 'em.historythinking.com' AND tracking_domain = 'projectjarvis.io'`},
		{"tracking_domain_myownhealth", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.myownhealth.net', updated_at = NOW() WHERE sending_domain = 'em.myownhealth.net' AND tracking_domain = 'projectjarvis.io'`},
		{"tracking_domain_quizfiesta", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.quizfiesta.com', updated_at = NOW() WHERE sending_domain = 'em.quizfiesta.com' AND tracking_domain = 'projectjarvis.io'`},

		// Hardened: enforce correct tracking_domain for ALL profiles regardless of current value
		{"tracking_domain_enforce_db", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.discountblog.com', updated_at = NOW() WHERE sending_domain = 'em.discountblog.com' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 'trk.em.discountblog.com')`},
		{"tracking_domain_enforce_qf", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.quizfiesta.com', updated_at = NOW() WHERE sending_domain = 'em.quizfiesta.com' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 'trk.em.quizfiesta.com')`},
		{"tracking_domain_enforce_ht", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.historythinking.com', updated_at = NOW() WHERE sending_domain = 'em.historythinking.com' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 'trk.em.historythinking.com')`},
		{"tracking_domain_enforce_mh", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.em.myownhealth.net', updated_at = NOW() WHERE sending_domain = 'em.myownhealth.net' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 'trk.em.myownhealth.net')`},
		{"tracking_domain_enforce_m_db", `UPDATE mailing_sending_profiles SET tracking_domain = 'trk.m.discountblog.com', updated_at = NOW() WHERE sending_domain = 'm.discountblog.com' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 'trk.m.discountblog.com')`},

		// Migrate DB/QF tracking domains from trk.em.* (unknown CloudFront account)
		// to t.em.* (jamesventure CloudFront account). Runs after old enforce entries.
		{"tracking_domain_migrate_db_v2", `UPDATE mailing_sending_profiles SET tracking_domain = 't.em.discountblog.com', updated_at = NOW() WHERE sending_domain = 'em.discountblog.com' AND tracking_domain != 't.em.discountblog.com'`},
		{"tracking_domain_migrate_qf_v2", `UPDATE mailing_sending_profiles SET tracking_domain = 't.em.quizfiesta.com', updated_at = NOW() WHERE sending_domain = 'em.quizfiesta.com' AND tracking_domain != 't.em.quizfiesta.com'`},
		{"tracking_domain_migrate_m_db_v2", `UPDATE mailing_sending_profiles SET tracking_domain = 't.m.discountblog.com', updated_at = NOW() WHERE sending_domain = 'm.discountblog.com' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 't.m.discountblog.com')`},
		{"tracking_domain_migrate_m_qf_v2", `UPDATE mailing_sending_profiles SET tracking_domain = 't.m.quizfiesta.com', updated_at = NOW() WHERE sending_domain = 'm.quizfiesta.com' AND (tracking_domain IS NULL OR tracking_domain = '' OR tracking_domain != 't.m.quizfiesta.com')`},

		// m.* SES relay profiles route through the same PMTA server and
		// ISP pool infrastructure as their em.* counterparts. These must
		// be in runStartupMigrations (not runAdminMigrations) because
		// DB_ADMIN_URL is not set in production ECS.
		// SES relay: Create the ses-relay-pool and virtual IP entries so the
		// send worker can resolve VMTA "ses-relay" for m.* profiles.
		// The actual relay routing happens in PMTA config (smtp-hosts → SES).
		{"ses_relay_pool_create_v2", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_a_id UUID;
    pool_b_id UUID;
BEGIN
    -- Create ses-relay pool for Server A (DB/QF brands)
    INSERT INTO mailing_ip_pools (id, organization_id, name, status, created_at, updated_at)
    SELECT gen_random_uuid(), org_id, 'ses-relay-a', 'active', NOW(), NOW()
    WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ses-relay-a' AND organization_id = org_id);
    SELECT id INTO pool_a_id FROM mailing_ip_pools WHERE name = 'ses-relay-a' AND organization_id = org_id;

    -- Create ses-relay pool for Server B (HT/MH brands)
    INSERT INTO mailing_ip_pools (id, organization_id, name, status, created_at, updated_at)
    SELECT gen_random_uuid(), org_id, 'ses-relay-b', 'active', NOW(), NOW()
    WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ses-relay-b' AND organization_id = org_id);
    SELECT id INTO pool_b_id FROM mailing_ip_pools WHERE name = 'ses-relay-b' AND organization_id = org_id;

    -- Virtual IP for Server A relay VMTA (hostname drives VMTA selection)
    INSERT INTO mailing_ip_addresses (id, organization_id, pool_id, ip_address, hostname, status,
        warmup_daily_limit, warmup_stage, warmup_day, hosting_provider, acquisition_type,
        rdns_verified, reputation_score, created_at, updated_at)
    SELECT gen_random_uuid(), org_id, pool_a_id, '15.204.101.125'::inet,
           'ses-relay.mail.projectjarvis.io', 'active',
           50000, 'mature', 999, 'OVH', 'purchased', true, 100.0, NOW(), NOW()
    WHERE pool_a_id IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM mailing_ip_addresses WHERE hostname = 'ses-relay.mail.projectjarvis.io' AND pool_id = pool_a_id);

    -- Virtual IP for Server B relay VMTA
    INSERT INTO mailing_ip_addresses (id, organization_id, pool_id, ip_address, hostname, status,
        warmup_daily_limit, warmup_stage, warmup_day, hosting_provider, acquisition_type,
        rdns_verified, reputation_score, created_at, updated_at)
    SELECT gen_random_uuid(), org_id, pool_b_id, '15.204.107.107'::inet,
           'ses-relay.mail.projectjarvis.io', 'active',
           50000, 'mature', 999, 'OVH', 'purchased', true, 100.0, NOW(), NOW()
    WHERE pool_b_id IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM mailing_ip_addresses WHERE hostname = 'ses-relay.mail.projectjarvis.io' AND pool_id = pool_b_id);
END $$`},
		// Route m.* profiles through SES relay pools (empty pool_prefix = direct pool join)
		{"ses_relay_m_profiles", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET ip_pool = 'ses-relay-a', pool_prefix = ''
    WHERE sending_domain = 'm.discountblog.com' AND vendor_type = 'pmta'
      AND (ip_pool != 'ses-relay-a' OR pool_prefix != '' OR pool_prefix IS NULL);
    UPDATE mailing_sending_profiles SET ip_pool = 'ses-relay-a', pool_prefix = ''
    WHERE sending_domain = 'm.quizfiesta.com' AND vendor_type = 'pmta'
      AND (ip_pool != 'ses-relay-a' OR pool_prefix != '' OR pool_prefix IS NULL);
    -- HT/MH route through their per-brand tenant pools, NOT ses-relay-b.
    -- PMTA Server B has NO virtual-mta named ses-relay-b (only ht-ses-pool
    -- and mh-ses-pool); pinning these to ses-relay-b makes every send fail
    -- with a 554 "specified Virtual MTA ses-relay-b does not exist" reject.
    -- This MUST match fix_ht_mh_ses_tenant_ip_pool — otherwise whichever of the
    -- two migrations runs last on boot wins, and the bug silently returns.
    UPDATE mailing_sending_profiles SET ip_pool = 'ht-ses-pool', pool_prefix = ''
    WHERE sending_domain = 'm.historythinking.com' AND vendor_type = 'pmta'
      AND (ip_pool != 'ht-ses-pool' OR pool_prefix != '' OR pool_prefix IS NULL);
    UPDATE mailing_sending_profiles SET ip_pool = 'mh-ses-pool', pool_prefix = ''
    WHERE sending_domain = 'm.myownhealth.net' AND vendor_type = 'pmta'
      AND (ip_pool != 'mh-ses-pool' OR pool_prefix != '' OR pool_prefix IS NULL);
END $$`},

		// ── Audience architecture refactor: add finalizing_audience status ──
		{"drop_status_chk_v2", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`},
		{"readd_status_chk_v2", `ALTER TABLE mailing_campaigns ADD CONSTRAINT mailing_campaigns_status_check CHECK (status IN ('draft','scheduled','preparing','finalizing_audience','sending','paused','completed','completed_with_errors','cancelled','failed','deleted','sent'))`},

		// ── Segment pre-materialization table ──
		{"create_segment_members", `CREATE TABLE IF NOT EXISTS mailing_segment_members (
			segment_id UUID NOT NULL,
			subscriber_id UUID NOT NULL,
			email TEXT NOT NULL,
			materialized_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (segment_id, subscriber_id)
		)`},
		{"idx_segment_members_lookup", `CREATE INDEX IF NOT EXISTS idx_segment_members_lookup ON mailing_segment_members(segment_id, email)`},
		{"idx_tracking_events_segment", `CREATE INDEX IF NOT EXISTS idx_tracking_events_segment ON mailing_tracking_events (subscriber_id, event_type, sending_domain, event_at)`},
		{"idx_tracking_events_segment_v2", `CREATE INDEX IF NOT EXISTS idx_tracking_events_segment_v2 ON mailing_tracking_events (event_type, event_at, sending_domain, subscriber_id)`},

		// Reset campaigns orphaned in 'preparing' by a previous crash/deploy.
		{"reset_stale_preparing_v1", `UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW() WHERE status = 'preparing' AND updated_at < NOW() - INTERVAL '45 minutes'`},

		// ── Suppression worker infrastructure (Phase: sunset suppression) ──

		// Tracking event partitions (monthly range on event_at)
		{"create_tracking_partition_apr26", `CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_04 PARTITION OF mailing_tracking_events FOR VALUES FROM ('2026-04-01') TO ('2026-05-01')`},
		{"create_tracking_partition_may26", `CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_05 PARTITION OF mailing_tracking_events FOR VALUES FROM ('2026-05-01') TO ('2026-06-01')`},
		{"create_tracking_partition_jun26", `CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_06 PARTITION OF mailing_tracking_events FOR VALUES FROM ('2026-06-01') TO ('2026-07-01')`},
		{"create_tracking_partition_jul26", `CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_07 PARTITION OF mailing_tracking_events FOR VALUES FROM ('2026-07-01') TO ('2026-08-01')`},

		// base_domain column on sending profiles for reliable brand discovery
		{"add_sending_profile_base_domain", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS base_domain TEXT`},
		{"backfill_sending_profile_base_domain", `UPDATE mailing_sending_profiles SET base_domain = REGEXP_REPLACE(sending_domain, '^(em\.|m\.)','') WHERE base_domain IS NULL AND sending_domain IS NOT NULL`},

		// Run-tracking table for suppression list worker (prevents duplicate runs)
		{"create_suppression_list_runs", `CREATE TABLE IF NOT EXISTS suppression_list_runs (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			brand TEXT NOT NULL,
			started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at TIMESTAMPTZ,
			suppressed_count INTEGER DEFAULT 0,
			status TEXT DEFAULT 'running',
			error_message TEXT
		)`},
		{"idx_suppression_list_runs_brand", `CREATE INDEX IF NOT EXISTS idx_suppression_list_runs_brand ON suppression_list_runs(brand, started_at DESC)`},

		// One-off: requeue two failed campaigns for audience finalization after pipeline hardening.
		{"drop_status_check_constraint", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`},
		{"requeue_failed_campaigns_apr03_v2", `UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW() WHERE id IN ('9f4ed554-9c55-4995-add9-b6481e8798b9','bfb79e54-ff6b-409b-861b-d54889199039') AND status IN ('draft','scheduled','failed')`},
		{"requeue_failed_campaigns_apr03_v3", `UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW() WHERE id IN ('9f4ed554-9c55-4995-add9-b6481e8798b9','bfb79e54-ff6b-409b-861b-d54889199039') AND status = 'failed'`},
		{"requeue_failed_campaigns_apr03_v4", `UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW() WHERE id IN ('9f4ed554-9c55-4995-add9-b6481e8798b9','bfb79e54-ff6b-409b-861b-d54889199039') AND status = 'failed'`},

		// One-off: fix f5f8f4e5 that was marked failed due to race between 2 AudienceWorkers.
		{"fix_f5f8f4e5_to_scheduled", `UPDATE mailing_campaigns SET status = 'scheduled', updated_at = NOW() WHERE id = 'f5f8f4e5-e3b3-43f1-ac77-137fe8a41539' AND status = 'failed'`},

		// One-off: requeue all 8 cancelled April-4 campaigns for audience finalization.
		{"requeue_apr04_cancelled_campaigns", `UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW() WHERE id IN (
			'95d75b85-2712-456b-9d9a-1e8c2053633a',
			'bf294b87-26e7-4906-9fe1-d95c7ff1eb09',
			'50213f15-451b-479f-acdd-8fd0a414298f',
			'9c20495e-28f5-438c-a01d-c8c804e05453',
			'9f4ed554-9c55-4995-add9-b6481e8798b9',
			'bfb79e54-ff6b-409b-861b-d54889199039',
			'365a6ca0-1e74-4ac5-b3d5-3c25f56ed715',
			'f5f8f4e5-e3b3-43f1-ac77-137fe8a41539'
		) AND status = 'cancelled'`},

		// Source-level quality tracking: stores per-list quality metrics after campaigns complete.
		{"create_list_quality_metrics", `CREATE TABLE IF NOT EXISTS mailing_list_quality_metrics (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			list_id UUID NOT NULL,
			campaign_id UUID,
			evaluated_at TIMESTAMPTZ DEFAULT NOW(),
			total_sent INTEGER DEFAULT 0,
			delivered INTEGER DEFAULT 0,
			bounced INTEGER DEFAULT 0,
			complained INTEGER DEFAULT 0,
			acceptance_rate NUMERIC(5,4) DEFAULT 0,
			complaint_rate NUMERIC(7,6) DEFAULT 0,
			bounce_rate NUMERIC(5,4) DEFAULT 0
		)`},
		{"idx_list_quality_metrics_list", `CREATE INDEX IF NOT EXISTS idx_list_quality_metrics_list ON mailing_list_quality_metrics(list_id, evaluated_at DESC)`},

		{"create_pmta_acct_raw", `CREATE TABLE IF NOT EXISTS pmta_acct_raw (
			id BIGSERIAL PRIMARY KEY,
			received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			record_type TEXT NOT NULL,
			recipient TEXT NOT NULL,
			sender TEXT,
			source_ip TEXT,
			vmta TEXT,
			pool TEXT,
			recipient_domain TEXT,
			bounce_cat TEXT,
			dsn_status TEXT,
			dsn_diag TEXT,
			job_id TEXT,
			time_logged TEXT,
			processed BOOLEAN NOT NULL DEFAULT FALSE,
			campaign_id UUID,
			recipient_isp TEXT
		)`},
		{"idx_acct_raw_unprocessed", `CREATE INDEX IF NOT EXISTS idx_acct_raw_unprocessed ON pmta_acct_raw (processed, id) WHERE processed = FALSE`},
		{"idx_acct_raw_received", `CREATE INDEX IF NOT EXISTS idx_acct_raw_received ON pmta_acct_raw (received_at)`},

		{"create_pmta_acct_daily_summary", `CREATE TABLE IF NOT EXISTS pmta_acct_daily_summary (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			summary_date DATE NOT NULL,
			campaign_id UUID,
			recipient_isp TEXT NOT NULL DEFAULT 'other',
			delivered INT NOT NULL DEFAULT 0,
			relayed_to_ses INT NOT NULL DEFAULT 0,
			hard_bounced INT NOT NULL DEFAULT 0,
			soft_bounced INT NOT NULL DEFAULT 0,
			complained INT NOT NULL DEFAULT 0,
			deferred INT NOT NULL DEFAULT 0,
			total_records INT NOT NULL DEFAULT 0,
			last_updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		// relayed_to_ses: PMTA "d" records on SES relay VMTAs/pools are SES
		// handoffs, not recipient deliveries. Tracked separately so campaign
		// delivery rates reflect SES DELIVERY truth, not PMTA relay success.
		{"add_acct_summary_relayed_to_ses", `ALTER TABLE pmta_acct_daily_summary ADD COLUMN IF NOT EXISTS relayed_to_ses INT NOT NULL DEFAULT 0`},

		// ses_delivered: SES-confirmed DELIVERY events written back into the
		// rollup by the SES events webhook at the SAME (date, campaign, isp)
		// grain the summary builder uses. For via_ses brands PMTA "d" lands in
		// relayed_to_ses (handoff, not delivery), so the authoritative delivered
		// count is the SES DELIVERY event. Accounting-driven consumers read
		// (delivered + ses_delivered) as true delivered while delivered stays
		// pure PMTA-direct facts and relayed_to_ses stays the labeled handoff.
		{"add_acct_summary_ses_delivered", `ALTER TABLE pmta_acct_daily_summary ADD COLUMN IF NOT EXISTS ses_delivered INT NOT NULL DEFAULT 0`},

		// Audience funnel: one aggregate row per campaign capturing targeted vs
		// suppressed-by-reason at finalize time (Phase 0 analytics rebuild).
		// Hot-DB-safe aggregate form of the recipient_targeted/recipient_suppressed
		// signal; per-recipient events ride the Phase 1 S3 event lake.
		{"create_campaign_audience_funnel", `CREATE TABLE IF NOT EXISTS mailing_campaign_audience_funnel (
			campaign_id UUID PRIMARY KEY,
			organization_id UUID,
			total_seen INTEGER NOT NULL DEFAULT 0,
			targeted INTEGER NOT NULL DEFAULT 0,
			reserve INTEGER NOT NULL DEFAULT 0,
			suppressed_total INTEGER NOT NULL DEFAULT 0,
			reason_breakdown JSONB NOT NULL DEFAULT '{}'::jsonb,
			computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"idx_acct_summary_campaign", `CREATE INDEX IF NOT EXISTS idx_acct_summary_campaign ON pmta_acct_daily_summary (campaign_id)`},
		{"idx_acct_summary_date", `CREATE INDEX IF NOT EXISTS idx_acct_summary_date ON pmta_acct_daily_summary (summary_date)`},
		{"uq_acct_summary_key", `CREATE UNIQUE INDEX IF NOT EXISTS uq_acct_summary_key ON pmta_acct_daily_summary (summary_date, COALESCE(campaign_id, '00000000-0000-0000-0000-000000000000'::uuid), recipient_isp)`},

		// Phase 19: Delete decommissioned OVH IPs and fix IPXO yh hostname renames.
		// OVH IPs (15.204.22.x and 15.204.38.x) were permanently removed from service.
		// 26 IPXO IPs originally assigned to yahoo pools were redistributed to other ISP
		// pools (Phase 10) but their hostnames were never updated in earlier migrations.
		// This migration corrects hostnames to match the live PMTA config and DNS records.
		// Runs last so it wins over phase6_seed_all_ips ON CONFLICT DO UPDATE.
		{"phase19_delete_ovh_ips", `DELETE FROM mailing_ip_addresses WHERE ip_address << '15.204.22.0/24'::inet OR ip_address << '15.204.38.0/24'::inet`},
		{"phase19_fix_ipxo_yh_hostnames", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            ('144.225.178.7',   'mta-db-gm9.mail.em.discountblog.com',    'db-gmail-pool'),
            ('144.225.178.8',   'mta-db-ms9.mail.em.discountblog.com',    'db-msft-pool'),
            ('144.225.178.9',   'mta-db-ap8.mail.em.discountblog.com',    'db-apple-pool'),
            ('144.225.178.10',  'mta-db-cc8.mail.em.discountblog.com',    'db-comcast-pool'),
            ('144.225.178.11',  'mta-db-gn7.mail.em.discountblog.com',    'db-general-pool'),
            ('144.225.178.12',  'mta-db-ch7.mail.em.discountblog.com',    'db-charter-pool'),
            ('144.225.178.71',  'mta-qf-gm9.mail.em.quizfiesta.com',     'qf-gmail-pool'),
            ('144.225.178.72',  'mta-qf-ms9.mail.em.quizfiesta.com',     'qf-msft-pool'),
            ('144.225.178.73',  'mta-qf-ap8.mail.em.quizfiesta.com',     'qf-apple-pool'),
            ('144.225.178.74',  'mta-qf-cc8.mail.em.quizfiesta.com',     'qf-comcast-pool'),
            ('144.225.178.75',  'mta-qf-gn7.mail.em.quizfiesta.com',     'qf-general-pool'),
            ('144.225.178.76',  'mta-qf-ch7.mail.em.quizfiesta.com',     'qf-charter-pool'),
            ('144.225.178.136', 'mta-ht-gm9.mail.em.historythinking.com', 'ht-gmail-pool'),
            ('144.225.178.137', 'mta-ht-ms9.mail.em.historythinking.com', 'ht-msft-pool'),
            ('144.225.178.138', 'mta-ht-ap8.mail.em.historythinking.com', 'ht-apple-pool'),
            ('144.225.178.139', 'mta-ht-cc8.mail.em.historythinking.com', 'ht-comcast-pool'),
            ('144.225.178.140', 'mta-ht-gn7.mail.em.historythinking.com', 'ht-general-pool'),
            ('144.225.178.141', 'mta-ht-ch7.mail.em.historythinking.com', 'ht-charter-pool'),
            ('144.225.178.142', 'mta-ht-gn8.mail.em.historythinking.com', 'ht-general-pool'),
            ('144.225.178.200', 'mta-mh-gm9.mail.em.myownhealth.net',    'mh-gmail-pool'),
            ('144.225.178.201', 'mta-mh-ms9.mail.em.myownhealth.net',    'mh-msft-pool'),
            ('144.225.178.202', 'mta-mh-ap8.mail.em.myownhealth.net',    'mh-apple-pool'),
            ('144.225.178.203', 'mta-mh-cc8.mail.em.myownhealth.net',    'mh-comcast-pool'),
            ('144.225.178.204', 'mta-mh-gn7.mail.em.myownhealth.net',    'mh-general-pool'),
            ('144.225.178.205', 'mta-mh-ch7.mail.em.myownhealth.net',    'mh-charter-pool'),
            ('144.225.178.206', 'mta-mh-gn8.mail.em.myownhealth.net',    'mh-general-pool')
        ) AS t(ip_addr, correct_hostname, correct_pool)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools
            WHERE name = rec.correct_pool AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses
                SET hostname = rec.correct_hostname, pool_id = pool_id_val, updated_at = NOW()
                WHERE ip_address = rec.ip_addr::inet;
        END IF;
    END LOOP;
END $$`},

		// Phase 20: Deactivate m.* SES relay profiles that lack A/MX DNS records.
		// m.historythinking.com, m.quizfiesta.com, m.myownhealth.net all have SPF
		// but no A or MX record, causing "Domain not found" rejections from Apple
		// and other strict MTAs. m.discountblog.com is excluded (has valid MX).
		//
		// 2026-06-06 guard (via_ses=false): under the all-SES tenant cutover the
		// via_ses=true tenant profiles (Quiz Fiesta / History Thinking / My Own
		// Health (SES Tenant)) deliver through their SES tenant — the missing
		// m.* A/MX record is irrelevant on that path (proven by the 6 sibling
		// Server-B tenant brands hw/lp/rb/tt/wf/yi, which are active and deliver
		// fine with the identical no-A/MX posture). Without this guard, phase20
		// runs AFTER activate_qf_ht_mh_ses_tenant_profiles every boot (migrations
		// re-run in slice order, no applied-tracking) and silently re-deactivated
		// the tenant profiles, blocking all future HT/MH/QF partner-drip deploys
		// at preflight (which requires status='active'). The guard keeps phase20
		// deactivating only the legacy via_ses=false relay duplicates.
		{"phase20_deactivate_broken_ses_profiles", `UPDATE mailing_sending_profiles
			SET status = 'inactive', updated_at = NOW()
			WHERE sending_domain IN ('m.historythinking.com', 'm.quizfiesta.com', 'm.myownhealth.net')
			  AND status = 'active'
			  AND COALESCE(via_ses, false) = false`},

		// ---------------------------------------------------------------------
		// Phase 21: Master List Migration — additive schema (P1)
		//
		// Moves audience selection off the 40+ brand×ISP list duplication model
		// and onto a single master subscriber pool with per-domain operational
		// state. This block is ADDITIVE ONLY — no writes, no reads, no behavior
		// change until P2/P3. Reversible by dropping the new table + columns.
		//
		// Design (agreed, not speculative):
		//   - One row per unique email in mailing_subscribers (dedupe in P4).
		//   - Per-domain state in mailing_subscriber_domain_state keyed on
		//     (subscriber_id, sending_domain).
		//   - Hard bounce + complaint stay global on mailing_subscribers.
		//   - Unsubscribe is domain-scoped (lives in the SDS side table).
		//   - brand_affinity TEXT[] and engagement_score are reporting-only,
		//     never selection drivers. Selection always joins SDS.
		//   - cross_brand_daily_cap is configurable per org; initial default 2.
		// ---------------------------------------------------------------------
		{"phase21_add_brand_affinity", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS brand_affinity TEXT[] DEFAULT '{}'`},
		{"phase21_add_hard_bounced_at", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS hard_bounced_at TIMESTAMPTZ`},
		{"phase21_add_complained_at", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS complained_at TIMESTAMPTZ`},
		{"phase21_create_subscriber_domain_state", `CREATE TABLE IF NOT EXISTS mailing_subscriber_domain_state (
			subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
			sending_domain TEXT NOT NULL,
			last_mailed_at TIMESTAMPTZ,
			total_sent BIGINT NOT NULL DEFAULT 0,
			total_opens BIGINT NOT NULL DEFAULT 0,
			total_clicks BIGINT NOT NULL DEFAULT 0,
			last_open_at TIMESTAMPTZ,
			last_click_at TIMESTAMPTZ,
			score_local NUMERIC(5,4) NOT NULL DEFAULT 0,
			warmup_status TEXT NOT NULL DEFAULT 'cold',
			warmup_status_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			unsubscribed_at TIMESTAMPTZ,
			hard_bounced_at TIMESTAMPTZ,
			complained_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (subscriber_id, sending_domain),
			CONSTRAINT mailing_sds_warmup_chk CHECK (warmup_status IN ('cold','warming','engaged','dormant'))
		)`},
		{"phase21_sds_idx_domain_last_mailed", `CREATE INDEX IF NOT EXISTS idx_sds_domain_last_mailed ON mailing_subscriber_domain_state (sending_domain, last_mailed_at)`},
		{"phase21_sds_idx_domain_warmup", `CREATE INDEX IF NOT EXISTS idx_sds_domain_warmup ON mailing_subscriber_domain_state (sending_domain, warmup_status)`},
		{"phase21_sds_idx_domain_score", `CREATE INDEX IF NOT EXISTS idx_sds_domain_score ON mailing_subscriber_domain_state (sending_domain, score_local)`},
		{"phase21_sds_idx_domain_last_open", `CREATE INDEX IF NOT EXISTS idx_sds_domain_last_open ON mailing_subscriber_domain_state (sending_domain, last_open_at)`},
		{"phase21_sds_idx_mailable", `CREATE INDEX IF NOT EXISTS idx_sds_mailable ON mailing_subscriber_domain_state (subscriber_id) WHERE unsubscribed_at IS NULL AND hard_bounced_at IS NULL`},
		{"phase21_sds_idx_subscriber", `CREATE INDEX IF NOT EXISTS idx_sds_subscriber ON mailing_subscriber_domain_state (subscriber_id)`},

		// === SDS state machine extension (per-domain engagement engine, 2026-05-09) ===
		//
		// Adds a per-(subscriber, sending_domain) lifecycle state on top of the
		// existing SDS counters. Every subscriber is in one of:
		//   probe       — not yet classified (default; total_sent < 4 and no engagement)
		//   engaged     — has opened or clicked at least once on this sending domain
		//   cold        — received >= 4 sends with zero opens and zero clicks
		//   suppressed  — per-domain suppression (unsub / hard bounce / complaint)
		//
		// `state` is distinct from `warmup_status`: warmup_status describes
		// whether a subscriber should be considered for sends from a still-
		// warming SENDER, while `state` describes whether the subscriber is
		// engaged with that sender domain. Keep both — they answer different
		// questions.
		//
		// Subscriber-level companion column `cross_engaged` is set by the
		// nightly graduation job in internal/worker/sds_graduation_job.go for
		// any subscriber engaged on >= 2 distinct sending_domains. The audience
		// finalizer (SA-2) prioritizes cross_engaged subscribers across all
		// brand sends because they have proven inbox-placement signal across
		// the portfolio.
		//
		// All statements are idempotent (IF NOT EXISTS) and safe to re-run.
		{"sds_state_add_state_col", `ALTER TABLE mailing_subscriber_domain_state
			ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'probe'
			CHECK (state IN ('probe','engaged','cold','suppressed'))`},
		{"sds_state_add_state_updated_at", `ALTER TABLE mailing_subscriber_domain_state
			ADD COLUMN IF NOT EXISTS state_updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`},
		{"sds_state_idx_domain", `CREATE INDEX IF NOT EXISTS idx_sds_state_domain
			ON mailing_subscriber_domain_state (sending_domain, state)`},
		{"sds_state_idx_subscriber_engaged", `CREATE INDEX IF NOT EXISTS idx_sds_state_subscriber
			ON mailing_subscriber_domain_state (subscriber_id) WHERE state = 'engaged'`},
		{"sds_state_add_cross_engaged_col", `ALTER TABLE mailing_subscribers
			ADD COLUMN IF NOT EXISTS cross_engaged BOOLEAN NOT NULL DEFAULT FALSE`},
		{"sds_state_add_cross_engaged_at_col", `ALTER TABLE mailing_subscribers
			ADD COLUMN IF NOT EXISTS cross_engaged_at TIMESTAMPTZ`},
		{"sds_state_idx_subscribers_cross_engaged", `CREATE INDEX IF NOT EXISTS idx_subscribers_cross_engaged
			ON mailing_subscribers (cross_engaged) WHERE cross_engaged = true`},
		// Idempotent backfill: only fires when every row is still at the
		// schema defaults (state='probe' AND state_updated_at = created_at).
		// On re-run after a successful first pass the WHERE check returns
		// non-zero rows and the UPDATE is skipped. This pattern avoids the
		// need for a separate _migration_log row.
		{"sds_state_backfill_from_history", `DO $backfill$
		BEGIN
			IF (SELECT COUNT(*) FROM mailing_subscriber_domain_state
				WHERE state != 'probe' OR state_updated_at != created_at) = 0 THEN
				UPDATE mailing_subscriber_domain_state
				SET state = CASE
					WHEN total_opens > 0 OR total_clicks > 0 THEN 'engaged'
					WHEN total_sent >= 4 THEN 'cold'
					ELSE 'probe'
				END,
				state_updated_at = COALESCE(last_open_at, last_click_at, last_mailed_at, NOW());
			END IF;
		END $backfill$`},

		// === Machine-click classifier (per-domain engagement engine, 2026-05-09) ===
		//
		// Mirrors the existing `is_machine_open` column added in
		// `add_is_machine_open_col` (see line ~2110). Populated at ingest
		// time by tracking.ClassifyClickAsMachine using a conservative
		// rule set (known scanner UAs, bare Mozilla/5.0 + cloud ASN, sub-
		// 30-second click-after-send heuristic). Defaults to FALSE so
		// existing rows and any future writer that omits the column do
		// not get accidentally flagged as machine traffic.
		//
		// INFORMATIONAL ONLY in this release: segment definitions,
		// engagement state, and the audience finalizer continue to treat
		// every click row as engagement signal regardless of the
		// classifier's output. We are populating the column now so the
		// next operator decision can be made off real production data.
		//
		// The partial index covers the dominant analytics access pattern
		// — "last hour of clicks split by classifier verdict" — without
		// bloating the (much larger) full event_at index.
		{"add_is_machine_click_col", `ALTER TABLE mailing_tracking_events
			ADD COLUMN IF NOT EXISTS is_machine_click BOOLEAN DEFAULT FALSE`},
		{"add_idx_mte_machine_click", `CREATE INDEX IF NOT EXISTS idx_tracking_events_machine_click
			ON mailing_tracking_events (event_at, is_machine_click)
			WHERE event_type = 'clicked'`},

		// === Table-driven Microsoft/Azure datacenter click classifier (2026-07-06) ===
		// See cmd/server/datacenter_classifier.go for the full rationale and the
		// canonical DDL. Ordering is load-bearing: the containment table exists
		// before the helper that scans it, the helper before the verdict fn that
		// calls it, the verdict fn before the trigger fn that calls it, and the
		// click_verdict column before the trigger that writes it. All idempotent;
		// CREATE OR REPLACE FUNCTION + the seed + the guarded trigger DO-block are
		// not recognized by the skip-probe and so re-apply every boot (cheap), which
		// is exactly how a boot re-asserts the committed function body over any
		// drift (checkVerdictFunctionDrift logs a WARNING first — see boot goroutine).
		{"create_ignite_datacenter_ranges", igniteDatacenterRangesDDL},
		{"idx_ignite_datacenter_ranges_gist", igniteDatacenterRangesGistDDL},
		{"seed_ignite_datacenter_ranges", igniteDatacenterSeedDDL},
		{"create_ignite_ip_is_datacenter_fn", igniteIPIsDatacenterDDL},
		{"create_ignite_event_verdict_fn", igniteEventVerdictDDL},
		{"create_ignite_verdict_is_human_fn", igniteVerdictIsHumanDDL},
		{"add_click_verdict_col", clickVerdictColumnDDL},
		{"add_idx_mte_click_verdict", clickVerdictIndexDDL},
		{"create_ignite_set_click_verdict_fn", igniteSetClickVerdictFnDDL},
		{"create_trg_set_click_verdict", igniteSetClickVerdictTriggerDDL},

		// === Wave processor health view (per-domain engagement engine, 2026-05-09) ===
		//
		// Companion to SA-7's /api/wave-processor/status HTTP handler.
		// Surfaces the same per-sending-domain wave queue depth, overdue
		// counts, and recent throughput as a SQL-only view so operators
		// can run ad-hoc psql diagnostics without standing up a request:
		//
		//   SELECT * FROM v_wave_processor_health;
		//
		// CREATE OR REPLACE so re-runs (every server boot) are no-ops.
		// View definition mirrors the handler's consolidated query plus
		// the dispatched_last_5m and completed_last_1h windows that are
		// useful for psql ad-hoc but not surfaced in the JSON struct.
		{"create_v_wave_processor_health", `CREATE OR REPLACE VIEW v_wave_processor_health AS
			SELECT
				sp.sending_domain,
				COUNT(*) FILTER (WHERE w.status='planned' AND w.scheduled_at <= NOW())                                       AS waves_due,
				COUNT(*) FILTER (WHERE w.status='planned' AND w.scheduled_at < NOW() - INTERVAL '5 minutes')                 AS waves_overdue_5m,
				COUNT(*) FILTER (WHERE w.status='completed' AND w.completed_at > NOW() - INTERVAL '5 minutes')               AS dispatched_last_5m,
				COUNT(*) FILTER (WHERE w.status='completed' AND w.completed_at > NOW() - INTERVAL '1 hour')                  AS completed_last_1h,
				MAX(EXTRACT(EPOCH FROM (NOW() - w.scheduled_at))) FILTER (WHERE w.status='planned' AND w.scheduled_at <= NOW()) AS max_due_age_seconds,
				MAX(w.completed_at) FILTER (WHERE w.status='completed')                                                      AS last_completed_at
			FROM mailing_campaign_waves w
			JOIN mailing_campaigns c ON c.id = w.campaign_id
			JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
			GROUP BY sp.sending_domain
			ORDER BY max_due_age_seconds DESC NULLS LAST`},

		// Campaign pilot flag. False = legacy list_ids path; true = master-selection path.
		{"phase21_add_use_master_selection", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS use_master_selection BOOLEAN NOT NULL DEFAULT false`},
		// P5c: flip the default to true for all future campaigns. Legacy
		// branches in pmta_campaign_planner.go and campaign_builder_send_async.go
		// stay in place for one release as a kill switch. After the release
		// that ships this migration we expect zero campaigns to run with the
		// flag off; the follow-up deploy deletes the legacy code.
		{"phase21_default_use_master_selection_true", `ALTER TABLE mailing_campaigns ALTER COLUMN use_master_selection SET DEFAULT true`},
		// P6: hide the 40+ brand×ISP lists from the UI. They are no longer
		// the selection driver — SDS is. Lists remain in the DB for
		// historical queries and as ingestion targets, but the campaign
		// builder should default to segments going forward.
		// Name pattern: '<Brand> - <ISP>' where ISP is one of the nine
		// classifiers this workspace uses. Safe: no non-brand×ISP list
		// ever matches this regex.
		{"phase21_hide_brand_isp_lists", `
			UPDATE mailing_lists
			SET is_visible = false
			WHERE name ~ '^.+ - (AOL|ATT|Apple|Charter|Comcast|Cox|Microsoft|Other|SBCGlobal|Yahoo|Gmail)$'
			  AND is_visible = true`},
		// Org-level settings JSONB. Seeded with cross_brand_daily_cap=2 per the
		// agreed initial cap. Ceiling at scale is 4/sub/day; ramp is an ops
		// decision, not a code deploy.
		{"phase21_add_org_settings", `ALTER TABLE organizations ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}'`},
		{"phase21_seed_cross_brand_cap", `UPDATE organizations
			SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{cross_brand_daily_cap}', '2'::jsonb, true)
			WHERE settings->>'cross_brand_daily_cap' IS NULL`},
		// Rebrand: rename single-tenant org Ignite Media Group -> James Ventures Corp (idempotent).
		{"rebrand_org_to_james_ventures", `UPDATE organizations SET name='James Ventures Corp' WHERE name='Ignite Media Group'`},
		// ---------------------------------------------------------------------
		// Master List Migration P7 — subscriber provenance metadata.
		//
		// The legacy schema only carries two provenance hints on
		// mailing_subscribers:
		//   - source VARCHAR(50)   — often "acquisition_<date>" or empty
		//   - data_source          — added ad-hoc by import scripts
		//
		// Neither scales: they can't distinguish two imports from the same
		// partner on the same day, they can't carry UTM / referrer / file-
		// name context, and they can't be queried structurally (source is
		// pure freetext).
		//
		// This migration adds structured provenance that every future
		// ingest path MUST populate:
		//   source_system   — canonical bucket:
		//                     'acquisition_import' | 'web_signup' |
		//                     'api' | 'automation' | 'manual'
		//   source_detail   — human-readable provenance string
		//                     (e.g. "acquisition_04102026",
		//                      "quiz_fiesta_signup:/landing/history",
		//                      "everflow_api:offer=42")
		//   source_metadata — arbitrary JSONB (filename, row offset,
		//                     UTM params, referrer URL, partner batch id)
		//   imported_at     — when record was added to master list
		//                     (distinct from subscribed_at, which is when
		//                      the subscriber confirmed or opted in)
		//   imported_from_list_id — the list they came in through. Useful
		//                     for audit after the dedupe step since a row
		//                     may be re-pointed to a canonical list_id.
		//
		// Backfill rules:
		//   - source_system defaults to 'legacy_import' for existing rows
		//     that have any value in source or data_source, else
		//     'unknown'.
		//   - source_detail is populated from source for existing rows.
		//   - imported_at is set to created_at.
		//   - imported_from_list_id is set to list_id.
		// ---------------------------------------------------------------------
		{"phase21_add_source_system", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS source_system VARCHAR(50)`},
		{"phase21_add_source_detail", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS source_detail VARCHAR(200)`},
		{"phase21_add_source_metadata", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS source_metadata JSONB NOT NULL DEFAULT '{}'::jsonb`},
		{"phase21_add_imported_at", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS imported_at TIMESTAMPTZ`},
		{"phase21_add_imported_from_list_id", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS imported_from_list_id UUID`},
		{"phase21_idx_source_system", `CREATE INDEX IF NOT EXISTS idx_subscribers_source_system ON mailing_subscribers (source_system) WHERE source_system IS NOT NULL`},
		{"phase21_idx_source_detail", `CREATE INDEX IF NOT EXISTS idx_subscribers_source_detail ON mailing_subscribers (source_detail) WHERE source_detail IS NOT NULL`},
		{"phase21_idx_imported_at", `CREATE INDEX IF NOT EXISTS idx_subscribers_imported_at ON mailing_subscribers (imported_at DESC) WHERE imported_at IS NOT NULL`},
		{"phase21_idx_source_metadata_gin", `CREATE INDEX IF NOT EXISTS idx_subscribers_source_metadata_gin ON mailing_subscribers USING gin (source_metadata)`},
		// Backfill only rows that haven't been stamped yet. Capped by NOT EXISTS
		// so the UPDATE is idempotent across server restarts.
		{"phase21_backfill_source_system", `
			UPDATE mailing_subscribers
			SET source_system = CASE
			  WHEN source IS NOT NULL AND source <> '' THEN 'legacy_import'
			  ELSE 'unknown'
			END
			WHERE source_system IS NULL`},
		{"phase21_backfill_source_detail", `
			UPDATE mailing_subscribers
			SET source_detail = LEFT(source, 200)
			WHERE source_detail IS NULL AND source IS NOT NULL AND source <> ''`},
		{"phase21_backfill_imported_at", `
			UPDATE mailing_subscribers
			SET imported_at = COALESCE(subscribed_at, created_at)
			WHERE imported_at IS NULL`},
		{"phase21_backfill_imported_from_list_id", `
			UPDATE mailing_subscribers
			SET imported_from_list_id = list_id
			WHERE imported_from_list_id IS NULL AND list_id IS NOT NULL`},
		// ---------------------------------------------------------------------
		// Master List Migration P8 — canonical segments.
		//
		// Seeds three first-class, user-visible segments per organization
		// so the UI has named handles that express how the master-list
		// architecture is intended to be used:
		//
		//   1. Master List — every eligible subscriber (confirmed, not
		//      hard-bounced, not complained, not suppressed). This is the
		//      literal answer to "all subscribers should be associated to
		//      this list" — one named segment that spans the entire
		//      mailable pool regardless of which legacy brand×ISP list
		//      the row was originally imported through.
		//
		//   2. Engaged Openers (30d) — last_open_at within 30 days. Pre-
		//      built so operators don't have to reconstruct it every time.
		//
		//   3. Engaged Clickers (30d) — last_click_at within 30 days.
		//
		// Implementation notes:
		//   - These are dynamic V2 segments with conditions expressed as
		//     ConditionGroupBuilder JSON. The empty-conditions group on
		//     "Master List" is intentional: buildSegmentQuery's V2 branch
		//     falls through to the baseline WHERE clause (status +
		//     suppressions), which is exactly the master-mailable pool.
		//   - list_id = NULL so segments span the full subscriber table
		//     rather than a single legacy brand×ISP list.
		//   - Seed is idempotent via WHERE NOT EXISTS on (name, org_id).
		//   - Initial member rows are populated on boot by
		//     SegmentMaterializer.MaterializeCanonicalSegments, then
		//     refreshed nightly alongside every other dynamic segment.
		//   - When use_master_selection=true (campaign default), the
		//     planner sources candidates directly from SDS and honours
		//     MinRemailHours to exclude recently-mailed subscribers.
		//     These segments are the surface the UI/operators use to
		//     pick a cohort; the unmailed guarantee comes from SDS.
		// ---------------------------------------------------------------------
		{"seed_master_list_segment_row", `
			INSERT INTO mailing_segments (
				id, organization_id, list_id, name, description, segment_type, conditions,
				status, subscriber_count, created_at, updated_at
			)
			SELECT gen_random_uuid(), id, NULL,
				'Master List',
				'All eligible subscribers across every source: confirmed status, not hard-bounced, not complained, not suppressed. Use this as the default audience when you want the widest reachable pool; per-domain remail gaps and ISP quotas still apply via master-selection.',
				'dynamic',
				'{"logic_operator":"AND","conditions":[]}'::jsonb,
				'active', 0, NOW(), NOW()
			FROM organizations
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_segments
				WHERE name = 'Master List' AND organization_id = organizations.id
			)
		`},
		{"seed_engaged_openers_segment_row", `
			INSERT INTO mailing_segments (
				id, organization_id, list_id, name, description, segment_type, conditions,
				status, subscriber_count, created_at, updated_at
			)
			SELECT gen_random_uuid(), id, NULL,
				'Engaged Openers (30d)',
				'Subscribers whose most recent open was within the last 30 days. High-signal cohort for re-engagement sends and deliverability warmup.',
				'dynamic',
				'{"logic_operator":"AND","conditions":[{"condition_type":"profile","field":"last_open_at","field_type":"datetime","operator":"in_last_days","value":"30"}]}'::jsonb,
				'active', 0, NOW(), NOW()
			FROM organizations
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_segments
				WHERE name = 'Engaged Openers (30d)' AND organization_id = organizations.id
			)
		`},
		{"seed_engaged_clickers_segment_row", `
			INSERT INTO mailing_segments (
				id, organization_id, list_id, name, description, segment_type, conditions,
				status, subscriber_count, created_at, updated_at
			)
			SELECT gen_random_uuid(), id, NULL,
				'Engaged Clickers (30d)',
				'Subscribers whose most recent click was within the last 30 days. The tightest deliverability cohort — use for warmup and reputation recovery.',
				'dynamic',
				'{"logic_operator":"AND","conditions":[{"condition_type":"profile","field":"last_click_at","field_type":"datetime","operator":"in_last_days","value":"30"}]}'::jsonb,
				'active', 0, NOW(), NOW()
			FROM organizations
			WHERE NOT EXISTS (
				SELECT 1 FROM mailing_segments
				WHERE name = 'Engaged Clickers (30d)' AND organization_id = organizations.id
			)
		`},
		// ---------------------------------------------------------------------
		// Apr 20 2026 campaign flight — raise cross_brand_daily_cap from 2 to 3.
		//
		// The three-engager-per-day schedule (north-star-loans 3:01 AM,
		// totalhomeauto 11:01 AM, the-capital-wallet 2:01 PM MDT) requires
		// each subscriber in a brand's 30D opener/clicker segment to be
		// able to receive 3 mails from that brand in one day. The original
		// phase21 seed defaulted to 2, which would cause CapChecker.Reserve
		// to deny the third send with 'cross_brand_cap_exceeded'.
		//
		// Idempotent on value: only raises caps that are currently below 3.
		// If an operator manually sets a higher cap (6, 12) this migration
		// is a no-op and will not stomp the larger value. Safe to re-run on
		// every boot.
		//
		// Ceiling design note: at scale the agreed ceiling is 4/sub/day. 3
		// sits inside that envelope and reserves one slot for cross-brand
		// overlap on subscribers present in multiple brand segments.
		// ---------------------------------------------------------------------
		{"apr20_bump_cross_brand_cap_to_3", `UPDATE organizations
			SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{cross_brand_daily_cap}', '3'::jsonb, true)
			WHERE COALESCE((settings->>'cross_brand_daily_cap')::int, 0) < 3`},
		// Apr 20 2026 — terminal drop of the legacy mailing_campaigns_status_check
		// CHECK constraint.
		//
		// Context: prior migrations (readd_status_chk at line 1314,
		// readd_status_chk_v2 at 3348, drop_status_check_constraint at 3390)
		// together should leave the table with no status CHECK constraint, so
		// that reserveCampaignForDeploy() can INSERT status='finalizing_audience'
		// on new campaigns. Production reality: the Apr 20 deploy surfaced
		// '23514 new row ... violates check constraint "mailing_campaigns_status_check"'
		// on every INSERT, proving the constraint is live on prod with a
		// whitelist that does NOT include 'finalizing_audience'. Root cause is
		// ambiguous — either a migration landed partially during an earlier
		// rolling deploy, or the 5s statement_timeout clipped an ADD/DROP step.
		// Either way, the fix is deterministic: DROP IF EXISTS at the end of
		// the migration sequence guarantees a clean end state regardless of
		// which of the earlier variants re-added the constraint.
		//
		// DROP IF EXISTS is idempotent — safe to re-run forever. If a future
		// migration intentionally adds back a status check, it must run AFTER
		// this one.
		{"apr20_final_drop_status_check", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`},

		// =====================================================================
		// ENGINE SIGNALS ARCHIVE INDEX (Phase 1 storage maintenance, Apr 2026)
		// =====================================================================
		// Hot window: mailing_engine_signals retains the last 14 days only.
		// Cold store: s3://$ENGINE_S3_BUCKET/engine-signals/dt=YYYY-MM-DD/isp=<isp>/*.jsonl.gz
		// This index table keeps the DB pointer so the cold-read helper
		// (internal/engine/signal_archive.go) can look up which S3 objects
		// cover a given (isp, time-range) query.
		// Written by internal/worker/engine_signals_archiver.go.
		{"create_engine_signals_archive_index", `CREATE TABLE IF NOT EXISTS mailing_engine_signals_archive_index (
			id                BIGSERIAL PRIMARY KEY,
			date_bucket       DATE NOT NULL,
			isp               VARCHAR(32) NOT NULL,
			s3_bucket         TEXT NOT NULL,
			s3_key            TEXT NOT NULL,
			row_count         BIGINT NOT NULL,
			min_recorded_at   TIMESTAMPTZ NOT NULL,
			max_recorded_at   TIMESTAMPTZ NOT NULL,
			compressed_bytes  BIGINT NOT NULL DEFAULT 0,
			format            VARCHAR(16) NOT NULL DEFAULT 'jsonl.gz',
			archived_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CONSTRAINT mailing_engine_signals_archive_index_s3_key_unique UNIQUE (s3_key)
		)`},
		{"idx_signals_archive_bucket_isp", `CREATE INDEX IF NOT EXISTS idx_signals_archive_bucket_isp
			ON mailing_engine_signals_archive_index(date_bucket, isp)`},
		{"idx_signals_archive_range", `CREATE INDEX IF NOT EXISTS idx_signals_archive_range
			ON mailing_engine_signals_archive_index(min_recorded_at, max_recorded_at)`},
		{"idx_signals_archive_isp_range", `CREATE INDEX IF NOT EXISTS idx_signals_archive_isp_range
			ON mailing_engine_signals_archive_index(isp, min_recorded_at, max_recorded_at)`},

		// =====================================================================
		// Phase 0 (Welcome Series plan): Journey analytics view
		// =====================================================================
		// The journey executor (internal/worker/journey_executor.go logExecution)
		// writes one row per node execution to mailing_journey_execution_log
		// with action in ('continue', 'wait', 'exit', 'error', 'convert').
		// However the analytics handlers (internal/api/journey_center_analytics.go
		// HandleJourneyMetrics / HandleJourneyFunnel / HandleJourneyTrends /
		// HandleJourneyPerformanceComparison) read from mailing_journey_executions
		// expecting one row per (entered, completed, exited, failed) outcome plus
		// a details JSONB column with sent/opens/clicks/etc. That table doesn't
		// exist, so every journey analytics endpoint silently returns zeros.
		// This view bridges the two:
		//   - 'entered' rows: one per log entry (every node touch counts as an entry)
		//   - outcome rows ('completed', 'exited', 'failed'): emitted for log
		//     entries with action != 'wait', mapping continue/convert -> completed,
		//     exit -> exited, error/failed -> failed
		//   - details: empty for now; Phase 3 will replace this view to pull
		//     real send/open/click counts from shadow campaigns + tracking events
		// =====================================================================
		// Phase 2 (Welcome Series) — extend mailing_journeys with the
		// engagement-exit policy, hourly per-ISP quotas, and a default
		// sending profile referenced when an email node leaves its own
		// sendingProfileId blank. All four columns are idempotent and
		// have sane defaults so existing journey rows keep working.
		// =====================================================================
		{"alter_journeys_add_exit_on_open", `ALTER TABLE mailing_journeys ADD COLUMN IF NOT EXISTS exit_on_open BOOLEAN NOT NULL DEFAULT false`},
		{"alter_journeys_add_exit_on_click", `ALTER TABLE mailing_journeys ADD COLUMN IF NOT EXISTS exit_on_click BOOLEAN NOT NULL DEFAULT false`},
		{"alter_journeys_add_isp_quotas", `ALTER TABLE mailing_journeys ADD COLUMN IF NOT EXISTS isp_quotas JSONB NOT NULL DEFAULT '{}'::jsonb`},
		// sending_profile_id is intentionally nullable (no FK) because the
		// referenced row may not exist yet during cold-start migrations and
		// because the journey-level default is optional. We resolve it at
		// activation time, not insert time.
		{"alter_journeys_add_sending_profile_id", `ALTER TABLE mailing_journeys ADD COLUMN IF NOT EXISTS sending_profile_id UUID`},

		// Phase 3 (Welcome Series wave-native send) — tag mailing_campaigns
		// rows that originate from a journey email node. journey_id +
		// journey_node_id let /api/mailing/journeys/{id}/node-stats group
		// every shadow campaign produced for a node. journey_wave_index is
		// the per-node generation counter so re-running a node creates a
		// distinct row instead of overwriting the prior one. The partial
		// index keeps the regular-campaign path index-free since the column
		// is NULL for non-journey rows.
		{"alter_campaigns_add_journey_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS journey_id UUID`},
		{"alter_campaigns_add_journey_node_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS journey_node_id TEXT`},
		{"alter_campaigns_add_journey_wave_index", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS journey_wave_index INTEGER`},
		{"idx_campaigns_journey_node", `CREATE INDEX IF NOT EXISTS idx_campaigns_journey_node ON mailing_campaigns(journey_id, journey_node_id) WHERE journey_id IS NOT NULL`},

		{"create_journey_executions_view", `CREATE OR REPLACE VIEW mailing_journey_executions AS
			SELECT
				enrollment_id,
				journey_id,
				node_id,
				node_type,
				'entered'::text AS action,
				result,
				error_message,
				'{}'::jsonb AS details,
				executed_at AS entered_at,
				NULL::timestamptz AS completed_at,
				executed_at
			FROM mailing_journey_execution_log
			UNION ALL
			SELECT
				enrollment_id,
				journey_id,
				node_id,
				node_type,
				CASE
					WHEN action IN ('continue', 'convert') THEN 'completed'
					WHEN action = 'exit' THEN 'exited'
					WHEN action IN ('error', 'failed') THEN 'failed'
					ELSE action
				END AS action,
				result,
				error_message,
				'{}'::jsonb AS details,
				executed_at AS entered_at,
				CASE
					WHEN action IN ('continue', 'convert', 'exit', 'error', 'failed') THEN executed_at
					ELSE NULL::timestamptz
				END AS completed_at,
				executed_at
			FROM mailing_journey_execution_log
			WHERE action <> 'wait'`},

		// Open dedupe table — gates side effects in HandleTrackOpen and
		// processOpen so the dual-pixel rollout (top + bottom) doesn't
		// double-count, and so the long-standing (id, event_at) PK quirk
		// (which lets multiple opens of the same message create distinct
		// rows because event_at differs by microseconds) stops inflating
		// counters. One row per (campaign_id, subscriber_id) means
		// "this subscriber opened this campaign at least once".
		{"create_mailing_open_dedupe", `CREATE TABLE IF NOT EXISTS mailing_open_dedupe (
			campaign_id UUID NOT NULL,
			subscriber_id UUID NOT NULL,
			email_id UUID,
			first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (campaign_id, subscriber_id)
		)`},
		{"idx_mailing_open_dedupe_first_seen", `CREATE INDEX IF NOT EXISTS idx_mailing_open_dedupe_first_seen ON mailing_open_dedupe(first_seen_at DESC)`},

		// ---------------------------------------------------------------------
		// May 11 2026: Re-assert AOL carve from May 8.
		//
		// Operator carved 4 highest-numbered general-pool IPs into per-brand
		// AOL pools on 2026-05-08 to isolate AOL reputation. The carve set:
		//   - mailing_ip_addresses.pool_id  -> {brand}-aol-pool
		//   - mailing_ip_pool_preferences.isolation_mode = 'strict' (with preferred_ip_id)
		//
		// The pool_preferences row survives across boots (no migration touches
		// that table). But mailing_ip_addresses.pool_id was being reverted on
		// every boot by phase12_ipxo_out_of_yahoo and phase19_fix_ipxo_yh_hostnames,
		// both of which slam these IPs back into *-general-pool. Result:
		// *-aol-pool ended up empty AND flagged strict, so all AOL queue items
		// dead-letter with `strict_pool_exhausted: no available IP in
		// strict-isolation pool *-aol-pool`.
		//
		// This migration runs AFTER phase19 (slice order) and explicitly moves
		// the 4 carved IPs into their respective *-aol-pool. Hostname is left
		// as gn7/gn8 because the original carve preserved it and PMTA
		// virtual-mta blocks resolve by hostname (not pool prefix).
		// ---------------------------------------------------------------------
		{"may11_aol_carve_reassert", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            ('144.225.178.11'::inet,  'db-aol-pool'),
            ('144.225.178.75'::inet,  'qf-aol-pool'),
            ('144.225.178.142'::inet, 'ht-aol-pool'),
            ('144.225.178.206'::inet, 'mh-aol-pool')
        ) AS t(ip_addr, target_pool)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools
            WHERE name = rec.target_pool AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses
                SET pool_id = pool_id_val, updated_at = NOW()
                WHERE ip_address = rec.ip_addr AND organization_id = org_id;
        END IF;
    END LOOP;
END $$`},

		// ---------------------------------------------------------------------
		// May 11 2026: Cloudmark CSI cold-storage trim for Comcast / Charter.
		//
		// All 25 *-comcast-pool IPs and 22 *-charter-pool IPs are listed on
		// Cloudmark's CSI BL000010, producing 100% bounce on inbound delivery
		// to comcast.net / charter.net / spectrum.net.
		//
		// MAY 11 PM UPDATE — operator-prioritized 4-IP cohort.
		// The 4 IPs NOT trimmed are the curated cohort the operator submitted
		// to Cloudmark CSI portal in the first batch:
		//   .10  → mta-db-cc8.mail.em.discountblog.com    (db-comcast-pool)
		//   .93  → mta-qf-cc4.mail.em.quizfiesta.com      (qf-comcast-pool)
		//   .139 → mta-ht-cc8.mail.em.historythinking.com (ht-comcast-pool)
		//   .223 → mta-mh-cc1.mail.em.myownhealth.net     (mh-comcast-pool)
		//
		// All 4 retained IPs sit in *-comcast-pool. *-charter-pool is fully
		// cold for all 4 brands. Combined with the strict-isolation flag set
		// by phase19 (no fallback to general-pool), this means:
		//   - comcast deliveries route through the 4 mitigated IPs (1/brand)
		//   - charter deliveries dead-letter (strict_pool_exhausted) until
		//     the operator submits charter IPs and we move them back active.
		//
		// This is the explicit operator-chosen "honest minimum-intervention"
		// path: rather than route charter traffic to compromised IPs (more
		// CSI hits) or burn general-pool reputation, we pause charter cleanly
		// while the first batch goes through Cloudmark delisting.
		//
		// status='cold' is honoured by SendWorkerPool / vmtaPool.next() — cold
		// IPs are excluded from rotation. Reversible by setting status='active'
		// once additional IPs come back from Cloudmark delisting.
		//
		// Operator action in parallel: submit additional IPs to
		// https://csi.cloudmark.com/en/reset/ as time permits, and register
		// Comcast Postmaster at https://postmaster.comcastnet.email/.
		// Charter delisting flows through the same Cloudmark portal
		// (Spectrum has no public Postmaster dashboard).
		// ---------------------------------------------------------------------
		// NOTE: must use host(ip_address) here, not ip_address::text. The inet
		// type renders as '144.225.178.27/32' when cast directly to text, so
		// an IN ('144.225.178.27', ...) check would match zero rows. host()
		// strips the /32 mask. Verified the hard way during the May 11 deploy.
		{"may11_cloudmark_cold_trim", `UPDATE mailing_ip_addresses
    SET status = 'cold', updated_at = NOW()
    WHERE organization_id = '00000000-0000-0000-0000-000000000001'
      AND status != 'cold'
      AND host(ip_address) IN (
        -- *-comcast-pool cold (22 IPs; 4 retained active: .10, .93, .139, .223)
        '144.225.178.27','144.225.178.28','144.225.178.29','144.225.178.30','144.225.178.31',
        '144.225.178.74','144.225.178.91','144.225.178.92','144.225.178.94','144.225.178.95',
        '144.225.178.159','144.225.178.160','144.225.178.161','144.225.178.162','144.225.178.163','144.225.178.164',
        '144.225.178.203','144.225.178.224','144.225.178.225','144.225.178.226','144.225.178.227','144.225.178.228',
        -- *-charter-pool fully cold (23 IPs; 0 retained active)
        '144.225.178.12','144.225.178.45','144.225.178.46','144.225.178.47','144.225.178.48','144.225.178.49',
        '144.225.178.76','144.225.178.110','144.225.178.111','144.225.178.112','144.225.178.113',
        '144.225.178.141','144.225.178.180','144.225.178.181','144.225.178.182','144.225.178.183','144.225.178.184',
        '144.225.178.205','144.225.178.244','144.225.178.245','144.225.178.246','144.225.178.247','144.225.178.248'
      )`},

		// ---------------------------------------------------------------------
		// May 11 2026 PM: Charter pools → general-pool fallback.
		//
		// All 4 *-charter-pool pools are fully cold (no submitted/mitigated
		// charter IPs in the first Cloudmark batch). Without this change,
		// charter recipients would dead-letter on `strict_pool_exhausted` per
		// the May 8 strict-isolation flag (vmtaPool.next() short-circuits on
		// strict before the Tier-2 general fallback).
		//
		// Operator decision (2026-05-11 PM): switch the 4 *-charter-pool
		// preferences from 'strict' to 'normal' so that charter recipients
		// fall back to *-general-pool (1 IP per brand) instead of dead-
		// lettering. Comcast pools remain 'strict' — comcast traffic stays
		// pinned to the 4 mitigated IPs (.10, .93, .139, .223) only.
		//
		// vmtaPool.next() routing under this state:
		//   recipient_isp=charter → ispGroups['charter'] empty (all cold)
		//                        → strictPools['charter'] = false
		//                        → Tier-2 fallback to ispGroups['general']
		//                        → 1 active general-pool IP per brand
		//   recipient_isp=comcast → ispGroups['comcast'] = [4 mitigated IPs]
		//                        → 1 IP per brand (strict, no fallback)
		//
		// Risk acknowledged: charter volume (~25k/day) lands on per-brand
		// general-pool IPs that are mid-warmup and already sit at or above
		// daily warmup limits. Adding charter accelerates load and any
		// charter-side spam-rate signal contaminates the general IP. This
		// is preferred over dead-letter at the operator's discretion.
		//
		// Reverts to 'strict' once charter IPs come back from Cloudmark
		// delisting (cohort-trim migration above re-activates them).
		//
		// Idempotent: NOT WHERE clause limits update to current 'strict'
		// rows on charter pools; re-runs are no-ops.
		// ---------------------------------------------------------------------
		{"may11_charter_general_fallback", `UPDATE mailing_ip_pool_preferences pref
    SET isolation_mode = 'normal',
        reason = 'cloudmark-trim 2026-05-11 PM: charter pools fully cold (0 active IPs); intentional fallback to *-general-pool while charter IPs await Cloudmark CSI delisting. Operator decision: route charter to general rather than dead-letter. Reverts to ''strict'' once charter IPs come back active.',
        set_by = 'operator-2026-05-11-pm',
        set_at = NOW()
    FROM mailing_ip_pools p
    WHERE p.id = pref.pool_id
      AND p.name LIKE '%-charter-pool'
      AND pref.isolation_mode = 'strict'`},

		// =====================================================================
		// Send-Day Planner support tables (2026-05-12)
		// Single source of truth for banned creatives + Gate-A attestations
		// surfaced by the new /api/mailing/send-day/* endpoints.
		// =====================================================================
		{"create_banned_creatives_tbl", `CREATE TABLE IF NOT EXISTS mailing_banned_creatives (
			filename TEXT PRIMARY KEY,
			reason TEXT NOT NULL DEFAULT '',
			paused_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		// Seed today's known-banned creative. This MUST stay in sync with
		// eng_w2_rotation.py BANNED_CREATIVES — the Python set is a backup
		// guard only; the canvas + future deploy scripts read from this
		// table. ON CONFLICT DO NOTHING keeps us idempotent across boots.
		{"seed_banned_creative_trugreen_6w", `INSERT INTO mailing_banned_creatives (filename, reason, paused_at)
		VALUES (
			'trugreen-6weeks-free.html',
			'May 2 ISP content-block; identical body across all 4 brands amplified the fingerprint. Replaced by warby-parker family. See .cursor/rules/sending-throttle.mdc.',
			'2026-05-02 00:00:00+00'
		) ON CONFLICT (filename) DO NOTHING`},

		// Gate-A attestation table — operator-attested host health for v1.
		// Real OVH telemetry will be S3-pushed in a follow-up.
		{"create_gate_attestations_tbl", `CREATE TABLE IF NOT EXISTS mailing_send_day_gate_attestations (
			gate TEXT NOT NULL,
			server_key TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'unknown',
			message TEXT NOT NULL DEFAULT '',
			last_checked_at TIMESTAMPTZ,
			updated_by TEXT,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (gate, server_key)
		)`},
		// Seed initial unknown rows so the canvas always has both server slots
		// to render even when no operator has checked off yet.
		{"seed_gate_a_server_a", `INSERT INTO mailing_send_day_gate_attestations (gate, server_key, state, message)
		VALUES ('A', 'server_a', 'unknown', 'Awaiting first operator attestation')
		ON CONFLICT (gate, server_key) DO NOTHING`},
		{"seed_gate_a_server_b", `INSERT INTO mailing_send_day_gate_attestations (gate, server_key, state, message)
		VALUES ('A', 'server_b', 'unknown', 'Awaiting first operator attestation')
		ON CONFLICT (gate, server_key) DO NOTHING`},

		// =====================================================================
		// Cap-Aware Reserve Pool (Slice 1 — schema only, additive)
		// =====================================================================
		// Background: cross_brand_daily_cap=10 today causes a deterministic
		// cap-skip cascade across the 12-brand 15-minute stagger. Late-stagger
		// brands (WF idx=11) see 100% cross_brand_cap_exceeded because earlier
		// brands have consumed every subscriber's cap slot before WF's wave
		// runs. See plan: .cursor/plans/cap-aware_reserve_pool_*.plan.md.
		//
		// This migration is ADDITIVE ONLY — adds three columns + one partial
		// index. No code path uses them until Slice 2/4 ship. Safe to deploy
		// independently as Phase 1 of the rollout.
		//
		// Status values 'reserve' and 'cap_skipped' are NOT constrained by
		// any CHECK (the column is plain varchar), so no constraint changes
		// are required.
		{"reserve_pool_isp_plans_col", `ALTER TABLE mailing_campaign_isp_plans
			ADD COLUMN IF NOT EXISTS audience_reserve_count INTEGER NOT NULL DEFAULT 0`},
		{"reserve_pool_waves_cap_skip_col", `ALTER TABLE mailing_campaign_waves
			ADD COLUMN IF NOT EXISTS cap_skip_count INTEGER NOT NULL DEFAULT 0`},
		{"reserve_pool_waves_reserve_used_col", `ALTER TABLE mailing_campaign_waves
			ADD COLUMN IF NOT EXISTS reserve_used_count INTEGER NOT NULL DEFAULT 0`},
		// Partial index supports the dispatcher's cap-aware claim query
		// (SELECT ... WHERE isp_plan_id=$1 AND status IN ('selected','reserve')
		// ORDER BY (status='selected' DESC), selection_rank ASC). Filtering on
		// status keeps the index narrow (only rows that COULD be claimed) while
		// the (isp_plan_id, status, selection_rank) column order matches the
		// query's most-selective leading predicate.
		{"reserve_pool_claim_lookup_idx", `CREATE INDEX IF NOT EXISTS idx_plan_recipients_claim_lookup
			ON mailing_campaign_plan_recipients (isp_plan_id, status, selection_rank)
			WHERE status IN ('selected', 'reserve')`},

		// =========================================================================
		// Data Partner Ingestion System (May 2026)
		// =========================================================================
		// Forward-facing API for trusted data partners (e.g. Attribits) to inject
		// fresh subscriber records into our sending pipeline. Each partner has one
		// or more datasets (e.g. "Attribits HOME", "Attribits PERSONAL LOAN") that
		// each map to a single vertical. Records flow:
		//   inbound API -> S3 storage -> slicer (suppression check) ->
		//   validator (EmailOversight) -> ready_queue -> drip orchestrator
		//   (15-min mini-campaigns round-robin across DB/HT/MH/QF brands).
		// All tables idempotent. See docs in plan: data-partner-ingestion-system.
		// -------------------------------------------------------------------------
		{"dp_create_data_partners", `CREATE TABLE IF NOT EXISTS data_partners (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'::uuid,
			name TEXT NOT NULL UNIQUE,
			slug TEXT NOT NULL UNIQUE,
			contact_email TEXT,
			status TEXT NOT NULL DEFAULT 'active',
			notes TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"dp_create_partner_datasets", `CREATE TABLE IF NOT EXISTS partner_datasets (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			partner_id UUID NOT NULL REFERENCES data_partners(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			slug TEXT NOT NULL,
			vertical TEXT NOT NULL CHECK (vertical IN ('refi_heloc','personal_loans','tax_relief','remodel')),
			flush_window_hours INTEGER NOT NULL DEFAULT 24,
			min_wave_size INTEGER NOT NULL DEFAULT 25,
			max_wave_size INTEGER NOT NULL DEFAULT 5000,
			paused_emergency BOOLEAN NOT NULL DEFAULT false,
			paused_reason TEXT,
			paused_at TIMESTAMPTZ,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (partner_id, slug)
		)`},
		{"dp_create_partner_api_keys", `CREATE TABLE IF NOT EXISTS partner_api_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			partner_id UUID NOT NULL REFERENCES data_partners(id) ON DELETE CASCADE,
			dataset_id UUID NOT NULL REFERENCES partner_datasets(id) ON DELETE CASCADE,
			key_hash VARCHAR(64) NOT NULL UNIQUE,
			key_prefix VARCHAR(16) NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			last_used_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			revoked_at TIMESTAMPTZ
		)`},
		{"dp_idx_partner_api_keys_hash", `CREATE INDEX IF NOT EXISTS idx_partner_api_keys_hash ON partner_api_keys(key_hash) WHERE status = 'active'`},
		{"dp_create_partner_inbound_batches", `CREATE TABLE IF NOT EXISTS partner_inbound_batches (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			dataset_id UUID NOT NULL REFERENCES partner_datasets(id) ON DELETE CASCADE,
			partner_id UUID NOT NULL REFERENCES data_partners(id) ON DELETE CASCADE,
			s3_bucket TEXT NOT NULL,
			s3_key TEXT NOT NULL,
			record_count INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'received',
			emergency_stopped BOOLEAN NOT NULL DEFAULT false,
			next_record_offset INTEGER NOT NULL DEFAULT 0,
			received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			slicing_started_at TIMESTAMPTZ,
			slicing_completed_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			last_error TEXT,
			ingest_metadata JSONB NOT NULL DEFAULT '{}'::jsonb
		)`},
		{"dp_idx_pib_dataset_status", `CREATE INDEX IF NOT EXISTS idx_pib_dataset_status ON partner_inbound_batches(dataset_id, status, received_at)`},
		{"dp_idx_pib_status_received", `CREATE INDEX IF NOT EXISTS idx_pib_status_received ON partner_inbound_batches(status, received_at) WHERE status IN ('received','slicing','slicing_complete','validating')`},
		{"dp_create_partner_clean_queue", `CREATE TABLE IF NOT EXISTS partner_clean_queue (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			batch_id UUID NOT NULL REFERENCES partner_inbound_batches(id) ON DELETE CASCADE,
			dataset_id UUID NOT NULL REFERENCES partner_datasets(id) ON DELETE CASCADE,
			partner_id UUID NOT NULL REFERENCES data_partners(id) ON DELETE CASCADE,
			vertical TEXT NOT NULL,
			email TEXT NOT NULL,
			email_md5 VARCHAR(32) NOT NULL,
			isp_family TEXT NOT NULL DEFAULT 'other',
			status TEXT NOT NULL DEFAULT 'pending_eo',
			eo_result_code INTEGER,
			eo_result TEXT,
			eo_attempts INTEGER NOT NULL DEFAULT 0,
			mailed_campaign_id UUID,
			mailed_brand TEXT,
			ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			validated_at TIMESTAMPTZ,
			claimed_at TIMESTAMPTZ,
			mailed_at TIMESTAMPTZ,
			extra_metadata JSONB NOT NULL DEFAULT '{}'::jsonb
		)`},
		{"dp_idx_pcq_batch", `CREATE INDEX IF NOT EXISTS idx_pcq_batch ON partner_clean_queue(batch_id)`},
		{"dp_idx_pcq_dataset_status_ingested", `CREATE INDEX IF NOT EXISTS idx_pcq_dataset_status_ingested ON partner_clean_queue(dataset_id, status, ingested_at)`},
		{"dp_idx_pcq_status_ready", `CREATE INDEX IF NOT EXISTS idx_pcq_status_ready ON partner_clean_queue(vertical, status, ingested_at) WHERE status = 'ready'`},
		{"dp_idx_pcq_status_pending_eo", `CREATE INDEX IF NOT EXISTS idx_pcq_status_pending_eo ON partner_clean_queue(ingested_at) WHERE status = 'pending_eo'`},
		{"dp_idx_pcq_isp_family", `CREATE INDEX IF NOT EXISTS idx_pcq_isp_family ON partner_clean_queue(dataset_id, isp_family, status)`},
		{"dp_idx_pcq_email_md5", `CREATE INDEX IF NOT EXISTS idx_pcq_email_md5 ON partner_clean_queue(email_md5)`},
		{"dp_create_partner_drip_state", `CREATE TABLE IF NOT EXISTS partner_drip_state (
			vertical TEXT PRIMARY KEY,
			next_brand_index INTEGER NOT NULL DEFAULT 0,
			last_wave_at TIMESTAMPTZ,
			last_wave_campaign_id UUID,
			last_wave_brand TEXT,
			last_wave_size INTEGER,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"dp_seed_partner_drip_state", `INSERT INTO partner_drip_state (vertical, next_brand_index)
			VALUES ('refi_heloc', 0), ('personal_loans', 1), ('tax_relief', 2), ('remodel', 3)
			ON CONFLICT (vertical) DO NOTHING`},
		{"dp_create_partner_drip_creatives", `CREATE TABLE IF NOT EXISTS partner_drip_creatives (
			vertical TEXT NOT NULL,
			brand TEXT NOT NULL,
			creative_filename TEXT NOT NULL,
			subject_line TEXT NOT NULL,
			preheader TEXT NOT NULL DEFAULT '',
			from_name TEXT NOT NULL,
			active BOOLEAN NOT NULL DEFAULT true,
			effective_from TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_by TEXT,
			PRIMARY KEY (vertical, brand)
		)`},
		{"dp_create_partner_drip_copy_lines", `CREATE TABLE IF NOT EXISTS partner_drip_copy_lines (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			vertical TEXT NOT NULL,
			line_kind TEXT NOT NULL CHECK (line_kind IN ('subject', 'preheader')),
			copy_text TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0,
			active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"dp_idx_partner_drip_copy_lines", `CREATE INDEX IF NOT EXISTS idx_partner_drip_copy_lines_vertical_kind
			ON partner_drip_copy_lines(vertical, line_kind) WHERE active = true`},
		{"dp_copy_lines_from_name_kind", `
			DO $$ BEGIN
				ALTER TABLE partner_drip_copy_lines
					DROP CONSTRAINT IF EXISTS partner_drip_copy_lines_line_kind_check;
			EXCEPTION WHEN undefined_object THEN NULL;
			END $$;
			ALTER TABLE partner_drip_copy_lines
				ADD CONSTRAINT partner_drip_copy_lines_line_kind_check
				CHECK (line_kind IN ('subject', 'preheader', 'from_name'))`},
		// Seed all 16 (vertical, brand) creative rows. The 05132026 dated files
		// exist in docs/emails/ — verified during plan phase. Brand short codes:
		// db = Discount Blog, ht = History Thinking, mh = My Own Health, qf = Quiz Fiesta.
		// From-names match the brand voices used in deploy_may12_mature.py.
		{"dp_seed_creative_refi_db", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('refi_heloc', 'db', 'amerisave-db-newsletter-05132026.html',
				'Lock in todays HELOC rate before the next Fed move',
				'Tap your home equity at rates that are still historically low — see your personalized offer.',
				'Jamie @ Discount Blog')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_refi_ht", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('refi_heloc', 'ht', 'amerisave-ht-newsletter-05132026.html',
				'A history of low rates — and what it means for you',
				'Homeowners who refinance now could save thousands. See what AmeriSave can do.',
				'History Thinking')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_refi_mh", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('refi_heloc', 'mh', 'amerisave-mh-newsletter-05132026.html',
				'Your home equity could be working harder for you',
				'Homeowners are unlocking cash for repairs, healthcare bills, and family. See your offer.',
				'Arnold @ My Own Health')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_refi_qf", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('refi_heloc', 'qf', 'amerisave-qf-newsletter-05132026.html',
				'Did you know your home equity could fund this?',
				'Most homeowners do not realize how much they can borrow. AmeriSave will show you in 60 seconds.',
				'Quiz Master')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_pl_db", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('personal_loans', 'db', 'personal-loans-db-newsletter-05132026.html',
				'A personal loan from $1,000 to $100,000 — see your rate',
				'No collateral. No prepayment penalties. Pre-qualified offers in under 2 minutes.',
				'Jamie @ Discount Blog')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_pl_ht", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('personal_loans', 'ht', 'personal-loans-ht-newsletter-05132026.html',
				'Pay down high-interest debt the smart way',
				'A personal loan can consolidate credit cards into one fixed monthly payment.',
				'History Thinking')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_pl_mh", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('personal_loans', 'mh', 'personal-loans-mh-newsletter-05132026.html',
				'Cover the bills that life keeps stacking up',
				'A personal loan can consolidate medical bills, dental work, and emergency expenses.',
				'Arnold @ My Own Health')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_pl_qf", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('personal_loans', 'qf', 'personal-loans-qf-newsletter-05132026.html',
				'How much could you borrow? Take the 60-second quiz',
				'Most folks do not know the rate they qualify for. Find out without a hard pull.',
				'Quiz Master')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_tax_db", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('tax_relief', 'db', 'optima-tax-relief-db-newsletter-05132026.html',
				'Owe the IRS more than $10K? You may qualify for relief',
				'Optima Tax Relief has resolved over $1 billion in tax debt. See if you qualify.',
				'Jamie @ Discount Blog')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_tax_ht", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('tax_relief', 'ht', 'optima-tax-relief-ht-newsletter-05132026.html',
				'A short history of IRS amnesty programs',
				'You may qualify for an Offer in Compromise — see how Optima Tax Relief can help.',
				'History Thinking')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_tax_mh", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('tax_relief', 'mh', 'optima-tax-relief-mh-newsletter-05132026.html',
				'Tax debt stress affects your health — here is the way out',
				'Optima Tax Relief negotiates with the IRS so you can sleep at night.',
				'Arnold @ My Own Health')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_tax_qf", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('tax_relief', 'qf', 'optima-tax-relief-qf-newsletter-05132026.html',
				'IRS Quiz: how much could Optima reduce your tax debt?',
				'Take the 60-second qualification quiz from Optima Tax Relief. No commitment.',
				'Quiz Master')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_remodel_db", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('remodel', 'db', 'renewal-by-andersen-db-newsletter-05132026.html',
				'New windows that pay you back month after month',
				'Renewal by Andersen windows can cut energy bills and refresh your home.',
				'Jamie @ Discount Blog')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_remodel_ht", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('remodel', 'ht', 'renewal-by-andersen-ht-newsletter-05132026.html',
				'Window technology has come a long way — see what is new',
				'Renewal by Andersen offers a free in-home consultation. No obligation.',
				'History Thinking')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_remodel_mh", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('remodel', 'mh', 'renewal-by-andersen-mh-newsletter-05132026.html',
				'Drafty old windows are making your home work harder',
				'Renewal by Andersen energy-efficient windows can lower your bills and improve comfort.',
				'Arnold @ My Own Health')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_seed_creative_remodel_qf", `INSERT INTO partner_drip_creatives (vertical, brand, creative_filename, subject_line, preheader, from_name)
			VALUES ('remodel', 'qf', 'renewal-by-andersen-qf-newsletter-05132026.html',
				'Quiz: how much could new windows save you each year?',
				'Take the 60-second quiz from Renewal by Andersen and find out.',
				'Quiz Master')
			ON CONFLICT (vertical, brand) DO NOTHING`},
		{"dp_create_partner_isp_distribution_overrides", `CREATE TABLE IF NOT EXISTS partner_isp_distribution_overrides (
			dataset_id UUID NOT NULL REFERENCES partner_datasets(id) ON DELETE CASCADE,
			isp TEXT NOT NULL,
			pct_override DECIMAL(5,4) NOT NULL,
			max_per_wave INTEGER,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_by TEXT,
			PRIMARY KEY (dataset_id, isp)
		)`},
		{"dp_create_partner_admin_audit_log", `CREATE TABLE IF NOT EXISTS partner_admin_audit_log (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT NOT NULL,
			before_state JSONB,
			after_state JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"dp_idx_audit_target", `CREATE INDEX IF NOT EXISTS idx_dp_audit_target ON partner_admin_audit_log(target_type, target_id, created_at DESC)`},
		// Partner-drip campaign tag column on mailing_campaigns so we can find / archive
		// the mini-campaigns later. NULL for non-partner campaigns; "data_partner_<slug>"
		// for partner-drip waves. Cheap, indexed, no FK so we can keep the column even
		// after a partner is deleted.
		{"dp_add_campaign_partner_tag", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS partner_drip_tag TEXT`},
		{"dp_idx_campaign_partner_tag", `CREATE INDEX IF NOT EXISTS idx_mc_partner_drip_tag ON mailing_campaigns(partner_drip_tag, scheduled_at) WHERE partner_drip_tag IS NOT NULL`},
		// partner_dataset_id: the dataset FK stamped alongside partner_drip_tag by
		// stampPartnerAttributionOnCampaign (orchestrator). It already exists in
		// production (added out-of-band) but was never codified — without it the
		// orchestrator's combined UPDATE (sets BOTH columns) errors and writes
		// NEITHER tag, breaking the Campaign Center partner-drip rollup. Codified
		// idempotently so fresh / local environments match prod.
		{"dp_add_campaign_partner_dataset_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS partner_dataset_id UUID`},

		// Data Partners Master List — every record promoted from partner_clean_queue
		// becomes a mailing_subscribers row attached to this list. Per-vertical
		// segmentation happens via per-wave static segments built by the drip
		// orchestrator. Fixed UUID so the orchestrator can reference it without
		// a separate lookup. Idempotent: already exists rows are left alone.
		{"dp_seed_master_list", `INSERT INTO mailing_lists (id, organization_id, name, description, status)
			SELECT '00000000-0000-0000-0000-0000d4ada4a7'::uuid,
			       '00000000-0000-0000-0000-000000000001'::uuid,
			       'Data Partners Master',
			       'Auto-managed list for inbound data partner records. Subscribers populated by partner_drip_orchestrator. Do not edit manually.',
			       'active'
			WHERE NOT EXISTS (SELECT 1 FROM mailing_lists WHERE id = '00000000-0000-0000-0000-0000d4ada4a7'::uuid)`},

		// =====================================================================
		// May 12 2026: WF pool reassert — final-position migration that runs
		// after every downstream mh-* revert (phase10_ipxo_yh_redistribute,
		// phase12_ipxo_out_of_yahoo, phase19_fix_ipxo_yh_hostnames).
		//
		// MUST live at the END of this slice (runStartupMigrations) because
		// production ECS does not set DB_ADMIN_URL → runAdminMigrations is
		// a silent no-op there. Original placement in runAdminMigrations
		// (commit 4e11010) silently failed against production; hot-fixed
		// 2026-05-12 evening to move into runStartupMigrations after the
		// initial deploy verification caught the .202/.203/.205 IPs still
		// routed to mh-* pools.
		//
		// The original may09b_reassign_wf_harvested_ips (line ~3541) is
		// silently reverted on every boot because phase10/12/19 re-anchor
		// 144.225.178.{202,203,205} back to mh-apple/mh-comcast/mh-charter.
		// This migration runs last so it wins the revert race and keeps
		// DB state aligned with PMTA reality (Server B has the IPs renamed
		// to mta-wf-gn[1-3] since 2026-05-09). Donor-pool impact: mh-apple
		// retains 6 active IPs after .202 leaves; .203/.205 already cold
		// in mh-comcast/mh-charter from May 8 Cloudmark trim.
		// =====================================================================
		{"may12_wf_pool_reassert", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    wf_pool_id UUID;
    rec RECORD;
BEGIN
    SELECT id INTO wf_pool_id
      FROM mailing_ip_pools
     WHERE name = 'wf-general-pool' AND organization_id = org_id;

    IF wf_pool_id IS NULL THEN
        RETURN;
    END IF;

    FOR rec IN
        SELECT * FROM (VALUES
            ('144.225.178.202', 'mta-wf-gn1.mail.em.warrantyforyou.com'),
            ('144.225.178.203', 'mta-wf-gn2.mail.em.warrantyforyou.com'),
            ('144.225.178.205', 'mta-wf-gn3.mail.em.warrantyforyou.com')
        ) AS t(ip_addr, hostname)
    LOOP
        UPDATE mailing_ip_addresses
           SET pool_id = wf_pool_id,
               hostname = rec.hostname,
               status = 'active',
               warmup_stage = COALESCE(NULLIF(warmup_stage, ''), 'engaged'),
               updated_at = NOW()
         WHERE ip_address = rec.ip_addr::inet;
    END LOOP;
END $$`},

		// =====================================================================
		// May 29 2026: segment catalog cleanup — Phase A (schema)
		//
		// Adds a `category` column to mailing_segments so the wizard and
		// dashboard can filter the catalog by semantic type instead of
		// scrolling 11k+ unstructured rows. Companion frontend work in
		// PMTACampaignWizard.tsx and ListPortal.tsx surfaces filters and
		// badges off this field. Backfill is name-pattern driven and
		// idempotent — only touches rows still flagged 'uncategorized'.
		//
		// Source-of-truth enum (keep in sync with seg_category_metadata.ts
		// in upside-down/web): engagement_brand, engagement_global,
		// engagement_isp, framework, funnel, cohort_static,
		// suppression_exclusion, partner_wave_static, legacy_snapshot,
		// uncategorized. Pre-deploy direct API build of 102 engagement
		// segments lives in .scratch/may29_seed_engagement_segments.py.
		// =====================================================================
		{"may29_seg_add_category_col", `ALTER TABLE mailing_segments
			ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT 'uncategorized'`},
		{"may29_seg_backfill_engagement_brand", `UPDATE mailing_segments
			SET category = 'engagement_brand'
			WHERE category = 'uncategorized'
			  AND name ~ '^[A-Z]{2,3} (7|14|30|60)D (Openers|Clickers)$'`},
		{"may29_seg_backfill_engagement_global", `UPDATE mailing_segments
			SET category = 'engagement_global'
			WHERE category = 'uncategorized'
			  AND name ~ '^Global (7|14|30|60)D (Openers|Clickers)$'`},
		{"may29_seg_backfill_engagement_isp", `UPDATE mailing_segments
			SET category = 'engagement_isp'
			WHERE category = 'uncategorized'
			  AND name ~ '^(Discount Blog|Quiz Fiesta|History Thinking|My Own Health|Business Weekly Pro|Financial Calculate|Consumer Pro|Home Warranty Services|Refinance Rates USA|Thing of the Day|Your Insurance Hub|My Repair DIY|Casa Insure|Learn Personal Loans|Rates Bazar|Warranty For You) - .* - (Openers|Clickers)$'`},
		// Jun 4 2026: vertical (data-provenance) engagement segments. These are
		// named "{Vertical Label} {N}D {Openers|Clickers}" and are created by
		// seed_vertical_engagement_segments.py with category set explicitly;
		// this backfill is an idempotent safety net for any that arrive
		// uncategorized. Vertical labels come from vertical_metadata.py. The
		// 2-3 uppercase-letter brand rule above cannot match these (vertical
		// labels are mixed-case words), so there is no collision.
		{"jun04_seg_backfill_engagement_vertical", `UPDATE mailing_segments
			SET category = 'engagement_vertical'
			WHERE category = 'uncategorized'
			  AND name ~ '^(Mortgage|Finance|Lawn Care|Remodel|Tax Relief) (7|14|30|60)D (Openers|Clickers)$'`},
		{"may29_seg_backfill_framework", `UPDATE mailing_segments
			SET category = 'framework'
			WHERE category = 'uncategorized'
			  AND name LIKE 'Framework-%'`},
		{"may29_seg_backfill_funnel", `UPDATE mailing_segments
			SET category = 'funnel'
			WHERE category = 'uncategorized'
			  AND name ~ ' W\d - .*funnel'`},
		{"may29_seg_backfill_cohort_static", `UPDATE mailing_segments
			SET category = 'cohort_static'
			WHERE category = 'uncategorized'
			  AND (name LIKE 'Converters-%'
			    OR name LIKE 'WCM-%'
			    OR name LIKE 'List-WCM-%'
			    OR name LIKE 'Vertical-%'
			    OR name LIKE 'Master%'
			    OR name LIKE 'Mailgun-Engaged-%')`},
		{"may29_seg_backfill_suppression_exclusion", `UPDATE mailing_segments
			SET category = 'suppression_exclusion'
			WHERE category = 'uncategorized'
			  AND (name LIKE 'Welcome-Saturated%'
			    OR name LIKE '% Cold Remail Guard%'
			    OR name ~ '^[A-Z]{2,3} Sent \d+D ')`},
		{"may29_seg_backfill_partner_wave", `UPDATE mailing_segments
			SET category = 'partner_wave_static'
			WHERE category = 'uncategorized'
			  AND name LIKE 'data-partner-wave-%'`},
		{"may29_seg_backfill_legacy_snapshot", `UPDATE mailing_segments
			SET category = 'legacy_snapshot'
			WHERE category = 'uncategorized'
			  AND (name ~ '^Global ISP-Targeted Engaged .* \(\d{4}-\d{2}-\d{2}\)$'
			    OR name ~ '^[A-Z]{2,3} ISP-Targeted Engaged '
			    OR name ~ '^(Apr|May)\d')`},
		{"may29_seg_category_index", `CREATE INDEX IF NOT EXISTS idx_segments_org_category_status
			ON mailing_segments(organization_id, category, status)`},

		// =====================================================================
		// May 29 2026: segment catalog cleanup — Phase D (archive stale)
		//
		// Archives partner_wave_static and legacy_snapshot segments older than
		// 7 days that have no non-terminal referencing campaigns. The
		// partner_drip_orchestrator (worker/partner_drip_orchestrator.go:1174)
		// creates one-shot static segments per wave but never cleans them up,
		// which is why production has 11k+ segments. We archive (status='archived'),
		// never delete, so the catalog stays manageable and rows remain available
		// for historical audit.
		//
		// Safety: we only archive if no non-terminal campaign references the
		// segment as its primary target (mailing_campaigns.segment_id) or in
		// its pmta_config->'campaign_input' JSONB inclusion_segments /
		// exclusion_segments arrays. Terminal statuses ('sent','cancelled',
		// 'failed') don't matter for future scheduling.
		// =====================================================================
		{"may29_seg_archive_stale_partner_waves", `UPDATE mailing_segments s
			SET status = 'archived', updated_at = NOW()
			WHERE s.status NOT IN ('archived','inactive')
			  AND s.category = 'partner_wave_static'
			  AND s.created_at < NOW() - INTERVAL '7 days'
			  AND NOT EXISTS (
			    SELECT 1 FROM mailing_campaigns c
			    WHERE c.status IN ('scheduled','sending','preparing','finalizing_audience','paused')
			      AND (
			        c.segment_id = s.id
			        OR c.pmta_config->'campaign_input'->'inclusion_segments' @> to_jsonb(s.id::text)
			        OR c.pmta_config->'campaign_input'->'exclusion_segments' @> to_jsonb(s.id::text)
			      )
			  )`},
		{"may29_seg_archive_stale_legacy_snapshots", `UPDATE mailing_segments s
			SET status = 'archived', updated_at = NOW()
			WHERE s.status NOT IN ('archived','inactive')
			  AND s.category = 'legacy_snapshot'
			  AND s.created_at < NOW() - INTERVAL '7 days'
			  AND NOT EXISTS (
			    SELECT 1 FROM mailing_campaigns c
			    WHERE c.status IN ('scheduled','sending','preparing','finalizing_audience','paused')
			      AND (
			        c.segment_id = s.id
			        OR c.pmta_config->'campaign_input'->'inclusion_segments' @> to_jsonb(s.id::text)
			        OR c.pmta_config->'campaign_input'->'exclusion_segments' @> to_jsonb(s.id::text)
			      )
			  )`},

		// ────────────────────────────────────────────────────────────────────
		// Click-Drip Journey infrastructure (Phase 1, 2026-06-01)
		// Backs the Everflow click-postback → journey enrollment pipeline.
		// All inert until Phase 3 wires the email-node metadata override and
		// seeds journey records. Safe to deploy at any time.
		// ────────────────────────────────────────────────────────────────────
		{"jun01_click_drip_offer_journey_map", `CREATE TABLE IF NOT EXISTS mailing_offer_journey_map (
			everflow_offer_id VARCHAR(64) PRIMARY KEY,
			click_journey_id  VARCHAR(64),
			payout_type       VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN',
			enabled           BOOLEAN NOT NULL DEFAULT FALSE,
			notes             TEXT DEFAULT '',
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"jun01_click_drip_offer_journey_map_payout_chk", `DO $$ BEGIN
			ALTER TABLE mailing_offer_journey_map
				ADD CONSTRAINT mailing_offer_journey_map_payout_type_check
				CHECK (payout_type IN ('CPM','eCPM','CPA','CPL','CPC','IO','PRV','UNKNOWN'));
		EXCEPTION WHEN duplicate_object THEN NULL; WHEN OTHERS THEN NULL; END $$`},

		{"jun01_click_drip_event_triggers", `CREATE TABLE IF NOT EXISTS mailing_journey_event_triggers (
			id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			source               VARCHAR(64) NOT NULL,
			everflow_offer_id    VARCHAR(64) NOT NULL,
			subscriber_id        UUID NOT NULL,
			subscriber_email     VARCHAR(255),
			sub2_brand           VARCHAR(128) DEFAULT '',
			sub3_campaign_id     VARCHAR(64) DEFAULT '',
			click_id             VARCHAR(128) DEFAULT '',
			sending_profile_id   UUID,
			sending_domain       VARCHAR(128) DEFAULT '',
			click_url            TEXT DEFAULT '',
			status               VARCHAR(32) NOT NULL DEFAULT 'pending',
			skip_reason          TEXT DEFAULT '',
			received_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			processed_at         TIMESTAMPTZ
		)`},
		{"jun01_click_drip_event_triggers_pending_idx", `CREATE INDEX IF NOT EXISTS idx_mjet_pending_received
			ON mailing_journey_event_triggers(status, received_at)
			WHERE status = 'pending'`},
		{"jun01_click_drip_event_triggers_idem_idx", `CREATE INDEX IF NOT EXISTS idx_mjet_idempotency
			ON mailing_journey_event_triggers(subscriber_id, everflow_offer_id, received_at DESC)`},

		{"jun01_click_drip_reminder_subjects", `CREATE TABLE IF NOT EXISTS mailing_offer_reminder_subjects (
			everflow_offer_id  VARCHAR(64) NOT NULL,
			sequence_index     SMALLINT NOT NULL,
			subject            TEXT NOT NULL DEFAULT '',
			preheader          TEXT DEFAULT '',
			from_name_override VARCHAR(128) DEFAULT '',
			enabled            BOOLEAN NOT NULL DEFAULT TRUE,
			notes              TEXT DEFAULT '',
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (everflow_offer_id, sequence_index)
		)`},

		{"jun01_click_drip_enrollment_exit_cols", `ALTER TABLE mailing_journey_enrollments
			ADD COLUMN IF NOT EXISTS exited_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS exit_reason TEXT,
			ADD COLUMN IF NOT EXISTS converted_at TIMESTAMPTZ,
			ADD COLUMN IF NOT EXISTS enrolled_via VARCHAR(64) DEFAULT '',
			ADD COLUMN IF NOT EXISTS enrollment_offer_id VARCHAR(64) DEFAULT ''`},
		{"jun01_click_drip_enrollment_offer_idx", `CREATE INDEX IF NOT EXISTS idx_journey_enrollments_active_by_offer
			ON mailing_journey_enrollments(enrollment_offer_id, status)
			WHERE status = 'active' AND enrollment_offer_id <> ''`},

		// ────────────────────────────────────────────────────────────────────
		// Click-Drip Journey seed data (Phase 3, 2026-06-01)
		//
		// One shared journey graph (4 emails at +1h, +6h, +24h, +72h after
		// click) + two offer→journey map rows + 8 starter subject lines.
		//
		// Metal Roofing (9539) ships enabled=TRUE (operator-approved go-live
		// per send-day brief). NDR (7667) ships enabled=FALSE so operator
		// can flip it after Metal Roofing soaks 24h:
		//   UPDATE mailing_offer_journey_map SET enabled=true WHERE everflow_offer_id='7667';
		// ────────────────────────────────────────────────────────────────────
		{"jun01_click_drip_journey_4touch_72h", `INSERT INTO mailing_journeys (id, name, description, status, nodes, connections, created_at, updated_at)
			VALUES (
				'click-drip-4touch-72h',
				'Click-Drip 4-Touch (+1h, +6h, +24h, +72h)',
				'Standard click-drip journey: subscriber clicks an offer, 4 reminder emails fire over 72 hours, all from the original brand domain reusing the original creative. Subjects per-offer in mailing_offer_reminder_subjects.',
				'active',
				'[
					{"id":"trig","type":"trigger","config":{"triggerType":"event","eventType":"click_postback"},"connections":["delay-1h"]},
					{"id":"delay-1h","type":"delay","config":{"delayValue":1,"delayUnit":"hours"},"connections":["email-0"]},
					{"id":"email-0","type":"email","config":{"reminder_sequence_index":0,"subject":"","html_content":""},"connections":["delay-5h"]},
					{"id":"delay-5h","type":"delay","config":{"delayValue":5,"delayUnit":"hours"},"connections":["email-1"]},
					{"id":"email-1","type":"email","config":{"reminder_sequence_index":1,"subject":"","html_content":""},"connections":["delay-18h"]},
					{"id":"delay-18h","type":"delay","config":{"delayValue":18,"delayUnit":"hours"},"connections":["email-2"]},
					{"id":"email-2","type":"email","config":{"reminder_sequence_index":2,"subject":"","html_content":""},"connections":["delay-48h"]},
					{"id":"delay-48h","type":"delay","config":{"delayValue":48,"delayUnit":"hours"},"connections":["email-3"]},
					{"id":"email-3","type":"email","config":{"reminder_sequence_index":3,"subject":"","html_content":""},"connections":["goal"]},
					{"id":"goal","type":"goal","config":{},"connections":[]}
				]'::jsonb,
				'[
					{"from":"trig","to":"delay-1h"},
					{"from":"delay-1h","to":"email-0"},
					{"from":"email-0","to":"delay-5h"},
					{"from":"delay-5h","to":"email-1"},
					{"from":"email-1","to":"delay-18h"},
					{"from":"delay-18h","to":"email-2"},
					{"from":"email-2","to":"delay-48h"},
					{"from":"delay-48h","to":"email-3"},
					{"from":"email-3","to":"goal"}
				]'::jsonb,
				NOW(), NOW()
			) ON CONFLICT (id) DO NOTHING`},

		{"jun01_click_drip_map_metalroofing", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('9539', 'click-drip-4touch-72h', 'CPM', TRUE, 'Metal Roofing - first click-drip live offer (2026-06-01)')
			ON CONFLICT (everflow_offer_id) DO NOTHING`},

		{"jun01_click_drip_map_ndr", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('7667', 'click-drip-4touch-72h', 'eCPM', FALSE, 'National Debt Relief - paused at launch; operator flips enabled=true after Metal Roofing 24h soak')
			ON CONFLICT (everflow_offer_id) DO NOTHING`},

		// ────────────────────────────────────────────────────────────────────
		// Click-Drip internal-click trigger: cratoolpro slug → everflow offer id
		// (2026-06-02).
		//
		// Everflow's per-offer click postbacks don't reliably reach us (they
		// fire on conversion + macro substitution is flaky), so real clicks never
		// entered the drip. The JourneyClickTrackingEnroller worker instead reads
		// internal click events (mailing_tracking_events) and resolves the offer
		// from the cratoolpro money-URL's trailing slug
		// (https://www.cratoolpro.com/BJB4Q5BF/<SLUG>/). This table is that
		// slug → everflow_offer_id dictionary.
		//
		// Slugs verified 2026-06-02 from deploy scripts, live review-forge
		// creatives, and the operator's authoritative map. NDR uses GK847MZ
		// (shared with the 7412 partner-drip link); we point it at the journey's
		// 7667 entry per operator direction ("keep true to the dictionary").
		// Operator can correct any row with a single UPDATE — no redeploy.
		// ────────────────────────────────────────────────────────────────────
		{"jun02_click_drip_offer_slug_map", `CREATE TABLE IF NOT EXISTS mailing_offer_slug_map (
			cratoolpro_slug   VARCHAR(64) PRIMARY KEY,
			everflow_offer_id VARCHAR(64) NOT NULL,
			offer_name        TEXT DEFAULT '',
			enabled           BOOLEAN NOT NULL DEFAULT TRUE,
			notes             TEXT DEFAULT '',
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"jun02_click_drip_offer_slug_map_offer_idx", `CREATE INDEX IF NOT EXISTS idx_offer_slug_map_everflow
			ON mailing_offer_slug_map(everflow_offer_id)`},
		{"jun02_click_drip_offer_slug_seed", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('KW3Q1DJ', '9539', 'Get Metal Roofing',     TRUE,  'creative_id 643104; operator-confirmed 2026-06-02'),
			('K62P438', '9135', 'Affordable Windows USA', TRUE,  'creative_id 153833; operator-confirmed 2026-06-02'),
			('CL38PFR', '5990', 'CarShield Auto Warranty',TRUE,  'deploy script map 2026-06-02'),
			('J876SLX', '8614', 'AmeriSave HELOC',        TRUE,  'deploy script + Go offers catalog'),
			('J345SSD', '8511', 'Optima Tax Relief',      TRUE,  'deploy script map 2026-06-02'),
			('93W8N2N', '4575', 'Quicken Loans',          TRUE,  'deploy script + Go offers catalog'),
			('GK847MZ', '7667', 'National Debt Relief',   TRUE,  'NDR creatives use GK847MZ (also 7412 partner-drip); mapped to journey 7667 per operator 2026-06-02'),
			('7N8NS1K', '3776', 'Renewal by Andersen',    TRUE,  'Apr-29 operator map; appears as Quicken cross-sell; low volume')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun02_click_drip_enroller_cursor", `CREATE TABLE IF NOT EXISTS mailing_clickdrip_enroller_cursor (
			id             SMALLINT PRIMARY KEY DEFAULT 1,
			last_event_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (id = 1)
		)`},

		// Metal Roofing reminder subjects (4 steps, sequence_index 0..3)
		{"jun01_click_drip_subjects_9539_0", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9539', 0, 'You looked at metal roofing — quick reminder', 'Your luxury roofing quote is just one click away.', TRUE, '+1h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun01_click_drip_subjects_9539_1", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9539', 1, 'Did you forget about your roofing quote?', 'A few neighbors locked theirs in this week.', TRUE, '+6h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun01_click_drip_subjects_9539_2", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9539', 2, 'Last chance for free roofing inspection', 'Same offer, one-day window left.', TRUE, '+24h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun01_click_drip_subjects_9539_3", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9539', 3, 'Final reminder: your roofing quote is waiting', 'Closing this out — last touch before we move on.', TRUE, '+72h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},

		// NDR reminder subjects (4 steps; rows seeded but offer_journey_map is enabled=FALSE)
		{"jun01_click_drip_subjects_7667_0", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('7667', 0, 'Quick follow-up on your debt relief inquiry', 'A specialist can call you back today.', TRUE, '+1h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun01_click_drip_subjects_7667_1", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('7667', 1, 'Did you finish reviewing your debt options?', 'No-pressure 5-minute review of what you saw earlier.', TRUE, '+6h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun01_click_drip_subjects_7667_2", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('7667', 2, 'Your debt relief consultation is still available', 'Pre-qualification takes about 90 seconds.', TRUE, '+24h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun01_click_drip_subjects_7667_3", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('7667', 3, 'Final note about your debt relief application', 'Closing this out — last touch before we move on.', TRUE, '+72h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},

		// Sam's Club click-drip (2026-06-05): partner drip uses eos57ytf affiliate
		// links (https://www.eos57ytf.com/K4C5ZLC/PS8241/) instead of cratoolpro.
		// PS8241 is the affiliate URL slug; Everflow network offer id is 420 (operator-confirmed).
		// Internal offer cc108c5b-14ba-56c8-ad03-64d97a440f14.
		{"jun05_click_drip_slug_samsclub", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('PS8241', '420', 'Sam''s Club Membership', TRUE, 'eos57ytf.com/K4C5ZLC/PS8241; Everflow offer 420 (not 8241 — that is the URL slug)')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun05_click_drip_map_samsclub", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('420', 'click-drip-4touch-72h', 'CPA', TRUE, 'Sam''s Club Membership - partner drip; Everflow offer 420')
			ON CONFLICT (everflow_offer_id) DO NOTHING`},
		{"jun05_click_drip_subjects_420_0", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('420', 0, 'You looked at Sam''s Club membership — quick reminder', 'Member savings on fuel, groceries, and more are one click away.', TRUE, '+1h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun05_click_drip_subjects_420_1", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('420', 1, 'Did you finish signing up for Sam''s Club?', 'Your membership offer is still waiting — no pressure.', TRUE, '+6h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun05_click_drip_subjects_420_2", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('420', 2, 'Your Sam''s Club membership offer is still available', 'Join today and start saving at select locations.', TRUE, '+24h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun05_click_drip_subjects_420_3", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('420', 3, 'Final reminder: your Sam''s Club membership', 'Closing this out — last touch before we move on.', TRUE, '+72h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},

		// Correct the 8241 mis-map (inferred from PS8241 URL slug, not Everflow offer id).
		{"jun05_fix_samsclub_ef_offer_id_slug", `UPDATE mailing_offer_slug_map
			SET everflow_offer_id='420', notes='eos57ytf.com/K4C5ZLC/PS8241; Everflow offer 420 (PS8241 is URL slug only)', updated_at=NOW()
			WHERE cratoolpro_slug='PS8241' AND everflow_offer_id='8241'`},
		{"jun05_fix_samsclub_ef_offer_id_offers", `UPDATE mailing_offers
			SET everflow_offer_id='420', updated_at=NOW()
			WHERE id='cc108c5b-14ba-56c8-ad03-64d97a440f14' AND everflow_offer_id='8241'`},
		{"jun05_fix_samsclub_ef_offer_id_journey_del", `DELETE FROM mailing_offer_journey_map WHERE everflow_offer_id='8241'`},
		{"jun05_fix_samsclub_ef_offer_id_journey_ins", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('420', 'click-drip-4touch-72h', 'CPA', TRUE, 'Sam''s Club Membership - partner drip; Everflow offer 420')
			ON CONFLICT (everflow_offer_id) DO UPDATE SET
				click_journey_id=EXCLUDED.click_journey_id,
				payout_type=EXCLUDED.payout_type,
				enabled=EXCLUDED.enabled,
				notes=EXCLUDED.notes,
				updated_at=NOW()`},
		{"jun05_fix_samsclub_ef_offer_id_subjects_upd", `UPDATE mailing_offer_reminder_subjects
			SET everflow_offer_id='420', updated_at=NOW()
			WHERE everflow_offer_id='8241'`},

		// Consumer-signal redirect (2026-06-06): a click on a "consumer signal"
		// offer (Warby Parker / TruGreen) marks the subscriber as a buyer and
		// funnels them into a DIFFERENT offer's click-drip. Operator directive:
		// Warby Parker (slug K5C8PQQ) and TruGreen (slug BXPFT55) clickers enter
		// the Sam's Club (Everflow offer 420) drip.
		//
		// Why a separate table (not just a mailing_offer_slug_map row): a plain
		// slug→420 map row would tag the trigger as offer 420 but leave
		// sub3_campaign_id pointing at the WARBY/TRUGREEN campaign, so the
		// reminder body (journey_event_enroller reads body_html from sub3) would
		// be the Warby creative under a Sam's Club subject — wrong offer pitched,
		// wrong money link. The redirect therefore needs BOTH the target offer id
		// AND a way to resolve the clicker's brand Sam's Club creative.
		// target_campaign_name_ilike is the name pattern used to find that brand's
		// most recent target-offer campaign (Sam's Club campaigns are NOT
		// offer_id-linked in prod, so name match is the only signal).
		{"jun06_consumer_signal_slugs_table", `CREATE TABLE IF NOT EXISTS mailing_consumer_signal_slugs (
			slug                       VARCHAR(64) PRIMARY KEY,
			target_everflow_offer_id   VARCHAR(64) NOT NULL,
			target_campaign_name_ilike TEXT NOT NULL,
			label                      TEXT DEFAULT '',
			enabled                    BOOLEAN NOT NULL DEFAULT TRUE,
			notes                      TEXT DEFAULT '',
			created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"jun06_consumer_signal_slugs_seed", `INSERT INTO mailing_consumer_signal_slugs
			(slug, target_everflow_offer_id, target_campaign_name_ilike, label, enabled, notes) VALUES
			('K5C8PQQ', '420', '%sam%', 'Warby Parker', TRUE, 'cratoolpro slug K5C8PQQ (may23 redeploy); consumer signal → Sam''s Club offer 420 drip'),
			('BXPFT55', '420', '%sam%', 'TruGreen',     TRUE, 'cratoolpro slug BXPFT55 (trugreen newsletter creatives); consumer signal → Sam''s Club offer 420 drip')
			ON CONFLICT (slug) DO NOTHING`},

		// Click-drip lane updates (2026-06-11, operator brief):
		//   - NDR migrated to the eos57ytf network (K4C5ZLC/2HH43PB) and becomes
		//     CPA $85.00. The legacy cratoolpro GK847MZ row stays enabled so
		//     clicks on in-flight mail still drip.
		//   - Metal Roofing moves to a new cratoolpro slug (J78S2MD); legacy
		//     KW3Q1DJ stays enabled for in-flight mail.
		//   - New drip lanes: SBLI Quick Quote (9178, cratoolpro K86F3PC) and
		//     Empire Today Flooring (417791, xnonu TQ5MX18J/XF1SR2CS — first
		//     xnonu-network drip; journey_clicktracking_enroller learns the
		//     xnonu host + eos57ytf alphanumeric slugs in this same build).
		{"jun11_click_drip_slug_ndr_eos", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('2HH43PB', '7667', 'National Debt Relief', TRUE, 'eos57ytf.com/K4C5ZLC/2HH43PB — NDR migrated off cratoolpro GK847MZ 2026-06-11; CPA $85.00')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun11_click_drip_slug_metalroofing_new", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('J78S2MD', '9539', 'Get Metal Roofing', TRUE, 'new cratoolpro slug 2026-06-11; replaces KW3Q1DJ (legacy row kept enabled for in-flight mail)')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun11_click_drip_ndr_payout_cpa", `UPDATE mailing_offer_journey_map
			SET payout_type='CPA',
			    notes='National Debt Relief — eos57ytf K4C5ZLC/2HH43PB; CPA $85.00 (operator 2026-06-11)',
			    updated_at=NOW()
			WHERE everflow_offer_id='7667' AND payout_type<>'CPA'`},
		{"jun11_click_drip_ndr_offer_payout", `UPDATE mailing_offers
			SET payout=85.00, payout_type='CPA', updated_at=NOW()
			WHERE everflow_offer_id='7667'
			  AND (payout IS DISTINCT FROM 85.00 OR payout_type IS DISTINCT FROM 'CPA')`},
		{"jun11_click_drip_slug_sbli", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('K86F3PC', '9178', 'SBLI Quick Quote', TRUE, 'cratoolpro.com/BJB4Q5BF/K86F3PC; term life lead-gen (provisioned 2026-06-09, drip lane 2026-06-11)')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun11_click_drip_slug_empire", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('XF1SR2CS', '417791', 'Empire Today Flooring', TRUE, 'xnonu.com/TQ5MX18J/XF1SR2CS; 60% off sale ends 6/29 (drip lane 2026-06-11)')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun11_click_drip_map_sbli", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('9178', 'click-drip-4touch-72h', 'CPL', TRUE, 'SBLI Quick Quote — term life lead-gen; drip lane added 2026-06-11')
			ON CONFLICT (everflow_offer_id) DO NOTHING`},
		{"jun11_click_drip_map_empire", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('417791', 'click-drip-4touch-72h', 'CPA', TRUE, 'Empire Today Flooring — xnonu network; CPA $180.00 (operator 2026-06-11); 60% off ends 6/29')
			ON CONFLICT (everflow_offer_id) DO NOTHING`},
		{"jun11_click_drip_subjects_9178_0", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9178', 0, 'Your term life quote is ready to finish', 'Seeing your SBLI rate takes about 60 seconds.', TRUE, '+1h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_9178_1", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9178', 1, 'Did you finish your life insurance quote?', 'Your SBLI quick quote is still waiting — no pressure.', TRUE, '+6h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_9178_2", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9178', 2, 'Your SBLI rate is still available', 'Lock in your term life rate today.', TRUE, '+24h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_9178_3", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('9178', 3, 'Final reminder: your term life quote', 'Closing this out — last touch before we move on.', TRUE, '+72h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_417791_0", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('417791', 0, 'Your 60% off flooring estimate is reserved', 'Carpet, hardwood, laminate, and vinyl — free in-home estimate.', TRUE, '+1h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_417791_1", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('417791', 1, 'Did you schedule your free flooring estimate?', '60% off select styles ends June 29.', TRUE, '+6h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_417791_2", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('417791', 2, 'Last chance: 60% off Empire Today flooring', 'Sale pricing ends June 29 — book your free estimate.', TRUE, '+24h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11_click_drip_subjects_417791_3", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('417791', 3, 'Final reminder: your flooring estimate', 'Closing this out — last touch before we move on.', TRUE, '+72h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},

		// Click-drip lane updates round 2 (2026-06-11, operator brief):
		//   - Empire Today payout confirmed CPA $180.00 (guarded UPDATEs bring
		//     the prod rows in line; the jun11 seed above carries the same
		//     values for fresh DBs).
		//   - New drip lane: Home Repairly Roofing (421060, muqes network
		//     TQ5MX18J/XLRZDZ8K — the enroller learns the muqes host in this
		//     same build). Payout amount TBC.
		{"jun11b_click_drip_empire_payout_cpa", `UPDATE mailing_offer_journey_map
			SET payout_type='CPA',
			    notes='Empire Today Flooring — xnonu network; CPA $180.00 (operator 2026-06-11); 60% off ends 6/29',
			    updated_at=NOW()
			WHERE everflow_offer_id='417791' AND payout_type<>'CPA'`},
		{"jun11b_click_drip_empire_offer_payout", `UPDATE mailing_offers
			SET payout=180.00, payout_type='CPA', updated_at=NOW()
			WHERE everflow_offer_id='417791'
			  AND (payout IS DISTINCT FROM 180.00 OR payout_type IS DISTINCT FROM 'CPA')`},
		{"jun11b_click_drip_slug_homerepairly", `INSERT INTO mailing_offer_slug_map
			(cratoolpro_slug, everflow_offer_id, offer_name, enabled, notes) VALUES
			('XLRZDZ8K', '421060', 'Home Repairly Roofing', TRUE, 'muqes.com/TQ5MX18J/XLRZDZ8K; roofing-quotes lead-gen (drip lane 2026-06-11)')
			ON CONFLICT (cratoolpro_slug) DO NOTHING`},
		{"jun11b_click_drip_map_homerepairly", `INSERT INTO mailing_offer_journey_map
			(everflow_offer_id, click_journey_id, payout_type, enabled, notes)
			VALUES ('421060', 'click-drip-4touch-72h', 'CPL', TRUE, 'Home Repairly Roofing — muqes network (TQ5MX18J/XLRZDZ8K); roofing-quotes lead-gen; payout amount TBC; drip lane added 2026-06-11')
			ON CONFLICT (everflow_offer_id) DO NOTHING`},
		{"jun11b_click_drip_subjects_421060_0", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('421060', 0, 'Your local roofing quotes are ready', 'Compare up to 3 local roofers — free, no obligation.', TRUE, '+1h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11b_click_drip_subjects_421060_1", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('421060', 1, 'Did you finish requesting your roofing quotes?', 'Storm season is here — local pros can quote this week.', TRUE, '+6h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11b_click_drip_subjects_421060_2", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('421060', 2, 'Your 3 free roofing quotes are still waiting', 'Compare local prices before scheduling repairs.', TRUE, '+24h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},
		{"jun11b_click_drip_subjects_421060_3", `INSERT INTO mailing_offer_reminder_subjects (everflow_offer_id, sequence_index, subject, preheader, enabled, notes)
			VALUES ('421060', 3, 'Final reminder: your roofing quotes', 'Closing this out — last touch before we move on.', TRUE, '+72h reminder; operator-editable')
			ON CONFLICT (everflow_offer_id, sequence_index) DO NOTHING`},

		// Partner-drip multi-touch state columns (2026-06-11). The orchestrator's
		// follow-up path and the idx_pcq_followup_isp concurrent index already
		// reference these in production, but the ADD COLUMN DDL was never
		// captured in startup migrations (schema drift — applied manually).
		// Recorded here so fresh DBs and the drip-performance endpoint have
		// them; IF NOT EXISTS makes this a no-op in prod.
		{"jun11c_pcq_touch_count", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS touch_count INTEGER NOT NULL DEFAULT 0`},
		{"jun11c_pcq_last_touch_brand", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS last_touch_brand TEXT`},
		{"jun11c_pcq_last_touch_campaign_id", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS last_touch_campaign_id UUID`},
		{"jun11c_pcq_next_touch_at", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS next_touch_at TIMESTAMPTZ`},
		{"jun11c_pcq_engaged_at", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS engaged_at TIMESTAMPTZ`},
		{"jun11c_pcq_terminal_reason", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS terminal_reason TEXT`},
		{"jun11c_pcq_subscriber_id", `ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS subscriber_id UUID`},

		// Domain Agent (2026-06-09): per-domain × ISP daily scorecard rolled up
		// by domainagent.ScorecardWorker, plus the plan lifecycle table backing
		// /api/mailing/domain-agent/plans (draft → compiled → approved →
		// deployed/failed). See internal/domainagent/.
		{"create_domain_agent_scorecard", `CREATE TABLE IF NOT EXISTS mailing_domain_agent_scorecard (
			organization_id UUID NOT NULL, day DATE NOT NULL,
			sending_domain VARCHAR(255) NOT NULL, isp VARCHAR(50) NOT NULL,
			sends INTEGER NOT NULL DEFAULT 0, delivered INTEGER NOT NULL DEFAULT 0,
			human_opens INTEGER NOT NULL DEFAULT 0, machine_opens INTEGER NOT NULL DEFAULT 0,
			human_clicks INTEGER NOT NULL DEFAULT 0, machine_clicks INTEGER NOT NULL DEFAULT 0,
			hard_bounces INTEGER NOT NULL DEFAULT 0, soft_bounces INTEGER NOT NULL DEFAULT 0,
			other_bounces INTEGER NOT NULL DEFAULT 0, complaints INTEGER NOT NULL DEFAULT 0,
			unsubscribes INTEGER NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (organization_id, day, sending_domain, isp))`},
		{"create_domain_agent_plans", `CREATE TABLE IF NOT EXISTS mailing_domain_agent_plans (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), organization_id UUID NOT NULL,
			sending_domain VARCHAR(255) NOT NULL, plan_date DATE NOT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'draft',
			briefing JSONB NOT NULL DEFAULT '{}'::jsonb, slots JSONB NOT NULL DEFAULT '[]'::jsonb,
			payloads JSONB NOT NULL DEFAULT '[]'::jsonb, deploy_results JSONB NOT NULL DEFAULT '[]'::jsonb,
			approved_by VARCHAR(255), approved_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (organization_id, sending_domain, plan_date))`},
		{"create_domain_agent_plans_status_idx", `CREATE INDEX IF NOT EXISTS idx_domain_agent_plans_org_status ON mailing_domain_agent_plans(organization_id, status)`},

		// Content snapshots (2026-06-09): hash-addressed base creatives for
		// the set-based wave enqueue. One row per distinct (html, plain,
		// content_locked) creative; queue rows reference it via
		// content_snapshot_id instead of carrying a per-recipient body copy.
		// See docs/CAMPAIGN_QUEUE_STORAGE_REDESIGN.md §5 and
		// worker/content_snapshot.go.
		{"create_content_snapshots", `CREATE TABLE IF NOT EXISTS mailing_content_snapshots (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			content_hash TEXT NOT NULL UNIQUE,
			campaign_id UUID,
			wave_id UUID,
			html_content TEXT NOT NULL,
			plain_content TEXT NOT NULL DEFAULT '',
			content_locked BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},
		{"add_queue_content_snapshot_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS content_snapshot_id UUID`},

		// PMTACollector (internal/pmta/collector.go persistMetrics) has been
		// INSERTing status/ip/domain snapshots into this table since the
		// SparkPost-era schema was retired — every write failed with
		// "relation does not exist" (log spam confirmed back ≥24h on
		// 2026-06-10, AAR action item 5). Creating it both silences the
		// errors and starts retaining PMTA metric history. Retention:
		// data_cleanup.go cleanupESPMetricSnapshots (30 days).
		{"create_esp_metric_snapshots", `CREATE TABLE IF NOT EXISTS esp_metric_snapshots (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID,
			esp VARCHAR(50) NOT NULL,
			snapshot_type VARCHAR(50) NOT NULL,
			collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			period_start TIMESTAMPTZ,
			period_end TIMESTAMPTZ,
			metrics JSONB NOT NULL DEFAULT '{}'::jsonb,
			source_hash TEXT NOT NULL DEFAULT ''
		)`},
		{"idx_esp_metric_snapshots_collected", `CREATE INDEX IF NOT EXISTS idx_esp_metric_snapshots_collected ON esp_metric_snapshots (collected_at)`},

		// Segment build ledger (2026-06-09): app-owned, one-row-per-segment
		// summary table behind the v2 segments list. Replaces the per-page-load
		// COUNT(*) GROUP BY over mailing_segment_members (huge) with a tiny
		// indexed read. Every member-writing path (materializer, recalculate,
		// lake builders) upserts its row via internal/api/segment_ledger.go.
		// This is a COMPANION table by design: mailing_segments is owned by
		// apex_admin and the app user cannot ALTER it (CLAUDE.md §4), so the
		// summary columns live here under app ownership instead.
		// One-time backfill from mailing_segment_members runs AFTER this slice
		// behind a Go-side emptiness probe — see segment_build_ledger_backfill
		// below; do NOT add the backfill INSERT..SELECT to this slice.
		{"create_segment_build_ledger", `CREATE TABLE IF NOT EXISTS mailing_segment_build_ledger (
			segment_id UUID PRIMARY KEY,
			subscriber_count BIGINT NOT NULL DEFAULT 0,
			last_built_at TIMESTAMPTZ,
			build_source TEXT,
			last_build_status TEXT NOT NULL DEFAULT 'unknown',
			last_build_ms INTEGER,
			last_delta_pct REAL,
			last_error TEXT,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`},

		// =====================================================================
		// Migration 049: Kafka event-backbone SHADOW tables (ships DARK).
		// ADDITIVE ONLY — new tables, no ALTER of any existing table. Nothing
		// writes to these until the Wave-2 shadow consumers are wired behind
		// their flags (which require KAFKA_BROKERS set AND a flag ON). Schema
		// lands before/with the binary, so the consumers can never reference a
		// missing table. The exact snippet is carried in the header of
		// migrations/049_kafka_shadow_tables.sql. Each statement is its own
		// idempotent CREATE ... IF NOT EXISTS entry so it fits the per-statement
		// 5s budget of this runner.
		// =====================================================================
		{"create_ingest_events_shadow", `CREATE TABLE IF NOT EXISTS ingest_events_shadow (
			id                 UUID        NOT NULL,
			campaign_id        UUID,
			subscriber_id      UUID,
			recipient          TEXT,
			event_type         VARCHAR(20) NOT NULL,
			isp                VARCHAR(50),
			event_at           TIMESTAMPTZ NOT NULL,
			kafka_event_uid    TEXT,
			kafka_offset       BIGINT,
			kafka_partition    INT,
			shadow_ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT uq_ingest_events_shadow_id_event_at UNIQUE (id, event_at)
		)`},
		{"idx_ies_kafka_uid", `CREATE INDEX IF NOT EXISTS idx_ies_kafka_uid ON ingest_events_shadow (kafka_event_uid)`},
		{"idx_ies_event_at", `CREATE INDEX IF NOT EXISTS idx_ies_event_at ON ingest_events_shadow (event_at)`},
		{"idx_ies_campaign", `CREATE INDEX IF NOT EXISTS idx_ies_campaign ON ingest_events_shadow (campaign_id, event_at DESC)`},
		{"create_suppression_shadow", `CREATE TABLE IF NOT EXISTS suppression_shadow (
			email           TEXT NOT NULL,
			reason          TEXT,
			brand_root      TEXT NOT NULL DEFAULT '',
			source          TEXT,
			kafka_event_uid TEXT,
			applied_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT uq_suppression_shadow_email_brand UNIQUE (email, brand_root)
		)`},
		{"idx_supp_shadow_email", `CREATE INDEX IF NOT EXISTS idx_supp_shadow_email ON suppression_shadow (email)`},
		{"idx_supp_shadow_applied_at", `CREATE INDEX IF NOT EXISTS idx_supp_shadow_applied_at ON suppression_shadow (applied_at DESC)`},
		{"idx_supp_shadow_kafka_uid", `CREATE INDEX IF NOT EXISTS idx_supp_shadow_kafka_uid ON suppression_shadow (kafka_event_uid)`},
		// ---------------------------------------------------------------------
		// Compliance fix (2026-06-25): remove the "James Ventures Corp" HEADER block.
		//
		// A prior top-injecting disclaimer (injectUnsubDisclaimer, removed in this
		// change) baked a do-not-reply + corporate-identity block into the HEADER
		// of stored creatives at store time. It reached production via the offer
		// path (ratesbazar / Optima Tax, 2026-06-25). Strip that block from every
		// creative that can still send (mailing_campaigns, excluding terminal
		// statuses) and from all offer-creative sources. The fixed code now appends
		// a "{{ brand.domain }} · <postal>" footer at the BOTTOM — dynamic per
		// sending brand (send_worker resolves brand.domain = item.BrandRoot), never
		// the corporate identity.
		//
		// Guarded by a one-time sentinel in organizations.settings so it runs
		// exactly once (the position-agnostic strip would otherwise also match the
		// new bottom footer on later boots). Campaigns keep their own footer
		// (verified: 0 lose unsubscribe after strip). The 3 bare offer-creative
		// sources (no own footer) get the corrected brand footer re-appended so
		// they stay CAN-SPAM compliant. Atomic: a timeout rolls back and retries
		// next boot.
		// The block has TWO stored variants: (A) two paragraphs — do-not-reply +
		// address (the buildUnsubDisclaimerHTML form, may carry "James Ventures
		// Corp"); (B) one paragraph — do-not-reply only (the older may19 setup-script
		// form, no address). The strip is two NESTED regexp_replace passes using
		// NEGATED character classes ([^<]/[^>]) — NOT '.*?' — because PostgreSQL ARE
		// sets the WHOLE expression's greediness from its FIRST quantifier, so a
		// mixed '.*?</p>' silently over-matches into creative content. Pass 1 removes
		// the style-unique address paragraph (padding:4px 20px 8px + font-size:10px —
		// emitted only by our footer, never creative content). Pass 2 removes the
		// marker + do-not-reply paragraph. Validated on all 343 sendable campaigns +
		// 36 offer creatives: 0 retain the marker/JVC, 0 lose their own unsubscribe,
		// removal bounded 332–525 chars (exactly the block, no content touched).
		{"strip_jvc_header_disclaimer_jun25", `
			DO $$
			BEGIN
			IF NOT EXISTS (SELECT 1 FROM organizations WHERE settings ? 'jvc_header_stripped_jun25') THEN
				-- status IN (...) FIRST so idx_campaigns_status narrows the 103k-row /
				-- 2GB table to ~2.5k sendable rows before the html regex runs; a
				-- NOT IN / unscoped scan times out the 5s startup-migration budget.
				UPDATE mailing_campaigns
				SET html_content = regexp_replace(
					regexp_replace(html_content, '<p[^>]*padding:4px 20px 8px[^>]*font-size:10px[^>]*>[^<]*</p>', ''),
					'<!-- unsub-disclaimer --><p[^>]*>[^<]*box is not monitored[^<]*<a[^>]*>[^<]*</a>[^<]*</p>', '')
				WHERE status IN ('scheduled','draft','sending','finalizing_audience','preparing','paused','failed')
				  AND html_content ~ 'unsub-disclaimer';

				UPDATE mailing_offer_creatives
				SET html_content = CASE
					WHEN regexp_replace(regexp_replace(html_content, '<p[^>]*padding:4px 20px 8px[^>]*font-size:10px[^>]*>[^<]*</p>', ''), '<!-- unsub-disclaimer --><p[^>]*>[^<]*box is not monitored[^<]*<a[^>]*>[^<]*</a>[^<]*</p>', '')
						 ~* 'unsubscribe|opt-?out|/unsub'
					THEN regexp_replace(regexp_replace(html_content, '<p[^>]*padding:4px 20px 8px[^>]*font-size:10px[^>]*>[^<]*</p>', ''), '<!-- unsub-disclaimer --><p[^>]*>[^<]*box is not monitored[^<]*<a[^>]*>[^<]*</a>[^<]*</p>', '')
					ELSE regexp_replace(regexp_replace(html_content, '<p[^>]*padding:4px 20px 8px[^>]*font-size:10px[^>]*>[^<]*</p>', ''), '<!-- unsub-disclaimer --><p[^>]*>[^<]*box is not monitored[^<]*<a[^>]*>[^<]*</a>[^<]*</p>', '')
						|| '<!-- unsub-disclaimer --><p style="margin:0;padding:8px 20px;font-family:Arial,Helvetica,sans-serif;font-size:11px;color:#999999;text-align:center;">Please do not reply, this email box is not monitored. To stop email subscriptions at any time please <a href="{{ system.unsubscribe_url }}" style="color:#999999;">unsubscribe</a>.</p><p style="margin:0;padding:4px 20px 8px;font-family:Arial,Helvetica,sans-serif;font-size:10px;color:#bbbbbb;text-align:center;">{{ brand.domain }} · 30 N Gould St, Ste R, Sheridan, WY 82801</p>'
				END
				WHERE html_content ~ 'unsub-disclaimer';

				UPDATE organizations
				SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{jvc_header_stripped_jun25}', 'true'::jsonb, true);
			END IF;
			END $$;`},
	}

	// Use a dedicated connection with a short statement timeout so heavy
	// backfills fail fast (~5s) instead of holding up startup for 30s each.
	conn, connErr := db.Conn(context.Background())
	if connErr != nil {
		log.Printf("[StartupMigration] failed to get dedicated connection: %v — using pool", connErr)
	}
	execSQL := func(sql string) error {
		if conn != nil {
			_, err := conn.ExecContext(context.Background(), sql)
			if err != nil {
				// A timed-out/cancelled statement can leave the dedicated connection in a
				// failed state, silently erroring every LATER migration on the same boot
				// (this is what blocked the org-rename migration). Drop it; subsequent
				// statements fall back to the pool below with a per-statement timeout.
				conn.Close()
				conn = nil
			}
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := db.ExecContext(ctx, sql)
		return err
	}
	if conn != nil {
		defer conn.Close()
		execSQL("SET statement_timeout = '5s'")
	}

	var ok, fail, skip, alreadyApplied int
	for _, m := range migrations {
		// Catalog probe: skip recognized idempotent DDL whose effect already
		// exists, so a boot does not replay hundreds of lock-taking no-ops
		// against hot tables (the 2026-06-10 brownout mechanism). Probe
		// errors fail open into normal execution. See migration_skip.go.
		if migrationSkipProbe(db, m.sql) {
			alreadyApplied++
			continue
		}
		if err := execSQL(m.sql); err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "statement timeout") {
				log.Printf("[StartupMigration] %s: TIMEOUT (skipped — will retry next boot)", m.name)
				skip++
			} else {
				log.Printf("[StartupMigration] %s: ERROR %v", m.name, err)
				fail++
			}
		} else {
			ok++
		}
	}
	log.Printf("[StartupMigration] Complete: %d OK, %d errors, %d timeouts, %d skipped (already applied)", ok, fail, skip, alreadyApplied)

	// Diagnostic: check for invalid indexes (can happen if CREATE INDEX CONCURRENTLY fails mid-way)
	var invalidCount int
	db.QueryRow(`SELECT COUNT(*) FROM pg_class c JOIN pg_index i ON c.oid = i.indexrelid WHERE NOT i.indisvalid`).Scan(&invalidCount)
	if invalidCount > 0 {
		log.Printf("[StartupMigration] WARNING: %d invalid indexes detected — run REINDEX on affected tables", invalidCount)
	}

	// Diagnostic: log pool assignments for OVH IPs to verify routing
	poolRows, poolErr := db.Query(`
		SELECT ip.ip_address::text, ip.hostname, pool.name as pool_name, ip.status, COALESCE(ip.warmup_daily_limit, 0)
		FROM mailing_ip_addresses ip
		JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
		WHERE ip.ip_address::text LIKE '15.204.22.%' OR ip.ip_address::text LIKE '15.204.38.%' OR ip.ip_address::text LIKE '144.225.178.%'
		ORDER BY ip.ip_address`)
	if poolErr == nil {
		defer poolRows.Close()
		log.Println("[PoolDiag] === OVH IP Pool Assignments ===")
		for poolRows.Next() {
			var ipAddr, hostname, poolName, status string
			var warmupLimit int
			if err := poolRows.Scan(&ipAddr, &hostname, &poolName, &status, &warmupLimit); err == nil {
				log.Printf("[PoolDiag] %s → pool=%s vmta=%s status=%s limit=%d", ipAddr, poolName, vmtaShort(hostname), status, warmupLimit)
			}
		}
	}
	// Also log sending profile routing
	profRows, profErr := db.Query(`
		SELECT name, sending_domain, COALESCE(ip_pool, ''), COALESCE(pool_prefix, ''), status
		FROM mailing_sending_profiles WHERE vendor_type = 'pmta' AND status = 'active'
		ORDER BY sending_domain`)
	if profErr == nil {
		defer profRows.Close()
		log.Println("[PoolDiag] === PMTA Sending Profile Routing ===")
		for profRows.Next() {
			var name, domain, ipPool, poolPrefix, status string
			if err := profRows.Scan(&name, &domain, &ipPool, &poolPrefix, &status); err == nil {
				log.Printf("[PoolDiag] profile=%s domain=%s ip_pool=%s pool_prefix='%s' status=%s", name, domain, ipPool, poolPrefix, status)
			}
		}
	}

	// One-time bulk suppression import: besmed attacker bounces (59,155 emails)
	var besmedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mailing_global_suppressions WHERE source = 'pmta_attack_besmed_20260312'`).Scan(&besmedCount); err == nil && besmedCount == 0 {
		log.Println("[StartupMigration] Importing besmed attacker bounce suppressions (59,155 emails)...")
		if _, err := db.Exec(besmedSuppressionSQL); err != nil {
			log.Printf("[StartupMigration] besmed_suppression_import: ERROR %v", err)
		} else {
			log.Println("[StartupMigration] besmed_suppression_import: OK")
		}
	} else if besmedCount > 0 {
		log.Printf("[StartupMigration] besmed_suppression_import: already loaded (%d entries)", besmedCount)
	}

	// One-time segment-build-ledger backfill (companion to the
	// create_segment_build_ledger migration in the slice above).
	//
	// CRITICAL ordering: the Go-side emptiness probe runs FIRST, and the
	// INSERT..SELECT only executes when the ledger has ZERO rows.
	// mailing_segment_members is enormous, and the GROUP BY aggregate is a
	// full scan — ON CONFLICT DO NOTHING alone would still PAY that scan on
	// every boot of every ECS task before discarding the rows. The EXISTS
	// probe is an index-only point read, so all routine boots skip the scan
	// entirely; only the very first boot after this table ships (or a manual
	// TRUNCATE for a re-seed) runs the aggregate once.
	var ledgerSeeded bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM mailing_segment_build_ledger LIMIT 1)`).Scan(&ledgerSeeded); err != nil {
		log.Printf("[StartupMigration] segment_build_ledger_backfill: probe error %v (skipping backfill)", err)
	} else if ledgerSeeded {
		log.Println("[StartupMigration] segment_build_ledger_backfill: already seeded")
	} else {
		// The one-time GROUP BY scan will exceed the 5s migration timeout —
		// run it on a dedicated connection with a 10-minute statement_timeout
		// (same pattern as idx_mcq_dead_letter_recent below).
		ledgerCtx, ledgerCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		if ledgerConn, ledgerConnErr := db.Conn(ledgerCtx); ledgerConnErr == nil {
			if _, err := ledgerConn.ExecContext(ledgerCtx, "SET statement_timeout = '600000'"); err != nil {
				log.Printf("[StartupMigration] segment_build_ledger_backfill: SET timeout failed: %v", err)
			}
			if _, err := ledgerConn.ExecContext(ledgerCtx, `INSERT INTO mailing_segment_build_ledger
					(segment_id, subscriber_count, last_built_at, build_source, last_build_status)
				SELECT segment_id, COUNT(*), NOW(), 'backfill', 'ok'
				FROM mailing_segment_members
				GROUP BY segment_id
				ON CONFLICT (segment_id) DO NOTHING`); err != nil {
				log.Printf("[StartupMigration] segment_build_ledger_backfill: ERROR %v (ledger still empty — will retry next boot)", err)
			} else {
				log.Println("[StartupMigration] segment_build_ledger_backfill: OK")
			}
			ledgerConn.Close()
		} else {
			log.Printf("[StartupMigration] segment_build_ledger_backfill: db.Conn failed: %v", ledgerConnErr)
		}
		ledgerCancel()
	}

	// Outbox dead-letter listing index. mailing_campaign_queue is
	// partitioned and >>1M rows; CREATE INDEX timed out at the pool
	// default (~30s), so we run it on a dedicated connection with a
	// 10-minute statement_timeout AND use CONCURRENTLY so we don't
	// take an AccessExclusiveLock on every partition during the build.
	// CONCURRENTLY cannot run inside a transaction (sql.DB autocommit
	// each Exec is one statement, so this is fine — but it does need
	// a fresh Conn so the SET applies). IF NOT EXISTS makes it a fast
	// no-op on subsequent boots once built.
	//
	// Index shape matches HandleOutboxDeadLetter exactly: ORDER BY
	// COALESCE(last_attempt_at, created_at) DESC, WHERE status IN
	// ('dead_letter','dead_letter_strict'). Query becomes index-only
	// scan with LIMIT pushed down.
	idxCtx, idxCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	if idxConn, idxConnErr := db.Conn(idxCtx); idxConnErr == nil {
		defer idxConn.Close()
		if _, err := idxConn.ExecContext(idxCtx, "SET statement_timeout = '600000'"); err != nil {
			log.Printf("[StartupMigration] idx_mcq_dead_letter_recent: SET timeout failed: %v", err)
		}
		if _, err := idxConn.ExecContext(idxCtx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mcq_dead_letter_recent ON mailing_campaign_queue (COALESCE(last_attempt_at, created_at) DESC) WHERE status IN ('dead_letter','dead_letter_strict')`); err != nil {
			log.Printf("[StartupMigration] idx_mcq_dead_letter_recent: ERROR %v", err)
		} else {
			log.Println("[StartupMigration] idx_mcq_dead_letter_recent: OK")
		}
	} else {
		log.Printf("[StartupMigration] idx_mcq_dead_letter_recent: db.Conn failed: %v", idxConnErr)
	}
	idxCancel()

	// Cleanup indexes for terminal queue purge and pmta_acct_raw retention/backfill.
	cleanupIdxCtx, cleanupIdxCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	if cleanupConn, cleanupConnErr := db.Conn(cleanupIdxCtx); cleanupConnErr == nil {
		defer cleanupConn.Close()
		cleanupIndexes := []struct {
			name string
			sql  string
		}{
			{
				"idx_mcq_terminal_cleanup",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mcq_terminal_cleanup
				 ON mailing_campaign_queue (status, (COALESCE(updated_at, created_at)), id)
				 WHERE status IN ('accepted', 'cancelled', 'failed', 'dead_letter', 'dead_letter_strict')`,
			},
			{
				"idx_pmta_acct_raw_processed_retention",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pmta_acct_raw_processed_retention
				 ON pmta_acct_raw (processed, received_at, id)
				 WHERE processed = TRUE`,
			},
			{
				"idx_pmta_acct_raw_unprocessed",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pmta_acct_raw_unprocessed
				 ON pmta_acct_raw (processed, received_at, id)
				 WHERE processed = FALSE`,
			},
			{
				// Covering index for EngineSignalsArchiver.findUnarchivedBuckets:
				// makes the per-day `SELECT DISTINCT isp WHERE recorded_at IN [day)`
				// an index-only scan (isp was previously heap-fetched, which
				// timed out under load and wedged the archiver for ~2 months).
				// Also accelerates the bucket DELETE's inner id lookup.
				"idx_engine_signals_recorded_isp",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_engine_signals_recorded_isp
				 ON mailing_engine_signals (recorded_at, isp)`,
			},
			{
				// PartnerDripOrchestrator welcome path. resolvePerISPCaps
				// (GROUP BY isp_family) and claimRecordsByISPCaps
				// (PARTITION BY isp_family ORDER BY ingested_at) both filter
				// (vertical, status='ready') but rank/group by isp_family.
				// The existing idx_pcq_status_ready (vertical, status,
				// ingested_at) does NOT carry isp_family, so these queries
				// heap-fetch isp_family for every ready row — which times out
				// under RDS IO pressure and silently zeroes every tick
				// (the "partner drip behind schedule" incident, 2026-06-07).
				// Including isp_family in the partial index makes both
				// index-only. ingested_at last preserves the oldest-first ORDER.
				"idx_pcq_ready_isp",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pcq_ready_isp
				 ON partner_clean_queue (vertical, isp_family, ingested_at)
				 WHERE status = 'ready'`,
			},
			{
				// PartnerDripOrchestrator follow-up path. Mirrors the welcome
				// index for claimFollowupRecordsByISPCaps, which partitions the
				// due 'mailed' set by isp_family ORDER BY next_touch_at and
				// filters (vertical, touch_count, next_touch_at<=NOW(),
				// engaged_at IS NULL, terminal_reason IS NULL). Keeps the
				// isp-partitioned follow-up claim index-only under the same load.
				"idx_pcq_followup_isp",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pcq_followup_isp
				 ON partner_clean_queue (vertical, touch_count, isp_family, next_touch_at)
				 WHERE status = 'mailed' AND engaged_at IS NULL AND terminal_reason IS NULL`,
			},
			{
				// Per-ISP queue breakdown for the dispatcher / wave planner.
				// mailing_campaign_queue is partitioned and >>1M rows, so a
				// plain CREATE INDEX takes an AccessExclusiveLock on every
				// partition and blocks ALL sends for the duration of the build
				// (this happened during the 2026-06-07 outbox-zombie incident:
				// the non-concurrent build wedged behind an orphaned UPDATE and
				// froze the send path). Built CONCURRENTLY here on the dedicated
				// 10-min-timeout connection so a rebuild (new env, dropped index)
				// never freezes sending again. IF NOT EXISTS makes it a no-op
				// once present.
				"idx_campaign_queue_recipient_isp",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_campaign_queue_recipient_isp
				 ON mailing_campaign_queue (campaign_id, recipient_isp, status)
				 WHERE recipient_isp IS NOT NULL`,
			},
			{
				// Supports DataCleanupWorker.slimAcceptedQueueHTML's victim
				// SELECT and StorageGuard.checkQueueHTML's count. Partial on the
				// exact predicate (accepted rows that still carry HTML), so it
				// points straight at un-slimmed rows and shrinks to empty once
				// the backlog (5.5M rows flagged 2026-06-07) is drained —
				// keeping both the slim batches and the 5-min monitor count
				// index-only/instant instead of scanning the full accepted set.
				"idx_mcq_accepted_html",
				`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mcq_accepted_html
				 ON mailing_campaign_queue (id)
				 WHERE status = 'accepted' AND html_content IS NOT NULL`,
			},
		}
		if _, err := cleanupConn.ExecContext(cleanupIdxCtx, "SET statement_timeout = '600000'"); err != nil {
			log.Printf("[StartupMigration] cleanup indexes: SET timeout failed: %v", err)
		}
		for _, idx := range cleanupIndexes {
			if _, err := cleanupConn.ExecContext(cleanupIdxCtx, idx.sql); err != nil {
				log.Printf("[StartupMigration] %s: ERROR %v", idx.name, err)
			} else {
				log.Printf("[StartupMigration] %s: OK", idx.name)
			}
		}
	} else {
		log.Printf("[StartupMigration] cleanup indexes: db.Conn failed: %v", cleanupConnErr)
	}
	cleanupIdxCancel()

	// ---------------------------------------------------------------------
	// partner_clean_queue uniqueness — dedup + UNIQUE(vertical, email_md5).
	//
	// partner_clean_queue had NO uniqueness guarantee (only the PK on id).
	// The slicer's `INSERT ... ON CONFLICT DO NOTHING` (partner_slicer.go)
	// has no arbiter index, so it never actually deduped: re-uploads of the
	// same address AND slice-reprocessing after a bulk-insert statement
	// timeout (the 2026-06-09 RDS-IO-saturation incident) both produced
	// duplicate rows. Duplicate 'ready' rows get claimed independently by
	// the orchestrator → the same person is mailed twice in a lane.
	//
	// A UNIQUE index on (vertical, email_md5) — the orchestrator's natural
	// processing grain (one send per person per lane) — fixes this two ways:
	//   1. It becomes the arbiter for the slicer's existing ON CONFLICT DO
	//      NOTHING, so re-uploads and slice retries are silently deduped.
	//   2. It hard-prevents double-mailing the same address within a vertical.
	//
	// Build sequence (one-time; guarded so it's a no-op once the VALID index
	// exists):
	//   a. If a VALID unique index already exists → skip everything.
	//   b. Drop any INVALID leftover from a prior failed CONCURRENTLY build.
	//   c. Table-wide dedup keeping the most-progressed status per
	//      (vertical, email_md5): mailed > claimed > ready > eo_in_flight >
	//      pending_eo > suppressed_eo, then oldest ingested_at, then id.
	//      Keeping 'mailed' first guarantees we never delete an already-sent
	//      row (so nothing re-mails) and collapses double-'ready' to one.
	//   d. CREATE UNIQUE INDEX CONCURRENTLY (cannot run in a txn; the
	//      dedicated autocommit Conn handles that). If a live slicer insert
	//      races a new dup into the gap between (c) and (d) the build fails
	//      and leaves an invalid index — step (b) drops it and the next boot
	//      retries. Once the slicer's ON CONFLICT has this arbiter, no new
	//      dups can form and the build succeeds for good.
	//
	// Dedicated connection, 25-min statement_timeout (the table-wide dedup
	// DELETE can be heavy on the first boot; it shrinks to zero work after).
	// ---------------------------------------------------------------------
	pcqUniqCtx, pcqUniqCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	if pcqConn, pcqConnErr := db.Conn(pcqUniqCtx); pcqConnErr == nil {
		var validUniq bool
		_ = pcqConn.QueryRowContext(pcqUniqCtx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_class c
				JOIN pg_index i ON i.indexrelid = c.oid
				WHERE c.relname = 'uq_pcq_vertical_email_md5'
				  AND i.indisvalid AND i.indisunique
			)`).Scan(&validUniq)
		if validUniq {
			log.Println("[StartupMigration] uq_pcq_vertical_email_md5: valid index present — skipping dedup")
		} else {
			if _, err := pcqConn.ExecContext(pcqUniqCtx, "SET statement_timeout = '1500000'"); err != nil { // 25 min
				log.Printf("[StartupMigration] uq_pcq_vertical_email_md5: SET timeout failed: %v", err)
			}
			// (b) drop any invalid leftover from a prior failed CONCURRENTLY build.
			if _, err := pcqConn.ExecContext(pcqUniqCtx, `DROP INDEX IF EXISTS uq_pcq_vertical_email_md5`); err != nil {
				log.Printf("[StartupMigration] uq_pcq_vertical_email_md5: drop-invalid failed: %v", err)
			}
			// (c) table-wide dedup, keeping the most-progressed row per (vertical, email_md5).
			dedupStart := time.Now()
			res, derr := pcqConn.ExecContext(pcqUniqCtx, `
				DELETE FROM partner_clean_queue q
				USING (
					SELECT id, row_number() OVER (
						PARTITION BY vertical, email_md5
						ORDER BY CASE status
							WHEN 'mailed'        THEN 1
							WHEN 'claimed'       THEN 2
							WHEN 'ready'         THEN 3
							WHEN 'eo_in_flight'  THEN 4
							WHEN 'pending_eo'    THEN 5
							WHEN 'suppressed_eo' THEN 6
							ELSE 7 END,
							ingested_at ASC, id ASC
					) AS rn
					FROM partner_clean_queue
				) d
				WHERE q.id = d.id AND d.rn > 1`)
			if derr != nil {
				log.Printf("[StartupMigration] pcq_dedup_vertical_email_md5: ERROR %v (took %s) — deferring unique index to next boot", derr, time.Since(dedupStart))
			} else {
				n, _ := res.RowsAffected()
				log.Printf("[StartupMigration] pcq_dedup_vertical_email_md5: removed %d duplicate rows (took %s)", n, time.Since(dedupStart))
				// (d) build the unique index CONCURRENTLY.
				idxStart := time.Now()
				if _, ierr := pcqConn.ExecContext(pcqUniqCtx, `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_pcq_vertical_email_md5 ON partner_clean_queue (vertical, email_md5)`); ierr != nil {
					log.Printf("[StartupMigration] uq_pcq_vertical_email_md5: ERROR %v (took %s) — will retry next boot", ierr, time.Since(idxStart))
				} else {
					log.Printf("[StartupMigration] uq_pcq_vertical_email_md5: OK (took %s)", time.Since(idxStart))
				}
			}
		}
		pcqConn.Close()
	} else {
		log.Printf("[StartupMigration] uq_pcq_vertical_email_md5: db.Conn failed: %v", pcqConnErr)
	}
	pcqUniqCancel()

	// ---------------------------------------------------------------------
	// P2a — backfill recipient_domain on the last 7 days of 'opened' and
	// 'clicked' rows. Before tracking/consumer.go was fixed (commit
	// 7dfa4c3 / 2026-04-28), the SQS consumer wrote these rows without
	// populating recipient_domain, forcing per-ISP analytics to
	// reconstruct the bucket via a join-back to mailing_subscribers and
	// silently bucketing orphaned rows into 'other'. This pass reconciles
	// the recent history so the post-fix diagnostic (P4) operates on
	// clean data.
	//
	// Sized in production at ~403k rows over 7 days. The main migration
	// loop runs at statement_timeout=5s so we use a dedicated connection
	// here with a 15-minute timeout, mirroring idx_mcq_dead_letter_recent
	// above. The UPDATE is idempotent (recipient_domain IS NULL) so
	// subsequent boots do effectively zero work.
	//
	// The 30-day extension (backfill_open_recipient_domain_v1_w2) only
	// ships once P4 confirms the fix solves the issue.
	// ---------------------------------------------------------------------
	bfCtx, bfCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	if bfConn, bfConnErr := db.Conn(bfCtx); bfConnErr == nil {
		if _, err := bfConn.ExecContext(bfCtx, "SET statement_timeout = '600000'"); err != nil {
			log.Printf("[StartupMigration] backfill_open_recipient_domain_v1_w1: SET timeout failed: %v", err)
		}
		bfStart := time.Now()
		res, err := bfConn.ExecContext(bfCtx, `
			UPDATE mailing_tracking_events te
			SET recipient_domain = LOWER(SPLIT_PART(s.email, '@', 2))
			FROM mailing_subscribers s
			WHERE te.subscriber_id = s.id
			  AND te.event_type IN ('opened', 'clicked')
			  AND te.recipient_domain IS NULL
			  AND te.event_at >= NOW() - INTERVAL '7 days'
		`)
		if err != nil {
			log.Printf("[StartupMigration] backfill_open_recipient_domain_v1_w1: ERROR %v (took %s)", err, time.Since(bfStart))
		} else {
			n, _ := res.RowsAffected()
			log.Printf("[StartupMigration] backfill_open_recipient_domain_v1_w1: OK rows=%d (took %s)", n, time.Since(bfStart))
		}
		bfConn.Close()
	} else {
		log.Printf("[StartupMigration] backfill_open_recipient_domain_v1_w1: db.Conn failed: %v", bfConnErr)
	}
	bfCancel()

	// idx_campaign_queue_recipient_isp is now built CONCURRENTLY in the
	// cleanupIndexes block above (dedicated connection, 10-min timeout). It
	// previously ran here as a plain non-concurrent CREATE INDEX, which takes
	// an AccessExclusiveLock on every mailing_campaign_queue partition and
	// blocks all sends for the build duration — a latent send-blocking hazard
	// (see the 2026-06-07 outbox-zombie incident). Do not reintroduce a
	// non-concurrent build of this index here.

	// IP pool preferences for ops-controlled isolation (must be in startup migrations
	// because DB_ADMIN_URL is not set in production ECS so runAdminMigrations is skipped)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS mailing_ip_pool_preferences (
		pool_id UUID REFERENCES mailing_ip_pools(id),
		preferred_ip_id UUID REFERENCES mailing_ip_addresses(id),
		standby_ip_id UUID REFERENCES mailing_ip_addresses(id),
		isolation_mode VARCHAR(20) DEFAULT 'normal' CHECK (isolation_mode IN ('normal', 'strict')),
		reason TEXT,
		set_by TEXT DEFAULT 'manual',
		set_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (pool_id)
	)`); err != nil {
		log.Printf("[StartupMigration] create_ip_pool_preferences: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] create_ip_pool_preferences: OK")
	}

	// Add isolation_mode column to mailing_ip_pools
	if _, err := db.Exec(`ALTER TABLE mailing_ip_pools ADD COLUMN IF NOT EXISTS isolation_mode VARCHAR(20) DEFAULT 'normal'`); err != nil {
		log.Printf("[StartupMigration] add_pool_isolation_mode: ERROR %v", err)
	}

	// Add retry_after column to queue tables for strict-pool backoff scheduling
	db.Exec(`ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS retry_after TIMESTAMPTZ`)
	db.Exec(`ALTER TABLE IF EXISTS mailing_campaign_queue_v2 ADD COLUMN IF NOT EXISTS retry_after TIMESTAMPTZ`)

	// ---------------------------------------------------------------------
	// content_locked: gate fingerprint-diversification mutations for strict
	// advertisers who require the approved creative go out byte-faithful.
	// When true on a campaign row, EnqueuePMTAWave skips mutateSubjectLine
	// and mutateHTMLHash (honeypot injection remains on).
	// Offer-level flag seeds campaign default; TruGreen is seeded = TRUE.
	// ---------------------------------------------------------------------
	if _, err := db.Exec(`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS content_locked BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
		log.Printf("[StartupMigration] add_campaigns_content_locked: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] add_campaigns_content_locked: OK")
	}
	if _, err := db.Exec(`ALTER TABLE mailing_offers ADD COLUMN IF NOT EXISTS content_locked BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
		log.Printf("[StartupMigration] add_offers_content_locked: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] add_offers_content_locked: OK")
	}

	// ---------------------------------------------------------------------
	// late_alert_sent_at: dedup column for the CampaignHealthMonitor's
	// lateness-SMS pager (internal/worker/campaign_health_monitor.go →
	// checkLateCampaigns). Stamped only after a successful SMS; suppresses
	// repeat alerts within alerting.campaign_lateness.realert_after_hours.
	// ---------------------------------------------------------------------
	if _, err := db.Exec(`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS late_alert_sent_at TIMESTAMPTZ`); err != nil {
		log.Printf("[StartupMigration] add_campaigns_late_alert_sent_at: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] add_campaigns_late_alert_sent_at: OK")
	}
	// Seed TruGreen offer(s) as content_locked. Idempotent: sets flag only
	// when not already true. Matches on brand_name or name containing "trugreen".
	if res, err := db.Exec(`UPDATE mailing_offers
		SET content_locked = TRUE, updated_at = NOW()
		WHERE content_locked = FALSE
		  AND (brand_name ILIKE '%trugreen%' OR name ILIKE '%trugreen%')`); err != nil {
		log.Printf("[StartupMigration] seed_trugreen_content_locked: ERROR %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[StartupMigration] seed_trugreen_content_locked: OK (%d offers locked)", n)
	} else {
		log.Println("[StartupMigration] seed_trugreen_content_locked: OK (no change)")
	}

	// ---------------------------------------------------------------------
	// Vendor-batch import framework (durable, multi-vendor).
	//
	// Every external-vendor subscriber batch (WestCapital Jan 2026, future
	// LendingTree / SoFi / etc.) lands on a single org-wide master list
	// "Verified External Imports - Master". Per-batch separation is via
	// tags (batch:<vendor>_<batch>), source_detail, and custom_fields.
	// The vendor_batch_audit table is the append-only ledger keyed on
	// (organization_id, vendor, batch_key); load/suppress/rollback scripts
	// all read and stamp it so any batch is reversible.
	//
	// Docs: docs/VENDOR_BATCH_IMPORT.md
	// Framework scripts: scripts/import/prepare_vendor_verified.py,
	//                    load_vendor_verified.py,
	//                    suppress_vendor_unverified.py,
	//                    rollback_vendor_batch.py
	// Planner integration: pmta_campaign_planner.go hybrid path honours
	//                      send_priority+inclusion_segments BEFORE SDS when
	//                      use_master_selection=true, so a batch's segment
	//                      is drained first then master-list fills any
	//                      remaining ISP quota.
	// ---------------------------------------------------------------------
	if _, err := db.Exec(`ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}'::text[]`); err != nil {
		log.Printf("[StartupMigration] add_subscribers_tags: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] add_subscribers_tags: OK")
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_subscribers_tags_gin ON mailing_subscribers USING gin (tags)`); err != nil {
		log.Printf("[StartupMigration] idx_subscribers_tags_gin: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] idx_subscribers_tags_gin: OK")
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS mailing_vendor_batch_audit (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		organization_id UUID NOT NULL,
		vendor TEXT NOT NULL,
		batch_key TEXT NOT NULL,
		datatype TEXT,
		config_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
		verified_count INTEGER NOT NULL DEFAULT 0,
		merged_count INTEGER NOT NULL DEFAULT 0,
		inserted_count INTEGER NOT NULL DEFAULT 0,
		suppressed_count INTEGER NOT NULL DEFAULT 0,
		segment_id UUID,
		list_id UUID,
		imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		rolled_back_at TIMESTAMPTZ,
		rollback_reason TEXT,
		notes TEXT,
		CONSTRAINT uq_vendor_batch_audit UNIQUE (organization_id, vendor, batch_key)
	)`); err != nil {
		log.Printf("[StartupMigration] create_vendor_batch_audit: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] create_vendor_batch_audit: OK")
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_vendor_batch_audit_vendor ON mailing_vendor_batch_audit (vendor, batch_key)`); err != nil {
		log.Printf("[StartupMigration] idx_vendor_batch_audit_vendor: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] idx_vendor_batch_audit_vendor: OK")
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_vendor_batch_audit_live ON mailing_vendor_batch_audit (organization_id, imported_at DESC) WHERE rolled_back_at IS NULL`); err != nil {
		log.Printf("[StartupMigration] idx_vendor_batch_audit_live: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] idx_vendor_batch_audit_live: OK")
	}

	// Seed the single org-wide master list row. All vendor batches accumulate
	// into this list. Per-batch separation happens via tags. Idempotent across
	// boots; never duplicates per organization.
	if res, err := db.Exec(`
		INSERT INTO mailing_lists (organization_id, name, description, status, opt_in_type, created_at, updated_at)
		SELECT id, 'Verified External Imports - Master',
			'Org-wide master list for Email Oversight-verified external-vendor imports. All vendor batches accumulate into this list; per-batch separation is via tags (batch:<vendor>_<batch>), source_detail, and custom_fields.provenance. Segments targeting tag batch:<vendor>_<batch> surface each cohort. See docs/VENDOR_BATCH_IMPORT.md.',
			'active', 'single', NOW(), NOW()
		FROM organizations
		WHERE NOT EXISTS (
			SELECT 1 FROM mailing_lists
			WHERE name = 'Verified External Imports - Master' AND organization_id = organizations.id
		)
	`); err != nil {
		log.Printf("[StartupMigration] seed_verified_imports_master_list: ERROR %v", err)
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[StartupMigration] seed_verified_imports_master_list: OK (%d lists created)", n)
	} else {
		log.Println("[StartupMigration] seed_verified_imports_master_list: OK (no change)")
	}

	// Warn about active PMTA profiles missing an api_endpoint — these
	// will fall through to SMTP-only mode, which risks DMARC rejection.
	rows, err := db.Query(`
		SELECT name, smtp_host, api_endpoint FROM mailing_sending_profiles
		WHERE vendor_type = 'pmta' AND status = 'active'
		  AND (api_endpoint IS NULL OR api_endpoint = '')
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var pName string
			var pHost, pAPI sql.NullString
			if err := rows.Scan(&pName, &pHost, &pAPI); err == nil {
				log.Printf("[StartupMigration] WARNING: active PMTA profile '%s' (smtp_host=%s) has no api_endpoint — SMTP-only mode is a DMARC risk",
					pName, pHost.String)
			}
		}
	}
}

// runAdminMigrations connects with the RDS master user to run DDL that the
// regular ignite user cannot (table ownership issues). Uses DB_ADMIN_URL env
// var; skips silently if not set.
func runAdminMigrations() {
	adminURL := os.Getenv("DB_ADMIN_URL")
	if adminURL == "" {
		return
	}

	if !strings.Contains(adminURL, "connect_timeout") {
		sep := "?"
		if strings.Contains(adminURL, "?") {
			sep = "&"
		}
		adminURL += sep + "connect_timeout=10"
	}

	adminDB, err := sql.Open("postgres", adminURL)
	if err != nil {
		log.Printf("[AdminMigration] connect error: %v", err)
		return
	}
	defer adminDB.Close()

	adminDB.SetMaxOpenConns(2)
	adminDB.SetConnMaxLifetime(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adminDB.PingContext(ctx); err != nil {
		log.Printf("[AdminMigration] ping error (skipping): %v", err)
		return
	}

	ddlStatements := []struct {
		name string
		sql  string
	}{
		{"add_sending_domain", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_domain VARCHAR(255)`},
		{"add_sending_ip", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_ip VARCHAR(45)`},
		{"idx_sending_domain", `CREATE INDEX IF NOT EXISTS idx_tracking_sending_domain ON mailing_tracking_events(sending_domain)`},
		{"idx_sending_ip", `CREATE INDEX IF NOT EXISTS idx_tracking_sending_ip ON mailing_tracking_events(sending_ip)`},
		{"grant_tracking_to_ignite", `GRANT ALL ON TABLE mailing_tracking_events TO ignite`},
		{"grant_campaigns_to_ignite", `GRANT ALL ON TABLE mailing_campaigns TO ignite`},
		{"add_pmta_config", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS pmta_config JSONB DEFAULT '{}'::jsonb`},
		{"add_isp_quotas_admin", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS isp_quotas JSONB DEFAULT '{}'`},
		{"add_execution_mode_admin", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS execution_mode TEXT DEFAULT 'standard'`},
		{"add_queued_count_admin", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS queued_count INTEGER DEFAULT 0`},
		{"add_hard_bounce_count_admin", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS hard_bounce_count INTEGER DEFAULT 0`},
		{"add_soft_bounce_count_admin", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS soft_bounce_count INTEGER DEFAULT 0`},
		{"create_pmta_draft_index", `CREATE INDEX IF NOT EXISTS idx_mailing_campaigns_pmta_drafts ON mailing_campaigns (organization_id, updated_at DESC) WHERE status = 'draft' AND execution_mode = 'pmta_isp_wave'`},
		{"backfill_sending_domain", `
			UPDATE mailing_tracking_events t
			SET sending_domain = LOWER(SPLIT_PART(c.from_email, '@', 2))
			FROM mailing_campaigns c
			WHERE t.campaign_id = c.id
			  AND (t.sending_domain IS NULL OR t.sending_domain = '')
			  AND c.from_email IS NOT NULL
			  AND c.from_email LIKE '%@%'
		`},
		{"create_auto_fill_sending_domain_fn", `
			CREATE OR REPLACE FUNCTION auto_fill_sending_domain()
			RETURNS TRIGGER AS $$
			BEGIN
			  IF (NEW.sending_domain IS NULL OR NEW.sending_domain = '') AND NEW.campaign_id IS NOT NULL THEN
			    SELECT LOWER(SPLIT_PART(c.from_email, '@', 2))
			    INTO NEW.sending_domain
			    FROM mailing_campaigns c
			    WHERE c.id = NEW.campaign_id
			      AND c.from_email IS NOT NULL AND c.from_email LIKE '%@%';
			  END IF;
			  RETURN NEW;
			END;
			$$ LANGUAGE plpgsql
		`},
		{"create_auto_fill_trigger", `
			DO $$ BEGIN
			  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_auto_fill_sending_domain') THEN
			    CREATE TRIGGER trg_auto_fill_sending_domain
			    BEFORE INSERT ON mailing_tracking_events
			    FOR EACH ROW EXECUTE FUNCTION auto_fill_sending_domain();
			  END IF;
			END $$
		`},
		{"add_recipient_domain", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS recipient_domain VARCHAR(255)`},
		{"idx_recipient_domain", `CREATE INDEX IF NOT EXISTS idx_tracking_recipient_domain ON mailing_tracking_events(recipient_domain)`},
		{"backfill_recipient_domain_from_subscribers", `
			UPDATE mailing_tracking_events t
			SET recipient_domain = LOWER(SPLIT_PART(s.email, '@', 2))
			FROM mailing_subscribers s
			WHERE t.subscriber_id = s.id
			  AND (t.recipient_domain IS NULL OR t.recipient_domain = '')
			  AND s.email LIKE '%@%'
		`},
		{"enable_pg_trgm", `CREATE EXTENSION IF NOT EXISTS pg_trgm`},
		{"idx_tracking_link_url_trgm_noop", `SELECT 1`},
		{"grant_tracking_events_all", `GRANT ALL ON TABLE mailing_tracking_events TO ignite`},
		{"grant_all_mailing_tables", `
			DO $$ DECLARE r record;
			BEGIN
				FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'mailing_%'
				LOOP
					EXECUTE format('ALTER TABLE %I OWNER TO ignite', r.tablename);
					EXECUTE format('GRANT ALL ON TABLE %I TO ignite', r.tablename);
				END LOOP;
				FOR r IN SELECT sequence_name FROM information_schema.sequences WHERE sequence_schema = 'public' AND sequence_name LIKE 'mailing_%'
				LOOP
					EXECUTE format('ALTER SEQUENCE %I OWNER TO ignite', r.sequence_name);
					EXECUTE format('GRANT ALL ON SEQUENCE %I TO ignite', r.sequence_name);
				END LOOP;
			END $$
		`},
		{"add_queue_updated_at", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ DEFAULT NOW()`},
		{"add_queue_locked_at_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ`},
		{"add_queue_worker_id_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS worker_id VARCHAR(100)`},
		{"add_queue_isp_plan_id_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS isp_plan_id UUID`},
		{"add_queue_wave_id_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS wave_id UUID`},
		{"add_queue_recipient_isp_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS recipient_isp VARCHAR(50)`},
		{"add_queue_selection_rank_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS selection_rank INTEGER`},
		{"add_queue_audience_source_type_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS audience_source_type VARCHAR(30)`},
		{"add_queue_audience_source_id_admin", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS audience_source_id UUID`},
		{"create_queue_wave_indexes_admin", `
			CREATE INDEX IF NOT EXISTS idx_queue_wave_id ON mailing_campaign_queue(wave_id);
			CREATE INDEX IF NOT EXISTS idx_queue_plan_id ON mailing_campaign_queue(isp_plan_id);
			CREATE INDEX IF NOT EXISTS idx_queue_campaign_wave_schedule ON mailing_campaign_queue(campaign_id, wave_id, scheduled_at)
		`},
		{"cold_storage_176", `UPDATE mailing_ip_addresses SET status = 'cold', warmup_stage = 'paused', reputation_score = 0, updated_at = NOW() WHERE ip_address = '15.204.22.176'`},
		{"seed_warmup_ip_180", `DO $$
		DECLARE
			org_id UUID := '00000000-0000-0000-0000-000000000001';
			wp_id UUID;
		BEGIN
			SELECT id INTO wp_id FROM mailing_ip_pools WHERE organization_id = org_id AND name = 'warmup-pool';
			IF wp_id IS NOT NULL THEN
				INSERT INTO mailing_ip_addresses (organization_id, ip_address, hostname, pool_id, acquisition_type, hosting_provider, cidr_block, status, warmup_stage, warmup_day, warmup_daily_limit, rdns_verified, reputation_score)
				VALUES (org_id, '15.204.22.180', 'mta5.mail.projectjarvis.io', wp_id, 'purchased', 'OVH', '15.204.22.176/28', 'warmup', 'warming', 1, 200, true, 50.0)
				ON CONFLICT DO NOTHING;
			END IF;
		END $$`},

		{"fix_profiles_to_warmup_pool", `UPDATE mailing_sending_profiles SET ip_pool = 'warmup-pool', updated_at = NOW() WHERE vendor_type = 'pmta' AND ip_pool != 'warmup-pool' AND organization_id = '00000000-0000-0000-0000-000000000001'`},

		{"fix_cold_176_ensure", `UPDATE mailing_ip_addresses SET status = 'cold', warmup_stage = 'paused', reputation_score = 0, updated_at = NOW() WHERE ip_address = '15.204.22.176' AND status != 'cold'`},
		{"bump_warmup_limits_day4", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 1500 WHERE ip_address IN ('15.204.22.177','15.204.22.178','15.204.22.179','15.204.22.180') AND warmup_daily_limit < 1500`},
		{"bump_warmup_limits_day4b", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000 WHERE status IN ('active','warmup') AND warmup_daily_limit < 5000`},
		{"bump_warmup_limits_null", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000 WHERE status IN ('active','warmup') AND warmup_daily_limit IS NULL`},
		{"force_warmup_5000_all", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 5000 WHERE warmup_daily_limit != 5000 OR warmup_daily_limit IS NULL`},
		{"nuke_warmup_limits_v2", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 10000`},
		{"nuke_warmup_log_today", `DELETE FROM mailing_ip_warmup_log WHERE date = CURRENT_DATE`},
		{"nuke_mta1_cold", `UPDATE mailing_ip_addresses SET status = 'cold' WHERE hostname LIKE 'mta1%.mail.projectjarvis.io' OR ip_address::text LIKE '15.204.22.176%'`},

		{"idx_queue_recipient_isp", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_campaign_queue_recipient_isp ON mailing_campaign_queue (campaign_id, recipient_isp, status) WHERE recipient_isp IS NOT NULL`},
		{"idx_queue_campaign_status_queued", `CREATE INDEX IF NOT EXISTS idx_queue_campaign_status_scheduled ON mailing_campaign_queue (campaign_id, status, scheduled_at) WHERE status = 'queued'`},

		{"fix_warmup_ips_177_179_pool", `DO $$
		DECLARE
			org_id UUID := '00000000-0000-0000-0000-000000000001';
			wp_id UUID;
		BEGIN
			SELECT id INTO wp_id FROM mailing_ip_pools WHERE organization_id = org_id AND name = 'warmup-pool';
			IF wp_id IS NOT NULL THEN
				UPDATE mailing_ip_addresses SET pool_id = wp_id, updated_at = NOW()
				WHERE ip_address IN ('15.204.22.177', '15.204.22.178', '15.204.22.179', '15.204.22.180')
				  AND organization_id = org_id
				  AND pool_id != wp_id;
			END IF;
		END $$`},

		// =====================================================================
		// Phase 6 Overrides: Re-establish ISP-aware state after legacy fixes
		// The migrations above (fix_profiles_to_warmup_pool, nuke_warmup_limits_v2,
		// fix_warmup_ips_177_179_pool) predate Phase 6 and undo its changes.
		// These overrides run last to ensure Phase 6 state is authoritative.
		// =====================================================================

		// 2026-05-30: added NOT LIKE 'ses-relay%' guard on the em.discountblog.com
		// branch. Without this, the SES-relay profile's ip_pool='ses-relay-a' was
		// being clobbered to 'db-gmail-pool' on every boot, which broke the bare-pool
		// routing path (see internal/api/mailing_sending.go "bare VMTA pool" branch).
		// The other em.* domains do not yet have SES-relay profiles, so left as-is.
		{"phase6_final_ip_pool_profiles", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET ip_pool = 'db-gmail-pool' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND ip_pool != 'db-gmail-pool' AND COALESCE(ip_pool,'') NOT LIKE 'ses-relay%';
    UPDATE mailing_sending_profiles SET ip_pool = 'qf-gmail-pool' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND ip_pool != 'qf-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'ht-gmail-pool' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND ip_pool != 'ht-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'mh-gmail-pool' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND ip_pool != 'mh-gmail-pool';
END $$`},

		// 2026-05-30: added NOT LIKE 'ses-relay%' guard on the em.discountblog.com
		// branch. This was THE smoking-gun reseed that clobbered the SES-relay
		// profile's pool_prefix='' back to 'db' on every boot — silently flipping
		// 50k-recipient SES-relay campaigns onto the warm OVH pool.
		{"phase6_final_pool_prefix", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET pool_prefix = 'db' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'db') AND COALESCE(ip_pool,'') NOT LIKE 'ses-relay%';
    UPDATE mailing_sending_profiles SET pool_prefix = 'qf' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'qf');
    UPDATE mailing_sending_profiles SET pool_prefix = 'ht' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'ht');
    UPDATE mailing_sending_profiles SET pool_prefix = 'mh' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'mh');
END $$`},

		// m.* SES relay profiles share the same PMTA server and pool
		// infrastructure as their em.* counterparts but were missed by
		// the original Phase 6 overrides — they still had ip_pool =
		// 'warmup-pool' (now empty) and no pool_prefix.
		{"phase6_final_ip_pool_m_profiles", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET ip_pool = 'db-gmail-pool' WHERE sending_domain = 'm.discountblog.com' AND vendor_type = 'pmta' AND ip_pool != 'db-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'qf-gmail-pool' WHERE sending_domain = 'm.quizfiesta.com' AND vendor_type = 'pmta' AND ip_pool != 'qf-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'ht-gmail-pool' WHERE sending_domain = 'm.historythinking.com' AND vendor_type = 'pmta' AND ip_pool != 'ht-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'mh-gmail-pool' WHERE sending_domain = 'm.myownhealth.net' AND vendor_type = 'pmta' AND ip_pool != 'mh-gmail-pool';
END $$`},

		{"phase6_final_pool_prefix_m", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET pool_prefix = 'db' WHERE sending_domain = 'm.discountblog.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'db');
    UPDATE mailing_sending_profiles SET pool_prefix = 'qf' WHERE sending_domain = 'm.quizfiesta.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'qf');
    UPDATE mailing_sending_profiles SET pool_prefix = 'ht' WHERE sending_domain = 'm.historythinking.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'ht');
    UPDATE mailing_sending_profiles SET pool_prefix = 'mh' WHERE sending_domain = 'm.myownhealth.net' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'mh');
END $$`},

		{"phase6_final_25_warmup_limits", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 50 WHERE cidr_block IN ('144.225.178.0/25', '144.225.178.128/25') AND status = 'warmup' AND warmup_daily_limit != 50`},

		{"phase6_final_28_reassign", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            ('15.204.22.177', 'db-gmail-pool'),
            ('15.204.22.178', 'db-yahoo-pool'),
            ('15.204.22.179', 'qf-gmail-pool'),
            ('15.204.22.180', 'qf-yahoo-pool'),
            ('15.204.22.181', 'db-msft-pool'),
            ('15.204.22.182', 'qf-msft-pool'),
            ('15.204.22.183', 'db-apple-pool'),
            ('15.204.22.184', 'qf-apple-pool'),
            ('15.204.22.185', 'db-comcast-pool'),
            ('15.204.22.186', 'qf-comcast-pool'),
            ('15.204.22.187', 'db-att-pool'),
            ('15.204.22.188', 'qf-att-pool'),
            ('15.204.22.189', 'db-cox-pool'),
            ('15.204.22.190', 'qf-charter-pool'),
            ('15.204.22.191', 'db-general-pool')
        ) AS t(ip_addr, pool_name)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
            WHERE ip_address = rec.ip_addr::inet AND pool_id != pool_id_val;
        END IF;
    END LOOP;
END $$`},

		{"phase6_final_176_paused", `UPDATE mailing_ip_addresses SET status = 'paused', updated_at = NOW() WHERE ip_address = '15.204.22.176'::inet AND status NOT IN ('paused')`},

		// ========================================================================
		// Phase 7: Shared pools for test campaigns
		// ========================================================================

		// 7.1: Create shared-a pool on Server A (15 /28 OVH IPs)
		{"phase7_pool_shared_a", `INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status, created_at, updated_at)
			VALUES ('00000000-0000-0000-0000-000000000001', 'shared-a', 'Shared /28 OVH IPs on Server A (15.204.22.177-.191)', 'shared', 'active', NOW(), NOW())
			ON CONFLICT (organization_id, name) DO NOTHING`},

		// 7.2: Create shared-b pool on Server B (16 new OVH IPs)
		{"phase7_pool_shared_b", `INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type, status, created_at, updated_at)
			VALUES ('00000000-0000-0000-0000-000000000001', 'shared-b', 'Shared /28 OVH IPs on Server B (15.204.38.160-.175)', 'shared', 'active', NOW(), NOW())
			ON CONFLICT (organization_id, name) DO NOTHING`},

		// 7.3: Move Server A /28 IPs to shared-a pool and update hostnames to mta-a-shrd naming
		{"phase7_move_ips_to_shared_a", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
    rec RECORD;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'shared-a' AND organization_id = org_id;
    IF pool_id_val IS NULL THEN RAISE NOTICE 'shared-a pool not found'; RETURN; END IF;

    FOR rec IN
        SELECT * FROM (VALUES
            ('15.204.22.177', 'mta-a-shrd1.mail.em.discountblog.com'),
            ('15.204.22.178', 'mta-a-shrd2.mail.em.discountblog.com'),
            ('15.204.22.179', 'mta-a-shrd3.mail.em.discountblog.com'),
            ('15.204.22.180', 'mta-a-shrd4.mail.em.discountblog.com'),
            ('15.204.22.181', 'mta-a-shrd5.mail.em.discountblog.com'),
            ('15.204.22.182', 'mta-a-shrd6.mail.em.discountblog.com'),
            ('15.204.22.183', 'mta-a-shrd7.mail.em.discountblog.com'),
            ('15.204.22.184', 'mta-a-shrd8.mail.em.discountblog.com'),
            ('15.204.22.185', 'mta-a-shrd9.mail.em.quizfiesta.com'),
            ('15.204.22.186', 'mta-a-shrd10.mail.em.quizfiesta.com'),
            ('15.204.22.187', 'mta-a-shrd11.mail.em.quizfiesta.com'),
            ('15.204.22.188', 'mta-a-shrd12.mail.em.quizfiesta.com'),
            ('15.204.22.189', 'mta-a-shrd13.mail.em.quizfiesta.com'),
            ('15.204.22.190', 'mta-a-shrd14.mail.em.quizfiesta.com'),
            ('15.204.22.191', 'mta-a-shrd15.mail.em.quizfiesta.com')
        ) AS t(ip_addr, new_hostname)
    LOOP
        UPDATE mailing_ip_addresses
        SET pool_id = pool_id_val, hostname = rec.new_hostname, updated_at = NOW()
        WHERE ip_address = rec.ip_addr::inet;
    END LOOP;
END $$`},

		// 7.4: Seed Server B /28 IPs into shared-b pool
		{"phase7_seed_shared_b_ips", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
    server_id_val UUID;
    i INT;
    ip_text TEXT;
    hostname_val TEXT;
    shrd_num INT;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'shared-b' AND organization_id = org_id;
    SELECT id INTO server_id_val FROM mailing_pmta_servers WHERE host = '15.204.107.107';
    IF pool_id_val IS NULL OR server_id_val IS NULL THEN RAISE NOTICE 'shared-b pool or server B not found'; RETURN; END IF;

    FOR i IN 0..15 LOOP
        ip_text := '15.204.38.' || (160 + i);
        shrd_num := i + 1;
        IF shrd_num <= 8 THEN
            hostname_val := 'mta-b-shrd' || shrd_num || '.mail.em.historythinking.com';
        ELSE
            hostname_val := 'mta-b-shrd' || shrd_num || '.mail.em.myownhealth.net';
        END IF;

        INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, status, pool_id, pmta_server_id,
            warmup_stage, warmup_day, warmup_daily_limit, warmup_started_at, hosting_provider, acquisition_type,
            cidr_block, rdns_verified, reputation_score, created_at, updated_at)
        VALUES (gen_random_uuid(), org_id, ip_text::inet, hostname_val, 'warmup', pool_id_val, server_id_val,
            'warming', 1, 10000, NOW(), 'OVH', 'purchased', '15.204.38.160/28', false, 50.0, NOW(), NOW())
        ON CONFLICT (ip_address) DO UPDATE SET
            pool_id = pool_id_val, pmta_server_id = server_id_val, hostname = hostname_val,
            warmup_started_at = COALESCE(mailing_ip_addresses.warmup_started_at, NOW()), updated_at = NOW();
    END LOOP;
END $$`},

		// 7.5: Create test sending profiles for shared pools
		{"phase7_profile_test_db_shared", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Test DB Shared', 'pmta', 'DiscountBlog', 'hello@em.discountblog.com', 'reply@em.discountblog.com',
				'em.discountblog.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.discountblog.com',
				3200, 25000, 'shared-a', '', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE name = 'Test DB Shared')`},

		{"phase7_profile_test_qf_shared", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Test QF Shared', 'pmta', 'QuizFiesta', 'hello@em.quizfiesta.com', 'reply@em.quizfiesta.com',
				'em.quizfiesta.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.quizfiesta.com',
				3200, 25000, 'shared-a', '', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE name = 'Test QF Shared')`},

		{"phase7_profile_test_ht_shared", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Test HT Shared', 'pmta', 'History Thinking', 'hello@em.historythinking.com', 'reply@em.historythinking.com',
				'em.historythinking.com', '15.204.107.107', 587, 'http://15.204.107.107:19099', 'trk.em.historythinking.com',
				3200, 25000, 'shared-b', '', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE name = 'Test HT Shared')`},

		{"phase7_profile_test_mh_shared", `INSERT INTO mailing_sending_profiles
			(id, organization_id, name, vendor_type, from_name, from_email, reply_email,
			 sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain,
			 hourly_limit, daily_limit, ip_pool, pool_prefix, status, is_default, created_at, updated_at)
			SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001',
				'Test MH Shared', 'pmta', 'My Own Health', 'hello@em.myownhealth.net', 'reply@em.myownhealth.net',
				'em.myownhealth.net', '15.204.107.107', 587, 'http://15.204.107.107:19099', 'trk.em.myownhealth.net',
				3200, 25000, 'shared-b', '', 'active', false, NOW(), NOW()
			WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE name = 'Test MH Shared')`},

		// 7.6: (superseded by phase 8 in runStartupMigrations — kept as no-ops)

		// =====================================================================
		// Phase 8: Switch to ISP-dedicated pools (IPXO IPs)
		// IPXO PTR/FCrDNS now fully working. Route each domain through its
		// own ISP-specific pools on the correct PMTA server.
		// =====================================================================
		// 2026-05-30: added NOT LIKE 'ses-relay%' guard (duplicate of
		// phase8_startup_route_db — both reseed the same fields). Same
		// root cause as phase6 — see comment there.
		{"phase8_route_db_dedicated", `UPDATE mailing_sending_profiles SET pool_prefix = 'db', smtp_host = '15.204.101.125', api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND status = 'active' AND COALESCE(ip_pool,'') NOT LIKE 'ses-relay%'`},
		{"phase8_route_qf_dedicated", `UPDATE mailing_sending_profiles SET pool_prefix = 'qf', smtp_host = '15.204.101.125', api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND status = 'active'`},
		{"phase8_route_ht_dedicated", `UPDATE mailing_sending_profiles SET pool_prefix = 'ht', smtp_host = '15.204.107.107', api_endpoint = 'http://15.204.107.107:19099', updated_at = NOW() WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND status = 'active'`},
		{"phase8_route_mh_dedicated", `UPDATE mailing_sending_profiles SET pool_prefix = 'mh', smtp_host = '15.204.107.107', api_endpoint = 'http://15.204.107.107:19099', updated_at = NOW() WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND status = 'active'`},
		// 2026-05-30 — Defense in depth: force pool_prefix = '' on every
		// SES-relay profile, regardless of migration order or future
		// regressions. The SaaS routing layer treats pool_prefix='' +
		// ip_pool='ses-relay-a' as the trigger to stamp
		// X-Virtual-MTA: ses-relay-a on PMTA injects (see
		// internal/api/mailing_sending.go and internal/worker/esp_profile.go,
		// "bare-pool fallback"). If pool_prefix is non-empty the SaaS builds
		// per-ISP VMTA pool names like db-gmail-pool and routes via the warm
		// OVH IPs, silently bypassing the SES relay <virtual-mta-pool>.
		// This idempotent reseed runs every boot and is a no-op once the
		// state is correct (the `pool_prefix IS DISTINCT FROM ''` guard
		// prevents row-touch churn).
		{"phase8_ses_relay_clear_pool_prefix", `UPDATE mailing_sending_profiles SET pool_prefix = '', updated_at = NOW() WHERE vendor_type = 'pmta' AND status = 'active' AND COALESCE(ip_pool,'') LIKE 'ses-relay%' AND COALESCE(pool_prefix,'') IS DISTINCT FROM ''`},
		{"phase8_ipxo_warmup_1000", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 1000, updated_at = NOW() WHERE cidr_block IN ('144.225.178.0/25', '144.225.178.128/25') AND warmup_daily_limit < 1000`},

		// =====================================================================
		// Phase 10: Yahoo pool swap — OVH IPs become Yahoo-dedicated,
		// IPXO Yahoo IPs redistribute to other ISP pools, shared pools retired.
		// Rationale: OVH IPs have better Yahoo reputation (TSS04 only),
		// while IPXO IPs hit permanent TSS09 blocks on Yahoo.
		// =====================================================================

		{"phase10_deactivate_shared_pools", `UPDATE mailing_ip_pools SET status = 'inactive', updated_at = NOW() WHERE name IN ('shared-a', 'shared-b') AND status = 'active'`},

		{"phase10_ovh_to_db_yahoo", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = org_id;
    IF pool_id_val IS NOT NULL THEN
        UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
        WHERE ip_address IN ('15.204.22.177'::inet, '15.204.22.178'::inet, '15.204.22.179'::inet, '15.204.22.180'::inet,
                             '15.204.22.181'::inet, '15.204.22.182'::inet, '15.204.22.183'::inet, '15.204.22.184'::inet)
          AND pool_id != pool_id_val;
    END IF;
END $$`},

		{"phase10_ovh_to_qf_yahoo", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = org_id;
    IF pool_id_val IS NOT NULL THEN
        UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
        WHERE ip_address IN ('15.204.22.185'::inet, '15.204.22.186'::inet, '15.204.22.187'::inet, '15.204.22.188'::inet,
                             '15.204.22.189'::inet, '15.204.22.190'::inet, '15.204.22.191'::inet)
          AND pool_id != pool_id_val;
    END IF;
END $$`},

		{"phase10_ovh_to_ht_yahoo", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = org_id;
    IF pool_id_val IS NOT NULL THEN
        UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
        WHERE ip_address IN ('15.204.38.160'::inet, '15.204.38.161'::inet, '15.204.38.162'::inet, '15.204.38.163'::inet,
                             '15.204.38.164'::inet, '15.204.38.165'::inet, '15.204.38.166'::inet, '15.204.38.167'::inet)
          AND pool_id != pool_id_val;
    END IF;
END $$`},

		{"phase10_ovh_to_mh_yahoo", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    pool_id_val UUID;
BEGIN
    SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = org_id;
    IF pool_id_val IS NOT NULL THEN
        UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
        WHERE ip_address IN ('15.204.38.168'::inet, '15.204.38.169'::inet, '15.204.38.170'::inet, '15.204.38.171'::inet,
                             '15.204.38.172'::inet, '15.204.38.173'::inet, '15.204.38.174'::inet, '15.204.38.175'::inet)
          AND pool_id != pool_id_val;
    END IF;
END $$`},

		{"phase10_ipxo_yh_redistribute", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            -- Server A: DB IPXO Yahoo IPs → other ISP pools
            ('144.225.178.7',   'db-gmail-pool'),
            ('144.225.178.8',   'db-msft-pool'),
            ('144.225.178.9',   'db-apple-pool'),
            ('144.225.178.10',  'db-comcast-pool'),
            ('144.225.178.11',  'db-att-pool'),
            ('144.225.178.12',  'db-cox-pool'),
            -- .13 reserved for db-yahoo-pool (Phase 14)
            -- Server A: QF IPXO Yahoo IPs → other ISP pools
            ('144.225.178.71',  'qf-gmail-pool'),
            ('144.225.178.72',  'qf-msft-pool'),
            ('144.225.178.73',  'qf-apple-pool'),
            ('144.225.178.74',  'qf-comcast-pool'),
            ('144.225.178.75',  'qf-att-pool'),
            ('144.225.178.76',  'qf-cox-pool'),
            -- .77 reserved for qf-yahoo-pool (Phase 14)
            -- Server B: HT IPXO Yahoo IPs → other ISP pools
            ('144.225.178.136', 'ht-gmail-pool'),
            ('144.225.178.137', 'ht-msft-pool'),
            ('144.225.178.138', 'ht-apple-pool'),
            ('144.225.178.139', 'ht-comcast-pool'),
            ('144.225.178.140', 'ht-att-pool'),
            ('144.225.178.141', 'ht-cox-pool'),
            ('144.225.178.142', 'ht-charter-pool'),
            -- .143 reserved for ht-yahoo-pool (Phase 14)
            -- Server B: MH IPXO Yahoo IPs → other ISP pools
            ('144.225.178.200', 'mh-gmail-pool'),
            ('144.225.178.201', 'mh-msft-pool'),
            ('144.225.178.202', 'mh-apple-pool'),
            ('144.225.178.203', 'mh-comcast-pool'),
            ('144.225.178.204', 'mh-att-pool'),
            ('144.225.178.205', 'mh-cox-pool'),
            ('144.225.178.206', 'mh-charter-pool')
            -- .207 reserved for mh-yahoo-pool (Phase 14)
        ) AS t(ip_addr, pool_name)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        IF pool_id_val IS NOT NULL THEN
            UPDATE mailing_ip_addresses SET pool_id = pool_id_val, updated_at = NOW()
            WHERE ip_address = rec.ip_addr::inet AND pool_id != pool_id_val;
        END IF;
    END LOOP;
END $$`},

		{"phase10_deactivate_test_shared_profiles", `UPDATE mailing_sending_profiles SET status = 'draft', updated_at = NOW() WHERE name IN ('Test DB Shared', 'Test QF Shared', 'Test HT Shared', 'Test MH Shared') AND status = 'active'`},

		// Phase 11: Promote all OVH Yahoo IPs to active (both servers).
		// These IPs are announced to Yahoo and have good reputation.
		// Active status bypasses warmup daily limits in vmtaPool.next(),
		// ensuring Yahoo mail always routes through OVH IPs. PMTA's own
		// per-domain rate limits (50/h for yahoo.com) handle queuing.
		// Server A: db-yahoo-pool (15.204.22.177-184), qf-yahoo-pool (15.204.22.185-191)
		// Server B: ht-yahoo-pool (15.204.38.160-167), mh-yahoo-pool (15.204.38.168-175)
		{"phase11_promote_db_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.22.177'::inet, '15.204.22.178'::inet, '15.204.22.179'::inet, '15.204.22.180'::inet,
			                     '15.204.22.181'::inet, '15.204.22.182'::inet, '15.204.22.183'::inet, '15.204.22.184'::inet)
			  AND status = 'warmup'`},
		{"phase11_promote_qf_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.22.185'::inet, '15.204.22.186'::inet, '15.204.22.187'::inet, '15.204.22.188'::inet,
			                     '15.204.22.189'::inet, '15.204.22.190'::inet, '15.204.22.191'::inet)
			  AND status = 'warmup'`},
		{"phase11_promote_ht_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.38.160'::inet, '15.204.38.161'::inet, '15.204.38.162'::inet, '15.204.38.163'::inet,
			                     '15.204.38.164'::inet, '15.204.38.165'::inet, '15.204.38.166'::inet, '15.204.38.167'::inet)
			  AND status = 'warmup'`},
		{"phase11_promote_mh_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.38.168'::inet, '15.204.38.169'::inet, '15.204.38.170'::inet, '15.204.38.171'::inet,
			                     '15.204.38.172'::inet, '15.204.38.173'::inet, '15.204.38.174'::inet, '15.204.38.175'::inet)
			  AND status = 'warmup'`},
		{"phase11_force_promote_db_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.22.177'::inet, '15.204.22.178'::inet, '15.204.22.179'::inet, '15.204.22.180'::inet,
			                     '15.204.22.181'::inet, '15.204.22.182'::inet, '15.204.22.183'::inet, '15.204.22.184'::inet)
			  AND status != 'active'`},
		{"phase11_force_promote_qf_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.22.185'::inet, '15.204.22.186'::inet, '15.204.22.187'::inet, '15.204.22.188'::inet,
			                     '15.204.22.189'::inet, '15.204.22.190'::inet, '15.204.22.191'::inet)
			  AND status != 'active'`},
		{"phase11_force_promote_ht_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.38.160'::inet, '15.204.38.161'::inet, '15.204.38.162'::inet, '15.204.38.163'::inet,
			                     '15.204.38.164'::inet, '15.204.38.165'::inet, '15.204.38.166'::inet, '15.204.38.167'::inet)
			  AND status != 'active'`},
		{"phase11_force_promote_mh_yahoo_ips", `UPDATE mailing_ip_addresses SET status = 'active', updated_at = NOW()
			WHERE ip_address IN ('15.204.38.168'::inet, '15.204.38.169'::inet, '15.204.38.170'::inet, '15.204.38.171'::inet,
			                     '15.204.38.172'::inet, '15.204.38.173'::inet, '15.204.38.174'::inet, '15.204.38.175'::inet)
			  AND status != 'active'`},

		{"phase11_reset_all_quarantined_ips", `UPDATE mailing_ip_addresses ip
			SET status = 'active', updated_at = NOW()
			FROM mailing_ip_pools pool
			WHERE ip.pool_id = pool.id
			  AND pool.status = 'active'
			  AND ip.status NOT IN ('active', 'paused', 'cold')
			  AND ip.ip_address != '15.204.22.176'::inet`},

		{"idx_plan_recipients_campaign", `CREATE INDEX IF NOT EXISTS idx_campaign_plan_recipients_campaign ON mailing_campaign_plan_recipients(campaign_id)`},
		{"idx_isp_time_spans_campaign", `CREATE INDEX IF NOT EXISTS idx_campaign_isp_time_spans_campaign ON mailing_campaign_isp_time_spans(campaign_id)`},

		// Bounce classification: add reputation_blocked column to separate provider blocks from true hard bounces
		{"add_reputation_blocked_col", `ALTER TABLE pmta_acct_daily_summary ADD COLUMN IF NOT EXISTS reputation_blocked INT NOT NULL DEFAULT 0`},

		// MH Gmail Recovery: pause 5 blocked MH Gmail IPs, keeping only .192 (mta-mh-gm1) and .193 (mta-mh-gm2)
		{"mh_gmail_recovery_pause_blocked_ips", `UPDATE mailing_ip_addresses
			SET status = 'paused', updated_at = NOW()
			WHERE ip_address IN (
				'144.225.178.194'::inet,
				'144.225.178.195'::inet,
				'144.225.178.196'::inet,
				'144.225.178.198'::inet,
				'144.225.178.199'::inet
			) AND status NOT IN ('paused', 'cold')`},

		// IP pool preferences for ops-controlled isolation
		{"create_ip_pool_preferences", `CREATE TABLE IF NOT EXISTS mailing_ip_pool_preferences (
			pool_id UUID REFERENCES mailing_ip_pools(id),
			preferred_ip_id UUID REFERENCES mailing_ip_addresses(id),
			standby_ip_id UUID REFERENCES mailing_ip_addresses(id),
			isolation_mode VARCHAR(20) DEFAULT 'normal' CHECK (isolation_mode IN ('normal', 'strict')),
			reason TEXT,
			set_by TEXT DEFAULT 'manual',
			set_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (pool_id)
		)`},

		// Add isolation_mode column to mailing_ip_pools
		{"add_pool_isolation_mode", `ALTER TABLE mailing_ip_pools ADD COLUMN IF NOT EXISTS isolation_mode VARCHAR(20) DEFAULT 'normal'`},

		// Add retry_after column to queue tables for strict-pool backoff scheduling
		{"add_queue_retry_after", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS retry_after TIMESTAMPTZ`},
		{"add_queue_v2_retry_after", `ALTER TABLE IF EXISTS mailing_campaign_queue_v2 ADD COLUMN IF NOT EXISTS retry_after TIMESTAMPTZ`},

		// Phase 20: Insert the 4 missing Yahoo IPXO IPs that were "reserved" but never inserted.
		// Phases 12-15 only had UPDATE statements for these IPs, which silently affected 0 rows.
		// Phase 19 deleted the OVH IPs that previously filled Yahoo pools, leaving them empty.
		// Without these rows, vmtaPool.next("yahoo") falls through to non-Yahoo VMTAs.
		{"phase20_insert_yahoo_ips", `DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    rec RECORD;
    pool_id_val UUID;
    server_id_val UUID;
BEGIN
    FOR rec IN
        SELECT * FROM (VALUES
            ('144.225.178.13',  'mta-db-yh8.mail.em.discountblog.com',    'db-yahoo-pool',  '15.204.101.125', '144.225.178.0/25'),
            ('144.225.178.77',  'mta-qf-yh8.mail.em.quizfiesta.com',     'qf-yahoo-pool',  '15.204.101.125', '144.225.178.0/25'),
            ('144.225.178.143', 'mta-ht-yh8.mail.em.historythinking.com', 'ht-yahoo-pool',  '15.204.107.107', '144.225.178.128/25'),
            ('144.225.178.207', 'mta-mh-yh8.mail.em.myownhealth.net',    'mh-yahoo-pool',  '15.204.107.107', '144.225.178.128/25')
        ) AS t(ip_addr, hostname, pool_name, server_host, cidr)
    LOOP
        SELECT id INTO pool_id_val FROM mailing_ip_pools WHERE name = rec.pool_name AND organization_id = org_id;
        SELECT id INTO server_id_val FROM mailing_pmta_servers WHERE host = rec.server_host;
        IF pool_id_val IS NOT NULL AND server_id_val IS NOT NULL THEN
            INSERT INTO mailing_ip_addresses (id, organization_id, ip_address, hostname, status, pool_id, pmta_server_id,
                warmup_stage, warmup_day, warmup_daily_limit, warmup_started_at, hosting_provider, acquisition_type,
                cidr_block, rdns_verified, created_at, updated_at)
            VALUES (gen_random_uuid(), org_id, rec.ip_addr::inet, rec.hostname, 'warmup', pool_id_val, server_id_val,
                'warming', 1, 5000, NOW(), 'ovh', 'leased', rec.cidr, false, NOW(), NOW())
            ON CONFLICT (ip_address) DO UPDATE SET
                hostname = EXCLUDED.hostname,
                pool_id = EXCLUDED.pool_id,
                pmta_server_id = EXCLUDED.pmta_server_id,
                warmup_daily_limit = GREATEST(mailing_ip_addresses.warmup_daily_limit, 5000),
                updated_at = NOW();
        END IF;
    END LOOP;
END $$`},

		// NOTE: may12_wf_pool_reassert lived here in commit 4e11010 but was
		// hot-fixed 2026-05-12 evening into runStartupMigrations() because
		// production ECS does not set DB_ADMIN_URL → this entire function
		// is a silent no-op there. See the new entry at the end of the
		// runStartupMigrations slice.
	}

	var ok, fail int
	for _, s := range ddlStatements {
		if _, err := adminDB.ExecContext(ctx, s.sql); err != nil {
			log.Printf("[AdminMigration] %s: ERROR %v", s.name, err)
			fail++
		} else {
			ok++
		}
	}
	log.Printf("[AdminMigration] Complete: %d OK, %d errors", ok, fail)
}

func vmtaShort(hostname string) string {
	if idx := strings.Index(hostname, "."); idx > 0 {
		return hostname[:idx]
	}
	return hostname
}

// pipelineAlerterAdapter bridges worker.PipelineNotifier to engine.Alerter.
type pipelineAlerterAdapter struct {
	alerter *engine.Alerter
}

func (a *pipelineAlerterAdapter) SendPipelineReport(report worker.PipelineReport) error {
	ar := engine.PipelineRunReport{
		RunID:            report.RunID,
		StartedAt:        report.StartedAt,
		CompletedAt:      report.CompletedAt,
		FilesProcessed:   report.FilesProcessed,
		EmailsTotal:      report.EmailsTotal,
		EmailsVerified:   report.EmailsVerified,
		EmailsSuppressed: report.EmailsSuppressed,
		EmailsDeduped:    report.EmailsDeduped,
		Errors:           report.Errors,
	}
	for _, d := range report.DomainBreakdown {
		ar.DomainBreakdown = append(ar.DomainBreakdown, engine.PipelineDomainStat{
			SendingDomain: d.SendingDomain,
			ISP:           d.ISP,
			ListName:      d.ListName,
			Added:         d.Added,
			Suppressed:    d.Suppressed,
			Deduped:       d.Deduped,
		})
	}
	return a.alerter.SendPipelineReport(ar)
}
