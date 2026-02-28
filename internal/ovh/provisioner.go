package ovh

import (
	"fmt"
	"strings"
)

// GenerateSetupScript produces a bash script to configure a deployed OVHCloud
// dedicated server for PowerMTA. Unlike Vultr, OVHCloud uses failover IPs
// bound directly to network interfaces — no BGP/BIRD required.
func GenerateSetupScript(cfg ProvisionConfig) string {
	var sb strings.Builder

	sb.WriteString("#!/bin/bash\nset -euo pipefail\n\n")
	sb.WriteString("exec > /var/log/pmta-provision.log 2>&1\n")
	sb.WriteString("echo '=== OVHCloud PMTA Server Setup ==='\n")
	sb.WriteString(fmt.Sprintf("echo \"Server: %s (%s)\"\n", cfg.Hostname, cfg.ServerIP))
	sb.WriteString("echo \"Started: $(date)\"\n\n")

	iface := cfg.Interface
	if iface == "" {
		iface = "eth0"
	}

	// System updates & dependencies
	sb.WriteString("# --- System Setup ---\n")
	sb.WriteString("export DEBIAN_FRONTEND=noninteractive\n\n")
	sb.WriteString("# Detect package manager\n")
	sb.WriteString("if command -v dnf &>/dev/null; then\n")
	sb.WriteString("  PKG=dnf\n")
	sb.WriteString("  dnf update -y\n")
	sb.WriteString("  dnf install -y epel-release\n")
	sb.WriteString("  dnf install -y bind-utils net-tools wget curl jq postfix-pcre\n")
	sb.WriteString("elif command -v apt-get &>/dev/null; then\n")
	sb.WriteString("  PKG=apt\n")
	sb.WriteString("  apt-get update -y\n")
	sb.WriteString("  apt-get install -y dnsutils net-tools wget curl jq\n")
	sb.WriteString("fi\n\n")

	// Hostname
	sb.WriteString("# --- Hostname ---\n")
	sb.WriteString(fmt.Sprintf("hostnamectl set-hostname %s\n", cfg.Hostname))
	sb.WriteString(fmt.Sprintf("echo '%s %s' >> /etc/hosts\n\n", cfg.ServerIP, cfg.Hostname))

	// Bind failover IPs to the network interface
	if len(cfg.FailoverIPs) > 0 {
		sb.WriteString("# --- Bind Failover IPs ---\n")
		sb.WriteString(fmt.Sprintf("# OVHCloud failover IPs are bound as virtual interfaces on %s\n", iface))
		sb.WriteString("# These persist across reboots via netplan or ifcfg files.\n\n")

		for i, ip := range cfg.FailoverIPs {
			alias := fmt.Sprintf("%s:%d", iface, i)
			sb.WriteString(fmt.Sprintf("ip addr add %s/32 dev %s label %s 2>/dev/null || true\n", ip, iface, alias))
		}
		sb.WriteString("\n")

		// Persist: Netplan (Ubuntu/Debian) or ifcfg (RHEL/Rocky)
		sb.WriteString("# --- Persist Failover IP Bindings ---\n")
		sb.WriteString("if [ -d /etc/netplan ]; then\n")
		sb.WriteString("  # Ubuntu/Debian: Add to netplan config\n")
		sb.WriteString(fmt.Sprintf("  cat > /etc/netplan/51-failover-ips.yaml << 'NETEOF'\n"))
		sb.WriteString("network:\n")
		sb.WriteString("  version: 2\n")
		sb.WriteString("  ethernets:\n")
		sb.WriteString(fmt.Sprintf("    %s:\n", iface))
		sb.WriteString("      addresses:\n")
		for _, ip := range cfg.FailoverIPs {
			sb.WriteString(fmt.Sprintf("        - %s/32\n", ip))
		}
		sb.WriteString("NETEOF\n")
		sb.WriteString("  netplan apply\n")
		sb.WriteString("else\n")
		sb.WriteString("  # RHEL/Rocky: Create ifcfg alias files\n")
		for i, ip := range cfg.FailoverIPs {
			alias := fmt.Sprintf("%s:%d", iface, i)
			sb.WriteString(fmt.Sprintf("  cat > /etc/sysconfig/network-scripts/ifcfg-%s << 'IFEOF'\n", alias))
			sb.WriteString(fmt.Sprintf("DEVICE=%s\n", alias))
			sb.WriteString(fmt.Sprintf("IPADDR=%s\n", ip))
			sb.WriteString("NETMASK=255.255.255.255\n")
			sb.WriteString("ONBOOT=yes\n")
			sb.WriteString("IFEOF\n")
		}
		sb.WriteString("fi\n\n")
	}

	// PowerMTA installation
	if cfg.InstallPMTA && cfg.PMTARPMPath != "" {
		sb.WriteString("# --- PowerMTA Installation ---\n")
		sb.WriteString("if [ \"$PKG\" = \"dnf\" ]; then\n")
		sb.WriteString(fmt.Sprintf("  rpm -ivh %s || echo 'PMTA RPM install failed — may need manual upload'\n", cfg.PMTARPMPath))
		sb.WriteString("else\n")
		pmtaDeb := strings.Replace(cfg.PMTARPMPath, ".rpm", ".deb", 1)
		sb.WriteString(fmt.Sprintf("  dpkg -i %s || apt-get install -f -y\n", pmtaDeb))
		sb.WriteString("fi\n")
		sb.WriteString("mkdir -p /etc/pmta\n\n")

		// PMTA config
		sb.WriteString("cat > /etc/pmta/config << 'PMTAEOF'\n")
		sb.WriteString(generatePMTAConfig(cfg))
		sb.WriteString("PMTAEOF\n\n")

		sb.WriteString("systemctl enable pmta\n")
		sb.WriteString("systemctl start pmta || echo 'PMTA start deferred — config may need adjustment'\n\n")
	}

	// Firewall
	sb.WriteString("# --- Firewall ---\n")
	sb.WriteString("if command -v firewall-cmd &>/dev/null; then\n")
	sb.WriteString("  firewall-cmd --permanent --add-port=25/tcp || true\n")
	sb.WriteString("  firewall-cmd --permanent --add-port=587/tcp || true\n")
	sb.WriteString("  firewall-cmd --permanent --add-port=19000/tcp || true\n")
	sb.WriteString("  firewall-cmd --reload || true\n")
	sb.WriteString("elif command -v ufw &>/dev/null; then\n")
	sb.WriteString("  ufw allow 25/tcp\n")
	sb.WriteString("  ufw allow 587/tcp\n")
	sb.WriteString("  ufw allow 19000/tcp\n")
	sb.WriteString("  ufw --force enable || true\n")
	sb.WriteString("fi\n\n")

	// Kernel tuning for high-volume mail
	sb.WriteString("# --- Kernel Tuning for Mail Delivery ---\n")
	sb.WriteString("cat >> /etc/sysctl.conf << 'SYSEOF'\n")
	sb.WriteString("net.ipv4.ip_local_port_range = 1024 65535\n")
	sb.WriteString("net.core.somaxconn = 4096\n")
	sb.WriteString("net.core.netdev_max_backlog = 5000\n")
	sb.WriteString("net.ipv4.tcp_max_syn_backlog = 4096\n")
	sb.WriteString("net.ipv4.tcp_tw_reuse = 1\n")
	sb.WriteString("net.ipv4.tcp_fin_timeout = 15\n")
	sb.WriteString("net.ipv4.tcp_keepalive_time = 300\n")
	sb.WriteString("net.ipv4.tcp_keepalive_intvl = 30\n")
	sb.WriteString("net.ipv4.tcp_keepalive_probes = 5\n")
	sb.WriteString("SYSEOF\n")
	sb.WriteString("sysctl -p\n\n")

	sb.WriteString("echo '=== OVHCloud PMTA Setup Complete ==='\n")
	sb.WriteString("echo \"Finished: $(date)\"\n")
	sb.WriteString("echo ''\n")
	sb.WriteString("echo 'Next steps:'\n")
	sb.WriteString("echo '  1. Set rDNS (PTR records) for each failover IP in OVH panel'\n")
	sb.WriteString("echo '  2. Configure forward DNS (A records) for each mta hostname'\n")
	sb.WriteString("echo '  3. Set up DKIM signing in /etc/pmta/config'\n")
	sb.WriteString("echo '  4. Upload PMTA license to /etc/pmta/license'\n")
	sb.WriteString("echo '  5. Create a sending profile in IGNITE pointing to this server'\n")

	return sb.String()
}

func generatePMTAConfig(cfg ProvisionConfig) string {
	var sb strings.Builder

	hostname := cfg.Hostname
	if hostname == "" {
		hostname = "pmta1.mail.ignitemailing.com"
	}
	mgmtPort := cfg.MgmtPort
	if mgmtPort == 0 {
		mgmtPort = 19000
	}

	sb.WriteString(fmt.Sprintf("postmaster postmaster@%s\n\n", hostname))

	// Management interface
	sb.WriteString(fmt.Sprintf("http-mgmt-port %d\n", mgmtPort))
	if cfg.MgmtAPIKey != "" {
		sb.WriteString(fmt.Sprintf("http-access %s monitor\n", cfg.MgmtAPIKey))
	}
	sb.WriteString("http-access 127.0.0.1 monitor\n\n")

	sb.WriteString("run-as-root no\n")
	sb.WriteString("min-free-disk-space 512M\n\n")

	// SMTP source on the primary server IP
	sb.WriteString("# --- Primary IP SMTP Source ---\n")
	sb.WriteString(fmt.Sprintf("smtp-source-host %s %s\n\n", cfg.ServerIP, hostname))

	// Virtual MTAs — one per failover IP
	sb.WriteString("# --- Virtual MTAs (one per failover IP) ---\n")
	for i, ip := range cfg.FailoverIPs {
		vmtaName := fmt.Sprintf("mta%d", i+1)
		mtaHostname := fmt.Sprintf("mta%d.%s", i+1, stripFirstLabel(hostname))

		sb.WriteString(fmt.Sprintf("\n<virtual-mta %s>\n", vmtaName))
		sb.WriteString(fmt.Sprintf("  smtp-source-host %s %s\n", ip, mtaHostname))
		sb.WriteString("  <domain *>\n")
		sb.WriteString("    use-starttls yes\n")
		sb.WriteString("    max-msg-rate 200/h\n")
		sb.WriteString("    max-rcpt-rate 200/h\n")
		sb.WriteString("    retry-after           15m\n")
		sb.WriteString("    max-retry-time         2d\n")
		sb.WriteString("    bounce-after           3d\n")
		sb.WriteString("  </domain>\n")

		// Gmail throttling
		sb.WriteString("  <domain gmail.com>\n")
		sb.WriteString("    max-msg-rate 50/h\n")
		sb.WriteString("    max-rcpt-rate 50/h\n")
		sb.WriteString("  </domain>\n")

		// Yahoo/AOL throttling
		sb.WriteString("  <domain yahoo.com>\n")
		sb.WriteString("    max-msg-rate 50/h\n")
		sb.WriteString("    max-rcpt-rate 50/h\n")
		sb.WriteString("  </domain>\n")

		// Microsoft throttling
		sb.WriteString("  <domain outlook.com>\n")
		sb.WriteString("    max-msg-rate 50/h\n")
		sb.WriteString("    max-rcpt-rate 50/h\n")
		sb.WriteString("  </domain>\n")
		sb.WriteString("  <domain hotmail.com>\n")
		sb.WriteString("    max-msg-rate 50/h\n")
		sb.WriteString("    max-rcpt-rate 50/h\n")
		sb.WriteString("  </domain>\n")

		sb.WriteString(fmt.Sprintf("</virtual-mta>\n"))
	}

	// Default pool with all VMTAs
	if len(cfg.FailoverIPs) > 0 {
		sb.WriteString("\n# --- Default Pool (all VMTAs) ---\n")
		sb.WriteString("<virtual-mta-pool default-pool>\n")
		for i := range cfg.FailoverIPs {
			sb.WriteString(fmt.Sprintf("  virtual-mta mta%d\n", i+1))
		}
		sb.WriteString("</virtual-mta-pool>\n")

		// Warmup pool (single IP for controlled warmup)
		sb.WriteString("\n# --- Warmup Pool (first IP only for controlled warmup) ---\n")
		sb.WriteString("<virtual-mta-pool warmup-pool>\n")
		sb.WriteString("  virtual-mta mta1\n")
		sb.WriteString("</virtual-mta-pool>\n")
	}

	// Accounting
	sb.WriteString("\n# --- Accounting ---\n")
	sb.WriteString("<acct-file /var/log/pmta/acct.csv>\n")
	sb.WriteString("  move-to /var/log/pmta/acct-archive\n")
	sb.WriteString("  move-interval 1h\n")
	sb.WriteString("  max-size 500M\n")
	sb.WriteString("  records d b f\n")
	sb.WriteString("</acct-file>\n")

	// Bounce processing
	sb.WriteString("\n# --- Bounce Processing ---\n")
	sb.WriteString("<acct-file /var/log/pmta/bounce.csv>\n")
	sb.WriteString("  move-to /var/log/pmta/bounce-archive\n")
	sb.WriteString("  move-interval 1h\n")
	sb.WriteString("  max-size 500M\n")
	sb.WriteString("  records b\n")
	sb.WriteString("</acct-file>\n")

	return sb.String()
}

// stripFirstLabel removes the first DNS label, e.g. "pmta1.mail.example.com" -> "mail.example.com"
func stripFirstLabel(hostname string) string {
	parts := strings.SplitN(hostname, ".", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return hostname
}

// GenerateRDNSCommands produces OVH API commands or panel instructions
// for setting PTR records on all failover IPs.
func GenerateRDNSCommands(cfg ProvisionConfig) []ReverseDNS {
	var records []ReverseDNS
	for i, ip := range cfg.FailoverIPs {
		hostname := fmt.Sprintf("mta%d.%s", i+1, stripFirstLabel(cfg.Hostname))
		records = append(records, ReverseDNS{
			IPReverse: ip,
			Reverse:   hostname,
		})
	}
	return records
}

// GenerateDNSRecords returns the A records that must be created in your
// DNS provider to match the rDNS/PTR records.
func GenerateDNSRecords(cfg ProvisionConfig) map[string]string {
	records := make(map[string]string)
	for i, ip := range cfg.FailoverIPs {
		hostname := fmt.Sprintf("mta%d.%s", i+1, stripFirstLabel(cfg.Hostname))
		records[hostname] = ip
	}
	return records
}
