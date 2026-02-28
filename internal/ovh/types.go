package ovh

import "time"

// Config holds OVHCloud API credentials.
// Generate these at https://api.ovh.com/createToken/
type Config struct {
	Endpoint          string `yaml:"endpoint"`           // ovh-us, ovh-eu, ovh-ca
	ApplicationKey    string `yaml:"application_key"`
	ApplicationSecret string `yaml:"application_secret"`
	ConsumerKey       string `yaml:"consumer_key"`
}

// DedicatedServer represents an OVHCloud dedicated server.
type DedicatedServer struct {
	Name            string `json:"name"`
	IP              string `json:"ip"`
	Datacenter      string `json:"datacenter"`
	ProfessionalUse bool   `json:"professionalUse"`
	OS              string `json:"os"`
	State           string `json:"state"` // ok, error, hacked, hackedBlocked
	Reverse         string `json:"reverse"`
	Monitoring      bool   `json:"monitoring"`
	Rack            string `json:"rack"`
	RootDevice      string `json:"rootDevice"`
}

// FailoverIP represents an OVHCloud failover (additional) IP.
type FailoverIP struct {
	IP              string `json:"ip"`
	RouteredTo      string `json:"routedTo"`
	Country         string `json:"country"`
	Block           string `json:"block,omitempty"`
	Server          string `json:"server,omitempty"`
	Type            string `json:"type"` // failover, cloud
	Status          string `json:"status"`
}

// IPBlock represents an OVHCloud IP block (/28, /27, etc).
type IPBlock struct {
	Block       string   `json:"block"`
	Type        string   `json:"type"` // failover, cloud
	Country     string   `json:"country"`
	RoutedTo    string   `json:"routedTo"`
	Description string   `json:"description"`
	IPs         []string `json:"ips,omitempty"`
}

// ReverseDNS represents a PTR record for an IP.
type ReverseDNS struct {
	IPReverse string `json:"ipReverse"`
	Reverse   string `json:"reverse"`
}

// ServerTask represents an async operation on a server.
type ServerTask struct {
	TaskID   int64  `json:"taskId"`
	Function string `json:"function"`
	Status   string `json:"status"` // init, doing, done, error
	Comment  string `json:"comment"`
	DoneDate string `json:"doneDate,omitempty"`
}

// ProvisionConfig holds parameters for configuring a deployed OVHCloud server.
type ProvisionConfig struct {
	ServerName  string
	ServerIP    string   // Primary IP of the dedicated server
	FailoverIPs []string // Failover IPs attached to this server
	Hostname    string   // e.g. pmta1.mail.ignitemailing.com
	InstallPMTA bool
	PMTARPMPath string
	MgmtPort    int
	MgmtAPIKey  string
	Interface   string // Network interface for failover IPs (default: eth0)
}

// ProvisionResult tracks setup status.
type ProvisionResult struct {
	ServerName string          `json:"server_name"`
	ServerIP   string          `json:"server_ip"`
	Status     string          `json:"status"`
	Steps      []ProvisionStep `json:"steps"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ProvisionStep tracks an individual setup step.
type ProvisionStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // completed, pending, manual_required, error
	Detail string `json:"detail,omitempty"`
}

// SetupStatus tracks the full setup workflow for an OVH PMTA server.
type SetupStatus struct {
	ServerReachable bool   `json:"server_reachable"`
	ServerName      string `json:"server_name,omitempty"`
	ServerIP        string `json:"server_ip,omitempty"`
	FailoverIPs     int    `json:"failover_ips"`
	IPsBound        bool   `json:"ips_bound"`
	PTRConfigured   bool   `json:"ptr_configured"`
	PMTAInstalled   bool   `json:"pmta_installed"`
	FirewallOpen    bool   `json:"firewall_open"`
}
