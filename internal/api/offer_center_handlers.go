package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/lib/pq"
)

// =============================================================================
// OFFER CENTER HANDLERS
// Replaces the Templates section with a unified Offer Center that combines
// network-wide Everflow intelligence, per-team performance comparison,
// AI-driven mailing suggestions, and a local creative library.
// =============================================================================

// OfferCenterHandlers provides HTTP handlers for the Offer Center feature.
// It holds a reference to the parent Handlers struct so that collectors that
// are set after route registration (e.g. Everflow) are always accessible.
type OfferCenterHandlers struct {
	db      *sql.DB
	parent  *Handlers
}

// collectors returns the current Everflow collectors from the parent Handlers.
// These may be nil during startup and populated later — this lazy access
// avoids the bug where collectors were copied as nil at route registration time.
func (och *OfferCenterHandlers) getEverflowCollector() *everflow.Collector {
	if och.parent != nil {
		return och.parent.everflowCollector
	}
	return nil
}

func (och *OfferCenterHandlers) getNetworkIntelCollector() *everflow.NetworkIntelligenceCollector {
	if och.parent != nil {
		return och.parent.networkIntelCollector
	}
	return nil
}

// teamAffiliateIDs are the hardcoded affiliate IDs for our internal email teams.
var teamAffiliateIDs = []string{"9533", "9572"}

// teamAffiliateNames maps affiliate IDs to human-readable names.
var teamAffiliateNames = map[string]string{
	"9533": "Ignite Media Internal Email",
	"9572": "Ignite Media Internal Email 4",
}

// RegisterOfferCenterRoutes registers all Offer Center routes on the given router.
// Intended to be called inside the /api/mailing route group in server.go.
// Returns the delta sync worker so callers can call Stop() on shutdown.
func RegisterOfferCenterRoutes(r chi.Router, db *sql.DB, h *Handlers, suppS3 *SuppressionS3Client, suppMgr *OfferSuppressionManager) *OptizmoDeltaSyncWorker {
	och := &OfferCenterHandlers{
		db:     db,
		parent: h,
	}

	r.Get("/offer-center/comparison", och.HandleGetOfferComparison)
	r.Get("/offer-center/suggestions", och.HandleGetAISuggestions)
	r.Get("/offer-center/creatives", och.HandleGetCreativeLibrary)
	r.Post("/offer-center/creatives", och.HandleSaveCreative)
	r.Delete("/offer-center/creatives/{creativeId}", och.HandleDeleteCreative)
	r.Get("/offer-center/performance", och.HandleGetOfferPerformance)

	// --- Phase 1B: Offer Lifecycle CRUD ---

	// Verticals
	r.Get("/offer-center/verticals", och.HandleListVerticals)
	r.Post("/offer-center/verticals", och.HandleCreateVertical)
	r.Put("/offer-center/verticals/{id}", och.HandleUpdateVertical)
	r.Delete("/offer-center/verticals/{id}", och.HandleDeleteVertical)

	// Offer tree (registered before /offers/{id} so chi doesn't match "tree" as {id})
	r.Get("/offer-center/offers/tree", och.HandleGetOfferTree)

	// Offers CRUD
	// List is wrapped with cheap per-offer suppression/conversion counts
	// (HandleListOffersWithCounts reuses the same query + filters as
	// HandleListOffers and merges one grouped count query in Go).
	r.Get("/offer-center/offers", och.HandleListOffersWithCounts)
	r.Post("/offer-center/offers", och.HandleCreateOffer)
	r.Get("/offer-center/offers/{id}", och.HandleGetOffer)
	r.Put("/offer-center/offers/{id}", och.HandleUpdateOffer)
	r.Delete("/offer-center/offers/{id}", och.HandleDeleteOffer)

	// Subject Lines
	r.Get("/offer-center/offers/{id}/subjects", och.HandleListSubjectLines)
	r.Post("/offer-center/offers/{id}/subjects", och.HandleCreateSubjectLine)
	r.Put("/offer-center/offers/{id}/subjects/{sid}", och.HandleUpdateSubjectLine)
	r.Delete("/offer-center/offers/{id}/subjects/{sid}", och.HandleDeleteSubjectLine)

	// From Names
	r.Get("/offer-center/offers/{id}/from-names", och.HandleListFromNames)
	r.Post("/offer-center/offers/{id}/from-names", och.HandleCreateFromName)
	r.Put("/offer-center/offers/{id}/from-names/{fid}", och.HandleUpdateFromName)
	r.Delete("/offer-center/offers/{id}/from-names/{fid}", och.HandleDeleteFromName)

	// Offer Creatives (mailing_offer_creatives, not mailing_creative_library)
	r.Get("/offer-center/offers/{id}/creatives", och.HandleListOfferCreatives)
	r.Post("/offer-center/offers/{id}/creatives", och.HandleCreateOfferCreative)
	r.Post("/offer-center/offers/{id}/creatives/generate", och.HandleGenerateCreatives)
	r.Get("/offer-center/offers/{id}/creatives/download-zip", och.HandleDownloadCreativesZip)
	r.Delete("/offer-center/offers/{id}/creatives/all", och.HandleDeleteAllOfferCreatives)
	r.Put("/offer-center/offers/{id}/creatives/{cid}", och.HandleUpdateOfferCreative)
	r.Delete("/offer-center/offers/{id}/creatives/{cid}", och.HandleDeleteOfferCreative)

	// Deployments
	r.Get("/offer-center/offers/{id}/deployments", och.HandleListOfferDeployments)

	// --- Landing Page Generation (Phase 5A) ---
	lpHandlers := &LandingPageHandlers{db: db}
	r.Post("/offer-center/offers/{id}/landing-page/generate", lpHandlers.HandleGenerateLandingPage)
	r.Post("/offer-center/offers/{id}/landing-page/republish", lpHandlers.HandleRepublishLandingPage)

	// --- Optizmo List Scrub Management ---
	optizmo := NewOptizmoHandlers(db)
	optizmo.SetS3Client(suppS3)
	optizmo.SetSuppressionManager(suppMgr)
	r.Post("/offer-center/offers/{id}/optizmo/request-scrub", optizmo.HandleRequestScrub)
	r.Post("/offer-center/offers/{id}/optizmo/import-result", optizmo.HandleImportScrubResult)
	r.Post("/offer-center/offers/{id}/optizmo/cancel-scrub", optizmo.HandleCancelScrub)
	r.Post("/offer-center/offers/{id}/optizmo/reset-scrub", optizmo.HandleResetScrub)
	r.Get("/offer-center/offers/{id}/optizmo/status", optizmo.HandleGetScrubStatus)

	// --- Manual per-offer suppression (single/bulk address upload) ---
	offerSupp := &OfferSuppressionUploadHandlers{db: db, suppMgr: suppMgr}
	r.Get("/offer-center/offers/{id}/suppressions", offerSupp.HandleListOfferSuppressions)
	r.Post("/offer-center/offers/{id}/suppressions", offerSupp.HandleAddOfferSuppressions)
	r.Delete("/offer-center/offers/{id}/suppressions", offerSupp.HandleRemoveOfferSuppression)

	// --- Optizmo Nightly Delta Sync ---
	deltaSyncWorker := NewOptizmoDeltaSyncWorker(db)
	deltaSyncWorker.SetS3Client(suppS3)
	deltaSyncWorker.SetSuppressionManager(suppMgr)
	r.Post("/offer-center/offers/{id}/optizmo/toggle-sync", deltaSyncWorker.HandleToggleSync)
	r.Post("/offer-center/offers/{id}/optizmo/trigger-sync", deltaSyncWorker.HandleManualSync)
	r.Get("/offer-center/offers/{id}/optizmo/sync-status", deltaSyncWorker.HandleSyncStatus)
	deltaSyncWorker.Start()

	// --- Proof Send & Deploy ---
	proofHandler := NewProofSendHandler(db)
	r.Post("/offer-center/offers/{id}/proof-send", proofHandler.HandleProofSend)
	r.Post("/offer-center/offers/{id}/deploy", proofHandler.HandleDeployOffer)

	// Per-offer performance
	r.Get("/offer-center/offers/{id}/stats", och.HandleGetOfferStats)
	r.Get("/offer-center/offers/{id}/performance", och.HandleGetOfferDetailPerformance)
	r.Get("/offer-center/offers/{id}/creatives/{cid}/performance", och.HandleGetCreativePerformance)
	r.Get("/offer-center/offers/{id}/subjects/{sid}/performance", och.HandleGetSubjectPerformance)
	r.Get("/offer-center/offers/{id}/from-names/{fid}/performance", och.HandleGetFromNamePerformance)

	return deltaSyncWorker
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

// OfferComparisonEntry represents a single offer with both network and team metrics.
type OfferComparisonEntry struct {
	OfferID            string  `json:"offer_id"`
	OfferName          string  `json:"offer_name"`
	OfferType          string  `json:"offer_type"`
	NetworkClicks      int64   `json:"network_clicks"`
	NetworkConversions int64   `json:"network_conversions"`
	NetworkRevenue     float64 `json:"network_revenue"`
	NetworkEPC         float64 `json:"network_epc"`
	NetworkCVR         float64 `json:"network_cvr"`
	YourClicks         int64   `json:"your_clicks"`
	YourConversions    int64   `json:"your_conversions"`
	YourRevenue        float64 `json:"your_revenue"`
	YourEPC            float64 `json:"your_epc"`
	YourCVR            float64 `json:"your_cvr"`
	DeltaEPC           float64 `json:"delta_epc"`
	DeltaCVR           float64 `json:"delta_cvr"`
}

// OfferComparisonTotals holds aggregate totals.
type OfferComparisonTotals struct {
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
	EPC         float64 `json:"epc"`
	CVR         float64 `json:"cvr"`
}

// OfferComparisonResponse is the response for HandleGetOfferComparison.
type OfferComparisonResponse struct {
	Offers        []OfferComparisonEntry `json:"offers"`
	YourTotals    OfferComparisonTotals  `json:"your_totals"`
	NetworkTotals OfferComparisonTotals  `json:"network_totals"`
}

// AISuggestion represents a single AI-generated mailing suggestion.
type AISuggestion struct {
	OfferID   string                 `json:"offer_id"`
	OfferName string                 `json:"offer_name"`
	Type      string                 `json:"type"` // trending, untested, high_epc, opportunity
	Reasoning string                 `json:"reasoning"`
	Score     float64                `json:"score"`
	Metrics   map[string]interface{} `json:"metrics"`
}

// AISuggestionsResponse is the response for HandleGetAISuggestions.
type AISuggestionsResponse struct {
	Suggestions []AISuggestion `json:"suggestions"`
}

// CreativeEntry represents a row from mailing_creative_library.
type CreativeEntry struct {
	ID                   string          `json:"id"`
	OrganizationID       string          `json:"organization_id"`
	OfferID              *string         `json:"offer_id"`
	OfferName            *string         `json:"offer_name"`
	CreativeName         *string         `json:"creative_name"`
	Source               string          `json:"source"`
	HTMLContent          *string         `json:"html_content"`
	TextContent          *string         `json:"text_content"`
	ThumbnailURL         *string         `json:"thumbnail_url"`
	AiOptimized          bool            `json:"ai_optimized"`
	VariantOf            *string         `json:"variant_of"`
	EverflowCreativeID   *string         `json:"everflow_creative_id"`
	TrackingLinkTemplate *string         `json:"tracking_link_template"`
	Metadata             json.RawMessage `json:"metadata"`
	Tags                 []string        `json:"tags"`
	UseCount             int             `json:"use_count"`
	Status               string          `json:"status"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// CreativeLibraryResponse is the response for HandleGetCreativeLibrary.
type CreativeLibraryResponse struct {
	Creatives []CreativeEntry `json:"creatives"`
	Total     int             `json:"total"`
}

// SaveCreativeRequest is the request body for HandleSaveCreative.
type SaveCreativeRequest struct {
	OfferID              string          `json:"offer_id"`
	OfferName            string          `json:"offer_name"`
	CreativeName         string          `json:"creative_name"`
	HTMLContent          string          `json:"html_content"`
	TextContent          string          `json:"text_content"`
	Source               string          `json:"source"`
	EverflowCreativeID   string          `json:"everflow_creative_id"`
	TrackingLinkTemplate string          `json:"tracking_link_template"`
	Metadata             json.RawMessage `json:"metadata"`
	Tags                 []string        `json:"tags"`
}

// OfferPerformanceResponse is the response for HandleGetOfferPerformance.
type OfferPerformanceResponse struct {
	OfferID       string                     `json:"offer_id"`
	OfferName     string                     `json:"offer_name"`
	OfferType     string                     `json:"offer_type"`
	Network       *OfferPerformanceMetrics   `json:"network"`
	YourTeam      *OfferPerformanceMetrics   `json:"your_team"`
	DailyTrend    []OfferDailyTrendEntry     `json:"daily_trend,omitempty"`
}

// OfferPerformanceMetrics holds aggregated metrics for one side (network or team).
type OfferPerformanceMetrics struct {
	Clicks       int64   `json:"clicks"`
	UniqueClicks int64   `json:"unique_clicks"`
	Conversions  int64   `json:"conversions"`
	Revenue      float64 `json:"revenue"`
	Payout       float64 `json:"payout"`
	Profit       float64 `json:"profit"`
	Margin       float64 `json:"margin"` // profit/revenue * 100
	EPC          float64 `json:"epc"`
	CVR          float64 `json:"cvr"`
	CPA          float64 `json:"cpa"`
	RPC          float64 `json:"rpc"` // revenue per conversion
}

// OfferDailyTrendEntry holds daily performance for an offer.
type OfferDailyTrendEntry struct {
	Date        string  `json:"date"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
	Payout      float64 `json:"payout"`
	Profit      float64 `json:"profit"`
	EPC         float64 `json:"epc"`
	CVR         float64 `json:"cvr"`
}

// ---------------------------------------------------------------------------
// 1. HandleGetOfferComparison — GET /offer-center/comparison
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleGetOfferComparison(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")

	now := time.Now()
	var startDate time.Time

	switch daysStr {
	case "today":
		// Current day: midnight local time
		startDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "mtd":
		// Month-to-date: first day of the current month
		startDate = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	default:
		days := 7
		if daysStr != "" {
			if d, err := strconv.Atoi(daysStr); err == nil && d > 0 && d <= 90 {
				days = d
			}
		}
		startDate = now.AddDate(0, 0, -days)
	}

	// --- Network-wide data from the cached snapshot ---
	networkOfferMap := make(map[string]everflow.NetworkOfferIntelligence)
	var networkTotalClicks, networkTotalConversions int64
	var networkTotalRevenue float64

	if och.getNetworkIntelCollector() != nil {
		snapshot := och.getNetworkIntelCollector().GetSnapshot()
		if snapshot != nil {
			for _, o := range snapshot.TopOffers {
				networkOfferMap[o.OfferID] = o
			}
			networkTotalClicks = snapshot.NetworkTotalClicks
			networkTotalConversions = snapshot.NetworkTotalConversions
			networkTotalRevenue = snapshot.NetworkTotalRevenue
		}
	}

	// --- Your team's data from Everflow entity report ---
	yourOfferMap := make(map[string]*teamOfferStats)
	var yourTotalClicks, yourTotalConversions int64
	var yourTotalRevenue float64

	if och.getEverflowCollector() != nil {
		client := och.getEverflowCollector().GetClient()
		if client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			report, err := client.GetEntityReportByOffer(ctx, startDate, now, teamAffiliateIDs)
			if err == nil && report != nil {
				for _, row := range report.Table {
					var offerID, offerName string
					for _, col := range row.Columns {
						if col.ColumnType == "offer" {
							offerID = col.ID
							offerName = col.Label
							break
						}
					}
					if offerID == "" {
						continue
					}
					stats := &teamOfferStats{
						offerID:     offerID,
						offerName:   offerName,
						clicks:      row.Reporting.TotalClick,
						conversions: row.Reporting.Conversions,
						revenue:     row.Reporting.Revenue,
					}
					if stats.clicks > 0 {
						stats.epc = stats.revenue / float64(stats.clicks)
						stats.cvr = float64(stats.conversions) / float64(stats.clicks) * 100
					}
					yourOfferMap[offerID] = stats
					yourTotalClicks += stats.clicks
					yourTotalConversions += stats.conversions
					yourTotalRevenue += stats.revenue
				}
			}
		}
	}

	// --- Merge into comparison entries ---
	seen := make(map[string]bool)
	var offers []OfferComparisonEntry

	// Start with network offers (most comprehensive list)
	for offerID, nOffer := range networkOfferMap {
		entry := OfferComparisonEntry{
			OfferID:            offerID,
			OfferName:          nOffer.OfferName,
			OfferType:          nOffer.OfferType,
			NetworkClicks:      nOffer.NetworkClicks,
			NetworkConversions: nOffer.NetworkConversions,
			NetworkRevenue:     nOffer.NetworkRevenue,
			NetworkEPC:         nOffer.NetworkEPC,
			NetworkCVR:         nOffer.NetworkCVR,
		}
		if yOffer, ok := yourOfferMap[offerID]; ok {
			entry.YourClicks = yOffer.clicks
			entry.YourConversions = yOffer.conversions
			entry.YourRevenue = yOffer.revenue
			entry.YourEPC = yOffer.epc
			entry.YourCVR = yOffer.cvr
		}
		entry.DeltaEPC = roundTo(entry.YourEPC-entry.NetworkEPC, 4)
		entry.DeltaCVR = roundTo(entry.YourCVR-entry.NetworkCVR, 2)
		offers = append(offers, entry)
		seen[offerID] = true
	}

	// Add any team-only offers not in the network snapshot
	for offerID, yOffer := range yourOfferMap {
		if seen[offerID] {
			continue
		}
		entry := OfferComparisonEntry{
			OfferID:         offerID,
			OfferName:       yOffer.offerName,
			OfferType:       everflow.GetOfferType(yOffer.offerName),
			YourClicks:      yOffer.clicks,
			YourConversions: yOffer.conversions,
			YourRevenue:     yOffer.revenue,
			YourEPC:         yOffer.epc,
			YourCVR:         yOffer.cvr,
			DeltaEPC:        yOffer.epc,
			DeltaCVR:        yOffer.cvr,
		}
		offers = append(offers, entry)
	}

	// Sort by network revenue descending, then your revenue
	sort.Slice(offers, func(i, j int) bool {
		if offers[i].NetworkRevenue != offers[j].NetworkRevenue {
			return offers[i].NetworkRevenue > offers[j].NetworkRevenue
		}
		return offers[i].YourRevenue > offers[j].YourRevenue
	})

	// Build totals
	networkTotals := OfferComparisonTotals{
		Clicks:      networkTotalClicks,
		Conversions: networkTotalConversions,
		Revenue:     networkTotalRevenue,
	}
	if networkTotalClicks > 0 {
		networkTotals.EPC = roundTo(networkTotalRevenue/float64(networkTotalClicks), 4)
		networkTotals.CVR = roundTo(float64(networkTotalConversions)/float64(networkTotalClicks)*100, 2)
	}

	yourTotals := OfferComparisonTotals{
		Clicks:      yourTotalClicks,
		Conversions: yourTotalConversions,
		Revenue:     yourTotalRevenue,
	}
	if yourTotalClicks > 0 {
		yourTotals.EPC = roundTo(yourTotalRevenue/float64(yourTotalClicks), 4)
		yourTotals.CVR = roundTo(float64(yourTotalConversions)/float64(yourTotalClicks)*100, 2)
	}

	respondJSON(w, http.StatusOK, OfferComparisonResponse{
		Offers:        offers,
		YourTotals:    yourTotals,
		NetworkTotals: networkTotals,
	})
}

// ---------------------------------------------------------------------------
// 2. HandleGetAISuggestions — GET /offer-center/suggestions
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleGetAISuggestions(w http.ResponseWriter, r *http.Request) {
	var suggestions []AISuggestion

	// Gather network snapshot
	var snapshot *everflow.NetworkIntelligenceSnapshot
	if och.getNetworkIntelCollector() != nil {
		snapshot = och.getNetworkIntelCollector().GetSnapshot()
	}
	if snapshot == nil || len(snapshot.TopOffers) == 0 {
		respondJSON(w, http.StatusOK, AISuggestionsResponse{Suggestions: suggestions})
		return
	}

	// Gather your team's offer IDs for "untested" detection
	yourOfferIDs := make(map[string]bool)
	if och.getEverflowCollector() != nil {
		client := och.getEverflowCollector().GetClient()
		if client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			now := time.Now()
			// Use current month for "untested" detection
			monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
			report, err := client.GetEntityReportByOffer(ctx, monthStart, now, teamAffiliateIDs)
			if err == nil && report != nil {
				for _, row := range report.Table {
					for _, col := range row.Columns {
						if col.ColumnType == "offer" {
							yourOfferIDs[col.ID] = true
							break
						}
					}
				}
			}
		}
	}

	// Build your team offer stats map for "opportunity" detection
	yourOfferStats := make(map[string]int64) // offer_id -> clicks
	if och.getEverflowCollector() != nil {
		client := och.getEverflowCollector().GetClient()
		if client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			now := time.Now()
			report, err := client.GetEntityReportByOffer(ctx, now.AddDate(0, 0, -7), now, teamAffiliateIDs)
			if err == nil && report != nil {
				for _, row := range report.Table {
					for _, col := range row.Columns {
						if col.ColumnType == "offer" {
							yourOfferStats[col.ID] = row.Reporting.TotalClick
							break
						}
					}
				}
			}
		}
	}

	// Category 1: Trending — offers with accelerating revenue
	for _, o := range snapshot.TopOffers {
		if o.RevenueTrend == "accelerating" && o.TodayRevenue > 0 {
			suggestions = append(suggestions, AISuggestion{
				OfferID:   o.OfferID,
				OfferName: o.OfferName,
				Type:      "trending",
				Reasoning: fmt.Sprintf(
					"Revenue is accelerating (+%.0f%% vs 7-day avg). Today: $%.2f revenue, %d conversions. Network rank #%d.",
					o.TrendPercentage, o.TodayRevenue, o.TodayConversions, o.NetworkRank,
				),
				Score: math.Min(o.AIScore+10, 100),
				Metrics: map[string]interface{}{
					"today_revenue":    o.TodayRevenue,
					"trend_percentage": o.TrendPercentage,
					"network_epc":     o.NetworkEPC,
					"network_cvr":     o.NetworkCVR,
					"network_rank":    o.NetworkRank,
				},
			})
		}
	}

	// Category 2: Untested — network offers your teams haven't sent
	for _, o := range snapshot.TopOffers {
		if !yourOfferIDs[o.OfferID] && o.NetworkRevenue > 0 && o.AIScore >= 40 {
			suggestions = append(suggestions, AISuggestion{
				OfferID:   o.OfferID,
				OfferName: o.OfferName,
				Type:      "untested",
				Reasoning: fmt.Sprintf(
					"This offer is generating $%.2f revenue across the network but your teams have not tested it yet. Network EPC: $%.4f, CVR: %.2f%%.",
					o.NetworkRevenue, o.NetworkEPC, o.NetworkCVR,
				),
				Score: o.AIScore,
				Metrics: map[string]interface{}{
					"network_revenue":     o.NetworkRevenue,
					"network_clicks":      o.NetworkClicks,
					"network_conversions": o.NetworkConversions,
					"network_epc":         o.NetworkEPC,
					"network_cvr":         o.NetworkCVR,
				},
			})
		}
	}

	// Category 3: High EPC — offers with EPC above network average
	if snapshot.NetworkAvgEPC > 0 {
		for _, o := range snapshot.TopOffers {
			if o.NetworkEPC > snapshot.NetworkAvgEPC*1.3 && o.NetworkClicks > 50 {
				suggestions = append(suggestions, AISuggestion{
					OfferID:   o.OfferID,
					OfferName: o.OfferName,
					Type:      "high_epc",
					Reasoning: fmt.Sprintf(
						"EPC of $%.4f is %.1fx the network average ($%.4f). Strong earnings potential with %d network clicks.",
						o.NetworkEPC, o.NetworkEPC/snapshot.NetworkAvgEPC, snapshot.NetworkAvgEPC, o.NetworkClicks,
					),
					Score: math.Min(o.AIScore+5, 100),
					Metrics: map[string]interface{}{
						"network_epc":      o.NetworkEPC,
						"network_avg_epc":  snapshot.NetworkAvgEPC,
						"epc_multiplier":   roundTo(o.NetworkEPC/snapshot.NetworkAvgEPC, 2),
						"network_clicks":   o.NetworkClicks,
						"network_revenue":  o.NetworkRevenue,
					},
				})
			}
		}
	}

	// Category 4: Opportunity — high network volume but low your team volume
	for _, o := range snapshot.TopOffers {
		yourClicks := yourOfferStats[o.OfferID]
		if o.NetworkClicks > 500 && yourClicks < 50 && o.NetworkRevenue > 100 {
			suggestions = append(suggestions, AISuggestion{
				OfferID:   o.OfferID,
				OfferName: o.OfferName,
				Type:      "opportunity",
				Reasoning: fmt.Sprintf(
					"Network has %d clicks and $%.2f revenue on this offer, but your teams only sent %d clicks. There is a large volume gap to exploit.",
					o.NetworkClicks, o.NetworkRevenue, yourClicks,
				),
				Score: math.Min(o.AIScore+8, 100),
				Metrics: map[string]interface{}{
					"network_clicks":  o.NetworkClicks,
					"network_revenue": o.NetworkRevenue,
					"your_clicks":     yourClicks,
					"volume_gap":      o.NetworkClicks - yourClicks,
					"network_epc":     o.NetworkEPC,
				},
			})
		}
	}

	// Deduplicate: keep highest-scoring entry per offer
	suggestions = deduplicateSuggestions(suggestions)

	// Sort by score descending
	sort.Slice(suggestions, func(i, j int) bool {
		return suggestions[i].Score > suggestions[j].Score
	})

	// Cap at 30
	if len(suggestions) > 30 {
		suggestions = suggestions[:30]
	}

	respondJSON(w, http.StatusOK, AISuggestionsResponse{Suggestions: suggestions})
}

// ---------------------------------------------------------------------------
// 3. HandleGetCreativeLibrary — GET /offer-center/creatives
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleGetCreativeLibrary(w http.ResponseWriter, r *http.Request) {
	if och.db == nil {
		respondJSON(w, http.StatusOK, CreativeLibraryResponse{Creatives: []CreativeEntry{}, Total: 0})
		return
	}

	search := r.URL.Query().Get("search")
	offerID := r.URL.Query().Get("offer_id")
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "active"
	}

	query := `SELECT id, organization_id, offer_id, offer_name, creative_name,
		source, html_content, text_content, thumbnail_url, ai_optimized,
		variant_of, everflow_creative_id, tracking_link_template,
		metadata, tags, use_count, status, created_at, updated_at
		FROM mailing_creative_library WHERE status = $1`
	args := []interface{}{status}
	argIdx := 2

	if offerID != "" {
		query += fmt.Sprintf(" AND offer_id = $%d", argIdx)
		args = append(args, offerID)
		argIdx++
	}
	if search != "" {
		query += fmt.Sprintf(" AND (creative_name ILIKE $%d OR offer_name ILIKE $%d)", argIdx, argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}

	query += " ORDER BY updated_at DESC LIMIT 100"

	ctx := r.Context()
	rows, err := och.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("ERROR: failed to query creatives: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve creatives")
		return
	}
	defer rows.Close()

	var creatives []CreativeEntry
	for rows.Next() {
		var c CreativeEntry
		var metadata []byte
		var tags []byte

		err := rows.Scan(
			&c.ID, &c.OrganizationID, &c.OfferID, &c.OfferName, &c.CreativeName,
			&c.Source, &c.HTMLContent, &c.TextContent, &c.ThumbnailURL, &c.AiOptimized,
			&c.VariantOf, &c.EverflowCreativeID, &c.TrackingLinkTemplate,
			&metadata, &tags, &c.UseCount, &c.Status, &c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			log.Printf("ERROR: failed to scan creative row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve creatives")
			return
		}

		if len(metadata) > 0 {
			c.Metadata = metadata
		} else {
			c.Metadata = json.RawMessage(`{}`)
		}

		// Parse PostgreSQL text[] into Go []string
		c.Tags = parsePgTextArray(string(tags))

		creatives = append(creatives, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("ERROR: row iteration error for creatives: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve creatives")
		return
	}

	if creatives == nil {
		creatives = []CreativeEntry{}
	}

	respondJSON(w, http.StatusOK, CreativeLibraryResponse{
		Creatives: creatives,
		Total:     len(creatives),
	})
}

// ---------------------------------------------------------------------------
// 4. HandleSaveCreative — POST /offer-center/creatives
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleSaveCreative(w http.ResponseWriter, r *http.Request) {
	if och.db == nil {
		respondError(w, http.StatusServiceUnavailable, "Database not available")
		return
	}

	var req SaveCreativeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body: "+err.Error())
		return
	}

	if req.CreativeName == "" {
		respondError(w, http.StatusBadRequest, "creative_name is required")
		return
	}
	if req.Source == "" {
		req.Source = "manual"
	}

	id := uuid.New().String()
	orgID := uuid.Nil.String() // Default org; real orgs come from auth context

	metadataBytes := req.Metadata
	if len(metadataBytes) == 0 {
		metadataBytes = json.RawMessage(`{}`)
	}

	// Convert tags slice to PostgreSQL text[] literal
	pgTags := toPgTextArray(req.Tags)

	now := time.Now()
	ctx := r.Context()

	_, err := och.db.ExecContext(ctx, `
		INSERT INTO mailing_creative_library
			(id, organization_id, offer_id, offer_name, creative_name, source,
			 html_content, text_content, everflow_creative_id, tracking_link_template,
			 metadata, tags, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'active',$13,$14)`,
		id, orgID, ocNilIfEmpty(req.OfferID), ocNilIfEmpty(req.OfferName), req.CreativeName, req.Source,
		ocNilIfEmpty(req.HTMLContent), ocNilIfEmpty(req.TextContent),
		ocNilIfEmpty(req.EverflowCreativeID), ocNilIfEmpty(req.TrackingLinkTemplate),
		string(metadataBytes), pgTags, now, now,
	)
	if err != nil {
		log.Printf("ERROR: failed to save creative: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to save creative")
		return
	}

	creative := CreativeEntry{
		ID:                   id,
		OrganizationID:       orgID,
		OfferID:              nilStrPtr(req.OfferID),
		OfferName:            nilStrPtr(req.OfferName),
		CreativeName:         &req.CreativeName,
		Source:               req.Source,
		HTMLContent:          nilStrPtr(req.HTMLContent),
		TextContent:          nilStrPtr(req.TextContent),
		EverflowCreativeID:   nilStrPtr(req.EverflowCreativeID),
		TrackingLinkTemplate: nilStrPtr(req.TrackingLinkTemplate),
		Metadata:             metadataBytes,
		Tags:                 req.Tags,
		UseCount:             0,
		Status:               "active",
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	respondJSON(w, http.StatusCreated, creative)
}

// ---------------------------------------------------------------------------
// 5. HandleDeleteCreative — DELETE /offer-center/creatives/{creativeId}
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleDeleteCreative(w http.ResponseWriter, r *http.Request) {
	if och.db == nil {
		respondError(w, http.StatusServiceUnavailable, "Database not available")
		return
	}

	creativeID := chi.URLParam(r, "creativeId")
	if creativeID == "" {
		respondError(w, http.StatusBadRequest, "creativeId is required")
		return
	}

	ctx := r.Context()
	result, err := och.db.ExecContext(ctx,
		`UPDATE mailing_creative_library SET status = 'archived', updated_at = $1 WHERE id = $2 AND status != 'archived'`,
		time.Now(), creativeID,
	)
	if err != nil {
		log.Printf("ERROR: failed to archive creative %s: %v", creativeID, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete creative")
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		respondError(w, http.StatusNotFound, "Creative not found or already archived")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deleted":     true,
		"creative_id": creativeID,
	})
}

// ---------------------------------------------------------------------------
// 6. HandleGetOfferPerformance — GET /offer-center/performance
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleGetOfferPerformance(w http.ResponseWriter, r *http.Request) {
	offerID := r.URL.Query().Get("offer_id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer_id query parameter is required")
		return
	}

	resp := OfferPerformanceResponse{
		OfferID: offerID,
	}

	// --- Network metrics from snapshot ---
	if och.getNetworkIntelCollector() != nil {
		snapshot := och.getNetworkIntelCollector().GetSnapshot()
		if snapshot != nil {
			for _, o := range snapshot.TopOffers {
				if o.OfferID == offerID {
					resp.OfferName = o.OfferName
					resp.OfferType = o.OfferType
					nm := &OfferPerformanceMetrics{
						Clicks:      o.NetworkClicks,
						Conversions: o.NetworkConversions,
						Revenue:     o.NetworkRevenue,
						EPC:         o.NetworkEPC,
						CVR:         o.NetworkCVR,
					}
					if nm.Conversions > 0 {
						nm.RPC = roundTo(nm.Revenue/float64(nm.Conversions), 4)
					}
					resp.Network = nm
					break
				}
			}
		}
	}

	// --- Your team metrics ---
	if och.getEverflowCollector() != nil {
		client := och.getEverflowCollector().GetClient()
		if client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			now := time.Now()
			// Use current month for team performance (consistent with comparison view)
			perfStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

			report, err := client.GetEntityReportByOffer(ctx, perfStart, now, teamAffiliateIDs)
			if err == nil && report != nil {
				for _, row := range report.Table {
					for _, col := range row.Columns {
						if col.ColumnType == "offer" && col.ID == offerID {
							if resp.OfferName == "" {
								resp.OfferName = col.Label
								resp.OfferType = everflow.GetOfferType(col.Label)
							}
							metrics := &OfferPerformanceMetrics{
								Clicks:       row.Reporting.TotalClick,
								UniqueClicks: row.Reporting.UniqueClick,
								Conversions:  row.Reporting.Conversions,
								Revenue:      row.Reporting.Revenue,
								Payout:       row.Reporting.Payout,
								Profit:       roundTo(row.Reporting.Revenue-row.Reporting.Payout, 2),
							}
							if metrics.Revenue > 0 {
								metrics.Margin = roundTo(metrics.Profit/metrics.Revenue*100, 2)
							}
							if metrics.Clicks > 0 {
								metrics.EPC = roundTo(metrics.Revenue/float64(metrics.Clicks), 4)
								metrics.CVR = roundTo(float64(metrics.Conversions)/float64(metrics.Clicks)*100, 2)
								metrics.CPA = roundTo(metrics.Payout/float64(metrics.Conversions), 4)
							}
							if metrics.Conversions > 0 {
								metrics.RPC = roundTo(metrics.Revenue/float64(metrics.Conversions), 4)
								if metrics.CPA == 0 {
									metrics.CPA = roundTo(metrics.Payout/float64(metrics.Conversions), 4)
								}
							}
							resp.YourTeam = metrics
							break
						}
					}
				}
			}

			// --- Daily trend for your team ---
			dailyReport, err := client.GetEntityReportByDate(ctx, perfStart, now, teamAffiliateIDs)
			if err == nil && dailyReport != nil {
				// The daily report shows aggregate across all offers; to get per-offer daily
				// we use the performance array if available, otherwise fall back to daily totals.
				// For a more precise view we query offer+date, but the Everflow entity API
				// supports only one column dimension. We return the available daily data.
				for _, row := range dailyReport.Table {
					var dateLabel string
					for _, col := range row.Columns {
						if col.ColumnType == "date" {
							dateLabel = col.Label
							break
						}
					}
					if dateLabel != "" {
						entry := OfferDailyTrendEntry{
							Date:        dateLabel,
							Clicks:      row.Reporting.TotalClick,
							Conversions: row.Reporting.Conversions,
							Revenue:     row.Reporting.Revenue,
							Payout:      row.Reporting.Payout,
							Profit:      roundTo(row.Reporting.Revenue-row.Reporting.Payout, 2),
						}
						if entry.Clicks > 0 {
							entry.EPC = roundTo(entry.Revenue/float64(entry.Clicks), 4)
							entry.CVR = roundTo(float64(entry.Conversions)/float64(entry.Clicks)*100, 2)
						}
						resp.DailyTrend = append(resp.DailyTrend, entry)
					}
				}
			}
		}
	}

	// Fall back on offer type if still empty
	if resp.OfferType == "" && resp.OfferName != "" {
		resp.OfferType = everflow.GetOfferType(resp.OfferName)
	}

	respondJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// teamOfferStats is an internal helper for aggregating your team's offer data.
type teamOfferStats struct {
	offerID     string
	offerName   string
	clicks      int64
	conversions int64
	revenue     float64
	epc         float64
	cvr         float64
}

// roundTo rounds a float to the given number of decimal places.
func roundTo(val float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(val*pow) / pow
}

// deduplicateSuggestions keeps the highest-scoring suggestion per offer_id.
func deduplicateSuggestions(suggestions []AISuggestion) []AISuggestion {
	best := make(map[string]AISuggestion)
	for _, s := range suggestions {
		if existing, ok := best[s.OfferID]; !ok || s.Score > existing.Score {
			best[s.OfferID] = s
		}
	}
	result := make([]AISuggestion, 0, len(best))
	for _, s := range best {
		result = append(result, s)
	}
	return result
}

// ocNilIfEmpty returns nil (for SQL NULL) when the string is empty.
func ocNilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nilStrPtr returns a *string pointer, or nil if the string is empty.
func nilStrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toPgTextArray converts a Go string slice to a PostgreSQL text[] literal.
func toPgTextArray(tags []string) string {
	if len(tags) == 0 {
		return "{}"
	}
	escaped := make([]string, len(tags))
	for i, t := range tags {
		// Escape double quotes and backslashes for PG array syntax
		t = strings.ReplaceAll(t, `\`, `\\`)
		t = strings.ReplaceAll(t, `"`, `\"`)
		escaped[i] = `"` + t + `"`
	}
	return "{" + strings.Join(escaped, ",") + "}"
}

// parsePgTextArray parses a PostgreSQL text[] literal into a Go string slice.
func parsePgTextArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" || s == "NULL" {
		return []string{}
	}
	// Strip outer braces
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return []string{}
	}

	var result []string
	var current strings.Builder
	inQuote := false
	escaped := false

	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if ch == ',' && !inQuote {
			result = append(result, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

// =============================================================================
// OFFER STATS — per-offer delivery/engagement aggregates
//
// GET /offer-center/offers/{id}/stats?days=30
//
// Aggregates are computed from mailing_tracking_events for the campaigns
// linked to the offer (mailing_campaigns.offer_id). The campaign counter
// columns (sent_count/open_count/…) are STALE and deliberately NOT used.
// Conversions and the suppression total come from mailing_offer_suppressions
// (Everflow postback writes reason='converted' rows there).
// =============================================================================

// OfferStatsTotals holds the window totals for an offer.
type OfferStatsTotals struct {
	Sent             int64 `json:"sent"`
	Delivered        int64 `json:"delivered"`
	Opened           int64 `json:"opened"`
	Clicked          int64 `json:"clicked"`
	HardBounces      int64 `json:"hard_bounces"`
	SoftBounces      int64 `json:"soft_bounces"`
	Deferred         int64 `json:"deferred"`
	Complaints       int64 `json:"complaints"`
	Conversions      int64 `json:"conversions"`
	SuppressionTotal int64 `json:"suppression_total"`
}

// OfferStatsCampaign is one recent campaign linked to the offer with its
// event-derived counts.
type OfferStatsCampaign struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	Sent        int64      `json:"sent"`
	Delivered   int64      `json:"delivered"`
	Opens       int64      `json:"opens"`
	Clicks      int64      `json:"clicks"`
}

// OfferStatsDaily is one day of the per-day series.
type OfferStatsDaily struct {
	Date        string `json:"date"`
	Sent        int64  `json:"sent"`
	Delivered   int64  `json:"delivered"`
	Opened      int64  `json:"opened"`
	Clicked     int64  `json:"clicked"`
	Conversions int64  `json:"conversions"`
}

// OfferSuppressionWeek is one week of the suppressed-count trend
// (mailing_offer_suppressions.suppressed_at bucketed by ISO week start).
type OfferSuppressionWeek struct {
	WeekStart string `json:"week_start"` // YYYY-MM-DD (Monday)
	Count     int64  `json:"count"`
}

// OfferStatsResponse is the response for HandleGetOfferStats and the payload
// embedded in the CPM Planner's /deals/{id}/offer-performance endpoint —
// ONE shared implementation (loadOfferStats), two endpoints.
type OfferStatsResponse struct {
	OfferID       string               `json:"offer_id"`
	Days          int                  `json:"days"`
	Totals        OfferStatsTotals     `json:"totals"`
	CampaignCount int                  `json:"campaign_count"`
	Campaigns     []OfferStatsCampaign `json:"campaigns"`
	Daily         []OfferStatsDaily    `json:"daily"`
	// DNM list size = file_count of the latest completed Optizmo scrub job
	// for the offer (0 when the offer has never been scrubbed).
	DnmListSize int64 `json:"dnm_list_size"`
	// AudienceSize = audience_count of that same scrub job — the cheapest
	// honest audience denominator we have. 0 = unknown (no scrub yet);
	// consumers must skip the suppressed-share figure in that case.
	AudienceSize int64 `json:"audience_size"`
	// Suppressed-count trend, last 8 weeks (dense — zero-count weeks emitted).
	SuppressionWeekly []OfferSuppressionWeek `json:"suppression_weekly"`
}

// countOfferConversions is THE conversion-attribution query — Everflow
// postbacks write reason='converted' rows into mailing_offer_suppressions,
// and every surface (Offers tab stats, CPM Planner deal progress, deal
// offer-performance panel) counts conversions through this one function so
// the numbers can never drift apart. orgID == "" skips the org filter
// (offer ids are globally-unique UUIDs; the offer-center endpoints predate
// org plumbing and pass "").
func countOfferConversions(ctx context.Context, db *sql.DB, orgID, offerID string, since time.Time) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_offer_suppressions
		 WHERE offer_id = $1 AND reason = 'converted' AND suppressed_at > $2
		   AND ($3 = '' OR organization_id::text = $3)`,
		offerID, since, orgID).Scan(&n)
	return n, err
}

// loadOfferStats is the shared per-offer performance aggregation used by
// BOTH GET /offer-center/offers/{id}/stats and
// GET /cpm-planner/deals/{id}/offer-performance (requirement: single
// implementation, two endpoints).
//
// Ground truth: mailing_tracking_events for the offer's campaigns (campaign
// counter columns are STALE and never used); opens/clicks are HUMAN counts
// (machine/MPP events excluded via is_machine_open / is_machine_click);
// conversions come from countOfferConversions; suppression totals + weekly
// trend from mailing_offer_suppressions; DNM list size + audience size from
// the latest completed Optizmo scrub job.
func loadOfferStats(ctx context.Context, db *sql.DB, orgID, offerID string, days int) (*OfferStatsResponse, error) {
	windowStart := time.Now().AddDate(0, 0, -days)

	resp := &OfferStatsResponse{
		OfferID:           offerID,
		Days:              days,
		Campaigns:         []OfferStatsCampaign{},
		Daily:             []OfferStatsDaily{},
		SuppressionWeekly: []OfferSuppressionWeek{},
	}

	// 1) Bound campaign ids first — mailing_tracking_events is huge but
	// indexed on campaign_id, so everything else fans out from this list.
	campRows, err := db.QueryContext(ctx,
		`SELECT id::text, COALESCE(name,''), COALESCE(status,''), scheduled_at
		 FROM mailing_campaigns
		 WHERE offer_id = $1 AND created_at > $2
		   AND ($3 = '' OR organization_id::text = $3)
		 ORDER BY created_at DESC`, offerID, windowStart, orgID)
	if err != nil {
		return nil, fmt.Errorf("offer stats campaigns: %w", err)
	}
	var campaignIDs []string
	campaignIdx := make(map[string]int) // campaign id -> index in resp.Campaigns
	for campRows.Next() {
		var c OfferStatsCampaign
		var scheduledAt sql.NullTime
		if err := campRows.Scan(&c.ID, &c.Name, &c.Status, &scheduledAt); err != nil {
			campRows.Close()
			return nil, fmt.Errorf("offer stats scan campaign: %w", err)
		}
		if scheduledAt.Valid {
			t := scheduledAt.Time
			c.ScheduledAt = &t
		}
		campaignIDs = append(campaignIDs, c.ID)
		if len(resp.Campaigns) < 15 { // recent list: 15 newest (already ordered DESC)
			campaignIdx[c.ID] = len(resp.Campaigns)
			resp.Campaigns = append(resp.Campaigns, c)
		}
	}
	campRows.Close()
	if err := campRows.Err(); err != nil {
		return nil, fmt.Errorf("offer stats campaign rows: %w", err)
	}
	resp.CampaignCount = len(campaignIDs)

	hb := HardBounceSQL("t")
	// Human engagement only: machine/MPP fetches and scanner clicks are
	// flagged at ingest and excluded here on every surface.
	const humanOpen = `t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open, FALSE)`
	const humanClick = `t.event_type = 'clicked' AND NOT COALESCE(t.is_machine_click, FALSE)`

	// Maps for the per-day series (filled below, emitted as a dense range).
	dailyEvents := make(map[string]*OfferStatsDaily)
	dailyConversions := make(map[string]int64)

	if len(campaignIDs) > 0 {
		// 2) Window totals from tracking events. The event_at bound also
		// gives the monthly partitions a pruning predicate.
		totalsQ := fmt.Sprintf(`SELECT
			COUNT(*) FILTER (WHERE t.event_type = 'sent'),
			COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
			COUNT(*) FILTER (WHERE %s),
			COUNT(*) FILTER (WHERE %s),
			COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND %s),
			COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND NOT (%s)),
			COUNT(*) FILTER (WHERE t.event_type IN ('deferred','deferral')),
			COUNT(*) FILTER (WHERE t.event_type = 'complained')
		 FROM mailing_tracking_events t
		 WHERE t.campaign_id = ANY($1::uuid[])
		   AND t.event_at > $2
		   AND t.event_type IN ('sent','delivered','opened','clicked','bounced','deferred','deferral','complained')`,
			humanOpen, humanClick, hb, hb)
		if err := db.QueryRowContext(ctx, totalsQ, pq.Array(campaignIDs), windowStart).Scan(
			&resp.Totals.Sent, &resp.Totals.Delivered, &resp.Totals.Opened, &resp.Totals.Clicked,
			&resp.Totals.HardBounces, &resp.Totals.SoftBounces, &resp.Totals.Deferred, &resp.Totals.Complaints,
		); err != nil {
			return nil, fmt.Errorf("offer stats totals: %w", err)
		}

		// 3) Per-campaign breakdown for the recent list.
		recentIDs := make([]string, 0, len(campaignIdx))
		for id := range campaignIdx {
			recentIDs = append(recentIDs, id)
		}
		perCampQ := fmt.Sprintf(`SELECT t.campaign_id::text,
				COUNT(*) FILTER (WHERE t.event_type = 'sent'),
				COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
				COUNT(*) FILTER (WHERE %s),
				COUNT(*) FILTER (WHERE %s)
			 FROM mailing_tracking_events t
			 WHERE t.campaign_id = ANY($1::uuid[])
			   AND t.event_at > $2
			   AND t.event_type IN ('sent','delivered','opened','clicked')
			 GROUP BY t.campaign_id`, humanOpen, humanClick)
		perCampRows, err := db.QueryContext(ctx, perCampQ, pq.Array(recentIDs), windowStart)
		if err != nil {
			return nil, fmt.Errorf("offer stats per-campaign: %w", err)
		}
		for perCampRows.Next() {
			var id string
			var sent, delivered, opens, clicks int64
			if err := perCampRows.Scan(&id, &sent, &delivered, &opens, &clicks); err != nil {
				continue
			}
			if i, ok := campaignIdx[id]; ok {
				c := &resp.Campaigns[i]
				c.Sent, c.Delivered, c.Opens, c.Clicks = sent, delivered, opens, clicks
			}
		}
		perCampRows.Close()

		// 4) Daily event series.
		dailyQ := fmt.Sprintf(`SELECT to_char(t.event_at::date, 'YYYY-MM-DD'),
				COUNT(*) FILTER (WHERE t.event_type = 'sent'),
				COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
				COUNT(*) FILTER (WHERE %s),
				COUNT(*) FILTER (WHERE %s)
			 FROM mailing_tracking_events t
			 WHERE t.campaign_id = ANY($1::uuid[])
			   AND t.event_at > $2
			   AND t.event_type IN ('sent','delivered','opened','clicked')
			 GROUP BY 1`, humanOpen, humanClick)
		dailyRows, err := db.QueryContext(ctx, dailyQ, pq.Array(campaignIDs), windowStart)
		if err != nil {
			return nil, fmt.Errorf("offer stats daily: %w", err)
		}
		for dailyRows.Next() {
			var d OfferStatsDaily
			if err := dailyRows.Scan(&d.Date, &d.Sent, &d.Delivered, &d.Opened, &d.Clicked); err != nil {
				continue
			}
			dd := d
			dailyEvents[d.Date] = &dd
		}
		dailyRows.Close()
	}

	// 5) Conversions (window count, shared attribution query) + all-time
	// suppression total.
	if resp.Totals.Conversions, err = countOfferConversions(ctx, db, orgID, offerID, windowStart); err != nil {
		return nil, fmt.Errorf("offer stats conversions: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_offer_suppressions
		 WHERE offer_id = $1 AND ($2 = '' OR organization_id::text = $2)`,
		offerID, orgID).Scan(&resp.Totals.SuppressionTotal); err != nil {
		return nil, fmt.Errorf("offer stats suppression total: %w", err)
	}

	// 6) Daily conversion series.
	convRows, err := db.QueryContext(ctx,
		`SELECT to_char(suppressed_at::date, 'YYYY-MM-DD'), COUNT(*)
		 FROM mailing_offer_suppressions
		 WHERE offer_id = $1 AND reason = 'converted' AND suppressed_at > $2
		   AND ($3 = '' OR organization_id::text = $3)
		 GROUP BY 1`, offerID, windowStart, orgID)
	if err == nil {
		for convRows.Next() {
			var date string
			var n int64
			if err := convRows.Scan(&date, &n); err == nil {
				dailyConversions[date] = n
			}
		}
		convRows.Close()
	}

	// 7) Emit a dense day-by-day series across the window so the chart has
	// continuous bars even on zero-volume days.
	now := time.Now()
	for d := windowStart; !d.After(now); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		entry := OfferStatsDaily{Date: key}
		if ev, ok := dailyEvents[key]; ok {
			entry.Sent, entry.Delivered, entry.Opened, entry.Clicked = ev.Sent, ev.Delivered, ev.Opened, ev.Clicked
		}
		entry.Conversions = dailyConversions[key]
		resp.Daily = append(resp.Daily, entry)
	}

	// 8) Suppressed-count trend — weekly buckets, last 8 ISO weeks (dense).
	// Best-effort: a failure leaves an empty trend rather than failing stats.
	weeklyCounts := make(map[string]int64)
	wkRows, err := db.QueryContext(ctx,
		`SELECT to_char(date_trunc('week', suppressed_at)::date, 'YYYY-MM-DD'), COUNT(*)
		 FROM mailing_offer_suppressions
		 WHERE offer_id = $1
		   AND suppressed_at >= date_trunc('week', NOW()) - INTERVAL '7 weeks'
		   AND ($2 = '' OR organization_id::text = $2)
		 GROUP BY 1`, offerID, orgID)
	if err != nil {
		log.Printf("WARN: offer stats weekly suppression trend for %s: %v", offerID, err)
	} else {
		for wkRows.Next() {
			var wk string
			var n int64
			if err := wkRows.Scan(&wk, &n); err == nil {
				weeklyCounts[wk] = n
			}
		}
		wkRows.Close()
	}
	// Dense 8-week emission anchored on the current ISO week's Monday.
	monday := time.Now()
	for monday.Weekday() != time.Monday {
		monday = monday.AddDate(0, 0, -1)
	}
	for i := 7; i >= 0; i-- {
		wk := monday.AddDate(0, 0, -7*i).Format("2006-01-02")
		resp.SuppressionWeekly = append(resp.SuppressionWeekly, OfferSuppressionWeek{
			WeekStart: wk,
			Count:     weeklyCounts[wk],
		})
	}

	// 9) DNM list size + audience size from the latest completed Optizmo
	// scrub job ("where applicable" — zeros when never scrubbed).
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(file_count, 0), COALESCE(audience_count, 0)
		 FROM mailing_optizmo_scrub_jobs
		 WHERE offer_id = $1 AND status = 'completed'
		 ORDER BY completed_at DESC NULLS LAST LIMIT 1`, offerID).Scan(
		&resp.DnmListSize, &resp.AudienceSize,
	); err != nil && err != sql.ErrNoRows {
		log.Printf("WARN: offer stats DNM size for %s: %v", offerID, err)
	}

	return resp, nil
}

// HandleGetOfferStats — GET /offer-center/offers/{id}/stats?days=30
//
// Thin wrapper over loadOfferStats (shared with the CPM Planner's
// deal offer-performance endpoint).
func (och *OfferCenterHandlers) HandleGetOfferStats(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}
	if och.db == nil {
		respondError(w, http.StatusServiceUnavailable, "Database not available")
		return
	}

	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}

	resp, err := loadOfferStats(r.Context(), och.db, "", offerID, days)
	if err != nil {
		log.Printf("ERROR: offer stats for %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to load offer stats")
		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// =============================================================================
// OFFERS LIST WITH COUNTS
// =============================================================================

// offerListItemWithCounts decorates the standard OfferRecord (all existing
// JSON fields preserved via embedding) with cheap per-offer counts.
type offerListItemWithCounts struct {
	OfferRecord
	SuppressionCount int64 `json:"suppression_count"`
	Conversions30d   int64 `json:"conversions_30d"`
}

// HandleListOffersWithCounts — GET /offer-center/offers
//
// Same query/filters as HandleListOffers (offer_center_lifecycle.go), plus
// suppression_count (all reasons) and conversions_30d (reason='converted',
// last 30 days) from ONE grouped query over mailing_offer_suppressions,
// merged in Go — no correlated subqueries per row.
func (och *OfferCenterHandlers) HandleListOffersWithCounts(w http.ResponseWriter, r *http.Request) {
	verticalID := r.URL.Query().Get("vertical_id")
	status := r.URL.Query().Get("status")
	search := r.URL.Query().Get("search")

	query := fmt.Sprintf("SELECT %s FROM mailing_offers WHERE organization_id = $1", offerSelectCols)
	args := []interface{}{defaultOrgID}

	if verticalID != "" {
		args = append(args, verticalID)
		query += fmt.Sprintf(" AND vertical_id = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		query += fmt.Sprintf(" AND (name ILIKE $%d OR brand_name ILIKE $%d)", len(args), len(args))
	}

	query += " ORDER BY brand_name, name"

	ctx := r.Context()
	rows, err := och.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("ERROR: list offers (with counts): %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to list offers")
		return
	}

	offers := []offerListItemWithCounts{}
	idx := make(map[string]int)
	for rows.Next() {
		o, err := scanOfferRow(rows)
		if err != nil {
			rows.Close()
			log.Printf("ERROR: scan offer row (with counts): %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list offers")
			return
		}
		idx[o.ID] = len(offers)
		offers = append(offers, offerListItemWithCounts{OfferRecord: o})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("ERROR: offer rows (with counts): %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to list offers")
		return
	}

	if len(offers) > 0 {
		// Bound the grouped count to the offers actually being listed — the
		// suppression table holds multi-million-row Optizmo syncs and an
		// unbounded GROUP BY over it on every screen load is a prod hazard
		// (QA finding C3, 2026-06-12).
		offerIDs := make([]string, 0, len(offers))
		for id := range idx {
			offerIDs = append(offerIDs, id)
		}
		countRows, err := och.db.QueryContext(ctx,
			`SELECT offer_id::text,
				COUNT(*),
				COUNT(*) FILTER (WHERE reason = 'converted' AND suppressed_at > NOW() - INTERVAL '30 days')
			 FROM mailing_offer_suppressions
			 WHERE offer_id = ANY($1::uuid[])
			 GROUP BY offer_id`, pq.Array(offerIDs))
		if err != nil {
			// Non-fatal: ship the list without counts rather than failing the screen.
			log.Printf("WARN: offer suppression counts query failed: %v", err)
		} else {
			for countRows.Next() {
				var offerID string
				var total, conv30 int64
				if err := countRows.Scan(&offerID, &total, &conv30); err != nil {
					continue
				}
				if i, ok := idx[offerID]; ok {
					offers[i].SuppressionCount = total
					offers[i].Conversions30d = conv30
				}
			}
			countRows.Close()
		}
	}

	respondJSON(w, http.StatusOK, offers)
}
