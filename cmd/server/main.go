package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/api"
	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/ignite/sparkpost-monitor/internal/azure"
	"github.com/ignite/sparkpost-monitor/internal/buildinfo"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/datainjections"
	"github.com/ignite/sparkpost-monitor/internal/datanorm"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/financial"
	"github.com/ignite/sparkpost-monitor/internal/intelligence"
	"github.com/ignite/sparkpost-monitor/internal/kanban"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/snowflake"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
	"github.com/ignite/sparkpost-monitor/internal/worker"

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

func main() {
	log.Println("╔════════════════════════════════════════════════════════════╗")
	log.Println("║  IGNITE Production Server (cmd/server/main.go)            ║")
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

	// Start the SparkPost collector in background
	ctx, cancel := context.WithCancel(context.Background())
	go spCollector.Start(ctx)

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

	// Initialize Mailing Platform with PostgreSQL
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
		mailingDB, err := sql.Open("postgres", dbURL)
		if err != nil {
			log.Printf("Warning: Failed to connect to mailing database: %v", err)
		} else {
			// Set OpenAI config for AI-powered features (subject suggestions, etc.)
			server.SetOpenAIConfig(cfg.OpenAI)

			// Initialize Image CDN S3 client before registering routes
			{
				imgBucket := os.Getenv("IGNITE_S3_BUCKET")
				if imgBucket == "" && cfg.Storage.S3Bucket != "" {
					imgBucket = cfg.Storage.S3Bucket
				}
				if imgBucket != "" {
					imgRegion := cfg.Storage.AWSRegion
					if imgRegion == "" {
						imgRegion = "us-east-1"
					}
					awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(imgRegion))
					if err != nil {
						log.Printf("WARNING: Failed to load AWS config for Image CDN: %v", err)
					} else {
						imgS3Client := s3.NewFromConfig(awsCfg)
						imgCDNDomain := os.Getenv("IMAGE_CDN_DOMAIN")
						server.SetImageCDNConfig(imgS3Client, imgBucket, imgCDNDomain, imgRegion)
						log.Printf("Image CDN initialized: bucket=%s, cdn=%s", imgBucket, imgCDNDomain)
					}
				}
			}

			// Initialize Redis BEFORE mailing routes so throttle routes register inside the group
			var redisClient *redis.Client
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

			// Register mailing routes (reads s.redisClient for throttle routes)
			server.SetMailingDB(mailingDB)
			log.Println("Mailing Platform routes registered")

			mailingDB.SetMaxOpenConns(25)
			mailingDB.SetMaxIdleConns(15)
			mailingDB.SetConnMaxLifetime(5 * time.Minute)
			mailingDB.SetConnMaxIdleTime(2 * time.Minute)

			// Test connection with timeout — only start background workers if DB is reachable
			pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
			if err := mailingDB.PingContext(pingCtx); err != nil {
				pingCancel()
				log.Printf("Warning: Mailing database ping failed: %v — routes registered but workers skipped", err)
			} else {
				pingCancel()
				log.Println("Mailing Platform database connected successfully")

				// Run critical schema migrations at startup (DB is inside VPC,
				// so migrations must run from the server, not the CI runner).
				runStartupMigrations(mailingDB)
				runAdminMigrations()

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
				if pmtaWaveQueueURL != "" {
					awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
					if err != nil {
						log.Printf("Warning: AWS config for PMTA wave SQS failed: %v", err)
					} else {
						pmtaWaveSQSClient = sqs.NewFromConfig(awsCfg)
						pmtaWaveConsumer := worker.NewPMTAWaveConsumer(pmtaWaveSQSClient, pmtaWaveQueueURL, mailingDB)
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
				sendWorkerPool := worker.NewSendWorkerPool(mailingDB, 10)
				profileSender := worker.NewProfileBasedSender(mailingDB)
				sendWorkerPool.SetESPSenders(profileSender, profileSender, profileSender, profileSender)

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

				sendWorkerPool.Start()
				log.Printf("SendWorkerPool: Starting 10 workers (batch_size=100)")

				// Start Queue Recovery Worker (reclaims stuck items from crashed workers)
				queueRecovery := worker.NewQueueRecoveryWorker(mailingDB)
				go queueRecovery.Start(ctx)
				log.Println("Queue Recovery Worker started (scans every 2m for stuck items, max 5 retries)")

				// Start Data Cleanup Worker (removes old queue items, tracking events, agent decisions)
				dataCleanup := worker.NewDataCleanupWorker(mailingDB)
				go dataCleanup.Start(ctx)
				log.Println("Data Cleanup Worker started (runs every 1h, batch deletes old data)")

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

				// Ensure workers stop on shutdown (H12)
				go func() {
					<-ctx.Done()
					campaignScheduler.Stop()
					sendWorkerPool.Stop()
					if trackingConsumer != nil {
						trackingConsumer.Stop()
					}
					if normalizer != nil {
						normalizer.Stop()
					}
					if redisClient != nil {
						redisClient.Close()
					}
				}()
			}
		}
	} else {
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

	s3Bucket := os.Getenv("IGNITE_S3_BUCKET")
	s3Prefix := os.Getenv("IGNITE_S3_PREFIX")
	s3EncKey := os.Getenv("IGNITE_S3_ENCRYPTION_KEY") // Base64-encoded 32-byte AES-256 key
	useAWSOnly := os.Getenv("IGNITE_USE_AWS_ONLY") == "true"

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

	// Register comprehensive health routes (must be after all Set* calls so
	// the checker can access db, redis, s3, etc.)
	server.RegisterHealthRoutes()
	log.Println("Health check routes registered: /health, /health/live, /health/ready")

	// Setup graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Server.GetHost(), cfg.Server.Port)
		log.Printf("Starting server on %s", addr)
		if err := server.ListenAndServe(addr); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	log.Println("All services initialized — server is ready")

	<-done
	log.Println("Shutting down...")

	// Cancel background tasks
	cancel()

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	log.Println("Server stopped")
}

// runStartupMigrations applies critical schema fixes that must run before
// the scheduler and send workers start. These are idempotent and safe to
// re-run on every boot.
func runStartupMigrations(db *sql.DB) {
	migrations := []struct {
		name string
		sql  string
	}{
		// Ensure tracking events table has all required columns
		// Ensure partition exists for current month
		{"create_tracking_partition_mar26", `CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_03 PARTITION OF mailing_tracking_events FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')`},
		{"add_tracking_email_col", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT`},
		{"add_tracking_event_time_col", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS event_time TIMESTAMPTZ DEFAULT NOW()`},
		{"add_tracking_metadata_col", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}'`},
		{"add_tracking_is_unique_col", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS is_unique BOOLEAN DEFAULT false`},
		// Drop restrictive event_type constraint so hard_bounce, soft_bounce, delivered etc. can be stored
		{"drop_tracking_evt_chk", `ALTER TABLE mailing_tracking_events DROP CONSTRAINT IF EXISTS mailing_tracking_events_event_type_check`},
		// Ensure inbox profiles has email column
		{"add_inbox_email_col", `ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS email TEXT`},
		{"drop_status_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`},
		{"drop_type_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_campaign_type_check`},
		{"drop_send_type_chk", `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_send_type_check`},
		{"widen_status_col", `ALTER TABLE mailing_campaigns ALTER COLUMN status TYPE TEXT`},
		{"readd_status_chk", `ALTER TABLE mailing_campaigns ADD CONSTRAINT mailing_campaigns_status_check CHECK (status IN ('draft','scheduled','preparing','sending','paused','completed','completed_with_errors','cancelled','failed','deleted','sent'))`},
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
		{"add_queue_locked_at", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ`},
		{"add_queue_worker_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS worker_id VARCHAR(100)`},
		{"add_queue_isp_plan_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS isp_plan_id UUID`},
		{"add_queue_wave_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS wave_id UUID`},
		{"add_queue_recipient_isp", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS recipient_isp VARCHAR(50)`},
		{"add_queue_selection_rank", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS selection_rank INTEGER`},
		{"add_queue_audience_source_type", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS audience_source_type VARCHAR(30)`},
		{"add_queue_audience_source_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS audience_source_id UUID`},
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
		{"reset_orphaned_sending_v2", `UPDATE mailing_campaigns SET status = 'cancelled', completed_at = NOW(), updated_at = NOW() WHERE status = 'sending' AND NOT EXISTS (SELECT 1 FROM mailing_campaign_queue q WHERE q.campaign_id = mailing_campaigns.id AND q.status IN ('queued','sending','claimed'))`},
		{"unstick_locked_queue_items", `UPDATE mailing_campaign_queue SET status = 'queued', worker_id = NULL, locked_at = NULL WHERE status = 'sending' AND locked_at < NOW() - INTERVAL '10 minutes'`},
		// Ensure api_endpoint column exists before referencing it
		{"add_api_endpoint_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS api_endpoint VARCHAR(500)`},
		// One-time fix: profiles seeded with wrong host 178.128.215.13 → correct OVH PMTA server
		{"fix_pmta_host_178_to_ovh", `UPDATE mailing_sending_profiles SET smtp_host = '15.204.101.125', api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE vendor_type = 'pmta' AND smtp_host = '178.128.215.13'`},
		// Ensure PMTA profiles with correct host also have api_endpoint set
		{"set_api_endpoint_for_ovh", `UPDATE mailing_sending_profiles SET api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE vendor_type = 'pmta' AND smtp_host = '15.204.101.125' AND (api_endpoint IS NULL OR api_endpoint = '')`},
		{"fix_pmta_bridge_port_19099", `UPDATE mailing_sending_profiles SET api_endpoint = 'http://15.204.101.125:19099', updated_at = NOW() WHERE vendor_type = 'pmta' AND smtp_host = '15.204.101.125' AND api_endpoint != 'http://15.204.101.125:19099'`},
		// Seed quizfiesta.com profile if not present
		{"seed_quizfiesta_profile", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'QuizFiesta PMTA', 'pmta', 'QuizFiesta', 'hello@em.quizfiesta.com', 'reply@em.quizfiesta.com', 'em.quizfiesta.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 3200, 25000, 'warmup-pool', 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.quizfiesta.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
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
		{"reset_emergency_agents", `UPDATE mailing_isp_agents SET status = 'active', updated_at = NOW() WHERE agent_type = 'emergency' AND status = 'firing'`},
		{"add_tracking_sending_domain", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_domain VARCHAR(255)`},
		{"add_tracking_sending_ip", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_ip VARCHAR(45)`},
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
		{"create_auto_fill_sending_domain_fn_startup", `
			CREATE OR REPLACE FUNCTION auto_fill_sending_domain()
			RETURNS TRIGGER AS $$
			BEGIN
			  IF NEW.sending_domain IS NULL OR NEW.sending_domain = '' THEN
			    SELECT LOWER(SPLIT_PART(c.from_email, '@', 2)) INTO NEW.sending_domain
			    FROM mailing_campaigns c
			    WHERE c.id = NEW.campaign_id
			      AND c.from_email IS NOT NULL AND c.from_email LIKE '%@%';
			  END IF;
			  RETURN NEW;
			END;
			$$ LANGUAGE plpgsql
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
		{"consolidate_suppression_entries", `
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
			ON CONFLICT DO NOTHING
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
	}

	var ok, fail int
	for _, m := range migrations {
		if _, err := db.Exec(m.sql); err != nil {
			log.Printf("[StartupMigration] %s: ERROR %v", m.name, err)
			fail++
		} else {
			ok++
		}
	}
	log.Printf("[StartupMigration] Complete: %d OK, %d errors", ok, fail)
}

// runAdminMigrations connects with the RDS master user to run DDL that the
// regular ignite user cannot (table ownership issues). Uses DB_ADMIN_URL env
// var; skips silently if not set.
func runAdminMigrations() {
	adminURL := os.Getenv("DB_ADMIN_URL")
	if adminURL == "" {
		return
	}

	adminDB, err := sql.Open("postgres", adminURL)
	if err != nil {
		log.Printf("[AdminMigration] connect error: %v", err)
		return
	}
	defer adminDB.Close()

	adminDB.SetMaxOpenConns(2)
	adminDB.SetConnMaxLifetime(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := adminDB.PingContext(ctx); err != nil {
		log.Printf("[AdminMigration] ping error: %v", err)
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
		{"grant_tracking_events_all", `GRANT ALL ON TABLE mailing_tracking_events TO ignite`},
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
