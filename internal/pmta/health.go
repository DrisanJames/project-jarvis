package pmta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// HealthChecker performs DNS verification and blacklist checks for IPs.
type HealthChecker struct {
	db *sql.DB
}

// NewHealthChecker creates a health checker backed by the database.
func NewHealthChecker(db *sql.DB) *HealthChecker {
	return &HealthChecker{db: db}
}

// DNSCheckResult contains forward and reverse DNS verification results.
type DNSCheckResult struct {
	IP              string `json:"ip"`
	Hostname        string `json:"hostname"`
	ForwardMatch    bool   `json:"forward_match"`
	ReverseMatch    bool   `json:"reverse_match"`
	ForwardResolved string `json:"forward_resolved"`
	ReverseResolved string `json:"reverse_resolved"`
	Error           string `json:"error,omitempty"`
}

// BlacklistCheckResult contains the results of checking an IP against DNS blacklists.
type BlacklistCheckResult struct {
	IP         string   `json:"ip"`
	Listed     bool     `json:"listed"`
	Blacklists []string `json:"blacklists"`
	Clean      []string `json:"clean"`
	Errors     []string `json:"errors,omitempty"`
	CheckedAt  string   `json:"checked_at"`
}

var dnsBlacklists = []string{
	"zen.spamhaus.org",
	"b.barracudacentral.org",
	"bl.spamcop.net",
	"dnsbl.sorbs.net",
	"cbl.abuseat.org",
	"dnsbl-1.uceprotect.net",
	"psbl.surriel.com",
	"dyna.spamrats.com",
}

// CheckDNS verifies that forward and reverse DNS match for an IP/hostname pair.
func (h *HealthChecker) CheckDNS(ctx context.Context, ip, hostname string) (*DNSCheckResult, error) {
	result := &DNSCheckResult{
		IP:       ip,
		Hostname: hostname,
	}

	resolver := &net.Resolver{PreferGo: true}

	// Reverse DNS: IP -> hostname
	names, err := resolver.LookupAddr(ctx, ip)
	if err != nil {
		result.Error = fmt.Sprintf("reverse DNS lookup failed: %v", err)
	} else if len(names) > 0 {
		result.ReverseResolved = strings.TrimSuffix(names[0], ".")
		result.ReverseMatch = strings.EqualFold(result.ReverseResolved, hostname)
	}

	// Forward DNS: hostname -> IP
	addrs, err := resolver.LookupHost(ctx, hostname)
	if err != nil {
		if result.Error != "" {
			result.Error += "; "
		}
		result.Error += fmt.Sprintf("forward DNS lookup failed: %v", err)
	} else {
		for _, addr := range addrs {
			if addr == ip {
				result.ForwardMatch = true
				break
			}
		}
		if len(addrs) > 0 {
			result.ForwardResolved = addrs[0]
		}
	}

	// Persist verification result
	if h.db != nil {
		verified := result.ForwardMatch && result.ReverseMatch
		_, _ = h.db.ExecContext(ctx, `
			UPDATE mailing_ip_addresses
			SET rdns_verified = $1,
			    rdns_last_checked = NOW(),
			    updated_at = NOW()
			WHERE ip_address = $2::inet
		`, verified, ip)
	}

	return result, nil
}

// CheckBlacklists queries DNS-based blacklists for an IP address.
func (h *HealthChecker) CheckBlacklists(ctx context.Context, ip string) (*BlacklistCheckResult, error) {
	result := &BlacklistCheckResult{
		IP:        ip,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Reverse the IP octets for DNSBL queries
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid IPv4 address: %s", ip)
	}
	reversed := fmt.Sprintf("%s.%s.%s.%s", parts[3], parts[2], parts[1], parts[0])

	resolver := &net.Resolver{PreferGo: true}

	for _, bl := range dnsBlacklists {
		query := fmt.Sprintf("%s.%s", reversed, bl)
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := resolver.LookupHost(lookupCtx, query)
		cancel()

		if err != nil {
			// DNS NXDOMAIN means not listed (this is the normal case)
			result.Clean = append(result.Clean, bl)
			continue
		}

		// A response with 127.0.0.x means the IP is listed
		for _, addr := range addrs {
			if strings.HasPrefix(addr, "127.") {
				result.Listed = true
				result.Blacklists = append(result.Blacklists, bl)
				break
			}
		}
		if !result.Listed || len(result.Blacklists) == 0 || result.Blacklists[len(result.Blacklists)-1] != bl {
			result.Clean = append(result.Clean, bl)
		}
	}

	// Persist blacklist results
	if h.db != nil {
		blacklistJSON, _ := json.Marshal(result.Blacklists)
		status := "active"
		if result.Listed {
			status = "blacklisted"
		}
		_, _ = h.db.ExecContext(ctx, `
			UPDATE mailing_ip_addresses
			SET blacklisted_on = $1,
			    last_blacklist_check = NOW(),
			    status = CASE WHEN $2 = 'blacklisted' AND status != 'retired' THEN 'blacklisted' ELSE status END,
			    updated_at = NOW()
			WHERE ip_address = $3::inet
		`, blacklistJSON, status, ip)
	}

	return result, nil
}

// RunHealthChecks performs DNS and blacklist checks for all active IPs.
func (h *HealthChecker) RunHealthChecks(ctx context.Context) (int, int, error) {
	if h.db == nil {
		return 0, 0, fmt.Errorf("no database connection")
	}

	rows, err := h.db.QueryContext(ctx, `
		SELECT ip_address::text, hostname
		FROM mailing_ip_addresses
		WHERE status IN ('active', 'warmup', 'paused')
		ORDER BY last_blacklist_check ASC NULLS FIRST
	`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	checked := 0
	issues := 0

	for rows.Next() {
		var ip, hostname string
		if err := rows.Scan(&ip, &hostname); err != nil {
			continue
		}

		dnsResult, err := h.CheckDNS(ctx, ip, hostname)
		if err != nil {
			log.Printf("[PMTAHealth] DNS check failed for %s: %v", ip, err)
			issues++
		} else if !dnsResult.ForwardMatch || !dnsResult.ReverseMatch {
			log.Printf("[PMTAHealth] DNS mismatch for %s: forward=%v reverse=%v", ip, dnsResult.ForwardMatch, dnsResult.ReverseMatch)
			issues++
		}

		blResult, err := h.CheckBlacklists(ctx, ip)
		if err != nil {
			log.Printf("[PMTAHealth] Blacklist check failed for %s: %v", ip, err)
		} else if blResult.Listed {
			log.Printf("[PMTAHealth] IP %s listed on: %v", ip, blResult.Blacklists)
			issues++
		}

		checked++
	}

	log.Printf("[PMTAHealth] Health check complete: %d IPs checked, %d issues found", checked, issues)
	return checked, issues, nil
}
