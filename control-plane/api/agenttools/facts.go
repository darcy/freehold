package agenttools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// WorldFacts is the deployer-side world inventory the box registers onto the
// CP at build (the "register-at-build" mechanism), so a management/login-only
// box can render the DATA + Certs views without holding the deployer's local
// config or probing the host. Served on world_status.
type WorldFacts struct {
	// Domains are the canonical world domains.
	Domains WorldDomains `json:"domains"`
	// Plane is the durable-plane layout (backend, pool, tenant mounts).
	Plane WorldPlane `json:"plane"`
	// Certs is the edge's per-slot certificate metadata.
	Certs []WorldCert `json:"certs"`
}

// WorldDomains are the canonical world hostnames.
type WorldDomains struct {
	Relay string `json:"relay,omitempty"`
	CP    string `json:"cp,omitempty"`
	Proxy string `json:"proxy,omitempty"`
}

// WorldPlane is the durable-plane layout (the DATA view's static half).
type WorldPlane struct {
	Backend    string          `json:"backend,omitempty"`     // pve
	BackendKind string         `json:"backend_kind,omitempty"` // zfs | lvmth
	ThinPool   string          `json:"thin_pool,omitempty"`
	Mounts     []WorldPlaneMount `json:"mounts"`
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
	Slot   string `json:"slot"`            // relay | cp
	Domain string `json:"domain"`          // the vhost it fronts
	Expiry string `json:"expiry,omitempty"` // RFC3339 notAfter, empty = unknown
	Issuer string `json:"issuer,omitempty"` // e.g. lego (DNS-01, cloudflare)
}

// FactsStore is the agent-tools server's durable world-facts store (facts.json
// under the state dir — survives compute-only teardown, like registry.json).
type FactsStore struct {
	mu    sync.Mutex
	path  string
	facts WorldFacts
}

// OpenFacts loads (creating if needed) the facts store at path.
func OpenFacts(path string) (*FactsStore, error) {
	f := &FactsStore{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &f.facts); err != nil {
			return nil, fmt.Errorf("malformed facts %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return f, nil
}

func (f *FactsStore) save() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f.facts, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// Register replaces the recorded world facts (the build's idempotent
// register-at-build — same input → same state).
func (f *FactsStore) Register(facts WorldFacts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = facts
	return f.save()
}

// Facts returns the recorded world facts (an empty value when unregistered).
func (f *FactsStore) Facts() WorldFacts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.facts
}