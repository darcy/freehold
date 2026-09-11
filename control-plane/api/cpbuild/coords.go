package cpbuild

// Coords is the operator-supplied world configuration the CP needs to own the
// build — the non-secret subset of Spec, serialized so a box can hand it to
// the console (route: deploy --world-config) for the console to be the CP
// build executor. It carries the runner + world coordinates and the agents'
// provenance, but NO credential: security material (operator DNS/litellm
// secrets, the runner package) never rides here.
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

// NewSpec builds a build engine Spec from the world coords + a driving
// identity (the console's own secret + pubkey, granted on the co-located
// runner at deploy). sec must be 32 bytes.
func NewSpec(c Coords, sec []byte, audience string) *Spec {
	return &Spec{
		StateDir:       c.StateDir,
		RelayURL:       c.RelayURL,
		RelayAuthURL:   c.RelayAuthURL,
		RelayWS:        c.RelayWS,
		RelayPK:        c.RelayPK,
		RelayHost:      c.RelayHost,
		RelayIP:        c.RelayIP,
		CpHost:         c.CpHost,
		CpIP:           c.CpIP,
		CpLxc:          c.CpLxc,
		ProxyIP:        c.ProxyIP,
		LitellmIP:      c.LitellmIP,
		PlanePool:      c.PlanePool,
		PlaneKind:      c.PlaneKind,
		ThinPool:       c.ThinPool,
		SizeGB:         c.SizeGB,
		PoolSizeGB:     c.PoolSizeGB,
		RootfsGB:       c.RootfsGB,
		MemoryMB:       c.MemoryMB,
		StorageName:    c.StorageName,
		RelayGW:        c.RelayGW,
		Bridge:         c.Bridge,
		RelayLxc:       c.RelayLxc,
		RelayCompose:   c.RelayCompose,
		K3sVmid:        c.K3sVmid,
		RunnerAddr:     c.RunnerAddr,
		RunnerPK:       c.RunnerPK,
		RunnerTarget:   c.RunnerTarget,
		CpaName:        c.CpaName,
		OwnerPub:       c.OwnerPub,
		LitellmBaseURL: c.LitellmBaseURL,
		SelfURL:        c.SelfURL,
		Sec:            sec,
		Audience:       audience,
	}
}

// Coords returns the Spec's non-secret world coords (the inverse of NewSpec).
func (s *Spec) Coords() Coords {
	return Coords{
		StateDir: s.StateDir, RelayURL: s.RelayURL, RelayAuthURL: s.RelayAuthURL,
		RelayWS: s.RelayWS, RelayPK: s.RelayPK, RelayHost: s.RelayHost, RelayIP: s.RelayIP,
		CpHost: s.CpHost, CpIP: s.CpIP, CpLxc: s.CpLxc, ProxyIP: s.ProxyIP, LitellmIP: s.LitellmIP,
		PlanePool: s.PlanePool, PlaneKind: s.PlaneKind, ThinPool: s.ThinPool,
		SizeGB: s.SizeGB, PoolSizeGB: s.PoolSizeGB, RootfsGB: s.RootfsGB, MemoryMB: s.MemoryMB,
		StorageName: s.StorageName, RelayGW: s.RelayGW, Bridge: s.Bridge, RelayLxc: s.RelayLxc,
		RelayCompose: s.RelayCompose, K3sVmid: s.K3sVmid, RunnerAddr: s.RunnerAddr,
		RunnerPK: s.RunnerPK, RunnerTarget: s.RunnerTarget, CpaName: s.CpaName,
		OwnerPub: s.OwnerPub, LitellmBaseURL: s.LitellmBaseURL, SelfURL: s.SelfURL,
	}
}
