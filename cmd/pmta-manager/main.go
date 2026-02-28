package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/ignite/sparkpost-monitor/internal/pmta"
)

const (
	pmtaConfigDir = "/etc/pmta/conf.d"
	pmtaDKIMDir   = "/etc/pmta/dkim-keys"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "ip":
		handleIP(os.Args[2:])
	case "domain":
		handleDomain(os.Args[2:])
	case "pool":
		handlePool(os.Args[2:])
	case "status":
		handleStatus()
	case "queues":
		handleQueues()
	case "reload":
		handleReload()
	case "warmup":
		handleWarmup(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`pmta-manager — PowerMTA IP and Domain Management Tool

Usage:
  pmta-manager <command> <subcommand> [flags]

Commands:
  ip add       --ip <addr> --hostname <name> [--pool <name>]    Register IP as VMTA
  ip remove    --ip <addr>                                       Remove VMTA for IP
  ip list                                                        List configured VMTAs
  ip check-dns --ip <addr>                                       Verify forward/reverse DNS

  domain add     --domain <name> --selector <sel> [--generate-dkim]   Add sending domain
  domain remove  --domain <name>                                       Remove domain config
  domain list                                                          List configured domains
  domain verify-dns --domain <name>                                    Verify DKIM DNS record

  pool create    --name <name> --ips <ip1,ip2,...>               Create VMTA pool
  pool add-ip    --pool <name> --ip <addr>                       Add IP to pool
  pool remove-ip --pool <name> --ip <addr>                       Remove IP from pool

  status                  Show PMTA server status
  queues                  Show queue status
  reload                  Reload PMTA configuration

  warmup start   --ip <addr> --schedule standard                 Start IP warmup schedule
  warmup status                                                  Show warmup progress

Environment:
  PMTA_MGMT_HOST    PMTA management API host (default: 127.0.0.1)
  PMTA_MGMT_PORT    PMTA management API port (default: 19000)
  PMTA_API_KEY      PMTA management API key`)
}

func getMgmtClient() *pmta.Client {
	host := envOrDefault("PMTA_MGMT_HOST", "127.0.0.1")
	port := envOrDefaultInt("PMTA_MGMT_PORT", 19000)
	key := os.Getenv("PMTA_API_KEY")
	return pmta.NewClient(host, port, key)
}

// =============================================================================
// IP COMMANDS
// =============================================================================

func handleIP(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pmta-manager ip <add|remove|list|check-dns>")
		os.Exit(1)
	}

	switch args[0] {
	case "add":
		ipAddr := flagValue(args, "--ip")
		hostname := flagValue(args, "--hostname")
		pool := flagValue(args, "--pool")
		if ipAddr == "" || hostname == "" {
			fatal("--ip and --hostname are required")
		}
		addIP(ipAddr, hostname, pool)

	case "remove":
		ipAddr := flagValue(args, "--ip")
		if ipAddr == "" {
			fatal("--ip is required")
		}
		removeIP(ipAddr)

	case "list":
		listIPs()

	case "check-dns":
		ipAddr := flagValue(args, "--ip")
		if ipAddr == "" {
			fatal("--ip is required")
		}
		checkDNS(ipAddr)
	}
}

func addIP(ipAddr, hostname, pool string) {
	vmtaName := hostname

	vmtaTmpl := `<virtual-mta {{.Name}}>
    smtp-source-host {{.IP}} {{.Hostname}}
    <domain *>
        dkim-sign yes
        max-smtp-out 20
    </domain>
</virtual-mta>
`

	t := template.Must(template.New("vmta").Parse(vmtaTmpl))
	data := map[string]string{"Name": vmtaName, "IP": ipAddr, "Hostname": hostname}

	// Append to vmtas.conf
	path := filepath.Join(pmtaConfigDir, "vmtas.conf")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fatal("Cannot write %s: %v", path, err)
	}
	defer f.Close()

	fmt.Fprintln(f)
	if err := t.Execute(f, data); err != nil {
		fatal("Template error: %v", err)
	}

	fmt.Printf("Added VMTA '%s' for IP %s -> %s\n", vmtaName, ipAddr, hostname)

	// If pool specified, add to pool config
	if pool != "" {
		addIPToPool(pool, vmtaName)
	}

	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

func removeIP(ipAddr string) {
	path := filepath.Join(pmtaConfigDir, "vmtas.conf")
	content, err := os.ReadFile(path)
	if err != nil {
		fatal("Cannot read %s: %v", path, err)
	}

	lines := strings.Split(string(content), "\n")
	var out []string
	skip := false
	removed := false

	for _, line := range lines {
		if strings.Contains(line, "smtp-source-host "+ipAddr+" ") {
			// Find the start of this VMTA block
			for len(out) > 0 && !strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "<virtual-mta") {
				out = out[:len(out)-1]
			}
			if len(out) > 0 {
				out = out[:len(out)-1] // remove the <virtual-mta line
			}
			skip = true
			removed = true
			continue
		}
		if skip {
			if strings.Contains(line, "</virtual-mta>") {
				skip = false
				continue
			}
			continue
		}
		out = append(out, line)
	}

	if !removed {
		fmt.Printf("No VMTA found for IP %s\n", ipAddr)
		return
	}

	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644); err != nil {
		fatal("Cannot write %s: %v", path, err)
	}

	fmt.Printf("Removed VMTA for IP %s\n", ipAddr)
	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

func listIPs() {
	path := filepath.Join(pmtaConfigDir, "vmtas.conf")
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("No VMTAs configured (vmtas.conf not found)")
		return
	}

	lines := strings.Split(string(content), "\n")
	fmt.Printf("%-30s %-18s %s\n", "VMTA NAME", "IP ADDRESS", "HOSTNAME")
	fmt.Println(strings.Repeat("-", 70))

	currentVMTA := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<virtual-mta ") {
			currentVMTA = strings.TrimSuffix(strings.TrimPrefix(trimmed, "<virtual-mta "), ">")
		}
		if strings.HasPrefix(trimmed, "smtp-source-host ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				fmt.Printf("%-30s %-18s %s\n", currentVMTA, parts[1], parts[2])
			}
		}
	}
}

func checkDNS(ipAddr string) {
	fmt.Printf("Checking DNS for %s...\n\n", ipAddr)

	// Reverse lookup
	names, err := net.LookupAddr(ipAddr)
	if err != nil {
		fmt.Printf("  Reverse DNS (PTR):  FAILED — %v\n", err)
	} else if len(names) > 0 {
		ptr := strings.TrimSuffix(names[0], ".")
		fmt.Printf("  Reverse DNS (PTR):  %s\n", ptr)

		// Forward lookup of the PTR result
		addrs, err := net.LookupHost(ptr)
		if err != nil {
			fmt.Printf("  Forward DNS (A):    FAILED — %v\n", err)
		} else {
			match := false
			for _, a := range addrs {
				if a == ipAddr {
					match = true
					break
				}
			}
			fmt.Printf("  Forward DNS (A):    %s\n", strings.Join(addrs, ", "))
			if match {
				fmt.Printf("\n  ✓ Forward/Reverse match: PASS\n")
			} else {
				fmt.Printf("\n  ✗ Forward/Reverse match: FAIL (PTR resolves to %s but A record points to %s)\n", ptr, strings.Join(addrs, ","))
			}
		}
	} else {
		fmt.Printf("  Reverse DNS (PTR):  No PTR record found\n")
	}
}

// =============================================================================
// DOMAIN COMMANDS
// =============================================================================

func handleDomain(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pmta-manager domain <add|remove|list|verify-dns>")
		os.Exit(1)
	}

	switch args[0] {
	case "add":
		domain := flagValue(args, "--domain")
		selector := flagValue(args, "--selector")
		if domain == "" {
			fatal("--domain is required")
		}
		if selector == "" {
			selector = "s1"
		}
		genDKIM := hasFlag(args, "--generate-dkim")
		addDomain(domain, selector, genDKIM)

	case "remove":
		domain := flagValue(args, "--domain")
		if domain == "" {
			fatal("--domain is required")
		}
		removeDomain(domain)

	case "list":
		listDomains()

	case "verify-dns":
		domain := flagValue(args, "--domain")
		if domain == "" {
			fatal("--domain is required")
		}
		verifyDomainDNS(domain)
	}
}

func addDomain(domain, selector string, generateDKIM bool) {
	if generateDKIM {
		fmt.Printf("Generating 2048-bit DKIM key for %s (selector: %s)...\n", domain, selector)

		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			fatal("Key generation failed: %v", err)
		}

		privPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})

		// Save private key
		os.MkdirAll(pmtaDKIMDir, 0700)
		keyPath := filepath.Join(pmtaDKIMDir, domain+".pem")
		if err := os.WriteFile(keyPath, privPEM, 0600); err != nil {
			fatal("Cannot write key file: %v", err)
		}
		fmt.Printf("  Private key saved: %s\n", keyPath)

		// Generate DNS TXT record value
		pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		pubB64 := base64.StdEncoding.EncodeToString(pubDER)
		fmt.Printf("\n  DNS TXT record to add:\n")
		fmt.Printf("    Name:  %s._domainkey.%s\n", selector, domain)
		fmt.Printf("    Value: v=DKIM1; k=rsa; p=%s\n\n", pubB64)
	}

	// Add domain-key entry to dkim.conf
	keyPath := filepath.Join(pmtaDKIMDir, domain+".pem")
	dkimPath := filepath.Join(pmtaConfigDir, "dkim.conf")
	f, err := os.OpenFile(dkimPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fatal("Cannot write %s: %v", dkimPath, err)
	}
	defer f.Close()

	fmt.Fprintf(f, "domain-key %s, %s, %s\n", selector, domain, keyPath)
	fmt.Printf("Added DKIM config for %s (selector: %s)\n", domain, selector)
	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

func removeDomain(domain string) {
	dkimPath := filepath.Join(pmtaConfigDir, "dkim.conf")
	content, err := os.ReadFile(dkimPath)
	if err != nil {
		fmt.Println("No DKIM config found")
		return
	}

	lines := strings.Split(string(content), "\n")
	var out []string
	removed := false
	for _, line := range lines {
		if strings.Contains(line, ", "+domain+",") {
			removed = true
			continue
		}
		out = append(out, line)
	}

	if !removed {
		fmt.Printf("No DKIM entry found for %s\n", domain)
		return
	}

	os.WriteFile(dkimPath, []byte(strings.Join(out, "\n")), 0644)
	fmt.Printf("Removed DKIM config for %s\n", domain)
	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

func listDomains() {
	dkimPath := filepath.Join(pmtaConfigDir, "dkim.conf")
	content, err := os.ReadFile(dkimPath)
	if err != nil {
		fmt.Println("No DKIM config found")
		return
	}

	fmt.Printf("%-15s %-30s %s\n", "SELECTOR", "DOMAIN", "KEY PATH")
	fmt.Println(strings.Repeat("-", 80))

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "domain-key ") {
			parts := strings.SplitN(strings.TrimPrefix(line, "domain-key "), ",", 3)
			if len(parts) == 3 {
				fmt.Printf("%-15s %-30s %s\n",
					strings.TrimSpace(parts[0]),
					strings.TrimSpace(parts[1]),
					strings.TrimSpace(parts[2]))
			}
		}
	}
}

func verifyDomainDNS(domain string) {
	dkimPath := filepath.Join(pmtaConfigDir, "dkim.conf")
	content, _ := os.ReadFile(dkimPath)

	for _, line := range strings.Split(string(content), "\n") {
		if !strings.Contains(line, ", "+domain+",") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(line), "domain-key "), ",", 3)
		if len(parts) < 1 {
			continue
		}
		selector := strings.TrimSpace(parts[0])
		record := fmt.Sprintf("%s._domainkey.%s", selector, domain)

		fmt.Printf("Checking %s...\n", record)
		txts, err := net.LookupTXT(record)
		if err != nil {
			fmt.Printf("  ✗ DNS lookup failed: %v\n", err)
		} else if len(txts) == 0 {
			fmt.Printf("  ✗ No TXT records found\n")
		} else {
			for _, txt := range txts {
				if strings.Contains(txt, "v=DKIM1") {
					fmt.Printf("  ✓ DKIM record found: %s\n", txt[:min(80, len(txt))]+"...")
				} else {
					fmt.Printf("  ? TXT record (not DKIM): %s\n", txt[:min(80, len(txt))])
				}
			}
		}
	}
}

// =============================================================================
// POOL COMMANDS
// =============================================================================

func handlePool(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pmta-manager pool <create|add-ip|remove-ip>")
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		name := flagValue(args, "--name")
		ips := flagValue(args, "--ips")
		if name == "" {
			fatal("--name is required")
		}
		createPool(name, ips)

	case "add-ip":
		pool := flagValue(args, "--pool")
		ip := flagValue(args, "--ip")
		if pool == "" || ip == "" {
			fatal("--pool and --ip are required")
		}
		addIPToPool(pool, ip)

	case "remove-ip":
		pool := flagValue(args, "--pool")
		ip := flagValue(args, "--ip")
		if pool == "" || ip == "" {
			fatal("--pool and --ip are required")
		}
		removeIPFromPool(pool, ip)
	}
}

func createPool(name, ips string) {
	poolPath := filepath.Join(pmtaConfigDir, "pools.conf")
	f, err := os.OpenFile(poolPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fatal("Cannot write %s: %v", poolPath, err)
	}
	defer f.Close()

	fmt.Fprintf(f, "\n<virtual-mta-pool %s>\n", name)
	if ips != "" {
		for _, ip := range strings.Split(ips, ",") {
			vmta := strings.TrimSpace(ip)
			fmt.Fprintf(f, "    virtual-mta %s\n", vmta)
		}
	}
	fmt.Fprintln(f, "</virtual-mta-pool>")

	fmt.Printf("Created pool '%s'\n", name)
	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

func addIPToPool(pool, vmta string) {
	poolPath := filepath.Join(pmtaConfigDir, "pools.conf")
	content, _ := os.ReadFile(poolPath)

	lines := strings.Split(string(content), "\n")
	var out []string
	added := false

	for _, line := range lines {
		out = append(out, line)
		if strings.Contains(line, "<virtual-mta-pool "+pool+">") {
			out = append(out, fmt.Sprintf("    virtual-mta %s", vmta))
			added = true
		}
	}

	if !added {
		fmt.Printf("Pool '%s' not found. Creating it.\n", pool)
		out = append(out, fmt.Sprintf("\n<virtual-mta-pool %s>", pool))
		out = append(out, fmt.Sprintf("    virtual-mta %s", vmta))
		out = append(out, "</virtual-mta-pool>")
	}

	os.WriteFile(poolPath, []byte(strings.Join(out, "\n")), 0644)
	fmt.Printf("Added %s to pool '%s'\n", vmta, pool)
	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

func removeIPFromPool(pool, vmta string) {
	poolPath := filepath.Join(pmtaConfigDir, "pools.conf")
	content, err := os.ReadFile(poolPath)
	if err != nil {
		fmt.Println("No pools config found")
		return
	}

	lines := strings.Split(string(content), "\n")
	var out []string
	inPool := false

	for _, line := range lines {
		if strings.Contains(line, "<virtual-mta-pool "+pool+">") {
			inPool = true
		}
		if inPool && strings.Contains(strings.TrimSpace(line), "virtual-mta "+vmta) {
			continue // skip this line
		}
		if strings.Contains(line, "</virtual-mta-pool>") {
			inPool = false
		}
		out = append(out, line)
	}

	os.WriteFile(poolPath, []byte(strings.Join(out, "\n")), 0644)
	fmt.Printf("Removed %s from pool '%s'\n", vmta, pool)
	fmt.Println("Run 'pmta-manager reload' to apply changes.")
}

// =============================================================================
// STATUS / QUEUES / RELOAD
// =============================================================================

func handleStatus() {
	client := getMgmtClient()
	status, err := client.GetStatus()
	if err != nil {
		fatal("Cannot reach PMTA: %v", err)
	}
	fmt.Printf("PMTA Server Status\n")
	fmt.Printf("  Version:      %s\n", status.Version)
	fmt.Printf("  Uptime:       %s\n", status.Uptime)
	fmt.Printf("  Queued:       %d\n", status.TotalQueued)
	fmt.Printf("  Conn In:      %d\n", status.ConnectionsIn)
	fmt.Printf("  Conn Out:     %d\n", status.ConnectionsOut)
}

func handleQueues() {
	client := getMgmtClient()
	queues, err := client.GetQueues()
	if err != nil {
		fatal("Cannot reach PMTA: %v", err)
	}
	fmt.Printf("%-35s %-25s %8s %8s %8s\n", "DOMAIN", "VMTA", "QUEUED", "ERRORS", "EXPIRED")
	fmt.Println(strings.Repeat("-", 90))
	for _, q := range queues {
		fmt.Printf("%-35s %-25s %8d %8d %8d\n", q.Domain, q.VMTA, q.Queued, q.Errors, q.Expired)
	}
}

func handleReload() {
	cmd := exec.Command("pmta", "reload")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Fall back to management API
		client := getMgmtClient()
		if apiErr := client.Reload(); apiErr != nil {
			fatal("Reload failed (both CLI and API): CLI=%v API=%v", err, apiErr)
		}
		fmt.Println("PMTA reloaded via management API")
		return
	}
	fmt.Printf("PMTA reloaded: %s\n", strings.TrimSpace(string(output)))
}

// =============================================================================
// WARMUP COMMANDS
// =============================================================================

func handleWarmup(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: pmta-manager warmup <start|status>")
		os.Exit(1)
	}

	switch args[0] {
	case "start":
		ip := flagValue(args, "--ip")
		if ip == "" {
			fatal("--ip is required")
		}
		fmt.Printf("Warmup schedule for IP %s:\n\n", ip)
		schedule := []struct {
			days   string
			volume int
		}{
			{"Day 1-2", 50}, {"Day 3-4", 100}, {"Day 5-7", 250},
			{"Day 8-10", 500}, {"Day 11-14", 1000}, {"Day 15-18", 2500},
			{"Day 19-22", 5000}, {"Day 23-26", 10000}, {"Day 27-30", 25000},
		}
		for _, s := range schedule {
			fmt.Printf("  %-12s  %6d emails/day\n", s.days, s.volume)
		}
		fmt.Println("\nWarmup must be managed through the IGNITE platform API for automated tracking.")
		fmt.Println("Use: POST /api/mailing/ips/{id}/warmup/start")

	case "status":
		fmt.Println("Warmup status is tracked in the IGNITE platform database.")
		fmt.Println("Use: GET /api/mailing/warmup/dashboard")
	}
}

// =============================================================================
// HELPERS
// =============================================================================

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrDefaultInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	fmt.Sscanf(v, "%d", &n)
	if n == 0 {
		return def
	}
	return n
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
