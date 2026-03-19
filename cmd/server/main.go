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
	"github.com/ignite/sparkpost-monitor/internal/mailing"
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

//go:embed embed/suppress_besmed_bounces.sql
var besmedSuppressionSQL string

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
				// Admin migrations run first to fix table ownership, then startup
				// migrations can ALTER tables owned by the app user.
				runAdminMigrations()
				runStartupMigrations(mailingDB)

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
					log.Println("ISP rate registry wired to send worker pool")
				}

				sendWorkerPool.Start()

				// Start Queue Recovery Worker (reclaims stuck items from crashed workers)
				queueRecovery := worker.NewQueueRecoveryWorker(mailingDB)
				go queueRecovery.Start(ctx)
				log.Println("Queue Recovery Worker started (scans every 2m for stuck items, max 5 retries)")

				// Start Data Cleanup Worker (removes old queue items, tracking events, agent decisions)
				dataCleanup := worker.NewDataCleanupWorker(mailingDB)
				go dataCleanup.Start(ctx)
				log.Println("Data Cleanup Worker started (runs every 1h, batch deletes old data)")

				// Start Segment Refresh Worker (recalculates dynamic segment subscriber counts)
				segRefresh := worker.NewSegmentRefreshWorker(mailingDB, 30*time.Minute)
				segRefresh.Start(ctx)
				log.Println("Segment Refresh Worker started (recalculates dynamic segments every 30m)")

				// Start List Refresh Worker (updates subscriber_count and mailed_to on lists)
				listRefresh := worker.NewListRefreshWorker(mailingDB, 1*time.Hour)
				listRefresh.Start(ctx)
				log.Println("List Refresh Worker started (updates list counts every 1h)")

				// Start Content Refresh Worker (pre-generates wave email content nightly)
				contentRefresh := worker.NewContentRefreshWorker(mailingDB, 24*time.Hour)
				contentRefresh.RegisterBrand(worker.ContentBrand{
					Key: "discountblog", BlogDomain: "discountblog.com",
					SendingDomain: "em.discountblog.com", BrandName: "Discount Blog",
					CampaignType: "newsletter",
					Voice: `You are writing as "Jamie" from Discount Blog — a relatable, practical person who genuinely loves saving money and sharing what works. First-person storytelling. You say things like "My wife and I tried this..." and "Here's what actually worked for our family." Pull specific numbers from the articles — dollar amounts, percentages, timeframes. Never generic. Every sentence should teach or reveal something useful. Tone: warm, honest, slightly conspiratorial (like sharing a secret with a friend). NOT salesy, NOT clickbaity, NOT corporate. Think: personal finance blog meets friendly text message.`,
					Audience: `Budget-conscious families, young professionals figuring out adulting, and savvy deal hunters. They're busy, skeptical of hype, and want actionable advice — not listicles. They read Discount Blog because it feels real, not manufactured.`,
					HTMLTemplate: api.DiscountBlogHTMLTemplate,
				})
				contentRefresh.RegisterBrand(worker.ContentBrand{
					Key: "quizfiesta", BlogDomain: "quizfiesta.com",
					SendingDomain: "em.quizfiesta.com", BrandName: "QuizFiesta",
					CampaignType: "trivia",
					Voice: `You are the voice of QuizFiesta — a retro-arcade trivia platform. Write like an arcade machine that gained sentience and got really into hyping people up. Short punchy sentences. Direct challenges to the reader. Arcade/gaming lingo: "INSERT COIN", "GAME OVER", "player", "high score", "streak", "level up." Competitive but encouraging — you want them to play, not feel bad. Use ALL CAPS sparingly for emphasis on key words (one per paragraph max). Think: the text on an arcade cabinet's attract screen mixed with a friend trash-talking you at game night.`,
					Audience: `Trivia lovers, casual gamers, friend groups who want game night content, and competitive players chasing leaderboard spots. They're on their phone, they have 2 minutes, and you need to get them excited enough to tap PLAY.`,
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
					Voice: `You are writing as "Jamie" from Discount Blog — a relatable, practical person who genuinely loves saving money and sharing what works. First-person storytelling. Warm, honest, slightly conspiratorial (like sharing a secret with a friend). NOT salesy, NOT clickbaity, NOT corporate.`,
					Audience: `Brand-new subscribers who just signed up. They're curious but uncommitted — this is the first impression. They need to see immediate value and understand what they'll get by staying subscribed.`,
					HTMLTemplate: api.DiscountBlogWelcomeHTMLTemplate,
				})
				contentRefresh.RegisterBrand(worker.ContentBrand{
					Key: "quizfiesta", BlogDomain: "quizfiesta.com",
					SendingDomain: "em.quizfiesta.com", BrandName: "QuizFiesta",
					CampaignType: "welcome",
					Voice: `You are the voice of QuizFiesta — a retro-arcade trivia platform. Short punchy sentences. Arcade/gaming lingo. Competitive but encouraging — you want them to play, not feel bad.`,
					Audience: `Brand-new players who just signed up. They've never played before. Get them excited to try their first game — make them feel like they've entered an arcade.`,
					HTMLTemplate: api.QuizFiestaWelcomeHTMLTemplate,
					FallbackContent: []mailing.BlogExcerpt{
						{Title: "Classic Mode — Test Your Knowledge", Excerpt: "15 questions. 30 seconds each. Streak multipliers and adaptive difficulty.", URL: "https://quizfiesta.com/play"},
						{Title: "Survival Mode — One Wrong Answer and It's Over", Excerpt: "3 lives. Questions get harder the longer you last.", URL: "https://quizfiesta.com/play"},
						{Title: "Speed Run — 30 Seconds. Go.", Excerpt: "The clock is ticking. Answer as many as you can before time runs out.", URL: "https://quizfiesta.com/play"},
						{Title: "Multiplayer Duels — Settle It in Real-Time", Excerpt: "Share a room code. Go head-to-head. Real-time trivia battles.", URL: "https://quizfiesta.com/play"},
					},
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

				// Ensure workers stop on shutdown (H12)
				go func() {
					<-ctx.Done()
					campaignScheduler.Stop()
					sendWorkerPool.Stop()
					offerSuppWorker.Stop()
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

	// Register early health endpoint so ALB health checks pass while slow
	// integrations (Mailgun, SES, Ongage, Everflow, Snowflake, etc.) initialize.
	server.RegisterHealthRoutes()
	log.Println("Health check routes registered: /health, /health/live, /health/ready")

	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Server.GetHost(), cfg.Server.Port)
		log.Printf("Starting server on %s (early — integration init continues in background)", addr)
		if err := server.ListenAndServe(addr); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

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
		{"ensure_tracking_email_col", `DO $$
		DECLARE
			part regclass;
		BEGIN
			ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT;
			FOR part IN
				SELECT inhrelid::regclass
				FROM pg_inherits
				WHERE inhparent = 'public.mailing_tracking_events'::regclass
			LOOP
				EXECUTE format('ALTER TABLE %s ADD COLUMN IF NOT EXISTS email TEXT', part);
			END LOOP;
		END $$`},
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
		// Seed em.discountblog.com PMTA profile (mirrors em.quizfiesta.com setup)
		{"seed_pmta_discountblog_profile", `INSERT INTO mailing_sending_profiles (id, organization_id, name, vendor_type, from_name, from_email, reply_email, sending_domain, smtp_host, smtp_port, api_endpoint, tracking_domain, hourly_limit, daily_limit, ip_pool, status, is_default, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'DiscountBlog PMTA (em)', 'pmta', 'Jamie @ Discount Blog', 'hello@em.discountblog.com', 'reply@em.discountblog.com', 'em.discountblog.com', '15.204.101.125', 587, 'http://15.204.101.125:19099', 'trk.em.discountblog.com', 3200, 25000, 'warmup-pool', 'active', false, NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_sending_profiles WHERE sending_domain = 'em.discountblog.com' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
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

		{"add_subscriber_isp_col", `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS isp VARCHAR(20) DEFAULT ''`},
		{"idx_subscriber_isp", `CREATE INDEX IF NOT EXISTS idx_subscribers_isp ON mailing_subscribers(isp) WHERE isp != ''`},

		{"startup_warmup_limits_10k", `UPDATE mailing_ip_addresses SET warmup_daily_limit = 10000 WHERE warmup_daily_limit < 10000 AND status IN ('active', 'warmup')`},
		{"drop_ip_status_check", `ALTER TABLE mailing_ip_addresses DROP CONSTRAINT IF EXISTS mailing_ip_addresses_status_check`},
		{"startup_mta1_cold", `UPDATE mailing_ip_addresses SET status = 'cold', warmup_stage = 'paused' WHERE (hostname LIKE 'mta1%' OR ip_address::text LIKE '15.204.22.176%') AND status != 'cold'`},

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

		{"add_campaign_queue_offer_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS offer_id UUID`},
		{"add_campaign_queue_creative_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS creative_id UUID`},
		{"add_campaign_queue_subject_line_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
		{"add_campaign_queue_from_name_id", `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS from_name_id UUID`},

		{"add_tracking_events_offer_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS offer_id UUID`},
		{"add_tracking_events_creative_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS creative_id UUID`},
		{"add_tracking_events_subject_line_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS subject_line_id UUID`},
		{"add_tracking_events_from_name_id", `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS from_name_id UUID`},

		{"add_campaigns_offer_id", `ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS offer_id UUID`},

		// =====================================================================
		// Phase 6: Two-PMTA Multi-Server Infrastructure
		// =====================================================================

		// Step 6.1: Add pool_prefix column for ISP-aware pool routing
		{"add_pool_prefix_col", `ALTER TABLE mailing_sending_profiles ADD COLUMN IF NOT EXISTS pool_prefix TEXT DEFAULT ''`},

		// Step 6.2: Seed PMTA servers
		{"seed_pmta_server_a", `INSERT INTO mailing_pmta_servers (id, organization_id, name, host, smtp_port, mgmt_port, mgmt_api_key, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'PMTA Server A (OVH Original)', '15.204.101.125', 587, 19000, '', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_pmta_servers WHERE host = '15.204.101.125')`},
		{"seed_pmta_server_b", `INSERT INTO mailing_pmta_servers (id, organization_id, name, host, smtp_port, mgmt_port, mgmt_api_key, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'PMTA Server B (OVH New)', '15.204.107.107', 587, 19000, '', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_pmta_servers WHERE host = '15.204.107.107')`},

		// Step 6.3: Seed 36 ISP sub-pools (9 ISPs x 4 domains)
		{"seed_pool_db_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-gmail-pool', 'DiscountBlog Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-yahoo-pool', 'DiscountBlog Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-msft-pool', 'DiscountBlog Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-apple-pool', 'DiscountBlog Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-comcast-pool', 'DiscountBlog Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-att-pool', 'DiscountBlog ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-cox-pool', 'DiscountBlog Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-charter-pool', 'DiscountBlog Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_db_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'db-general-pool', 'DiscountBlog General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'db-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-gmail-pool', 'QuizFiesta Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-yahoo-pool', 'QuizFiesta Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-msft-pool', 'QuizFiesta Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-apple-pool', 'QuizFiesta Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-comcast-pool', 'QuizFiesta Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-att-pool', 'QuizFiesta ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-cox-pool', 'QuizFiesta Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-charter-pool', 'QuizFiesta Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_qf_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'qf-general-pool', 'QuizFiesta General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'qf-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-gmail-pool', 'HistoryThinking Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-yahoo-pool', 'HistoryThinking Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-msft-pool', 'HistoryThinking Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-apple-pool', 'HistoryThinking Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-comcast-pool', 'HistoryThinking Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-att-pool', 'HistoryThinking ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_cox", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-cox-pool', 'HistoryThinking Cox ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-cox-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_charter", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-charter-pool', 'HistoryThinking Charter ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-charter-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_ht_general", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'ht-general-pool', 'HistoryThinking General ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'ht-general-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_gmail", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-gmail-pool', 'MyOwnHealth Gmail ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-gmail-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_yahoo", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-yahoo-pool', 'MyOwnHealth Yahoo ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-yahoo-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_msft", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-msft-pool', 'MyOwnHealth Microsoft ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-msft-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_apple", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-apple-pool', 'MyOwnHealth Apple ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-apple-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_comcast", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-comcast-pool', 'MyOwnHealth Comcast ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-comcast-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
		{"seed_pool_mh_att", `INSERT INTO mailing_ip_pools (id, organization_id, name, description, pool_type, status, created_at, updated_at) SELECT gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 'mh-att-pool', 'MyOwnHealth ATT ISP pool', 'dedicated', 'active', NOW(), NOW() WHERE NOT EXISTS (SELECT 1 FROM mailing_ip_pools WHERE name = 'mh-att-pool' AND organization_id = '00000000-0000-0000-0000-000000000001')`},
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
		{"phase6_pool_prefix_db", `UPDATE mailing_sending_profiles SET pool_prefix = 'db' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_pool_prefix_qf", `UPDATE mailing_sending_profiles SET pool_prefix = 'qf' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_pool_prefix_ht", `UPDATE mailing_sending_profiles SET pool_prefix = 'ht' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_pool_prefix_mh", `UPDATE mailing_sending_profiles SET pool_prefix = 'mh' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix = '') AND COALESCE(ip_pool,'') NOT LIKE 'shared-%'`},
		{"phase6_ip_pool_db", `UPDATE mailing_sending_profiles SET ip_pool = 'db-gmail-pool' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},
		{"phase6_ip_pool_qf", `UPDATE mailing_sending_profiles SET ip_pool = 'qf-gmail-pool' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},
		{"phase6_ip_pool_ht", `UPDATE mailing_sending_profiles SET ip_pool = 'ht-gmail-pool' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},
		{"phase6_ip_pool_mh", `UPDATE mailing_sending_profiles SET ip_pool = 'mh-gmail-pool' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND ip_pool = 'warmup-pool'`},

		{"create_subscriber_events", `CREATE TABLE IF NOT EXISTS subscriber_events (
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
		{"idx_sub_events_hash", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_hash ON subscriber_events(email_hash)`},
		{"idx_sub_events_type", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_type ON subscriber_events(event_type)`},
		{"idx_sub_events_at", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_at ON subscriber_events(event_at)`},
		{"idx_sub_events_source", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_source ON subscriber_events(source)`},
		{"idx_sub_events_domain", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_domain ON subscriber_events((metadata->>'domain'))`},
		{"idx_sub_events_subscriber", `CREATE INDEX IF NOT EXISTS idx_subscriber_events_subscriber ON subscriber_events(subscriber_id)`},
		{"add_sub_events_identity_cols", `DO $$
		BEGIN
			ALTER TABLE subscriber_events ADD COLUMN IF NOT EXISTS subscriber_id UUID;
			ALTER TABLE subscriber_events ADD COLUMN IF NOT EXISTS subscriber_email VARCHAR(255);
			ALTER TABLE subscriber_events ADD COLUMN IF NOT EXISTS ip_address VARCHAR(45);
			ALTER TABLE subscriber_events ADD COLUMN IF NOT EXISTS user_agent TEXT;
		EXCEPTION WHEN OTHERS THEN NULL;
		END $$`},
		{"grant_subscriber_events", `GRANT ALL ON TABLE subscriber_events TO ignite; GRANT ALL ON SEQUENCE subscriber_events_id_seq TO ignite`},

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

		// Seed image domains for each sending domain (idempotent via ON CONFLICT)
		{"seed_img_domain_projectjarvis", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000001', '00000000-0000-0000-0000-000000000001', 'img.projectjarvis.io', false, 'provisioning', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_quizfiesta", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000002', '00000000-0000-0000-0000-000000000001', 'img.quizfiesta.com', false, 'provisioning', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_discountblog", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000003', '00000000-0000-0000-0000-000000000001', 'img.discountblog.com', false, 'provisioning', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_historythinking", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000004', '00000000-0000-0000-0000-000000000001', 'img.historythinking.com', false, 'provisioning', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},
		{"seed_img_domain_myownhealth", `INSERT INTO mailing_image_domains (id, org_id, domain, verified, ssl_status, s3_bucket, created_at, updated_at)
			VALUES ('d0000000-0000-0000-0001-000000000005', '00000000-0000-0000-0000-000000000001', 'img.myownhealth.net', false, 'provisioning', 'jarvis-image-cdn', NOW(), NOW())
			ON CONFLICT (id) DO NOTHING`},

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

		// 7.4: Route primary profiles to shared pools
		{"phase7_startup_route_db", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-a', pool_prefix = '' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND name LIKE 'DiscountBlog PMTA%' AND (COALESCE(ip_pool,'') != 'shared-a' OR COALESCE(pool_prefix,'') != '')`},
		{"phase7_startup_route_qf", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-a', pool_prefix = '' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND name LIKE 'QuizFiesta PMTA%' AND (COALESCE(ip_pool,'') != 'shared-a' OR COALESCE(pool_prefix,'') != '')`},
		{"phase7_startup_route_ht", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-b', pool_prefix = '' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND name LIKE 'HistoryThinking PMTA%' AND (COALESCE(ip_pool,'') != 'shared-b' OR COALESCE(pool_prefix,'') != '')`},
		{"phase7_startup_route_mh", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-b', pool_prefix = '' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND name LIKE 'MyOwnHealth PMTA%' AND (COALESCE(ip_pool,'') != 'shared-b' OR COALESCE(pool_prefix,'') != '')`},
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

	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_campaign_queue_recipient_isp ON mailing_campaign_queue (campaign_id, recipient_isp, status) WHERE recipient_isp IS NOT NULL`); err != nil {
		log.Printf("[StartupMigration] idx_campaign_queue_recipient_isp: ERROR %v", err)
	} else {
		log.Println("[StartupMigration] idx_campaign_queue_recipient_isp: OK")
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
		{"nuke_mta1_cold", `UPDATE mailing_ip_addresses SET status = 'cold' WHERE hostname LIKE 'mta1%' OR ip_address::text LIKE '15.204.22.176%'`},

		{"idx_queue_recipient_isp", `CREATE INDEX IF NOT EXISTS idx_campaign_queue_recipient_isp ON mailing_campaign_queue (campaign_id, recipient_isp, status) WHERE recipient_isp IS NOT NULL`},

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

		{"phase6_final_ip_pool_profiles", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET ip_pool = 'db-gmail-pool' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND ip_pool != 'db-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'qf-gmail-pool' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND ip_pool != 'qf-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'ht-gmail-pool' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND ip_pool != 'ht-gmail-pool';
    UPDATE mailing_sending_profiles SET ip_pool = 'mh-gmail-pool' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND ip_pool != 'mh-gmail-pool';
END $$`},

		{"phase6_final_pool_prefix", `DO $$
BEGIN
    UPDATE mailing_sending_profiles SET pool_prefix = 'db' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'db');
    UPDATE mailing_sending_profiles SET pool_prefix = 'qf' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'qf');
    UPDATE mailing_sending_profiles SET pool_prefix = 'ht' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'ht');
    UPDATE mailing_sending_profiles SET pool_prefix = 'mh' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND (pool_prefix IS NULL OR pool_prefix != 'mh');
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

		// 7.6: Route existing primary profiles through shared pools (OVH IPs with valid PTR)
		{"phase7_route_db_to_shared_a", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-a', pool_prefix = '' WHERE sending_domain = 'em.discountblog.com' AND vendor_type = 'pmta' AND name LIKE 'DiscountBlog PMTA%'`},
		{"phase7_route_qf_to_shared_a", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-a', pool_prefix = '' WHERE sending_domain = 'em.quizfiesta.com' AND vendor_type = 'pmta' AND name LIKE 'QuizFiesta PMTA%'`},
		{"phase7_route_ht_to_shared_b", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-b', pool_prefix = '' WHERE sending_domain = 'em.historythinking.com' AND vendor_type = 'pmta' AND name LIKE 'HistoryThinking PMTA%'`},
		{"phase7_route_mh_to_shared_b", `UPDATE mailing_sending_profiles SET ip_pool = 'shared-b', pool_prefix = '' WHERE sending_domain = 'em.myownhealth.net' AND vendor_type = 'pmta' AND name LIKE 'MyOwnHealth PMTA%'`},
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
