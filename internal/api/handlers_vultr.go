package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/vultr"
)

// VultrService handles Vultr API endpoints for server provisioning.
type VultrService struct {
	client *vultr.Client
}

// NewVultrService creates a Vultr API handler service.
func NewVultrService(client *vultr.Client) *VultrService {
	return &VultrService{client: client}
}

// RegisterRoutes mounts Vultr routes within the mailing route group.
func (s *VultrService) RegisterRoutes(r chi.Router) {
	r.Route("/vultr", func(r chi.Router) {
		r.Get("/status", s.HandleStatus)
		r.Get("/plans", s.HandlePlans)
		r.Get("/regions", s.HandleRegions)
		r.Get("/ssh-keys", s.HandleSSHKeys)
		r.Get("/servers", s.HandleListServers)
		r.Get("/servers/{id}", s.HandleGetServer)
		r.Post("/servers", s.HandleProvisionServer)
		r.Post("/servers/{id}/reboot", s.HandleReboot)
		r.Post("/servers/{id}/halt", s.HandleHalt)
		r.Delete("/servers/{id}", s.HandleDeleteServer)
		r.Get("/bgp", s.HandleBGPInfo)
		r.Post("/generate-loa", s.HandleGenerateLOA)
		r.Post("/generate-cloud-init", s.HandleGenerateCloudInit)
	})
}

// HandleStatus returns the overall Vultr integration status.
func (s *VultrService) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.client.IsConfigured() {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"configured": false,
			"message":    "Set VULTR_API_KEY to enable Vultr integration.",
		})
		return
	}

	result := map[string]interface{}{
		"configured": true,
	}

	servers, err := s.client.ListBareMetalServers()
	if err != nil {
		result["servers_error"] = err.Error()
	} else {
		result["server_count"] = len(servers)
		result["servers"] = servers
	}

	bgp, err := s.client.GetBGPInfo()
	if err != nil {
		result["bgp_error"] = err.Error()
	} else {
		result["bgp"] = bgp
	}

	respondJSON(w, http.StatusOK, result)
}

// HandlePlans returns available bare metal plans.
func (s *VultrService) HandlePlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.client.ListBareMetalPlans()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"plans": plans, "total": len(plans)})
}

// HandleRegions returns Vultr regions.
func (s *VultrService) HandleRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := s.client.ListRegions()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"regions": regions, "total": len(regions)})
}

// HandleSSHKeys returns stored SSH keys.
func (s *VultrService) HandleSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.client.ListSSHKeys()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ssh_keys": keys, "total": len(keys)})
}

// HandleListServers returns all bare metal instances.
func (s *VultrService) HandleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.client.ListBareMetalServers()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if servers == nil {
		servers = []vultr.BareMetalInstance{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"servers": servers, "total": len(servers)})
}

// HandleGetServer returns a specific bare metal instance.
func (s *VultrService) HandleGetServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	server, err := s.client.GetBareMetalServer(id)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, server)
}

// HandleProvisionServer provisions a new bare metal PMTA server.
func (s *VultrService) HandleProvisionServer(w http.ResponseWriter, r *http.Request) {
	var input vultr.ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.Region == "" || input.Plan == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "region and plan are required"})
		return
	}

	if input.Label == "" {
		input.Label = "pmta-node-1"
	}
	if input.Hostname == "" {
		input.Hostname = "pmta1.mail.ignitemailing.com"
	}

	// Generate cloud-init with PMTA setup
	ips := vultr.GenerateIPList(input.SubnetBlock)
	provConfig := vultr.ProvisionConfig{
		SubnetBlock: input.SubnetBlock,
		IPs:         ips,
		InstallPMTA: input.InstallPMTA,
		PMTARPMPath: "/root/PowerMTA-5.5r2.rpm",
		Hostname:    input.Hostname,
		MgmtPort:    19000,
	}

	// Check for BGP credentials
	bgp, err := s.client.GetBGPInfo()
	if err == nil && bgp.Enabled {
		provConfig.BGPEnabled = true
		provConfig.BGPASN = bgp.ASN
		provConfig.BGPPassword = bgp.Password
	}

	userData := vultr.GenerateCloudInitBase64(provConfig)

	// CentOS Stream 9 = os_id 542
	req := vultr.CreateBareMetalRequest{
		Region:    input.Region,
		Plan:      input.Plan,
		OsID:      542,
		Label:     input.Label,
		Hostname:  input.Hostname,
		Tag:       "pmta",
		UserData:  userData,
		SSHKeyIDs: input.SSHKeyIDs,
	}

	server, err := s.client.CreateBareMetalServer(req)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	result := &vultr.ProvisionResult{
		ServerID:  server.ID,
		Status:    server.Status,
		MainIP:    server.MainIP,
		Region:    server.Region,
		Plan:      server.Plan,
		Label:     server.Label,
		CreatedAt: time.Now(),
		Steps: []vultr.ProvisionStep{
			{Name: "Server provisioned", Status: "completed", Detail: server.ID},
			{Name: "Cloud-init queued", Status: "pending", Detail: "BIRD + PMTA + IP binding will run on first boot"},
		},
	}

	if !provConfig.BGPEnabled {
		result.Steps = append(result.Steps, vultr.ProvisionStep{
			Name:   "BGP not enabled",
			Status: "manual_required",
			Detail: "Request BGP access in Vultr Portal → Network → BGP before IPs can be announced",
		})
	}

	respondJSON(w, http.StatusCreated, result)
}

// HandleReboot reboots a bare metal instance.
func (s *VultrService) HandleReboot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.client.RebootBareMetalServer(id); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "rebooting", "id": id})
}

// HandleHalt halts a bare metal instance.
func (s *VultrService) HandleHalt(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.client.HaltBareMetalServer(id); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "halted", "id": id})
}

// HandleDeleteServer destroys a bare metal instance.
func (s *VultrService) HandleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.client.DeleteBareMetalServer(id); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": id})
}

// HandleBGPInfo returns BGP credentials and status.
func (s *VultrService) HandleBGPInfo(w http.ResponseWriter, r *http.Request) {
	bgp, err := s.client.GetBGPInfo()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, bgp)
}

// HandleGenerateLOA generates a Letter of Authorization document for Vultr BGP request.
func (s *VultrService) HandleGenerateLOA(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CompanyName string `json:"company_name"`
		Subnet      string `json:"subnet"`
		ContactName string `json:"contact_name"`
		Email       string `json:"email"`
		Phone       string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	loa := vultr.GenerateLOADocument(
		input.CompanyName, input.Subnet,
		input.ContactName, input.Email, input.Phone,
	)
	respondJSON(w, http.StatusOK, map[string]string{"loa": loa})
}

// HandleGenerateCloudInit generates a preview of the cloud-init script.
func (s *VultrService) HandleGenerateCloudInit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SubnetBlock string `json:"subnet_block"`
		Hostname    string `json:"hostname"`
		InstallPMTA bool   `json:"install_pmta"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	ips := vultr.GenerateIPList(input.SubnetBlock)
	cfg := vultr.ProvisionConfig{
		SubnetBlock: input.SubnetBlock,
		IPs:         ips,
		InstallPMTA: input.InstallPMTA,
		PMTARPMPath: "/root/PowerMTA-5.5r2.rpm",
		Hostname:    input.Hostname,
		MgmtPort:    19000,
	}

	bgp, err := s.client.GetBGPInfo()
	if err == nil && bgp.Enabled {
		cfg.BGPEnabled = true
		cfg.BGPASN = bgp.ASN
		cfg.BGPPassword = bgp.Password
	}

	script := vultr.GenerateCloudInit(cfg)
	respondJSON(w, http.StatusOK, map[string]string{"cloud_init": script})
}
