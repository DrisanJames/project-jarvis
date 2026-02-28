package engine

import (
	"fmt"
	"strings"
)

// PoolConfig defines which IPs belong to each pool.
type PoolConfig struct {
	ISPPools    map[ISP][]string // ISP -> list of IPs
	WarmupIPs   []string
	QuarantineIPs []string
}

// GeneratePoolConfigs produces PMTA virtual-mta-pool blocks for all 10 pools.
// Each ISP pool includes domain-level overrides and a suppress-delivery directive.
func GeneratePoolConfigs(pool PoolConfig, ispConfigs []ISPConfig, suppressionDir string) string {
	var sb strings.Builder

	cfgMap := make(map[ISP]ISPConfig)
	for _, c := range ispConfigs {
		cfgMap[c.ISP] = c
	}

	// 8 ISP production pools
	for _, isp := range AllISPs() {
		ips := pool.ISPPools[isp]
		cfg, hasCfg := cfgMap[isp]

		sb.WriteString(fmt.Sprintf("\n<virtual-mta-pool %s>\n", PoolNameForISP(isp)))

		// Suppression file
		if suppressionDir != "" {
			sb.WriteString(fmt.Sprintf("    suppress-delivery %s/%s.txt\n", suppressionDir, isp))
		}

		// Assign VMTAs
		for _, ip := range ips {
			vmtaName := ipToVMTAName(ip)
			sb.WriteString(fmt.Sprintf("    virtual-mta %s\n", vmtaName))
		}

		// Domain-level overrides from ISP config
		if hasCfg {
			for _, domain := range cfg.DomainPatterns {
				sb.WriteString(fmt.Sprintf("    <domain %s>\n", domain))
				sb.WriteString(fmt.Sprintf("        max-smtp-out %d\n", cfg.MaxConnections))
				sb.WriteString(fmt.Sprintf("        max-msg-rate %d/h\n", cfg.MaxMsgRate))
				sb.WriteString(fmt.Sprintf("    </domain>\n"))
			}
		}

		sb.WriteString(fmt.Sprintf("</virtual-mta-pool>\n"))
	}

	// Warmup pool
	sb.WriteString("\n<virtual-mta-pool warmup-pool>\n")
	for _, ip := range pool.WarmupIPs {
		sb.WriteString(fmt.Sprintf("    virtual-mta %s\n", ipToVMTAName(ip)))
	}
	sb.WriteString("    <domain *>\n")
	sb.WriteString("        max-msg-rate 50/d\n")
	sb.WriteString("        max-smtp-out 2\n")
	sb.WriteString("    </domain>\n")
	sb.WriteString("</virtual-mta-pool>\n")

	// Quarantine pool
	sb.WriteString("\n<virtual-mta-pool quarantine-pool>\n")
	for _, ip := range pool.QuarantineIPs {
		sb.WriteString(fmt.Sprintf("    virtual-mta %s\n", ipToVMTAName(ip)))
	}
	sb.WriteString("    <domain *>\n")
	sb.WriteString("        max-msg-rate 0/h\n")
	sb.WriteString("        max-smtp-out 0\n")
	sb.WriteString("    </domain>\n")
	sb.WriteString("</virtual-mta-pool>\n")

	return sb.String()
}

// GenerateVMTABlocks produces PMTA virtual-mta blocks for all IPs.
func GenerateVMTABlocks(ips []string, hostname string) string {
	var sb strings.Builder
	for _, ip := range ips {
		vmtaName := ipToVMTAName(ip)
		host := fmt.Sprintf("%s.%s", vmtaName, hostname)
		sb.WriteString(fmt.Sprintf("\nsmtp-source-host %s %s\n", ip, host))
		sb.WriteString(fmt.Sprintf("<virtual-mta %s>\n", vmtaName))
		sb.WriteString(fmt.Sprintf("    smtp-source-host %s %s\n", ip, host))
		sb.WriteString(fmt.Sprintf("</virtual-mta>\n"))
	}
	return sb.String()
}

// GenerateAccountingWebhook produces the PMTA accounting pipe config
// that feeds records to the engine webhook.
func GenerateAccountingWebhook(webhookURL string) string {
	var sb strings.Builder
	sb.WriteString("\n# --- Engine Accounting Webhook ---\n")
	sb.WriteString(fmt.Sprintf("<acct-file |/usr/local/bin/pmta-webhook-relay %s>\n", webhookURL))
	sb.WriteString("    records d,b,t,tq,f\n")
	sb.WriteString("    record-fields d *\n")
	sb.WriteString("    record-fields b *\n")
	sb.WriteString("    record-fields t *\n")
	sb.WriteString("    record-fields tq *\n")
	sb.WriteString("    record-fields f *\n")
	sb.WriteString("</acct-file>\n")
	return sb.String()
}

// DefaultPoolDistribution allocates 254 IPs across 10 pools.
func DefaultPoolDistribution(ips []string) PoolConfig {
	pc := PoolConfig{
		ISPPools: make(map[ISP][]string),
	}

	// Distribution: Gmail 40, Yahoo 30, Microsoft 35, Apple 20, Comcast 15, ATT 15, Cox 10, Charter 10, Warmup 50, Quarantine 29
	allocations := []struct {
		isp   ISP
		count int
	}{
		{ISPGmail, 40},
		{ISPYahoo, 30},
		{ISPMicrosoft, 35},
		{ISPApple, 20},
		{ISPComcast, 15},
		{ISPAtt, 15},
		{ISPCox, 10},
		{ISPCharter, 10},
	}

	idx := 0
	for _, alloc := range allocations {
		end := idx + alloc.count
		if end > len(ips) {
			end = len(ips)
		}
		pc.ISPPools[alloc.isp] = ips[idx:end]
		idx = end
	}

	// Warmup pool: next 50
	warmupEnd := idx + 50
	if warmupEnd > len(ips) {
		warmupEnd = len(ips)
	}
	pc.WarmupIPs = ips[idx:warmupEnd]
	idx = warmupEnd

	// Quarantine: remainder
	if idx < len(ips) {
		pc.QuarantineIPs = ips[idx:]
	}

	return pc
}

func ipToVMTAName(ip string) string {
	return "vmta-" + strings.ReplaceAll(ip, ".", "-")
}
