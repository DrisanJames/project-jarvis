package ipxo

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Service bridges IPXO data with the local IP management database.
type Service struct {
	client *Client
	db     *sql.DB
}

// NewService creates an IPXO service.
func NewService(client *Client, db *sql.DB) *Service {
	return &Service{client: client, db: db}
}

// SyncResult holds the results of a sync operation.
type SyncResult struct {
	PrefixesFetched int      `json:"prefixes_fetched"`
	IPsImported     int      `json:"ips_imported"`
	IPsUpdated      int      `json:"ips_updated"`
	IPsSkipped      int      `json:"ips_skipped"`
	Errors          []string `json:"errors,omitempty"`
}

// SyncPrefixesToDB fetches all prefixes from IPXO and syncs them to the
// mailing_ip_addresses table. For each /24 block, it can optionally
// expand to individual IPs (for blocks assigned to the org).
func (s *Service) SyncPrefixesToDB(ctx context.Context, orgID string, expandIPs bool) (*SyncResult, error) {
	result := &SyncResult{}

	prefixes, err := s.client.SearchPrefixesAll()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch IPXO prefixes: %w", err)
	}
	result.PrefixesFetched = len(prefixes.Data)

	for _, p := range prefixes.Data {
		if expandIPs && p.MaskSize >= 24 {
			// Expand /24 into individual host IPs (skip .0 network and .255 broadcast)
			baseIP := strings.Split(p.Notation, "/")[0]
			octets := strings.Split(baseIP, ".")
			if len(octets) != 4 {
				result.Errors = append(result.Errors, fmt.Sprintf("invalid IP in prefix %s", p.Notation))
				continue
			}

			prefix3 := strings.Join(octets[:3], ".")
			for i := 1; i <= 254; i++ {
				ip := fmt.Sprintf("%s.%d", prefix3, i)
				hostname := fmt.Sprintf("mta%d.mail.ignitemailing.com", i)
				imported, err := s.upsertIP(ctx, orgID, ip, hostname, p)
				if err != nil {
					result.Errors = append(result.Errors, fmt.Sprintf("IP %s: %v", ip, err))
					continue
				}
				if imported {
					result.IPsImported++
				} else {
					result.IPsUpdated++
				}
			}
		} else {
			// Register the prefix as a single entry
			ip := strings.Split(p.Notation, "/")[0]
			imported, err := s.upsertIP(ctx, orgID, ip, "", p)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("prefix %s: %v", p.Notation, err))
				continue
			}
			if imported {
				result.IPsImported++
			} else {
				result.IPsUpdated++
			}
		}
	}

	log.Printf("[IPXO Sync] Fetched %d prefixes, imported %d IPs, updated %d, errors: %d",
		result.PrefixesFetched, result.IPsImported, result.IPsUpdated, len(result.Errors))
	return result, nil
}

func (s *Service) upsertIP(ctx context.Context, orgID, ip, hostname string, p Prefix) (bool, error) {
	if hostname == "" {
		hostname = ip
	}

	rir := ""
	country := ""
	if p.Whois != nil {
		rir = p.Whois.Registrar
		country = p.Whois.Country
	}

	var existingID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM mailing_ip_addresses WHERE ip_address = $1::inet`, ip).Scan(&existingID)

	if err == sql.ErrNoRows {
		// Insert new IP
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO mailing_ip_addresses (
				organization_id, ip_address, hostname,
				acquisition_type, broker, rir, cidr_block,
				hosting_provider, status, acquired_at
			) VALUES ($1, $2::inet, $3, 'purchased', 'ipxo', $4, $5, $6, 'pending', NOW())
		`, orgID, ip, hostname, rir, p.Notation, country)
		return true, err
	}
	if err != nil {
		return false, err
	}

	// Update existing IP with IPXO metadata
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_ip_addresses
		SET broker = 'ipxo',
		    rir = COALESCE(NULLIF($1, ''), rir),
		    cidr_block = COALESCE(NULLIF($2, ''), cidr_block),
		    updated_at = NOW()
		WHERE id = $3
	`, rir, p.Notation, existingID)
	return false, err
}

// GetDashboard assembles a full IPXO dashboard summary.
func (s *Service) GetDashboard() (*DashboardSummary, error) {
	dashboard := &DashboardSummary{}

	// Fetch prefixes from Nethub (may return 403 if not enabled in portal)
	prefixes, err := s.client.SearchPrefixesAll()
	if err != nil {
		log.Printf("[IPXO] Nethub prefix fetch not available (enable Resource Discovery in IPXO Portal): %v", err)
	} else {
		dashboard.Prefixes = prefixes.Data
		dashboard.TotalPrefixes = len(prefixes.Data)
		for _, p := range prefixes.Data {
			if p.MaskSize <= 24 {
				dashboard.TotalIPs += 1 << (32 - p.MaskSize)
			}
		}
	}

	// Fetch unannounced
	unannounced, err := s.client.GetUnannouncedPrefixes()
	if err != nil {
		log.Printf("[IPXO] Nethub analytics not available: %v", err)
	} else {
		dashboard.UnannouncedCount = len(unannounced.Data)
		dashboard.AnnouncedCount = dashboard.TotalPrefixes - dashboard.UnannouncedCount
	}

	// Fetch subscriptions — this is the most reliable source of IP block info
	subs, err := s.client.ListSubscriptions("active")
	if err != nil {
		log.Printf("[IPXO] Failed to fetch subscriptions: %v", err)
	} else {
		dashboard.Subscriptions = subs
		// If nethub prefixes are unavailable, derive prefix count from subscriptions
		if dashboard.TotalPrefixes == 0 && len(subs) > 0 {
			dashboard.TotalPrefixes = len(subs)
			for _, sub := range subs {
				dashboard.SubnetBlock = sub.Name
				// Parse /24 from subscription name
				if strings.Contains(sub.Name, "/24") {
					dashboard.TotalIPs += 256
				} else if strings.Contains(sub.Name, "/23") {
					dashboard.TotalIPs += 512
				} else if strings.Contains(sub.Name, "/22") {
					dashboard.TotalIPs += 1024
				}
			}
		}
	}

	// Fetch invoices
	invoices, err := s.client.ListInvoices(1, 10)
	if err != nil {
		log.Printf("[IPXO] Failed to fetch invoices: %v", err)
	} else {
		dashboard.Invoices = invoices
	}

	// Fetch credit balance
	balance, err := s.client.GetCreditBalance()
	if err != nil {
		log.Printf("[IPXO] Failed to fetch credit balance: %v", err)
	} else {
		dashboard.CreditBalance = balance
	}

	return dashboard, nil
}

// TagPrefixInIPXO writes custom metadata back to IPXO for a prefix.
func (s *Service) TagPrefixInIPXO(notation string, tags map[string]interface{}) error {
	return s.client.UpdatePrefixMetadata(notation, tags)
}

// SchedulePeriodicSync starts a background goroutine that syncs IPXO data every interval.
func (s *Service) SchedulePeriodicSync(orgID string, interval time.Duration) {
	go func() {
		log.Printf("[IPXO] Starting periodic sync (interval: %s)", interval)
		time.Sleep(30 * time.Second) // initial delay

		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, err := s.SyncPrefixesToDB(ctx, orgID, false)
			cancel()
			if err != nil {
				log.Printf("[IPXO] Periodic sync error: %v", err)
			}
			time.Sleep(interval)
		}
	}()
}
