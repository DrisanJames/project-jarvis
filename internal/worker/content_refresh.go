package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// ContentRefreshWorker runs on a schedule (default 24h) to pre-generate
// fresh wave content for each brand. Generated content is stored in
// mailing_wave_content_cache so the campaign scheduler can use it.
type ContentRefreshWorker struct {
	db       *sql.DB
	interval time.Duration
	brands   map[string]ContentBrand
	wg       sync.WaitGroup
}

// ContentBrand holds the config needed to generate content for a brand.
type ContentBrand struct {
	Key             string
	BlogDomain      string
	SendingDomain   string
	BrandName       string
	CampaignType    string
	Voice           string
	Audience        string
	DesignSystem    string
	HTMLTemplate    string
	FallbackContent []mailing.BlogExcerpt
}

func NewContentRefreshWorker(db *sql.DB, interval time.Duration) *ContentRefreshWorker {
	return &ContentRefreshWorker{
		db:       db,
		interval: interval,
		brands:   make(map[string]ContentBrand),
	}
}

func (w *ContentRefreshWorker) RegisterBrand(b ContentBrand) {
	w.brands[b.Key] = b
}

func (w *ContentRefreshWorker) Start(ctx context.Context) {
	w.wg.Add(1)
	go w.loop(ctx)
}

func (w *ContentRefreshWorker) loop(ctx context.Context) {
	defer w.wg.Done()

	w.ensureTable()

	// Run once at startup after a short delay
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
		w.refreshAll(ctx)
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.refreshAll(ctx)
		}
	}
}

func (w *ContentRefreshWorker) ensureTable() {
	_, err := w.db.Exec(`
		CREATE TABLE IF NOT EXISTS mailing_wave_content_cache (
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
		)
	`)
	if err != nil {
		log.Printf("[content-refresh] failed to ensure cache table: %v", err)
	}

	w.db.Exec(`CREATE INDEX IF NOT EXISTS idx_wave_cache_brand_unused ON mailing_wave_content_cache (brand_key, generated_at DESC) WHERE used_at IS NULL`)
}

func (w *ContentRefreshWorker) refreshAll(ctx context.Context) {
	log.Printf("[content-refresh] starting nightly refresh for %d brands", len(w.brands))
	start := time.Now()

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		for _, cfgPath := range []string{"config/config.yaml", "/app/config/config.yaml"} {
			if cfg, err := config.Load(cfgPath); err == nil && cfg.OpenAI.APIKey != "" {
				openaiKey = cfg.OpenAI.APIKey
				break
			}
		}
	}
	if anthropicKey == "" && openaiKey == "" {
		log.Printf("[content-refresh] no AI API key — skipping")
		return
	}

	aiSvc := mailing.NewAIContentService(w.db, anthropicKey, openaiKey)
	waveGen := mailing.NewWaveContentGenerator(aiSvc)

	for key, b := range w.brands {
		if ctx.Err() != nil {
			return
		}
		w.refreshBrand(ctx, key, b, aiSvc, waveGen)
	}

	log.Printf("[content-refresh] completed in %s", time.Since(start).Round(time.Millisecond))
}

func (w *ContentRefreshWorker) refreshBrand(ctx context.Context, key string, b ContentBrand, aiSvc *mailing.AIContentService, waveGen *mailing.WaveContentGenerator) {
	log.Printf("[content-refresh] refreshing %s from %s", b.BrandName, b.BlogDomain)
	scrapeStart := time.Now()

	brand := aiSvc.ScrapeBrandIntelligence(ctx, b.BlogDomain)
	contentPool := brand.BlogPosts
	usedFallback := false
	if len(contentPool) < 3 && len(b.FallbackContent) > 0 {
		contentPool = b.FallbackContent
		usedFallback = true
	}

	scrapeReport := mailing.BuildScrapeReport(b.BlogDomain, brand, usedFallback, time.Since(scrapeStart))
	log.Printf("[content-refresh] %s: scraped %d posts (%d with full text), fallback=%v",
		b.BrandName, scrapeReport.PostsFound, scrapeReport.PostsWithFullText, usedFallback)

	numWaves := 5
	req := mailing.WaveContentRequest{
		SendingDomain: b.SendingDomain,
		BrandName:     b.BrandName,
		NumWaves:      numWaves,
		CampaignType:  b.CampaignType,
		Voice:         b.Voice,
		Audience:      b.Audience,
		DesignSystem:  b.DesignSystem,
		HTMLTemplate:  b.HTMLTemplate,
		BrandInfo:     brand,
		ContentPool:   contentPool,
	}

	variations, report, err := waveGen.Generate(ctx, req)
	if report != nil {
		report.ScrapeReport = scrapeReport
	}
	if err != nil {
		log.Printf("[content-refresh] %s generation failed: %v", b.BrandName, err)
		return
	}

	// Validate and store
	stored := 0
	for _, v := range variations {
		wr := mailing.ValidateWave(v, key, contentPool)
		if len(wr.Issues) > 0 {
			log.Printf("[content-refresh] %s wave %d has %d issues, storing anyway", b.BrandName, v.WaveIndex, len(wr.Issues))
		}

		diagJSON, _ := json.Marshal(map[string]interface{}{
			"wave_report":    wr,
			"scrape_report":  scrapeReport,
			"template_match": wr.TemplateMatch,
			"version":        mailing.GeneratorVersion,
		})

		_, err := w.db.ExecContext(ctx, `
			INSERT INTO mailing_wave_content_cache
				(brand_key, wave_index, subject, preview_text, from_name, html_content, diagnostics, version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, key, v.WaveIndex, v.Subject, v.PreviewText, v.FromName, v.HTMLContent, diagJSON, mailing.GeneratorVersion)
		if err != nil {
			log.Printf("[content-refresh] failed to store wave %d for %s: %v", v.WaveIndex, b.BrandName, err)
			continue
		}
		stored++
	}

	log.Printf("[content-refresh] %s: stored %d/%d waves", b.BrandName, stored, len(variations))

	// Clean up old unused cache entries (keep latest 15 per brand)
	w.db.ExecContext(ctx, `
		DELETE FROM mailing_wave_content_cache
		WHERE brand_key = $1 AND used_at IS NULL
		  AND id NOT IN (
			SELECT id FROM mailing_wave_content_cache
			WHERE brand_key = $1 AND used_at IS NULL
			ORDER BY generated_at DESC LIMIT 15
		  )
	`, key)

	if report != nil {
		mailing.LogReport(report)
	}
}

// GetUnusedWave fetches and marks-as-used the next available cached wave for a brand.
func GetUnusedWave(ctx context.Context, db *sql.DB, brandKey string) (*mailing.WaveVariation, error) {
	var v mailing.WaveVariation
	var diagJSON sql.NullString
	err := db.QueryRowContext(ctx, `
		UPDATE mailing_wave_content_cache
		SET used_at = NOW()
		WHERE id = (
			SELECT id FROM mailing_wave_content_cache
			WHERE brand_key = $1 AND used_at IS NULL
			ORDER BY generated_at DESC
			LIMIT 1
		)
		RETURNING wave_index, subject, preview_text, from_name, html_content, diagnostics
	`, brandKey).Scan(&v.WaveIndex, &v.Subject, &v.PreviewText, &v.FromName, &v.HTMLContent, &diagJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no cached content for brand %q", brandKey)
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}
