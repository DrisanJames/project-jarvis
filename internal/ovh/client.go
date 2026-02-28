package ovh

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var endpoints = map[string]string{
	"ovh-us": "https://api.us.ovhcloud.com/1.0",
	"ovh-eu": "https://eu.api.ovh.com/1.0",
	"ovh-ca": "https://ca.api.ovh.com/1.0",
}

// Client is the OVHCloud REST API client.
// Auth uses OVH's signed-request scheme (AK + AS + CK + timestamp signature).
type Client struct {
	appKey      string
	appSecret   string
	consumerKey string
	baseURL     string
	httpClient  *http.Client
	timeDelta   int64
}

// NewClient creates a new OVHCloud API client.
func NewClient(endpoint, appKey, appSecret, consumerKey string) *Client {
	base, ok := endpoints[endpoint]
	if !ok {
		base = endpoints["ovh-us"]
	}
	return &Client{
		appKey:      appKey,
		appSecret:   appSecret,
		consumerKey: consumerKey,
		baseURL:     base,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// IsConfigured returns true if all API credentials are set.
func (c *Client) IsConfigured() bool {
	return c.appKey != "" && c.appSecret != "" && c.consumerKey != ""
}

func (c *Client) getTimeDelta() int64 {
	if c.timeDelta != 0 {
		return c.timeDelta
	}
	resp, err := http.Get(c.baseURL + "/auth/time")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var serverTime int64
	if json.Unmarshal(body, &serverTime) == nil {
		c.timeDelta = serverTime - time.Now().Unix()
	}
	return c.timeDelta
}

func (c *Client) sign(method, url, body string, timestamp int64) string {
	toSign := fmt.Sprintf("%s+%s+%s+%s+%s+%d",
		c.appSecret, c.consumerKey, method, url, body, timestamp)
	h := sha1.New()
	h.Write([]byte(toSign))
	return fmt.Sprintf("$1$%x", h.Sum(nil))
}

func (c *Client) doRequest(method, path string, reqBody interface{}, result interface{}) error {
	url := c.baseURL + path

	var bodyStr string
	var reader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyStr = string(data)
		reader = strings.NewReader(bodyStr)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	timestamp := time.Now().Unix() + c.getTimeDelta()

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ovh-Application", c.appKey)
	req.Header.Set("X-Ovh-Consumer", c.consumerKey)
	req.Header.Set("X-Ovh-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("X-Ovh-Signature", c.sign(method, url, bodyStr, timestamp))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("OVH API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("OVH API %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}
	return nil
}

// =============================================================================
// DEDICATED SERVERS
// =============================================================================

// ListDedicatedServers returns service names for all dedicated servers.
func (c *Client) ListDedicatedServers() ([]string, error) {
	var names []string
	err := c.doRequest("GET", "/dedicated/server", nil, &names)
	return names, err
}

// GetDedicatedServer returns details for a specific dedicated server.
func (c *Client) GetDedicatedServer(serviceName string) (*DedicatedServer, error) {
	var server DedicatedServer
	err := c.doRequest("GET", "/dedicated/server/"+serviceName, nil, &server)
	if err != nil {
		return nil, err
	}
	return &server, nil
}

// RebootServer requests a hardware reboot.
func (c *Client) RebootServer(serviceName string) (*ServerTask, error) {
	var task ServerTask
	err := c.doRequest("POST", "/dedicated/server/"+serviceName+"/reboot", nil, &task)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// =============================================================================
// FAILOVER IPs & IP BLOCKS
// =============================================================================

// ListIPs returns all IP blocks associated with the account.
// Filter by type: "failover", "cloud", or "" for all.
func (c *Client) ListIPs(ipType string) ([]string, error) {
	path := "/ip"
	if ipType != "" {
		path += "?type=" + ipType
	}
	var blocks []string
	err := c.doRequest("GET", path, nil, &blocks)
	return blocks, err
}

// GetIPBlock returns details for an IP block.
func (c *Client) GetIPBlock(block string) (*IPBlock, error) {
	var info IPBlock
	err := c.doRequest("GET", "/ip/"+block, nil, &info)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ListFailoverIPs returns all failover IPs for a given server.
func (c *Client) ListFailoverIPs(serviceName string) ([]FailoverIP, error) {
	var ips []string
	err := c.doRequest("GET", fmt.Sprintf("/dedicated/server/%s/secondaryDnsDomains", serviceName), nil, &ips)
	if err != nil {
		// Fallback: list all IPs and filter by routedTo
		return c.listFailoverIPsForServer(serviceName)
	}
	return nil, nil
}

func (c *Client) listFailoverIPsForServer(serviceName string) ([]FailoverIP, error) {
	blocks, err := c.ListIPs("failover")
	if err != nil {
		return nil, err
	}

	var result []FailoverIP
	for _, block := range blocks {
		info, err := c.GetIPBlock(block)
		if err != nil {
			continue
		}
		if info.RoutedTo == serviceName {
			result = append(result, FailoverIP{
				IP:         block,
				RouteredTo: info.RoutedTo,
				Country:    info.Country,
				Type:       info.Type,
				Status:     "active",
			})
		}
	}
	return result, nil
}

// MoveFailoverIP routes a failover IP to a different server.
func (c *Client) MoveFailoverIP(ip, targetServer string) (*ServerTask, error) {
	body := map[string]string{"to": targetServer}
	var task ServerTask
	err := c.doRequest("POST", "/ip/"+ip+"/move", body, &task)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// =============================================================================
// REVERSE DNS (PTR RECORDS)
// =============================================================================

// GetReverseDNS returns the PTR record for an IP.
func (c *Client) GetReverseDNS(ip string) (*ReverseDNS, error) {
	var rev ReverseDNS
	err := c.doRequest("GET", fmt.Sprintf("/ip/%s/reverse/%s", ip, ip), nil, &rev)
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

// SetReverseDNS sets the PTR record for a failover IP.
// The reverse hostname must resolve to the IP (forward DNS must match).
func (c *Client) SetReverseDNS(ip, reverse string) error {
	body := map[string]string{
		"ipReverse": ip,
		"reverse":   reverse,
	}
	return c.doRequest("POST", fmt.Sprintf("/ip/%s/reverse", ip), body, nil)
}

// DeleteReverseDNS removes the PTR record for an IP.
func (c *Client) DeleteReverseDNS(ip string) error {
	return c.doRequest("DELETE", fmt.Sprintf("/ip/%s/reverse/%s", ip, ip), nil, nil)
}

// =============================================================================
// FIREWALL
// =============================================================================

// GetFirewallStatus checks if the OVH network firewall is enabled for an IP.
func (c *Client) GetFirewallStatus(ip string) (bool, error) {
	var result struct {
		Enabled bool `json:"enabled"`
	}
	err := c.doRequest("GET", fmt.Sprintf("/ip/%s/firewall/%s", ip, ip), nil, &result)
	if err != nil {
		return false, err
	}
	return result.Enabled, nil
}
