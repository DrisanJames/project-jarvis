package ipxo

import "time"

// Config holds IPXO API credentials and settings.
type Config struct {
	ClientID    string `yaml:"client_id"`
	SecretKey   string `yaml:"secret_key"`
	CompanyUUID string `yaml:"company_uuid"`
}

// TokenResponse is the OAuth2 token response from IPXO Hydra.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	TokenType   string `json:"token_type"`
}

// Prefix represents a discovered IP prefix from IPXO's Nethub.
type Prefix struct {
	Notation string `json:"notation"`
	IPNet    struct {
		IP   string `json:"IP"`
		Mask string `json:"Mask"`
	} `json:"ipNet"`
	MaskSize         int               `json:"maskSize"`
	InternalMetadata *InternalMetadata  `json:"internalMetadata,omitempty"`
	Geodata          []GeoProvider      `json:"geodata,omitempty"`
	Whois            *WhoisData         `json:"whois,omitempty"`
	BGP              interface{}        `json:"bgp,omitempty"`
	Routes           []RouteEntry       `json:"routes,omitempty"`
	RPKI             interface{}        `json:"rpki,omitempty"`
	HolderMetadata   map[string]interface{} `json:"holderMetadata,omitempty"`
}

// InternalMetadata contains IPXO's internal resource metadata.
type InternalMetadata struct {
	Internal          bool   `json:"internal"`
	ReadOnly          bool   `json:"readOnly"`
	Master            bool   `json:"master"`
	PrefixLengthLimits struct {
		Min   int  `json:"min"`
		Max   int  `json:"max"`
		Exact *int `json:"exact"`
	} `json:"prefixLengthLimits"`
	Holder struct {
		TenantUUID   string `json:"tenantUUID"`
		ProjectID    string `json:"projectID"`
		Organisation string `json:"organisation"`
	} `json:"holder"`
}

// GeoProvider holds geolocation data from a single provider.
type GeoProvider struct {
	Provider    string `json:"provider"`
	CountryName string `json:"countryName"`
	CountryCode string `json:"countryCode"`
	CityName    string `json:"cityName"`
	State       string `json:"state,omitempty"`
	Date        string `json:"date"`
}

// WhoisData holds WHOIS registration data for a prefix.
type WhoisData struct {
	Inetnum      string    `json:"inetnum"`
	Registrar    string    `json:"registrar"`
	Netname      string    `json:"netname"`
	Description  string    `json:"description"`
	Country      string    `json:"country"`
	Status       string    `json:"status"`
	Source       string    `json:"source"`
	Organisation string    `json:"organisation"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"last_modified"`
	MntBy        []string  `json:"mnt_by"`
	Domains      []struct {
		Domain   string   `json:"domain"`
		NServers []string `json:"nservers"`
	} `json:"domains"`
}

// RouteEntry represents a BGP route object.
type RouteEntry struct {
	Route  string `json:"route"`
	Origin string `json:"origin"`
	Source string `json:"source"`
}

// PrefixSearchResponse wraps the prefix search results.
type PrefixSearchResponse struct {
	Data     []Prefix `json:"data"`
	Metadata struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	} `json:"metadata"`
}

// UnannouncedResponse wraps the unannounced prefix results.
type UnannouncedResponse struct {
	Data []struct {
		Notation   string    `json:"notation"`
		TenantUUID string    `json:"tenant_uuid"`
		Org        string    `json:"organisation"`
		IsMaster   bool      `json:"isMaster"`
		LastSeen   time.Time `json:"lastSeen"`
	} `json:"data"`
	Metadata struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	} `json:"metadata"`
}

// MoneyAmount represents a monetary value from the IPXO API.
type MoneyAmount struct {
	Currency    string `json:"currency"`
	Amount      string `json:"amount"`
	AmountMinor int    `json:"amount_minor"`
}

// Subscription represents an IPXO subscription (e.g. a leased subnet).
type Subscription struct {
	UUID                 string     `json:"uuid"`
	Name                 string     `json:"name"`
	ShortDescription     string     `json:"short_description"`
	Status               string     `json:"status"`
	CurrentPeriodStart   string     `json:"current_period_start"`
	CurrentPeriodEnd     string     `json:"current_period_end"`
	StartedAt            string     `json:"started_at"`
	TerminatedAt         *string    `json:"terminated_at"`
	Total                MoneyAmount `json:"total"`
	SubTotal             MoneyAmount `json:"sub_total"`
	HasImmediateTermination bool    `json:"has_immediate_termination"`
	IsInGracePeriod      bool       `json:"is_in_grace_period"`
}

// SubscriptionSearchResponse wraps subscription search results.
type SubscriptionSearchResponse struct {
	Data []Subscription `json:"data"`
	Meta struct {
		CurrentPage int `json:"current_page"`
		Total       int `json:"total"`
		PerPage     int `json:"per_page"`
	} `json:"meta"`
}

// Invoice represents an IPXO invoice.
type Invoice struct {
	UUID           string      `json:"uuid"`
	Reference      string      `json:"reference"`
	Status         string      `json:"status"`
	PlacedAt       string      `json:"placed_at"`
	SubTotal       MoneyAmount `json:"sub_total"`
	DiscountTotal  MoneyAmount `json:"discount_total"`
	TaxTotal       MoneyAmount `json:"tax_total"`
	CreditsTotal   MoneyAmount `json:"credits_total"`
	Total          MoneyAmount `json:"total"`
}

// InvoiceListResponse wraps invoice list results.
type InvoiceListResponse struct {
	Data []Invoice `json:"data"`
	Meta struct {
		CurrentPage int `json:"current_page"`
		Total       int `json:"total"`
		PerPage     int `json:"per_page"`
	} `json:"meta"`
}

// LOA represents a Letter of Authorization for a subnet.
type LOA struct {
	UUID        string `json:"uuid"`
	Status      string `json:"status"`
	ASN         int    `json:"asn"`
	CompanyName string `json:"company_name"`
	CreatedAt   string `json:"created_at"`
}

// ASNValidation holds the result of validating an ASN for a subnet.
type ASNValidation struct {
	Valid       bool   `json:"valid"`
	ASName      string `json:"as_name"`
	Country     string `json:"country"`
	AlreadyAdded bool  `json:"already_added"`
}

// CreditBalance holds the credit balance for a tenant.
type CreditBalance struct {
	AvailableBalance float64 `json:"available_balance"`
	TotalBalance     float64 `json:"total_balance"`
	Currency         string  `json:"currency"`
	UpdatedAt        string  `json:"updated_at"`
}

// IPv4Service represents a leased IPv4 service in IPXO.
type IPv4Service struct {
	BillingService struct {
		Address          string  `json:"address"`
		CIDR             int     `json:"cidr"`
		NextDueDate      int64   `json:"next_due_date"`
		RecurringAmount  float64 `json:"recurring_amount"`
		Status           string  `json:"status"`
		UUID             string  `json:"uuid"`
		EcomSubUUID      *string `json:"ecommerce_subscription_uuid"`
	} `json:"billing_service"`
	LOA []struct {
		UUID   string `json:"uuid"`
		Status string `json:"status"`
	} `json:"loa"`
	MarketService struct {
		Registry string  `json:"registry"`
		UUID     string  `json:"uuid"`
		ExpiresAt *string `json:"expires_at"`
	} `json:"market_service"`
	EcomSubUUID    *string `json:"ecommerce_subscription_uuid"`
}

// ServicesResponse wraps the IPv4 services list.
type ServicesResponse struct {
	Data []IPv4Service `json:"data"`
	Meta struct {
		CurrentPage int `json:"current_page"`
		Total       int `json:"total"`
		PerPage     int `json:"per_page"`
	} `json:"meta"`
}

// BillingAddress holds a tenant's billing address.
type BillingAddress struct {
	UUID         string  `json:"uuid"`
	CompanyName  *string `json:"company_name"`
	LineOne      string  `json:"line_one"`
	City         string  `json:"city"`
	CountryCode  string  `json:"country_code"`
	Postcode     string  `json:"postcode"`
	ContactEmail string  `json:"contact_email"`
}

// PaymentMethod holds a tenant's payment method.
type PaymentMethod struct {
	UUID      string `json:"uuid"`
	Type      string `json:"type"`
	IsDefault bool   `json:"is_default"`
	Details   struct {
		CardBrand    string `json:"card_brand,omitempty"`
		CardLastFour string `json:"card_last_four,omitempty"`
		Email        string `json:"email,omitempty"`
	} `json:"details"`
}

// ASNAssignResult holds the result of an ASN assignment (LOA creation) flow.
type ASNAssignResult struct {
	ASN       int    `json:"asn"`
	ASName    string `json:"as_name"`
	Country   string `json:"country"`
	Subnet    string `json:"subnet"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	CartUUID  string `json:"cart_uuid,omitempty"`
	OrderUUID string `json:"order_uuid,omitempty"`
}

// SetupStatus tracks the overall IP setup workflow progress.
type SetupStatus struct {
	SubnetLeased     bool   `json:"subnet_leased"`
	SubnetBlock      string `json:"subnet_block"`
	ASNAssigned      bool   `json:"asn_assigned"`
	ASN              int    `json:"asn,omitempty"`
	ASName           string `json:"as_name,omitempty"`
	LOAStatus        string `json:"loa_status"`
	RDNSConfigured   bool   `json:"rdns_configured"`
	RDNSNote         string `json:"rdns_note"`
	ForwardDNSNote   string `json:"forward_dns_note"`
	ServerAttached   bool   `json:"server_attached"`
	ServerNote       string `json:"server_note"`
}

// DashboardSummary aggregates IPXO data for the frontend dashboard.
type DashboardSummary struct {
	TotalPrefixes    int             `json:"total_prefixes"`
	TotalIPs         int             `json:"total_ips"`
	AnnouncedCount   int             `json:"announced_count"`
	UnannouncedCount int             `json:"unannounced_count"`
	Prefixes         []Prefix        `json:"prefixes"`
	Subscriptions    []Subscription  `json:"subscriptions"`
	Invoices         []Invoice       `json:"invoices"`
	CreditBalance    *CreditBalance  `json:"credit_balance"`
	SubnetBlock      string          `json:"subnet_block,omitempty"`
}
