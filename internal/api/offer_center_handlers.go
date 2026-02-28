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
func RegisterOfferCenterRoutes(r chi.Router, db *sql.DB, h *Handlers) {
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
