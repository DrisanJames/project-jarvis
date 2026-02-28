package pmta

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client communicates with the PMTA HTTP management API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a PMTA management API client.
func NewClient(host string, port int, apiKey string) *Client {
	return &Client{
		baseURL:    fmt.Sprintf("http://%s:%d", host, port),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) get(path string) ([]byte, error) {
	url := fmt.Sprintf("%s%s", c.baseURL, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PMTA API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read PMTA response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PMTA API returned %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// XML response structures matching PMTA's management API output format.

type xmlStatus struct {
	XMLName xml.Name `xml:"status"`
	Version string   `xml:"version"`
	Uptime  string   `xml:"uptime"`
	Traffic struct {
		Queued struct {
			Total int `xml:"total"`
		} `xml:"queued"`
		ConnectionsIn  int `xml:"conn-in"`
		ConnectionsOut int `xml:"conn-out"`
	} `xml:"traffic"`
}

type xmlQueues struct {
	XMLName xml.Name       `xml:"queues"`
	Entries []xmlQueueItem `xml:"queue"`
}

type xmlQueueItem struct {
	Domain     string `xml:"domain,attr"`
	VMTA       string `xml:"vmta,attr"`
	Queued     int    `xml:"queued"`
	Recipients int    `xml:"rcpts"`
	Errors     int    `xml:"errors"`
	Expired    int    `xml:"expired"`
}

type xmlVMTAs struct {
	XMLName xml.Name      `xml:"vmtas"`
	VMTAs   []xmlVMTAItem `xml:"vmta"`
}

type xmlVMTAItem struct {
	Name      string `xml:"name,attr"`
	SourceIP  string `xml:"source-ip"`
	Hostname  string `xml:"hostname"`
	ConnOut   int    `xml:"conn-out"`
	Queued    int    `xml:"queued"`
	Delivered int    `xml:"delivered"`
	Bounced   int    `xml:"bounced"`
}

type xmlDomains struct {
	XMLName xml.Name        `xml:"domains"`
	Domains []xmlDomainItem `xml:"domain"`
}

type xmlDomainItem struct {
	Name      string `xml:"name,attr"`
	Queued    int    `xml:"queued"`
	Delivered int    `xml:"delivered"`
	Bounced   int    `xml:"bounced"`
	ConnOut   int    `xml:"conn-out"`
}

// GetStatus returns the overall PMTA server status.
func (c *Client) GetStatus() (*ServerStatus, error) {
	body, err := c.get("/status?format=xml")
	if err != nil {
		return nil, err
	}

	var xs xmlStatus
	if err := xml.Unmarshal(body, &xs); err != nil {
		return nil, fmt.Errorf("failed to parse PMTA status XML: %w", err)
	}

	return &ServerStatus{
		Version:        xs.Version,
		Uptime:         xs.Uptime,
		TotalQueued:    xs.Traffic.Queued.Total,
		ConnectionsIn:  xs.Traffic.ConnectionsIn,
		ConnectionsOut: xs.Traffic.ConnectionsOut,
		CheckedAt:      time.Now(),
	}, nil
}

// GetQueues returns the current queue state grouped by domain/VMTA.
func (c *Client) GetQueues() ([]QueueEntry, error) {
	body, err := c.get("/queues?format=xml")
	if err != nil {
		return nil, err
	}

	var xq xmlQueues
	if err := xml.Unmarshal(body, &xq); err != nil {
		return nil, fmt.Errorf("failed to parse PMTA queues XML: %w", err)
	}

	entries := make([]QueueEntry, len(xq.Entries))
	for i, e := range xq.Entries {
		entries[i] = QueueEntry{
			Domain:     e.Domain,
			VMTA:       e.VMTA,
			Queued:     e.Queued,
			Recipients: e.Recipients,
			Errors:     e.Errors,
			Expired:    e.Expired,
		}
	}
	return entries, nil
}

// GetVMTAs returns the status of all Virtual MTAs.
func (c *Client) GetVMTAs() ([]VMTAStatus, error) {
	body, err := c.get("/vmtas?format=xml")
	if err != nil {
		return nil, err
	}

	var xv xmlVMTAs
	if err := xml.Unmarshal(body, &xv); err != nil {
		return nil, fmt.Errorf("failed to parse PMTA vmtas XML: %w", err)
	}

	vmtas := make([]VMTAStatus, len(xv.VMTAs))
	for i, v := range xv.VMTAs {
		total := v.Delivered + v.Bounced
		var rate float64
		if total > 0 {
			rate = float64(v.Delivered) / float64(total) * 100
		}
		vmtas[i] = VMTAStatus{
			Name:           v.Name,
			SourceIP:       v.SourceIP,
			Hostname:       v.Hostname,
			ConnectionsOut: v.ConnOut,
			Queued:         v.Queued,
			Delivered:      v.Delivered,
			Bounced:        v.Bounced,
			DeliveryRate:   rate,
		}
	}
	return vmtas, nil
}

// GetDomains returns delivery stats for all destination domains.
func (c *Client) GetDomains() ([]DomainStatus, error) {
	body, err := c.get("/domains?format=xml")
	if err != nil {
		return nil, err
	}

	var xd xmlDomains
	if err := xml.Unmarshal(body, &xd); err != nil {
		return nil, fmt.Errorf("failed to parse PMTA domains XML: %w", err)
	}

	domains := make([]DomainStatus, len(xd.Domains))
	for i, d := range xd.Domains {
		total := d.Delivered + d.Bounced
		var rate float64
		if total > 0 {
			rate = float64(d.Delivered) / float64(total) * 100
		}
		domains[i] = DomainStatus{
			Domain:         d.Name,
			Queued:         d.Queued,
			Delivered:      d.Delivered,
			Bounced:        d.Bounced,
			ConnectionsOut: d.ConnOut,
			DeliveryRate:   rate,
		}
	}
	return domains, nil
}

// Reload triggers a configuration reload on the PMTA server.
func (c *Client) Reload() error {
	url := fmt.Sprintf("%s/reload", c.baseURL)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PMTA reload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PMTA reload failed with %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// UploadConfig uploads a config file to PMTA via the management API.
func (c *Client) UploadConfig(configContent string) error {
	url := fmt.Sprintf("%s/configFile", c.baseURL)
	req, err := http.NewRequest("POST", url, strings.NewReader(configContent))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PMTA config upload failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PMTA config upload returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ParseUptime converts PMTA uptime strings like "3d 4h 12m" into a duration.
func ParseUptime(s string) time.Duration {
	var d time.Duration
	parts := strings.Fields(s)
	for _, p := range parts {
		if len(p) < 2 {
			continue
		}
		unit := p[len(p)-1]
		val, err := strconv.Atoi(p[:len(p)-1])
		if err != nil {
			continue
		}
		switch unit {
		case 'd':
			d += time.Duration(val) * 24 * time.Hour
		case 'h':
			d += time.Duration(val) * time.Hour
		case 'm':
			d += time.Duration(val) * time.Minute
		case 's':
			d += time.Duration(val) * time.Second
		}
	}
	return d
}
