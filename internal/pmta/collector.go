package pmta

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Collector periodically polls PMTA server(s) for metrics and persists
// the results to the analytics database.
type Collector struct {
	db       *sql.DB
	interval time.Duration
	servers  []ServerConfig
	metrics  *CollectorMetrics
	mu       sync.RWMutex
	parser   *AcctParser

	ctx    context.Context
	cancel context.CancelFunc
}

// ServerConfig describes a PMTA server to monitor.
type ServerConfig struct {
	ID       string
	OrgID    string
	Name     string
	Host     string
	SMTPPort int
	MgmtPort int
	APIKey   string
	AcctPath string // path to accounting CSV on the PMTA server (for local collection)
}

// NewCollector creates a PMTA metrics collector.
func NewCollector(db *sql.DB, interval time.Duration) *Collector {
	return &Collector{
		db:       db,
		interval: interval,
		parser:   NewAcctParser(),
		metrics:  &CollectorMetrics{IPHealth: make(map[string]*IPHealth)},
	}
}

// SetServers configures the PMTA servers to monitor. Call before Start.
func (c *Collector) SetServers(servers []ServerConfig) {
	c.servers = servers
}

// LoadServersFromDB reads PMTA server configs from the database.
func (c *Collector) LoadServersFromDB() error {
	rows, err := c.db.Query(`
		SELECT id, organization_id, name, host, smtp_port, mgmt_port, COALESCE(mgmt_api_key, '')
		FROM mailing_pmta_servers
		WHERE status = 'active'
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var servers []ServerConfig
	for rows.Next() {
		var s ServerConfig
		if err := rows.Scan(&s.ID, &s.OrgID, &s.Name, &s.Host, &s.SMTPPort, &s.MgmtPort, &s.APIKey); err != nil {
			log.Printf("[PMTACollector] Failed to scan server row: %v", err)
			continue
		}
		servers = append(servers, s)
	}
	c.servers = servers
	log.Printf("[PMTACollector] Loaded %d PMTA servers from database", len(servers))
	return nil
}

// Start begins periodic collection in a background goroutine.
func (c *Collector) Start() {
	c.ctx, c.cancel = context.WithCancel(context.Background())

	go func() {
		log.Printf("[PMTACollector] Starting collector (interval: %s, servers: %d)", c.interval, len(c.servers))

		// Initial collection after short delay
		time.Sleep(5 * time.Second)
		c.collect()

		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.collect()
			case <-c.ctx.Done():
				log.Println("[PMTACollector] Collector stopped")
				return
			}
		}
	}()
}

// Stop halts the collector.
func (c *Collector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

// GetMetrics returns the latest collected metrics (thread-safe).
func (c *Collector) GetMetrics() *CollectorMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metrics
}

func (c *Collector) collect() {
	start := time.Now()

	for _, srv := range c.servers {
		client := NewClient(srv.Host, srv.MgmtPort, srv.APIKey)
		m := c.collectServer(client, srv)
		c.persistMetrics(srv, m)
		c.updateServerHealth(srv.ID, m)
	}

	c.mu.Lock()
	c.metrics.LastCollected = time.Now()
	c.mu.Unlock()

	log.Printf("[PMTACollector] Collection cycle completed in %s", time.Since(start))
}

func (c *Collector) collectServer(client *Client, srv ServerConfig) *CollectorMetrics {
	m := &CollectorMetrics{
		IPHealth:      make(map[string]*IPHealth),
		LastCollected: time.Now(),
	}

	// Fetch status
	status, err := client.GetStatus()
	if err != nil {
		log.Printf("[PMTACollector] Failed to get status for %s: %v", srv.Name, err)
	} else {
		m.ServerStatus = status
	}

	// Fetch queues
	queues, err := client.GetQueues()
	if err != nil {
		log.Printf("[PMTACollector] Failed to get queues for %s: %v", srv.Name, err)
	} else {
		m.Queues = queues
	}

	// Fetch VMTAs
	vmtas, err := client.GetVMTAs()
	if err != nil {
		log.Printf("[PMTACollector] Failed to get VMTAs for %s: %v", srv.Name, err)
	} else {
		m.VMTAs = vmtas
	}

	// Fetch domains
	domains, err := client.GetDomains()
	if err != nil {
		log.Printf("[PMTACollector] Failed to get domains for %s: %v", srv.Name, err)
	} else {
		m.Domains = domains
	}

	// Parse accounting file if available locally
	if srv.AcctPath != "" {
		records, err := c.parser.ParseFile(srv.AcctPath)
		if err != nil {
			log.Printf("[PMTACollector] Failed to parse accounting file for %s: %v", srv.Name, err)
		} else {
			m.IPHealth = AggregateByIP(records)
			log.Printf("[PMTACollector] Parsed %d accounting records for %s", len(records), srv.Name)
		}
	}

	c.mu.Lock()
	c.metrics = m
	c.mu.Unlock()

	return m
}

func (c *Collector) persistMetrics(srv ServerConfig, m *CollectorMetrics) {
	if c.db == nil || m == nil {
		return
	}

	now := time.Now()

	// Persist server status snapshot
	if m.ServerStatus != nil {
		metricsJSON, _ := json.Marshal(m.ServerStatus)
		_, err := c.db.Exec(`
			INSERT INTO esp_metric_snapshots (id, organization_id, esp, snapshot_type, collected_at, period_start, period_end, metrics, source_hash)
			VALUES (gen_random_uuid(), $1, 'pmta', 'status', $2, $3, $4, $5, '')
			ON CONFLICT DO NOTHING
		`, srv.OrgID, now, now.Add(-c.interval), now, metricsJSON)
		if err != nil {
			log.Printf("[PMTACollector] Failed to persist status for %s: %v", srv.Name, err)
		}
	}

	// Persist VMTA (per-IP) metrics
	if len(m.VMTAs) > 0 {
		metricsJSON, _ := json.Marshal(m.VMTAs)
		_, err := c.db.Exec(`
			INSERT INTO esp_metric_snapshots (id, organization_id, esp, snapshot_type, collected_at, period_start, period_end, metrics, source_hash)
			VALUES (gen_random_uuid(), $1, 'pmta', 'ip', $2, $3, $4, $5, '')
			ON CONFLICT DO NOTHING
		`, srv.OrgID, now, now.Add(-c.interval), now, metricsJSON)
		if err != nil {
			log.Printf("[PMTACollector] Failed to persist VMTA metrics for %s: %v", srv.Name, err)
		}
	}

	// Persist domain metrics
	if len(m.Domains) > 0 {
		metricsJSON, _ := json.Marshal(m.Domains)
		_, err := c.db.Exec(`
			INSERT INTO esp_metric_snapshots (id, organization_id, esp, snapshot_type, collected_at, period_start, period_end, metrics, source_hash)
			VALUES (gen_random_uuid(), $1, 'pmta', 'domain', $2, $3, $4, $5, '')
			ON CONFLICT DO NOTHING
		`, srv.OrgID, now, now.Add(-c.interval), now, metricsJSON)
		if err != nil {
			log.Printf("[PMTACollector] Failed to persist domain metrics for %s: %v", srv.Name, err)
		}
	}
}

func (c *Collector) updateServerHealth(serverID string, m *CollectorMetrics) {
	if c.db == nil || serverID == "" {
		return
	}

	status := "healthy"
	if m.ServerStatus == nil {
		status = "unreachable"
	}

	_, err := c.db.Exec(`
		UPDATE mailing_pmta_servers
		SET last_health_check = NOW(),
		    health_status = $1,
		    updated_at = NOW()
		WHERE id = $2
	`, status, serverID)
	if err != nil {
		log.Printf("[PMTACollector] Failed to update server health for %s: %v", serverID, err)
	}
}
