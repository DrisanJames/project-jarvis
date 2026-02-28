package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pmta"
)

// PMTAService handles all PMTA-related API endpoints.
type PMTAService struct {
	db        *sql.DB
	collector *pmta.Collector
	health    *pmta.HealthChecker
}

// NewPMTAService creates a PMTA service.
func NewPMTAService(db *sql.DB, collector *pmta.Collector) *PMTAService {
	return &PMTAService{
		db:        db,
		collector: collector,
		health:    pmta.NewHealthChecker(db),
	}
}

// RegisterRoutes mounts all PMTA routes under the provided router.
// Expects to be called within the /api/mailing route group.
func (s *PMTAService) RegisterRoutes(r chi.Router) {
	// IP Management
	r.Route("/ips", func(r chi.Router) {
		r.Get("/", s.HandleListIPs)
		r.Post("/", s.HandleCreateIP)
		r.Get("/{ipId}", s.HandleGetIP)
		r.Put("/{ipId}", s.HandleUpdateIP)
		r.Delete("/{ipId}", s.HandleDeleteIP)
		r.Post("/{ipId}/check-dns", s.HandleCheckDNS)
		r.Post("/{ipId}/check-blacklist", s.HandleCheckBlacklist)
		r.Post("/{ipId}/warmup/start", s.HandleStartWarmup)
		r.Post("/{ipId}/warmup/pause", s.HandlePauseWarmup)
		r.Get("/{ipId}/warmup/status", s.HandleWarmupStatus)
	})

	// IP Pools
	r.Route("/ip-pools", func(r chi.Router) {
		r.Get("/", s.HandleListPools)
		r.Post("/", s.HandleCreatePool)
		r.Put("/{poolId}", s.HandleUpdatePool)
		r.Post("/{poolId}/add-ip", s.HandleAddIPToPool)
		r.Post("/{poolId}/remove-ip", s.HandleRemoveIPFromPool)
	})

	// PMTA Servers
	r.Route("/pmta-servers", func(r chi.Router) {
		r.Get("/", s.HandleListServers)
		r.Post("/", s.HandleCreateServer)
		r.Get("/{serverId}/status", s.HandleServerStatus)
		r.Get("/{serverId}/queues", s.HandleServerQueues)
		r.Post("/{serverId}/sync", s.HandleSyncConfig)
	})

	// DKIM Keys (under domains)
	r.Post("/domains/{domainId}/dkim/generate", s.HandleGenerateDKIM)
	r.Get("/domains/{domainId}/dkim", s.HandleGetDKIM)
	r.Post("/domains/{domainId}/dkim/verify", s.HandleVerifyDKIM)

	// Warmup Dashboard
	r.Get("/warmup/dashboard", s.HandleWarmupDashboard)

	// PMTA Analytics
	r.Route("/pmta", func(r chi.Router) {
		r.Get("/dashboard", s.HandlePMTADashboard)
		r.Get("/ip-health", s.HandleIPHealth)
		r.Get("/queues", s.HandlePMTAQueues)
		r.Get("/domains", s.HandlePMTADomains)
		r.Get("/reconciliation", s.HandleReconciliation)
		r.Get("/reconciliation/per-ip", s.HandleReconciliationPerIP)
	})
}

// =============================================================================
// IP MANAGEMENT
// =============================================================================

func (s *PMTAService) HandleListIPs(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("organization_id")
	if orgID == "" {
		orgID = getOrgID(r)
	}

	status := r.URL.Query().Get("status")
	poolID := r.URL.Query().Get("pool_id")

	query := `
		SELECT ip.id, ip.organization_id, ip.ip_address::text, ip.hostname,
		       ip.acquisition_type, ip.broker, ip.hosting_provider,
		       ip.pool_id, ip.pmta_server_id,
		       ip.status, ip.warmup_stage, ip.warmup_day, ip.warmup_daily_limit,
		       ip.reputation_score, ip.rdns_verified,
		       ip.blacklisted_on, ip.last_blacklist_check,
		       ip.total_sent, ip.total_delivered, ip.total_bounced, ip.total_complained,
		       ip.last_sent_at, ip.created_at, ip.updated_at,
		       COALESCE(pool.name, '') as pool_name
		FROM mailing_ip_addresses ip
		LEFT JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
		WHERE ip.organization_id = $1
	`
	args := []interface{}{orgID}
	argN := 2

	if status != "" {
		query += fmt.Sprintf(" AND ip.status = $%d", argN)
		args = append(args, status)
		argN++
	}
	if poolID != "" {
		query += fmt.Sprintf(" AND ip.pool_id = $%d", argN)
		args = append(args, poolID)
		argN++
	}
	query += " ORDER BY ip.created_at DESC"

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var ips []map[string]interface{}
	for rows.Next() {
		var (
			id, orgIDVal, ipAddr, hostname                                             string
			acqType, broker, hostingProvider                                            sql.NullString
			poolIDVal, serverID                                                        sql.NullString
			statusVal, warmupStage                                                     string
			warmupDay, warmupLimit                                                     int
			repScore                                                                   float64
			rdnsVerified                                                               bool
			blacklistedOn                                                              json.RawMessage
			lastBlCheck, lastSent, createdAt, updatedAt                                sql.NullTime
			totalSent, totalDelivered, totalBounced, totalComplained                   int64
			poolName                                                                   string
		)
		err := rows.Scan(
			&id, &orgIDVal, &ipAddr, &hostname,
			&acqType, &broker, &hostingProvider,
			&poolIDVal, &serverID,
			&statusVal, &warmupStage, &warmupDay, &warmupLimit,
			&repScore, &rdnsVerified,
			&blacklistedOn, &lastBlCheck,
			&totalSent, &totalDelivered, &totalBounced, &totalComplained,
			&lastSent, &createdAt, &updatedAt,
			&poolName,
		)
		if err != nil {
			log.Printf("[PMTA API] scan error: %v", err)
			continue
		}

		ip := map[string]interface{}{
			"id":                id,
			"organization_id":  orgIDVal,
			"ip_address":       ipAddr,
			"hostname":         hostname,
			"acquisition_type": nullStr(acqType),
			"broker":           nullStr(broker),
			"hosting_provider": nullStr(hostingProvider),
			"pool_id":          nullStr(poolIDVal),
			"pool_name":        poolName,
			"pmta_server_id":   nullStr(serverID),
			"status":           statusVal,
			"warmup_stage":     warmupStage,
			"warmup_day":       warmupDay,
			"warmup_daily_limit": warmupLimit,
			"reputation_score": repScore,
			"rdns_verified":    rdnsVerified,
			"blacklisted_on":   blacklistedOn,
			"total_sent":       totalSent,
			"total_delivered":  totalDelivered,
			"total_bounced":    totalBounced,
			"total_complained": totalComplained,
			"created_at":       createdAt.Time,
		}
		if lastSent.Valid {
			ip["last_sent_at"] = lastSent.Time
		}
		ips = append(ips, ip)
	}

	if ips == nil {
		ips = []map[string]interface{}{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ips": ips, "total": len(ips)})
}

func (s *PMTAService) HandleCreateIP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID  string  `json:"organization_id"`
		IPAddress       string  `json:"ip_address"`
		Hostname        string  `json:"hostname"`
		AcquisitionType string  `json:"acquisition_type"`
		Broker          string  `json:"broker"`
		BrokerReference string  `json:"broker_reference"`
		RIR             string  `json:"rir"`
		CIDRBlock       string  `json:"cidr_block"`
		ASN             string  `json:"asn"`
		HostingProvider string  `json:"hosting_provider"`
		PMTAServerID    *string `json:"pmta_server_id"`
		PoolID          *string `json:"pool_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	if input.IPAddress == "" || input.Hostname == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_address and hostname are required"})
		return
	}

	if input.OrganizationID == "" {
		input.OrganizationID = getOrgID(r)
	}
	if input.AcquisitionType == "" {
		input.AcquisitionType = "purchased"
	}

	var id uuid.UUID
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_ip_addresses (
			organization_id, ip_address, hostname,
			acquisition_type, broker, broker_reference, rir, cidr_block, asn,
			hosting_provider, pmta_server_id, pool_id,
			status, acquired_at
		) VALUES ($1, $2::inet, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'pending', NOW())
		RETURNING id
	`, input.OrganizationID, input.IPAddress, input.Hostname,
		input.AcquisitionType, nullIfEmpty(input.Broker), nullIfEmpty(input.BrokerReference),
		nullIfEmpty(input.RIR), nullIfEmpty(input.CIDRBlock), nullIfEmpty(input.ASN),
		nullIfEmpty(input.HostingProvider), input.PMTAServerID, input.PoolID,
	).Scan(&id)

	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":         id,
		"ip_address": input.IPAddress,
		"hostname":   input.Hostname,
		"status":     "pending",
		"message":    "IP registered. Run DNS check and blacklist check before activating.",
	})
}

func (s *PMTAService) HandleGetIP(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")

	var ip struct {
		ID, OrgID, IPAddr, Hostname, Status, WarmupStage string
		AcqType, Broker, BrokerRef, RIR, CIDR, ASN, Provider sql.NullString
		PoolID, ServerID sql.NullString
		WarmupDay, WarmupLimit int
		RepScore float64
		RDNSVerified bool
		BlacklistedOn json.RawMessage
		TotalSent, TotalDelivered, TotalBounced, TotalComplained int64
		CreatedAt, UpdatedAt time.Time
	}

	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, organization_id, ip_address::text, hostname, status, warmup_stage,
		       acquisition_type, broker, broker_reference, rir, cidr_block, asn, hosting_provider,
		       pool_id, pmta_server_id, warmup_day, warmup_daily_limit,
		       reputation_score, rdns_verified, blacklisted_on,
		       total_sent, total_delivered, total_bounced, total_complained,
		       created_at, updated_at
		FROM mailing_ip_addresses WHERE id = $1
	`, ipID).Scan(
		&ip.ID, &ip.OrgID, &ip.IPAddr, &ip.Hostname, &ip.Status, &ip.WarmupStage,
		&ip.AcqType, &ip.Broker, &ip.BrokerRef, &ip.RIR, &ip.CIDR, &ip.ASN, &ip.Provider,
		&ip.PoolID, &ip.ServerID, &ip.WarmupDay, &ip.WarmupLimit,
		&ip.RepScore, &ip.RDNSVerified, &ip.BlacklistedOn,
		&ip.TotalSent, &ip.TotalDelivered, &ip.TotalBounced, &ip.TotalComplained,
		&ip.CreatedAt, &ip.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "IP not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id": ip.ID, "organization_id": ip.OrgID, "ip_address": ip.IPAddr,
		"hostname": ip.Hostname, "status": ip.Status, "warmup_stage": ip.WarmupStage,
		"acquisition_type": nullStr(ip.AcqType), "broker": nullStr(ip.Broker),
		"rir": nullStr(ip.RIR), "cidr_block": nullStr(ip.CIDR), "asn": nullStr(ip.ASN),
		"hosting_provider": nullStr(ip.Provider),
		"pool_id": nullStr(ip.PoolID), "pmta_server_id": nullStr(ip.ServerID),
		"warmup_day": ip.WarmupDay, "warmup_daily_limit": ip.WarmupLimit,
		"reputation_score": ip.RepScore, "rdns_verified": ip.RDNSVerified,
		"blacklisted_on": ip.BlacklistedOn,
		"total_sent": ip.TotalSent, "total_delivered": ip.TotalDelivered,
		"total_bounced": ip.TotalBounced, "total_complained": ip.TotalComplained,
		"created_at": ip.CreatedAt,
	})
}

func (s *PMTAService) HandleUpdateIP(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")
	var input struct {
		Hostname        *string `json:"hostname"`
		Status          *string `json:"status"`
		PoolID          *string `json:"pool_id"`
		PMTAServerID    *string `json:"pmta_server_id"`
		HostingProvider *string `json:"hosting_provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	if input.Hostname != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_addresses SET hostname=$1, updated_at=NOW() WHERE id=$2`, *input.Hostname, ipID)
	}
	if input.Status != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_addresses SET status=$1, updated_at=NOW() WHERE id=$2`, *input.Status, ipID)
	}
	if input.PoolID != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_addresses SET pool_id=$1, updated_at=NOW() WHERE id=$2`, nullIfEmpty(*input.PoolID), ipID)
	}
	if input.PMTAServerID != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_addresses SET pmta_server_id=$1, updated_at=NOW() WHERE id=$2`, nullIfEmpty(*input.PMTAServerID), ipID)
	}
	if input.HostingProvider != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_addresses SET hosting_provider=$1, updated_at=NOW() WHERE id=$2`, *input.HostingProvider, ipID)
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *PMTAService) HandleDeleteIP(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")
	_, err := s.db.ExecContext(r.Context(), `UPDATE mailing_ip_addresses SET status='retired', updated_at=NOW() WHERE id=$1`, ipID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "retired"})
}

func (s *PMTAService) HandleCheckDNS(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")

	var ipAddr, hostname string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT ip_address::text, hostname FROM mailing_ip_addresses WHERE id=$1`, ipID).Scan(&ipAddr, &hostname)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "IP not found"})
		return
	}

	result, err := s.health.CheckDNS(r.Context(), ipAddr, hostname)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

func (s *PMTAService) HandleCheckBlacklist(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")

	var ipAddr string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT ip_address::text FROM mailing_ip_addresses WHERE id=$1`, ipID).Scan(&ipAddr)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "IP not found"})
		return
	}

	result, err := s.health.CheckBlacklists(r.Context(), ipAddr)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// =============================================================================
// IP POOLS
// =============================================================================

func (s *PMTAService) HandleListPools(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("organization_id")
	if orgID == "" {
		orgID = getOrgID(r)
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT p.id, p.name, p.description, p.pool_type, p.status, p.created_at,
		       COUNT(ip.id) as ip_count,
		       COUNT(CASE WHEN ip.status = 'active' THEN 1 END) as active_count
		FROM mailing_ip_pools p
		LEFT JOIN mailing_ip_addresses ip ON ip.pool_id = p.id
		WHERE p.organization_id = $1
		GROUP BY p.id
		ORDER BY p.name
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var pools []map[string]interface{}
	for rows.Next() {
		var id, name, poolType, status string
		var desc sql.NullString
		var createdAt time.Time
		var ipCount, activeCount int

		rows.Scan(&id, &name, &desc, &poolType, &status, &createdAt, &ipCount, &activeCount)

		pools = append(pools, map[string]interface{}{
			"id":           id,
			"name":         name,
			"description":  nullStr(desc),
			"pool_type":    poolType,
			"status":       status,
			"ip_count":     ipCount,
			"active_count": activeCount,
			"created_at":   createdAt,
		})
	}
	if pools == nil {
		pools = []map[string]interface{}{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"pools": pools})
}

func (s *PMTAService) HandleCreatePool(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID string `json:"organization_id"`
		Name           string `json:"name"`
		Description    string `json:"description"`
		PoolType       string `json:"pool_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if input.OrganizationID == "" {
		input.OrganizationID = getOrgID(r)
	}
	if input.PoolType == "" {
		input.PoolType = "dedicated"
	}

	var id uuid.UUID
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_ip_pools (organization_id, name, description, pool_type)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, input.OrganizationID, input.Name, nullIfEmpty(input.Description), input.PoolType).Scan(&id)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "name": input.Name})
}

func (s *PMTAService) HandleUpdatePool(w http.ResponseWriter, r *http.Request) {
	poolID := chi.URLParam(r, "poolId")
	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Status      *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.Name != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_pools SET name=$1, updated_at=NOW() WHERE id=$2`, *input.Name, poolID)
	}
	if input.Description != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_pools SET description=$1, updated_at=NOW() WHERE id=$2`, *input.Description, poolID)
	}
	if input.Status != nil {
		s.db.ExecContext(r.Context(), `UPDATE mailing_ip_pools SET status=$1, updated_at=NOW() WHERE id=$2`, *input.Status, poolID)
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *PMTAService) HandleAddIPToPool(w http.ResponseWriter, r *http.Request) {
	poolID := chi.URLParam(r, "poolId")
	var input struct {
		IPID string `json:"ip_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.IPID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_id is required"})
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_ip_addresses SET pool_id=$1, updated_at=NOW() WHERE id=$2`, poolID, input.IPID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (s *PMTAService) HandleRemoveIPFromPool(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IPID string `json:"ip_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.IPID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_id is required"})
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_ip_addresses SET pool_id=NULL, updated_at=NOW() WHERE id=$1`, input.IPID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// =============================================================================
// PMTA SERVERS
// =============================================================================

func (s *PMTAService) HandleListServers(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("organization_id")
	if orgID == "" {
		orgID = getOrgID(r)
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT s.id, s.name, s.host, s.smtp_port, s.mgmt_port, s.provider, s.status,
		       s.health_status, s.last_health_check, s.created_at,
		       COUNT(ip.id) as ip_count
		FROM mailing_pmta_servers s
		LEFT JOIN mailing_ip_addresses ip ON ip.pmta_server_id = s.id
		WHERE s.organization_id = $1
		GROUP BY s.id
		ORDER BY s.name
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var servers []map[string]interface{}
	for rows.Next() {
		var id, name, host, status, healthStatus string
		var provider sql.NullString
		var smtpPort, mgmtPort, ipCount int
		var lastCheck sql.NullTime
		var createdAt time.Time

		rows.Scan(&id, &name, &host, &smtpPort, &mgmtPort, &provider, &status,
			&healthStatus, &lastCheck, &createdAt, &ipCount)

		srv := map[string]interface{}{
			"id": id, "name": name, "host": host,
			"smtp_port": smtpPort, "mgmt_port": mgmtPort,
			"provider": nullStr(provider), "status": status,
			"health_status": healthStatus, "ip_count": ipCount,
			"created_at": createdAt,
		}
		if lastCheck.Valid {
			srv["last_health_check"] = lastCheck.Time
		}
		servers = append(servers, srv)
	}
	if servers == nil {
		servers = []map[string]interface{}{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"servers": servers})
}

func (s *PMTAService) HandleCreateServer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID string `json:"organization_id"`
		Name           string `json:"name"`
		Host           string `json:"host"`
		SMTPPort       int    `json:"smtp_port"`
		MgmtPort       int    `json:"mgmt_port"`
		MgmtAPIKey     string `json:"mgmt_api_key"`
		Provider       string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if input.Name == "" || input.Host == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name and host are required"})
		return
	}
	if input.OrganizationID == "" {
		input.OrganizationID = getOrgID(r)
	}
	if input.SMTPPort == 0 {
		input.SMTPPort = 25
	}
	if input.MgmtPort == 0 {
		input.MgmtPort = 19000
	}

	var id uuid.UUID
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_pmta_servers (organization_id, name, host, smtp_port, mgmt_port, mgmt_api_key, provider)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id
	`, input.OrganizationID, input.Name, input.Host, input.SMTPPort, input.MgmtPort,
		nullIfEmpty(input.MgmtAPIKey), nullIfEmpty(input.Provider)).Scan(&id)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "name": input.Name})
}

func (s *PMTAService) HandleServerStatus(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverId")

	var host string
	var mgmtPort int
	var apiKey sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT host, mgmt_port, mgmt_api_key FROM mailing_pmta_servers WHERE id=$1`, serverID).Scan(&host, &mgmtPort, &apiKey)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Server not found"})
		return
	}

	client := pmta.NewClient(host, mgmtPort, nullStr(apiKey))
	status, err := client.GetStatus()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Cannot reach PMTA: %v", err)})
		return
	}
	respondJSON(w, http.StatusOK, status)
}

func (s *PMTAService) HandleServerQueues(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverId")

	var host string
	var mgmtPort int
	var apiKey sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT host, mgmt_port, mgmt_api_key FROM mailing_pmta_servers WHERE id=$1`, serverID).Scan(&host, &mgmtPort, &apiKey)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Server not found"})
		return
	}

	client := pmta.NewClient(host, mgmtPort, nullStr(apiKey))
	queues, err := client.GetQueues()
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Cannot reach PMTA: %v", err)})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"queues": queues})
}

func (s *PMTAService) HandleSyncConfig(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverId")

	var host string
	var mgmtPort int
	var apiKey sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT host, mgmt_port, mgmt_api_key FROM mailing_pmta_servers WHERE id=$1`, serverID).Scan(&host, &mgmtPort, &apiKey)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Server not found"})
		return
	}

	// Build VMTA config from IPs assigned to this server
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT ip.ip_address::text, ip.hostname
		FROM mailing_ip_addresses ip
		WHERE ip.pmta_server_id = $1 AND ip.status IN ('active', 'warmup')
		ORDER BY ip.hostname
	`, serverID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var config string
	for rows.Next() {
		var ipAddr, hostname string
		rows.Scan(&ipAddr, &hostname)
		config += fmt.Sprintf("<virtual-mta %s>\n    smtp-source-host %s %s\n    <domain *>\n        dkim-sign yes\n        max-smtp-out 20\n    </domain>\n</virtual-mta>\n\n", hostname, ipAddr, hostname)
	}

	// Push config to PMTA
	client := pmta.NewClient(host, mgmtPort, nullStr(apiKey))
	if err := client.UploadConfig(config); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Config sync failed: %v", err)})
		return
	}
	if err := client.Reload(); err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("Reload failed: %v", err)})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "synced", "message": "VMTA config pushed and PMTA reloaded"})
}

// =============================================================================
// DKIM
// =============================================================================

func (s *PMTAService) HandleGenerateDKIM(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")

	var domain, orgID string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT domain, organization_id FROM mailing_sending_domains WHERE id=$1`, domainID).Scan(&domain, &orgID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Domain not found"})
		return
	}

	var input struct {
		Selector string `json:"selector"`
		KeySize  int    `json:"key_size"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	if input.Selector == "" {
		input.Selector = "s1"
	}
	if input.KeySize == 0 {
		input.KeySize = 2048
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, input.KeySize)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "Key generation failed"})
		return
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	pubDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "Public key encoding failed"})
		return
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})

	// Build DNS TXT record value
	pubB64 := encodeDKIMPublicKey(pubDER)
	dnsValue := fmt.Sprintf("v=DKIM1; k=rsa; p=%s", pubB64)
	dnsRecord := fmt.Sprintf("%s._domainkey.%s", input.Selector, domain)

	var id uuid.UUID
	err = s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_dkim_keys (organization_id, domain, selector, private_key_encrypted, public_key, key_size, dns_record_value)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (domain, selector) DO UPDATE SET
			private_key_encrypted = EXCLUDED.private_key_encrypted,
			public_key = EXCLUDED.public_key,
			key_size = EXCLUDED.key_size,
			dns_record_value = EXCLUDED.dns_record_value,
			dns_verified = false
		RETURNING id
	`, orgID, domain, input.Selector, privPEM, string(pubPEM), input.KeySize, dnsValue).Scan(&id)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":         id,
		"domain":     domain,
		"selector":   input.Selector,
		"dns_record": dnsRecord,
		"dns_type":   "TXT",
		"dns_value":  dnsValue,
		"key_size":   input.KeySize,
		"message":    fmt.Sprintf("Add TXT record: %s -> %s", dnsRecord, dnsValue),
	})
}

func (s *PMTAService) HandleGetDKIM(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")

	var domain string
	s.db.QueryRowContext(r.Context(),
		`SELECT domain FROM mailing_sending_domains WHERE id=$1`, domainID).Scan(&domain)

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, selector, public_key, algorithm, key_size, dns_record_value, dns_verified, active, created_at
		FROM mailing_dkim_keys WHERE domain = $1 ORDER BY created_at DESC
	`, domain)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var keys []map[string]interface{}
	for rows.Next() {
		var id, selector, pubKey, algorithm, dnsValue string
		var keySize int
		var verified, active bool
		var createdAt time.Time
		rows.Scan(&id, &selector, &pubKey, &algorithm, &keySize, &dnsValue, &verified, &active, &createdAt)
		keys = append(keys, map[string]interface{}{
			"id": id, "selector": selector, "algorithm": algorithm,
			"key_size": keySize, "dns_record_value": dnsValue,
			"dns_verified": verified, "active": active, "created_at": createdAt,
			"dns_record": fmt.Sprintf("%s._domainkey.%s", selector, domain),
		})
	}
	if keys == nil {
		keys = []map[string]interface{}{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"domain": domain, "keys": keys})
}

func (s *PMTAService) HandleVerifyDKIM(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")

	var domain string
	s.db.QueryRowContext(r.Context(),
		`SELECT domain FROM mailing_sending_domains WHERE id=$1`, domainID).Scan(&domain)

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, selector, dns_record_value FROM mailing_dkim_keys WHERE domain = $1 AND active = true`, domain)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, selector, expectedValue string
		rows.Scan(&id, &selector, &expectedValue)

		record := fmt.Sprintf("%s._domainkey.%s", selector, domain)
		txts, err := net.LookupTXT(record)

		verified := false
		actualValue := ""
		errMsg := ""

		if err != nil {
			errMsg = err.Error()
		} else {
			for _, txt := range txts {
				if txt == expectedValue || containsDKIMKey(txt, expectedValue) {
					verified = true
					actualValue = txt
					break
				}
				actualValue = txt
			}
		}

		s.db.ExecContext(r.Context(),
			`UPDATE mailing_dkim_keys SET dns_verified=$1 WHERE id=$2`, verified, id)

		results = append(results, map[string]interface{}{
			"selector": selector, "record": record,
			"verified": verified, "expected": expectedValue,
			"actual": actualValue, "error": errMsg,
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"domain": domain, "results": results})
}

// =============================================================================
// WARMUP
// =============================================================================

func (s *PMTAService) HandleStartWarmup(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")

	_, err := s.db.ExecContext(r.Context(), `
		UPDATE mailing_ip_addresses
		SET status = 'warmup', warmup_stage = 'day1', warmup_day = 1,
		    warmup_daily_limit = 50, warmup_started_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, ipID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Create initial warmup log entries for the 30-day schedule
	schedule := []struct{ day, volume int }{
		{1, 50}, {2, 50}, {3, 100}, {4, 100},
		{5, 250}, {6, 250}, {7, 250},
		{8, 500}, {9, 500}, {10, 500},
		{11, 1000}, {12, 1000}, {13, 1000}, {14, 1000},
		{15, 2500}, {16, 2500}, {17, 2500}, {18, 2500},
		{19, 5000}, {20, 5000}, {21, 5000}, {22, 5000},
		{23, 10000}, {24, 10000}, {25, 10000}, {26, 10000},
		{27, 25000}, {28, 25000}, {29, 25000}, {30, 25000},
	}

	today := time.Now().Truncate(24 * time.Hour)
	for _, entry := range schedule {
		date := today.AddDate(0, 0, entry.day-1)
		s.db.ExecContext(r.Context(), `
			INSERT INTO mailing_ip_warmup_log (ip_id, date, planned_volume, warmup_day, status)
			VALUES ($1, $2, $3, $4, 'pending')
			ON CONFLICT (ip_id, date) DO NOTHING
		`, ipID, date, entry.volume, entry.day)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "warmup_started",
		"ip_id":   ipID,
		"schedule": "30-day graduated warmup",
		"day_1_limit": 50,
	})
}

func (s *PMTAService) HandlePauseWarmup(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")
	_, err := s.db.ExecContext(r.Context(), `
		UPDATE mailing_ip_addresses SET status='paused', updated_at=NOW() WHERE id=$1 AND status='warmup'
	`, ipID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (s *PMTAService) HandleWarmupStatus(w http.ResponseWriter, r *http.Request) {
	ipID := chi.URLParam(r, "ipId")

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT date, planned_volume, actual_sent, actual_delivered, actual_bounced, actual_complained,
		       bounce_rate, complaint_rate, warmup_day, status, notes
		FROM mailing_ip_warmup_log
		WHERE ip_id = $1
		ORDER BY date ASC
	`, ipID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var days []map[string]interface{}
	for rows.Next() {
		var date time.Time
		var planned, sent, delivered, bounced, complained, warmupDay int
		var bounceRate, complaintRate sql.NullFloat64
		var status string
		var notes sql.NullString

		rows.Scan(&date, &planned, &sent, &delivered, &bounced, &complained,
			&bounceRate, &complaintRate, &warmupDay, &status, &notes)

		days = append(days, map[string]interface{}{
			"date": date.Format("2006-01-02"), "planned_volume": planned,
			"actual_sent": sent, "actual_delivered": delivered,
			"actual_bounced": bounced, "actual_complained": complained,
			"bounce_rate": bounceRate.Float64, "complaint_rate": complaintRate.Float64,
			"warmup_day": warmupDay, "status": status, "notes": nullStr(notes),
		})
	}
	if days == nil {
		days = []map[string]interface{}{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ip_id": ipID, "days": days})
}

func (s *PMTAService) HandleWarmupDashboard(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("organization_id")
	if orgID == "" {
		orgID = getOrgID(r)
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT ip.id, ip.ip_address::text, ip.hostname, ip.warmup_day, ip.warmup_daily_limit,
		       ip.warmup_started_at, ip.total_sent, ip.reputation_score, ip.status,
		       COALESCE(wl.actual_sent, 0) as today_sent,
		       COALESCE(wl.bounce_rate, 0) as today_bounce_rate,
		       COALESCE(wl.complaint_rate, 0) as today_complaint_rate
		FROM mailing_ip_addresses ip
		LEFT JOIN mailing_ip_warmup_log wl ON wl.ip_id = ip.id AND wl.date = CURRENT_DATE
		WHERE ip.organization_id = $1 AND ip.status = 'warmup'
		ORDER BY ip.warmup_started_at ASC
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var ips []map[string]interface{}
	for rows.Next() {
		var id, ipAddr, hostname, status string
		var warmupDay, warmupLimit int
		var warmupStarted sql.NullTime
		var totalSent int64
		var repScore, todayBounce, todayComplaint float64
		var todaySent int

		rows.Scan(&id, &ipAddr, &hostname, &warmupDay, &warmupLimit,
			&warmupStarted, &totalSent, &repScore, &status,
			&todaySent, &todayBounce, &todayComplaint)

		ip := map[string]interface{}{
			"id": id, "ip_address": ipAddr, "hostname": hostname,
			"warmup_day": warmupDay, "warmup_daily_limit": warmupLimit,
			"total_sent": totalSent, "reputation_score": repScore, "status": status,
			"today_sent": todaySent, "today_bounce_rate": todayBounce,
			"today_complaint_rate": todayComplaint,
			"progress_pct": float64(warmupDay) / 30.0 * 100,
		}
		if warmupStarted.Valid {
			ip["warmup_started_at"] = warmupStarted.Time
		}
		ips = append(ips, ip)
	}
	if ips == nil {
		ips = []map[string]interface{}{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"warming_ips": ips, "total": len(ips)})
}

// =============================================================================
// PMTA ANALYTICS
// =============================================================================

func (s *PMTAService) HandlePMTADashboard(w http.ResponseWriter, r *http.Request) {
	metrics := s.collector.GetMetrics()

	summary := pmta.DashboardSummary{}
	if metrics.ServerStatus != nil {
		summary.TotalQueued = metrics.ServerStatus.TotalQueued
	}

	for _, h := range metrics.IPHealth {
		summary.TotalDelivered += h.TotalDelivered
		summary.TotalBounced += h.TotalBounced
		summary.TotalComplained += h.TotalComplained
		summary.ActiveIPs++
		switch h.Status {
		case "healthy":
			summary.HealthyIPs++
		case "warning":
			summary.WarningIPs++
		case "critical":
			summary.CriticalIPs++
		}
	}

	totalSent := summary.TotalDelivered + summary.TotalBounced
	if totalSent > 0 {
		summary.OverallDelivery = float64(summary.TotalDelivered) / float64(totalSent) * 100
		summary.OverallBounce = float64(summary.TotalBounced) / float64(totalSent) * 100
	}

	respondJSON(w, http.StatusOK, pmta.DashboardData{
		Server:   metrics.ServerStatus,
		Queues:   metrics.Queues,
		VMTAs:    metrics.VMTAs,
		Domains:  metrics.Domains,
		IPHealth: metrics.IPHealth,
		Summary:  summary,
	})
}

func (s *PMTAService) HandleIPHealth(w http.ResponseWriter, r *http.Request) {
	metrics := s.collector.GetMetrics()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ip_health":     metrics.IPHealth,
		"last_collected": metrics.LastCollected,
	})
}

func (s *PMTAService) HandlePMTAQueues(w http.ResponseWriter, r *http.Request) {
	metrics := s.collector.GetMetrics()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"queues":        metrics.Queues,
		"last_collected": metrics.LastCollected,
	})
}

func (s *PMTAService) HandlePMTADomains(w http.ResponseWriter, r *http.Request) {
	metrics := s.collector.GetMetrics()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domains":       metrics.Domains,
		"last_collected": metrics.LastCollected,
	})
}

// =============================================================================
// RECONCILIATION
// =============================================================================

func (s *PMTAService) HandleReconciliation(w http.ResponseWriter, r *http.Request) {
	engine := pmta.NewReconciliationEngine(s.db)

	// Default to last 7 days
	end := time.Now()
	start := end.AddDate(0, 0, -7)

	if startParam := r.URL.Query().Get("start_date"); startParam != "" {
		if t, err := time.Parse("2006-01-02", startParam); err == nil {
			start = t
		}
	}
	if endParam := r.URL.Query().Get("end_date"); endParam != "" {
		if t, err := time.Parse("2006-01-02", endParam); err == nil {
			end = t
		}
	}

	report, err := engine.Reconcile(r.Context(), start, end)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, report)
}

func (s *PMTAService) HandleReconciliationPerIP(w http.ResponseWriter, r *http.Request) {
	engine := pmta.NewReconciliationEngine(s.db)
	results, err := engine.ReconcilePerIP(r.Context())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if results == nil {
		results = []pmta.IPReconciliation{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"reconciliation": results, "total": len(results)})
}

// =============================================================================
// HELPERS
// =============================================================================

func getOrgID(r *http.Request) string {
	if orgID := r.Header.Get("X-Organization-ID"); orgID != "" {
		return orgID
	}
	if orgID := r.URL.Query().Get("organization_id"); orgID != "" {
		return orgID
	}
	return "00000000-0000-0000-0000-000000000001"
}

func encodeDKIMPublicKey(der []byte) string {
	return base64.StdEncoding.EncodeToString(der)
}

func containsDKIMKey(actual, expected string) bool {
	a := strings.ReplaceAll(actual, " ", "")
	e := strings.ReplaceAll(expected, " ", "")
	return strings.Contains(a, e) || a == e
}

