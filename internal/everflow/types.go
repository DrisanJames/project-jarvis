package everflow

import (
	"strings"
	"time"
)

// Config holds Everflow API configuration
type Config struct {
	APIKey     string `yaml:"api_key"`
	BaseURL    string `yaml:"base_url"`
	TimezoneID int    `yaml:"timezone_id"`
	CurrencyID string `yaml:"currency_id"`
	Enabled    bool   `yaml:"enabled"`
	// Affiliate IDs to track
	AffiliateIDs []string `yaml:"affiliate_ids"`
}

// PropertyMapping maps property codes to full domain names
var PropertyMapping = map[string]string{
	"FTT":  "FinancialTipsToday",
	"DHF":  "dailyhistoryfacts.org",
	"SFT":  "savvyfinancetips.net",
	"EHG":  "everydayhealthguide.net",
	"BPG":  "bestpropertyguides.net",
	"JOTD": "jokeoftheday.info",
	"SH":   "sportshistory.info",
	"NPY":  "newproductsforyou.com",
	"FNI":  "e.financialsinfo.com",
	"AFI":  "affordinginsurance.com",
	"SBD":  "secretbeautydiscounts.com",
	"OTD":  "theoftheday.com",
	"HRO":  "horoscopeinfo.com",
	"TDIH": "thisdayinhistory.co",
	"OTDD": "onthisdaydaily.com",
	"FMO":  "financial-money.com",
	"ALC":  "alcatrazblog.com",
	"MHH":  "myhealthyhabitsblog.net",
	"GHH":  "goodhomehub.org",
	"DIH":  "dayinhistory.org",
	"FTD":  "financialtipsdaily.net",
	"FYF":  "findyourfit.net",
	"IGN":  "ignitemedia.com", // Internal/default
}

// DataPartnerMapping maps data-set prefixes (from Everflow sub2) to partner names.
// Only EXTERNAL data partners are listed here. Any prefix NOT in this map is
// treated as an internal Ignite data set and rolls up under the "Ignite" partner.
var DataPartnerMapping = map[string]string{
	"ATT": "Attribits",
	"GLB": "GlobeUSA",
	"SCO": "Suited Connector",
	"M77": "Media717",
}

// partnerGroupNames maps group keys to display names
var partnerGroupNames = map[string]string{
	"ATT": "Attribits",
	"GLB": "GlobeUSA",
	"SCO": "Suited Connector",
	"M77": "Media717",
	"IGN": "Ignite",
}

// DataSetCodeOverrides maps specific data-set codes to a partner group key
// when the code does NOT follow the simple prefix rule.
// Derived from Ongage reporting (DATA_SET → DATAPARTNER).
var DataSetCodeOverrides = map[string]string{
	// ATT prefix but belongs to Ignite (bare ATT without suffix)
	"ATT": "IGN",
	// GLB prefix but belongs to Ignite
	"GLB_BR": "IGN",
	// SCO prefix but belongs to Ignite
	"SCO_BATH": "IGN",
	// M77 prefix but belongs to Ignite (per Ongage ground truth)
	"M77_HW": "IGN",
	// No standard prefix but belongs to Attribits
	"BANKRUPTCYSEND": "ATT",
	// HAR prefix but belongs to GlobeUSA
	"HAR_HOME_09232024": "GLB",
	// MAS prefix but belongs to Media717
	"MAS_SP": "M77",
	// SENIOR prefix but belongs to Ignite
	"SENIOR_SIGNAL": "IGN",
}

// ResolvePartnerGroup resolves a data-set code to its partner group.
// It first checks full data-set-code overrides (for codes that don't follow
// the prefix rule), then falls back to prefix-based mapping.
// Returns (groupPrefix, groupName).
func ResolvePartnerGroup(dataSetCode string) (string, string) {
	upper := strings.ToUpper(dataSetCode)

	// 1. Check full data-set-code overrides first
	if groupKey, ok := DataSetCodeOverrides[upper]; ok {
		if name, ok := partnerGroupNames[groupKey]; ok {
			return groupKey, name
		}
		return groupKey, groupKey
	}

	// 2. Extract prefix (before first underscore) and check prefix mapping
	prefix := upper
	if idx := strings.Index(upper, "_"); idx > 0 {
		prefix = upper[:idx]
	}
	if name, ok := DataPartnerMapping[prefix]; ok {
		return prefix, name
	}

	// 3. Everything else is Ignite's internal data
	return "IGN", "Ignite"
}

// GetDataPartnerName returns the full partner name from a data-set code
func GetDataPartnerName(code string) string {
	_, name := ResolvePartnerGroup(code)
	return name
}

// ========== API Request Types ==========

// ClicksRequest is the request body for the clicks endpoint
type ClicksRequest struct {
	TimezoneID int         `json:"timezone_id"`
	From       string      `json:"from"`
	To         string      `json:"to"`
	Query      ClicksQuery `json:"query"`
}

// ClicksQuery defines the query parameters for clicks
type ClicksQuery struct {
	Filters       []Filter        `json:"filters"`
	UserMetrics   []interface{}   `json:"user_metrics"`
	Exclusions    []interface{}   `json:"exclusions"`
	MetricFilters []interface{}   `json:"metric_filters"`
	Settings      ClicksSettings  `json:"settings"`
}

// ClicksSettings defines settings for click queries
type ClicksSettings struct {
	CampaignDataOnly      bool `json:"campaign_data_only"`
	IgnoreFailTraffic     bool `json:"ignore_fail_traffic"`
	OnlyIncludeFailTraffic bool `json:"only_include_fail_traffic"`
}

// EntityReportRequest is the request for the entity reporting endpoint
type EntityReportRequest struct {
	TimezoneID int                  `json:"timezone_id"`
	CurrencyID string               `json:"currency_id"`
	From       string               `json:"from"`
	To         string               `json:"to"`
	Columns    []EntityReportColumn `json:"columns"`
	Query      EntityReportQuery    `json:"query"`
}

// EntityReportColumn specifies a column for entity reporting
type EntityReportColumn struct {
	Column string `json:"column"`
}

// EntityReportQuery defines the query for entity reporting
type EntityReportQuery struct {
	Filters []Filter `json:"filters"`
}

// EntityReportResponse is the response from the entity reporting endpoint
type EntityReportResponse struct {
	Summary     EntityReportSummary `json:"summary"`
	Table       []EntityReportRow   `json:"table"`
	Performance []EntityPerformance `json:"performance"`
}

// EntityReportSummary contains summary metrics
type EntityReportSummary struct {
	TotalClick      int64   `json:"total_click"`
	UniqueClick     int64   `json:"unique_click"`
	InvalidClick    int64   `json:"invalid_click"`
	GrossClick      int64   `json:"gross_click"`
	Conversions     int64   `json:"cv"`
	TotalCV         int64   `json:"total_cv"`
	CVR             float64 `json:"cvr"`
	Payout          float64 `json:"payout"`
	Revenue         float64 `json:"revenue"`
	CPC             float64 `json:"cpc"`
	CPA             float64 `json:"cpa"`
	RPC             float64 `json:"rpc"`
	RPA             float64 `json:"rpa"`
	Events          int64   `json:"event"`
	EventRevenue    float64 `json:"event_revenue"`
	GrossSales      float64 `json:"gross_sales"`
}

// EntityReportRow represents a row in the entity report table
type EntityReportRow struct {
	Columns   []EntityColumnValue `json:"columns"`
	Reporting EntityReportSummary `json:"reporting"`
}

// EntityColumnValue contains column data
type EntityColumnValue struct {
	ColumnType string `json:"column_type"`
	ID         string `json:"id"`
	Label      string `json:"label"`
}

// EntityPerformance contains hourly performance data
type EntityPerformance struct {
	Unix      int64               `json:"unix"`
	Reporting EntityReportSummary `json:"reporting"`
}

// ConversionsRequest is the request body for the conversions endpoint
type ConversionsRequest struct {
	TimezoneID          int              `json:"timezone_id"`
	CurrencyID          string           `json:"currency_id"`
	From                string           `json:"from"`
	To                  string           `json:"to"`
	ShowEvents          bool             `json:"show_events"`
	ShowConversions     bool             `json:"show_conversions"`
	ShowOnlyVT          bool             `json:"show_only_vt"`
	ShowOnlyFailTraffic bool             `json:"show_only_fail_traffic"`
	ShowOnlyScrub       bool             `json:"show_only_scrub"`
	ShowOnlyCT          bool             `json:"show_only_ct"`
	Query               ConversionsQuery `json:"query"`
}

// ConversionsRequestPaged is the request body with pagination
type ConversionsRequestPaged struct {
	TimezoneID          int              `json:"timezone_id"`
	CurrencyID          string           `json:"currency_id"`
	From                string           `json:"from"`
	To                  string           `json:"to"`
	ShowEvents          bool             `json:"show_events"`
	ShowConversions     bool             `json:"show_conversions"`
	ShowOnlyVT          bool             `json:"show_only_vt"`
	ShowOnlyFailTraffic bool             `json:"show_only_fail_traffic"`
	ShowOnlyScrub       bool             `json:"show_only_scrub"`
	ShowOnlyCT          bool             `json:"show_only_ct"`
	Query               ConversionsQuery `json:"query"`
	Page                int              `json:"page"`
	PageSize            int              `json:"page_size"`
}

// ConversionsQuery defines the query parameters for conversions
type ConversionsQuery struct {
	Filters     []Filter `json:"filters"`
	SearchTerms []string `json:"search_terms"`
}

// Filter represents a filter in the query
type Filter struct {
	ResourceType  string `json:"resource_type"`
	FilterIDValue string `json:"filter_id_value"`
}

// ========== API Response Types ==========

// ClicksResponse is the response from the clicks endpoint
type ClicksResponse struct {
	Clicks []ClickRecord `json:"clicks"`
}

// ClickRecord represents a single click from the API
type ClickRecord struct {
	ClickID           string  `json:"click_id"`
	TransactionID     string  `json:"transaction_id"`
	AffiliateID       string  `json:"affiliate_id"`
	AffiliateName     string  `json:"affiliate_name"`
	OfferID           string  `json:"offer_id"`
	OfferName         string  `json:"offer_name"`
	AdvertiserID      string  `json:"advertiser_id"`
	AdvertiserName    string  `json:"advertiser_name"`
	Sub1              string  `json:"sub1"`
	Sub2              string  `json:"sub2"`
	Sub3              string  `json:"sub3"`
	Sub4              string  `json:"sub4"`
	Sub5              string  `json:"sub5"`
	SourceID          string  `json:"source_id"`
	CreativeID        string  `json:"creative_id"`
	Timestamp         string  `json:"timestamp"`
	IPAddress         string  `json:"ip_address"`
	UserAgent         string  `json:"user_agent"`
	Device            string  `json:"device"`
	DeviceType        string  `json:"device_type"`
	Browser           string  `json:"browser"`
	OS                string  `json:"os"`
	Country           string  `json:"country"`
	Region            string  `json:"region"`
	City              string  `json:"city"`
	Referer           string  `json:"referer"`
	RedirectURL       string  `json:"redirect_url"`
	IsFailed          bool    `json:"is_failed"`
	FailReason        string  `json:"fail_reason"`
}

// ConversionsResponse is the response from the conversions endpoint
type ConversionsResponse struct {
	Conversions []ConversionRecord `json:"conversions"`
}

// ConversionsResponsePaged is the response with pagination info
type ConversionsResponsePaged struct {
	Conversions []ConversionRecord `json:"conversions"`
	Paging      *PagingInfo        `json:"paging"`
}

// PagingInfo contains pagination information
type PagingInfo struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	TotalCount int `json:"total_count"`
}

// RelationshipOffer contains offer details from the relationship object
type RelationshipOffer struct {
	NetworkOfferID int    `json:"network_offer_id"`
	NetworkID      int    `json:"network_id"`
	Name           string `json:"name"`
	OfferStatus    string `json:"offer_status"`
}

// RelationshipAdvertiser contains advertiser details from the relationship object
type RelationshipAdvertiser struct {
	NetworkAdvertiserID int    `json:"network_advertiser_id"`
	NetworkID           int    `json:"network_id"`
	Name                string `json:"name"`
	AccountStatus       string `json:"account_status"`
}

// RelationshipAffiliate contains affiliate details from the relationship object
type RelationshipAffiliate struct {
	NetworkAffiliateID int    `json:"network_affiliate_id"`
	NetworkID          int    `json:"network_id"`
	Name               string `json:"name"`
	AccountStatus      string `json:"account_status"`
}

// Relationship contains nested relationship data from the API
type Relationship struct {
	Offer      *RelationshipOffer      `json:"offer"`
	Advertiser *RelationshipAdvertiser `json:"advertiser"`
	Affiliate  *RelationshipAffiliate  `json:"affiliate"`
}

// ConversionRecord represents a single conversion from the API
type ConversionRecord struct {
	ConversionID            string       `json:"conversion_id"`
	TransactionID           string       `json:"transaction_id"`
	ClickID                 string       `json:"click_id"`
	AffiliateID             string       `json:"affiliate_id"`
	AffiliateName           string       `json:"affiliate_name"`
	OfferID                 string       `json:"offer_id"`
	OfferName               string       `json:"offer_name"`
	AdvertiserID            string       `json:"advertiser_id"`
	AdvertiserName          string       `json:"advertiser_name"`
	Status                  string       `json:"status"`
	EventName               string       `json:"event_name"`
	Event                   string       `json:"event"`
	IsEvent                 bool         `json:"is_event"`
	Revenue                 float64      `json:"revenue"`
	Payout                  float64      `json:"payout"`
	RevenueType             string       `json:"revenue_type"`
	PayoutType              string       `json:"payout_type"`
	Currency                string       `json:"currency"`
	Sub1                    string       `json:"sub1"`
	Sub2                    string       `json:"sub2"`
	Sub3                    string       `json:"sub3"`
	Sub4                    string       `json:"sub4"`
	Sub5                    string       `json:"sub5"`
	SourceID                string       `json:"source_id"`
	ConversionUnixTimestamp int64        `json:"conversion_unix_timestamp"`
	ClickUnixTimestamp      int64        `json:"click_unix_timestamp"`
	SessionUserIP           string       `json:"session_user_ip"`
	ConversionUserIP        string       `json:"conversion_user_ip"`
	Relationship            *Relationship `json:"relationship"`
	IPAddressSession  string  `json:"ip_address_session"`
	IPAddressConv     string  `json:"ip_address_conversion"`
	UserAgent         string  `json:"http_user_agent"`
	Platform          string  `json:"platform"`
	OSVersion         string  `json:"os_version"`
	DeviceType        string  `json:"device_type"`
	DeviceModel       string  `json:"device_model"`
	Brand             string  `json:"brand"`
	Browser           string  `json:"browser"`
	Language          string  `json:"language"`
	Country           string  `json:"country"`
	Region            string  `json:"region"`
	City              string  `json:"city"`
	DMA               int     `json:"dma"`
	Carrier           string  `json:"carrier"`
	ISP               string  `json:"isp"`
	Referer           string  `json:"referer"`
	URL               string  `json:"url"`
	CouponCode        string  `json:"coupon_code"`
	OrderID           string  `json:"order_id"`
	SaleAmount        float64 `json:"sale_amount"`
	IsScrub           bool    `json:"is_scrub"`
	ErrorCode         int     `json:"error_code"`
	ErrorMessage      string  `json:"error_message"`
	Notes             string  `json:"notes"`
}

// ========== Parsed/Processed Types ==========

// ParsedSub1 contains the parsed components from sub1 field
type ParsedSub1 struct {
	PropertyCode string `json:"property_code"`
	PropertyName string `json:"property_name"`
	OfferID      string `json:"offer_id"`
	Date         string `json:"date"`
	MailingID    string `json:"mailing_id"`
	Raw          string `json:"raw"`
}

// Click represents a processed click with parsed data
type Click struct {
	ClickID       string     `json:"click_id"`
	TransactionID string     `json:"transaction_id"`
	AffiliateID   string     `json:"affiliate_id"`
	AffiliateName string     `json:"affiliate_name"`
	OfferID       string     `json:"offer_id"`
	OfferName     string     `json:"offer_name"`
	Sub1          string     `json:"sub1"`
	Sub2          string     `json:"sub2"`
	Sub3          string     `json:"sub3"`
	Timestamp     time.Time  `json:"timestamp"`
	IPAddress     string     `json:"ip_address"`
	Device        string     `json:"device"`
	Browser       string     `json:"browser"`
	Country       string     `json:"country"`
	Region        string     `json:"region"`
	City          string     `json:"city"`
	IsFailed      bool       `json:"is_failed"`
	// Parsed from sub1
	PropertyCode  string     `json:"property_code"`
	PropertyName  string     `json:"property_name"`
	MailingID     string     `json:"mailing_id"`
	ParsedOfferID string     `json:"parsed_offer_id"`
	// Parsed from sub2 (data partner attribution)
	DataSetCode   string     `json:"data_set_code,omitempty"`
	DataPartner   string     `json:"data_partner,omitempty"`
}

// Conversion represents a processed conversion with parsed data
type Conversion struct {
	ConversionID   string    `json:"conversion_id"`
	TransactionID  string    `json:"transaction_id"`
	ClickID        string    `json:"click_id"`
	AffiliateID    string    `json:"affiliate_id"`
	AffiliateName  string    `json:"affiliate_name"`
	OfferID        string    `json:"offer_id"`
	OfferName      string    `json:"offer_name"`
	AdvertiserID   string    `json:"advertiser_id"`
	AdvertiserName string    `json:"advertiser_name"`
	Status         string    `json:"status"`
	EventName      string    `json:"event_name"`
	Revenue        float64   `json:"revenue"`
	Payout         float64   `json:"payout"`
	RevenueType    string    `json:"revenue_type"`
	PayoutType     string    `json:"payout_type"`
	Currency       string    `json:"currency"`
	Sub1           string    `json:"sub1"`
	Sub2           string    `json:"sub2"`
	Sub3           string    `json:"sub3"`
	ConversionTime time.Time `json:"conversion_time"`
	ClickTime      time.Time `json:"click_time"`
	IPAddress      string    `json:"ip_address"`
	Device         string    `json:"device"`
	Browser        string    `json:"browser"`
	Country        string    `json:"country"`
	Region         string    `json:"region"`
	City           string    `json:"city"`
	// Parsed from sub1
	PropertyCode   string    `json:"property_code"`
	PropertyName   string    `json:"property_name"`
	MailingID      string    `json:"mailing_id"`
	ParsedOfferID  string    `json:"parsed_offer_id"`
	// Parsed from sub2 (data partner attribution)
	DataSetCode    string    `json:"data_set_code,omitempty"`
	DataPartner    string    `json:"data_partner,omitempty"`
}

// ========== Aggregated Performance Types ==========

// DailyPerformance represents daily aggregated metrics
type DailyPerformance struct {
	Date           string  `json:"date"`
	Clicks         int64   `json:"clicks"`
	Conversions    int64   `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	Payout         float64 `json:"payout"`
	ConversionRate float64 `json:"conversion_rate"`
	EPC            float64 `json:"epc"` // Earnings per click
}

// OfferPerformance represents offer-level aggregated metrics
type OfferPerformance struct {
	OfferID        string  `json:"offer_id"`
	OfferName      string  `json:"offer_name"`
	Clicks         int64   `json:"clicks"`
	Conversions    int64   `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	Payout         float64 `json:"payout"`
	ConversionRate float64 `json:"conversion_rate"`
	EPC            float64 `json:"epc"`
}

// PropertyPerformance represents property/domain-level metrics
type PropertyPerformance struct {
	PropertyCode   string  `json:"property_code"`
	PropertyName   string  `json:"property_name"`
	Clicks         int64   `json:"clicks"`
	Conversions    int64   `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	Payout         float64 `json:"payout"`
	ConversionRate float64 `json:"conversion_rate"`
	EPC            float64 `json:"epc"`
	UniqueOffers   int     `json:"unique_offers"`
	// For unattributed revenue categorization
	IsUnattributed bool   `json:"is_unattributed,omitempty"`
	UnattribReason string `json:"unattrib_reason,omitempty"` // Tooltip explanation
}

// Unattributed revenue reason constants
const (
	UnattribReasonEmptySub1       = "Revenue from conversions with no tracking data (empty sub1 field)"
	UnattribReasonParseError      = "Revenue from conversions with unparseable tracking data"
	UnattribReasonNoMailingID     = "Revenue from conversions without a mailing ID in tracking data"
	UnattribReasonUnknownProperty = "Revenue from conversions with unknown property code - not in configured property list"
)

// DataPartnerDailyMetrics holds daily metrics for one data partner
type DataPartnerDailyMetrics struct {
	Date        string  `json:"date"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
}

// DataSetCodeMetrics holds per-data-set-code breakdown within a partner
type DataSetCodeMetrics struct {
	DataSetCode string  `json:"data_set_code"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
	Volume      int64   `json:"volume"`
	CVR         float64 `json:"cvr"`
	EPC         float64 `json:"epc"`
}

// OfferPartnerMetrics holds per-offer metrics within a data partner
type OfferPartnerMetrics struct {
	OfferID     string  `json:"offer_id"`
	OfferName   string  `json:"offer_name"`
	IsCPM       bool    `json:"is_cpm"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
}

// DataPartnerPerformance holds aggregated metrics for one data partner
type DataPartnerPerformance struct {
	PartnerPrefix    string                    `json:"code"`
	PartnerName      string                    `json:"name"`
	DataSetCode      string                    `json:"data_set_code"`
	Clicks           int64                     `json:"clicks"`
	Conversions      int64                     `json:"conversions"`
	Revenue          float64                   `json:"revenue"`
	CPARevenue       float64                   `json:"cpa_revenue"`
	CPMRevenue       float64                   `json:"cpm_revenue"`
	Volume           int64                     `json:"volume"`
	Payout           float64                   `json:"payout"`
	ConversionRate   float64                   `json:"cvr"`
	EPC              float64                   `json:"epc"`
	DailySeries      []DataPartnerDailyMetrics `json:"daily_series"`
	DataSetBreakdown []DataSetCodeMetrics      `json:"data_set_breakdown"`
	OfferBreakdown   []OfferPartnerMetrics     `json:"offer_breakdown"`
}

// DataPartnerMoMComparison holds month-over-month comparison
type DataPartnerMoMComparison struct {
	CurrentMonth         DataPartnerPeriodSummary `json:"current_month"`
	PreviousMonth        DataPartnerPeriodSummary `json:"previous_month"`
	RevenueChangePct     float64                  `json:"revenue_change_pct"`
	ConversionsChangePct float64                  `json:"conversions_change_pct"`
	ClicksChangePct      float64                  `json:"clicks_change_pct"`
}

// DataPartnerPeriodSummary holds totals for a single period
type DataPartnerPeriodSummary struct {
	Label       string  `json:"label"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
	CPARevenue  float64 `json:"cpa_revenue"`
	CPMRevenue  float64 `json:"cpm_revenue"`
	Volume      int64   `json:"volume"`
}

// OfferPartnerBreakdownEntry holds one partner's performance for a specific offer
type OfferPartnerBreakdownEntry struct {
	PartnerPrefix string  `json:"partner_code"`
	PartnerName   string  `json:"partner_name"`
	Clicks        int64   `json:"clicks"`
	ClickShare    float64 `json:"click_share"` // 0-100 percentage
	Conversions   int64   `json:"conversions"`
	Revenue       float64 `json:"revenue"`
}

// OfferWithPartnerBreakdown holds one offer with all partners' performance
type OfferWithPartnerBreakdown struct {
	OfferID       string                       `json:"offer_id"`
	OfferName     string                       `json:"offer_name"`
	IsCPM         bool                         `json:"is_cpm"`
	TotalClicks   int64                        `json:"total_clicks"`
	TotalConv     int64                        `json:"total_conversions"`
	TotalRevenue  float64                      `json:"total_revenue"`
	Partners      []OfferPartnerBreakdownEntry `json:"partners"`
}

// DataPartnerAnalyticsResponse is the full API response
type DataPartnerAnalyticsResponse struct {
	Partners      []DataPartnerPerformance `json:"partners"`
	Totals        DataPartnerPeriodSummary `json:"totals"`
	MoMComparison DataPartnerMoMComparison `json:"mom_comparison"`
	CachedAt      string                   `json:"cached_at,omitempty"`
	// Cost model defaults (from ESP revenue data)
	DefaultVolume   int64   `json:"default_volume"`     // total sends across all ESPs
	DefaultCostECPM float64 `json:"default_cost_ecpm"`  // total ESP cost / sends * 1000
	TotalESPCost    float64 `json:"total_esp_cost"`     // sum of all ESP costs
	// Offer-centric view: per-offer partner breakdown
	CPMOffers []OfferWithPartnerBreakdown `json:"cpm_offers"`
	CPAOffers []OfferWithPartnerBreakdown `json:"cpa_offers"`
}

// CampaignRevenue links Ongage campaign to Everflow revenue
type CampaignRevenue struct {
	MailingID      string  `json:"mailing_id"`
	CampaignName   string  `json:"campaign_name"`
	PropertyCode   string  `json:"property_code"`
	PropertyName   string  `json:"property_name"`
	OfferID        string  `json:"offer_id"`
	OfferName      string  `json:"offer_name"`
	Clicks         int64   `json:"clicks"`
	Conversions    int64   `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	Payout         float64 `json:"payout"`
	ConversionRate float64 `json:"conversion_rate"`
	EPC            float64 `json:"epc"`
	// From Ongage (pre-enriched in background)
	AudienceSize    int64  `json:"audience_size"`    // Targeted audience from Ongage
	Sent            int64  `json:"sent"`
	Delivered       int64  `json:"delivered"`
	Opens           int64  `json:"opens"`
	UniqueOpens     int64  `json:"unique_opens"`
	EmailClicks     int64  `json:"email_clicks"`     // Ongage clicks
	SendingDomain   string `json:"sending_domain"`
	ESPName         string `json:"esp_name"`         // SparkPost, Mailgun, SES, etc.
	ESPConnectionID string `json:"esp_connection_id"`
	OngageLinked    bool   `json:"ongage_linked"`    // Whether Ongage data was found
	// Calculated
	RPM            float64 `json:"rpm"`             // Revenue per 1000 sent
	ECPM           float64 `json:"ecpm"`            // Revenue per 1000 delivered
	RevenuePerOpen float64 `json:"revenue_per_open"`
}

// SegmentInfo contains information about a segment
type SegmentInfo struct {
	SegmentID   string `json:"segment_id"`
	Name        string `json:"name"`
	Count       int64  `json:"count"`
	IsSuppression bool `json:"is_suppression"`
}

// EnrichedCampaignDetails contains campaign data combined from Everflow and Ongage
type EnrichedCampaignDetails struct {
	// Identifiers
	MailingID    string `json:"mailing_id"`
	CampaignName string `json:"campaign_name"`
	
	// From Everflow
	PropertyCode   string  `json:"property_code"`
	PropertyName   string  `json:"property_name"`
	OfferID        string  `json:"offer_id"`
	OfferName      string  `json:"offer_name"`
	Clicks         int64   `json:"clicks"`
	Conversions    int64   `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	Payout         float64 `json:"payout"`
	
	// From Ongage
	Subject             string        `json:"subject"`
	SendingDomain       string        `json:"sending_domain"`
	ESPName             string        `json:"esp_name"`
	ESPConnectionID     string        `json:"esp_connection_id"`
	AudienceSize        int64         `json:"audience_size"`
	Sent                int64         `json:"sent"`
	Delivered           int64         `json:"delivered"`
	Opens               int64         `json:"opens"`
	UniqueOpens         int64         `json:"unique_opens"`
	EmailClicks         int64         `json:"email_clicks"`         // Unique clickers
	TotalEmailClicks    int64         `json:"total_email_clicks"`   // Total clicks (including repeats)
	UniqueEmailClicks   int64         `json:"unique_email_clicks"`
	Bounces             int64         `json:"bounces"`              // Total bounces (hard + soft)
	HardBounces         int64         `json:"hard_bounces"`         // Permanent delivery failures
	SoftBounces         int64         `json:"soft_bounces"`         // Temporary delivery failures
	Failed              int64         `json:"failed"`               // Non-bounce failures
	Unsubscribes        int64         `json:"unsubscribes"`
	Complaints          int64         `json:"complaints"`
	ScheduleDate        string        `json:"schedule_date"`
	SendingStartDate    string        `json:"sending_start_date"`
	SendingEndDate      string        `json:"sending_end_date"`
	Status              string        `json:"status"`
	StatusDesc          string        `json:"status_desc"`
	SendingSegments     []SegmentInfo `json:"sending_segments"`
	SuppressionSegments []SegmentInfo `json:"suppression_segments"`
	
	// Calculated Metrics
	ECPM              float64 `json:"ecpm"`               // (Revenue / Audience) * 1000
	RevenuePerClick   float64 `json:"revenue_per_click"`  // Revenue / Everflow Clicks
	ConversionRate    float64 `json:"conversion_rate"`    // Conversions / Clicks
	DeliveryRate      float64 `json:"delivery_rate"`      // Delivered / Sent
	OpenRate          float64 `json:"open_rate"`          // UniqueOpens / Delivered
	ClickToOpenRate   float64 `json:"click_to_open_rate"` // EmailClicks / UniqueOpens
	
	// Link status
	OngageLinked bool   `json:"ongage_linked"`
	LinkError    string `json:"link_error,omitempty"`
}

// PeriodPerformance represents performance for a time period (daily/weekly/monthly)
type PeriodPerformance struct {
	Period         string                `json:"period"` // "2026-01-27", "2026-W04", "2026-01"
	PeriodType     string                `json:"period_type"` // "daily", "weekly", "monthly"
	StartDate      string                `json:"start_date"`
	EndDate        string                `json:"end_date"`
	TotalClicks    int64                 `json:"total_clicks"`
	TotalConversions int64               `json:"total_conversions"`
	TotalRevenue   float64               `json:"total_revenue"`
	TotalPayout    float64               `json:"total_payout"`
	ConversionRate float64               `json:"conversion_rate"`
	EPC            float64               `json:"epc"`
	ByOffer        []OfferPerformance    `json:"by_offer"`
	ByProperty     []PropertyPerformance `json:"by_property"`
	ByCampaign     []CampaignRevenue     `json:"by_campaign"`
}

// CollectorMetrics represents the latest collected Everflow metrics
type CollectorMetrics struct {
	LastFetch           time.Time              `json:"last_fetch"`
	TodayClicks         int64                  `json:"today_clicks"`
	TodayConversions    int64                  `json:"today_conversions"`
	TodayRevenue        float64                `json:"today_revenue"`
	TodayPayout         float64                `json:"today_payout"`
	DailyPerformance    []DailyPerformance     `json:"daily_performance"`
	OfferPerformance    []OfferPerformance     `json:"offer_performance"`
	PropertyPerformance []PropertyPerformance  `json:"property_performance"`
	CampaignRevenue     []CampaignRevenue      `json:"campaign_revenue"`
	RevenueBreakdown    *RevenueBreakdown      `json:"revenue_breakdown"`
	ESPRevenue          []ESPRevenuePerformance `json:"esp_revenue"` // Revenue by ESP
	// Raw data for detailed views
	RecentClicks      []Click      `json:"recent_clicks"`
	RecentConversions []Conversion `json:"recent_conversions"`
}

// ESPRevenuePerformance represents revenue aggregated by ESP (SparkPost, Mailgun, SES, etc.)
type ESPRevenuePerformance struct {
	ESPName        string  `json:"esp_name"`
	CampaignCount  int     `json:"campaign_count"`
	TotalSent      int64   `json:"total_sent"`
	TotalDelivered int64   `json:"total_delivered"`
	TotalOpens     int64   `json:"total_opens"`
	Clicks         int64   `json:"clicks"`          // Everflow clicks
	Conversions    int64   `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	Payout         float64 `json:"payout"`
	Percentage     float64 `json:"percentage"`      // Percentage of total revenue
	AvgECPM        float64 `json:"avg_ecpm"`        // Average eCPM across campaigns (revenue-based)
	ConversionRate float64 `json:"conversion_rate"`
	EPC            float64 `json:"epc"`
	
	// Cost metrics (from ESP contract)
	CostMetrics    *ESPCostMetrics `json:"cost_metrics,omitempty"`
}

// ESPCostMetrics contains cost calculations for an ESP
type ESPCostMetrics struct {
	// Contract details
	MonthlyIncluded    int64   `json:"monthly_included"`       // Emails included in monthly fee
	MonthlyFee         float64 `json:"monthly_fee"`            // Monthly contract cost
	OverageRatePer1000 float64 `json:"overage_rate_per_1000"`  // Cost per 1000 extra emails
	
	// Usage for period
	EmailsSent         int64   `json:"emails_sent"`            // Total emails sent in period
	EmailsOverIncluded int64   `json:"emails_over_included"`   // Emails over the included amount
	
	// Cost calculations
	ProRatedBaseCost   float64 `json:"pro_rated_base_cost"`    // Portion of monthly fee for this period
	OverageCost        float64 `json:"overage_cost"`           // Cost for emails over included
	TotalCost          float64 `json:"total_cost"`             // Total cost (base + overage)
	
	// eCPM calculations (cost per 1000 emails)
	CostECPM           float64 `json:"cost_ecpm"`              // Cost per 1000 emails sent
	RevenueECPM        float64 `json:"revenue_ecpm"`           // Revenue per 1000 emails sent
	
	// Profitability
	GrossProfit        float64 `json:"gross_profit"`           // Revenue - Total Cost
	GrossMargin        float64 `json:"gross_margin"`           // Gross Profit / Revenue (as percentage)
	NetRevenuePerEmail float64 `json:"net_revenue_per_email"`  // (Revenue - Cost) / Emails Sent
	ROI                float64 `json:"roi"`                    // (Revenue - Cost) / Cost (as percentage)
}

// ESPContractInfo holds ESP contract configuration (mirrors config but for runtime use)
type ESPContractInfo struct {
	ESPName            string  `json:"esp_name"`
	MonthlyIncluded    int64   `json:"monthly_included"`
	MonthlyFee         float64 `json:"monthly_fee"`
	OverageRatePer1000 float64 `json:"overage_rate_per_1000"`
}

// RevenueBreakdown contains CPM vs Non-CPM revenue analysis
type RevenueBreakdown struct {
	CPM       RevenueCategory   `json:"cpm"`
	NonCPM    RevenueCategory   `json:"non_cpm"`
	DailyTrend []DailyBreakdown `json:"daily_trend"`
}

// RevenueCategory contains aggregated metrics for a category (CPM or Non-CPM)
type RevenueCategory struct {
	OfferCount  int     `json:"offer_count"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
	Payout      float64 `json:"payout"`
	Percentage  float64 `json:"percentage"` // Percentage of total revenue
}

// DailyBreakdown contains daily CPM vs Non-CPM breakdown
type DailyBreakdown struct {
	Date         string  `json:"date"`
	CPMRevenue   float64 `json:"cpm_revenue"`
	NonCPMRevenue float64 `json:"non_cpm_revenue"`
	CPMClicks    int64   `json:"cpm_clicks"`
	NonCPMClicks int64   `json:"non_cpm_clicks"`
}

// ========== Utility Functions ==========

// GetPropertyName returns the full property name from a code
func GetPropertyName(code string) string {
	if name, ok := PropertyMapping[code]; ok {
		return name
	}
	return code // Return code if not found
}

// GetAllPropertyCodes returns all known property codes
func GetAllPropertyCodes() []string {
	codes := make([]string, 0, len(PropertyMapping))
	for code := range PropertyMapping {
		codes = append(codes, code)
	}
	return codes
}

// IsCPMOffer determines if an offer is CPM based on its name
func IsCPMOffer(offerName string) bool {
	return strings.Contains(strings.ToUpper(offerName), "CPM")
}

// GetOfferType returns the offer type (CPM, CPA, CPL, CPS, etc.) based on the name
func GetOfferType(offerName string) string {
	upperName := strings.ToUpper(offerName)
	types := []string{"CPM", "CPA", "CPL", "CPS", "CPC", "CPV"}
	for _, t := range types {
		if strings.Contains(upperName, t) {
			return t
		}
	}
	return "OTHER"
}
