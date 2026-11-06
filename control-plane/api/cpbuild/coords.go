package cpbuild

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"freehold/contract/config"
)

// Coords is the shared world-config type, owned by the contract (both this
// console and the install CLI render/consume them). Alias kept so existing
// callers keep `cpbuild.Coords`.
type Coords = config.Coords

// NewSpec builds a build engine Spec from the world coords + a driving
// identity (the console's own secret + pubkey, granted on the co-located
// runner at deploy). sec must be 32 bytes.
// PangolinFromArg decodes the agent-tools serve argv's pangolin block (b64
// JSON of the non-secret coords shape). Empty returns nil.
func PangolinFromArg(arg string) (*config.PangolinCoords, error) {
	if strings.TrimSpace(arg) == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(arg))
	if err != nil {
		return nil, err
	}
	var p config.PangolinCoords
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// EdgeAppsFromArg decodes the agent-tools serve argv's edge-apps block (b64
// JSON of the profile's [edge.apps] riders). Empty returns nil — a serve
// predating the flag renders the registry-only edge, exactly as before.
func EdgeAppsFromArg(arg string) (map[string]config.EdgeApp, error) {
	if strings.TrimSpace(arg) == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(arg))
	if err != nil {
		return nil, err
	}
	var apps map[string]config.EdgeApp
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// EdgeAppsArg encodes the Spec's edge riders for the agent-tools serve argv
// (the b64 JSON EdgeAppsFromArg decodes) — the verb path's render must carry
// the same riders the console's does, or an expose/unexpose re-applies an
// edge that silently strips them (the two-appliers-render-identically
// invariant). "" when the Spec carries none.
func EdgeAppsArg(apps map[string]config.EdgeApp) string {
	if len(apps) == 0 {
		return ""
	}
	b, err := json.Marshal(apps)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

func NewSpec(c Coords, sec []byte, audience string) *Spec {
	s := &Spec{Sec: sec, Audience: audience}
	s.ApplyCoords(c)
	return s
}

// ApplyCoords re-seats the world coords on an already-built Spec (the
// console's build executor). The PROVISIONING box ships its profile's render
// before a build, so a coords change — the pangolin block or its clear, an
// edge rider, an allowlist — reaches the CP without a redeploy. The driving
// identity (Sec/Audience) is untouched; the caller re-asserts its own dirs and
// the serve-flag relay overrides after applying (the console owns those).
// A login-only box renders no coords (empty WorldConfigJSON), so it ships
// nothing and the CP keeps its deployed config.
func (s *Spec) ApplyCoords(c Coords) {
	s.Name = c.Name
	s.StateDir = c.StateDir
	s.RelayURL = c.RelayURL
	s.RelayAuthURL = c.RelayAuthURL
	s.RelayWS = c.RelayWS
	s.RelayPK = c.RelayPK
	s.RelayHost = c.RelayHost
	s.RelayIP = c.RelayIP
	s.CpHost = c.CpHost
	s.CpIP = c.CpIP
	s.CpLxc = c.CpLxc
	s.ProxyIP = c.ProxyIP
	s.LitellmIP = c.LitellmIP
	s.PlanePool = c.PlanePool
	s.PlaneKind = c.PlaneKind
	s.ThinPool = c.ThinPool
	s.SizeGB = c.SizeGB
	s.PoolSizeGB = c.PoolSizeGB
	s.RootfsGB = c.RootfsGB
	s.MemoryMB = c.MemoryMB
	s.StorageName = c.StorageName
	s.RelayGW = c.RelayGW
	s.Bridge = c.Bridge
	s.RelayLxc = c.RelayLxc
	s.RelayCompose = c.RelayCompose
	s.K3sVmid = c.K3sVmid
	s.RunnerAddr = c.RunnerAddr
	s.RunnerPK = c.RunnerPK
	s.RunnerTarget = c.RunnerTarget
	s.CpaName = c.CpaName
	s.OwnerPub = c.OwnerPub
	s.OperatorName = c.OperatorName
	s.WorldDomainOverride = c.WorldDomain
	s.Pangolin = c.Pangolin
	s.LitellmBaseURL = c.LitellmBaseURL
	s.SelfURL = c.SelfURL
	s.GatewayCIDR = c.GatewayCIDR
	s.GatewayVlan = c.GatewayVlan
	s.GatewayLxc = c.GatewayLxc
	s.HostedGateway = c.HostedGateway
	s.K3sIP = c.K3sIP
	s.EdgeApps = c.EdgeApps
	s.FillEdgeURLs()
}

// FillEdgeURLs fills the gateway URLs that are pure functions of the proxy
// IP. Fresh-world installs happen before litellm exists, so a spec built from
// the install-time world-config arrives with LitellmIP/LitellmBaseURL blank —
// without them the agent pods get an empty OPENAI_COMPAT_BASE_URL and every
// turn fails with "llm: transport: builder error". Behind a gateway the
// RECORDED litellm URL is the stale install-time render (the edge IP); the
// pods dial the k3s node's INTERNAL NodePort directly, so the internal k3s
// IP wins over the recording. Idempotent.
func (s *Spec) FillEdgeURLs() {
	node := s.k3sIP()
	if node == "" {
		return
	}
	if s.LitellmIP == "" || s.GatewayCIDR != "" {
		s.LitellmIP = config.StripCIDR(node)
	}
	if s.LitellmBaseURL == "" || s.GatewayCIDR != "" {
		if s.LitellmIP != "" {
			s.LitellmBaseURL = "http://" + s.LitellmIP + ":31400/v1"
		}
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
		OwnerPub: s.OwnerPub, OperatorName: s.OperatorName, WorldDomain: s.WorldDomainOverride, LitellmBaseURL: s.LitellmBaseURL, SelfURL: s.SelfURL,
		GatewayCIDR: s.GatewayCIDR, GatewayVlan: s.GatewayVlan, GatewayLxc: s.GatewayLxc,
		HostedGateway: s.HostedGateway,
		K3sIP:         s.K3sIP,
		EdgeApps:      s.EdgeApps,
		Pangolin:      s.Pangolin,
	}
}
