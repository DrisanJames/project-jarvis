package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/ipxo"
)

// IPXOService handles IPXO API endpoints for the platform.
type IPXOService struct {
	service *ipxo.Service
	client  *ipxo.Client
}

// NewIPXOService creates an IPXO API handler service.
func NewIPXOService(client *ipxo.Client, service *ipxo.Service) *IPXOService {
	return &IPXOService{client: client, service: service}
}

// RegisterRoutes mounts IPXO routes within the mailing route group.
func (s *IPXOService) RegisterRoutes(r chi.Router) {
	r.Route("/ipxo", func(r chi.Router) {
		r.Get("/dashboard", s.HandleDashboard)
		r.Get("/prefixes", s.HandlePrefixes)
		r.Get("/prefixes/unannounced", s.HandleUnannounced)
		r.Post("/prefixes/{notation}/tag", s.HandleTagPrefix)
		r.Get("/subscriptions", s.HandleSubscriptions)
		r.Get("/invoices", s.HandleInvoices)
		r.Get("/credits", s.HandleCredits)
		r.Get("/services", s.HandleServices)
		r.Get("/setup-status", s.HandleSetupStatus)
		r.Post("/asn/validate", s.HandleValidateASN)
		r.Post("/asn/assign", s.HandleAssignASN)
		r.Post("/sync", s.HandleSync)
	})
}

// HandleDashboard returns a complete IPXO dashboard summary.
func (s *IPXOService) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.client.IsConfigured() {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"configured": false,
			"message":    "IPXO integration is not configured. Set IPXO_CLIENT_ID, IPXO_SECRET_KEY, and IPXO_COMPANY_UUID.",
		})
		return
	}

	dashboard, err := s.service.GetDashboard()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"configured": true,
		"dashboard":  dashboard,
	})
}

// HandlePrefixes returns all discovered prefixes with enriched data.
func (s *IPXOService) HandlePrefixes(w http.ResponseWriter, r *http.Request) {
	fieldsParam := r.URL.Query().Get("fields")
	var fields []string
	if fieldsParam != "" {
		for _, f := range splitCSV(fieldsParam) {
			fields = append(fields, f)
		}
	}

	result, err := s.client.SearchPrefixes(fields, 0)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// HandleUnannounced returns prefixes not currently announced via BGP.
func (s *IPXOService) HandleUnannounced(w http.ResponseWriter, r *http.Request) {
	result, err := s.client.GetUnannouncedPrefixes()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// HandleTagPrefix writes custom metadata to a prefix in IPXO.
func (s *IPXOService) HandleTagPrefix(w http.ResponseWriter, r *http.Request) {
	notation := chi.URLParam(r, "notation")

	var metadata map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := s.client.UpdatePrefixMetadata(notation, metadata); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "tagged", "notation": notation})
}

// HandleSubscriptions returns active IPXO subscriptions.
func (s *IPXOService) HandleSubscriptions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "active"
	}

	subs, err := s.client.ListSubscriptions(status)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if subs == nil {
		subs = []ipxo.Subscription{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"subscriptions": subs, "total": len(subs)})
}

// HandleInvoices returns recent IPXO invoices.
func (s *IPXOService) HandleInvoices(w http.ResponseWriter, r *http.Request) {
	invoices, err := s.client.ListInvoices(1, 50)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if invoices == nil {
		invoices = []ipxo.Invoice{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"invoices": invoices, "total": len(invoices)})
}

// HandleCredits returns the IPXO credit balance.
func (s *IPXOService) HandleCredits(w http.ResponseWriter, r *http.Request) {
	balance, err := s.client.GetCreditBalance()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, balance)
}

// HandleValidateASN validates an ASN for given subnets.
func (s *IPXOService) HandleValidateASN(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ASN     int      `json:"asn"`
		Subnets []string `json:"subnets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.ASN == 0 || len(input.Subnets) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "asn and subnets are required"})
		return
	}

	result, err := s.client.ValidateASN(input.ASN, input.Subnets)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// HandleSync syncs IPXO prefixes into the local IP management database.
func (s *IPXOService) HandleSync(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID string `json:"organization_id"`
		ExpandIPs      bool   `json:"expand_ips"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	if input.OrganizationID == "" {
		input.OrganizationID = getOrgID(r)
	}

	result, err := s.service.SyncPrefixesToDB(r.Context(), input.OrganizationID, input.ExpandIPs)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// HandleServices returns all IPv4 services (leased subnets) with LOA and billing info.
func (s *IPXOService) HandleServices(w http.ResponseWriter, r *http.Request) {
	services, err := s.client.ListServices()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, services)
}

// HandleSetupStatus returns the current IP setup workflow status.
func (s *IPXOService) HandleSetupStatus(w http.ResponseWriter, r *http.Request) {
	setupStatus := &ipxo.SetupStatus{
		RDNSNote:       "rDNS/PTR records must be configured in the IPXO Portal (portal-only, no API). Go to Leasing → Leased Subnets → rDNS Management.",
		ForwardDNSNote: "Forward DNS (A records, SPF, DKIM) is managed by your DNS provider (e.g., Cloudflare, Route53), not IPXO.",
		ServerNote:     "After ROA confirmation (~48hrs), complete the BYOIP setup at your hosting provider (Vultr, Hetzner, etc.).",
	}

	services, err := s.client.ListServices()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	for _, svc := range services.Data {
		if svc.BillingService.Status == "active" {
			setupStatus.SubnetLeased = true
			setupStatus.SubnetBlock = fmt.Sprintf("%s/%d", svc.BillingService.Address, svc.BillingService.CIDR)

			if len(svc.LOA) > 0 {
				setupStatus.ASNAssigned = true
				setupStatus.LOAStatus = svc.LOA[0].Status
			} else {
				setupStatus.LOAStatus = "not_assigned"
			}
			break
		}
	}

	respondJSON(w, http.StatusOK, setupStatus)
}

// HandleAssignASN runs the full ASN assignment (LOA purchase) flow via IPXO API.
func (s *IPXOService) HandleAssignASN(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ASN         int    `json:"asn"`
		Subnet      string `json:"subnet"`
		CompanyName string `json:"company_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.ASN == 0 || input.Subnet == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "asn and subnet are required"})
		return
	}
	if input.CompanyName == "" {
		input.CompanyName = "Ignite Media Group"
	}

	result, err := s.client.AssignASN(input.ASN, input.Subnet, input.CompanyName)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

func splitCSV(s string) []string {
	var parts []string
	for _, p := range split(s, ",") {
		p = trim(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s, sep string) []string {
	result := []string{}
	for {
		idx := indexOf(s, sep)
		if idx < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	return result
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
