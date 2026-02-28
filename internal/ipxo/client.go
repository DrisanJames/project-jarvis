package ipxo

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL = "https://hydra.ipxo.com/oauth2/token"
	apiBase  = "https://apigw.ipxo.com"
)

// Client is the IPXO API client with automatic OAuth2 token management.
type Client struct {
	config     Config
	httpClient *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// NewClient creates a new IPXO API client.
func NewClient(cfg Config) *Client {
	return &Client{
		config:     cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// IsConfigured returns true if the client has valid credentials.
func (c *Client) IsConfigured() bool {
	return c.config.ClientID != "" && c.config.SecretKey != "" && c.config.CompanyUUID != ""
}

// AllScopes is the full set of OAuth2 scopes the IPXO client requests.
const AllScopes = "billing ecommerce credits nethub-data nethub-analytics"

// getToken returns a cached token or fetches a new one. Thread-safe.
// Uses AllScopes to get a single token that works across all IPXO APIs.
func (c *Client) getToken(scopes string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return cached token if still valid (with 60s buffer)
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry.Add(-60*time.Second)) {
		return c.accessToken, nil
	}

	if scopes == "" {
		scopes = AllScopes
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.SecretKey},
		"scope":         {scopes},
	}

	resp, err := c.httpClient.PostForm(tokenURL, form)
	if err != nil {
		return "", fmt.Errorf("IPXO token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IPXO token error %d: %s", resp.StatusCode, string(body))
	}

	var tok TokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("IPXO token parse error: %w", err)
	}

	c.accessToken = tok.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	log.Printf("[IPXO] Token acquired (scope: %s, expires in: %ds)", tok.Scope, tok.ExpiresIn)

	return c.accessToken, nil
}

func (c *Client) doRequest(method, path string, reqBody interface{}, scopes string) ([]byte, int, error) {
	token, err := c.getToken(scopes)
	if err != nil {
		return nil, 0, err
	}

	apiURL := fmt.Sprintf("%s%s", apiBase, path)

	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = strings.NewReader(string(data))
	}

	req, err := http.NewRequest(method, apiURL, bodyReader)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("IPXO API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read IPXO response: %w", err)
	}

	return body, resp.StatusCode, nil
}

func (c *Client) get(path, scopes string) ([]byte, error) {
	body, status, err := c.doRequest("GET", path, nil, scopes)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("IPXO API %d: %s", status, string(body))
	}
	return body, nil
}

func (c *Client) post(path string, reqBody interface{}, scopes string) ([]byte, int, error) {
	return c.doRequest("POST", path, reqBody, scopes)
}

// tenant is a helper that injects the company UUID into a path template.
func (c *Client) tenant() string {
	return c.config.CompanyUUID
}

// =============================================================================
// NETHUB — Resource Discovery & Management
// =============================================================================

// SearchPrefixes returns all discovered prefixes for the tenant, with optional
// field filtering and limits.
func (c *Client) SearchPrefixes(fields []string, limit int) (*PrefixSearchResponse, error) {
	reqBody := map[string]interface{}{}
	if len(fields) > 0 {
		reqBody["fields"] = fields
	}
	if limit > 0 {
		reqBody["limit"] = limit
	}

	body, status, err := c.post(
		fmt.Sprintf("/nethub-data/%s/prefixes/search", c.tenant()),
		reqBody,
		AllScopes,
	)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("IPXO prefix search error %d: %s", status, string(body))
	}

	var result PrefixSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse prefix response: %w", err)
	}
	return &result, nil
}

// SearchPrefixesAll returns all prefixes with full enrichment.
func (c *Client) SearchPrefixesAll() (*PrefixSearchResponse, error) {
	return c.SearchPrefixes(nil, 0)
}

// GetUnannouncedPrefixes returns prefixes that are not currently announced via BGP.
func (c *Client) GetUnannouncedPrefixes() (*UnannouncedResponse, error) {
	body, status, err := c.post(
		fmt.Sprintf("/nethub-analytics/%s/aggregates/search/unannounced", c.tenant()),
		map[string]interface{}{},
		AllScopes,
	)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("IPXO unannounced error %d: %s", status, string(body))
	}

	var result UnannouncedResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse unannounced response: %w", err)
	}
	return &result, nil
}

// UpdatePrefixMetadata sets custom internal metadata on prefixes.
func (c *Client) UpdatePrefixMetadata(notation string, metadata map[string]interface{}) error {
	reqBody := []map[string]interface{}{
		{
			"notation": notation,
			"metadata": metadata,
		},
	}

	_, status, err := c.doRequest(
		"PATCH",
		fmt.Sprintf("/nethub-data/%s/prefixes/metadata", c.tenant()),
		reqBody,
		AllScopes,
	)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("IPXO metadata update failed with status %d", status)
	}
	return nil
}

// =============================================================================
// BILLING — Subscriptions, Invoices, Market
// =============================================================================

// ListSubscriptions returns active subscriptions for the tenant.
func (c *Client) ListSubscriptions(status string) ([]Subscription, error) {
	filter := map[string]interface{}{
		"filter": map[string]string{"status": status},
		"per_page": "100",
	}

	body, statusCode, err := c.post(
		fmt.Sprintf("/ecommerce/public/%s/subscriptions/search", c.tenant()),
		filter,
		AllScopes,
	)
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		return nil, fmt.Errorf("IPXO subscription search error %d: %s", statusCode, string(body))
	}

	var result SubscriptionSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse subscriptions: %w", err)
	}
	return result.Data, nil
}

// ListInvoices returns recent invoices.
func (c *Client) ListInvoices(page, perPage int) ([]Invoice, error) {
	path := fmt.Sprintf(
		"/ecommerce/public/%s/invoices?filter[status]=paid,unpaid,refunded&page=%d&per_page=%d&sort=-placed_at",
		c.tenant(), page, perPage,
	)

	body, err := c.get(path, AllScopes)
	if err != nil {
		return nil, err
	}

	var result InvoiceListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse invoices: %w", err)
	}
	return result.Data, nil
}

// GetCreditBalance returns the credit balance for the tenant.
func (c *Client) GetCreditBalance() (*CreditBalance, error) {
	path := fmt.Sprintf("/credits/public/%s/balances?filter[currency]=USD", c.tenant())
	body, err := c.get(path, AllScopes)
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Data []CreditBalance `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse credit balance: %w", err)
	}
	if len(wrapper.Data) > 0 {
		return &wrapper.Data[0], nil
	}
	return &CreditBalance{AvailableBalance: 0, TotalBalance: 0, Currency: "USD"}, nil
}

// ValidateASN checks whether an ASN can be assigned to the given subnets.
func (c *Client) ValidateASN(asn int, subnets []string) (*ASNValidation, error) {
	body, status, err := c.post(
		fmt.Sprintf("/billing/v1/%s/asn/validate/%d", c.tenant(), asn),
		map[string]interface{}{"subnets": subnets},
		AllScopes,
	)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("IPXO ASN validation error %d: %s", status, string(body))
	}

	var result ASNValidation
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse ASN validation: %w", err)
	}
	return &result, nil
}

// GetServiceLOAs returns LOAs for a service.
func (c *Client) GetServiceLOAs(serviceUUID string) ([]LOA, error) {
	path := fmt.Sprintf(
		"/billing/v1/%s/market/ipv4/services/%s/loa?statuses[0]=Active&statuses[1]=Pending",
		c.tenant(), serviceUUID,
	)
	body, err := c.get(path, AllScopes)
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Data []LOA `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse LOAs: %w", err)
	}
	return wrapper.Data, nil
}

// =============================================================================
// SERVICES — IPv4 Services (Leased Subnets) with LOA & ASN Status
// =============================================================================

// ListServices returns all IPv4 services (leased subnets) with LOA and billing details.
func (c *Client) ListServices() (*ServicesResponse, error) {
	path := fmt.Sprintf("/billing/v1/%s/market/ipv4/services", c.tenant())
	body, err := c.get(path, AllScopes)
	if err != nil {
		return nil, err
	}

	var result ServicesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse services: %w", err)
	}
	return &result, nil
}

// GetBillingAddresses returns billing addresses for the tenant.
func (c *Client) GetBillingAddresses() ([]BillingAddress, error) {
	path := fmt.Sprintf(
		"/ecommerce/public/%s/addresses?filter[addressable_type]=customer",
		c.tenant(),
	)
	body, err := c.get(path, AllScopes)
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Data []BillingAddress `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse addresses: %w", err)
	}
	return wrapper.Data, nil
}

// GetPaymentMethods returns payment methods for the tenant.
func (c *Client) GetPaymentMethods() ([]PaymentMethod, error) {
	path := fmt.Sprintf("/ecommerce/public/%s/payment-methods?per_page=999", c.tenant())
	body, err := c.get(path, AllScopes)
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Data []PaymentMethod `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse payment methods: %w", err)
	}
	return wrapper.Data, nil
}

// AssignASN performs the full LOA creation flow: validate → add to cart → checkout.
// Returns the order UUID on success.
func (c *Client) AssignASN(asn int, subnet string, companyName string) (*ASNAssignResult, error) {
	result := &ASNAssignResult{ASN: asn, Subnet: subnet}

	// Step 1: Validate ASN
	validation, err := c.ValidateASN(asn, []string{subnet})
	if err != nil {
		return nil, fmt.Errorf("ASN validation failed: %w", err)
	}
	if !validation.Valid {
		return nil, fmt.Errorf("ASN %d is not valid for subnet %s", asn, subnet)
	}
	result.ASName = validation.ASName
	result.Country = validation.Country

	if validation.AlreadyAdded {
		result.Status = "already_assigned"
		result.Message = fmt.Sprintf("ASN %d is already assigned to %s", asn, subnet)
		return result, nil
	}

	// Step 2: Add LOA item to cart
	loaItem := map[string]interface{}{
		"product_type":  "loa",
		"billing_cycle": 0,
		"product_fields": map[string]interface{}{
			"max_length":           24,
			"company_name":         companyName,
			"asn":                  asn,
			"info":                 "",
			"create_whois_inetnum": true,
			"whois_data_exposed":   false,
			"subnets":              []string{subnet},
		},
		"product_options": map[string]interface{}{
			"selection": map[string]string{
				"roa":   "yes",
				"radb":  "yes",
				"route": "yes",
			},
		},
	}

	_, addStatus, err := c.post(
		fmt.Sprintf("/billing/v1/%s/cart/items", c.tenant()),
		loaItem,
		AllScopes,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to add LOA to cart: %w", err)
	}
	if addStatus >= 400 && addStatus != 204 {
		return nil, fmt.Errorf("add LOA to cart returned status %d", addStatus)
	}

	// Step 3: Get cart UUID
	cartBody, err := c.get(
		fmt.Sprintf("/ecommerce/public/%s/cart", c.tenant()),
		AllScopes,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get cart: %w", err)
	}
	var cart struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(cartBody, &cart); err != nil {
		return nil, fmt.Errorf("failed to parse cart: %w", err)
	}
	result.CartUUID = cart.UUID

	// Step 4: Attach billing address
	addresses, err := c.GetBillingAddresses()
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("no billing address available")
	}

	_, addrStatus, err := c.post(
		fmt.Sprintf("/ecommerce/public/%s/cart/%s/addresses/%s", c.tenant(), cart.UUID, addresses[0].UUID),
		map[string]string{"type": "billing"},
		AllScopes,
	)
	if err != nil || (addrStatus >= 400 && addrStatus != 204) {
		return nil, fmt.Errorf("failed to attach billing address (status %d): %w", addrStatus, err)
	}

	// Step 5: Apply credits (free LOA or use credits to avoid 3DS card issue)
	balance, _ := c.GetCreditBalance()
	if balance != nil && balance.AvailableBalance > 0 {
		c.post(
			fmt.Sprintf("/ecommerce/public/%s/cart/%s/credits/apply", c.tenant(), cart.UUID),
			map[string]interface{}{"amount": balance.AvailableBalance},
			AllScopes,
		)
	}

	// Step 6: Attach payment method (required even if credits cover it)
	methods, err := c.GetPaymentMethods()
	if err != nil || len(methods) == 0 {
		return nil, fmt.Errorf("no payment method available")
	}
	defaultMethod := methods[0]
	for _, m := range methods {
		if m.IsDefault {
			defaultMethod = m
			break
		}
	}

	c.doRequest(
		"PATCH",
		fmt.Sprintf("/ecommerce/public/%s/cart/%s/payment-method/%s", c.tenant(), cart.UUID, defaultMethod.UUID),
		map[string]interface{}{},
		AllScopes,
	)

	// Step 7: Checkout
	checkoutBody, checkoutStatus, err := c.post(
		fmt.Sprintf("/ecommerce/public/%s/cart/%s/checkout", c.tenant(), cart.UUID),
		map[string]interface{}{},
		AllScopes,
	)
	if err != nil {
		return nil, fmt.Errorf("checkout failed: %w", err)
	}
	if checkoutStatus >= 400 {
		return nil, fmt.Errorf("checkout error %d: %s", checkoutStatus, string(checkoutBody))
	}

	var order struct {
		Order struct {
			UUID string `json:"uuid"`
		} `json:"order"`
	}
	json.Unmarshal(checkoutBody, &order)

	result.OrderUUID = order.Order.UUID
	result.Status = "submitted"
	result.Message = "LOA/ROA creation submitted. ROA confirmation typically arrives within 48 hours."

	log.Printf("[IPXO] ASN %d assignment to %s submitted (order: %s)", asn, subnet, result.OrderUUID)
	return result, nil
}
