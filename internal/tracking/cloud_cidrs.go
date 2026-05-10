package tracking

import (
	"net"
)

// CloudCIDRs is a curated list of CIDR ranges belonging to major cloud
// providers and known email security scanners. Populated 2026-05-09 from
// public CIDR lists. Intentionally conservative — only well-known ranges.
//
// Sources:
//   - AWS: ip-ranges.json sample (us-east-1, us-west-2 ec2 ranges)
//   - Microsoft Azure: ServiceTags AzureCloud
//   - GCP: cloud.google.com/compute/docs/faq#find_ip_range
//   - Microsoft Defender / SafeLinks: known outbound ranges
//   - Mimecast: published outbound IP list
//   - Proofpoint: published prefilter IPs
//
// This list is intentionally a STATIC SAMPLE and not an exhaustive scan
// of every cloud provider's public CIDR file. The classifier in
// click_classifier.go only consults these ranges when the user-agent is
// already suspicious (bare or empty Mozilla/5.0). False negatives are
// the desired posture — we never want to flag a real subscriber click
// as machine traffic just because they happen to be on a corporate VPN
// or a residential ISP that leases from a cloud provider.
var CloudCIDRs = []string{
	// AWS — sample of largest us-east-1 / us-west-2 EC2 blocks
	"3.0.0.0/9",
	"13.32.0.0/15",
	"18.116.0.0/14",
	"34.192.0.0/12",
	"44.192.0.0/11",
	"52.0.0.0/11",
	"54.144.0.0/12",
	// Azure
	"13.64.0.0/11",
	"13.96.0.0/13",
	"20.0.0.0/8",
	"40.64.0.0/10",
	"52.96.0.0/12",
	"104.40.0.0/13",
	// GCP
	"34.64.0.0/10",
	"35.184.0.0/13",
	"104.196.0.0/14",
	"146.148.0.0/17",
	// Microsoft 365 / Defender outbound
	"40.92.0.0/15",
	"40.107.0.0/16",
	"52.100.0.0/14",
	// Mimecast
	"207.211.31.0/24",
	"146.101.78.0/24",
	// Proofpoint
	"67.231.144.0/20",
	"148.163.128.0/19",
}

// parsedCloudCIDRs is the package-private cache of *net.IPNet values
// built once at init. Doing the parse lazily on every classifier call
// would burn CPU on the click ingest hot path; the cache makes
// IsCloudOrScannerIP an O(N) IP comparison with N ~= len(CloudCIDRs).
var parsedCloudCIDRs []*net.IPNet

func init() {
	for _, cidr := range CloudCIDRs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err == nil {
			parsedCloudCIDRs = append(parsedCloudCIDRs, ipnet)
		}
	}
}

// IsCloudOrScannerIP returns true if the given IP string is in any of
// the parsed cloud / scanner CIDR ranges. Returns false for invalid IPs
// (no panic on bad input — the click ingest path can pass arbitrary
// strings sourced from request headers).
func IsCloudOrScannerIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, ipnet := range parsedCloudCIDRs {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}
