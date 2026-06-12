package api

// domain_dns_health.go — Domain Center DNS / reputation health checks.
//
// GET /api/mailing/domain-center/dns-health?domain=em.discountblog.com[&ips=1.2.3.4,5.6.7.8]
//
// Performs live server-side DNS lookups (SPF / DMARC / DKIM / MX / NS / A) plus
// Spamhaus DBL + open DNSBL checks (zen.spamhaus.org, bl.spamcop.net,
// b.barracudacentral.org) for the domain's sending IPs. Sending IPs resolve from
// mailing_sending_profiles → mailing_ip_pools → mailing_ip_addresses; the ?ips=
// param and the domain's A record act as fallbacks.
//
// IMPORTANT: Spamhaus answers 127.255.255.x to blocked public/open resolvers —
// those are reported as "unverifiable", never "listed". NXDOMAIN = clean.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

type dnsSPFResult struct {
	Status string   `json:"status"` // pass | warn | fail | missing
	Record string   `json:"record,omitempty"`
	Issues []string `json:"issues,omitempty"`
}

type dnsDMARCResult struct {
	Status string   `json:"status"` // pass | warn | fail | missing
	Record string   `json:"record,omitempty"`
	Policy string   `json:"policy,omitempty"`
	Issues []string `json:"issues,omitempty"`
}

type dnsDKIMResult struct {
	SelectorsFound []string `json:"selectors_found"`
	Status         string   `json:"status"` // found | unknown
	Note           string   `json:"note,omitempty"`
}

type dnsNSResult struct {
	Servers  []string `json:"servers"`
	Provider string   `json:"provider,omitempty"`
}

type dnsBlocklistResult struct {
	List   string `json:"list"`
	Target string `json:"target"`
	Status string `json:"status"` // clean | listed | unverifiable
	Detail string `json:"detail,omitempty"`
}

type dnsHealthResponse struct {
	Domain     string               `json:"domain"`
	Apex       string               `json:"apex"`
	CheckedAt  time.Time            `json:"checked_at"`
	SPF        dnsSPFResult         `json:"spf"`
	DMARC      dnsDMARCResult       `json:"dmarc"`
	DKIM       dnsDKIMResult        `json:"dkim"`
	MX         []string             `json:"mx"`
	NS         dnsNSResult          `json:"ns"`
	A          []string             `json:"a"`
	Blocklists []dnsBlocklistResult `json:"blocklists"`
	IPSource   string               `json:"ip_source,omitempty"` // db-pool | query-param | a-record | none
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// DomainDNSHealthHandler serves live DNS/reputation health checks for the
// Domain Center.
type DomainDNSHealthHandler struct {
	db *sql.DB
}

// NewDomainDNSHealthHandler creates the handler. db may be nil — sending-IP
// resolution then falls back to ?ips= / A records.
func NewDomainDNSHealthHandler(db *sql.DB) *DomainDNSHealthHandler {
	return &DomainDNSHealthHandler{db: db}
}

// dkimCommonSelectors are tried at <selector>._domainkey.<domain>.
var dkimCommonSelectors = []string{"default", "s1", "s2", "mail", "dkim", "k1", "selector1", "selector2", "pmta"}

// dnsblZones are the IP-based blocklists checked for every sending IP.
var dnsblZones = []string{"zen.spamhaus.org", "bl.spamcop.net", "b.barracudacentral.org"}

// Handle is the GET /domain-center/dns-health endpoint.
func (h *DomainDNSHealthHandler) Handle(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(r.URL.Query().Get("domain"), ".")))
	if domain == "" || !strings.Contains(domain, ".") || strings.ContainsAny(domain, " /\\") {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "valid ?domain= is required"})
		return
	}

	apex := apexOf(domain)

	// Total budget ~8s for the whole fan-out.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	resp := dnsHealthResponse{
		Domain:    domain,
		Apex:      apex,
		CheckedAt: time.Now().UTC(),
		MX:        []string{},
		A:         []string{},
	}

	// Resolve sending IPs up front (DB → ?ips= → A record fallback). The DB
	// query is fast; A-record fallback shares the lookup performed below.
	sendingIPs, ipSource := h.resolveSendingIPs(ctx, r, domain)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}

	// --- SPF -------------------------------------------------------------
	run(func() {
		res := checkSPF(ctx, domain)
		mu.Lock()
		resp.SPF = res
		mu.Unlock()
	})

	// --- DMARC (organizational/apex domain) -------------------------------
	run(func() {
		res := checkDMARC(ctx, apex)
		mu.Lock()
		resp.DMARC = res
		mu.Unlock()
	})

	// --- DKIM common selectors --------------------------------------------
	run(func() {
		res := checkDKIM(ctx, domain)
		mu.Lock()
		resp.DKIM = res
		mu.Unlock()
	})

	// --- MX ----------------------------------------------------------------
	run(func() {
		lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
		defer lcancel()
		mxs, err := net.DefaultResolver.LookupMX(lctx, domain)
		if err != nil {
			return
		}
		var out []string
		for _, mx := range mxs {
			out = append(out, fmt.Sprintf("%s (pref %d)", strings.TrimSuffix(mx.Host, "."), mx.Pref))
		}
		sort.Strings(out)
		mu.Lock()
		resp.MX = out
		mu.Unlock()
	})

	// --- NS on the apex + provider inference --------------------------------
	run(func() {
		lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
		defer lcancel()
		nss, err := net.DefaultResolver.LookupNS(lctx, apex)
		if err != nil {
			return
		}
		var servers []string
		for _, ns := range nss {
			servers = append(servers, strings.TrimSuffix(strings.ToLower(ns.Host), "."))
		}
		sort.Strings(servers)
		mu.Lock()
		resp.NS = dnsNSResult{Servers: servers, Provider: inferDNSProvider(servers)}
		mu.Unlock()
	})

	// --- A record of the sending domain (tracking-host sanity) --------------
	aRecordCh := make(chan []string, 1)
	run(func() {
		lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
		defer lcancel()
		var v4 []string
		if addrs, err := net.DefaultResolver.LookupIP(lctx, "ip4", domain); err == nil {
			for _, a := range addrs {
				v4 = append(v4, a.String())
			}
			sort.Strings(v4)
		}
		aRecordCh <- v4
		mu.Lock()
		resp.A = append([]string{}, v4...)
		mu.Unlock()
	})

	// --- Spamhaus DBL for the domain itself ----------------------------------
	blResults := []dnsBlocklistResult{}
	run(func() {
		status, detail := queryDNSBL(ctx, domain+".dbl.spamhaus.org")
		mu.Lock()
		blResults = append(blResults, dnsBlocklistResult{
			List: "dbl.spamhaus.org", Target: domain, Status: status, Detail: detail,
		})
		mu.Unlock()
	})

	// --- IP blocklists --------------------------------------------------------
	// If no IPs came from DB/param, wait for the A record as a last resort.
	run(func() {
		ips := sendingIPs
		src := ipSource
		if len(ips) == 0 {
			select {
			case v4 := <-aRecordCh:
				if len(v4) > 0 {
					ips, src = v4, "a-record"
				}
			case <-ctx.Done():
			}
		}
		if len(ips) > 16 {
			ips = ips[:16] // stay inside the time budget on big pools
		}
		mu.Lock()
		resp.IPSource = src
		mu.Unlock()

		var ipWG sync.WaitGroup
		for _, ip := range ips {
			rev := reverseIPv4(ip)
			if rev == "" {
				continue // skip non-IPv4 targets
			}
			for _, zone := range dnsblZones {
				ipWG.Add(1)
				go func(ip, zone, rev string) {
					defer ipWG.Done()
					status, detail := queryDNSBL(ctx, rev+"."+zone)
					mu.Lock()
					blResults = append(blResults, dnsBlocklistResult{
						List: zone, Target: ip, Status: status, Detail: detail,
					})
					mu.Unlock()
				}(ip, zone, rev)
			}
		}
		ipWG.Wait()
	})

	wg.Wait()

	sort.Slice(blResults, func(i, j int) bool {
		if blResults[i].Target != blResults[j].Target {
			return blResults[i].Target < blResults[j].Target
		}
		return blResults[i].List < blResults[j].List
	})
	resp.Blocklists = blResults

	respondJSON(w, http.StatusOK, resp)
}

// resolveSendingIPs returns the sending IPs for the domain and where they came
// from: explicit ?ips= param wins, then the DB pool join, else empty (the
// caller falls back to the A record).
func (h *DomainDNSHealthHandler) resolveSendingIPs(ctx context.Context, r *http.Request, domain string) ([]string, string) {
	if raw := strings.TrimSpace(r.URL.Query().Get("ips")); raw != "" {
		var ips []string
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if net.ParseIP(p) != nil {
				ips = append(ips, p)
			}
		}
		if len(ips) > 0 {
			return ips, "query-param"
		}
	}

	if h.db == nil {
		return nil, "none"
	}

	orgID := getOrgID(r)
	var poolPrefix, ipPool string
	err := h.db.QueryRowContext(ctx, `
		SELECT COALESCE(pool_prefix, ''), COALESCE(ip_pool, '')
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND sending_domain = $2 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`, orgID, domain).Scan(&poolPrefix, &ipPool)
	if err != nil {
		return nil, "none"
	}

	var rows *sql.Rows
	switch {
	case strings.TrimSpace(poolPrefix) != "":
		// ISP-dedicated pools: prefix "db" → db-gmail-pool, db-yahoo-pool, …
		rows, err = h.db.QueryContext(ctx, `
			SELECT DISTINCT ip.ip_address::text
			FROM mailing_ip_addresses ip
			JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
			WHERE pool.name LIKE $1 || '-%'
			  AND ip.status IN ('active', 'warmup')
			  AND pool.status = 'active'
			ORDER BY 1
		`, strings.TrimSpace(poolPrefix))
	case strings.TrimSpace(ipPool) != "":
		rows, err = h.db.QueryContext(ctx, `
			SELECT DISTINCT ip.ip_address::text
			FROM mailing_ip_addresses ip
			JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
			WHERE pool.name = $1
			  AND ip.status IN ('active', 'warmup')
			  AND pool.status = 'active'
			ORDER BY 1
		`, strings.TrimSpace(ipPool))
	default:
		return nil, "none"
	}
	if err != nil {
		return nil, "none"
	}
	defer rows.Close()

	var ips []string
	for rows.Next() {
		var ip string
		if rows.Scan(&ip) == nil {
			// ip_address may carry a CIDR suffix depending on the column type.
			ip = strings.SplitN(ip, "/", 2)[0]
			if net.ParseIP(ip) != nil {
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) == 0 {
		return nil, "none"
	}
	return ips, "db-pool"
}

// ---------------------------------------------------------------------------
// Individual checks
// ---------------------------------------------------------------------------

func checkSPF(ctx context.Context, domain string) dnsSPFResult {
	lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
	defer lcancel()

	txts, err := net.DefaultResolver.LookupTXT(lctx, domain)
	if err != nil && !isDNSNotFound(err) {
		return dnsSPFResult{Status: "fail", Issues: []string{"TXT lookup error: " + err.Error()}}
	}

	var spfRecords []string
	for _, t := range txts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=spf1") {
			spfRecords = append(spfRecords, strings.TrimSpace(t))
		}
	}

	if len(spfRecords) == 0 {
		return dnsSPFResult{Status: "missing", Issues: []string{"no v=spf1 record found on " + domain}}
	}
	if len(spfRecords) > 1 {
		return dnsSPFResult{
			Status: "fail",
			Record: strings.Join(spfRecords, "  ||  "),
			Issues: []string{fmt.Sprintf("multiple SPF records (%d) — receivers treat this as a permanent error (RFC 7208 §4.5)", len(spfRecords))},
		}
	}

	record := spfRecords[0]
	res := dnsSPFResult{Status: "pass", Record: record}

	lookups := 0
	sawAll := false
	for _, term := range strings.Fields(record) {
		lt := strings.ToLower(term)
		if lt == "v=spf1" {
			continue
		}
		// Strip qualifier for mechanism matching.
		mech := strings.TrimLeft(lt, "+-~?")
		switch {
		case mech == "all":
			sawAll = true
			if strings.HasPrefix(lt, "+all") || lt == "all" {
				res.Status = "fail"
				res.Issues = append(res.Issues, "CRITICAL: '+all' authorizes the entire internet to send as this domain")
			}
		case strings.HasPrefix(mech, "include:"),
			strings.HasPrefix(mech, "exists:"),
			strings.HasPrefix(mech, "redirect="),
			mech == "a", strings.HasPrefix(mech, "a:"), strings.HasPrefix(mech, "a/"),
			mech == "mx", strings.HasPrefix(mech, "mx:"), strings.HasPrefix(mech, "mx/"),
			mech == "ptr", strings.HasPrefix(mech, "ptr:"):
			lookups++
		}
	}
	if lookups > 10 {
		res.Issues = append(res.Issues, fmt.Sprintf("%d DNS-lookup mechanisms — exceeds the RFC 7208 limit of 10 (SPF will permerror)", lookups))
		if res.Status == "pass" {
			res.Status = "warn"
		}
	}
	if !sawAll && !strings.Contains(strings.ToLower(record), "redirect=") {
		res.Issues = append(res.Issues, "no terminal 'all' mechanism (defaults to neutral)")
		if res.Status == "pass" {
			res.Status = "warn"
		}
	}
	return res
}

func checkDMARC(ctx context.Context, apex string) dnsDMARCResult {
	lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
	defer lcancel()

	txts, err := net.DefaultResolver.LookupTXT(lctx, "_dmarc."+apex)
	if err != nil && !isDNSNotFound(err) {
		return dnsDMARCResult{Status: "fail", Issues: []string{"TXT lookup error on _dmarc." + apex + ": " + err.Error()}}
	}

	var records []string
	for _, t := range txts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=dmarc1") {
			records = append(records, strings.TrimSpace(t))
		}
	}
	if len(records) == 0 {
		return dnsDMARCResult{Status: "missing", Issues: []string{"no v=DMARC1 record at _dmarc." + apex}}
	}
	if len(records) > 1 {
		return dnsDMARCResult{
			Status: "fail",
			Record: strings.Join(records, "  ||  "),
			Issues: []string{fmt.Sprintf("multiple DMARC records (%d) — receivers ignore all of them", len(records))},
		}
	}

	record := records[0]
	res := dnsDMARCResult{Status: "pass", Record: record}

	tags := map[string]string{}
	for _, part := range strings.Split(record, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			tags[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.TrimSpace(kv[1])
		}
	}

	res.Policy = strings.ToLower(tags["p"])
	switch res.Policy {
	case "":
		res.Status = "fail"
		res.Issues = append(res.Issues, "no p= policy tag — record is invalid")
	case "none":
		res.Status = "warn"
		res.Issues = append(res.Issues, "p=none — monitoring only, no enforcement (consider quarantine/reject once aligned)")
	}
	if tags["rua"] == "" {
		res.Issues = append(res.Issues, "no rua= aggregate-report address — you receive no DMARC feedback")
		if res.Status == "pass" {
			res.Status = "warn"
		}
	}
	return res
}

func checkDKIM(ctx context.Context, domain string) dnsDKIMResult {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		found []string
	)
	for _, sel := range dkimCommonSelectors {
		wg.Add(1)
		go func(sel string) {
			defer wg.Done()
			lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
			defer lcancel()
			txts, err := net.DefaultResolver.LookupTXT(lctx, sel+"._domainkey."+domain)
			if err != nil {
				return
			}
			for _, t := range txts {
				lt := strings.ToLower(t)
				if strings.Contains(lt, "v=dkim1") || strings.Contains(lt, "p=") {
					mu.Lock()
					found = append(found, sel)
					mu.Unlock()
					return
				}
			}
		}(sel)
	}
	wg.Wait()
	sort.Strings(found)

	if len(found) == 0 {
		return dnsDKIMResult{
			SelectorsFound: []string{},
			Status:         "unknown",
			Note:           "no common selector found; signing may use a custom selector",
		}
	}
	return dnsDKIMResult{SelectorsFound: found, Status: "found"}
}

// queryDNSBL resolves a DNSBL query name and classifies the answer.
// NXDOMAIN = clean; 127.255.255.x = Spamhaus open-resolver/error code
// (unverifiable via this resolver); other 127.x answers = listed.
func queryDNSBL(ctx context.Context, query string) (status, detail string) {
	lctx, lcancel := context.WithTimeout(ctx, 5*time.Second)
	defer lcancel()

	addrs, err := net.DefaultResolver.LookupIP(lctx, "ip4", query)
	if err != nil {
		if isDNSNotFound(err) {
			return "clean", ""
		}
		return "unverifiable", "lookup error: " + err.Error()
	}

	var codes []string
	for _, a := range addrs {
		codes = append(codes, a.String())
	}
	sort.Strings(codes)
	joined := strings.Join(codes, ", ")

	for _, c := range codes {
		if strings.HasPrefix(c, "127.255.255.") {
			return "unverifiable", "blocklist returned " + c + " — public/open resolver blocked by Spamhaus; query via a dedicated resolver to verify"
		}
	}
	for _, c := range codes {
		if strings.HasPrefix(c, "127.") {
			return "listed", "return code(s): " + joined
		}
	}
	return "unverifiable", "unexpected answer: " + joined
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// apexOf strips a well-known sending-subdomain label (em., m., mail., mta.)
// when the remainder is still a registrable domain. em.discountblog.com →
// discountblog.com; discountblog.com → discountblog.com.
func apexOf(domain string) string {
	labels := strings.Split(domain, ".")
	if len(labels) >= 3 {
		switch labels[0] {
		case "em", "m", "mail", "mta", "track", "link":
			return strings.Join(labels[1:], ".")
		}
	}
	return domain
}

// inferDNSProvider maps well-known nameserver hostnames to a DNS host name.
func inferDNSProvider(servers []string) string {
	patterns := []struct{ substr, provider string }{
		{"awsdns", "Amazon Route 53"},
		{"cloudflare.com", "Cloudflare"},
		{"domaincontrol.com", "GoDaddy"},
		{"registrar-servers.com", "Namecheap"},
		{"dnsimple", "DNSimple"},
		{"googledomains.com", "Google"},
		{"google.com", "Google"},
		{"porkbun.com", "Porkbun"},
		{"azure-dns", "Azure DNS"},
		{"nsone.net", "NS1"},
		{"ultradns", "UltraDNS"},
		{"dnsmadeeasy.com", "DNS Made Easy"},
		{"digitalocean.com", "DigitalOcean"},
		{"ovh.net", "OVH"},
		{"gandi.net", "Gandi"},
	}
	for _, s := range servers {
		for _, p := range patterns {
			if strings.Contains(s, p.substr) {
				return p.provider
			}
		}
	}
	return ""
}

// reverseIPv4 turns 1.2.3.4 into 4.3.2.1 for DNSBL queries; "" for non-IPv4.
func reverseIPv4(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	v4 := parsed.To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", v4[3], v4[2], v4[1], v4[0])
}

// isDNSNotFound reports whether err is an NXDOMAIN / no-such-host DNS error.
func isDNSNotFound(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}
