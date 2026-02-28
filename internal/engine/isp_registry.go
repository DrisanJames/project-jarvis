package engine

import (
	"net"
	"strings"
	"sync"
	"time"
)

// ISPRegistry maps recipient domains to ISP groups using static domain lists
// and dynamic MX-based resolution with caching.
type ISPRegistry struct {
	staticMap map[string]ISP
	mxCache   sync.Map // domain -> cacheEntry
	cacheTTL  time.Duration
}

type cacheEntry struct {
	isp       ISP
	expiresAt time.Time
}

// NewISPRegistry creates a registry with all 8 ISP groups mapped.
func NewISPRegistry() *ISPRegistry {
	r := &ISPRegistry{
		staticMap: make(map[string]ISP),
		cacheTTL:  1 * time.Hour,
	}
	r.seedStaticMap()
	return r
}

func (r *ISPRegistry) seedStaticMap() {
	gmail := []string{"gmail.com", "googlemail.com", "google.com"}
	yahoo := []string{
		"yahoo.com", "yahoo.co.uk", "yahoo.co.jp", "yahoo.co.in", "yahoo.ca",
		"yahoo.com.au", "yahoo.com.br", "yahoo.fr", "yahoo.de", "yahoo.it",
		"ymail.com", "aol.com", "aim.com", "verizon.net", "frontier.com", "rogers.com",
	}
	microsoft := []string{"outlook.com", "hotmail.com", "live.com", "msn.com", "passport.com"}
	apple := []string{"icloud.com", "me.com", "mac.com"}
	comcast := []string{"comcast.net", "xfinity.com"}
	att := []string{
		"att.net", "sbcglobal.net", "pacbell.net", "bellsouth.net",
		"ameritech.net", "nvbell.net", "prodigy.net",
	}
	cox := []string{"cox.net"}
	charter := []string{"charter.net", "spectrum.net", "rr.com", "twc.com", "brighthouse.com"}

	for _, d := range gmail {
		r.staticMap[d] = ISPGmail
	}
	for _, d := range yahoo {
		r.staticMap[d] = ISPYahoo
	}
	for _, d := range microsoft {
		r.staticMap[d] = ISPMicrosoft
	}
	for _, d := range apple {
		r.staticMap[d] = ISPApple
	}
	for _, d := range comcast {
		r.staticMap[d] = ISPComcast
	}
	for _, d := range att {
		r.staticMap[d] = ISPAtt
	}
	for _, d := range cox {
		r.staticMap[d] = ISPCox
	}
	for _, d := range charter {
		r.staticMap[d] = ISPCharter
	}
}

// ClassifyDomain returns the ISP group for a recipient domain.
// Checks static map first, then falls back to MX-based resolution with caching.
func (r *ISPRegistry) ClassifyDomain(domain string) ISP {
	domain = strings.ToLower(strings.TrimSpace(domain))

	if isp, ok := r.staticMap[domain]; ok {
		return isp
	}

	if entry, ok := r.mxCache.Load(domain); ok {
		ce := entry.(cacheEntry)
		if time.Now().Before(ce.expiresAt) {
			return ce.isp
		}
		r.mxCache.Delete(domain)
	}

	isp := r.resolveMX(domain)
	r.mxCache.Store(domain, cacheEntry{isp: isp, expiresAt: time.Now().Add(r.cacheTTL)})
	return isp
}

// ClassifyEmail extracts the domain from an email and classifies it.
func (r *ISPRegistry) ClassifyEmail(email string) ISP {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	return r.ClassifyDomain(parts[1])
}

// DomainsForISP returns all static domains assigned to an ISP.
func (r *ISPRegistry) DomainsForISP(isp ISP) []string {
	var domains []string
	for d, i := range r.staticMap {
		if i == isp {
			domains = append(domains, d)
		}
	}
	return domains
}

// mxPatterns maps MX hostname suffixes to ISP groups.
var mxPatterns = map[string]ISP{
	"google.com":                ISPGmail,
	"googlemail.com":            ISPGmail,
	"yahoodns.net":              ISPYahoo,
	"protection.outlook.com":    ISPMicrosoft,
	"olc.protection.outlook.com": ISPMicrosoft,
	"icloud.com":                ISPApple,
	"comcast.net":               ISPComcast,
	"att.net":                   ISPAtt,
	"cox.net":                   ISPCox,
	"charter.net":               ISPCharter,
}

func (r *ISPRegistry) resolveMX(domain string) ISP {
	records, err := net.LookupMX(domain)
	if err != nil || len(records) == 0 {
		return ""
	}

	for _, mx := range records {
		host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		for suffix, isp := range mxPatterns {
			if strings.HasSuffix(host, suffix) {
				return isp
			}
		}
	}
	return ""
}

// PoolNameForISP returns the PMTA pool name for an ISP.
func PoolNameForISP(isp ISP) string {
	if isp == "" {
		return ""
	}
	return string(isp) + "-pool"
}

// ISPDisplayName returns a human-readable name for an ISP.
func ISPDisplayName(isp ISP) string {
	names := map[ISP]string{
		ISPGmail: "Gmail", ISPYahoo: "Yahoo", ISPMicrosoft: "Microsoft",
		ISPApple: "Apple iCloud", ISPComcast: "Comcast", ISPAtt: "AT&T",
		ISPCox: "Cox", ISPCharter: "Charter/Spectrum",
	}
	if n, ok := names[isp]; ok {
		return n
	}
	return string(isp)
}

// AllPoolNames returns all 10 pool names (8 ISP + warmup + quarantine).
func AllPoolNames() []string {
	pools := make([]string, 0, 10)
	for _, isp := range AllISPs() {
		pools = append(pools, PoolNameForISP(isp))
	}
	pools = append(pools, "warmup-pool", "quarantine-pool")
	return pools
}
