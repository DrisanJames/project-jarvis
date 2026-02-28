package vultr

import "time"

// Config holds Vultr API credentials.
type Config struct {
	APIKey string `yaml:"api_key"`
}

// BareMetalPlan represents an available bare metal plan.
type BareMetalPlan struct {
	ID        string   `json:"id"`
	CPUCount  int      `json:"cpu_count"`
	CPUModel  string   `json:"cpu_model"`
	CPUThreads int     `json:"cpu_threads"`
	RAM       int      `json:"ram"`
	Disk      int      `json:"disk"`
	DiskCount int      `json:"disk_count"`
	Bandwidth int      `json:"bandwidth"`
	MonthlyCost float64 `json:"monthly_cost"`
	Type      string   `json:"type"`
	Locations []string `json:"locations"`
}

// Region represents a Vultr data center region.
type Region struct {
	ID        string   `json:"id"`
	City      string   `json:"city"`
	Country   string   `json:"country"`
	Continent string   `json:"continent"`
	Options   []string `json:"options"`
}

// OperatingSystem represents an available OS.
type OperatingSystem struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Family string `json:"family"`
}

// BareMetalInstance represents a provisioned bare metal server.
type BareMetalInstance struct {
	ID          string  `json:"id"`
	OS          string  `json:"os"`
	RAM         string  `json:"ram"`
	Disk        string  `json:"disk"`
	MainIP      string  `json:"main_ip"`
	CPUCount    int     `json:"cpu_count"`
	Region      string  `json:"region"`
	Plan        string  `json:"plan"`
	Label       string  `json:"label"`
	Hostname    string  `json:"hostname"`
	Tag         string  `json:"tag"`
	OsID        int     `json:"os_id"`
	AppID       int     `json:"app_id"`
	Status      string  `json:"status"`
	PowerStatus string  `json:"power_status"`
	ServerStatus string `json:"server_status"`
	V6MainIP    string  `json:"v6_main_ip"`
	V6Network   string  `json:"v6_network"`
	V6NetworkSize int   `json:"v6_network_size"`
	MacAddress  int64   `json:"mac_address"`
	NetmaskV4   string  `json:"netmask_v4"`
	GatewayV4   string  `json:"gateway_v4"`
	DateCreated string  `json:"date_created"`
	UserData    string  `json:"user_data"`
}

// BGPInfo holds the account's BGP configuration.
type BGPInfo struct {
	Enabled     bool   `json:"enabled"`
	ASN         int    `json:"asn"`
	Password    string `json:"password"`
	Networks    []struct {
		V4Subnet    string `json:"v4_subnet"`
		V4SubnetLen int    `json:"v4_subnet_len"`
	} `json:"networks"`
}

// CreateBareMetalRequest holds parameters for provisioning a new server.
type CreateBareMetalRequest struct {
	Region   string `json:"region"`
	Plan     string `json:"plan"`
	OsID     int    `json:"os_id"`
	Label    string `json:"label"`
	Hostname string `json:"hostname"`
	Tag      string `json:"tag,omitempty"`
	UserData string `json:"user_data,omitempty"`
	ScriptID string `json:"script_id,omitempty"`
	SSHKeyIDs []string `json:"sshkey_id,omitempty"`
}

// ProvisionRequest is the frontend-facing request to spin up a PMTA node.
type ProvisionRequest struct {
	Region     string `json:"region"`
	Plan       string `json:"plan"`
	Label      string `json:"label"`
	Hostname   string `json:"hostname"`
	SSHKeyIDs  []string `json:"ssh_key_ids,omitempty"`
	SubnetBlock string `json:"subnet_block"`
	InstallPMTA bool   `json:"install_pmta"`
}

// ProvisionResult tracks the status of a server provisioning operation.
type ProvisionResult struct {
	ServerID    string    `json:"server_id"`
	Status      string    `json:"status"`
	MainIP      string    `json:"main_ip"`
	Region      string    `json:"region"`
	Plan        string    `json:"plan"`
	Label       string    `json:"label"`
	CreatedAt   time.Time `json:"created_at"`
	Steps       []ProvisionStep `json:"steps"`
}

// ProvisionStep tracks an individual provisioning step.
type ProvisionStep struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ServerSetupStatus tracks the full setup workflow for a PMTA server.
type ServerSetupStatus struct {
	ServerProvisioned bool   `json:"server_provisioned"`
	ServerID          string `json:"server_id,omitempty"`
	ServerIP          string `json:"server_ip,omitempty"`
	ServerStatus      string `json:"server_status,omitempty"`
	BGPEnabled        bool   `json:"bgp_enabled"`
	BGPNote           string `json:"bgp_note"`
	IPsBound          bool   `json:"ips_bound"`
	PMTAInstalled     bool   `json:"pmta_installed"`
}

// SSHKey represents a stored SSH key in Vultr.
type SSHKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SSHKey      string `json:"ssh_key"`
	DateCreated string `json:"date_created"`
}
