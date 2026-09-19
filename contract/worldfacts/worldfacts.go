// Package worldfacts holds the wire format of the CP's world inventory (the
// facts.json the build registers and every box's DATA/Certs view reads). It is
// a thin shared leaf so both the CP toolset and the local CLI can name the
// shape without one importing the other.
package worldfacts

// WorldFacts is the deployer-side world inventory the box registers onto the
// CP at build (the "register-at-build" mechanism), so a management/login-only
// box can render the DATA + Certs views without holding the deployer's local
// config or probing the host.
type WorldFacts struct {
	Domains WorldDomains `json:"domains"`
	Plane   WorldPlane   `json:"plane"`
	Certs   []WorldCert  `json:"certs"`
}

// WorldDomains are the canonical world hostnames.
type WorldDomains struct {
	Relay string `json:"relay,omitempty"`
	CP    string `json:"cp,omitempty"`
	Proxy string `json:"proxy,omitempty"`
}

// WorldPlane is the durable-plane layout (the DATA view's static half).
type WorldPlane struct {
	Backend     string            `json:"backend,omitempty"`      // pve
	BackendKind string            `json:"backend_kind,omitempty"` // zfs | lvmth
	ThinPool    string            `json:"thin_pool,omitempty"`
	Mounts      []WorldPlaneMount `json:"mounts"`
}

// WorldPlaneMount is one durable tenant mount (the /srv/data convention).
type WorldPlaneMount struct {
	Tenant    string `json:"tenant"`     // relay | cp | k3s
	Source    string `json:"source"`     // the host LV/ZFS source
	GuestPath string `json:"guest_path"` // the guest mount point
	Backup    bool   `json:"backup"`
	// Live usage as registered at build (the co-located plane probe) — served
	// by the CP so every box's DATA view renders the same durable-plane state.
	Size string `json:"size,omitempty"` // e.g. "100G"
	Used string `json:"used,omitempty"`
	Fill string `json:"fill,omitempty"`
}

// WorldCert is one edge slot's certificate metadata.
type WorldCert struct {
	Slot   string `json:"slot"`             // relay | cp
	Domain string `json:"domain"`           // the vhost it fronts
	Expiry string `json:"expiry,omitempty"` // RFC3339 notAfter, empty = unknown
	Issuer string `json:"issuer,omitempty"` // e.g. lego (DNS-01, cloudflare)
}
