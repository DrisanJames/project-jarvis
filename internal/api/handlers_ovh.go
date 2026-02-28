package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/ovh"
)

// OVHService handles OVHCloud API endpoints for server management.
type OVHService struct {
	client *ovh.Client
}

// NewOVHService creates an OVH API handler service.
func NewOVHService(client *ovh.Client) *OVHService {
	return &OVHService{client: client}
}

// RegisterRoutes mounts OVH routes within the mailing route group.
func (s *OVHService) RegisterRoutes(r chi.Router) {
	r.Route("/ovh", func(r chi.Router) {
		r.Get("/status", s.HandleStatus)

		// Server management
		r.Get("/servers", s.HandleListServers)
		r.Get("/servers/{name}", s.HandleGetServer)
		r.Post("/servers/{name}/reboot", s.HandleReboot)

		// Failover IPs
		r.Get("/ips", s.HandleListIPs)
		r.Get("/ips/{ip}", s.HandleGetIPBlock)
		r.Post("/ips/{ip}/move", s.HandleMoveIP)

		// Reverse DNS
		r.Get("/rdns/{ip}", s.HandleGetRDNS)
		r.Post("/rdns", s.HandleSetRDNS)
		r.Post("/rdns/bulk", s.HandleBulkSetRDNS)
		r.Delete("/rdns/{ip}", s.HandleDeleteRDNS)

		// Provisioning / setup
		r.Post("/generate-setup-script", s.HandleGenerateSetupScript)
		r.Post("/generate-dns-records", s.HandleGenerateDNSRecords)
	})
}

// HandleStatus returns the OVH integration status and server inventory.
func (s *OVHService) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.client.IsConfigured() {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"configured": false,
			"message":    "Set OVH_APP_KEY, OVH_APP_SECRET, and OVH_CONSUMER_KEY to enable OVHCloud integration.",
		})
		return
	}

	result := map[string]interface{}{
		"configured": true,
		"provider":   "ovhcloud",
	}

	servers, err := s.client.ListDedicatedServers()
	if err != nil {
		result["servers_error"] = err.Error()
	} else {
		result["server_count"] = len(servers)
		result["server_names"] = servers
	}

	ips, err := s.client.ListIPs("failover")
	if err != nil {
		result["ips_error"] = err.Error()
	} else {
		result["failover_ip_count"] = len(ips)
		result["failover_ips"] = ips
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleListServers returns all dedicated server service names.
func (s *OVHService) HandleListServers(w http.ResponseWriter, r *http.Request) {
	names, err := s.client.ListDedicatedServers()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	var servers []interface{}
	for _, name := range names {
		server, err := s.client.GetDedicatedServer(name)
		if err != nil {
			servers = append(servers, map[string]string{"name": name, "error": err.Error()})
			continue
		}
		servers = append(servers, server)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"servers": servers,
		"total":   len(servers),
	})
}

// HandleGetServer returns details for a specific server.
func (s *OVHService) HandleGetServer(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	server, err := s.client.GetDedicatedServer(name)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, server)
}

// HandleReboot requests a hardware reboot.
func (s *OVHService) HandleReboot(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	task, err := s.client.RebootServer(name)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, task)
}

// HandleListIPs returns all failover IP blocks.
func (s *OVHService) HandleListIPs(w http.ResponseWriter, r *http.Request) {
	ipType := r.URL.Query().Get("type")
	if ipType == "" {
		ipType = "failover"
	}

	blocks, err := s.client.ListIPs(ipType)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	var details []interface{}
	for _, block := range blocks {
		info, err := s.client.GetIPBlock(block)
		if err != nil {
			details = append(details, map[string]string{"block": block, "error": err.Error()})
			continue
		}
		details = append(details, info)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ip_blocks": details,
		"total":     len(details),
	})
}

// HandleGetIPBlock returns details for a specific IP block.
func (s *OVHService) HandleGetIPBlock(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")
	info, err := s.client.GetIPBlock(ip)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, info)
}

// HandleMoveIP routes a failover IP to a different server.
func (s *OVHService) HandleMoveIP(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")
	var input struct {
		TargetServer string `json:"target_server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.TargetServer == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "target_server is required"})
		return
	}

	task, err := s.client.MoveFailoverIP(ip, input.TargetServer)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, task)
}

// HandleGetRDNS returns the PTR record for an IP.
func (s *OVHService) HandleGetRDNS(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")
	rev, err := s.client.GetReverseDNS(ip)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, rev)
}

// HandleSetRDNS sets the PTR record for a single IP.
func (s *OVHService) HandleSetRDNS(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IP      string `json:"ip"`
		Reverse string `json:"reverse"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.IP == "" || input.Reverse == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "ip and reverse are required"})
		return
	}

	if err := s.client.SetReverseDNS(input.IP, input.Reverse); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"ip":      input.IP,
		"reverse": input.Reverse,
	})
}

// HandleBulkSetRDNS sets PTR records for multiple IPs at once.
func (s *OVHService) HandleBulkSetRDNS(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Records []ovh.ReverseDNS `json:"records"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	type rdnsResult struct {
		IP      string `json:"ip"`
		Reverse string `json:"reverse"`
		Status  string `json:"status"`
		Error   string `json:"error,omitempty"`
	}

	var results []rdnsResult
	for _, rec := range input.Records {
		if err := s.client.SetReverseDNS(rec.IPReverse, rec.Reverse); err != nil {
			results = append(results, rdnsResult{
				IP: rec.IPReverse, Reverse: rec.Reverse,
				Status: "error", Error: err.Error(),
			})
		} else {
			results = append(results, rdnsResult{
				IP: rec.IPReverse, Reverse: rec.Reverse,
				Status: "success",
			})
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// HandleDeleteRDNS removes the PTR record for an IP.
func (s *OVHService) HandleDeleteRDNS(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")
	if err := s.client.DeleteReverseDNS(ip); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted", "ip": ip})
}

// HandleGenerateSetupScript generates a bash script to configure the OVH server.
func (s *OVHService) HandleGenerateSetupScript(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ServerIP    string   `json:"server_ip"`
		FailoverIPs []string `json:"failover_ips"`
		Hostname    string   `json:"hostname"`
		InstallPMTA bool     `json:"install_pmta"`
		Interface   string   `json:"interface"`
		MgmtAPIKey  string   `json:"mgmt_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	if input.Hostname == "" {
		input.Hostname = "pmta1.mail.ignitemailing.com"
	}

	cfg := ovh.ProvisionConfig{
		ServerIP:    input.ServerIP,
		FailoverIPs: input.FailoverIPs,
		Hostname:    input.Hostname,
		InstallPMTA: input.InstallPMTA,
		PMTARPMPath: "/root/PowerMTA-5.5r2.rpm",
		MgmtPort:    19000,
		MgmtAPIKey:  input.MgmtAPIKey,
		Interface:   input.Interface,
	}

	script := ovh.GenerateSetupScript(cfg)
	dnsRecords := ovh.GenerateDNSRecords(cfg)
	rdnsRecords := ovh.GenerateRDNSCommands(cfg)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"setup_script":  script,
		"dns_records":   dnsRecords,
		"rdns_records":  rdnsRecords,
		"created_at":    time.Now(),
	})
}

// HandleGenerateDNSRecords returns the A records and PTR records needed.
func (s *OVHService) HandleGenerateDNSRecords(w http.ResponseWriter, r *http.Request) {
	var input struct {
		FailoverIPs []string `json:"failover_ips"`
		Hostname    string   `json:"hostname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	cfg := ovh.ProvisionConfig{
		FailoverIPs: input.FailoverIPs,
		Hostname:    input.Hostname,
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"a_records":   ovh.GenerateDNSRecords(cfg),
		"ptr_records": ovh.GenerateRDNSCommands(cfg),
	})
}
