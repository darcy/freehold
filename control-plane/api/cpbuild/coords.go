package cpbuild

import "freehold/contract/config"

// Coords is the shared world-config type, owned by the contract (both this
// console and the install CLI render/consume them). Alias kept so existing
// callers keep `cpbuild.Coords`.
type Coords = config.Coords

// NewSpec builds a build engine Spec from the world coords + a driving
// identity (the console's own secret + pubkey, granted on the co-located
// runner at deploy). sec must be 32 bytes.
func NewSpec(c Coords, sec []byte, audience string) *Spec {
	s := &Spec{
		Name:           c.Name,
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
	s.FillEdgeURLs()
	return s
}

// FillEdgeURLs fills the gateway URLs that are pure functions of the proxy
// IP. Fresh-world installs happen before litellm exists, so a spec built from
// the install-time world-config arrives with LitellmIP/LitellmBaseURL blank —
// without them the agent pods get an empty OPENAI_COMPAT_BASE_URL and every
// turn fails with "llm: transport: builder error". Idempotent.
func (s *Spec) FillEdgeURLs() {
	if s.ProxyIP == "" {
		return
	}
	if s.LitellmIP == "" {
		s.LitellmIP = config.StripCIDR(s.ProxyIP)
	}
	if s.LitellmBaseURL == "" && s.LitellmIP != "" {
		s.LitellmBaseURL = "http://" + s.LitellmIP + ":31400/v1"
	}
}

// Coords returns the Spec's non-secret world coords (the inverse of NewSpec).
func (s *Spec) Coords() Coords {
	return Coords{
		Name:     s.Name,
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
