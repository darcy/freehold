package config

// AgentToolsPort is the CP's freehold-agent-tools MCP bind port. Any URL the
// console/install hands to the toolset — and the URL the CPA pod uses to curl
// its stdio bridge binary — is derived from it.
const AgentToolsPort = "8089"

// Coords is the operator-supplied world configuration the CP needs to own the
// build — the non-secret subset of the build Spec, serialized so a box can hand
// it to the console (route: deploy --world-config) for the console to be the CP
// build executor. It carries the runner + world coordinates and the agents'
// provenance, but NO credential: security material (operator DNS/litellm
// secrets, the runner package) never rides here. Shared by the install CLI (it
// renders these coords at deploy-cp) and the CP console (it consumes them), so
// it lives in the contract, not in either module.
type Coords struct {
	StateDir       string `json:"state_dir,omitempty"`
	RelayURL       string `json:"relay_url,omitempty"`
	RelayAuthURL   string `json:"relay_auth_url,omitempty"`
	RelayWS        string `json:"relay_ws,omitempty"`
	RelayPK        string `json:"relay_pubkey,omitempty"`
	RelayHost      string `json:"relay_host,omitempty"`
	RelayIP        string `json:"relay_ip,omitempty"`
	CpHost         string `json:"cp_host,omitempty"`
	CpIP           string `json:"cp_ip,omitempty"`
	CpLxc          uint32 `json:"cp_lxc,omitempty"`
	ProxyIP        string `json:"proxy_ip,omitempty"`
	LitellmIP      string `json:"litellm_ip,omitempty"`
	PlanePool      string `json:"plane_pool,omitempty"`
	PlaneKind      string `json:"plane_kind,omitempty"`
	ThinPool       string `json:"thin_pool,omitempty"`
	SizeGB         uint64 `json:"size_gb,omitempty"`
	PoolSizeGB     uint64 `json:"pool_size_gb,omitempty"`
	RootfsGB       uint32 `json:"rootfs_gb,omitempty"`
	MemoryMB       uint32 `json:"memory_mb,omitempty"`
	StorageName    string `json:"storage,omitempty"`
	RelayGW        string `json:"relay_gw,omitempty"`
	Bridge         string `json:"bridge,omitempty"`
	RelayLxc       uint32 `json:"relay_lxc,omitempty"`
	RelayCompose   string `json:"relay_compose,omitempty"`
	K3sVmid        uint32 `json:"k3s_vmid,omitempty"`
	RunnerAddr     string `json:"runner_addr,omitempty"`
	RunnerPK       string `json:"runner_pubkey,omitempty"`
	RunnerTarget   string `json:"runner_target,omitempty"`
	CpaName        string `json:"cpa_name,omitempty"`
	OwnerPub       string `json:"owner_pubkey,omitempty"`
	LitellmBaseURL string `json:"litellm_base,omitempty"`
	SelfURL        string `json:"self_url,omitempty"`
}
