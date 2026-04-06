package engine

import (
	"net"
	"strings"
	"sync"
	"time"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
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

// NewISPRegistry creates a registry with all 10 ISP groups mapped.
// AOL is separate from Yahoo, SBC Global/BellSouth is separate from AT&T.
// See internal/pkg/isp/isp.go for rationale.
func NewISPRegistry() *ISPRegistry {
	r := &ISPRegistry{
		staticMap: make(map[string]ISP),
		cacheTTL:  1 * time.Hour,
	}
	r.seedStaticMap()
	return r
}

func (r *ISPRegistry) seedStaticMap() {
	for _, group := range []struct {
		domains []string
		isp     ISP
	}{
		{isppkg.DomainsForGroup(isppkg.Gmail), ISPGmail},
		{isppkg.DomainsForGroup(isppkg.Yahoo), ISPYahoo},
		{isppkg.DomainsForGroup(isppkg.Aol), ISPAol},
		{isppkg.DomainsForGroup(isppkg.Microsoft), ISPMicrosoft},
		{isppkg.DomainsForGroup(isppkg.Apple), ISPApple},
		{isppkg.DomainsForGroup(isppkg.Comcast), ISPComcast},
		{isppkg.DomainsForGroup(isppkg.ATT), ISPAtt},
		{isppkg.DomainsForGroup(isppkg.Sbcglobal), ISPSbcglobal},
		{isppkg.DomainsForGroup(isppkg.Cox), ISPCox},
		{isppkg.DomainsForGroup(isppkg.Charter), ISPCharter},
	} {
		for _, d := range group.domains {
			r.staticMap[d] = group.isp
		}
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
	"sbcglobal.net":             ISPSbcglobal,
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
		ISPGmail: "Gmail", ISPYahoo: "Yahoo", ISPAol: "AOL",
		ISPMicrosoft: "Microsoft", ISPApple: "Apple iCloud",
		ISPComcast: "Comcast", ISPAtt: "AT&T",
		ISPSbcglobal: "SBC Global/BellSouth",
		ISPCox: "Cox", ISPCharter: "Charter/Spectrum",
	}
	if n, ok := names[isp]; ok {
		return n
	}
	return string(isp)
}

// AllPoolNames returns all 12 pool names (10 ISP + warmup + quarantine).
func AllPoolNames() []string {
	pools := make([]string, 0, 12)
	for _, isp := range AllISPs() {
		pools = append(pools, PoolNameForISP(isp))
	}
	pools = append(pools, "warmup-pool", "quarantine-pool")
	return pools
}
