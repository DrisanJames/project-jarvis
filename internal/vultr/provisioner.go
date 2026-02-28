package vultr

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// GenerateCloudInit produces a cloud-init user-data script that runs on first boot.
// It installs PMTA dependencies, binds IPs, configures BIRD for BGP, and installs PMTA.
func GenerateCloudInit(cfg ProvisionConfig) string {
	var sb strings.Builder

	sb.WriteString("#!/bin/bash\nset -euo pipefail\n\n")
	sb.WriteString("exec > /var/log/pmta-provision.log 2>&1\n")
	sb.WriteString("echo '=== PMTA Server Provisioning ==='\n")
	sb.WriteString("echo \"Started: $(date)\"\n\n")

	// System updates
	sb.WriteString("# --- System Setup ---\n")
	sb.WriteString("dnf update -y\n")
	sb.WriteString("dnf install -y epel-release\n")
	sb.WriteString("dnf install -y bird2 bind-utils net-tools wget curl jq\n\n")

	// Bind IPs to loopback (for BGP announcement)
	if cfg.SubnetBlock != "" && len(cfg.IPs) > 0 {
		sb.WriteString("# --- Bind IPs to Dummy Interface ---\n")
		sb.WriteString("ip link add dev dummy0 type dummy 2>/dev/null || true\n")
		sb.WriteString("ip link set dev dummy0 up\n\n")

		for _, ip := range cfg.IPs {
			sb.WriteString(fmt.Sprintf("ip addr add %s/32 dev dummy0 2>/dev/null || true\n", ip))
		}
		sb.WriteString("\n")

		// Persist via networkd config
		sb.WriteString("# --- Persist IP bindings ---\n")
		sb.WriteString("cat > /etc/sysconfig/network-scripts/ifcfg-dummy0 << 'NETEOF'\n")
		sb.WriteString("DEVICE=dummy0\n")
		sb.WriteString("TYPE=dummy\n")
		sb.WriteString("ONBOOT=yes\n")
		sb.WriteString("NETEOF\n\n")
	}

	// BIRD2 BGP configuration
	if cfg.BGPEnabled && cfg.BGPASN > 0 {
		sb.WriteString("# --- BIRD2 BGP Configuration ---\n")
		sb.WriteString("cat > /etc/bird.conf << 'BIRDEOF'\n")
		sb.WriteString(generateBIRDConfig(cfg))
		sb.WriteString("BIRDEOF\n\n")
		sb.WriteString("systemctl enable bird\n")
		sb.WriteString("systemctl start bird\n\n")
	}

	// PMTA installation
	if cfg.InstallPMTA && cfg.PMTARPMPath != "" {
		sb.WriteString("# --- PowerMTA Installation ---\n")
		sb.WriteString(fmt.Sprintf("rpm -ivh %s || echo 'PMTA install failed, RPM may need manual upload'\n", cfg.PMTARPMPath))
		sb.WriteString("mkdir -p /etc/pmta\n\n")

		// Base PMTA config
		sb.WriteString("cat > /etc/pmta/config << 'PMTAEOF'\n")
		sb.WriteString(generatePMTAConfig(cfg))
		sb.WriteString("PMTAEOF\n\n")

		sb.WriteString("systemctl enable pmta\n")
		sb.WriteString("systemctl start pmta || echo 'PMTA start deferred — config may need adjustment'\n\n")
	}

	// Firewall rules
	sb.WriteString("# --- Firewall ---\n")
	sb.WriteString("firewall-cmd --permanent --add-port=25/tcp || true\n")
	sb.WriteString("firewall-cmd --permanent --add-port=587/tcp || true\n")
	sb.WriteString("firewall-cmd --permanent --add-port=19000/tcp || true\n")
	sb.WriteString("firewall-cmd --reload || true\n\n")

	sb.WriteString("echo '=== Provisioning Complete ==='\n")
	sb.WriteString("echo \"Finished: $(date)\"\n")

	return sb.String()
}

// GenerateCloudInitBase64 returns the cloud-init script as base64 for the Vultr API user_data field.
func GenerateCloudInitBase64(cfg ProvisionConfig) string {
	return base64.StdEncoding.EncodeToString([]byte(GenerateCloudInit(cfg)))
}

// ProvisionConfig holds all parameters for server provisioning.
type ProvisionConfig struct {
	SubnetBlock  string
	IPs          []string
	BGPEnabled   bool
	BGPASN       int
	BGPPassword  string
	PeerIP       string // Vultr's BGP neighbor IP (from server's gateway)
	InstallPMTA  bool
	PMTARPMPath  string
	Hostname     string
	MgmtPort     int
	MgmtAPIKey   string
}

func generateBIRDConfig(cfg ProvisionConfig) string {
	var sb strings.Builder

	sb.WriteString("log syslog all;\n")
	sb.WriteString("router id from \"dummy0\";\n\n")

	sb.WriteString("protocol device {\n  scan time 5;\n}\n\n")

	sb.WriteString("protocol direct {\n")
	sb.WriteString("  interface \"dummy0\";\n")
	sb.WriteString("  ipv4 { table master4; import all; };\n")
	sb.WriteString("}\n\n")

	sb.WriteString("protocol static announced_v4 {\n")
	sb.WriteString("  ipv4 { table master4; };\n")
	if cfg.SubnetBlock != "" {
		sb.WriteString(fmt.Sprintf("  route %s blackhole;\n", cfg.SubnetBlock))
	}
	sb.WriteString("}\n\n")

	sb.WriteString("filter export_bgp {\n")
	sb.WriteString(fmt.Sprintf("  if net ~ [ %s ] then accept;\n", cfg.SubnetBlock))
	sb.WriteString("  reject;\n")
	sb.WriteString("}\n\n")

	sb.WriteString("protocol bgp vultr {\n")
	sb.WriteString(fmt.Sprintf("  local as %d;\n", cfg.BGPASN))
	sb.WriteString("  neighbor 169.254.169.254 as 64515;\n")
	sb.WriteString("  multihop 2;\n")
	if cfg.BGPPassword != "" {
		sb.WriteString(fmt.Sprintf("  password \"%s\";\n", cfg.BGPPassword))
	}
	sb.WriteString("  ipv4 {\n")
	sb.WriteString("    import none;\n")
	sb.WriteString("    export filter export_bgp;\n")
	sb.WriteString("  };\n")
	sb.WriteString("}\n")

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

	sb.WriteString(fmt.Sprintf("http-mgmt-port %d\n", mgmtPort))
	if cfg.MgmtAPIKey != "" {
		sb.WriteString(fmt.Sprintf("http-access %s monitor\n", cfg.MgmtAPIKey))
	}
	sb.WriteString("http-access 127.0.0.1 monitor\n\n")

	sb.WriteString("run-as-root no\n")
	sb.WriteString("min-free-disk-space 512M\n\n")

	// SMTP source hosts — one per IP
	sb.WriteString("# --- SMTP Sources (one virtual-mta per IP) ---\n")
	for i, ip := range cfg.IPs {
		vmtaName := fmt.Sprintf("mta%d", i+1)
		hostName := fmt.Sprintf("mta%d.mail.ignitemailing.com", i+1)
		sb.WriteString(fmt.Sprintf("\nsmtp-source-host %s %s\n", ip, hostName))
		sb.WriteString(fmt.Sprintf("<virtual-mta %s>\n", vmtaName))
		sb.WriteString(fmt.Sprintf("  smtp-source-host %s %s\n", ip, hostName))
		sb.WriteString(fmt.Sprintf("  <domain *>\n"))
		sb.WriteString(fmt.Sprintf("    max-msg-rate 100/h\n"))
		sb.WriteString(fmt.Sprintf("    max-rcpt-rate 100/h\n"))
		sb.WriteString(fmt.Sprintf("  </domain>\n"))
		sb.WriteString(fmt.Sprintf("</virtual-mta>\n"))
	}

	// Default pool with all VMTAs
	if len(cfg.IPs) > 0 {
		sb.WriteString("\n# --- Default Pool ---\n")
		sb.WriteString("<virtual-mta-pool default-pool>\n")
		for i := range cfg.IPs {
			sb.WriteString(fmt.Sprintf("  virtual-mta mta%d\n", i+1))
		}
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

	return sb.String()
}

// GenerateIPList expands a /24 block into individual host IPs (1-254).
func GenerateIPList(subnetBlock string) []string {
	parts := strings.Split(subnetBlock, "/")
	if len(parts) != 2 {
		return nil
	}
	octets := strings.Split(parts[0], ".")
	if len(octets) != 4 {
		return nil
	}

	prefix := strings.Join(octets[:3], ".")
	var ips []string
	for i := 1; i <= 254; i++ {
		ips = append(ips, fmt.Sprintf("%s.%d", prefix, i))
	}
	return ips
}

// GenerateLOADocument produces a Letter of Authorization for Vultr BGP request.
func GenerateLOADocument(companyName, subnet, contactName, email, phone string) string {
	return fmt.Sprintf(`AUTHORIZATION LETTER

%s

To whom it may concern,

This letter serves as authorization for Vultr with AS20473 to announce the
following IP address blocks:

  %s

As a representative of the company %s that is the owner of the subnet
and/or ASN, I hereby declare that I'm authorized to represent and sign
for this LOA.

Should you have questions about this request, email me at %s,
or call: %s

From,

%s
%s
`, timeNowFormatted(), subnet, companyName, email, phone, contactName, companyName)
}

func timeNowFormatted() string {
	return time.Now().Format("January 2, 2006")
}
