package vultr

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.vultr.com/v2"

// Client is the Vultr API v2 client.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new Vultr API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// IsConfigured returns true if the API key is set.
func (c *Client) IsConfigured() bool {
	return c.apiKey != ""
}

func (c *Client) doRequest(method, path string, body interface{}) ([]byte, int, error) {
	url := apiBase + path

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = strings.NewReader(string(data))
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Vultr API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return respBody, resp.StatusCode, nil
}

func (c *Client) get(path string) ([]byte, error) {
	body, status, err := c.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("Vultr API %d: %s", status, string(body))
	}
	return body, nil
}

// =============================================================================
// BARE METAL
// =============================================================================

// ListBareMetalPlans returns available bare metal plans.
func (c *Client) ListBareMetalPlans() ([]BareMetalPlan, error) {
	body, err := c.get("/plans-metal")
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Plans []BareMetalPlan `json:"plans_metal"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse plans: %w", err)
	}
	return wrapper.Plans, nil
}

// ListRegions returns all Vultr regions.
func (c *Client) ListRegions() ([]Region, error) {
	body, err := c.get("/regions")
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Regions []Region `json:"regions"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse regions: %w", err)
	}
	return wrapper.Regions, nil
}

// ListOperatingSystems returns available OS options.
func (c *Client) ListOperatingSystems() ([]OperatingSystem, error) {
	body, err := c.get("/os")
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		OS []OperatingSystem `json:"os"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse OS list: %w", err)
	}
	return wrapper.OS, nil
}

// ListSSHKeys returns stored SSH keys.
func (c *Client) ListSSHKeys() ([]SSHKey, error) {
	body, err := c.get("/ssh-keys")
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		Keys []SSHKey `json:"ssh_keys"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse SSH keys: %w", err)
	}
	return wrapper.Keys, nil
}

// CreateBareMetalServer provisions a new bare metal instance.
func (c *Client) CreateBareMetalServer(req CreateBareMetalRequest) (*BareMetalInstance, error) {
	body, status, err := c.doRequest("POST", "/bare-metals", req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("Vultr create bare metal error %d: %s", status, string(body))
	}

	var wrapper struct {
		BareMetal BareMetalInstance `json:"bare_metal"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse bare metal response: %w", err)
	}
	return &wrapper.BareMetal, nil
}

// GetBareMetalServer returns details for a specific bare metal instance.
func (c *Client) GetBareMetalServer(id string) (*BareMetalInstance, error) {
	body, err := c.get("/bare-metals/" + id)
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		BareMetal BareMetalInstance `json:"bare_metal"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse bare metal: %w", err)
	}
	return &wrapper.BareMetal, nil
}

// ListBareMetalServers returns all bare metal instances.
func (c *Client) ListBareMetalServers() ([]BareMetalInstance, error) {
	body, err := c.get("/bare-metals")
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		BareMetals []BareMetalInstance `json:"bare_metals"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse bare metal list: %w", err)
	}
	return wrapper.BareMetals, nil
}

// RebootBareMetalServer reboots a bare metal instance.
func (c *Client) RebootBareMetalServer(id string) error {
	_, status, err := c.doRequest("POST", "/bare-metals/"+id+"/reboot", nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("Vultr reboot error: status %d", status)
	}
	return nil
}

// HaltBareMetalServer halts a bare metal instance.
func (c *Client) HaltBareMetalServer(id string) error {
	_, status, err := c.doRequest("POST", "/bare-metals/"+id+"/halt", nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("Vultr halt error: status %d", status)
	}
	return nil
}

// DeleteBareMetalServer destroys a bare metal instance.
func (c *Client) DeleteBareMetalServer(id string) error {
	_, status, err := c.doRequest("DELETE", "/bare-metals/"+id, nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("Vultr delete error: status %d", status)
	}
	return nil
}

// =============================================================================
// BGP
// =============================================================================

// GetBGPInfo retrieves BGP credentials and status for the account.
func (c *Client) GetBGPInfo() (*BGPInfo, error) {
	body, err := c.get("/account/bgp")
	if err != nil {
		return nil, err
	}

	var wrapper struct {
		BGP BGPInfo `json:"bgp"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse BGP info: %w", err)
	}
	return &wrapper.BGP, nil
}
