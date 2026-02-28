package pmta

import "time"

// ServerStatus represents the overall PMTA server state from the management API.
type ServerStatus struct {
	Version       string    `json:"version"`
	Uptime        string    `json:"uptime"`
	TotalQueued   int       `json:"total_queued"`
	TotalDomains  int       `json:"total_domains"`
	TotalVMTAs    int       `json:"total_vmtas"`
	ConnectionsIn int       `json:"connections_in"`
	ConnectionsOut int      `json:"connections_out"`
	CheckedAt     time.Time `json:"checked_at"`
}

// QueueEntry represents a single domain/VMTA queue from PMTA.
type QueueEntry struct {
	Domain     string `json:"domain"`
	VMTA       string `json:"vmta"`
	Queued     int    `json:"queued"`
	Recipients int    `json:"recipients"`
	Errors     int    `json:"errors"`
	Expired    int    `json:"expired"`
}

// VMTAStatus represents a Virtual MTA's current state.
type VMTAStatus struct {
	Name           string  `json:"name"`
	SourceIP       string  `json:"source_ip"`
	Hostname       string  `json:"hostname"`
	ConnectionsOut int     `json:"connections_out"`
	Queued         int     `json:"queued"`
	Delivered      int     `json:"delivered"`
	Bounced        int     `json:"bounced"`
	DeliveryRate   float64 `json:"delivery_rate"`
}

// DomainStatus represents delivery stats for a destination domain.
type DomainStatus struct {
	Domain         string  `json:"domain"`
	Queued         int     `json:"queued"`
	Delivered      int     `json:"delivered"`
	Bounced        int     `json:"bounced"`
	ConnectionsOut int     `json:"connections_out"`
	DeliveryRate   float64 `json:"delivery_rate"`
}

// AcctRecord represents a parsed row from the PMTA accounting CSV file.
type AcctRecord struct {
	Type        string    `json:"type"` // d=delivered, b=bounced, f=FBL, rb=remote-bounce
	TimeLogged  time.Time `json:"time_logged"`
	Orig        string    `json:"orig"`        // envelope sender
	Rcpt        string    `json:"rcpt"`        // envelope recipient
	SourceIP    string    `json:"source_ip"`
	VMTA        string    `json:"vmta"`
	JobID       string    `json:"job_id"`
	Domain      string    `json:"domain"`       // recipient domain
	BounceCode  string    `json:"bounce_code"`
	DSNDiag     string    `json:"dsn_diag"`
	BounceCat   string    `json:"bounce_cat"`   // hard, soft, block, etc.
	MessageID   string    `json:"message_id"`
	DKIMResult  string    `json:"dkim_result"`
}

// CollectorMetrics holds the aggregated analytics from the latest collection cycle.
type CollectorMetrics struct {
	ServerStatus  *ServerStatus         `json:"server_status"`
	Queues        []QueueEntry          `json:"queues"`
	VMTAs         []VMTAStatus          `json:"vmtas"`
	Domains       []DomainStatus        `json:"domains"`
	IPHealth      map[string]*IPHealth  `json:"ip_health"`
	LastCollected time.Time             `json:"last_collected"`
}

// IPHealth tracks delivery health for a single IP address.
type IPHealth struct {
	IP             string    `json:"ip"`
	Hostname       string    `json:"hostname"`
	TotalSent      int64     `json:"total_sent"`
	TotalDelivered int64     `json:"total_delivered"`
	TotalBounced   int64     `json:"total_bounced"`
	TotalComplained int64    `json:"total_complained"`
	DeliveryRate   float64   `json:"delivery_rate"`
	BounceRate     float64   `json:"bounce_rate"`
	ComplaintRate  float64   `json:"complaint_rate"`
	Status         string    `json:"status"` // healthy, warning, critical
	Blacklists     []string  `json:"blacklists"`
	LastChecked    time.Time `json:"last_checked"`
}

// DashboardData is the complete PMTA dashboard response for the frontend.
type DashboardData struct {
	Server   *ServerStatus         `json:"server"`
	Queues   []QueueEntry          `json:"queues"`
	VMTAs    []VMTAStatus          `json:"vmtas"`
	Domains  []DomainStatus        `json:"domains"`
	IPHealth map[string]*IPHealth  `json:"ip_health"`
	Summary  DashboardSummary      `json:"summary"`
}

// DashboardSummary provides top-level metrics for the PMTA dashboard.
type DashboardSummary struct {
	TotalQueued     int     `json:"total_queued"`
	TotalDelivered  int64   `json:"total_delivered"`
	TotalBounced    int64   `json:"total_bounced"`
	TotalComplained int64   `json:"total_complained"`
	OverallDelivery float64 `json:"overall_delivery_rate"`
	OverallBounce   float64 `json:"overall_bounce_rate"`
	ActiveIPs       int     `json:"active_ips"`
	HealthyIPs      int     `json:"healthy_ips"`
	WarningIPs      int     `json:"warning_ips"`
	CriticalIPs     int     `json:"critical_ips"`
}
